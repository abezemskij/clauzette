// Package openai is a client for the OpenAI Chat Completions API, which
// OpenRouter, OpenAI and many self-hosted servers (vLLM, LM Studio, ...)
// speak. It implements llm.Backend: streaming chat with tools, and the model
// list. It has nothing local: no loading, no GPU.
//
// Differences from Ollama it absorbs: replies stream as server-sent events;
// tool-call arguments arrive as fragments of a JSON string, assembled per
// call index; tool results name their call with tool_call_id; the key goes
// in an Authorization header. OpenRouter's extras (reasoning, the model
// catalogue with capabilities and prices) are used when available.
package openai

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
	"time"

	"clauzette/internal/llm"
)

// Options configures a client.
type Options struct {
	Name     string // provider name for messages, e.g. "openrouter"
	BaseURL  string // e.g. https://openrouter.ai/api/v1
	APIKey   string // from the environment variable KeyEnv; "" when it is not set
	KeyEnv   string // the variable's name, for messages
	FreeOnly bool   // refuse every model whose price is not known to be zero
	Timeouts llm.Timeouts
}

// Client talks to one provider.
type Client struct {
	opt Options
	hc  *http.Client

	mu        sync.Mutex
	catalog   []llm.ModelInfo
	catalogAt time.Time

	sleep func(context.Context, time.Duration) error // tests replace it
}

// catalogTTL is how long the model list is reused. OpenRouter's has
// hundreds of entries and changes rarely.
const catalogTTL = 10 * time.Minute

// maxRetries is how often a rate-limited or overloaded request is retried
// before the reply has started; longer waits are not retried.
const (
	maxRetries = 3
	maxWait    = 60 * time.Second
)

func New(o Options) *Client {
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	connect := o.Timeouts.Connect
	if connect <= 0 {
		connect = 10 * time.Second
	}
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        4,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &Client{opt: o, hc: &http.Client{Transport: tr}, sleep: sleepCtx}
}

// Name is the provider's name, e.g. "openrouter".
func (c *Client) Name() string { return c.opt.Name }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) keyError() error {
	if c.opt.APIKey != "" || c.opt.KeyEnv == "" {
		return nil
	}
	return fmt.Errorf("%s needs an API key: set the environment variable %s (the key never goes in the config)", c.opt.Name, c.opt.KeyEnv)
}

func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.opt.BaseURL+path, r)
	if err != nil {
		return nil, err
	}
	if c.opt.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.opt.APIKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Title", "clauzette") // OpenRouter's app attribution; ignored elsewhere
	return req, nil
}

// --- model list --------------------------------------------------------------

type wireModel struct {
	ID            string `json:"id"`
	ContextLength int    `json:"context_length"`
	Pricing       *struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
	SupportedParameters []string `json:"supported_parameters"`
	Architecture        *struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
	TopProvider *struct {
		ContextLength int `json:"context_length"`
	} `json:"top_provider"`
}

