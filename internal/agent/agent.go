// Package agent is the core of clauzette: it owns the session, runs the agent
// loop (model → tool calls → results → model …), manages the context
// window and reports everything as events. It has no terminal code.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"clauzette/internal/llm"
	"clauzette/internal/modeldb"
	"clauzette/internal/ollama"
	"clauzette/internal/session"
	"clauzette/internal/textutil"
	"clauzette/internal/tools"
)

type Deps struct {
	Cfg      *config.Config
	Client   llm.Backend
	Models   *Models // the active profile; nil = the config's default profile, nothing remembered
	Registry *tools.Registry
	Store    *session.Store
	Guard    *gpu.Guard
	GPUMem   *gpu.Memory // reads GPU memory on demand (/adapt); nil without gpu.metrics_url
	Audit    *Audit
	WS       *tools.Workspace   // lets safe-mode restore mark restored files as read
	Todos    *tools.TodoList    // the agent's task plan, shared with the todo tools
	Fetch    *tools.FetchPolicy // the fetch_url allowlist, shared with the fetch tool
	Images   *tools.ImageStore  // attached images, shared with the view_image tool; nil = no images
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
	chatFixed  int               // the chat-only system prompt (no tool definitions)
	rt         *rogueTurn        // the current turn in rogue mode, nil otherwise
	// cpt is the estimated characters per token, calibrated from Ollama's
	// counts. It belongs to the model's weights, not to the session: it is
	// read from and written back to the models file (cptKey), shared by all
	// sessions. cptSaved is the value last written.
	cpt          float64
	cptKey       string
	cptSaved     float64
	limitsWarned string // last context-limit warning shown, so it is shown once
	always       map[string]bool
}

const defaultCPT = 3.5

func New(d Deps, sess *session.Session, events chan<- Event) *Agent {
	if d.Models == nil {
		p, _ := d.Cfg.FindProfile(d.Cfg.DefaultProfileName())
		d.Models = NewModels(d.Cfg, nil, p)
	}
	if d.Models.Local == nil {
		d.Models.Local = d.Client
	}
	a := &Agent{Deps: d, sess: sess, events: events, cpt: defaultCPT, always: map[string]bool{}}
	a.specs = d.Registry.Specs()
	a.rogueSpecs = append(append([]ollama.ToolSpec(nil), a.specs...), finishSpec(), restoreBackupSpec())
	a.recomputeFixed()
	a.think.Store(sess.Think)
	a.show.Store(sess.ShowThinking)
	a.safe.Store(sess.Safe)
	a.plan.Store(sess.Plan)
	for _, name := range sess.AlwaysApprove {
		a.always[name] = true
	}
	a.syncCPT(a.Models.Active())
	if a.Todos != nil && len(sess.Todos) > 0 {
		var items []tools.TodoItem
		if err := json.Unmarshal(sess.Todos, &items); err == nil {
			a.Todos.Set(items)
		}
	}
	if a.Fetch != nil {
		for _, h := range sess.FetchAllowed {
			a.Fetch.Add(h)
		}
	}
	return a
}

func (a *Agent) recomputeFixed() {
	specJSON, _ := json.Marshal(a.specs)
	a.fixedChars = len(a.sess.System) + len(specJSON) + 64
	rogueJSON, _ := json.Marshal(a.rogueSpecs)
	a.rogueFixed = len(a.rogueSystem()) + len(rogueJSON) + 64
	a.chatFixed = len(a.chatSystem()) + 64
}

func (a *Agent) Session() *session.Session { return a.sess }
func (a *Agent) Think() bool               { return a.think.Load() }
func (a *Agent) SetThink(v bool)           { a.think.Store(v) }
func (a *Agent) Show() bool                { return a.show.Load() }
func (a *Agent) SetShow(v bool)            { a.show.Store(v) }
func (a *Agent) Safe() bool                { return a.safe.Load() }
func (a *Agent) SetSafe(v bool)            { a.safe.Store(v) }
func (a *Agent) Plan() bool                { return a.plan.Load() }

