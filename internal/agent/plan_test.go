package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clauzette/internal/config"
	"clauzette/internal/ollama"
	"clauzette/internal/session"
	"clauzette/internal/tools"
)

func newPlanAgent(t *testing.T, ws *tools.Workspace, ts ...tools.Tool) *Agent {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	return New(Deps{
		Cfg:      config.Defaults(),
		Registry: tools.NewRegistry(ts...),
		Store:    store,
		WS:       ws,
		Audit:    nil,
	}, &session.Session{ID: "plansession"}, make(chan Event, 64))
}

func writeCall(name string, args map[string]any) ollama.ToolCall {
	return ollama.ToolCall{Function: ollama.ToolCallFunction{Name: name, Arguments: args}}
}

// wsFor builds a workspace bound to an existing root, for tools passed in.
func wsFor(t *testing.T, root string) *tools.Workspace {
	t.Helper()
	ws, err := tools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestPlanCapturesThenApproves(t *testing.T) {
	root := t.TempDir()
	ws := wsFor(t, root)
	a := newPlanAgent(t, ws, &tools.WriteFile{WS: ws})
	ctx := context.Background()

	if !a.SetPlan(true) {
		t.Fatal("SetPlan(true) reported no change")
	}
	if a.SetPlan(true) {
		t.Fatal("second SetPlan(true) reported a change")
	}

	out := a.runTool(ctx, writeCall("write_file", map[string]any{"path": "x.txt", "content": "hi\n"}))
	if !strings.Contains(out, "Planned, not executed") {
		t.Fatalf("expected a planned result, got %q", out)
	}
	if a.PlanPending() != 1 {
		t.Fatalf("expected 1 pending action, got %d", a.PlanPending())
	}
	if _, err := os.Stat(filepath.Join(root, "x.txt")); !os.IsNotExist(err) {
		t.Fatal("the file was created although it should only be planned")
	}

	// auto-approve so /plan approve does not wait for the operator
	a.always["write_file"] = true
	report, cont, err := a.PlanApprove(ctx, nil, PlanApproveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.PlanPending() != 0 || strings.Count(report, "running 1 planned action") == 0 {
		t.Fatalf("approve did not report correctly: %q", report)
	}
	if !strings.Contains(cont, "The operator approved") {
		t.Fatalf("missing the model continuation: %q", cont)
	}
	b, err := os.ReadFile(filepath.Join(root, "x.txt"))
	if err != nil || string(b) != "hi\n" {
		t.Fatalf("approved action did not create the file: %q %v", b, err)
	}
	if _, _, err := a.PlanApprove(ctx, nil, PlanApproveOptions{}); err == nil {
		t.Fatal("approving with an empty list was accepted")
	}
}

func TestPlanReject(t *testing.T) {
	root := t.TempDir()
	ws := wsFor(t, root)
	a := newPlanAgent(t, ws, &tools.WriteFile{WS: ws})
	a.SetPlan(true)
	a.runTool(context.Background(), writeCall("write_file", map[string]any{"path": "x.txt", "content": "hi\n"}))

	report, cont, err := a.PlanReject(nil, "use the other directory")
	if err != nil {
		t.Fatal(err)
	}
	if a.PlanPending() != 0 || strings.Count(report, "rejected 1 planned action") == 0 {
		t.Fatalf("reject did not report correctly: %q", report)
	}
	for _, sub := range []string{"The operator rejected", "use the other directory"} {
		if !strings.Contains(cont, sub) {
			t.Fatalf("continuation missing %q: %q", sub, cont)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "x.txt")); !os.IsNotExist(err) {
		t.Fatal("the file was created although its action was rejected")
	}
}

func TestPlanReadOnlyStillRuns(t *testing.T) {
	root := t.TempDir()
	ws, _ := tools.NewWorkspace(root)
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newPlanAgent(t, ws, &tools.ReadFile{WS: ws})
	a.SetPlan(true)
	out := a.runTool(context.Background(), writeCall("read_file", map[string]any{"path": "f.txt", "limit": float64(50)}))
	if strings.Contains(out, "Planned") || a.PlanPending() != 0 {
		t.Fatalf("read_file should run in plan mode, got %q", out)
	}
	if !strings.Contains(out, "data") {
		t.Fatalf("expected the file content, got %q", out)
	}
}

func TestPlanSelectByNumber(t *testing.T) {
	root := t.TempDir()
	ws := wsFor(t, root)
	a := newPlanAgent(t, ws, &tools.WriteFile{WS: ws})
	a.SetPlan(true)
	a.always["write_file"] = true
	ctx := context.Background()
	a.runTool(ctx, writeCall("write_file", map[string]any{"path": "one.txt", "content": "1\n"}))
	a.runTool(ctx, writeCall("write_file", map[string]any{"path": "two.txt", "content": "2\n"}))

	if _, _, err := a.PlanApprove(ctx, []int{2}, PlanApproveOptions{}); err != nil {
		t.Fatal(err)
	}
	if a.PlanPending() != 1 {
		t.Fatalf("expected 1 pending action to remain, got %d", a.PlanPending())
	}
	if _, err := os.Stat(filepath.Join(root, "one.txt")); !os.IsNotExist(err) {
		t.Fatal("action 1 should not have run")
	}
	if _, err := os.Stat(filepath.Join(root, "two.txt")); err != nil {
		t.Fatal("action 2 should have run")
	}
	if _, _, err := a.PlanApprove(ctx, []int{5}, PlanApproveOptions{}); err == nil {
		t.Fatal("an out-of-range number was accepted")
	}
}

// newApprovingAgent is newPlanAgent with an operator: every approval
// request is answered by decide, which may also cancel the turn instead.
func newApprovingAgent(t *testing.T, ws *tools.Workspace, decide func(*ApprovalRequest) (Decision, bool), ts ...tools.Tool) *Agent {
	t.Helper()
	events := make(chan Event)
	a := newPlanAgent(t, ws, ts...)
	a.events = events
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case ev := <-events:
				if ev.Kind == EvApproval {
					if d, ok := decide(ev.Approval); ok {
						ev.Approval.Reply <- d
					}
				}
			case <-done:
				return
			}
		}
	}()
	return a
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPlanTwoEditsToOneFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "f.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := wsFor(t, root)
	a := newPlanAgent(t, ws, &tools.EditFile{WS: ws})
	a.SetPlan(true)
	ctx := context.Background()
	a.runTool(ctx, writeCall("edit_file", map[string]any{"path": "f.txt", "old_text": "alpha", "new_text": "ALPHA"}))
	a.runTool(ctx, writeCall("edit_file", map[string]any{"path": "f.txt", "old_text": "beta", "new_text": "BETA"}))
	a.always["edit_file"] = true

	_, cont, err := a.PlanApprove(ctx, nil, PlanApproveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "ALPHA\nBETA\n" {
		t.Fatalf("both edits should apply, file is %q", got)
	}
	if strings.Count(cont, "(ok)") != 2 || strings.Contains(cont, "(error)") {
		t.Fatalf("expected two ok results: %q", cont)
	}
}

