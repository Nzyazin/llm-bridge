// devin-llm-bridge exposes Devin CLI (`devin -p`) as an OpenAI-compatible
// /v1/chat/completions endpoint so that OpenAI clients (e.g. Quasar Bridge on a
// Yandex Station) can talk to Devin models.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

type config struct {
	listenAddr        string
	devinBin          string
	devinModel        string
	botToken          string
	timeout           time.Duration
	maxHistory        int
	authToken         string
	proxyURL          string
	systemIntro       string
	ttsPrefix         string
	classifierOn      bool
	classifierBaseURL string
	classifierAPIKey  string
	classifierModel   string
	classifierTimeout time.Duration
	classifierExtra   string
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig() config {
	timeout, err := time.ParseDuration(envOr("DEVIN_TIMEOUT", "90s"))
	if err != nil {
		timeout = 90 * time.Second
	}
	maxHistory, err := strconv.Atoi(envOr("MAX_HISTORY", "10"))
	if err != nil || maxHistory < 1 {
		maxHistory = 10
	}
	classifierTimeout, err := time.ParseDuration(envOr("CLASSIFIER_TIMEOUT", "6s"))
	if err != nil {
		classifierTimeout = 6 * time.Second
	}
	classifierOn := envOr("CLASSIFIER_ENABLED", "true") != "false"
	return config{
		listenAddr: envOr("LISTEN_ADDR", ":8765"),
		devinBin:   envOr("DEVIN_BIN", "devin"),
		devinModel: envOr("DEVIN_MODEL", "swe-2-flash-medium"),
		botToken:   envOr("BOT_TOKEN", ""),
		timeout:    timeout,
		maxHistory: maxHistory,
		authToken:  os.Getenv("AUTH_TOKEN"),
		proxyURL:   os.Getenv("PROXY_URL"),
		systemIntro: envOr("SYSTEM_PROMPT",
			"Ты — голосовой помощник умной колонки. Твой ответ будет зачитан вслух. "+
				"Отвечай кратко и по-русски, обычной связной речью: без разметки, списков, "+
				"таблиц, кода и эмодзи. Если для ответа нужны актуальные данные "+
				"(погода, новости, курсы валют, расписание) — используй веб-поиск. "+
				"Другие инструменты не используй и никаких действий не выполняй."),
		ttsPrefix:         os.Getenv("TTS_PREFIX"),
		classifierOn:      classifierOn && os.Getenv("CLASSIFIER_API_KEY") != "",
		classifierBaseURL: envOr("CLASSIFIER_BASE_URL", "https://api.z.ai/api/paas/v4"),
		classifierAPIKey:  os.Getenv("CLASSIFIER_API_KEY"),
		classifierModel:   envOr("CLASSIFIER_MODEL", "glm-4.5-air"),
		classifierTimeout: classifierTimeout,
		classifierExtra:   os.Getenv("CLASSIFIER_EXTRA_BODY"),
	}
}

// --- OpenAI wire types ---

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type chatCompletion struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int         `json:"index"`
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func writeOpenAIError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": msg,
			"type":    "server_error",
		},
	})
}

// --- core ---

type bridge struct {
	cfg            config
	mu             sync.Mutex // devin runs are serialized: one prompt at a time
	classifierHTTP *http.Client
}

