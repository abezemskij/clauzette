package ui

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clauzette/internal/agent"
	"clauzette/internal/config"
	"clauzette/internal/gpu"
	"clauzette/internal/ollama"
	"clauzette/internal/session"
	"clauzette/internal/tools"
)

// The scripted session test drives the whole terminal front end (input,
// commands, approvals, queueing, cloud consent, rendering) against a fake
// Ollama and a fake OpenRouter, and compares the transcript with
// testdata/session.golden. It pins the behaviour, so a refactor of the
// front end must reproduce the transcript exactly. go test -run
// TestScriptedSession -update rewrites the golden file.

var update = flag.Bool("update", false, "rewrite testdata/session.golden")

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fakeOllamaServer has one model with tools and scripted chat replies.
func fakeOllamaServer(t *testing.T) *httptest.Server {
	t.Helper()
	writes := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version": "0.0.0-test"}`)
		case "/api/tags":
			fmt.Fprint(w, `{"models": [{"name": "testmodel:latest", "model": "testmodel:latest", "digest": "abc", "size": 1000,
				"capabilities": ["completion", "tools"], "details": {"family": "test", "parameter_size": "1B", "quantization_level": "Q4", "context_length": 65536}}]}`)
		case "/api/show":
			json.NewEncoder(w).Encode(map[string]any{"modelfile": "FROM sha256-" + strings.Repeat("a", 64), "capabilities": []string{"completion", "tools"},
				"model_info": map[string]any{"test.context_length": 65536}})
		case "/api/ps":
			fmt.Fprint(w, `{"models": [{"name": "testmodel:latest", "model": "testmodel:latest", "size": 1000, "size_vram": 1000, "context_length": 32768}]}`)
		case "/api/chat":
			var req ollama.ChatRequest
			json.NewDecoder(r.Body).Decode(&req)
			if len(req.Messages) == 0 { // load or unload
				fmt.Fprint(w, `{"done": true}`)
				return
			}
			msg := scriptedReply(req.Messages, writes)
			json.NewEncoder(w).Encode(map[string]any{"message": msg, "done": true, "done_reason": "stop", "prompt_eval_count": 0, "eval_count": 3})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// scriptedReply answers a chat request; writes (one counter per fake
// server) makes each scripted write differ, so a retried one is not a no-op.
func scriptedReply(msgs []ollama.Message, writes *atomic.Int64) ollama.Message {
	last := msgs[len(msgs)-1]
	say := func(s string) ollama.Message { return ollama.Message{Role: "assistant", Content: s} }
	call := func(name string, args map[string]any) ollama.Message {
		return ollama.Message{Role: "assistant", ToolCalls: []ollama.ToolCall{{Function: ollama.ToolCallFunction{Name: name, Arguments: args}}}}
	}
	if last.Role == "tool" {
		return say("Result noted: " + strings.SplitN(last.Content, "\n", 2)[0])
	}
	switch text := last.Content; {
	case text == "hello":
		return say("Hi there. **Bold** and `code`.\n\n- one\n- two")
	case text == "write a file":
		return call("write_file", map[string]any{"path": "a.txt", "content": fmt.Sprintf("x%d\n", writes.Add(1))})
	case text == "delete it":
		return call("delete_file", map[string]any{"path": "a.txt"})
	case text == "make another file":
		return call("write_file", map[string]any{"path": "b.txt", "content": "y\n"})
	case text == "slow":
		time.Sleep(300 * time.Millisecond)
		return say("slow reply")
	case strings.Contains(text, "approved planned actions"):
		return say("Plan results seen.")
	default:
		return say("ok: " + strings.SplitN(text, "\n", 2)[0])
	}
}

// fakeCloudServer is an OpenRouter stand-in with one free model.
func fakeCloudServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			fmt.Fprint(w, `{"data": [{"id": "free/tooly:free", "context_length": 65536, "pricing": {"prompt": "0", "completion": "0"}, "supported_parameters": ["tools"]}]}`)
		case "/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `data: {"choices": [{"index": 0, "delta": {"role": "assistant", "content": "hello from the cloud"}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type scriptStep struct {
	send string // what the operator types (may hold several lines)
	wait string // regexp the output must match (from the send on) before the next step; "" = the prompt
}

const promptRE = `\] › $`

