package agent

// Rogue mode (/rogue on): every action runs without asking the operator,
// and each operator message is worked on until the model calls the finish
// tool, the step budget is used up or the time limit (rogue.max_minutes)
// passes. Every tool result carries the steps left. mv_file and
// delete_file always back up their target first, as in safe mode.
//
// Rogue mode is never saved with the session: a resumed session starts
// with it off. It cannot be combined with plan mode.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"clauzette/internal/ollama"
	"clauzette/internal/session"
)

const (
	finishTool   = "finish"
	rogueNoteTag = "rogue"
)

func finishSpec() ollama.ToolSpec {
	return ollama.ToolSpec{
		Type: "function",
		Function: ollama.FunctionSpec{
			Name: finishTool,
			Description: "Rogue mode only: call this when the task is done (or cannot be done) to end your work on it. " +
				"Give a summary for the operator: what you changed, how you checked it, assumptions you made and anything left open.",
			Parameters: map[string]any{
				"type":     "object",
				"required": []string{"summary"},
				"properties": map[string]any{
					"summary": map[string]any{"type": "string", "description": "Summary for the operator."},
				},
			},
		},
	}
}

// rogueNoteText marks in the conversation where the rules changed; the
// rules themselves are in the rogue-mode system prompt.
func rogueNoteText(on bool) string {
	if on {
		return "[Rogue mode is now ON: from here on nobody approves your actions or answers questions. Work on each task until it is done, then call finish with your summary.]"
	}
	return "[Rogue mode is now OFF. Actions need the operator's approval again, and the finish tool is no longer available.]"
}

// Rogue reports whether rogue mode is on.
func (a *Agent) Rogue() bool { return a.rogue.Load() }

// RogueBudget is the step budget per message in rogue mode.
func (a *Agent) RogueBudget() int {
	if n := int(a.rogueSteps.Load()); n > 0 {
		return n
	}
	if n := a.Cfg.Agent.MaxSteps; n > 0 {
		return n
	}
	return 100
}

// SetRogue switches rogue mode on (with a step budget per message; 0 means
// agent.max_steps) or off, and reports whether the state changed. The flag
// applies at once, so switching it off during a turn brings approvals back
// from the next action; the model is told at its next safe point.
func (a *Agent) SetRogue(on bool, steps int) (bool, error) {
	if on && a.plan.Load() {
		return false, errors.New("rogue mode cannot be used with plan mode; switch plan mode off first (!plan off)")
	}
	if steps < 0 {
		return false, errors.New("the step budget must be a positive number")
	}
	if on {
		a.rogueSteps.Store(int64(steps))
	}
	if on == a.rogue.Load() {
		return false, nil
	}
	a.rogue.Store(on)
	a.queueToggleNote(rogueNoteTag, rogueNoteText(on))
	a.Audit.Log(map[string]any{"session": a.sess.ID, "event": "rogue", "on": on, "steps": a.RogueBudget(), "max_minutes": a.Cfg.Rogue.MaxMinutes})
	return true, nil
}

// toolSpecs are the tool definitions for the next request: the registry's,
// plus finish in rogue mode.
func (a *Agent) toolSpecs() []ollama.ToolSpec {
	if a.rogue.Load() {
		return a.rogueSpecs
	}
	return a.specs
}

// fixed is the size of the system prompt and tool definitions now.
func (a *Agent) fixed() int {
	if a.rogue.Load() {
		return a.rogueFixed
	}
	return a.fixedChars
}

// rogueTimeLimit is the final reason when the time limit cuts a step short.
const rogueTimeLimit = "the time limit (rogue.max_minutes) is reached"

// rogueSummaryTimeout caps the final summary step, which runs after the
// budget or the time limit is used up.
const rogueSummaryTimeout = 5 * time.Minute

// minute is the unit of rogue.max_minutes; tests shorten it.
var minute = time.Minute

