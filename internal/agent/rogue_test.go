package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clauzette/internal/config"
	"clauzette/internal/ollama"
	"clauzette/internal/session"
	"clauzette/internal/tools"
)

// fakeModel is an Ollama stand-in: each /api/chat request is answered by
// the next scripted reply, and the requests are recorded.
type fakeModel struct {
	mu       sync.Mutex
	replies  []func(req ollama.ChatRequest) ollama.Message
	requests []ollama.ChatRequest
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req ollama.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	n := len(f.requests)
	f.mu.Unlock()
	msg := ollama.Message{Role: "assistant", Content: "(no more scripted replies)"}
	if n <= len(f.replies) {
		if f.replies[n-1] == nil { // a model that never finishes: wait until the client gives up
			<-r.Context().Done()
			return
		}
		msg = f.replies[n-1](req)
	}
	json.NewEncoder(w).Encode(map[string]any{"message": msg, "done": true, "done_reason": "stop", "prompt_eval_count": 1, "eval_count": 1})
}

func (f *fakeModel) request(i int) ollama.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

func (f *fakeModel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func say(text string) func(ollama.ChatRequest) ollama.Message {
	return func(ollama.ChatRequest) ollama.Message { return ollama.Message{Role: "assistant", Content: text} }
}

func call(name string, args map[string]any) func(ollama.ChatRequest) ollama.Message {
	return func(ollama.ChatRequest) ollama.Message {
		return ollama.Message{Role: "assistant", ToolCalls: []ollama.ToolCall{{Function: ollama.ToolCallFunction{Name: name, Arguments: args}}}}
	}
}

func lastContent(req ollama.ChatRequest) string { return req.Messages[len(req.Messages)-1].Content }

func hasTool(req ollama.ChatRequest, name string) bool {
	for _, t := range req.Tools {
		if t.Function.Name == name {
			return true
		}
	}
	return false
}

type rogueRig struct {
	a         *Agent
	model     *fakeModel
	root      string
	mu        sync.Mutex
	approvals []string
}

// newRogueRig builds an agent on a fake model. Approval requests are
// recorded and denied, so a test sees any action that was not auto-run.
func newRogueRig(t *testing.T, replies ...func(ollama.ChatRequest) ollama.Message) *rogueRig {
	t.Helper()
	root := t.TempDir()
	ws, err := tools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	model := &fakeModel{replies: replies}
	srv := httptest.NewServer(model)
	t.Cleanup(srv.Close)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	reg := tools.NewRegistry(&tools.WriteFile{WS: ws}, &tools.DeleteFile{WS: ws})
	events := make(chan Event)
	rig := &rogueRig{model: model, root: root}
	rig.a = New(Deps{
		Cfg: cfg, Registry: reg, Store: store, WS: ws,
		Client: ollama.New(srv.URL, ollama.Timeouts{Connect: time.Second, FirstToken: 10 * time.Second, InterToken: 10 * time.Second}),
	}, &session.Session{ID: "rogue", System: "test"}, events)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case ev := <-events:
				if ev.Kind == EvApproval {
					rig.mu.Lock()
					rig.approvals = append(rig.approvals, ev.Tool)
					rig.mu.Unlock()
					ev.Approval.Reply <- Decision{Approve: false}
				}
			case <-done:
				return
			}
		}
	}()
	return rig
}

func (r *rogueRig) approvalCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.approvals)
}

func TestRogueRunsWithoutApprovalAndFinishes(t *testing.T) {
	r := newRogueRig(t,
		call("write_file", map[string]any{"path": "a.txt", "content": "x\n"}),
		call("finish", map[string]any{"summary": "wrote a.txt"}),
	)
	if _, err := r.a.SetRogue(true, 0); err != nil {
		t.Fatal(err)
	}
	if err := r.a.RunTurn(context.Background(), "write a.txt"); err != nil {
		t.Fatal(err)
	}
	if r.approvalCount() != 0 {
		t.Fatal("rogue mode asked for approval")
	}
	if _, err := os.Stat(filepath.Join(r.root, "a.txt")); err != nil {
		t.Fatal("the write did not run")
	}
	if r.model.count() != 2 {
		t.Fatalf("finish should end the task after 2 requests, made %d", r.model.count())
	}
	first, second := r.model.request(0), r.model.request(1)
	if !hasTool(first, "finish") {
		t.Fatal("the finish tool should be offered in rogue mode")
	}
	if !strings.Contains(first.Messages[1].Content, "Rogue mode is now ON") {
		t.Fatalf("the model should be told rogue mode is on before the task: %q", first.Messages[1].Content)
	}
	if got := lastContent(second); !strings.Contains(got, "[rogue mode: 99 of 100 steps left]") {
		t.Fatalf("tool result should carry the steps left: %q", got)
	}
}