func (b *bridge) buildPrompt(messages []chatMessage) string {
	var sb strings.Builder
	sb.WriteString(b.cfg.systemIntro)
	sb.WriteString("\n\n")

	start := 0
	if len(messages) > b.cfg.maxHistory {
		start = len(messages) - b.cfg.maxHistory
	}
	for _, m := range messages[start:] {
		role := m.Role
		if role == "system" {
			role = "Инструкция"
		} else if role == "user" {
			role = "Пользователь"
		} else if role == "assistant" {
			role = "Ассистент"
		}
		sb.WriteString(role)
		sb.WriteString(": ")
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	return sb.String()
}

func (b *bridge) askDevin(ctx context.Context, prompt string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, b.cfg.timeout)
	defer cancel()

	// Fresh empty workspace per call: keeps runs stateless and outside any
	// real project directory.
	workDir, err := os.MkdirTemp("", "devin-llm-")
	if err != nil {
		return "", fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(workDir)

	args := []string{
		"-p",
		"--model", b.cfg.devinModel,
		"--permission-mode", "auto", // explicit: auto-approves read-only tools only, overrides DEVIN_PERMISSION_MODE env
		"--respect-workspace-trust", "false",
		"--", prompt,
	}
	cmd := exec.CommandContext(ctx, b.cfg.devinBin, args...)
	cmd.Dir = workDir
	cmd.Env = os.Environ()
	if b.cfg.proxyURL != "" {
		cmd.Env = append(cmd.Env,
			"HTTP_PROXY="+b.cfg.proxyURL,
			"HTTPS_PROXY="+b.cfg.proxyURL,
			"ALL_PROXY="+b.cfg.proxyURL,
		)
	}

	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("devin timeout after %s", b.cfg.timeout)
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("devin exit %v: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("devin: %w", err)
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "", fmt.Errorf("devin returned empty response")
	}
	return text, nil
}

func completionResponse(model, text string) chatCompletion {
	var r chatCompletion
	r.ID = fmt.Sprintf("chatcmpl-devin-%d", time.Now().UnixNano())
	r.Object = "chat.completion"
	r.Created = time.Now().Unix()
	r.Model = model
	r.Choices = []struct {
		Index        int         `json:"index"`
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	}{{
		Index:        0,
		Message:      chatMessage{Role: "assistant", Content: text},
		FinishReason: "stop",
	}}
	return r
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

func lastUserMessage(messages []chatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

// --- Quasar routing classifier fast-path ---
//
// Quasar Bridge has no separate classifier backend: it sends its classifier
// instruction (which quotes all three {"route":"..."} literals) plus the user
// phrase to the same /v1/chat/completions. Answering those through `devin -p`
// costs several seconds and holds the devin mutex, so we detect them and
// forward to a cheap fast model instead.

var routeRe = regexp.MustCompile(`"route"\s*:\s*"(home|llm|yandex)"`)

func isClassifierRequest(messages []chatMessage) bool {
	var sb strings.Builder
	for _, m := range messages {
		sb.WriteString(m.Content)
	}
	compact := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, sb.String())
	return strings.Contains(compact, `{"route":"home"}`) &&
		strings.Contains(compact, `{"route":"llm"}`) &&
		strings.Contains(compact, `{"route":"yandex"}`)
}

func (b *bridge) newClassifierClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	switch p := envOr("CLASSIFIER_PROXY", "env"); p {
	case "direct":
		tr.Proxy = nil // measured faster than the loaded local proxy; fallback degrades to yandex
	case "", "env":
	default:
		if u, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	if b.cfg.proxyURL != "" && envOr("CLASSIFIER_PROXY", "env") == "env" {
		if u, err := url.Parse(b.cfg.proxyURL); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: tr}
}

// classify returns one of "home", "llm", "yandex"; any failure degrades to
// "yandex" so Quasar always gets a valid fast route instead of an error wait.
// Two hedged attempts: upstream latency spikes occasionally stall the first
// try past the deadline while a parallel retry answers in ~1s.
func (b *bridge) classify(ctx context.Context, messages []chatMessage) string {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.classifierTimeout)
	defer cancel()

	results := make(chan string, 1)
	go b.classifyAttempt(ctx, messages, results)
	select {
	case r := <-results:
		return r
	case <-time.After(1500 * time.Millisecond):
	}
	go b.classifyAttempt(ctx, messages, results)
	select {
	case r := <-results:
		return r
	case <-ctx.Done():
		return "yandex"
	}
}

func (b *bridge) classifyAttempt(ctx context.Context, messages []chatMessage, results chan<- string) {
	if r := b.classifyOnce(ctx, messages); r != "" {
		select {
		case results <- r:
		default:
		}
	}
}

func (b *bridge) classifyOnce(ctx context.Context, messages []chatMessage) string {
	body := map[string]any{
		"model":       b.cfg.classifierModel,
		"messages":    messages,
		"max_tokens":  256,
		"temperature": 0,
		"stream":      false,
		// GLM thinking mode adds ~5s of reasoning before the answer — a
		// classifier must be fast. Backends that don't know this param ignore it.
		"thinking": map[string]any{"type": "disabled"},
	}
	if b.cfg.classifierExtra != "" {
		var extra map[string]any
		if json.Unmarshal([]byte(b.cfg.classifierExtra), &extra) == nil {
			for k, v := range extra {
				body[k] = v
			}
		}
	}
	payload, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(b.cfg.classifierBaseURL, "/")+"/chat/completions",
		bytes.NewReader(payload))
	if err != nil {
		log.Printf("classifier request build error: %v", err)
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.cfg.classifierAPIKey)

	resp, err := b.classifierHTTP.Do(req)
	if err != nil {
		if ctx.Err() == nil { // hedged sibling's cancellation is expected, not an error
			log.Printf("classifier backend error: %v", err)
		}
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("classifier backend status %d", resp.StatusCode)
		return ""
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Choices) == 0 {
		log.Printf("classifier decode error: %v", err)
		return ""
	}
	if m := routeRe.FindStringSubmatch(out.Choices[0].Message.Content); m != nil {
		return m[1]
	}
	log.Printf("classifier: no route in reply %q", truncate(out.Choices[0].Message.Content, 200))
	return ""
}

