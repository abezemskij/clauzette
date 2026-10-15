package agent

import (
	"context"
	"path/filepath"
	"testing"

	"clauzette/internal/config"
	"clauzette/internal/session"
	"clauzette/internal/tools"
)

// fakeFetchTool stands in for fetch_url: an Exec tool whose action carries a
// Scope (the target host) and counts its runs.
type fakeFetchTool struct {
	scope string
	ran   *int
}

func (fakeFetchTool) Name() string               { return "fetch_url" }
func (fakeFetchTool) Risk() tools.Risk           { return tools.Exec }
func (fakeFetchTool) Description() string        { return "fake fetch" }
func (fakeFetchTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (t fakeFetchTool) Prepare(_ context.Context, _ tools.Args) (*tools.Action, error) {
	return &tools.Action{Summary: "fetch " + t.scope, Scope: t.scope, Run: func(context.Context) (string, error) {
		*t.ran++
		return "fetched", nil
	}}, nil
}

func newFetchAgent(t *testing.T, policy *tools.FetchPolicy, tool tools.Tool, events chan Event) *Agent {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	return New(Deps{
		Cfg:      config.Defaults(),
		Registry: tools.NewRegistry(tool),
		Store:    store,
		Fetch:    policy,
	}, &session.Session{ID: "fetchsess"}, events)
}

func TestFetchGateListedRunsWithoutApproval(t *testing.T) {
	ran := 0
	policy := tools.NewFetchPolicy(true, []string{"example.com"})
	events := make(chan Event, 32)
	a := newFetchAgent(t, policy, fakeFetchTool{scope: "example.com", ran: &ran}, events)

	out := a.runTool(context.Background(), writeCall("fetch_url", nil))
	if ran != 1 || out != "fetched" {
		t.Fatalf("listed host should run without approval: ran=%d out=%q", ran, out)
	}
	for len(events) > 0 {
		if ev := <-events; ev.Kind == EvApproval {
			t.Fatal("a listed host must not prompt for approval")
		}
	}
}

func TestFetchGateOffRogueRuns(t *testing.T) {
	ran := 0
	policy := tools.NewFetchPolicy(false, nil) // gate off
	events := make(chan Event, 32)
	a := newFetchAgent(t, policy, fakeFetchTool{scope: "example.com", ran: &ran}, events)
	a.SetRogue(true, 0)

	out := a.runTool(context.Background(), writeCall("fetch_url", nil))
	if ran != 1 || out != "fetched" {
		t.Fatalf("gate off + rogue should run: ran=%d out=%q", ran, out)
	}
}

func TestFetchGateUnlistedAsksEvenInRogue(t *testing.T) {
	ran := 0
	policy := tools.NewFetchPolicy(true, nil) // gate on, nothing listed
	events := make(chan Event, 32)
	a := newFetchAgent(t, policy, fakeFetchTool{scope: "docs.example.com", ran: &ran}, events)
	a.SetRogue(true, 0)

	// Drive the approval: approve with Always, which should add the host.
	go func() {
		for ev := range events {
			if ev.Kind == EvApproval {
				if ev.Approval.AllowScope != "docs.example.com" {
					t.Errorf("approval should carry the host scope, got %q", ev.Approval.AllowScope)
				}
				ev.Approval.Reply <- Decision{Approve: true, Always: true}
			}
		}
	}()

	out := a.runTool(context.Background(), writeCall("fetch_url", nil))
	if ran != 1 || out != "fetched" {
		t.Fatalf("approved fetch should run: ran=%d out=%q", ran, out)
	}
	if !policy.Allowed("docs.example.com") {
		t.Fatal("'always' on a gated fetch should add the host to the allowlist")
	}
}

// TestFetchGateAutoApproveWins: an auto_approved fetch_url runs without a prompt
// even when the gate is on and the host is not listed.
func TestFetchGateAutoApproveWins(t *testing.T) {
	ran := 0
	policy := tools.NewFetchPolicy(true, nil) // gate on, nothing listed
	events := make(chan Event, 32)
	a := newFetchAgent(t, policy, fakeFetchTool{scope: "unlisted.example", ran: &ran}, events)
	a.always["fetch_url"] = true // operator put it in auto_approve

	out := a.runTool(context.Background(), writeCall("fetch_url", nil))
	if ran != 1 || out != "fetched" {
		t.Fatalf("auto_approve should run without a prompt: ran=%d out=%q", ran, out)
	}
	for len(events) > 0 {
		if ev := <-events; ev.Kind == EvApproval {
			t.Fatal("auto_approve must not prompt even with the gate on")
		}
	}
}
