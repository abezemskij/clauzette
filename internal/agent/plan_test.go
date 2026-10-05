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
	report, cont, err := a.PlanApprove(ctx, nil)
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
	if _, _, err := a.PlanApprove(ctx, nil); err == nil {
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

	if _, _, err := a.PlanApprove(ctx, []int{2}); err != nil {
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
	if _, _, err := a.PlanApprove(ctx, []int{5}); err == nil {
		t.Fatal("an out-of-range number was accepted")
	}
}
