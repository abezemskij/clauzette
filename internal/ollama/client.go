// Package ollama is a small client for the parts of the Ollama HTTP API the
// agent needs: streaming chat with tools, loading a model, and listing
// loaded models.
package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrStalled is returned when Ollama sends nothing for longer than the
// timeout of the current phase (before the first token, or between tokens).
var ErrStalled = errors.New("no data from ollama within the timeout")

type ToolCallFunction struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type ToolCall struct {
	Function ToolCallFunction `json:"function"`
}

type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Thinking  string     `json:"thinking,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"`
}

type FunctionSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ToolSpec struct {
	Type     string       `json:"type"`
	Function FunctionSpec `json:"function"`
}

type ChatRequest struct {
	Model     string         `json:"model"`
	Messages  []Message      `json:"messages"`
	Tools     []ToolSpec     `json:"tools,omitempty"`
	Stream    bool           `json:"stream"`
	Think     *bool          `json:"think,omitempty"`
	Options   map[string]any `json:"options,omitempty"`
	KeepAlive string         `json:"keep_alive,omitempty"`
}

type chatChunk struct {
	Message            Message `json:"message"`
	Done               bool    `json:"done"`
	DoneReason         string  `json:"done_reason"`
	Error              string  `json:"error"`
	LoadDuration       int64   `json:"load_duration"`
	PromptEvalCount    int     `json:"prompt_eval_count"`
	PromptEvalDuration int64   `json:"prompt_eval_duration"`
	EvalCount          int     `json:"eval_count"`
	EvalDuration       int64   `json:"eval_duration"`
}

// Result is a complete (or, on error, partial) assistant response.
type Result struct {
	Message         Message
	DoneReason      string
	PromptEvalCount int
	EvalCount       int
	LoadDuration    time.Duration
	PromptDuration  time.Duration
	EvalDuration    time.Duration
}

func (r *Result) TokensPerSecond() float64 {
	if r == nil || r.EvalDuration <= 0 {
		return 0
	}
	return float64(r.EvalCount) / r.EvalDuration.Seconds()
}

type Timeouts struct {
	Connect    time.Duration // establishing the TCP connection
	FirstToken time.Duration // from sending a request to the first streamed data
	InterToken time.Duration // maximum silence once data is flowing
	Load       time.Duration // explicit model load (warm-up)
}

type Client struct {
	base string
	hc   *http.Client
	t    Timeouts

	// nativeCtx caches the model's native context length (0 = unknown),
	// fetched once via /api/show. Both the agent and the UI read it.
	nativeCtx atomic.Int64
}

func New(baseURL string, t Timeouts) *Client {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: t.Connect, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        4,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	// No overall timeout on the client: long requests are normal here.
	// Each phase is guarded by a watchdog instead.
	return &Client{base: strings.TrimRight(baseURL, "/"), hc: &http.Client{Transport: tr}, t: t}
}

func (c *Client) BaseURL() string { return c.base }

// ModelNativeContext returns the model's native context length in tokens,
// as reported by Ollama (0 if not fetched yet). This is the largest
// context the model itself supports, independent of the num_ctx we request.
func (c *Client) ModelNativeContext() int { return int(c.nativeCtx.Load()) }

// SetModelNativeContext records the value so it need not be refetched.
func (c *Client) SetModelNativeContext(n int) { c.nativeCtx.Store(int64(n)) }

// ShowModelContext asks Ollama for the model's native context length, from
// model_info (keys ending in ".context_length"), and caches it. It returns
// 0 (with no error) if the model does not report one.
func (c *Client) ShowModelContext(ctx context.Context, model string) (int, error) {
	var out struct {
		ModelInfo map[string]any `json:"model_info"`
	}
	if err := c.postJSON(ctx, "/api/show", map[string]any{"name": model}, &out); err != nil {
		return 0, err
	}
	best := 0
	for k, v := range out.ModelInfo {
		if !strings.HasSuffix(k, ".context_length") {
			continue
		}
		switch n := v.(type) {
		case float64:
			if int(n) > best {
				best = int(n)
			}
		case string:
			if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil && i > best {
				best = i
			}
		}
	}
	c.SetModelNativeContext(best)
	return best, nil
}