func TestRogueNudgesOnceThenEnds(t *testing.T) {
	r := newRogueRig(t, say("Should I go ahead?"), say("Still waiting for you."))
	r.a.SetRogue(true, 10)
	if err := r.a.RunTurn(context.Background(), "do it"); err != nil {
		t.Fatal(err)
	}
	if r.model.count() != 2 {
		t.Fatalf("expected exactly one nudge (2 requests), made %d", r.model.count())
	}
	if got := lastContent(r.model.request(1)); !strings.Contains(got, "no operator is watching") || !strings.Contains(got, "9 of 10 steps left") {
		t.Fatalf("second request should end with the nudge: %q", got)
	}
}

func TestRogueLastStepRunsNoTools(t *testing.T) {
	r := newRogueRig(t,
		call("write_file", map[string]any{"path": "a.txt", "content": "x\n"}),
		call("write_file", map[string]any{"path": "b.txt", "content": "x\n"}),
	)
	r.a.SetRogue(true, 2)
	if err := r.a.RunTurn(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if r.model.count() != 2 {
		t.Fatalf("a budget of 2 should allow 2 requests, made %d", r.model.count())
	}
	second := r.model.request(1)
	if !strings.Contains(lastContent(second), "this is your last step") {
		t.Fatalf("the last request should carry the final note: %q", lastContent(second))
	}
	prev := second.Messages[len(second.Messages)-2].Content
	if !strings.Contains(prev, "1 of 2 steps left; your next reply is the last step") {
		t.Fatalf("the tool result before the last step should warn: %q", prev)
	}
	if _, err := os.Stat(filepath.Join(r.root, "b.txt")); err == nil {
		t.Fatal("a tool call in the last step ran")
	}
	last := r.a.sess.Context[len(r.a.sess.Context)-1]
	if last.Role != "tool" || !strings.Contains(last.Content, "Not run") {
		t.Fatalf("the skipped call should get a result: %+v", last)
	}
}

func TestRogueTimeLimit(t *testing.T) {
	rt := &rogueTurn{budget: 100, deadline: time.Now().Add(-time.Second)}
	if !strings.Contains(rt.finalReason(1), "time limit") {
		t.Fatal("a passed deadline should make the step final")
	}
	if (&rogueTurn{budget: 100}).finalReason(5) != "" {
		t.Fatal("no deadline and budget left: not final")
	}
}

func TestRogueAlwaysBacksUpDeletes(t *testing.T) {
	r := newRogueRig(t,
		call("delete_file", map[string]any{"path": "f.txt"}),
		call("finish", map[string]any{"summary": "deleted"}),
	)
	os.WriteFile(filepath.Join(r.root, "f.txt"), []byte("keep a copy\n"), 0o644)
	r.a.SetRogue(true, 0)
	if r.a.Safe() {
		t.Fatal("test expects safe mode off")
	}
	if err := r.a.RunTurn(context.Background(), "delete f.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.root, "f.txt")); err == nil {
		t.Fatal("the delete did not run")
	}
	if len(r.a.listBackups()) != 1 {
		t.Fatal("rogue mode should back up before a delete even with safe mode off")
	}
}

func TestRogueOffMidTurnBringsBackApprovals(t *testing.T) {
	var r *rogueRig
	r = newRogueRig(t,
		func(ollama.ChatRequest) ollama.Message {
			r.a.SetRogue(false, 0) // the operator types /rogue off while the model works
			return call("write_file", map[string]any{"path": "a.txt", "content": "x\n"})(ollama.ChatRequest{})
		},
		say("ok"),
	)
	r.a.SetRogue(true, 0)
	if err := r.a.RunTurn(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if r.approvalCount() != 1 {
		t.Fatalf("after /rogue off the write should ask for approval, asked %d times", r.approvalCount())
	}
	if _, err := os.Stat(filepath.Join(r.root, "a.txt")); err == nil {
		t.Fatal("the denied write ran")
	}
	if hasTool(r.model.request(1), "finish") {
		t.Fatal("finish should not be offered after /rogue off")
	}
	if !strings.Contains(r.model.request(1).Messages[len(r.model.request(1).Messages)-1].Content, "Rogue mode is now OFF") {
		t.Fatal("the model should be told rogue mode is off at the next step")
	}
}

func TestRogueAndPlanExclusive(t *testing.T) {
	r := newRogueRig(t)
	r.a.SetPlan(true)
	if _, err := r.a.SetRogue(true, 0); err == nil {
		t.Fatal("rogue mode was allowed in plan mode")
	}
	r.a.SetPlan(false)
	if _, err := r.a.SetRogue(true, 0); err != nil {
		t.Fatal(err)
	}
	r.a.SetPlan(true)
	if r.a.Rogue() {
		t.Fatal("plan mode should switch rogue mode off")
	}
}

func TestRogueNotSavedWithSession(t *testing.T) {
	r := newRogueRig(t)
	r.a.SetRogue(true, 0)
	r.a.Save()
	loaded, err := r.a.Store.Load("rogue")
	if err != nil {
		t.Fatal(err)
	}
	if New(r.a.Deps, loaded, make(chan Event, 8)).Rogue() {
		t.Fatal("a resumed session must start with rogue mode off")
	}
}

func TestFinishOutsideRogue(t *testing.T) {
	r := newRogueRig(t, call("finish", map[string]any{"summary": "x"}), say("ok"))
	if err := r.a.RunTurn(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if hasTool(r.model.request(0), "finish") {
		t.Fatal("finish should not be offered outside rogue mode")
	}
	if got := lastContent(r.model.request(1)); !strings.Contains(got, `no tool named "finish"`) {
		t.Fatalf("calling finish outside rogue mode should be an error: %q", got)
	}
}

func TestRogueUsesItsOwnSystemPrompt(t *testing.T) {
	r := newRogueRig(t, call("finish", map[string]any{"summary": "done"}), say("normal again"))
	r.a.SetSystems("NORMAL PROMPT", "ROGUE PROMPT")
	r.a.SetRogue(true, 0)
	if err := r.a.RunTurn(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	r.a.SetRogue(false, 0)
	if err := r.a.RunTurn(context.Background(), "and now?"); err != nil {
		t.Fatal(err)
	}
	if got := r.model.request(0).Messages[0].Content; got != "ROGUE PROMPT" {
		t.Fatalf("rogue mode should send its own system prompt, sent %q", got)
	}
	if got := r.model.request(1).Messages[0].Content; got != "NORMAL PROMPT" {
		t.Fatalf("after /rogue off the normal prompt should be back, sent %q", got)
	}
}

func TestRogueTimeLimitCutsOffThinking(t *testing.T) {
	old := minute
	minute = 200 * time.Millisecond // rogue.max_minutes 1 = 200ms here
	defer func() { minute = old }()

	r := newRogueRig(t, nil, say("Summary: I was still thinking."))
	r.a.Cfg.Rogue.MaxMinutes = 1
	r.a.SetThink(true)
	r.a.SetRogue(true, 0)
	start := time.Now()
	if err := r.a.RunTurn(context.Background(), "think hard"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the time limit did not stop the reply in progress")
	}
	if r.model.count() != 2 {
		t.Fatalf("expected the cut-off request and the summary request, made %d", r.model.count())
	}
	final := r.model.request(1)
	if final.Think == nil || *final.Think {
		t.Fatal("the summary step should run with thinking off")
	}
	if !strings.Contains(lastContent(final), "last step") || !strings.Contains(lastContent(final), "time limit") {
		t.Fatalf("the summary request should carry the final note: %q", lastContent(final))
	}
	var interrupted bool
	for _, e := range r.a.sess.Context {
		if e.Kind == session.KindInterrupted && strings.Contains(e.Content, "time limit") {
			interrupted = true
		}
	}
	if !interrupted {
		t.Fatal("the cut-off reply should be marked as interrupted by the time limit")
	}
}
