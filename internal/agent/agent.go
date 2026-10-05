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
	sess       *session.Session
	events     chan<- Event
	think      atomic.Bool
	show       atomic.Bool
	safe       atomic.Bool
	plan       atomic.Bool
	pending    []*PendingAction // plan mode: prepared actions awaiting /plan approve
	specs      []ollama.ToolSpec
	fixedChars int     // system prompt + tool definitions
	cpt        float64 // estimated characters per token, calibrated from Ollama's counts
	always     map[string]bool
}

func New(d Deps, sess *session.Session, events chan<- Event) *Agent {
	a := &Agent{Deps: d, sess: sess, events: events, cpt: 3.5, always: map[string]bool{}}
	a.specs = d.Registry.Specs()
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
}

func (a *Agent) Session() *session.Session { return a.sess }
func (a *Agent) Think() bool                { return a.think.Load() }
func (a *Agent) SetThink(v bool)            { a.think.Store(v) }
func (a *Agent) Show() bool                 { return a.show.Load() }
func (a *Agent) SetShow(v bool)             { a.show.Store(v) }
func (a *Agent) Safe() bool                 { return a.safe.Load() }
func (a *Agent) SetSafe(v bool)             { a.safe.Store(v) }
func (a *Agent) Plan() bool                 { return a.plan.Load() }

// SetSystem replaces the session's system prompt (used by /system reload).
func (a *Agent) SetSystem(s string) {
	a.sess.System = s
	a.recomputeFixed()
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

func (a *Agent) emit(e Event)                     { a.events <- e }
func (a *Agent) info(format string, args ...any)  { a.emit(Event{Kind: EvInfo, Text: fmt.Sprintf(format, args...)}) }
func (a *Agent) warnf(format string, args ...any) { a.emit(Event{Kind: EvWarn, Text: fmt.Sprintf(format, args...)}) }

// Save writes the session to disk. Called after every step, so a killed
// pod or dropped connection loses at most the step in progress.
func (a *Agent) Save() {
	a.sess.Think = a.think.Load()
	a.sess.ShowThinking = a.show.Load()
	a.sess.Safe = a.safe.Load()
	a.sess.Plan = a.plan.Load()
	a.sess.CharsPerToken = a.cpt
	if err := a.Store.Save(a.sess); err != nil {
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
	msgs = append(msgs, ollama.Message{Role: "system", Content: a.sess.System})
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
	defer a.Save()
	if a.sess.Title == "" {
		a.sess.Title = textutil.OneLine(text, 60)
	}
	a.sess.Append(ollama.Message{Role: "user", Content: text}, "")
	a.Save()

	for step := 1; ; step++ {
		if limit := a.Cfg.Agent.MaxSteps; limit > 0 && step > limit {
			a.warnf("stopped after %d model calls without a final answer (agent.max_steps); send a message to let it continue", limit)
			a.sess.Append(ollama.Message{Role: "assistant", Content: "[Stopped by the harness: step limit reached before a final answer.]"}, session.KindInterrupted)
			return nil
		}
		if err := a.Guard.Wait(ctx, func(s string) { a.info("%s", s) }); err != nil {
			a.markInterrupted(nil, "")
			return err
		}
		if err := a.ensureRoom(ctx); err != nil {
			if ctx.Err() != nil {
				a.markInterrupted(nil, "")
				return ctx.Err()
			}
			return err
		}
		res, err := a.callModel(ctx)
		if err != nil {
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
			out := a.runTool(ctx, call)
			a.sess.Append(ollama.Message{Role: "tool", ToolName: call.Function.Name, Content: out}, "")
			a.Save()
		}
		if ctx.Err() != nil {
			return ctx.Err()
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
	for _, c := range calls {
		a.sess.Append(ollama.Message{Role: "tool", ToolName: c.Function.Name, Content: "Cancelled by the operator before it ran."}, session.KindCancelled)
	}
}

func (a *Agent) callModel(ctx context.Context) (*ollama.Result, error) {
	msgs := a.buildMessages()
	think := a.think.Load()
	req := ollama.ChatRequest{
		Model:     a.Cfg.Model,
		Messages:  msgs,
		Tools:     a.specs,
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
		a.pending = append(a.pending, &PendingAction{Name: name, Action: action})
		rec["decision"] = "planned"
		out := fmt.Sprintf("Planned, not executed (pending action %d): %s", len(a.pending), action.Summary)
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
	auto := t.Risk() == tools.ReadOnly || (a.autoApproved(name) && !(a.safe.Load() && t.Risk() == tools.Destructive))
	if auto {
		rec["decision"] = "auto"
	} else {
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

	if a.safe.Load() && len(action.Backups) > 0 {
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