// rogueTurn tracks rogue mode within one turn.
type rogueTurn struct {
	budget   int
	deadline time.Time // zero: no time limit
	nudged   bool      // a reply without tool calls has been nudged once
	finished bool      // the model called finish
	summary  string
}

func (a *Agent) startRogueTurn() *rogueTurn {
	if !a.rogue.Load() {
		return nil
	}
	rt := &rogueTurn{budget: a.RogueBudget()}
	if m := a.Cfg.Rogue.MaxMinutes; m > 0 {
		rt.deadline = time.Now().Add(time.Duration(m) * minute)
	}
	a.rt = rt
	return rt
}

// finalReason says why this step must be the last one, or "".
func (rt *rogueTurn) finalReason(step int) string {
	switch {
	case step >= rt.budget:
		return fmt.Sprintf("the step budget (%d) is used up", rt.budget)
	case !rt.deadline.IsZero() && time.Now().After(rt.deadline):
		return rogueTimeLimit
	}
	return ""
}

// reminder is appended to each tool result in rogue mode.
func (rt *rogueTurn) reminder(step int) string {
	left := rt.budget - step
	s := fmt.Sprintf("\n\n[rogue mode: %d of %d steps left", left, rt.budget)
	if left == 1 {
		s += "; your next reply is the last step: it must be your final summary, and tool calls in it will not run"
	}
	return s + "]"
}

func (rt *rogueTurn) nudge(step int) string {
	return fmt.Sprintf("[rogue mode: no operator is watching or answering questions. If the task is not done, keep working with the tools and make reasonable assumptions instead of asking. If it is done, call the finish tool with your summary. %d of %d steps left.]",
		rt.budget-step, rt.budget)
}

func finalNote(reason string) string {
	return "[rogue mode: this is your last step, because " + reason + ". Reply with a summary for the operator: what is done, how you checked it, and what is left. Tool calls in this reply will not run.]"
}

// rogueFinal runs the last step of a rogue task: the model is told why and
// replies with a summary, with thinking off and capped at
// rogueSummaryTimeout, so it cannot run on. Tool calls in that reply do not
// run. ctx is the operator's, not the expired turn deadline.
func (a *Agent) rogueFinal(ctx context.Context, reason string) error {
	fctx, cancel := context.WithTimeout(ctx, rogueSummaryTimeout)
	defer cancel()
	_ = a.ensureRoom(fctx) // best effort: a summary over a full context is still worth trying
	a.appendHarness(finalNote(reason))
	res, err := a.callModel(fctx, false)
	if err != nil {
		if ctx.Err() != nil {
			a.markInterrupted(res, "")
			return ctx.Err()
		}
		if res != nil && (res.Message.Content != "" || res.Message.Thinking != "") {
			a.markInterrupted(res, fmt.Sprintf("[The summary failed: %v]", err))
		}
		a.warnf("rogue: stopped because %s, and the final summary failed: %v", reason, err)
		return nil
	}
	a.sess.Append(res.Message, "")
	a.updateUsage(res)
	a.Save()
	if len(res.Message.ToolCalls) > 0 {
		a.skipCalls(res.Message.ToolCalls, "Not run: rogue mode's last step allows no tool calls.")
	}
	a.warnf("rogue: stopped because %s; the model's summary is above", reason)
	return nil
}

// runFinish handles the finish tool: it ends the rogue task after the
// current tool calls.
func (a *Agent) runFinish(args map[string]any) (string, bool) {
	if a.rt == nil {
		return "Error: finish is only available in rogue mode.", true
	}
	summary, _ := args["summary"].(string)
	a.rt.finished, a.rt.summary = true, summary
	return "Task marked as finished; the operator will read your summary.", false
}

// appendHarness adds a harness message in the user role to the context.
func (a *Agent) appendHarness(text string) {
	a.sess.Append(ollama.Message{Role: "user", Content: text}, session.KindHarness)
}