func TestPlanCreateThenOverwrite(t *testing.T) {
	root := t.TempDir()
	ws := wsFor(t, root)
	a := newPlanAgent(t, ws, &tools.WriteFile{WS: ws})
	a.SetPlan(true)
	ctx := context.Background()
	a.runTool(ctx, writeCall("write_file", map[string]any{"path": "n.txt", "content": "one\n"}))
	a.runTool(ctx, writeCall("write_file", map[string]any{"path": "n.txt", "content": "two\n"}))
	a.always["write_file"] = true

	if _, _, err := a.PlanApprove(ctx, nil, PlanApproveOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(root, "n.txt")); got != "two\n" {
		t.Fatalf("the second write should win, file is %q", got)
	}
}

func TestPlanPrepareFailureIsReported(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "f.txt")
	os.WriteFile(path, []byte("alpha\n"), 0o644)
	ws := wsFor(t, root)
	a := newPlanAgent(t, ws, &tools.EditFile{WS: ws})
	a.SetPlan(true)
	ctx := context.Background()
	a.runTool(ctx, writeCall("edit_file", map[string]any{"path": "f.txt", "old_text": "alpha", "new_text": "ALPHA"}))
	os.WriteFile(path, []byte("changed by someone else\n"), 0o644)
	a.always["edit_file"] = true

	_, cont, err := a.PlanApprove(ctx, nil, PlanApproveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cont, "(error)") || !strings.Contains(cont, "could not be prepared again") {
		t.Fatalf("the failure should reach the model: %q", cont)
	}
	if a.PlanPending() != 0 {
		t.Fatal("a call that cannot be prepared should leave the list")
	}
}