// Model is the active profile; ChatOnly, ThinkAvailable and Window are
// shortcuts for the UI.
func (a *Agent) Model() *Active       { return a.Models.Active() }
func (a *Agent) ChatOnly() bool       { return a.Models.Active().ChatOnly() }
func (a *Agent) ThinkAvailable() bool { return a.Models.Active().Caps.Thinking }
func (a *Agent) Window() int          { return a.Models.Active().NumCtx }

// Prompts are a session's frozen system prompts: the normal one, the one
// for rogue mode, and the one for models without tools.
type Prompts struct {
	Normal, Rogue, Chat string
}

// SetSystems replaces the session's system prompts (used by /system-prompt
// reload, and to add a prompt missing from sessions saved before it
// existed).
func (a *Agent) SetSystems(p Prompts) {
	a.sess.System, a.sess.SystemRogue, a.sess.SystemChat = p.Normal, p.Rogue, p.Chat
	a.recomputeFixed()
}

// HasRogueSystem reports whether the session has a system prompt for rogue mode.
func (a *Agent) HasRogueSystem() bool { return a.sess.SystemRogue != "" }

// HasChatSystem reports whether the session has a system prompt for chat-only models.
func (a *Agent) HasChatSystem() bool { return a.sess.SystemChat != "" }

// system is the system prompt for the next request.
func (a *Agent) system() string {
	if a.ChatOnly() {
		return a.chatSystem()
	}
	if a.rogue.Load() {
		return a.rogueSystem()
	}
	return a.sess.System
}

// chatSystem is the prompt for a model without tools, or the normal one in
// a session that has none.
func (a *Agent) chatSystem() string {
	if a.sess.SystemChat != "" {
		return a.sess.SystemChat
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
	if a.Todos != nil {
		if b, err := json.Marshal(a.Todos.Items()); err == nil {
			a.sess.Todos = b
		}
	}
	if a.Fetch != nil {
		a.sess.FetchAllowed = a.Fetch.List()
	}
	a.mu.Lock()
	err := a.Store.Save(a.sess)
	a.mu.Unlock()
	if err != nil {
		a.warnf("could not save the session: %v", err)
	}
	a.saveCPT()
}

// syncCPT switches the token estimate to the active model's weights: the
// value learned in earlier sessions, if any. Until the weights are known,
// or for weights never seen, the current estimate is kept and refined.
func (a *Agent) syncCPT(act *Active) {
	if act.Key == a.cptKey {
		return
	}
	if r, ok := a.Models.DB.Get(act.Key); ok && r.CharsPerToken > 0 {
		a.cpt = r.CharsPerToken
		a.cptSaved = r.CharsPerToken
	}
	a.cptKey = act.Key
}

// saveCPT writes the estimate back to the models file when it changed.
func (a *Agent) saveCPT() {
	if a.cptKey == "" || a.cpt == a.cptSaved {
		return
	}
	cpt, model := a.cpt, a.Models.Active().Model
	a.Models.DB.Update(a.cptKey, model, func(r *modeldb.Record) { r.CharsPerToken = cpt })
	if err := a.Models.DB.Save(); err != nil {
		a.warnf("could not save the models file: %v", err)
		return
	}
	a.cptSaved = cpt
}

// newToolCallID returns an id for a tool call that came without one.
func newToolCallID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "call_" + hex.EncodeToString(b)
}

// assignToolCallIDs gives every call an id, so its result can name it.
func assignToolCallIDs(calls []ollama.ToolCall) {
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = newToolCallID()
		}
	}
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
		if e.Role == "user" && e.Kind != session.KindImage {
			lastUser = i // images the model asked for belong to the current turn
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
	return a.runTurnWith(ctx, text, kind, nil)
}

