package agent

// Plan mode: while it is on, write, exec and destructive calls are
// validated and kept in a pending list instead of running. Read-only tools
// still run, so the model can keep exploring. The operator reviews the list
// with /plan and either runs it with /plan approve or discards it with
// /plan reject; both hand the outcome back to the model.
//
// The pending list holds the calls (tool name and arguments), not prepared
// actions: /plan approve prepares each call again right before it runs, so
// a later edit of a file sees the earlier ones, and the list is saved with
// the session.

import (
	"context"
	"fmt"
	"strings"

	"clauzette/internal/ollama"
	"clauzette/internal/session"
	"clauzette/internal/textutil"
	"clauzette/internal/tools"
)

const planNoteTag = "plan"

func planNoteText(on bool) string {
	if on {
		return "[Plan mode is now ON. Your write, exec and destructive actions are captured in a pending list and NOT executed; the operator runs them with /plan approve. Read-only tools still work. Keep reading, planning and asking questions; when you have proposed the full set of changes, say what the operator should approve.]"
	}
	return "[Plan mode is now OFF. Actions run as usual, with approval. Pending actions from plan mode, if any, stay in the list until the operator approves or rejects them.]"
}

// SetPlan toggles plan mode and reports whether the state changed. The flag
// applies at once; the note that tells the model is queued and added to the
// context at the agent's next safe point, so this is safe to call while a
// turn runs. Switching plan mode on switches rogue mode off.
func (a *Agent) SetPlan(v bool) bool {
	if v == a.plan.Load() {
		return false
	}
	if v {
		a.SetRogue(false, 0)
	}
	a.plan.Store(v)
	a.queueToggleNote(planNoteTag, planNoteText(v))
	return true
}

// queueToggleNote queues the note for a mode that was just toggled. If a
// note for the same mode is still queued, the model was never told about
// that earlier toggle and still believes the state this one restores, so
// both are dropped instead.
func (a *Agent) queueToggleNote(tag, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, n := range a.sess.QueuedNotes {
		if n.Tag == tag {
			a.sess.QueuedNotes = append(a.sess.QueuedNotes[:i], a.sess.QueuedNotes[i+1:]...)
			return
		}
	}
	a.sess.QueuedNotes = append(a.sess.QueuedNotes, session.QueuedNote{Tag: tag, Text: text})
}

// queueNote adds a harness message for the model at the next safe point.
func (a *Agent) queueNote(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sess.QueuedNotes = append(a.sess.QueuedNotes, session.QueuedNote{Text: text})
}

// flushNotes moves queued harness messages into the context. It runs on the
// agent's goroutine only, where a user-role message is valid: before the
// operator's message, or after all tool results of a step.
func (a *Agent) flushNotes() {
	a.mu.Lock()
	notes := a.sess.QueuedNotes
	a.sess.QueuedNotes = nil
	a.mu.Unlock()
	for _, n := range notes {
		a.sess.Append(ollama.Message{Role: "user", Content: n.Text}, session.KindHarness)
	}
}

// PlanPending counts the planned calls awaiting the operator.
func (a *Agent) PlanPending() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sess.Pending)
}

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
	a.mu.Lock()
	pending := append([]*session.PendingCall(nil), a.sess.Pending...)
	a.mu.Unlock()
	if len(pending) == 0 {
		sb.WriteString("No pending actions.")
		return sb.String()
	}
	for i, pc := range pending {
		fmt.Fprintf(&sb, "%2d  %-14s %s\n", i+1, pc.Name, textutil.OneLine(pc.Summary, 100))
	}
	sb.WriteString("Run them: /plan approve <n ...|all> [comment for the model]  (--no-continue: don't start a model turn)\n")
	sb.WriteString("Discard:  /plan reject <n ...|all> [reason]")
	return sb.String()
}

// PlanApproveOptions are the extras of /plan approve.
type PlanApproveOptions struct {
	// Comment is the operator's note to the model, delivered with the
	// results; it takes priority over continuing the plan.
	Comment string
	// NoContinue queues the results for the operator's next message
	// instead of returning them to start a model turn now.
	NoContinue bool
}

