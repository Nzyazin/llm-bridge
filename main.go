// llm-bridge exposes Devin CLI (`devin -p`) as an OpenAI-compatible
// /v1/chat/completions endpoint so that OpenAI clients (e.g. Quasar Bridge on a
// Yandex Station) can talk to Devin models.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

type config struct {
	listenAddr        string
	paseoBin          string
	provider          string // paseo provider/model, e.g. "devin/swe-2-flash-medium"
	workspaceID       string // paseo workspace hosting the voice session
	workspaceDir      string // used to create the workspace when workspaceID is empty
	agentFile         string // persists the voice session agent id across restarts
	devinBin          string // throwaway `devin -p` for the classifier fallback
	devinModel        string
	botToken          string
	botChatID         string
	timeout           time.Duration
	voiceIdleTTL      time.Duration
	authToken         string
	proxyURL          string
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
	timeout, err := time.ParseDuration(envOr("TURN_TIMEOUT", "90s"))
	if err != nil {
		timeout = 90 * time.Second
	}
	classifierTimeout, err := time.ParseDuration(envOr("CLASSIFIER_TIMEOUT", "6s"))
	if err != nil {
		classifierTimeout = 6 * time.Second
	}
	voiceIdleTTL, err := time.ParseDuration(envOr("VOICE_IDLE_TTL", "30m"))
	if err != nil {
		voiceIdleTTL = 30 * time.Minute
	}
	return config{
		listenAddr:        envOr("LISTEN_ADDR", ":8765"),
		paseoBin:          envOr("PASEO_BIN", "paseo"),
		provider:          envOr("PASEO_PROVIDER", "devin/swe-2-flash-medium"),
		workspaceID:       envOr("PASEO_WORKSPACE", "wks_b080f9f7ce6d8e09"),
		workspaceDir:      envOr("VOICE_WORKSPACE_DIR", `C:\OSPanel\domains\Golang\voice-agent`),
		agentFile:         envOr("VOICE_AGENT_FILE", "run/voice-agent.id"),
		devinBin:          envOr("DEVIN_BIN", "devin"),
		devinModel:        envOr("DEVIN_MODEL", "swe-2-flash-medium"),
		botToken:          envOr("BOT_TOKEN", ""),
		botChatID:         envOr("BOT_ID", ""),
		timeout:           timeout,
		voiceIdleTTL:      voiceIdleTTL,
		authToken:         os.Getenv("AUTH_TOKEN"),
		proxyURL:          os.Getenv("PROXY_URL"),
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
	cfg     config
	mu      sync.Mutex // voice turns are serialized: one prompt at a time
	tgHTTP  *http.Client
	agentID string // paseo voice session; guarded by mu

	replyMu  sync.Mutex
	lastSent map[string]sentEntry // client IP -> last forwarded payload: repeats are refused
}

type sentEntry struct {
	text string
	at   time.Time
}

// paseoCmd spawns the paseo CLI stripped of agent-scope env vars: otherwise
// the daemon treats calls as agent-originated and delivers them as
// agent-messages instead of plain user prompts.
func (b *bridge) paseoCmd(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, b.cfg.paseoBin, args...)
	env := os.Environ()
	keep := env[:0]
	for _, e := range env {
		if strings.HasPrefix(e, "PASEO_AGENT_ID=") || strings.HasPrefix(e, "PASEO_WORKSPACE_ID=") {
			continue
		}
		keep = append(keep, e)
	}
	cmd.Env = keep
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%w: %s", err, truncate(string(ee.Stderr), 200))
		}
		return nil, err
	}
	return out, nil
}

func (b *bridge) ensureWorkspace(ctx context.Context) (string, error) {
	if b.cfg.workspaceID != "" {
		return b.cfg.workspaceID, nil
	}
	out, err := b.paseoCmd(ctx, "workspace", "create", "--isolation", "local",
		"--path", b.cfg.workspaceDir, "--title", "voice-assistant", "--json")
	if err != nil {
		return "", fmt.Errorf("workspace create: %w", err)
	}
	var r struct {
		WorkspaceID string `json:"workspaceId"`
	}
	if json.Unmarshal(out, &r) != nil || r.WorkspaceID == "" {
		return "", fmt.Errorf("workspace create: bad output %s", truncate(string(out), 200))
	}
	b.cfg.workspaceID = r.WorkspaceID
	return r.WorkspaceID, nil
}

func (b *bridge) createVoiceAgent(ctx context.Context) (string, error) {
	ws, err := b.ensureWorkspace(ctx)
	if err != nil {
		return "", err
	}
	out, err := b.paseoCmd(ctx, "run", "--workspace", ws,
		"--provider", b.cfg.provider, "--mode", "ask",
		"--title", "Voice-assistant", "--label", "source=voice",
		"--no-wait", "--json", "Привет")
	if err != nil {
		return "", fmt.Errorf("paseo run: %w", err)
	}
	var r struct {
		AgentID string `json:"agentId"`
	}
	if json.Unmarshal(out, &r) != nil || r.AgentID == "" {
		return "", fmt.Errorf("paseo run: bad output %s", truncate(string(out), 200))
	}
	return r.AgentID, nil
}

