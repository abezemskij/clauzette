package agent

import (
	"context"
	"strings"
	"testing"

	"clauzette/internal/ollama"
	"clauzette/internal/session"
)

// fillConversation adds n alternating operator/agent messages of equal size.
func fillConversation(a *Agent, n int) {
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		a.sess.Append(ollama.Message{Role: role, Content: strings.Repeat("x", 3500)}, "")
	}
}

func verbatimKept(a *Agent) int {
	n := 0
	for _, e := range a.sess.Context {
		if e.Kind == "" && len(e.Content) == 3500 {
			n++
		}
	}
	return n
}

func TestCompactRatioKeepsNewestHalf(t *testing.T) {
	r := newRogueRig(t, say("SUMMARY OF THE OLDER HALF"))
	r.a.Cfg.Context.KeepRecentRatio = 0.5
	fillConversation(r.a, 20)

	if err := r.a.Compact(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if r.model.count() != 1 {
		t.Fatalf("plain messages cannot be shrunk, so the older half should be summarised (requests: %d)", r.model.count())
	}
	if kept := verbatimKept(r.a); kept < 9 || kept > 11 {
		t.Fatalf("about half of the 20 messages should stay verbatim, kept %d", kept)
	}
	if first := r.a.sess.Context[0]; first.Kind != session.KindSummary || !strings.Contains(first.Content, "SUMMARY OF THE OLDER HALF") {
		t.Fatalf("the context should start with the summary: %+v", first.Kind)
	}
}

func TestCompactPercentOverridesOnce(t *testing.T) {
	r := newRogueRig(t, say("SUMMARY"))
	fillConversation(r.a, 20)
	if err := r.a.Compact(context.Background(), 0.25); err != nil { // /compact 75
		t.Fatal(err)
	}
	if kept := verbatimKept(r.a); kept < 4 || kept > 6 {
		t.Fatalf("/compact 75 should keep about a quarter (5) verbatim, kept %d", kept)
	}
	if r.a.Cfg.Context.KeepRecentRatio != 0 {
		t.Fatal("a one-off percentage must not change the config")
	}
}

func TestCompactRatioStageOneEnoughWhenToolOutputHalves(t *testing.T) {
	r := newRogueRig(t) // no scripted replies: a summary request would fail the test
	r.a.Cfg.Context.KeepRecentRatio = 0.5
	// Older half: one call with a large result per exchange; newer half: plain messages.
	for i := 0; i < 5; i++ {
		r.a.sess.Append(ollama.Message{Role: "assistant", ToolCalls: []ollama.ToolCall{{Function: ollama.ToolCallFunction{Name: "read_file", Arguments: map[string]any{"path": "f"}}}}}, "")
		r.a.sess.Append(ollama.Message{Role: "tool", ToolName: "read_file", Content: strings.Repeat("y", 7000)}, "")
	}
	fillConversation(r.a, 10)

	if err := r.a.Compact(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if r.model.count() != 0 {
		t.Fatal("removing the old tool output halves the older part, so no summary should be requested")
	}
	for _, e := range r.a.sess.Context {
		if e.Role == "tool" && len(e.Content) == 7000 {
			t.Fatal("old tool output should be replaced by a placeholder")
		}
	}
	if verbatimKept(r.a) != 10 {
		t.Fatal("the newer half should stay verbatim")
	}
}

func TestContextReportShowsRatioAndPreview(t *testing.T) {
	r := newRogueRig(t)
	r.a.Cfg.Context.KeepRecentRatio = 0.5
	fillConversation(r.a, 20)
	rep := r.a.ContextReport()
	for _, want := range []string{"keeps the newest 50%", "Compacting now would keep the newest", "of 20 messages"} {
		if !strings.Contains(rep, want) {
			t.Fatalf("/context is missing %q:\n%s", want, rep)
		}
	}
}

// The prompt count of the last request describes the context before it was
// rewritten: compaction and undo must drop it so the meter shows the new
// size, and compaction says so with a usage event straight away.
func TestCompactAndUndoRefreshUsedTokens(t *testing.T) {
	r := newRogueRig(t, say("SUMMARY"))
	events := make(chan Event, 64)
	r.a.events = events
	fillConversation(r.a, 20)
	r.a.sess.LastPromptTokens = 999_999 // what the model reported before compaction

	if err := r.a.Compact(context.Background(), 0.25); err != nil {
		t.Fatal(err)
	}
	est := r.a.ContextTokens()
	if got := r.a.UsedTokens(); got != est {
		t.Fatalf("after compaction the meter should show the estimate ~%d, got %d", est, got)
	}
	var usage *Usage
	for len(events) > 0 {
		if ev := <-events; ev.Kind == EvUsage {
			u := ev.Usage
			usage = &u
		}
	}
	if usage == nil || usage.Used != est || usage.NumCtx != r.a.Window() || usage.CompactAt != r.a.CompactAt() || usage.TokPerSec != 0 {
		t.Fatalf("compaction should end with a usage event for the new size, got %+v", usage)
	}

	r.a.sess.Append(ollama.Message{Role: "user", Content: "again"}, "")
	r.a.sess.Append(ollama.Message{Role: "assistant", Content: strings.Repeat("z", 9000)}, "")
	r.a.sess.LastPromptTokens = 888_888
	if _, err := r.a.Undo(); err != nil {
		t.Fatal(err)
	}
	if got := r.a.UsedTokens(); got != r.a.ContextTokens() {
		t.Fatalf("after undo the meter should show the estimate, got %d", got)
	}
}