// ListModels returns the provider's models. OpenRouter reports context,
// capabilities and prices; a plain OpenAI-compatible server only names its
// models (their capabilities then stay unknown, and are assumed).
func (c *Client) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	c.mu.Lock()
	if c.catalog != nil && time.Since(c.catalogAt) < catalogTTL {
		out := c.catalog
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.opt.Name, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.opt.Name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: %s", c.opt.Name, resp.Status, apiError(b))
	}
	var out struct {
		Data []wireModel `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("%s: unexpected model list: %w", c.opt.Name, err)
	}
	list := make([]llm.ModelInfo, 0, len(out.Data))
	for _, w := range out.Data {
		list = append(list, w.info())
	}
	c.mu.Lock()
	c.catalog, c.catalogAt = list, time.Now()
	c.mu.Unlock()
	return list, nil
}

func (w wireModel) info() llm.ModelInfo {
	mi := llm.ModelInfo{Name: w.ID, Model: w.ID}
	mi.Details.ContextLength = w.ContextLength
	if mi.Details.ContextLength == 0 && w.TopProvider != nil {
		mi.Details.ContextLength = w.TopProvider.ContextLength
	}
	if w.SupportedParameters != nil {
		mi.Capabilities = []string{"completion"}
		has := func(p string) bool {
			for _, s := range w.SupportedParameters {
				if s == p {
					return true
				}
			}
			return false
		}
		if has("tools") {
			mi.Capabilities = append(mi.Capabilities, "tools")
		}
		if has("reasoning") || has("include_reasoning") {
			mi.Capabilities = append(mi.Capabilities, "thinking")
		}
		if w.Architecture != nil {
			for _, m := range w.Architecture.InputModalities {
				if m == "image" {
					mi.Capabilities = append(mi.Capabilities, "vision")
				}
			}
		}
	}
	if w.Pricing != nil {
		p, err1 := strconv.ParseFloat(w.Pricing.Prompt, 64)
		q, err2 := strconv.ParseFloat(w.Pricing.Completion, 64)
		if err1 == nil && err2 == nil {
			mi.Pricing = &llm.Pricing{Prompt: p, Completion: q}
		}
	}
	return mi
}

// checkFree enforces free_only for a model.
func (c *Client) checkFree(ctx context.Context, model string) error {
	if !c.opt.FreeOnly {
		return nil
	}
	list, err := c.ListModels(ctx)
	if err != nil {
		return fmt.Errorf("free_only is on, and the model list needed to check %s could not be read: %w", model, err)
	}
	for _, mi := range list {
		if mi.Name != model {
			continue
		}
		if mi.Free() {
			return nil
		}
		if mi.Pricing == nil {
			return fmt.Errorf("free_only is on and %s reports no price for %s, so it is not used", c.opt.Name, model)
		}
		return fmt.Errorf("free_only is on and %s is not free (%s per million prompt tokens, %s per million completion tokens)",
			model, dollars(mi.Pricing.Prompt), dollars(mi.Pricing.Completion))
	}
	return fmt.Errorf("free_only is on and %s does not list %s", c.opt.Name, model)
}

func dollars(perToken float64) string { return fmt.Sprintf("$%.2f", perToken*1e6) }

// --- chat --------------------------------------------------------------------

type wireCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireMessage struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"` // null for an assistant message that only calls tools
	ToolCalls  []wireCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type wireRequest struct {
	Model         string         `json:"model"`
	Messages      []wireMessage  `json:"messages"`
	Tools         []llm.ToolSpec `json:"tools,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	Temperature *float64       `json:"temperature,omitempty"`
	TopP        *float64       `json:"top_p,omitempty"`
	MaxTokens   *int           `json:"max_tokens,omitempty"`
	Seed        *int           `json:"seed,omitempty"`
	Stop        []string       `json:"stop,omitempty"`
	Reasoning   *wireReasoning `json:"reasoning,omitempty"`
}

// wireReasoning is OpenRouter's unified reasoning setting.
type wireReasoning struct {
	Enabled bool `json:"enabled"`
}

// buildRequest maps a neutral request onto the wire. Thinking from earlier
// turns is not sent back (it is optional for these APIs). Tool calls and
// results without ids (sessions saved before ids existed) get matching ones.
func (c *Client) buildRequest(req llm.ChatRequest) wireRequest {
	w := wireRequest{Model: req.Model, Tools: req.Tools, Stream: true}
	w.StreamOptions = &struct {
		IncludeUsage bool `json:"include_usage"`
	}{true}
	var pending []string
	next := 0
	for _, m := range req.Messages {
		content := m.Content
		wm := wireMessage{Role: m.Role, Content: &content}
		switch m.Role {
		case "assistant":
			pending = pending[:0]
			for _, tc := range m.ToolCalls {
				id := tc.ID
				if id == "" {
					next++
					id = fmt.Sprintf("call_%d", next)
				}
				pending = append(pending, id)
				args, _ := json.Marshal(tc.Function.Arguments)
				if tc.Function.Arguments == nil {
					args = []byte("{}")
				}
				var wc wireCall
				wc.ID, wc.Type = id, "function"
				wc.Function.Name, wc.Function.Arguments = tc.Function.Name, string(args)
				wm.ToolCalls = append(wm.ToolCalls, wc)
			}
			if len(wm.ToolCalls) > 0 && content == "" {
				wm.Content = nil
			}
		case "tool":
			wm.ToolCallID = m.ToolCallID
			if wm.ToolCallID == "" && len(pending) > 0 {
				wm.ToolCallID = pending[0]
			}
			if len(pending) > 0 && pending[0] == wm.ToolCallID {
				pending = pending[1:]
			}
		}
		w.Messages = append(w.Messages, wm)
	}
	o := req.Options
	if v, ok := number(o["temperature"]); ok {
		w.Temperature = &v
	}
	if v, ok := number(o["top_p"]); ok {
		w.TopP = &v
	}
	if v, ok := number(o["num_predict"]); ok && v > 0 {
		n := int(v)
		w.MaxTokens = &n
	}
	if v, ok := number(o["seed"]); ok {
		n := int(v)
		w.Seed = &n
	}
	if stop, ok := o["stop"].([]any); ok {
		for _, s := range stop {
			if str, ok := s.(string); ok {
				w.Stop = append(w.Stop, str)
			}
		}
	}
	if req.Think != nil && c.opt.Name == "openrouter" {
		w.Reasoning = &wireReasoning{Enabled: *req.Think}
	}
	return w
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

type wireChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *wireError `json:"error"`
}

type wireError struct {
	Message string `json:"message"`
	Code    any    `json:"code"`
}

// RateLimitError is a rate limit that clauzette does not wait out: the
// daily free-model quota, or a wait longer than it retries for.
type RateLimitError struct {
	Provider string
	Reset    time.Time // zero when unknown
	Message  string
}

func (e *RateLimitError) Error() string {
	s := fmt.Sprintf("%s rate limit: %s", e.Provider, e.Message)
	if !e.Reset.IsZero() {
		s += fmt.Sprintf(" (resets at %s)", e.Reset.Local().Format("15:04"))
	}
	return s
}

// ChatStream sends a chat request and streams the reply. onDelta gets every
// piece of content or reasoning. On error the Result holds what arrived.
func (c *Client) ChatStream(ctx context.Context, req llm.ChatRequest, onDelta func(llm.Message)) (*llm.Result, error) {
	if err := c.keyError(); err != nil {
		return nil, err
	}
	if err := c.checkFree(ctx, req.Model); err != nil {
		return nil, err
	}
	body, err := json.Marshal(c.buildRequest(req))
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		res, wait, err := c.stream(ctx, body, onDelta)
		if wait <= 0 || attempt >= maxRetries {
			return res, err
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// stream makes one attempt. A positive wait means "retry after this long":
// the request was rate-limited or the provider overloaded before any reply.
func (c *Client) stream(ctx context.Context, body []byte, onDelta func(llm.Message)) (*llm.Result, time.Duration, error) {
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wd := llm.NewWatchdog(c.opt.Timeouts.FirstToken, "waiting for the first token", cancel)
	defer wd.Stop()

	hreq, err := c.newRequest(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return nil, 0, err
	}
	hreq.Header.Set("Accept", "text/event-stream")
	start := time.Now()
	resp, err := c.hc.Do(hreq)
	if err != nil {
		return nil, 0, c.classify(parent, wd, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, c.retryAfter(resp), c.statusError(resp, b)
	}

	var content, thinking strings.Builder
	type call struct {
		id, name string
		args     strings.Builder
	}
	var calls []*call
	byIndex := map[int]*call{}
	res := &llm.Result{}
	var firstAt time.Time
	partial := func() *llm.Result {
		res.Message = llm.Message{Role: "assistant", Content: content.String(), Thinking: thinking.String()}
		return res
	}

	r := bufio.NewReaderSize(resp.Body, 64*1024)
	gotFirst, done := false, false
	for !done {
		line, rerr := r.ReadBytes('\n')
		text := strings.TrimRight(string(line), "\r\n")
		// Only data counts as progress: keep-alive comments (": OPENROUTER
		// PROCESSING") arrive while nothing is generated, and must not hold
		// off the first-token or inter-token timeout forever.
		if strings.HasPrefix(text, "data:") {
			if !gotFirst {
				gotFirst = true
				wd.Reset(c.opt.Timeouts.InterToken, "waiting for the next token")
			} else {
				wd.Kick()
			}
		}
		switch {
		case text == "" || strings.HasPrefix(text, ":"):
			// blank separator, or a keep-alive comment (": OPENROUTER PROCESSING")
		case !strings.HasPrefix(text, "data:"):
			// other SSE fields (event:, id:) carry nothing we use
		default:
			data := strings.TrimSpace(strings.TrimPrefix(text, "data:"))
			if data == "[DONE]" {
				done = true
				break
			}
			var ch wireChunk
			if err := json.Unmarshal([]byte(data), &ch); err != nil {
				return partial(), 0, fmt.Errorf("%s: malformed stream data: %w", c.opt.Name, err)
			}
			if ch.Error != nil {
				return partial(), 0, c.chunkError(ch.Error)
			}
			if u := ch.Usage; u != nil {
				res.PromptEvalCount, res.EvalCount = u.PromptTokens, u.CompletionTokens
			}
			for _, choice := range ch.Choices {
				d := choice.Delta
				reasoning := d.Reasoning + d.ReasoningContent
				if d.Content != "" || reasoning != "" {
					if firstAt.IsZero() {
						firstAt = time.Now()
					}
					content.WriteString(d.Content)
					thinking.WriteString(reasoning)
					if onDelta != nil {
						onDelta(llm.Message{Content: d.Content, Thinking: reasoning})
					}
				}
				for _, tc := range d.ToolCalls {
					cl := byIndex[tc.Index]
					if cl == nil {
						cl = &call{}
						byIndex[tc.Index] = cl
						calls = append(calls, cl)
						if firstAt.IsZero() {
							firstAt = time.Now()
						}
					}
					if tc.ID != "" {
						cl.id = tc.ID
					}
					if tc.Function.Name != "" {
						cl.name = tc.Function.Name
					}
					cl.args.WriteString(tc.Function.Arguments)
				}
				if choice.FinishReason != "" {
					if choice.FinishReason == "error" {
						return partial(), 0, fmt.Errorf("%s: the reply failed (finish_reason error)", c.opt.Name)
					}
					res.DoneReason = choice.FinishReason
				}
			}
		}
		if rerr != nil {
			if done {
				break
			}
			fired, _ := wd.FiredPhase()
			if errors.Is(rerr, io.EOF) && parent.Err() == nil && !fired {
				if res.DoneReason != "" {
					break // some servers end the stream without [DONE]
				}
				return partial(), 0, fmt.Errorf("%s: the stream ended before the reply was complete", c.opt.Name)
			}
			return partial(), 0, c.classify(parent, wd, rerr)
		}
	}
	out := partial()
	for _, cl := range calls {
		args := map[string]any{}
		if raw := strings.TrimSpace(cl.args.String()); raw != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				// The tool reports what is missing, so the model can try again.
				args = map[string]any{"_invalid_arguments": raw}
			}
		}
		out.Message.ToolCalls = append(out.Message.ToolCalls, llm.ToolCall{ID: cl.id, Function: llm.ToolCallFunction{Name: cl.name, Arguments: args}})
	}
	if !firstAt.IsZero() {
		out.PromptDuration = firstAt.Sub(start)
		out.EvalDuration = time.Since(firstAt)
	}
	if out.Message.Content == "" && out.Message.Thinking == "" && len(out.Message.ToolCalls) == 0 && res.DoneReason == "" {
		return out, 0, fmt.Errorf("%s: the reply was empty", c.opt.Name)
	}
	return out, 0, nil
}

// retryAfter decides whether a failed request is worth retrying, and when.
// The daily free quota, or any reset further away than maxWait, is not.
func (c *Client) retryAfter(resp *http.Response) time.Duration {
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable:
	default:
		return 0
	}
	if reset := rateLimitReset(resp); !reset.IsZero() && time.Until(reset) > maxWait {
		return 0
	}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			d := time.Duration(n) * time.Second
			if d > maxWait {
				return 0
			}
			return max(d, time.Second)
		}
	}
	if reset := rateLimitReset(resp); !reset.IsZero() {
		return max(time.Until(reset), time.Second)
	}
	return 5 * time.Second
}

// rateLimitReset reads X-RateLimit-Reset (milliseconds since the epoch on
// OpenRouter).
func rateLimitReset(resp *http.Response) time.Time {
	s := resp.Header.Get("X-RateLimit-Reset")
	if s == "" {
		return time.Time{}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	if n < 1e12 { // seconds, not milliseconds
		return time.Unix(n, 0)
	}
	return time.UnixMilli(n)
}

func (c *Client) statusError(resp *http.Response, body []byte) error {
	msg := apiError(body)
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s rejected the API key (%s): check %s", c.opt.Name, msg, c.opt.KeyEnv)
	case http.StatusPaymentRequired:
		return fmt.Errorf("%s: %s (payment required; free models need no credit)", c.opt.Name, msg)
	case http.StatusTooManyRequests:
		reset := rateLimitReset(resp)
		if reset.IsZero() || time.Until(reset) > maxWait || resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return &RateLimitError{Provider: c.opt.Name, Reset: reset, Message: msg}
		}
	}
	return fmt.Errorf("%s %s: %s", c.opt.Name, resp.Status, msg)
}

func (c *Client) chunkError(e *wireError) error {
	if code, ok := number(e.Code); ok && int(code) == http.StatusTooManyRequests {
		return &RateLimitError{Provider: c.opt.Name, Message: e.Message}
	}
	return fmt.Errorf("%s: %s", c.opt.Name, e.Message)
}

func (c *Client) classify(parent context.Context, wd *llm.Watchdog, err error) error {
	if fired, phase := wd.FiredPhase(); fired {
		return fmt.Errorf("%w (%s)", llm.ErrStalled, phase)
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	return fmt.Errorf("%s: %w", c.opt.Name, err)
}

func apiError(b []byte) string {
	var e struct {
		Error *wireError `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != nil && e.Error.Message != "" {
		return e.Error.Message
	}
	if s := strings.TrimSpace(string(b)); s != "" {
		if len(s) > 300 {
			s = s[:300] + "..."
		}
		return s
	}
	return "(empty response)"
}

var _ llm.Backend = (*Client)(nil)