func (a *Agent) runTurnWith(ctx context.Context, text, kind string, images []llm.Image) error {
	defer a.Save()
	if a.sess.Title == "" && kind == "" {
		a.sess.Title = textutil.OneLine(text, 60)
	}
	if a.Images != nil {
		a.Images.Take() // left over from a step that never finished
	}
	a.flushNotes()
	a.sess.Append(ollama.Message{Role: "user", Content: text, ImageRefs: images}, kind)
	a.Save()
	if a.ChatOnly() {
		// A session in rogue or plan mode resumed with a model without
		// tools: both modes are about tool calls, so they cannot apply.
		if a.rogue.Swap(false) {
			a.warnf("rogue mode switched off: %s has no tool support", a.Models.Active().Model)
		}
		if a.plan.Swap(false) {
			a.warnf("plan mode switched off: %s has no tool support", a.Models.Active().Model)
		}
	}
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
		guard := a.Guard
		if a.Models.Active().Cloud != "" {
			guard = nil // a cloud model does not heat this GPU
		}
		if err := guard.Wait(sctx, func(s string) { a.info("%s", s) }); err != nil {
			if timedOut() {
				return a.rogueFinal(ctx, rogueTimeLimit)
			}
			a.markInterrupted(nil, "")
			return err
		}
		a.syncCPT(a.Models.Active())
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
		assignToolCallIDs(res.Message.ToolCalls)
		a.sess.AppendFrom(res.Message, "", a.Models.Active().Model)
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
				a.takeToolImages()
				return ctx.Err()
			}
			if timedOut() {
				a.skipCalls(res.Message.ToolCalls[i:], "Not run: rogue mode's time limit was reached.")
				a.takeToolImages()
				return a.rogueFinal(ctx, rogueTimeLimit)
			}
			out := a.runTool(sctx, call)
			if rt != nil && !rt.finished {
				out += rt.reminder(step)
			}
			a.sess.Append(ollama.Message{Role: "tool", ToolName: call.Function.Name, ToolCallID: call.ID, Content: out}, "")
			a.Save()
		}
		a.takeToolImages()
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
	model := ""
	if res != nil {
		model = a.Models.Active().Model
	}
	a.sess.AppendFrom(m, session.KindInterrupted, model)
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
		a.sess.Append(ollama.Message{Role: "tool", ToolName: c.Function.Name, ToolCallID: c.ID, Content: why}, session.KindCancelled)
	}
}

