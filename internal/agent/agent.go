// Package agent is the core of clauzette: it owns the session, runs the agent
// loop (model → tool calls → results → model …), manages the context
// window and reports everything as events. It has no terminal code.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"clauzette/internal/config"
	"clauzette/internal/gpu"
	"clauzette/internal/ollama"
	"clauzette/internal/session"
	"clauzette/internal/textutil"
	"clauzette/internal/tools"
)

type Deps struct {
	Cfg      *config.Config
	Client   *ollama.Client
	Registry *tools.Registry
	Store    *session.Store
	Guard    *gpu.Guard
	Audit    *Audit
	WS       *tools.Workspace // lets safe-mode restore mark restored files as read
}

// Agent runs one session. RunTurn and Compact must not run concurrently;
// the front end guarantees that. SetThink/SetShow/SetSafe/SetPlan may be
// called at any time.
type Agent struct {
	Deps
	sess   *session.Session
	events chan<- Event
	think  atomic.Bool
	show   atomic.Bool
	safe   atomic.Bool
	plan   atomic.Bool
	rogue  atomic.Bool
	// rogueSteps is the step budget per message set by /rogue on <n>; 0
	// means agent.max_steps.
	rogueSteps atomic.Int64
	// mu guards the session fields the front end may touch while a turn
	// runs (Pending, QueuedNotes) and serializes saving.
	mu         sync.Mutex
	specs      []ollama.ToolSpec
	rogueSpecs []ollama.ToolSpec // specs plus finish
	fixedChars int               // system prompt + tool definitions
	rogueFixed int               // the same with rogueSpecs
	rt         *rogueTurn        // the current turn in rogue mode, nil otherwise
	cpt        float64           // estimated characters per token, calibrated from Ollama's counts
	always     map[string]bool
}

func New(d Deps, sess *session.Session, events chan<- Event) *Agent {
	a := &Agent{Deps: d, sess: sess, events: events, cpt: 3.5, always: map[string]bool{}}
	a.specs = d.Registry.Specs()
	a.rogueSpecs = append(append([]ollama.ToolSpec(nil), a.specs...), finishSpec())
	a.recomputeFixed()
	a.think.Store(sess.Think)
	a.show.Store(sess.ShowThinking)
	a.safe.Store(sess.Safe)
	a.plan.Store(sess.Plan)
	for _, name := range sess.AlwaysApprove {
		a.always[name] = true
	}
	if sess.CharsPerToken > 0 {
		a.cpt = sess.CharsPerToken
	}
	return a
}

func (a *Agent) recomputeFixed() {
	specJSON, _ := json.Marshal(a.specs)
	a.fixedChars = len(a.sess.System) + len(specJSON) + 64
	rogueJSON, _ := json.Marshal(a.rogueSpecs)
	a.rogueFixed = len(a.rogueSystem()) + len(rogueJSON) + 64
}

func (a *Agent) Session() *session.Session { return a.sess }
func (a *Agent) Think() bool               { return a.think.Load() }
func (a *Agent) SetThink(v bool)           { a.think.Store(v) }
func (a *Agent) Show() bool                { return a.show.Load() }
func (a *Agent) SetShow(v bool)            { a.show.Store(v) }
func (a *Agent) Safe() bool                { return a.safe.Load() }
func (a *Agent) SetSafe(v bool)            { a.safe.Store(v) }
func (a *Agent) Plan() bool                { return a.plan.Load() }

// SetSystems replaces the session's system prompts, the normal one and
// the one for rogue mode (used by /system-prompt reload, and by /rogue on
// for sessions saved before rogue mode had its own prompt).
func (a *Agent) SetSystems(normal, rogue string) {
	a.sess.System, a.sess.SystemRogue = normal, rogue
	a.recomputeFixed()
}

// HasRogueSystem reports whether the session has a system prompt for rogue mode.
func (a *Agent) HasRogueSystem() bool { return a.sess.SystemRogue != "" }

// system is the system prompt for the next request.
func (a *Agent) system() string {
	if a.rogue.Load() {
		return a.rogueSystem()
	}
	return a.sess.System
}

// rogueSystem is the rogue-mode system prompt, or the normal one in a
// session that has none.
func (a *Agent) rogueSystem() string {
	if a.sess.SystemRogue != "" {
		return a.sess.SystemRogue
	}
	return a.sess.System
}

