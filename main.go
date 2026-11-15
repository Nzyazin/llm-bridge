// llm-bridge exposes Devin CLI (`devin -p`) as an OpenAI-compatible
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
	"path/filepath"
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
	botChatID         string
	timeout           time.Duration
	maxHistory        int
	authToken         string
	proxyURL          string
	systemIntro       string
	ttsPrefix         string
	classifierOn      bool
	classifierTimeout time.Duration
	opencodeBin       string
	opencodeModel     string
	opencodeVariant   string
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
	return config{
		listenAddr: envOr("LISTEN_ADDR", ":8765"),
		devinBin:   envOr("DEVIN_BIN", "devin"),
		devinModel: envOr("DEVIN_MODEL", "swe-2-flash-medium"),
		botToken:   envOr("BOT_TOKEN", ""),
		botChatID:  envOr("BOT_ID", ""),
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
		classifierOn:      envOr("CLASSIFIER_ENABLED", "true") != "false",
		classifierTimeout: classifierTimeout,
		opencodeBin:       envOr("OPENCODE_BIN", "opencode"),
		opencodeModel:     envOr("OPENCODE_MODEL", "opencode/flint-40b-free"),
		opencodeVariant:   envOr("OPENCODE_VARIANT", "low"),
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
	cfg    config
	mu     sync.Mutex // devin runs are serialized: one prompt at a time
	tgHTTP *http.Client

	replyMu   sync.Mutex
	lastReply map[string]string    // client IP -> last devin reply, for "forward it to telegram"
	lastSent  map[string]sentEntry // client IP -> last forwarded payload: repeats are refused
}

type sentEntry struct {
	text string
	at   time.Time
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

func lastAssistantMessage(messages []chatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			return messages[i].Content
		}
	}
	return ""
}

// TTS markup is prepended to replies meant for the speaker; it must not leak
// into forwarded text.
func stripTTSTag(s, prefix string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), prefix))
}

// Quasar sends no assistant history (messages are [system, user] only), so
// "forward the last reply" needs a bridge-side cache keyed by client IP.
func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

func (b *bridge) rememberReply(client, text string) {
	b.replyMu.Lock()
	defer b.replyMu.Unlock()
	b.lastReply[client] = text
}

func (b *bridge) lastReplyFor(client string) string {
	b.replyMu.Lock()
	defer b.replyMu.Unlock()
	return b.lastReply[client]
}

// Quasar re-sends the previous question on a bare "спроси нейросеть", which
// replays side-effecting commands like "отправь в телеграм". Identical
// payloads are refused within this window (quasar context ttl is 300s).
const telegramDedupTTL = 10 * time.Minute

func (b *bridge) alreadySent(client, text string) bool {
	b.replyMu.Lock()
	defer b.replyMu.Unlock()
	e, ok := b.lastSent[client]
	return ok && e.text == text && time.Since(e.at) < telegramDedupTTL
}

func (b *bridge) rememberSent(client, text string) {
	b.replyMu.Lock()
	defer b.replyMu.Unlock()
	b.lastSent[client] = sentEntry{text: text, at: time.Now()}
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
	// The official opencode CLI is the only identity Zen's anonymous free tier
	// accepts, so classification is a `opencode run` spawn, not an HTTP call.
	if r := b.classifyViaOpencode(ctx, messages); r != "" {
		select {
		case results <- r:
		default:
		}
	}
}