func TestPlanDenialStopsAndIsReported(t *testing.T) {
	root := t.TempDir()
	ws := wsFor(t, root)
	n := 0
	a := newApprovingAgent(t, ws, func(r *ApprovalRequest) (Decision, bool) {
		n++
		if n == 2 {
			return Decision{Approve: false, Reason: "wrong directory"}, true
		}
		return Decision{Approve: true}, true
	}, &tools.WriteFile{WS: ws})
	a.SetPlan(true)
	ctx := context.Background()
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		a.runTool(ctx, writeCall("write_file", map[string]any{"path": f, "content": "x\n"}))
	}

	report, cont, err := a.PlanApprove(ctx, nil, PlanApproveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal("the first action should have run")
	}
	for _, f := range []string{"b.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(root, f)); !os.IsNotExist(err) {
			t.Fatalf("%s should not exist", f)
		}
	}
	if a.PlanPending() != 1 {
		t.Fatalf("only the action after the denial should stay pending, have %d", a.PlanPending())
	}
	for _, sub := range []string{"(ok)", "(denied)", "wrong directory", "Stopped early", "1 planned action is still pending"} {
		if !strings.Contains(cont, sub) {
			t.Fatalf("model message is missing %q: %q", sub, cont)
		}
	}
	if !strings.Contains(report, "stopped:") {
		t.Fatalf("the operator report should say it stopped: %q", report)
	}
}

func TestPlanFirstActionDeniedStillTellsTheModel(t *testing.T) {
	root := t.TempDir()
	ws := wsFor(t, root)
	a := newApprovingAgent(t, ws, func(*ApprovalRequest) (Decision, bool) {
		return Decision{Approve: false}, true
	}, &tools.WriteFile{WS: ws})
	a.SetPlan(true)
	a.runTool(context.Background(), writeCall("write_file", map[string]any{"path": "a.txt", "content": "x\n"}))

	_, cont, err := a.PlanApprove(context.Background(), nil, PlanApproveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cont, "(denied)") {
		t.Fatalf("the denial should reach the model: %q", cont)
	}
}