// AutoApproved lists tools that currently run without asking.
func (a *Agent) AutoApproved() []string {
	set := map[string]bool{}
	for _, n := range a.Cfg.Tools.AutoApprove {
		set[n] = true
	}
	for n := range a.always {
		set[n] = true
	}
	var out []string
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ResetApprovals forgets the "always" answers given in this session.
func (a *Agent) ResetApprovals() {
	a.always = map[string]bool{}
	a.sess.AlwaysApprove = nil
	a.Save()
}

func (a *Agent) autoApproved(name string) bool {
	if a.always[name] {
		return true
	}
	for _, n := range a.Cfg.Tools.AutoApprove {
		if n == name {
			return true
		}
	}
	return false
}

func (a *Agent) emit(e Event) { a.events <- e }
func (a *Agent) info(format string, args ...any) {
	a.emit(Event{Kind: EvInfo, Text: fmt.Sprintf(format, args...)})
}
func (a *Agent) warnf(format string, args ...any) {
	a.emit(Event{Kind: EvWarn, Text: fmt.Sprintf(format, args...)})
}

// Save writes the session to disk. Called after every step, so a killed
// pod or dropped connection loses at most the step in progress.
func (a *Agent) Save() {
	a.sess.Think = a.think.Load()
	a.sess.ShowThinking = a.show.Load()
	a.sess.Safe = a.safe.Load()
	a.sess.Plan = a.plan.Load()
	a.sess.CharsPerToken = a.cpt
	a.mu.Lock()
	err := a.Store.Save(a.sess)
	a.mu.Unlock()
	if err != nil {
		a.warnf("could not save the session: %v", err)
	}
}

// requestOptions are the Ollama options for every request. They must be the
// same for all requests (warm-up, turns, compaction): a different num_ctx
// makes Ollama reload the model.
func requestOptions(cfg *config.Config) map[string]any {
	o := make(map[string]any, len(cfg.Options)+1)
	for k, v := range cfg.Options {
		o[k] = v
	}
	o["num_ctx"] = cfg.NumCtx
	return o
}

// buildMessages is the exact message list sent to the model: the frozen
// system prompt followed by the working context. Reasoning from earlier,
// completed turns is dropped (Qwen-style templates expect this, and it
// saves a lot of context); reasoning within the current turn is kept, so
// the model remembers why it called its tools.
func (a *Agent) buildMessages() []ollama.Message {
	msgs := make([]ollama.Message, 0, len(a.sess.Context)+1)
	msgs = append(msgs, ollama.Message{Role: "system", Content: a.system()})
	lastUser := -1
	for i, e := range a.sess.Context {
		if e.Role == "user" {
			lastUser = i
		}
	}
	for i, e := range a.sess.Context {
		m := e.Message
		if m.Role == "assistant" && i < lastUser {
			m.Thinking = ""
		}
		msgs = append(msgs, m)
	}
	return msgs
}

// RunTurn handles one operator message: it calls the model, runs the tools
// it asks for, and repeats until the model answers without tool calls.
// Cancelling ctx (the operator typed !c) stops it at the next safe point
// and leaves a valid history behind.
func (a *Agent) RunTurn(ctx context.Context, text string) error {
	return a.runTurn(ctx, text, "")
}

// RunHarnessTurn starts a turn with a harness message (kind
// session.KindHarness) instead of an operator message, so /undo and /retry
// do not mistake it for something the operator typed.
func (a *Agent) RunHarnessTurn(ctx context.Context, text string) error {
	return a.runTurn(ctx, text, session.KindHarness)
}

func (a *Agent) runTurn(ctx context.Context, text, kind string) error {
	defer a.Save()
	if a.sess.Title == "" && kind == "" {
		a.sess.Title = textutil.OneLine(text, 60)
	}
	a.flushNotes()
	a.sess.Append(ollama.Message{Role: "user", Content: text}, kind)
	a.Save()
	rt := a.startRogueTurn()
	defer func() { a.rt = nil }()

	// In rogue mode the work runs under the time limit: sctx ends at the
	// deadline, which stops whatever is in progress (a model reply, thinking
	// included, a command, a GPU pause). ctx stays the operator's.
	sctx := ctx
	if rt != nil && !rt.deadline.IsZero() {
		var cancel context.CancelFunc
		sctx, cancel = context.WithDeadline(ctx, rt.deadline)
		defer cancel()
	}
	timedOut := func() bool { return rt != nil && sctx.Err() != nil && ctx.Err() == nil }

	for step := 1; ; step++ {
		if step > 1 {
			a.flushNotes()
		}
		if rt != nil && !a.rogue.Load() {
			// /rogue off during the turn: approvals, agent.max_steps and no time limit from here
			rt, a.rt, sctx = nil, nil, ctx
		}
		if rt != nil {
			if reason := rt.finalReason(step); reason != "" {
				return a.rogueFinal(ctx, reason)
			}
		} else if limit := a.Cfg.Agent.MaxSteps; limit > 0 && step > limit {
			a.warnf("stopped after %d model calls without a final answer (agent.max_steps); send a message to let it continue", limit)
			a.sess.Append(ollama.Message{Role: "assistant", Content: "[Stopped by the harness: step limit reached before a final answer.]"}, session.KindInterrupted)
			return nil
		}
		if err := a.Guard.Wait(sctx, func(s string) { a.info("%s", s) }); err != nil {
			if timedOut() {
				return a.rogueFinal(ctx, rogueTimeLimit)
			}
			a.markInterrupted(nil, "")
			return err
		}
		if err := a.ensureRoom(sctx); err != nil {
			if timedOut() {
				return a.rogueFinal(ctx, rogueTimeLimit)
			}
			if ctx.Err() != nil {
				a.markInterrupted(nil, "")
				return ctx.Err()
			}
			return err
		}
		res, err := a.callModel(sctx, a.think.Load())
		if err != nil {
			if timedOut() {
				a.markInterrupted(res, "[Stopped by the harness: rogue mode's time limit was reached.]")
				return a.rogueFinal(ctx, rogueTimeLimit)
			}
			if ctx.Err() != nil {
				a.markInterrupted(res, "")
				return ctx.Err()
			}
			if res != nil && (res.Message.Content != "" || res.Message.Thinking != "") {
				a.markInterrupted(res, fmt.Sprintf("[The response failed: %v]", err))
			}
			return err
		}
		a.sess.Append(res.Message, "")
		a.updateUsage(res)
		a.Save()

		if len(res.Message.ToolCalls) == 0 {
			if rt != nil && !rt.nudged {
				rt.nudged = true
				a.appendHarness(rt.nudge(step))
				a.info("rogue: the model replied without acting or calling finish; nudging it once to continue")
				continue
			}
			if strings.TrimSpace(res.Message.Content) == "" {
				a.warnf("the model returned an empty reply")
			}
			return nil
		}
		for i, call := range res.Message.ToolCalls {
			if ctx.Err() != nil {
				a.cancelRemaining(res.Message.ToolCalls[i:])
				return ctx.Err()
			}
			if timedOut() {
				a.skipCalls(res.Message.ToolCalls[i:], "Not run: rogue mode's time limit was reached.")
				return a.rogueFinal(ctx, rogueTimeLimit)
			}
			out := a.runTool(sctx, call)
			if rt != nil && !rt.finished {
				out += rt.reminder(step)
			}
			a.sess.Append(ollama.Message{Role: "tool", ToolName: call.Function.Name, Content: out}, "")
			a.Save()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if rt != nil && rt.finished {
			a.info("rogue: the model finished the task: %s", rt.summary)
			return nil
		}
		if timedOut() {
			return a.rogueFinal(ctx, rogueTimeLimit)
		}
	}
}

// markInterrupted records a reply that was cut short, so the model knows
// on the next turn that its previous answer is incomplete.
func (a *Agent) markInterrupted(res *ollama.Result, note string) {
	if note == "" {
		note = "[Interrupted by the operator.]"
	}
	m := ollama.Message{Role: "assistant"}
	if res != nil {
		m.Content, m.Thinking = res.Message.Content, res.Message.Thinking
	}
	if strings.TrimSpace(m.Content) == "" && strings.TrimSpace(m.Thinking) == "" {
		m.Content = strings.TrimSuffix(note, ".]") + " before responding.]"
	} else {
		m.Content = strings.TrimSpace(m.Content + "\n\n" + note)
	}
	a.sess.Append(m, session.KindInterrupted)
}

// cancelRemaining gives every tool call that will not run a result, because
// a tool call without a result confuses most models (and some templates
// reject it).
func (a *Agent) cancelRemaining(calls []ollama.ToolCall) {
	a.skipCalls(calls, "Cancelled by the operator before it ran.")
}

// skipCalls gives each tool call that will not run a result saying why.
func (a *Agent) skipCalls(calls []ollama.ToolCall, why string) {
	for _, c := range calls {
		a.sess.Append(ollama.Message{Role: "tool", ToolName: c.Function.Name, Content: why}, session.KindCancelled)
	}
}

func (a *Agent) callModel(ctx context.Context, think bool) (*ollama.Result, error) {
	msgs := a.buildMessages()
	req := ollama.ChatRequest{
		Model:     a.Cfg.Model,
		Messages:  msgs,
		Tools:     a.toolSpecs(),
		Think:     &think,
		Options:   requestOptions(a.Cfg),
		KeepAlive: a.Cfg.KeepAlive,
	}
	a.emit(Event{Kind: EvWaiting, Text: "waiting for the model"})
	waiting := true
	res, err := a.Client.ChatStream(ctx, req, func(d ollama.Message) {
		if waiting {
			waiting = false
			a.emit(Event{Kind: EvWaitingDone})
		}
		if d.Thinking != "" {
			a.emit(Event{Kind: EvThinking, Text: d.Thinking})
		}
		if d.Content != "" {
			a.emit(Event{Kind: EvText, Text: d.Content})
		}
	})
	if waiting {
		a.emit(Event{Kind: EvWaitingDone})
	}
	a.emit(Event{Kind: EvStreamEnd})
	if err == nil {
		a.calibrate(msgs, res)
	}
	return res, err
}

func (a *Agent) runTool(ctx context.Context, call ollama.ToolCall) string {
	name := call.Function.Name
	args := tools.Args(call.Function.Arguments)
	if args == nil {
		args = tools.Args{}
	}
	a.emit(Event{Kind: EvToolCall, Tool: name, Text: summarizeArgs(call.Function.Arguments)})

	start := time.Now()
	rec := map[string]any{"session": a.sess.ID, "tool": name, "args": auditArgs(call.Function.Arguments)}
	finish := func(out string, isErr bool) string {
		rec["duration_ms"] = time.Since(start).Milliseconds()
		rec["error"] = isErr
		rec["result"] = textutil.OneLine(out, 300)
		a.Audit.Log(rec)
		a.emit(Event{Kind: EvToolResult, Tool: name, Text: out, IsError: isErr})
		return out
	}

	if name == finishTool && a.rt != nil {
		rec["decision"] = "rogue"
		return finish(a.runFinish(args))
	}
	t, ok := a.Registry.Get(name)
	if !ok {
		return finish(fmt.Sprintf("Error: there is no tool named %q. Available tools: %s.",
			name, strings.Join(a.Registry.Names(), ", ")), true)
	}
	action, err := t.Prepare(ctx, args)
	if err != nil {
		return finish("Error: "+err.Error(), true)
	}
	rec["summary"] = action.Summary

	if a.plan.Load() && t.Risk() != tools.ReadOnly {
		a.mu.Lock()
		a.sess.Pending = append(a.sess.Pending, &session.PendingCall{Name: name, Args: copyArgs(args), Summary: action.Summary})
		n := len(a.sess.Pending)
		a.mu.Unlock()
		rec["decision"] = "planned"
		out := fmt.Sprintf("Planned, not executed (pending action %d): %s", n, action.Summary)
		out += " Plan mode is on: the operator reviews the list with /plan and runs it with /plan approve."
		out += " Keep reading and refining the plan; do not try to reach the same result through another tool."
		return finish(out, false)
	}

	out, isErr := a.executeAction(ctx, name, t, action, rec)
	return finish(out, isErr)
}

// executeAction asks for approval when needed, makes the safe-mode backup,
// and runs the prepared action. It returns the result text and whether it
// was an error or denial. runTool uses it for live actions, and /plan
// approve uses it for planned ones, so both paths behave identically.
func (a *Agent) executeAction(ctx context.Context, name string, t tools.Tool, action *tools.Action, rec map[string]any) (string, bool) {
	rogue := a.rogue.Load()
	auto := t.Risk() == tools.ReadOnly || (a.autoApproved(name) && !(a.safe.Load() && t.Risk() == tools.Destructive))
	switch {
	case auto:
		rec["decision"] = "auto"
	case rogue:
		rec["decision"] = "rogue"
	default:
		reply := make(chan Decision, 1)
		summ := action.Summary
		if a.safe.Load() {
			switch t.Risk() {
			case tools.Destructive:
				summ += " (safe mode: the target is backed up before it runs)"
			case tools.Exec:
				summ += " (safe mode: files changed by this command are NOT backed up)"
			}
		}
		a.emit(Event{Kind: EvApproval, Tool: name, Approval: &ApprovalRequest{
			Tool: name, Summary: summ, Preview: action.Preview, Risk: t.Risk(), Reply: reply,
		}})
		select {
		case d := <-reply:
			if !d.Approve {
				rec["decision"] = "denied"
				out := "The operator denied this action."
				if d.Reason != "" {
					out += " Their reason: " + d.Reason
				}
				out += " Do not try to reach the same result another way; adjust your plan or ask the operator."
				return out, true
			}
			rec["decision"] = "approved"
			if d.Always {
				rec["decision"] = "approved_always"
				if !a.always[name] {
					a.always[name] = true
					a.sess.AlwaysApprove = append(a.sess.AlwaysApprove, name)
				}
			}
		case <-ctx.Done():
			rec["decision"] = "cancelled"
			return "Cancelled by the operator before it ran.", true
		}
	}

	// Rogue mode always backs up: nobody reviews a move or delete there.
	if (a.safe.Load() || rogue) && len(action.Backups) > 0 {
		dest, err := a.backupTargets(name, action.Backups)
		if err != nil {
			return fmt.Sprintf("Error: safe mode could not make a backup before %s, so it was not run: %v", name, err), true
		}
		rec["backup"] = dest
		a.info("safe mode: backed up before %s → %s (/safe lists backups, /safe restore <n> puts them back)", name, dest)
	}

	out, err := action.Run(ctx)
	if err != nil {
		return "Error: " + err.Error(), true
	}
	return out, false
}

func (a *Agent) updateUsage(res *ollama.Result) {
	a.sess.LastPromptTokens = res.PromptEvalCount
	a.sess.LastEvalTokens = res.EvalCount
	a.emit(Event{Kind: EvUsage, Usage: a.Usage(res)})
}

// Usage describes the current context use; res may be nil.
func (a *Agent) Usage(res *ollama.Result) Usage {
	u := Usage{
		Tokens:     a.ContextTokens(),
		NumCtx:     a.Cfg.NumCtx,
		PromptEval: a.sess.LastPromptTokens,
		Eval:       a.sess.LastEvalTokens,
	}
	if res != nil {
		u.TokPerSec = res.TokensPerSecond()
	}
	return u
}

// Undo removes the last exchange (from the operator's last message to the
// end) from the working context and returns that message.
func (a *Agent) Undo() (string, error) {
	ctxs := a.sess.Context
	idx := -1
	for i := len(ctxs) - 1; i >= 0; i-- {
		if ctxs[i].Kind == session.KindSummary {
			return "", errors.New("the last exchange has been compacted into a summary and can't be undone")
		}
		if ctxs[i].Role == "user" && ctxs[i].Kind == "" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", errors.New("nothing to undo")
	}
	text := ctxs[idx].Content
	a.sess.Context = ctxs[:idx]
	a.sess.Note("/undo removed the last exchange from the working context")
	a.Save()
	return text, nil
}

// summarizeArgs renders tool arguments compactly for display.
func summarizeArgs(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		var v string
		switch t := m[k].(type) {
		case string:
			if len(t) > 80 || strings.Contains(t, "\n") {
				v = fmt.Sprintf("‹%d chars›", len(t))
			} else {
				v = fmt.Sprintf("%q", t)
			}
		default:
			b, _ := json.Marshal(t)
			v = textutil.Truncate(string(b), 60)
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, " ")
}

// copyArgs makes a shallow copy of tool arguments, so a planned call does
// not share its map with the message history.
func copyArgs(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// auditArgs copies the arguments, shortening long strings (file contents).
func auditArgs(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok && len(s) > 2000 {
			v = textutil.Truncate(s, 2000) + fmt.Sprintf(" [%d chars total]", len(s))
		}
		out[k] = v
	}
	return out
}