// classifyViaOpencode shells out to `opencode run --format json`, scans the
// NDJSON event stream for the last text part and extracts the route from it.
//
// A fixed --dir matters: opencode embeds the workdir into its ~11K-token agent
// system prompt, so a fresh temp dir per call busts the provider prompt cache
// and each classify costs ~10s. A stable dir caches the prefix → ~2s.
// opencode.json in that dir disables tools: the model must not go agentic and
// burn the budget on bash calls.
func (b *bridge) classifyViaOpencode(ctx context.Context, messages []chatMessage) string {
	var sb strings.Builder
	for _, m := range messages {
		sb.WriteString(m.Content)
		sb.WriteString("\n\n")
	}
	prompt := strings.TrimSpace(sb.String())
	if prompt == "" {
		return ""
	}

	workDir := filepath.Join(os.TempDir(), "llm-bridge-cls")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		log.Printf("classifier(opencode) mkdir: %v", err)
		return ""
	}
	if err := os.WriteFile(filepath.Join(workDir, "opencode.json"),
		[]byte(`{"tools":{"bash":false,"edit":false,"write":false,"patch":false,"task":false,"skill":false,"webfetch":false,"read":false,"grep":false,"glob":false,"list":false,"todoread":false,"todowrite":false}}`),
		0o644); err != nil {
		log.Printf("classifier(opencode) write config: %v", err)
	}

	args := []string{"run", "--format", "json", "--pure", "-m", b.cfg.opencodeModel, "--dir", workDir}
	if b.cfg.opencodeVariant != "" {
		args = append(args, "--variant", b.cfg.opencodeVariant)
	}
	args = append(args, prompt)
	cmd := exec.CommandContext(ctx, b.cfg.opencodeBin, args...)
	cmd.Dir = workDir
	cmd.Env = os.Environ()

	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			log.Printf("classifier(opencode) exit %v: %s", err, truncate(string(ee.Stderr), 300))
		} else {
			log.Printf("classifier(opencode): %v", err)
		}
		return ""
	}

	var text string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") || !strings.Contains(line, `"type":"text"`) {
			continue
		}
		var ev struct {
			Part struct {
				Text string `json:"text"`
			} `json:"part"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Part.Text != "" {
			text = ev.Part.Text
		}
	}
	if m := routeRe.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	log.Printf("classifier(opencode): no route in reply %q", truncate(text, 200))
	return ""
}

// --- local voice actions (no devin round-trip) ---

var (
	// No \b/\w: Go's RE2 defines them ASCII-only — Cyrillic is "non-word", so
	// \bв and \bтелеграм\b never match. Substring matching is fine here.
	telegramCmdRe   = regexp.MustCompile(`(?i)^\s*(?:отправь|отправить|перешли|переслать|скинь|кинь|скинуть)\s+(.+?)\s*[.!?…]*\s*$`)
	telegramPrepRe  = regexp.MustCompile(`(?i)в\s+телеграм\p{L}*`)
	telegramWordRe  = regexp.MustCompile(`(?i)телеграм\p{L}*`)
	telegramPronoun = map[string]bool{"это": true, "его": true, "её": true, "их": true}
)

// telegramBody reports whether msg is a "send to telegram" command and returns
// the explicit payload. "" payload means "forward the last assistant reply".
func telegramBody(msg string) (string, bool) {
	m := telegramCmdRe.FindStringSubmatch(msg)
	if m == nil {
		return "", false
	}
	rest := m[1]
	hasTG := telegramWordRe.MatchString(rest)
	rest = telegramPrepRe.ReplaceAllString(rest, " ")
	rest = telegramWordRe.ReplaceAllString(rest, " ")
	rest = strings.Trim(rest, " ,.:;—-")
	if telegramPronoun[strings.ToLower(rest)] {
		return "", true
	}
	return rest, hasTG
}

func (b *bridge) actionTelegram(ctx context.Context, messages []chatMessage, client string) string {
	text, _ := telegramBody(lastUserMessage(messages))
	if text == "" {
		text = stripTTSTag(lastAssistantMessage(messages), b.cfg.ttsPrefix)
	}
	if text == "" {
		text = b.lastReplyFor(client)
	}
	if text == "" {
		return "Нечего отправить."
	}
	if b.alreadySent(client, text) {
		return "Уже отправлено."
	}
	if err := b.sendTelegram(ctx, text); err != nil {
		log.Printf("telegram send: %v", err)
		return "Не получилось отправить."
	}
	b.rememberSent(client, text)
	return "Отправлено."
}

func (b *bridge) sendTelegram(ctx context.Context, text string) error {
	if b.cfg.botToken == "" || b.cfg.botChatID == "" {
		return fmt.Errorf("telegram not configured: set BOT_TOKEN and BOT_ID")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{
		"chat_id": b.cfg.botChatID,
		"text":    text,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.telegram.org/bot"+b.cfg.botToken+"/sendMessage",
		bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.tgHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || !out.OK {
		return fmt.Errorf("telegram api status %d: %s", resp.StatusCode, out.Description)
	}
	return nil
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
	// model= is the effective model (req.Model from the client is ignored;
	// the bridge always runs cfg.devinModel), asked= is what was requested.
	log.Printf("req=%s from=%s model=%q asked=%q messages=%d question=%q",
		reqID, r.RemoteAddr, b.cfg.devinModel, req.Model, len(req.Messages), truncate(lastUserMessage(req.Messages), 500))

	// Quasar routing classifier: detected before the telegram action because
	// classify prompts quote the user phrase verbatim and would misfire it.
	// opencode backend when enabled; otherwise devin answers the prompt and
	// we extract the route literal from its reply.
	if isClassifierRequest(req.Messages) {
		start := time.Now()
		route := ""
		if b.cfg.classifierOn {
			route = b.classify(r.Context(), req.Messages)
		} else if text, err := b.askDevin(r.Context(), b.buildPrompt(req.Messages)); err == nil {
			if m := routeRe.FindStringSubmatch(text); m != nil {
				route = m[1]
			}
		} else {
			log.Printf("req=%s classifier devin error after %s: %v", reqID, time.Since(start).Round(time.Millisecond), err)
		}
		if route == "" {
			route = "yandex"
		}
		log.Printf("req=%s classifier route=%s in %s", reqID, route, time.Since(start).Round(time.Millisecond))
		b.writeReply(w, req.Stream, `{"route":"`+route+`"}`)
		return
	}

	// Local actions bypass devin: fast (~0.5s) and no mutex.
	if _, ok := telegramBody(lastUserMessage(req.Messages)); ok {
		start := time.Now()
		text := b.actionTelegram(r.Context(), req.Messages, clientIP(r.RemoteAddr))
		log.Printf("req=%s telegram action in %s: %q", reqID, time.Since(start).Round(time.Millisecond), truncate(text, 200))
		b.writeReply(w, req.Stream, b.cfg.ttsPrefix+text)
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
	b.rememberReply(clientIP(r.RemoteAddr), text)

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
	b := &bridge{cfg: cfg, lastReply: make(map[string]string), lastSent: make(map[string]sentEntry)}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.proxyURL != "" {
		if u, err := url.Parse(cfg.proxyURL); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	b.tgHTTP = &http.Client{Transport: tr, Timeout: 15 * time.Second}

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
				"owned_by": "llm-bridge",
			}},
		})
	}))
	mux.HandleFunc("/v1/chat/completions", b.auth(b.handleChatCompletions))

	ln, err := net.Listen("tcp", cfg.listenAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", cfg.listenAddr, err)
	}
	log.Printf("llm-bridge listening on %s (model=%s, timeout=%s, tts_prefix=%q, classifier=%v:%s, telegram=%v)",
		cfg.listenAddr, cfg.devinModel, cfg.timeout, cfg.ttsPrefix, cfg.classifierOn, cfg.opencodeModel, cfg.botToken != "" && cfg.botChatID != "")
	log.Fatal(http.Serve(ln, mux))
}