func TestPlanCancelKeepsActionPending(t *testing.T) {
	root := t.TempDir()
	ws := wsFor(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := newApprovingAgent(t, ws, func(*ApprovalRequest) (Decision, bool) {
		cancel() // the operator types !c instead of answering
		return Decision{}, false
	}, &tools.WriteFile{WS: ws})
	a.SetPlan(true)
	for _, f := range []string{"a.txt", "b.txt"} {
		a.runTool(context.Background(), writeCall("write_file", map[string]any{"path": f, "content": "x\n"}))
	}

	report, cont, err := a.PlanApprove(ctx, nil, PlanApproveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.PlanPending() != 2 {
		t.Fatalf("both actions should stay pending, have %d", a.PlanPending())
	}
	if cont != "" {
		t.Fatalf("nothing ran, so there should be no model message: %q", cont)
	}
	if !strings.Contains(report, "stays pending") {
		t.Fatalf("report should say the action stays pending: %q", report)
	}
}

func TestPlanApproveComment(t *testing.T) {
	root := t.TempDir()
	ws := wsFor(t, root)
	a := newPlanAgent(t, ws, &tools.WriteFile{WS: ws})
	a.SetPlan(true)
	a.runTool(context.Background(), writeCall("write_file", map[string]any{"path": "a.txt", "content": "x\n"}))
	a.always["write_file"] = true

	comment := "but then stop and let's review the changes"
	_, cont, err := a.PlanApprove(context.Background(), nil, PlanApproveOptions{Comment: comment})
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"Operator's comment:\n" + comment, "takes priority over continuing"} {
		if !strings.Contains(cont, sub) {
			t.Fatalf("model message is missing %q: %q", sub, cont)
		}
	}
	if strings.Contains(cont, "give your final answer") {
		t.Fatalf("with a comment the default instruction should not appear: %q", cont)
	}
}

func TestPlanNoContinueQueuesResults(t *testing.T) {
	root := t.TempDir()
	ws := wsFor(t, root)
	a := newPlanAgent(t, ws, &tools.WriteFile{WS: ws})
	a.SetPlan(true)
	a.flushNotes() // deliver the plan-mode note first
	a.runTool(context.Background(), writeCall("write_file", map[string]any{"path": "a.txt", "content": "x\n"}))
	a.always["write_file"] = true

	report, cont, err := a.PlanApprove(context.Background(), nil, PlanApproveOptions{NoContinue: true, Comment: "look first"})
	if err != nil {
		t.Fatal(err)
	}
	if cont != "" {
		t.Fatalf("no model turn should be started: %q", cont)
	}
	if !strings.Contains(report, "next message") {
		t.Fatalf("report should say the results are held back: %q", report)
	}
	if n := len(a.sess.QueuedNotes); n != 1 {
		t.Fatalf("expected 1 queued note, have %d", n)
	}
	before := len(a.sess.Context)
	a.flushNotes()
	last := a.sess.Context[len(a.sess.Context)-1]
	if len(a.sess.Context) != before+1 || last.Kind != session.KindHarness || !strings.Contains(last.Content, "look first") {
		t.Fatalf("the held-back results should enter the context as a harness message: %+v", last)
	}
}

func TestPlanPendingSurvivesReload(t *testing.T) {
	root := t.TempDir()
	ws := wsFor(t, root)
	a := newPlanAgent(t, ws, &tools.WriteFile{WS: ws})
	a.SetPlan(true)
	a.runTool(context.Background(), writeCall("write_file", map[string]any{"path": "a.txt", "content": "x\n"}))
	a.Save()

	loaded, err := a.Store.Load(a.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	b := New(a.Deps, loaded, make(chan Event, 64))
	if b.PlanPending() != 1 || !b.Plan() {
		t.Fatalf("pending list or plan mode lost on reload: %d pending, plan %v", b.PlanPending(), b.Plan())
	}
	b.always["write_file"] = true
	if _, _, err := b.PlanApprove(context.Background(), nil, PlanApproveOptions{}); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(root, "a.txt")) != "x\n" {
		t.Fatal("the reloaded action did not run")
	}
}

func TestSetPlanQueuesNote(t *testing.T) {
	ws := wsFor(t, t.TempDir())
	a := newPlanAgent(t, ws)
	a.SetPlan(true)
	if len(a.sess.Context) != 0 || len(a.sess.QueuedNotes) != 1 {
		t.Fatalf("the note should be queued, not added: %d in context, %d queued", len(a.sess.Context), len(a.sess.QueuedNotes))
	}
	a.SetPlan(false)
	if len(a.sess.QueuedNotes) != 0 {
		t.Fatal("toggling back before the model was told should cancel the note")
	}
	if a.Plan() {
		t.Fatal("plan mode should be off")
	}
}

func TestUndoSkipsHarnessMessages(t *testing.T) {
	ws := wsFor(t, t.TempDir())
	a := newPlanAgent(t, ws)
	a.sess.Append(ollama.Message{Role: "user", Content: "do the thing"}, "")
	a.sess.Append(ollama.Message{Role: "assistant", Content: "planned"}, "")
	a.SetPlan(true)
	a.flushNotes()

	text, err := a.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if text != "do the thing" {
		t.Fatalf("undo should return the operator's message, got %q", text)
	}
}