// PlanApprove runs the selected pending calls (their /plan numbers, or all
// when nums is empty) in the order they were planned. Each call is prepared
// again and goes through the normal approval path. A denial stops the run
// (the denied call is dropped and reported); a cancellation stops it too,
// and the call that did not run stays pending with the rest.
//
// It returns the operator report and, unless NoContinue is set, the message
// that starts the next model turn ("" when nothing ran or was denied).
func (a *Agent) PlanApprove(ctx context.Context, nums []int, opt PlanApproveOptions) (string, string, error) {
	sel, err := a.selectPending(nums)
	if err != nil {
		return "", "", err
	}
	comment := strings.TrimSpace(opt.Comment)
	a.Audit.Log(map[string]any{
		"session":     a.sess.ID,
		"event":       "plan_approve",
		"selected":    len(sel),
		"comment":     comment,
		"no_continue": opt.NoContinue,
	})

	var rep strings.Builder
	fmt.Fprintf(&rep, "running %d planned action%s:\n", len(sel), pluralN(len(sel)))
	var results []string
	stopped := ""
	for i, pc := range sel {
		fmt.Fprintf(&rep, "%d. %s: %s\n", i+1, pc.Name, textutil.OneLine(pc.Summary, 120))
		rec := map[string]any{"session": a.sess.ID, "tool": pc.Name, "plan": true, "args": auditArgs(pc.Args), "summary": pc.Summary}
		out, isErr, status, summary := a.runPlanned(ctx, pc, rec)
		rec["error"] = isErr
		rec["result"] = textutil.OneLine(out, 300)
		a.Audit.Log(rec)
		a.emit(Event{Kind: EvToolResult, Tool: pc.Name, Text: out, IsError: isErr})

		if status == "cancelled" {
			rep.WriteString("   not run; it stays pending\n")
			stopped = "the operator cancelled; that action and the ones after it stayed pending"
			break
		}
		a.dropPending(pc)
		label := "ok"
		switch {
		case status == "denied":
			label = "denied"
		case isErr:
			label = "error"
			rep.WriteString("   (error reported to the model)\n")
		}
		results = append(results, fmt.Sprintf("### %d. %s: %s (%s)\n%s",
			len(results)+1, pc.Name, textutil.OneLine(summary, 200), label, strings.TrimRight(out, "\n")))
		if status == "denied" {
			if i < len(sel)-1 {
				stopped = "the operator denied an action; the ones after it stayed pending"
			}
			break
		}
		if ctx.Err() != nil {
			if i < len(sel)-1 {
				stopped = "the operator cancelled; the remaining actions stayed pending"
			}
			break
		}
	}
	if stopped != "" {
		rep.WriteString("stopped: " + stopped + "\n")
	}
	if comment != "" {
		rep.WriteString("your comment goes to the model with the results\n")
	}
	if len(results) == 0 {
		a.Save()
		return strings.TrimRight(rep.String(), "\n"), "", nil
	}

	msg := a.planResultsMessage(results, stopped, comment, opt.NoContinue)
	if opt.NoContinue {
		a.queueNote(msg)
		rep.WriteString("the results are held back and go to the model with your next message\n")
		msg = ""
	}
	a.Save()
	return strings.TrimRight(rep.String(), "\n"), msg, nil
}

// runPlanned prepares a pending call again and runs it through the normal
// approval path. status is "ran", "denied", "cancelled" (did not run) or
// "failed" (could not be prepared); summary describes the call as prepared
// now, or at planning time if preparing failed.
func (a *Agent) runPlanned(ctx context.Context, pc *session.PendingCall, rec map[string]any) (out string, isErr bool, status, summary string) {
	summary = pc.Summary
	t, ok := a.Registry.Get(pc.Name)
	if !ok {
		rec["decision"] = "failed"
		return fmt.Sprintf("Error: the tool %s is no longer available, so this action was not run.", pc.Name), true, "failed", summary
	}
	action, err := t.Prepare(ctx, tools.Args(copyArgs(pc.Args)))
	if err != nil {
		rec["decision"] = "failed"
		return "Error: this action could not be prepared again before running, so it was not run: " + err.Error(), true, "failed", summary
	}
	summary = action.Summary
	rec["summary"] = summary
	out, isErr = a.executeAction(ctx, pc.Name, t, action, rec)
	switch rec["decision"] {
	case "denied":
		return out, isErr, "denied", summary
	case "cancelled":
		return out, isErr, "cancelled", summary
	}
	return out, isErr, "ran", summary
}