func (b *bridge) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "messages is empty")
		return
	}

	reqID := fmt.Sprintf("%x", time.Now().UnixNano()&0xFFFFFF)
	log.Printf("req=%s from=%s model=%q messages=%d question=%q",
		reqID, r.RemoteAddr, req.Model, len(req.Messages), truncate(lastUserMessage(req.Messages), 500))

	// Quasar routing classifier: answer via the cheap backend, not devin.
	if b.cfg.classifierOn && isClassifierRequest(req.Messages) {
		start := time.Now()
		route := b.classify(r.Context(), req.Messages)
		log.Printf("req=%s classifier route=%s in %s", reqID, route, time.Since(start).Round(time.Millisecond))
		b.writeReply(w, req.Stream, `{"route":"`+route+`"}`)
		return
	}

	prompt := b.buildPrompt(req.Messages)
	start := time.Now()
	text, err := b.askDevin(r.Context(), prompt)
	if err != nil {
		log.Printf("req=%s devin error after %s: %v", reqID, time.Since(start).Round(time.Millisecond), err)
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("req=%s answered in %s: %q", reqID, time.Since(start).Round(time.Millisecond), truncate(text, 500))

	// TTS markup prefix (e.g. <speaker voice="zahar" emotion="evil">) applies to
	// the whole reply; Quasar Bridge strips it before showing text.
	b.writeReply(w, req.Stream, b.cfg.ttsPrefix+text)
}

func (b *bridge) writeReply(w http.ResponseWriter, stream bool, text string) {
	resp := completionResponse(b.cfg.devinModel, text)
	w.Header().Set("Content-Type", "application/json")
	if stream {
		// Minimal SSE: one chunk with the whole reply, then [DONE].
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := map[string]any{
			"id": resp.ID, "object": "chat.completion.chunk",
			"created": resp.Created, "model": resp.Model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]string{"role": "assistant", "content": text},
				"finish_reason": "stop",
			}},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	json.NewEncoder(w).Encode(resp)
}

func (b *bridge) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if b.cfg.authToken != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got != b.cfg.authToken {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func main() {
	// All log lines go to stdout: Start-Process redirects stdout to bridge.log,
	// stderr is reserved for actual panics/runtime errors.
	log.SetOutput(os.Stdout)

	cfg := loadConfig()
	b := &bridge{cfg: cfg}
	b.classifierHTTP = b.newClassifierClient()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/v1/models", b.auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{{
				"id":       cfg.devinModel,
				"object":   "model",
				"created":  0,
				"owned_by": "devin-llm-bridge",
			}},
		})
	}))
	mux.HandleFunc("/v1/chat/completions", b.auth(b.handleChatCompletions))

	ln, err := net.Listen("tcp", cfg.listenAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", cfg.listenAddr, err)
	}
	log.Printf("devin-llm-bridge listening on %s (model=%s, timeout=%s, tts_prefix=%q, classifier=%v:%s)",
		cfg.listenAddr, cfg.devinModel, cfg.timeout, cfg.ttsPrefix, cfg.classifierOn, cfg.classifierModel)
	log.Fatal(http.Serve(ln, mux))
}