func (a *Agent) callModel(ctx context.Context, think bool) (*ollama.Result, error) {
	act := a.Models.Active()
	msgs := a.attachImages(a.buildMessages())
	req := ollama.ChatRequest{
		Model:     act.Model,
		Messages:  msgs,
		Tools:     a.toolSpecs(),
		Think:     act.think(think),
		Options:   act.RequestOptions(),
		KeepAlive: act.KeepAlive,
	}
	b, err := a.backend(act)
	if err != nil {
		return nil, err
	}
	split := newThinkSplitter(act, req)
	a.emit(Event{Kind: EvWaiting, Text: "waiting for the model"})
	waiting := true
	res, err := b.ChatStream(ctx, req, func(d ollama.Message) {
		if waiting {
			waiting = false
			a.emit(Event{Kind: EvWaitingDone})
		}
		if d.Thinking != "" {
			a.emit(Event{Kind: EvThinking, Text: d.Thinking})
		}
		if d.Content != "" {
			thinking, answer := split.delta(d.Content)
			if thinking != "" {
				a.emit(Event{Kind: EvThinking, Text: thinking})
			}
			if answer != "" {
				a.emit(Event{Kind: EvText, Text: answer})
			}
		}
	})
	if waiting {
		a.emit(Event{Kind: EvWaitingDone})
	}
	if late := split.late(); late != "" {
		a.emit(Event{Kind: EvText, Text: late})
	}
	a.emit(Event{Kind: EvStreamEnd})
	if res != nil {
		split.fix(&res.Message)
	}
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
		out = tools.Redact(out)
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
	if name == restoreBackupTool && a.rt != nil {
		rec["decision"] = "rogue"
		return finish(a.runRestoreForModel(args))
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
	// scoped: fetch_url with the allowlist gate on. A listed host runs without
	// a prompt (even in manual mode); an unlisted host is always asked about,
	// even in rogue mode, with the option to add it to the allowlist.
	scoped := a.Fetch != nil && a.Fetch.GateOn() && action.Scope != ""
	auto := t.Risk() == tools.ReadOnly || (a.autoApproved(name) && !(a.safe.Load() && t.Risk() == tools.Destructive))
	switch {
	case auto:
		// auto_approve (or a read-only tool) runs without asking, and wins over
		// the fetch allowlist gate: it is the operator's explicit blanket OK.
		rec["decision"] = "auto"
	case scoped && a.Fetch.Allowed(action.Scope):
		rec["decision"] = "allowlisted"
	case scoped:
		if out, isErr, ok := a.askOperator(ctx, name, t, action, rec, action.Scope); !ok {
			return out, isErr
		}
	case rogue:
		rec["decision"] = "rogue"
	default:
		if out, isErr, ok := a.askOperator(ctx, name, t, action, rec, ""); !ok {
			return out, isErr
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

// askOperator runs the approval prompt and reports whether to proceed. When
// scope != "", the "always" choice adds scope to the fetch allowlist (and the
// prompt says so) instead of always-approving the whole tool. On a deny or
// cancel it returns the message for the model and ok=false.
func (a *Agent) askOperator(ctx context.Context, name string, t tools.Tool, action *tools.Action, rec map[string]any, scope string) (out string, isErr, ok bool) {
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
		Tool: name, Summary: summ, Preview: action.Preview, Risk: t.Risk(), AllowScope: scope, Reply: reply,
	}})
	select {
	case d := <-reply:
		if !d.Approve {
			rec["decision"] = "denied"
			msg := "The operator denied this action."
			if d.Reason != "" {
				msg += " Their reason: " + d.Reason
			}
			msg += " Do not try to reach the same result another way; adjust your plan or ask the operator."
			return msg, true, false
		}
		rec["decision"] = "approved"
		if d.Always {
			if scope != "" {
				rec["decision"] = "approved_allowlisted"
				if a.Fetch != nil {
					a.Fetch.Add(scope)
				}
			} else {
				rec["decision"] = "approved_always"
				if !a.always[name] {
					a.always[name] = true
					a.sess.AlwaysApprove = append(a.sess.AlwaysApprove, name)
				}
			}
		}
		return "", false, true
	case <-ctx.Done():
		rec["decision"] = "cancelled"
		return "Cancelled by the operator before it ran.", true, false
	}
}

// FetchGate reports whether the fetch allowlist gate is on.
func (a *Agent) FetchGate() bool { return a.Fetch != nil && a.Fetch.GateOn() }

// SetFetchGate turns the fetch allowlist gate on or off at runtime.
func (a *Agent) SetFetchGate(on bool) {
	if a.Fetch != nil {
		a.Fetch.SetGate(on)
	}
}

// FetchAllow adds a host to the allowlist; FetchForget removes one.
func (a *Agent) FetchAllow(host string) {
	if a.Fetch != nil {
		a.Fetch.Add(host)
	}
}
func (a *Agent) FetchForget(host string) {
	if a.Fetch != nil {
		a.Fetch.Forget(host)
	}
}

// FetchAllowed lists the allowlisted hosts.
func (a *Agent) FetchAllowed() []string {
	if a.Fetch == nil {
		return nil
	}
	return a.Fetch.List()
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
		NumCtx:     a.Window(),
		PromptEval: a.sess.LastPromptTokens,
		Eval:       a.sess.LastEvalTokens,
		Used:       a.UsedTokens(),
		CompactAt:  a.CompactAt(),
	}
	if res != nil {
		u.TokPerSec = res.TokensPerSecond()
	}
	return u
}

// Undo removes the last exchange (from the operator's last message to the
// end) from the working context and returns that message.
func (a *Agent) Undo() (string, error) {
	in, err := a.UndoInput()
	return in.Text, err
}

// UndoInput is Undo returning the removed message with its images, so
// /retry can send it again as it was.
func (a *Agent) UndoInput() (Input, error) {
	ctxs := a.sess.Context
	idx := -1
	for i := len(ctxs) - 1; i >= 0; i-- {
		if ctxs[i].Kind == session.KindSummary {
			return Input{}, errors.New("the last exchange has been compacted into a summary and can't be undone")
		}
		if ctxs[i].Role == "user" && ctxs[i].Kind == "" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Input{}, errors.New("nothing to undo")
	}
	in := Input{Text: ctxs[idx].Content, Images: ctxs[idx].ImageRefs}
	a.sess.Context = ctxs[:idx]
	a.sess.LastPromptTokens = 0 // it counted what was just removed
	a.sess.Note("/undo removed the last exchange from the working context")
	a.Save()
	return in, nil
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
