package agent

// Plan mode: while it is on, write, exec and destructive actions are
// prepared (validated, diffed, approved-free) and kept in a pending list
// instead of running. Read-only tools still run, so the model can keep
// exploring. The operator reviews the list with /plan and either runs it
// with /plan approve or discards it with /plan reject; both hand the
// outcome back to the model as the start of the next turn.
//
// The pending list is in memory per agent: /new or /load starts fresh.

import (
	"context"
	"fmt"
	"strings"

	"clauzette/internal/ollama"
	"clauzette/internal/textutil"
	"clauzette/internal/tools"
)

// userNote is a bracketed operator note in the user role, so the model
// distinguishes it from the operator's own messages.
func userNote(s string) ollama.Message {
	return ollama.Message{Role: "user", Content: "[" + s + "]"}
}

type PendingAction struct {
	Name   string
	Action *tools.Action
}

// SetPlan toggles plan mode and tells the model about it in the context.
// It reports whether the state changed.
func (a *Agent) SetPlan(v bool) bool {
	if v == a.plan.Load() {
		return false
	}
	a.plan.Store(v)
	a.sess.Plan = v
	if v {
		a.sess.Append(userNote("Plan mode is now ON. Your write, exec and destructive actions are captured in a pending list and NOT executed; the operator runs them with /plan approve. Read-only tools still work. Keep reading, planning and asking questions; when you have proposed the full set of changes, say what the operator should approve."), "")
	} else {
		a.sess.Append(userNote("Plan mode is now OFF. Actions run as usual, with approval. Pending actions from plan mode, if any, stay in the list until the operator approves or rejects them."), "")
	}
	return true
}

// PlanPending counts the prepared actions awaiting the operator.
func (a *Agent) PlanPending() int { return len(a.pending) }

// PlanReport describes plan mode and the pending list, with the same
// numbers /plan approve and /plan reject use.
func (a *Agent) PlanReport() string {
	var sb strings.Builder
	state := "off"
	if a.Plan() {
		state = "on"
	}
	fmt.Fprintf(&sb, "plan mode: %s  (toggle with !plan [on|off])\n", state)
	sb.WriteString("When on, write/exec/destructive actions are captured, not executed; read tools still run.\n")
	if len(a.pending) == 0 {
		sb.WriteString("No pending actions.")
		return sb.String()
	}
	for i, pa := range a.pending {
		fmt.Fprintf(&sb, "%2d  %-14s %s\n", i+1, pa.Name, textutil.OneLine(pa.Action.Summary, 100))
	}
	sb.WriteString("Run them: /plan approve [n ...] or /plan approve all\n")
	sb.WriteString("Discard:  /plan reject [n ...] or /plan reject all [reason]")
	return sb.String()
}

// PlanApprove runs the selected pending actions (their /plan numbers, or
// all when nums is empty) in the order they were planned. It stops at a
// denial or a cancellation. It returns the operator report and, when at
// least one action ran, a message to start the next model turn; the list
// of actions that stayed pending is left untouched.
func (a *Agent) PlanApprove(ctx context.Context, nums []int) (string, string, error) {
	sel, err := a.selectPending(nums)
	if err != nil {
		return "", "", err
	}
	var rep strings.Builder
	fmt.Fprintf(&rep, "running %d planned action%s:\n", len(sel), pluralN(len(sel)))
	results := make([]string, 0, len(sel))
	stopped := ""
	for i, pa := range sel {
		fmt.Fprintf(&rep, "%d. %s: %s\n", i+1, pa.Name, textutil.OneLine(pa.Action.Summary, 120))
		t, ok := a.Registry.Get(pa.Name)
		if !ok {
			rep.WriteString("   skipped: the tool is no longer registered\n")
			a.dropPending(pa)
			continue
		}
		rec := map[string]any{"session": a.sess.ID, "tool": pa.Name, "plan": true, "summary": pa.Action.Summary}
		out, isErr := a.executeAction(ctx, pa.Name, t, pa.Action, rec)
		rec["error"] = isErr
		rec["result"] = textutil.OneLine(out, 300)
		a.Audit.Log(rec)
		a.emit(Event{Kind: EvToolResult, Tool: pa.Name, Text: out, IsError: isErr})
		a.dropPending(pa)
		if rec["decision"] == "denied" {
			stopped = " the operator denied one; the rest stayed pending"
			break
		}
		if ctx.Err() != nil {
			stopped = " cancelled; the rest stayed pending"
			break
		}
		results = append(results, fmt.Sprintf("%s: %s", pa.Name, textutil.OneLine(out, 200)))
		if isErr {
			rep.WriteString("   (reported to the model)\n")
		}
	}
	a.Save()
	if len(results) == 0 {
		return rep.String() + stopped, "", nil
	}
	var m strings.Builder
	m.WriteString("You are in plan mode. The operator approved the pending actions and they have now run. Results:")
	for _, r := range results {
		m.WriteString("\n- " + r)
	}
	m.WriteString(stopped)
	m.WriteString("\nTake the results into account and continue; if the goal is reached, give your final answer.")
	return rep.String() + stopped, m.String(), nil
}

// PlanReject discards the selected pending actions and returns the operator
// report plus a message that hands the rejection (and any reason) to the
// model as the start of the next turn.
func (a *Agent) PlanReject(nums []int, reason string) (string, string, error) {
	sel, err := a.selectPending(nums)
	if err != nil {
		return "", "", err
	}
	var names []string
	for _, pa := range sel {
		names = append(names, pa.Name+" ("+textutil.OneLine(pa.Action.Summary, 60)+")")
		a.dropPending(pa)
	}
	a.Audit.Log(map[string]any{
		"session": a.sess.ID,
		"event":   "plan_reject",
		"actions": names,
		"reason":  reason,
	})
	a.Save()
	rep := fmt.Sprintf("rejected %d planned action%s: %s", len(sel), pluralN(len(sel)), strings.Join(names, "; "))
	m := "You are in plan mode. The operator rejected the following planned actions: " + strings.Join(names, "; ") + "."
	if reason != "" {
		m += " Their reason: " + reason
	} else {
		m += " No reason was given; if you are unsure what to do instead, ask the operator."
	}
	m += " Adjust the plan and continue."
	return rep, m, nil
}

// selectPending resolves /plan numbers (1 = first planned) to actions,
// returning them in planning order.
func (a *Agent) selectPending(nums []int) ([]*PendingAction, error) {
	if len(nums) == 0 {
		if len(a.pending) == 0 {
			return nil, fmt.Errorf("no pending actions; /plan shows the state")
		}
		return append([]*PendingAction(nil), a.pending...), nil
	}
	want := make(map[int]bool, len(nums))
	for _, n := range nums {
		if n < 1 || n > len(a.pending) {
			return nil, fmt.Errorf("no pending action %d; this session has %d (see /plan)", n, len(a.pending))
		}
		want[n] = true
	}
	var out []*PendingAction
	for i, pa := range a.pending {
		if want[i+1] {
			out = append(out, pa)
		}
	}
	return out, nil
}

func (a *Agent) dropPending(pa *PendingAction) {
	for i, p := range a.pending {
		if p == pa {
			a.pending = append(a.pending[:i], a.pending[i+1:]...)
			return
		}
	}
}

func pluralN(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
