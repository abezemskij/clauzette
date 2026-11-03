package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"clauzette/internal/llm"
)

// fakeProvider serves /models and /chat/completions. reply writes the SSE
// body for a chat request (after it has been recorded).
type fakeProvider struct {
	mu       sync.Mutex
	requests []map[string]any
	auth     []string
	status   []int // status per attempt; 0 or past the end = 200
	headers  http.Header
	reply    func(w io.Writer)
}

func (f *fakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/models":
		w.Write([]byte(`{"data": [
			{"id": "free/tooly:free", "context_length": 131072, "pricing": {"prompt": "0", "completion": "0"},
			 "supported_parameters": ["tools", "reasoning", "temperature"], "architecture": {"input_modalities": ["text", "image"]}},
			{"id": "paid/big", "context_length": 200000, "pricing": {"prompt": "0.000003", "completion": "0.000015"}, "supported_parameters": ["tools"]},
			{"id": "free/chat:free", "context_length": 32768, "pricing": {"prompt": "0", "completion": "0"}, "supported_parameters": ["temperature"]}
		]}`))
	case "/chat/completions":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.requests = append(f.requests, body)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		n := len(f.requests)
		status := 0
		if n <= len(f.status) {
			status = f.status[n-1]
		}
		f.mu.Unlock()
		for k, v := range f.headers {
			w.Header()[k] = v
		}
		if status != 0 && status != 200 {
			w.WriteHeader(status)
			w.Write([]byte(`{"error": {"message": "Rate limit exceeded", "code": 429}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f.reply(w)
	default:
		http.NotFound(w, r)
	}
}

func sse(w io.Writer, chunks ...string) {
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func newTestClient(t *testing.T, f *fakeProvider, opt Options) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	opt.BaseURL = srv.URL
	if opt.Name == "" {
		opt.Name = "openrouter"
	}
	if opt.KeyEnv == "" {
		opt.KeyEnv = "OPENROUTER_API_KEY"
	}
	opt.Timeouts = llm.Timeouts{Connect: time.Second, FirstToken: 5 * time.Second, InterToken: 5 * time.Second}
	c := New(opt)
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func TestStreamContentReasoningToolsUsage(t *testing.T) {
	f := &fakeProvider{reply: func(w io.Writer) {
		fmt.Fprint(w, ": OPENROUTER PROCESSING\n\n") // keep-alive comment
		sse(w,
			`{"choices": [{"delta": {"role": "assistant", "reasoning": "I should read "}}]}`,
			`{"choices": [{"delta": {"reasoning": "the file."}}]}`,
			`{"choices": [{"delta": {"content": "Reading it."}}]}`,
			`{"choices": [{"delta": {"tool_calls": [{"index": 0, "id": "call_abc", "type": "function", "function": {"name": "read_file", "arguments": "{\"pa"}}]}}]}`,
			`{"choices": [{"delta": {"tool_calls": [{"index": 0, "function": {"arguments": "th\": \"a.txt\"}"}}]}}]}`,
			`{"choices": [{"delta": {"tool_calls": [{"index": 1, "id": "call_def", "function": {"name": "list_dir", "arguments": "{}"}}]}}]}`,
			`{"choices": [{"delta": {}, "finish_reason": "tool_calls"}]}`,
			`{"choices": [], "usage": {"prompt_tokens": 1234, "completion_tokens": 56}}`,
		)
	}}
	c := newTestClient(t, f, Options{APIKey: "sk-test"})
	var shown strings.Builder
	think := true
	res, err := c.ChatStream(context.Background(), llm.ChatRequest{Model: "free/tooly:free", Think: &think,
		Messages: []llm.Message{{Role: "user", Content: "read a.txt"}}}, func(d llm.Message) {
		shown.WriteString(d.Thinking + "|" + d.Content + ";")
	})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Message
	if m.Thinking != "I should read the file." || m.Content != "Reading it." {
		t.Fatalf("thinking %q content %q", m.Thinking, m.Content)
	}
	if len(m.ToolCalls) != 2 || m.ToolCalls[0].ID != "call_abc" || m.ToolCalls[0].Function.Name != "read_file" ||
		m.ToolCalls[0].Function.Arguments["path"] != "a.txt" || m.ToolCalls[1].Function.Name != "list_dir" {
		t.Fatalf("tool calls: %+v", m.ToolCalls)
	}
	if res.PromptEvalCount != 1234 || res.EvalCount != 56 || res.DoneReason != "tool_calls" {
		t.Fatalf("usage %d/%d done %q", res.PromptEvalCount, res.EvalCount, res.DoneReason)
	}
	if !strings.Contains(shown.String(), "I should read |;") {
		t.Fatalf("deltas: %q", shown.String())
	}
	if f.auth[0] != "Bearer sk-test" {
		t.Fatalf("auth header %q", f.auth[0])
	}
	if r, _ := f.requests[0]["reasoning"].(map[string]any); r["enabled"] != true {
		t.Fatalf("reasoning not requested: %v", f.requests[0]["reasoning"])
	}
}

func TestRequestMapping(t *testing.T) {
	f := &fakeProvider{reply: func(w io.Writer) { sse(w, `{"choices": [{"delta": {"content": "ok"}, "finish_reason": "stop"}]}`) }}
	c := newTestClient(t, f, Options{APIKey: "k"})
	_, err := c.ChatStream(context.Background(), llm.ChatRequest{
		Model: "free/tooly:free",
		Messages: []llm.Message{
			{Role: "system", Content: "S"},
			{Role: "user", Content: "U"},
			// a session saved before tool-call ids: they are made up and matched
			{Role: "assistant", Thinking: "secret", ToolCalls: []llm.ToolCall{
				{Function: llm.ToolCallFunction{Name: "a", Arguments: map[string]any{"x": 1.0}}},
				{Function: llm.ToolCallFunction{Name: "b"}}}},
			{Role: "tool", ToolName: "a", Content: "ra"},
			{Role: "tool", ToolName: "b", Content: "rb"},
		},
		Tools:   []llm.ToolSpec{{Type: "function", Function: llm.FunctionSpec{Name: "a", Parameters: map[string]any{"type": "object"}}}},
		Options: map[string]any{"temperature": 0.2, "num_ctx": 131072.0, "num_predict": 64.0},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := f.requests[0]
	msgs := req["messages"].([]any)
	asst := msgs[2].(map[string]any)
	if asst["content"] != nil {
		t.Fatalf("an assistant message with only tool calls must have null content: %v", asst["content"])
	}
	if _, ok := asst["reasoning"]; ok {
		t.Fatal("earlier thinking must not be sent back")
	}
	calls := asst["tool_calls"].([]any)
	id1 := calls[0].(map[string]any)["id"]
	id2 := calls[1].(map[string]any)["id"]
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != `{"x":1}` || calls[1].(map[string]any)["function"].(map[string]any)["arguments"] != "{}" {
		t.Fatalf("arguments must be JSON strings: %v", calls)
	}
	if msgs[3].(map[string]any)["tool_call_id"] != id1 || msgs[4].(map[string]any)["tool_call_id"] != id2 || id1 == id2 {
		t.Fatalf("tool results not matched to calls: %v %v / %v %v", msgs[3], msgs[4], id1, id2)
	}
	if req["temperature"] != 0.2 || req["max_tokens"] != 64.0 || req["num_ctx"] != nil || req["stream"] != true {
		t.Fatalf("options: %v", req)
	}
	if _, ok := req["reasoning"]; ok {
		t.Fatal("no think setting means no reasoning field")
	}
}

func TestCatalog(t *testing.T) {
	c := newTestClient(t, &fakeProvider{}, Options{})
	list, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]llm.ModelInfo{}
	for _, m := range list {
		byName[m.Name] = m
	}
	tooly := byName["free/tooly:free"]
	if !tooly.Has("tools") || !tooly.Has("thinking") || !tooly.Has("vision") || tooly.Details.ContextLength != 131072 || !tooly.Free() {
		t.Fatalf("tooly: %+v", tooly)
	}
	if chat := byName["free/chat:free"]; chat.Has("tools") || !chat.Free() {
		t.Fatalf("chat: %+v", chat)
	}
	if paid := byName["paid/big"]; paid.Free() || paid.Pricing.Prompt != 0.000003 {
		t.Fatalf("paid: %+v", paid.Pricing)
	}
}

func TestFreeOnly(t *testing.T) {
	f := &fakeProvider{reply: func(w io.Writer) { sse(w, `{"choices": [{"delta": {"content": "ok"}, "finish_reason": "stop"}]}`) }}
	c := newTestClient(t, f, Options{APIKey: "k", FreeOnly: true})
	ctx := context.Background()
	if _, err := c.ChatStream(ctx, llm.ChatRequest{Model: "paid/big"}, nil); err == nil || !strings.Contains(err.Error(), "not free") {
		t.Fatalf("a paid model was allowed: %v", err)
	}
	if _, err := c.ChatStream(ctx, llm.ChatRequest{Model: "unlisted/x"}, nil); err == nil {
		t.Fatal("an unlisted model was allowed")
	}
	if len(f.requests) != 0 {
		t.Fatal("a refused model must not be sent at all")
	}
	if _, err := c.ChatStream(ctx, llm.ChatRequest{Model: "free/tooly:free"}, nil); err != nil {
		t.Fatalf("a free model was refused: %v", err)
	}
}

func TestMissingKey(t *testing.T) {
	c := newTestClient(t, &fakeProvider{}, Options{})
	_, err := c.ChatStream(context.Background(), llm.ChatRequest{Model: "free/tooly:free"}, nil)
	if err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Fatalf("err = %v", err)
	}
}

func TestRetryOnShortRateLimit(t *testing.T) {
	f := &fakeProvider{status: []int{429, 503}, reply: func(w io.Writer) {
		sse(w, `{"choices": [{"delta": {"content": "finally"}, "finish_reason": "stop"}]}`)
	}}
	c := newTestClient(t, f, Options{APIKey: "k"})
	res, err := c.ChatStream(context.Background(), llm.ChatRequest{Model: "free/tooly:free"}, nil)
	if err != nil || res.Message.Content != "finally" || len(f.requests) != 3 {
		t.Fatalf("err %v, %d attempts", err, len(f.requests))
	}
}

func TestDailyLimitIsNotRetried(t *testing.T) {
	tomorrow := time.Now().Add(10 * time.Hour)
	f := &fakeProvider{status: []int{429, 429, 429, 429},
		headers: http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {fmt.Sprint(tomorrow.UnixMilli())}},
		reply:   func(w io.Writer) {}}
	c := newTestClient(t, f, Options{APIKey: "k"})
	_, err := c.ChatStream(context.Background(), llm.ChatRequest{Model: "free/tooly:free"}, nil)
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Reset.IsZero() || len(f.requests) != 1 {
		t.Fatalf("err %v (%T), %d attempts", err, err, len(f.requests))
	}
	if !strings.Contains(err.Error(), "resets at") {
		t.Fatalf("the reset time must be shown: %v", err)
	}
}

func TestMidStreamError(t *testing.T) {
	f := &fakeProvider{reply: func(w io.Writer) {
		fmt.Fprint(w, "data: {\"choices\": [{\"delta\": {\"content\": \"part\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"error\": {\"message\": \"Rate limit exceeded\", \"code\": 429}, \"choices\": [{\"delta\": {}, \"finish_reason\": \"error\"}]}\n\n")
	}}
	c := newTestClient(t, f, Options{APIKey: "k"})
	res, err := c.ChatStream(context.Background(), llm.ChatRequest{Model: "free/tooly:free"}, nil)
	var rl *RateLimitError
	if !errors.As(err, &rl) || res == nil || res.Message.Content != "part" {
		t.Fatalf("err %v, partial %+v", err, res)
	}
}

func TestStreamEndsEarly(t *testing.T) {
	f := &fakeProvider{reply: func(w io.Writer) {
		fmt.Fprint(w, "data: {\"choices\": [{\"delta\": {\"content\": \"cut\"}}]}\n\n") // no finish, no [DONE]
	}}
	c := newTestClient(t, f, Options{APIKey: "k"})
	if _, err := c.ChatStream(context.Background(), llm.ChatRequest{Model: "free/tooly:free"}, nil); err == nil {
		t.Fatal("a stream cut short must be an error")
	}
}

func TestInvalidToolArguments(t *testing.T) {
	f := &fakeProvider{reply: func(w io.Writer) {
		sse(w, `{"choices": [{"delta": {"tool_calls": [{"index": 0, "id": "c1", "function": {"name": "read_file", "arguments": "{\"path\": "}}]}, "finish_reason": "tool_calls"}]}`)
	}}
	c := newTestClient(t, f, Options{APIKey: "k"})
	res, err := c.ChatStream(context.Background(), llm.ChatRequest{Model: "free/tooly:free"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if args := res.Message.ToolCalls[0].Function.Arguments; args["_invalid_arguments"] == nil {
		t.Fatalf("broken arguments must reach the tool as such: %v", args)
	}
}

// Keep-alive comments are not tokens: a stream that only sends them still
// times out waiting for the first token.
func TestKeepAliveDoesNotHoldOffTimeout(t *testing.T) {
	f := &fakeProvider{reply: func(w io.Writer) {
		for i := 0; i < 40; i++ {
			if _, err := fmt.Fprint(w, ": OPENROUTER PROCESSING\n\n"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}}
	c := newTestClient(t, f, Options{APIKey: "test-key"})
	c.opt.Timeouts.FirstToken = 300 * time.Millisecond
	start := time.Now()
	_, err := c.ChatStream(context.Background(), llm.ChatRequest{Model: "free/tooly:free", Messages: []llm.Message{{Role: "user", Content: "hi"}}}, nil)
	if !errors.Is(err, llm.ErrStalled) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("the first-token timeout fired late (%s): keep-alives held it off", d)
	}
}
