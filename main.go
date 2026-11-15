// devin-llm-bridge exposes Devin CLI (`devin -p`) as an OpenAI-compatible
// /v1/chat/completions endpoint so that OpenAI clients (e.g. Quasar Bridge on a
// Yandex Station) can talk to Devin models.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type config struct {
	listenAddr  string
	devinBin    string
	devinModel  string
	timeout     time.Duration
	maxHistory  int
	authToken   string
	proxyURL    string
	systemIntro string
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
	return config{
		listenAddr: envOr("LISTEN_ADDR", ":8765"),
		devinBin:   envOr("DEVIN_BIN", "devin"),
		devinModel: envOr("DEVIN_MODEL", "swe-2-high"),
		timeout:    timeout,
		maxHistory: maxHistory,
		authToken:  os.Getenv("AUTH_TOKEN"),
		proxyURL:   os.Getenv("PROXY_URL"),
		systemIntro: envOr("SYSTEM_PROMPT",
			"Ты — голосовой помощник умной колонки. Твой ответ будет зачитан вслух. "+
				"Отвечай кратко и по-русски, обычной связной речью: без разметки, списков, "+
				"таблиц, кода и эмодзи. Просто ответь текстом, не используй никакие инструменты."),
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
	cfg config
	mu  sync.Mutex // devin runs are serialized: one prompt at a time
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
	prompt := b.buildPrompt(req.Messages)
	log.Printf("req=%s from=%s model=%q messages=%d question=%q",
		reqID, r.RemoteAddr, req.Model, len(req.Messages), truncate(lastUserMessage(req.Messages), 500))

	start := time.Now()
	text, err := b.askDevin(r.Context(), prompt)
	if err != nil {
		log.Printf("req=%s devin error after %s: %v", reqID, time.Since(start).Round(time.Millisecond), err)
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("req=%s answered in %s: %q", reqID, time.Since(start).Round(time.Millisecond), truncate(text, 500))

	resp := completionResponse(b.cfg.devinModel, text)
	w.Header().Set("Content-Type", "application/json")
	if req.Stream {
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
	log.Printf("devin-llm-bridge listening on %s (model=%s, timeout=%s)",
		cfg.listenAddr, cfg.devinModel, cfg.timeout)
	log.Fatal(http.Serve(ln, mux))
}