// ChatStream sends a chat request and streams the response. onDelta is
// called synchronously for every piece of content or thinking text. On error
// the returned Result holds whatever was received before the failure.
func (c *Client) ChatStream(ctx context.Context, req ChatRequest, onDelta func(Message)) (*Result, error) {
	req.Stream = true
	if req.Messages == nil {
		req.Messages = []Message{}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wd := newWatchdog(c.t.FirstToken, "waiting for the first token", cancel)
	defer wd.stop()

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(hreq)
	if err != nil {
		return nil, c.classify(parent, wd, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, fmt.Errorf("ollama %s: %s", resp.Status, apiError(b))
	}

	var content, thinking strings.Builder
	var calls []ToolCall
	res := &Result{}
	partial := func() *Result {
		res.Message = Message{Role: "assistant", Content: content.String(), Thinking: thinking.String()}
		return res
	}

	// ReadBytes has no line-length limit, unlike bufio.Scanner's default.
	r := bufio.NewReaderSize(resp.Body, 64*1024)
	gotFirst, done := false, false
	for !done {
		line, rerr := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if !gotFirst {
				gotFirst = true
				wd.reset(c.t.InterToken, "waiting for the next token")
			} else {
				wd.kick()
			}
			var ch chatChunk
			if err := json.Unmarshal(line, &ch); err != nil {
				return partial(), fmt.Errorf("ollama: malformed stream data: %w", err)
			}
			if ch.Error != "" {
				return partial(), fmt.Errorf("ollama: %s", ch.Error)
			}
			if ch.Message.Content != "" || ch.Message.Thinking != "" {
				content.WriteString(ch.Message.Content)
				thinking.WriteString(ch.Message.Thinking)
				if onDelta != nil {
					onDelta(Message{Content: ch.Message.Content, Thinking: ch.Message.Thinking})
				}
			}
			calls = append(calls, ch.Message.ToolCalls...)
			if ch.Done {
				done = true
				res.DoneReason = ch.DoneReason
				res.PromptEvalCount = ch.PromptEvalCount
				res.EvalCount = ch.EvalCount
				res.LoadDuration = time.Duration(ch.LoadDuration)
				res.PromptDuration = time.Duration(ch.PromptEvalDuration)
				res.EvalDuration = time.Duration(ch.EvalDuration)
			}
		}
		if rerr != nil {
			if done {
				break
			}
			fired, _ := wd.firedPhase()
			if errors.Is(rerr, io.EOF) && parent.Err() == nil && !fired {
				return partial(), errors.New("ollama: the stream ended before the response was complete")
			}
			return partial(), c.classify(parent, wd, rerr)
		}
	}
	out := partial()
	out.Message.ToolCalls = calls
	return out, nil
}

// classify turns a transport error into a cancellation, a stall or a plain error.
func (c *Client) classify(parent context.Context, wd *watchdog, err error) error {
	if fired, phase := wd.firedPhase(); fired {
		return fmt.Errorf("%w (%s)", ErrStalled, phase)
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	return fmt.Errorf("ollama: %w", err)
}

// Load loads a model into memory without generating anything (a chat
// request with no messages). Options must match later requests, num_ctx in
// particular, or Ollama will reload the model on the first real request.
func (c *Client) Load(ctx context.Context, model string, options map[string]any, keepAlive string) (time.Duration, error) {
	if c.t.Load > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.t.Load)
		defer cancel()
	}
	req := ChatRequest{Model: model, Messages: []Message{}, Options: options, KeepAlive: keepAlive}
	var out struct {
		Error string `json:"error"`
	}
	start := time.Now()
	if err := c.postJSON(ctx, "/api/chat", req, &out); err != nil {
		return 0, err
	}
	if out.Error != "" {
		return 0, errors.New(out.Error)
	}
	return time.Since(start), nil
}

type RunningModel struct {
	Name          string    `json:"name"`
	Model         string    `json:"model"`
	Size          int64     `json:"size"`
	SizeVRAM      int64     `json:"size_vram"`
	ExpiresAt     time.Time `json:"expires_at"`
	ContextLength int       `json:"context_length"`
}

// Matches reports whether this running model is the configured model.
func (m RunningModel) Matches(model string) bool {
	return m.Name == model || m.Model == model || m.Name == model+":latest"
}

// PS lists the models currently loaded in memory.
func (c *Client) PS(ctx context.Context) ([]RunningModel, error) {
	var out struct {
		Models []RunningModel `json:"models"`
	}
	if err := c.getJSON(ctx, "/api/ps", &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

// Version returns the Ollama server version.
func (c *Client) Version(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	if err := c.getJSON(ctx, "/api/version", &out); err != nil {
		return "", err
	}
	return out.Version, nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	return c.do(ctx, req, out)
}

func (c *Client) postJSON(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(ctx, req, out)
}

func (c *Client) do(ctx context.Context, req *http.Request, out any) error {
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("ollama: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama %s: %s", resp.Status, apiError(b))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

func apiError(b []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return e.Error
	}
	if s := strings.TrimSpace(string(b)); s != "" {
		return s
	}
	return "(empty response)"
}

// watchdog cancels a request when no data arrives within the current
// phase's timeout. kick() restarts the countdown after each chunk.
type watchdog struct {
	mu     sync.Mutex
	d      time.Duration
	phase  string
	timer  *time.Timer
	fired  bool
	cancel context.CancelFunc
}

func newWatchdog(d time.Duration, phase string, cancel context.CancelFunc) *watchdog {
	w := &watchdog{cancel: cancel}
	w.reset(d, phase)
	return w
}

func (w *watchdog) reset(d time.Duration, phase string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.d, w.phase = d, phase
	if d > 0 {
		w.timer = time.AfterFunc(d, w.fire)
	}
}

func (w *watchdog) kick() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Reset(w.d)
	}
}

func (w *watchdog) fire() {
	w.mu.Lock()
	w.fired = true
	w.mu.Unlock()
	w.cancel()
}

func (w *watchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
}

func (w *watchdog) firedPhase() (bool, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired, w.phase
}