// planResultsMessage is the harness message that hands the outcome of
// /plan approve to the model.
func (a *Agent) planResultsMessage(results []string, stopped, comment string, queued bool) string {
	var m strings.Builder
	if a.Plan() {
		m.WriteString("You are in plan mode. ")
	}
	m.WriteString("The operator approved planned actions with /plan approve; they were processed in order. Results:")
	for _, r := range results {
		m.WriteString("\n\n" + r)
	}
	if stopped != "" {
		m.WriteString("\n\nStopped early: " + stopped + ".")
	}
	if n := a.PlanPending(); n > 0 {
		fmt.Fprintf(&m, "\n\n%d planned action%s still pending.", n, pluralVerb(n))
	}
	if comment != "" {
		m.WriteString("\n\nOperator's comment:\n" + comment)
	}
	switch {
	case queued && comment != "":
		m.WriteString("\n\nThese results were held back and are delivered with the operator's next message. Follow the operator's comment above; it takes priority over continuing the plan.")
	case queued:
		m.WriteString("\n\nThese results were held back and are delivered with the operator's next message; respond to that message, taking the results into account.")
	case comment != "":
		m.WriteString("\n\nFollow the operator's comment above; it takes priority over continuing the plan.")
	default:
		m.WriteString("\n\nTake the results into account and continue; if the goal is reached, give your final answer.")
	}
	return m.String()
}

// PlanReject discards the selected pending calls and returns the operator
// report plus a message that hands the rejection (and any reason) to the
// model as the start of the next turn.
func (a *Agent) PlanReject(nums []int, reason string) (string, string, error) {
	sel, err := a.selectPending(nums)
	if err != nil {
		return "", "", err
	}
	var names []string
	for _, pc := range sel {
		names = append(names, pc.Name+" ("+textutil.OneLine(pc.Summary, 60)+")")
		a.dropPending(pc)
	}
	a.Audit.Log(map[string]any{
		"session": a.sess.ID,
		"event":   "plan_reject",
		"actions": names,
		"reason":  reason,
	})
	a.Save()
	rep := fmt.Sprintf("rejected %d planned action%s: %s", len(sel), pluralN(len(sel)), strings.Join(names, "; "))
	m := ""
	if a.Plan() {
		m = "You are in plan mode. "
	}
	m += "The operator rejected the following planned actions: " + strings.Join(names, "; ") + "."
	if reason != "" {
		m += " Their reason: " + reason
	} else {
		m += " No reason was given; if you are unsure what to do instead, ask the operator."
	}
	m += " Adjust the plan and continue."
	return rep, m, nil
}

// selectPending resolves /plan numbers (1 = first planned) to calls,
// returning them in planning order.
func (a *Agent) selectPending(nums []int) ([]*session.PendingCall, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pending := a.sess.Pending
	if len(nums) == 0 {
		if len(pending) == 0 {
			return nil, fmt.Errorf("no pending actions; /plan shows the state")
		}
		return append([]*session.PendingCall(nil), pending...), nil
	}
	want := make(map[int]bool, len(nums))
	for _, n := range nums {
		if n < 1 || n > len(pending) {
			return nil, fmt.Errorf("no pending action %d; this session has %d (see /plan)", n, len(pending))
		}
		want[n] = true
	}
	var out []*session.PendingCall
	for i, pc := range pending {
		if want[i+1] {
			out = append(out, pc)
		}
	}
	return out, nil
}

func (a *Agent) dropPending(pc *session.PendingCall) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, p := range a.sess.Pending {
		if p == pc {
			a.sess.Pending = append(a.sess.Pending[:i], a.sess.Pending[i+1:]...)
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

func pluralVerb(n int) string {
	if n == 1 {
		return " is"
	}
	return "s are"
}