func (b *bridge) loadAgentID() string {
	data, err := os.ReadFile(b.cfg.agentFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (b *bridge) saveAgentID(id string) {
	b.agentID = id
	b.touchAgentFile()
}

// touchAgentFile rewrites the id file so its mtime is the last voice turn:
// the idle-reset check below survives bridge restarts for free.
func (b *bridge) touchAgentFile() {
	if err := os.WriteFile(b.cfg.agentFile, []byte(b.agentID+"\n"), 0o644); err != nil {
		log.Printf("agent id write: %v", err)
	}
}

func (b *bridge) voiceStale() bool {
	st, err := os.Stat(b.cfg.agentFile)
	return err != nil || time.Since(st.ModTime()) > b.cfg.voiceIdleTTL
}

// paseoSend delivers the prompt and waits for the turn; the reply itself is
// read back from the agent timeline by fetchLastReply.
func (b *bridge) paseoSend(ctx context.Context, agentID, prompt string) (string, error) {
	// interrupt: a voice question outranks whatever the session is still
	// chewing on; without it a send steers into a stale, over-time turn.
	out, err := b.paseoCmd(ctx, "send", agentID, "--behavior", "interrupt", "--prompt", prompt, "--json")
	if err != nil {
		return "", err
	}
	var r struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if json.Unmarshal(out, &r) != nil {
		return "", fmt.Errorf("paseo send: bad output %s", truncate(string(out), 200))
	}
	switch r.Status {
	case "completed":
		return b.fetchLastReply(ctx, agentID)
	case "permission":
		// Voice can't wait for a human: deny the request and take whatever
		// the agent settles on without the tool.
		b.paseoCmd(ctx, "permit", "deny", agentID, "--all")
		b.paseoCmd(ctx, "wait", agentID, "--timeout", "30")
		if text, err := b.fetchLastReply(ctx, agentID); err == nil {
			return text, nil
		}
		return "Такое действие мне нельзя.", nil
	default:
		return "", fmt.Errorf("paseo send status %q: %s", r.Status, r.Message)
	}
}

func (b *bridge) fetchLastReply(ctx context.Context, agentID string) (string, error) {
	out, err := b.paseoCmd(ctx, "logs", agentID, "--tail", "12", "--json")
	if err != nil {
		return "", fmt.Errorf("paseo logs: %w", err)
	}
	var r struct {
		Items []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"items"`
	}
	if json.Unmarshal(out, &r) != nil {
		return "", fmt.Errorf("paseo logs: bad output %s", truncate(string(out), 200))
	}
	for i := len(r.Items) - 1; i >= 0; i-- {
		if r.Items[i].Type == "assistant_message" && strings.TrimSpace(r.Items[i].Text) != "" {
			return strings.TrimSpace(r.Items[i].Text), nil
		}
	}
	return "", fmt.Errorf("no assistant_message in last %d items", len(r.Items))
}

func (b *bridge) askVoice(ctx context.Context, question string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, b.cfg.timeout)
	defer cancel()

	if b.agentID == "" {
		b.agentID = b.loadAgentID()
	}
	// Long-idle sessions only carry stale context: a fresh one is cheaper
	// than dragging the whole accumulated timeline into every reply.
	if b.agentID != "" && b.voiceStale() {
		log.Printf("voice session idle over %s — resetting", b.cfg.voiceIdleTTL)
		if _, err := b.paseoCmd(ctx, "delete", b.agentID); err != nil {
			log.Printf("voice session delete: %v", err)
		}
		b.agentID = ""
		os.Remove(b.cfg.agentFile)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if b.agentID == "" {
			id, err := b.createVoiceAgent(ctx)
			if err != nil {
				return "", err
			}
			b.saveAgentID(id)
		}
		text, err := b.paseoSend(ctx, b.agentID, question)
		if err == nil {
			b.touchAgentFile()
			return text, nil
		} else if ctx.Err() != nil {
			return "", fmt.Errorf("paseo timeout after %s", b.cfg.timeout)
		}
		log.Printf("voice agent send failed, recreating session: %v", err)
		b.agentID = ""
	}
	return "", fmt.Errorf("voice agent unreachable after recreate")
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

// Dedup is keyed by client IP so one sender can't replay the same forward.
func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
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

func joinMessages(messages []chatMessage) string {
	var sb strings.Builder
	for _, m := range messages {
		sb.WriteString(m.Content)
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String())
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
	prompt := joinMessages(messages)
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

// classifyViaDevin answers a routing-classifier prompt with a throwaway
// `devin -p` run. Used only when the opencode classifier is disabled: keeps
// classify turns out of the shared voice session.
func (b *bridge) classifyViaDevin(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.timeout)
	defer cancel()

	workDir, err := os.MkdirTemp("", "devin-cls-")
	if err != nil {
		return "", fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(workDir)

	cmd := exec.CommandContext(ctx, b.cfg.devinBin,
		"-p", "--model", b.cfg.devinModel,
		"--permission-mode", "auto", // auto-approves read-only tools only
		"--respect-workspace-trust", "false",
		"--", prompt)
	cmd.Dir = workDir
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("devin timeout after %s", b.cfg.timeout)
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("devin exit %v: %s", err, truncate(string(ee.Stderr), 200))
		}
		return "", fmt.Errorf("devin: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// --- telegram forwarding via agent marker ---
//
// The voice agent answers with "TG_SEND:" on its own line when the user asks
// to forward something to Telegram (contract lives in the workspace
// AGENTS.md); everything after the marker is the payload. Triggering is the
// model's semantic decision, so any phrasing works and no input regex is
// needed.

const tgSendMarker = "TG_SEND:"

func telegramMarker(reply string) (string, bool) {
	i := strings.Index(reply, tgSendMarker)
	if i < 0 {
		return "", false
	}
	payload := strings.TrimSpace(reply[i+len(tgSendMarker):])
	return payload, payload != ""
}

func (b *bridge) actionForward(ctx context.Context, payload, client string) string {
	if b.alreadySent(client, payload) {
		return "Уже отправлено."
	}
	if err := b.sendTelegram(ctx, payload); err != nil {
		log.Printf("telegram send: %v", err)
		return "Не получилось отправить."
	}
	b.rememberSent(client, payload)
	return "Отправлено."
}

func (b *bridge) sendTelegram(ctx context.Context, text string) error {
	if b.cfg.botToken == "" || b.cfg.botChatID == "" {
		return fmt.Errorf("telegram not configured: set BOT_TOKEN and BOT_ID")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{
		"chat_id": b.cfg.botChatID,
		"text":    text,
	})

	// The route to api.telegram.org (through the local ss proxy) flaps under
	// DPI: one attempt EOFs, the next gets through. Retry transport errors
	// so a send lands in a live window; API answers are not retried.
	for attempt := 1; ; attempt++ {
		err := b.sendTelegramOnce(ctx, payload)
		if err == nil {
			return nil
		}
		var apiErr *tgAPIError
		if errors.As(err, &apiErr) || attempt >= 4 || ctx.Err() != nil {
			return err
		}
		log.Printf("telegram send attempt %d failed (retrying): %v", attempt, err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(1500 * time.Millisecond):
		}
	}
}

// tgAPIError marks "Telegram answered, but not ok": retrying is pointless.
type tgAPIError struct {
	status int
	desc   string
}

func (e *tgAPIError) Error() string {
	return fmt.Sprintf("telegram api status %d: %s", e.status, e.desc)
}

func (b *bridge) sendTelegramOnce(ctx context.Context, payload []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.telegram.org/bot"+b.cfg.botToken+"/sendMessage",
		bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.tgHTTP.Do(req)
	if err != nil {
		// url.Error strings the bot-token URL into logs; keep the cause only.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return uerr.Err
		}
		return err
	}
	defer resp.Body.Close()
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || !out.OK {
		return &tgAPIError{status: resp.StatusCode, desc: out.Description}
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
	// provider= is the effective backend (req.Model from the client is
	// ignored), asked= is what was requested.
	log.Printf("req=%s from=%s provider=%q asked=%q messages=%d question=%q",
		reqID, r.RemoteAddr, b.cfg.provider, req.Model, len(req.Messages), truncate(lastUserMessage(req.Messages), 500))

	// Quasar routing classifier: detected before the voice session because
	// classify prompts quote the user phrase verbatim and must not pollute
	// its context. opencode backend when enabled; otherwise a throwaway
	// `devin -p` answers and we extract the route literal from its reply.
	if isClassifierRequest(req.Messages) {
		start := time.Now()
		route := ""
		if b.cfg.classifierOn {
			route = b.classify(r.Context(), req.Messages)
		} else if text, err := b.classifyViaDevin(r.Context(), joinMessages(req.Messages)); err == nil {
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

	start := time.Now()
	text, err := b.askVoice(r.Context(), lastUserMessage(req.Messages))
	if err != nil {
		log.Printf("req=%s paseo error after %s: %v", reqID, time.Since(start).Round(time.Millisecond), err)
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("req=%s answered in %s: %q", reqID, time.Since(start).Round(time.Millisecond), truncate(text, 500))

	// TG_SEND marker: the voice agent asks us to forward a payload to
	// Telegram; the speaker hears a short confirmation instead of the body.
	if payload, ok := telegramMarker(text); ok {
		text = b.actionForward(r.Context(), payload, clientIP(r.RemoteAddr))
	}

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
	b := &bridge{cfg: cfg, lastSent: make(map[string]sentEntry)}
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
	log.Printf("llm-bridge listening on %s (provider=%s, workspace=%s, timeout=%s, tts_prefix=%q, classifier=%v:%s, telegram=%v)",
		cfg.listenAddr, cfg.provider, cfg.workspaceID, cfg.timeout, cfg.ttsPrefix, cfg.classifierOn, cfg.opencodeModel, cfg.botToken != "" && cfg.botChatID != "")
	log.Fatal(http.Serve(ln, mux))
}