func TestScriptedSession(t *testing.T) {
	ollamaSrv := fakeOllamaServer(t)
	cloudSrv := fakeCloudServer(t)
	t.Setenv("CLAUZETTE_TEST_UI_KEY", "test-key-value")

	root := t.TempDir()
	ws, err := tools.NewWorkspace(filepath.Join(root, "ws"))
	if err != nil {
		os.MkdirAll(filepath.Join(root, "ws"), 0o755)
		if ws, err = tools.NewWorkspace(filepath.Join(root, "ws")); err != nil {
			t.Fatal(err)
		}
	}
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.OllamaURL = ollamaSrv.URL
	cfg.Model = "testmodel:latest"
	cfg.NumCtx = 32768
	cfg.Color = "never"
	cfg.Providers = map[string]config.ProviderConfig{"openrouter": {BaseURL: cloudSrv.URL, APIKeyEnv: "CLAUZETTE_TEST_UI_KEY", FreeOnly: true}}
	client := ollama.New(cfg.OllamaURL, ollama.Timeouts{Connect: time.Second, FirstToken: 10 * time.Second, InterToken: 10 * time.Second, Load: 10 * time.Second})
	profile, _ := cfg.FindProfile(cfg.DefaultProfileName())
	models := agent.NewModels(cfg, nil, profile)
	reg := tools.NewRegistry(&tools.ReadFile{WS: ws, DefaultLines: 100}, &tools.WriteFile{WS: ws}, &tools.DeleteFile{WS: ws})
	deps := agent.Deps{Cfg: cfg, Client: client, Models: models, Registry: reg, Store: store, Guard: gpu.New(cfg.GPU), WS: ws,
		Todos: tools.NewTodoList(), Fetch: tools.NewFetchPolicy(false, nil)}

	n := 0
	ids := []string{"S1", "S2", "S3", "S4"}
	newSession := func() (*session.Session, []string, error) {
		act := models.Active()
		s := &session.Session{ID: ids[n%len(ids)], Profile: act.Profile, Model: act.Model, Date: "2026-01-01",
			System: "NORMAL", SystemRogue: "ROGUE", SystemChat: "CHAT", Think: act.Think}
		n++
		return s, nil, store.Save(s)
	}
	buildPrompts := func(string) (agent.Prompts, []string, error) {
		return agent.Prompts{Normal: "NORMAL", Rogue: "ROGUE", Chat: "CHAT"}, nil, nil
	}
	sess, _, _ := newSession()

	u := New(Options{Deps: deps, NewSession: newSession, BuildPrompts: buildPrompts}, sess)
	inR, inW := io.Pipe()
	out := &syncBuf{}
	u.in, u.out, u.tty, u.color = inR, out, false, false
	u.md.plain = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- u.Run(ctx) }()

	waitFor := func(from int, re string) {
		t.Helper()
		rx := regexp.MustCompile(re)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if s := out.String(); len(s) >= from && rx.MatchString(s[from:]) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %q; output so far:\n%s", re, out.String())
	}
	waitFor(0, `already loaded`)
	time.Sleep(100 * time.Millisecond) // the rest of the warm-up report

	steps := []scriptStep{
		{"hello", ""},
		{"!think", ""},
		{"!show on", ""},
		{"!bogus", ""},
		{"/bogus", ""},
		{"!c", ""},
		{"/context", ""},
		{"write a file", `\(view all\): $`},
		{"v", `\(view all\): $`},
		{"maybe", `\(view all\): $`},
		{"y", ""},
		{"delete it", `\(view all\): $`},
		{"n not now", ""},
		{"/undo", ""},
		{"/retry", `\(view all\): $`},
		{"a", ""},
		{"/approvals", ""},
		{"!plan on", ""},
		{"make another file", ""},
		{"/plan", ""},
		{"/plan approve looks good", ""},
		{"/plan approve all looks good", `Plan results seen\.[\s\S]*` + promptRE},
		{"!plan off", ""},
		{"slow\nqueued while busy", `ok: queued while busy[\s\S]*` + promptRE},
		{"/compact", ""},
		{"/safe", ""},
		{"/fetch", ""},
		{"/fetch allow example.com", ""},
		{"/rogue on", ""},
		{"/model testmodel:latest", ""},
		{"/rogue off", ""},
		{"/gpu", ""},
		{"/system-prompt", ""},
		{"/model openrouter:free/tooly:free", `Switch\? \[y/N\]\n?.*$`},
		{"/gpu", ""}, // a command answers "no" and still runs
		{"/model openrouter:free/tooly:free", `Switch\? \[y/N\]\n?.*$`},
		{"y", ""},
		{"cloud question", ""},
		{"/new", `Send\? \[y/N\]\n?.*$`},
		{"n", ""},
		{"after new", `Send\? \[y/N\]\n?.*$`},
		{"y", ""},
		{"/sessions", ""},
		{"/clear", `\[y/N\]\n?.*$`},
		{"/sessions", ""}, // keeps the session, then lists
		{"/clear", `\[y/N\]\n?.*$`},
		{"y", `Send\? \[y/N\][\s\S]*` + promptRE},
		{"/load S1", ""}, // declines sending, then loads S1 (which approved the endpoint earlier)
		{"/model testmodel:latest", ""},
		{"!!bang message", ""},
		{"/exit", `resume with`},
	}
	for _, st := range steps {
		from := len(out.String())
		fmt.Fprintf(inW, "%s\n", st.send)
		re := st.wait
		if re == "" {
			re = promptRE
		}
		waitFor(from, re)
		time.Sleep(20 * time.Millisecond) // let any trailing output arrive
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the UI did not exit")
	}
	inW.Close()

	got := normalize(out.String(), root, ollamaSrv.URL, cloudSrv.URL)
	golden := filepath.Join("testdata", "session.golden")
	if *update {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := 0; i < len(gl) || i < len(wl); i++ {
			var g, w string
			if i < len(gl) {
				g = gl[i]
			}
			if i < len(wl) {
				w = wl[i]
			}
			if g != w {
				t.Fatalf("transcript differs from %s at line %d:\n got: %q\nwant: %q\n\nfull output:\n%s", golden, i+1, g, w, got)
			}
		}
	}
}

var timeRE = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}`)

func normalize(s, root, ollamaURL, cloudURL string) string {
	s = strings.ReplaceAll(s, root, "<root>")
	s = strings.ReplaceAll(s, ollamaURL, "<ollama>")
	s = strings.ReplaceAll(s, cloudURL, "<cloud>")
	return timeRE.ReplaceAllString(s, "<time>")
}
