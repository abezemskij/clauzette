// Package control is the front-end-neutral half of the user interface: it
// turns operator input into agent actions. It handles the ! and / commands,
// the y/N questions (cloud consent, /clear), approval answers, the message
// queue, model switches and running work in the background, and reports
// everything through a View. It prints nothing itself, so the terminal
// (package ui) and any other front end share one implementation of every
// command.
//
// A Controller is not safe for concurrent use: the front end calls it from
// one goroutine, the one that also reads Events, Done and WarmDone.
package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"clauzette/internal/agent"
	"clauzette/internal/llm"
	"clauzette/internal/session"
	"clauzette/internal/textutil"
	"clauzette/internal/tools"
)

type Options struct {
	Deps       agent.Deps
	NewSession func() (*session.Session, []string, error)
	// BuildPrompts assembles the system prompts: normal, rogue mode, chat-only.
	BuildPrompts func(date string) (agent.Prompts, []string, error)
	Notes        []string
	Resumed      bool
	// Unlock releases the first session's lock (session.Store.Lock), taken
	// by the caller; the controller moves the lock on /new, /load and
	// /clear and releases it on Quit. nil: the first session is not locked.
	Unlock func()
}

// Line is one piece of operator input.
type Line struct {
	Text    string
	Literal bool // never interpreted as a command (a """ block, a message that begins with pasted text)
}

// View is what a front end provides. The controller calls it for every
// piece of output; the front end decides how it looks.
type View interface {
	Render(ev agent.Event) // an agent event other than an approval request
	Info(msg string)
	Warn(msg string)
	Error(msg string)
	Block(text string) // a multi-line report
	Dim(line string)   // a quiet line (the last reply of a resumed session)
	Echo(text string)  // a queued message as it is sent
	ShowApproval(req *agent.ApprovalRequest, full bool)
	AskApproval(req *agent.ApprovalRequest) // ask again, after another answer or a command
	Prompt()                                // ready for input
	RefreshPrompt()                         // what the prompt shows has changed
	Working()                               // background work starts; no prompt is shown
	StopWaiting()                           // end a "waiting for…" indicator
	EndStream()                             // end streamed output
	// Command offers a command to the front end first, for commands only it
	// has (help, display settings). It reports whether it handled it.
	Command(name string, args []string, raw string) bool
}

type Controller struct {
	opt    Options
	view   View
	agent  *agent.Agent
	ctx    context.Context
	events chan agent.Event

	turnDone   chan error
	busy       bool
	busyKind   string     // "turn", "compact" or "model"
	nextModel  string     // a /model switch typed while busy, applied when the operation ends
	nextOK     string     // the cloud endpoint approved for nextModel, recorded once the switch succeeds
	clearID    string     // session id awaiting the y/N answer to /clear
	cloudModel string     // /model target awaiting the y/N answer to sending the session to a cloud provider
	cloudTurn  *cloudTurn // a message held until the operator approves the active cloud endpoint
	unlock     func()     // releases the current session's lock; nil if it holds none
	cancelBusy context.CancelFunc
	queue      []string
	pending    *agent.ApprovalRequest
	warm       bool
	warmDone   chan struct{}
}

func New(opt Options, sess *session.Session, view View) *Controller {
	c := &Controller{
		opt:      opt,
		view:     view,
		events:   make(chan agent.Event, 256),
		turnDone: make(chan error, 1),
		warmDone: make(chan struct{}),
		ctx:      context.Background(),
		unlock:   opt.Unlock,
	}
	c.agent = agent.New(opt.Deps, sess, c.events)
	return c
}

// Agent is the agent for the current session; it changes with /new, /load
// and /clear.
func (c *Controller) Agent() *agent.Agent { return c.agent }

// Events delivers the agent's events; pass each to HandleEvent.
func (c *Controller) Events() <-chan agent.Event { return c.events }

// Done delivers the result of background work; pass it to Finish.
func (c *Controller) Done() <-chan error { return c.turnDone }

// WarmDone is closed when the warm-up ends; then call Warmed. It is nil
// (never ready) afterwards.
func (c *Controller) WarmDone() <-chan struct{} { return c.warmDone }

// Busy reports background work in progress; Warm, that the model is ready.
func (c *Controller) Busy() bool { return c.busy }
func (c *Controller) Warm() bool { return c.warm }

// Start begins the warm-up (checking and loading the model) under ctx,
// which also bounds all background work. The returned function cancels the
// warm-up.
func (c *Controller) Start(ctx context.Context) context.CancelFunc {
	c.ctx = ctx
	warmCtx, cancelWarm := context.WithCancel(ctx)
	sess := c.agent.Session()
	done := c.warmDone
	go func() {
		agent.Warmup(warmCtx, c.opt.Deps, sess, func(e agent.Event) { c.events <- e })
		close(done)
	}()
	return cancelWarm
}

// Intro reports the session at startup: resumed or new, the notes from
// startup, and a model change since it was last used.
func (c *Controller) Intro() {
	s := c.agent.Session()
	c.ensureChatPrompt()
	if c.opt.Resumed {
		c.infof("resumed session %s: %d messages in context", s.ID, len(s.Context))
		c.warnModelChange(s)
		c.showLastReply(s)
	} else {
		c.infof("new session %s", s.ID)
	}
	for _, n := range c.opt.Notes {
		c.infof("%s", n)
	}
}

// Warmed is called once WarmDone fires: queued messages can go now, or the
// cloud question is asked.
func (c *Controller) Warmed() {
	c.warm = true
	c.warmDone = nil // a nil channel never fires again
	if c.askCloudStart() {
		c.view.RefreshPrompt()
	} else if !c.busy && !c.startNextQueued() {
		c.view.RefreshPrompt()
	}
}

// HandleEvent takes an approval request itself and passes every other
// event to the view.
func (c *Controller) HandleEvent(ev agent.Event) {
	if ev.Kind == agent.EvApproval {
		c.pending = ev.Approval
		c.view.ShowApproval(c.pending, false)
		return
	}
	c.view.Render(ev)
}

func (c *Controller) infof(format string, args ...any) { c.view.Info(fmt.Sprintf(format, args...)) }
func (c *Controller) warnf(format string, args ...any) { c.view.Warn(fmt.Sprintf(format, args...)) }
func (c *Controller) errorf(format string, args ...any) {
	c.view.Error(fmt.Sprintf(format, args...))
}

// Handle routes one line of operator input; it returns true when the
// program should exit.
func (c *Controller) Handle(in Line) bool {
	text := in.Text
	trimmed := strings.TrimSpace(text)
	if !in.Literal {
		switch {
		case strings.HasPrefix(trimmed, "!!"):
			text = trimmed[1:]
			trimmed = text
		case strings.HasPrefix(trimmed, "!"):
			if c.control(trimmed) {
				return true
			}
			c.afterCommand()
			return false
		}
	}
	// A y/N question takes the next line as its answer. A / command typed
	// instead counts as "no" and then runs, so it is never lost.
	command := !in.Literal && strings.HasPrefix(trimmed, "/")
	yes := !in.Literal && (strings.EqualFold(trimmed, "y") || strings.EqualFold(trimmed, "yes"))
	because := ""
	if command {
		because = " (you typed a command)"
	}
	if ct := c.cloudTurn; ct != nil {
		c.cloudTurn = nil
		switch {
		case yes && ct.in.Text == "" && !ct.retry: // asked at the start of the session
			c.agent.ApproveCloud(ct.endpoint)
			c.infof("approved %s for this session", ct.endpoint)
			if c.busy || !c.startNextQueued() {
				c.view.Prompt()
			}
			return false
		case yes:
			c.agent.ApproveCloud(ct.endpoint)
			c.sendTurn(ct.in, ct.retry)
			return false
		}
		c.infof("nothing was sent%s; you will be asked again with your next message, or /model <local profile> switches to a local model", because)
		if ct.in.Text != "" && !ct.retry {
			c.infof("your message: %s", textutil.OneLine(ct.in.Text, 80))
		}
		if n := len(c.queue); n > 0 {
			c.infof("dropped %d queued message(s) as well", n)
			c.queue = nil
		}
		if !command {
			c.view.Prompt()
			return false
		}
	}
	if c.cloudModel != "" {
		name := c.cloudModel
		c.cloudModel = ""
		if yes {
			_, endpoint := c.agent.CloudTarget(name)
			c.switchToApproved(name, endpoint)
		} else {
			c.infof("not switched%s", because)
		}
		if !command {
			if !c.busy {
				c.view.Prompt()
			}
			return false
		}
	}
	if c.clearID != "" {
		id := c.clearID
		c.clearID = ""
		if yes && c.agent.Session().ID == id {
			c.clearSession()
		} else {
			c.infof("kept the session%s", because)
		}
		if !command {
			if !c.busy {
				c.view.Prompt()
			}
			return false
		}
	}
	if c.pending != nil {
		c.answerApproval(trimmed, text)
		return false
	}
	if trimmed == "" {
		if !c.busy {
			c.view.Prompt()
		}
		return false
	}
	if !in.Literal && strings.HasPrefix(trimmed, "/") {
		// /rogue off is the one slash command that works mid-task: it is
		// how the operator takes back control without cancelling.
		if c.busy && strings.EqualFold(strings.Join(strings.Fields(trimmed), " "), "/rogue off") {
			c.rogue([]string{"off"})
			return false
		}
		// /model works mid-task too: the list is read-only, and a switch is
		// queued until the current operation ends.
		if f := strings.Fields(trimmed); c.busy && len(f) > 0 && strings.EqualFold(f[0], "/model") {
			c.model(f[1:])
			return false
		}
		if c.busy {
			c.warnf("/ commands work while the agent is idle; use ! commands while it works (!help)")
			return false
		}
		if c.slash(trimmed) {
			return true
		}
		if !c.busy {
			c.view.Prompt()
		}
		return false
	}
	if c.busy || !c.warm {
		c.queue = append(c.queue, text)
		if c.busy {
			c.infof("queued; it will be sent when the agent finishes (!c cancels the current work and the queue)")
		} else {
			c.infof("queued; it will be sent once the model is ready")
		}
		return false
	}
	c.startTurn(agent.Input{Text: text})
	return false
}

func (c *Controller) afterCommand() {
	switch {
	case c.pending != nil:
		c.view.AskApproval(c.pending)
	case !c.busy:
		c.view.Prompt()
	}
}

// control handles ! commands, which work at any time.
func (c *Controller) control(cmd string) bool {
	f := strings.Fields(cmd)
	name := strings.ToLower(f[0])
	arg := ""
	if len(f) > 1 {
		arg = strings.ToLower(f[1])
	}
	if c.view.Command(name, f[1:], cmd) {
		return false
	}
	switch name {
	case "!c":
		if !c.busy {
			c.infof("nothing to cancel")
			return false
		}
		dropped := len(c.queue)
		c.queue = nil
		c.pending = nil
		c.cancelBusy()
		msg := "cancelling…"
		if dropped > 0 {
			msg += fmt.Sprintf(" (%d queued message%s discarded)", dropped, plural(dropped))
		}
		c.warnf("%s", msg)
	case "!q":
		c.Quit()
		return true
	case "!think":
		v, ok := ParseToggle(arg, c.agent.Think())
		if !ok {
			c.warnf("usage: !think [on|off]")
			return false
		}
		if !c.agent.ThinkAvailable() {
			c.warnf("%s has no thinking mode; !think has no effect", c.agent.Model().Model)
			return false
		}
		c.agent.SetThink(v)
		when := ""
		if c.busy {
			when = " (applies from the next model request)"
		} else {
			c.agent.Save()
		}
		c.infof("thinking %s%s", onOff(v), when)
	case "!show":
		v, ok := ParseToggle(arg, c.agent.Show())
		if !ok {
			c.warnf("usage: !show [on|off]")
			return false
		}
		c.agent.SetShow(v)
		if !c.busy {
			c.agent.Save()
		}
		c.infof("thinking text %s", map[bool]string{true: "shown", false: "hidden"}[v])
	case "!safe":
		v, ok := ParseToggle(arg, c.agent.Safe())
		if !ok {
			c.warnf("usage: !safe [on|off]")
			return false
		}
		c.agent.SetSafe(v)
		if !c.busy {
			c.agent.Save()
		}
		if v {
			c.infof("safe mode on: mv_file and delete_file back up their target first, and each one needs a fresh 'y'")
		} else {
			c.infof("safe mode off: destructive actions behave like other write actions")
		}
	case "!plan":
		v, ok := ParseToggle(arg, c.agent.Plan())
		if !ok {
			c.warnf("usage: !plan [on|off]")
			return false
		}
		if v && c.agent.ChatOnly() {
			c.warnf("plan mode needs a model with tool support; %s has none", c.agent.Model().Model)
			return false
		}
		wasRogue := c.agent.Rogue()
		if c.agent.SetPlan(v) && !c.busy {
			c.agent.Save()
		}
		if wasRogue && !c.agent.Rogue() {
			c.warnf("rogue mode switched off: it cannot be combined with plan mode")
		}
		if v {
			c.infof("plan mode on: write/exec/destructive actions are captured, not executed; /plan lists them, /plan approve [n|all] [comment] runs them")
		} else {
			c.infof("plan mode off: actions run as usual (with approval); %d pending action%s stay in the list", c.agent.PlanPending(), plural(c.agent.PlanPending()))
		}
	case "!skip":
		if c.opt.Deps.Guard == nil {
			c.infof("the GPU guard is disabled")
		} else {
			c.opt.Deps.Guard.Skip()
			c.infof("skipping the cool-down pause, if one is active")
		}
	default:
		c.warnf("unknown command %s; type !help for the list, or start a message with !! to send a literal '!'", name)
	}
	return false
}

// slash handles / commands, which work only while the agent is idle.
func (c *Controller) slash(cmd string) bool {
	f := strings.Fields(cmd)
	name := strings.ToLower(f[0])
	args := f[1:]
	if c.view.Command(name, args, cmd) {
		return false
	}
	switch name {
	case "/exit", "/quit":
		c.Quit()
		return true
	case "/context":
		c.view.Block(c.agent.ContextReport())
	case "/compact":
		// /compact <percent> summarises that share of the oldest conversation once
		ratio := 0.0
		if len(args) > 0 {
			p, err := strconv.Atoi(strings.TrimSuffix(args[0], "%"))
			if err != nil || p < 10 || p > 90 || len(args) > 1 {
				c.warnf("usage: /compact [percent]  (10-90: the share of the oldest conversation to summarise)")
				return false
			}
			ratio = 1 - float64(p)/100
		}
		c.runBusy("compact", func(ctx context.Context) error { return c.agent.Compact(ctx, ratio) })
	case "/sessions":
		c.listSessions()
	case "/new":
		c.agent.Save()
		s, notes, err := c.opt.NewSession()
		if err != nil {
			c.errorf("%v", err)
			return false
		}
		c.switchSession(s, notes)
	case "/clear":
		s := c.agent.Session()
		msgs := 0
		for _, e := range s.Transcript {
			if e.Kind != session.KindNote {
				msgs++
			}
		}
		if msgs == 0 {
			c.infof("this session has no messages yet; nothing to clear")
			return false
		}
		c.clearID = s.ID
		c.warnf("/clear discards this session's %d-message conversation and starts a new one; files you worked on and safe-mode backups are kept. Clear it? [y/N]", msgs)
	case "/load":
		if len(args) == 0 {
			c.warnf("usage: /load <session id> (see /sessions)")
			return false
		}
		c.agent.Save()
		s, err := c.opt.Deps.Store.Load(args[0])
		if err != nil {
			c.errorf("%v", err)
			return false
		}
		c.switchSession(s, nil)
	case "/undo":
		if _, err := c.agent.Undo(); err != nil {
			c.warnf("%v", err)
			return false
		}
		c.infof("removed the last exchange from the context. Files changed by tools are not restored. The next request re-reads the conversation.")
	case "/retry":
		if e := c.agent.CloudApprovalNeeded(); e != "" {
			c.askCloudTurn(&cloudTurn{endpoint: e, retry: true})
			return false
		}
		in, err := c.agent.UndoInput()
		if err != nil {
			c.warnf("%v", err)
			return false
		}
		c.infof("resending: %s", textutil.OneLine(in.Text, 80))
		c.startTurn(in)
	case "/image":
		c.image(strings.TrimSpace(cmd[len(f[0]):]))
	case "/system":
		c.warnf("/system is now /system-prompt [reload]")
	case "/system-prompt":
		if len(args) > 0 && strings.EqualFold(args[0], "reload") {
			s := c.agent.Session()
			p, notes, err := c.opt.BuildPrompts(s.Date)
			if err != nil {
				c.errorf("%v", err)
				return false
			}
			c.agent.SetSystems(p)
			c.agent.Save()
			for _, n := range notes {
				c.infof("%s", n)
			}
			c.infof("system prompts rebuilt (normal %d, rogue mode %d, chat-only %d characters); the next request re-reads the whole conversation", len(p.Normal), len(p.Rogue), len(p.Chat))
		} else {
			s := c.agent.Session()
			if c.agent.ChatOnly() {
				c.infof("the system prompt for a model without tools (in use now):")
				if s.SystemChat != "" {
					c.view.Block(s.SystemChat)
				} else {
					c.view.Block(s.System)
				}
			} else if c.agent.Rogue() {
				c.infof("the system prompt for rogue mode (in use now):")
				if s.SystemRogue != "" {
					c.view.Block(s.SystemRogue)
				} else {
					c.view.Block(s.System)
				}
			} else {
				c.infof("the system prompt (rogue mode uses its own; /rogue on, then /system-prompt shows it):")
				c.view.Block(s.System)
			}
		}
	case "/model":
		c.model(args)
	case "/adapt":
		c.adapt(args)
	case "/gpu":
		c.view.Block(c.opt.Deps.Guard.Report())
	case "/approvals":
		if len(args) > 0 && strings.EqualFold(args[0], "reset") {
			c.agent.ResetApprovals()
			c.infof("session approvals reset; only tools.auto_approve from the config still run without asking")
		} else if list := c.agent.AutoApproved(); len(list) == 0 {
			c.infof("every write and exec action asks for approval")
		} else {
			c.infof("running without approval: %s (/approvals reset to undo this session's choices)", strings.Join(list, ", "))
		}
	case "/safe":
		if len(args) > 0 && strings.EqualFold(args[0], "restore") {
			force := len(args) == 3 && strings.EqualFold(args[2], "force")
			if len(args) != 2 && !force {
				c.warnf("usage: /safe restore <number> [force] (see /safe)")
				return false
			}
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 1 {
				c.warnf("the backup number must be a positive integer")
				return false
			}
			out, err := c.agent.RestoreBackup(n, force)
			if err != nil {
				c.errorf("%v", err)
				return false
			}
			c.infof("%s", out)
			return false
		}
		c.view.Block(c.agent.SafeReport())
	case "/rogue":
		c.rogue(args)
	case "/fetch":
		c.fetch(args)
	case "/plan":
		if len(args) > 0 && (strings.EqualFold(args[0], "approve") || strings.EqualFold(args[0], "reject")) {
			approve := strings.EqualFold(args[0], "approve")
			// the comment is taken from the raw line, so its spacing is kept
			_, rest := nextField(cmd)
			_, rest = nextField(rest)
			p, err := parsePlanArgs(rest, c.agent.PlanPending(), approve)
			if err != nil {
				c.warnf("%v", err)
				return false
			}
			c.runBusy("plan", func(ctx context.Context) error {
				var report, cont string
				var err error
				if approve {
					report, cont, err = c.agent.PlanApprove(ctx, p.nums, agent.PlanApproveOptions{Comment: p.comment, NoContinue: p.noContinue})
				} else {
					report, cont, err = c.agent.PlanReject(p.nums, p.comment)
				}
				if err != nil {
					return err
				}
				c.infof("%s", report)
				if cont != "" {
					return c.agent.RunHarnessTurn(ctx, cont)
				}
				return nil
			})
			return false
		}
		c.view.Block(c.agent.PlanReport())
	default:
		c.warnf("unknown command %s; type /help", name)
	}
	return false
}

// rogue handles /rogue [on [steps]|off].
// fetch controls the fetch_url allowlist: /fetch shows it, /fetch on|off
// toggles the gate, /fetch allow|forget <host> edits the list. When the gate
// is on, fetch_url to a listed host runs without asking (even in rogue mode)
// and an unlisted host is always asked about.
func (c *Controller) fetch(args []string) {
	a := c.agent
	show := func() {
		state := "off"
		if a.FetchGate() {
			state = "on"
		}
		c.infof("fetch allowlist gate: %s", state)
		if hosts := a.FetchAllowed(); len(hosts) > 0 {
			c.infof("allowed hosts: %s", strings.Join(hosts, ", "))
		} else {
			c.infof("allowed hosts: (none yet; approve a fetch with 'a', or /fetch allow <host>)")
		}
	}
	if len(args) == 0 {
		show()
		return
	}
	switch strings.ToLower(args[0]) {
	case "on":
		a.SetFetchGate(true)
		c.infof("fetch allowlist on: fetch_url to an unlisted host now asks for approval, even in rogue mode")
	case "off":
		a.SetFetchGate(false)
		c.infof("fetch allowlist off: fetch_url follows the normal rules (manual approves, rogue runs)")
	case "allow":
		if len(args) != 2 {
			c.warnf("usage: /fetch allow <host>")
			return
		}
		a.FetchAllow(args[1])
		a.Save()
		c.infof("added %s to the fetch allowlist", args[1])
	case "forget":
		if len(args) != 2 {
			c.warnf("usage: /fetch forget <host>")
			return
		}
		a.FetchForget(args[1])
		a.Save()
		c.infof("removed %s from the fetch allowlist", args[1])
	default:
		c.warnf("usage: /fetch [on|off|allow <host>|forget <host>]")
	}
}

func (c *Controller) rogue(args []string) {
	a := c.agent
	if len(args) == 0 {
		if !a.Rogue() {
			c.infof("rogue mode is off. /rogue on [steps] runs every action without approval until the model finishes each task")
			return
		}
		c.infof("rogue mode is on: %s", c.rogueLimits())
		return
	}
	switch strings.ToLower(args[0]) {
	case "on":
		steps := 0
		if len(args) > 2 {
			c.warnf("usage: /rogue on [steps]")
			return
		}
		if len(args) == 2 {
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 1 {
				c.warnf("the step budget must be a positive number")
				return
			}
			steps = n
		}
		if !a.HasRogueSystem() {
			// a session saved before rogue mode had its own system prompt
			s := a.Session()
			p, _, err := c.opt.BuildPrompts(s.Date)
			if err != nil {
				c.errorf("could not build the rogue-mode system prompt: %v", err)
				return
			}
			a.SetSystems(agent.Prompts{Normal: s.System, Rogue: p.Rogue, Chat: s.SystemChat}) // the other prompts stay as they were frozen
		}
		if _, err := a.SetRogue(true, steps); err != nil {
			c.warnf("%v", err)
			return
		}
		a.Save()
		var unattended []string
		for _, n := range c.opt.Deps.Registry.Names() {
			if t, ok := c.opt.Deps.Registry.Get(n); ok && t.Risk() != tools.ReadOnly {
				unattended = append(unattended, n)
			}
		}
		c.warnf("ROGUE MODE ON: %s now run without approval", strings.Join(unattended, ", "))
		c.warnf("per message: %s; mv/delete are always backed up first (/safe restore)", c.rogueLimits())
		if ws := c.opt.Deps.WS; ws != nil && ws.HasAgentDir() {
			c.warnf("outside %s the file tools have %s access; exec_command is not confined", ws.Root, ws.Access)
		}
		c.warnf("stop the current task with !c, take back approvals with /rogue off (works mid-task)")
		if !a.Think() {
			c.infof("thinking is off; the rogue-mode prompt asks the model to think before acting (!think on)")
		}
	case "off":
		if changed, _ := a.SetRogue(false, 0); !changed {
			c.infof("rogue mode is already off")
			return
		}
		if !c.busy {
			a.Save()
		}
		c.infof("rogue mode off: actions need approval again from the next one")
	default:
		c.warnf("usage: /rogue [on [steps]|off]")
	}
}

func (c *Controller) rogueLimits() string {
	s := fmt.Sprintf("up to %d steps", c.agent.RogueBudget())
	if m := c.opt.Deps.Cfg.Rogue.MaxMinutes; m > 0 {
		s += fmt.Sprintf(" and %d minutes", m)
	}
	return s + ", until the model calls finish"
}

// clearSession discards the current session and starts a new one in its
// place, so a throwaway conversation does not linger in /sessions. It
// removes only the session's own record: files the agent worked on, the
// session's safe-mode backups and the audit log are left as they are.
func (c *Controller) clearSession() {
	old := c.agent.Session()
	store := c.opt.Deps.Store
	s, notes, err := c.opt.NewSession()
	if err != nil {
		c.errorf("could not start a new session, so the old one was kept: %v", err)
		return
	}
	if err := store.Delete(old.ID); err != nil {
		c.warnf("started a new session, but the old one could not be removed from %s: %v", store.Dir, err)
	}
	if c.opt.Deps.Audit != nil {
		c.opt.Deps.Audit.Log(map[string]any{"session": old.ID, "event": "cleared"})
	}
	c.switchSession(s, notes)
	c.infof("cleared the previous session (%s)", old.ID)
}

func (c *Controller) switchSession(s *session.Session, notes []string) {
	if st := c.opt.Deps.Store; st != nil && s.ID != c.agent.Session().ID { // reloading the current session keeps its lock
		unlock, err := st.Lock(s.ID)
		if err != nil {
			c.errorf("%v", err)
			return
		}
		c.releaseLock()
		c.unlock = unlock
	}
	c.agent = agent.New(c.opt.Deps, s, c.events)
	c.infof("session %s: %d messages in context", s.ID, len(s.Context))
	for _, n := range notes {
		c.infof("%s", n)
	}
	c.warnModelChange(s)
	c.ensureChatPrompt()
	c.askCloudStart()
	c.showLastReply(s)
}

// warnModelChange says when a session continues with a different model
// than it last used (/load keeps the model that is loaded now), and how to
// switch back.
func (c *Controller) warnModelChange(s *session.Session) {
	m := c.agent.Model().Model
	if s.Model == "" || s.Model == m {
		return
	}
	c.warnf("this session last used %s; continuing with %s", s.Model, m)
	back := s.Profile
	if back == "" {
		back = s.Model
	}
	c.infof("/model %s switches back", back)
}

// ensureChatPrompt adds the chat-only system prompt to a session saved
// before it existed, so a model without tools never gets the prompt that
// describes tools. The normal and rogue-mode prompts stay as they were
// frozen.
func (c *Controller) ensureChatPrompt() {
	a := c.agent
	if a.HasChatSystem() || c.opt.BuildPrompts == nil {
		return
	}
	s := a.Session()
	p, _, err := c.opt.BuildPrompts(s.Date)
	if err != nil {
		c.warnf("could not build the chat-only system prompt: %v", err)
		return
	}
	a.SetSystems(agent.Prompts{Normal: s.System, Rogue: s.SystemRogue, Chat: p.Chat})
	a.Save()
}

func (c *Controller) listSessions() {
	list, err := c.opt.Deps.Store.List()
	if err != nil {
		c.errorf("%v", err)
		return
	}
	if len(list) == 0 {
		c.infof("no saved sessions")
		return
	}
	current := c.agent.Session().ID
	var sb strings.Builder
	for i, s := range list {
		if i == 20 {
			fmt.Fprintf(&sb, "… and %d more\n", len(list)-20)
			break
		}
		mark := " "
		if s.ID == current {
			mark = "*"
		}
		fmt.Fprintf(&sb, "%s %s  %s  %4d entries  %s\n", mark, s.ID, s.Updated.Format("2006-01-02 15:04"), s.Messages, textutil.Truncate(s.Title, 50))
	}
	c.view.Block(strings.TrimRight(sb.String(), "\n"))
}

func (c *Controller) askCloudTurn(ct *cloudTurn) {
	c.cloudTurn = ct
	c.warnf("the active model runs on %s: this conversation, including the file contents and command output in it, is sent there. Send? [y/N]", ct.endpoint)
}

// askCloudStart asks up front when the session starts on a cloud endpoint
// it has not approved (-profile, default_profile, -resume, /new, /clear,
// /load), instead of waiting for the first message. It reports whether it
// asked.
func (c *Controller) askCloudStart() bool {
	if c.cloudTurn != nil || !c.warm {
		return false
	}
	if e := c.agent.CloudApprovalNeeded(); e != "" {
		c.askCloudTurn(&cloudTurn{endpoint: e})
		return true
	}
	return false
}

// startTurn sends a message, after asking if the active model is a cloud
// one this session has not approved yet.
func (c *Controller) startTurn(in agent.Input) {
	if e := c.agent.CloudApprovalNeeded(); e != "" {
		c.askCloudTurn(&cloudTurn{endpoint: e, in: in})
		return
	}
	c.runBusy("turn", func(ctx context.Context) error { return c.agent.RunInput(ctx, in) })
}

// sendTurn runs an approved held message, or the held /retry.
func (c *Controller) sendTurn(in agent.Input, retry bool) {
	if retry {
		t, err := c.agent.UndoInput()
		if err != nil {
			c.warnf("%v", err)
			c.view.Prompt()
			return
		}
		c.infof("resending: %s", textutil.OneLine(t.Text, 80))
		in = t
	}
	c.startTurn(in)
}

// image handles /image <path> [question]: it attaches an image file to a
// message. A relative path is taken from the workspace root; the operator
// may also name a file outside it.
func (c *Controller) image(rest string) {
	p, question, err := splitPath(rest)
	if err != nil || p == "" {
		c.warnf("usage: /image <path> [question]  (quote a path with spaces)")
		return
	}
	if !c.warm {
		c.warnf("the model is still loading; send the image once it is ready")
		return
	}
	if !c.agent.Vision() {
		c.warnf("%s cannot see images; switch to a model that can (/model shows vision), or set \"caps\": {\"vision\": true} in its profile if Ollama does not report it", c.agent.Model().Model)
		return
	}
	abs := p
	if strings.HasPrefix(p, "~/") {
		if home, herr := os.UserHomeDir(); herr == nil {
			abs = filepath.Join(home, p[2:])
		}
	}
	if !filepath.IsAbs(abs) {
		root := "."
		if ws := c.opt.Deps.WS; ws != nil {
			root = ws.Root
		}
		abs = filepath.Join(root, abs)
	}
	img, err := c.agent.AddImageFile(abs, p)
	if err != nil {
		c.warnf("%v", err)
		return
	}
	if question == "" {
		question = "Describe this image."
	}
	imgs := []llm.Image{img}
	c.infof("attached %s (%s)", p, tools.DescribeImage(img))
	c.startTurn(agent.Input{Text: question + "\n\n" + agent.ImageNote(imgs), Images: imgs})
}

func (c *Controller) runBusy(kind string, fn func(context.Context) error) {
	ctx, cancel := context.WithCancel(c.ctx)
	c.view.Working()
	c.busy, c.busyKind, c.cancelBusy = true, kind, cancel
	go func() {
		err := fn(ctx)
		cancel()
		c.turnDone <- err
	}()
}

func (c *Controller) Finish(err error) {
	c.drainEvents() // the agent sent all its events before finishing
	c.view.StopWaiting()
	c.view.EndStream()
	kind := c.busyKind
	c.busy, c.busyKind, c.cancelBusy, c.pending = false, "", nil, nil
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		c.warnf("cancelled")
	default:
		c.errorf("%v", err)
		if kind == "turn" {
			c.infof("/retry resends your last message")
		}
	}
	if name := c.nextModel; name != "" {
		ok := c.nextOK
		c.nextModel, c.nextOK = "", ""
		if errors.Is(err, context.Canceled) {
			c.infof("the switch to %s was dropped with the cancelled operation", name)
		} else {
			c.startSwitch(name, ok) // queued messages wait until the switch is done
			return
		}
	}
	if !c.startNextQueued() {
		c.view.Prompt()
	}
}

// model lists the profiles and models, or switches to one. A switch typed
// while the agent works is applied when the current operation ends.
func (c *Controller) model(args []string) {
	if len(args) == 0 {
		ctx, cancel := context.WithTimeout(c.ctx, 20*time.Second)
		defer cancel()
		c.view.Block(c.agent.ModelList(ctx))
		return
	}
	if strings.EqualFold(args[0], "--online") {
		all, filter := false, ""
		for _, a := range args[1:] {
			if a == "--all" {
				all = true
			} else {
				filter = a
			}
		}
		ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
		defer cancel()
		c.view.Block(c.agent.OnlineList(ctx, filter, all))
		return
	}
	if len(args) > 1 {
		c.warnf("usage: /model [profile or model | --online [filter] [--all]]")
		return
	}
	name := args[0]
	if _, endpoint := c.agent.CloudTarget(name); endpoint != "" && !c.agent.CloudApproved(endpoint) && c.warm && !c.agent.Rogue() {
		c.cloudModel = name
		c.warnf("%s runs on %s: from now on this conversation, including the file contents and command output in it, is sent there. Switch? [y/N]", name, endpoint)
		return
	}
	c.switchTo(name)
}

// switchTo switches models now, or when the current operation ends.
func (c *Controller) switchTo(name string) { c.switchToApproved(name, "") }

// switchToApproved is switchTo with the cloud endpoint the operator just
// approved; the approval is recorded only if the switch succeeds.
func (c *Controller) switchToApproved(name, endpoint string) {
	switch {
	case !c.warm:
		c.warnf("the model is still loading; switch once it is ready")
	case c.agent.Rogue():
		c.warnf("rogue mode stays on the model it started with; turn it off first (/rogue off)")
	case c.busy:
		c.nextModel, c.nextOK = name, endpoint
		c.infof("switching to %s when the current operation ends", name)
	default:
		c.startSwitch(name, endpoint)
	}
}

// adapt lists adapted models, measures one, or pins or forgets a value.
func (c *Controller) adapt(args []string) {
	if len(args) == 0 {
		c.view.Block(c.agent.AdaptReport())
		return
	}
	name, rest := args[0], args[1:]
	ctx, cancel := context.WithTimeout(c.ctx, 20*time.Second)
	defer cancel()
	switch {
	case len(rest) == 2 && strings.EqualFold(rest[0], "set"):
		n, err := strconv.Atoi(rest[1])
		if err != nil {
			c.warnf("usage: /adapt <name> set <tokens>")
			return
		}
		if err := c.agent.AdaptSet(ctx, name, n); err != nil {
			c.warnf("%v", err)
		}
	case len(rest) == 1 && strings.EqualFold(rest[0], "forget"):
		if err := c.agent.AdaptForget(ctx, name); err != nil {
			c.warnf("%v", err)
		}
	case len(rest) == 0 || (len(rest) == 1 && rest[0] == "--verify"):
		switch {
		case !c.warm:
			c.warnf("the model is still loading; run /adapt once it is ready")
		case c.agent.Rogue():
			c.warnf("not in rogue mode: /adapt takes the GPU away from the model for minutes")
		default:
			verify := len(rest) == 1
			c.runBusy("adapt", func(ctx context.Context) error {
				return c.agent.AdaptModel(ctx, name, verify, c.opt.BuildPrompts)
			})
		}
	default:
		c.warnf("usage: /adapt [<name> [--verify] | <name> set <tokens> | <name> forget]")
	}
}

// startSwitch runs a switch; approved is the cloud endpoint the operator
// agreed to, recorded only if the switch lands on it.
func (c *Controller) startSwitch(name, approved string) {
	c.runBusy("model", func(ctx context.Context) error {
		err := c.agent.SwitchModel(ctx, name, c.opt.BuildPrompts)
		if err == nil && approved != "" && c.agent.Models.Active().Endpoint == approved {
			c.agent.ApproveCloud(approved)
		}
		return err
	})
}

func (c *Controller) startNextQueued() bool {
	if len(c.queue) == 0 || !c.warm || c.busy {
		return false
	}
	text := c.queue[0]
	c.queue = c.queue[1:]
	c.view.Echo(text)
	c.startTurn(agent.Input{Text: text})
	return true
}

func (c *Controller) drainEvents() {
	for {
		select {
		case ev := <-c.events:
			c.HandleEvent(ev)
		default:
			return
		}
	}
}

// Quit stops any running work, saves the session and says how to resume.
func (c *Controller) Quit() {
	if c.busy {
		c.cancelBusy()
		c.infof("stopping the current work…")
		timeout := time.After(20 * time.Second)
	wait:
		for {
			select {
			case ev := <-c.events:
				c.HandleEvent(ev)
			case <-c.turnDone:
				c.busy = false
				break wait
			case <-timeout:
				c.warnf("the agent did not stop in time; saving anyway")
				break wait
			}
		}
	}
	c.view.StopWaiting()
	c.drainEvents()
	c.view.EndStream()
	c.agent.Save()
	c.releaseLock()
	c.drainEvents()
	id := c.agent.Session().ID
	c.infof("session %s saved; resume with: clauzette -session %s (or clauzette -resume)", id, id)
}

func (c *Controller) answerApproval(trimmed, raw string) {
	a := c.pending
	lower := strings.ToLower(trimmed)
	reply := func(d agent.Decision) {
		a.Reply <- d
		c.pending = nil
	}
	switch {
	case lower == "y" || lower == "yes":
		reply(agent.Decision{Approve: true})
	case lower == "a" || lower == "always":
		reply(agent.Decision{Approve: true, Always: true})
		if a.AllowScope != "" {
			c.infof("%s added to the fetch allowlist; fetches to it now run without asking (/fetch forget %s to undo)", a.AllowScope, a.AllowScope)
		} else {
			c.infof("%s will run without asking for the rest of this session (/approvals reset to undo)", a.Tool)
		}
	case lower == "n" || lower == "no":
		reply(agent.Decision{Approve: false})
	case strings.HasPrefix(lower, "n ") || strings.HasPrefix(lower, "no "):
		reason := strings.TrimSpace(raw)
		reason = strings.TrimSpace(reason[strings.IndexByte(reason, ' ')+1:])
		reply(agent.Decision{Approve: false, Reason: reason})
	case lower == "v" || lower == "view":
		c.view.ShowApproval(c.pending, true)
	default:
		c.warnf("answer y, n (optionally followed by a reason), a or v; or !c to stop the whole turn")
		c.view.AskApproval(c.pending)
	}
}

func (c *Controller) showLastReply(s *session.Session) {
	for i := len(s.Context) - 1; i >= 0; i-- {
		e := s.Context[i]
		if e.Role == "assistant" && strings.TrimSpace(e.Content) != "" {
			c.view.Dim("last reply: " + textutil.Truncate(strings.TrimSpace(e.Content), 400))
			return
		}
	}
}

// cloudTurn is a message (or a /retry) waiting for the operator to approve
// sending the session to the active cloud endpoint.
type cloudTurn struct {
	endpoint string
	in       agent.Input
	retry    bool
}

// planArgs is the parsed tail of "/plan approve" or "/plan reject".
type planArgs struct {
	nums       []int  // nil selects all
	comment    string // the comment (approve) or reason (reject), as typed
	noContinue bool
}

// splitPath splits a path, optionally in single or double quotes, from the
// rest of s.
func splitPath(s string) (path, rest string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", nil
	}
	if q := s[0]; q == '"' || q == '\'' {
		end := strings.IndexByte(s[1:], q)
		if end < 0 {
			return "", "", errors.New("unclosed quote")
		}
		return s[1 : 1+end], strings.TrimSpace(s[2+end:]), nil
	}
	path, rest = nextField(s)
	return path, strings.TrimSpace(rest), nil
}

// ParseToggle reads an on/off argument; an empty one toggles current. ok
// is false for anything else.
func ParseToggle(arg string, current bool) (bool, bool) {
	switch arg {
	case "":
		return !current, true
	case "on", "true", "1", "yes":
		return true, true
	case "off", "false", "0", "no":
		return false, true
	}
	return false, false
}

// parsePlanArgs parses "<n ...|all> [--no-continue] [--] [comment]", the
// raw text after "/plan approve" or "/plan reject". The selection is the
// leading numbers or "all"; the comment is everything after it, spacing
// kept. "--" ends the selection, for a comment that starts with a number.
// A bare command selects all; a comment without a selection is refused,
// because "/plan approve looks good" may not mean all of them.
func parsePlanArgs(rest string, total int, allowNoContinue bool) (planArgs, error) {
	var p planArgs
	all := false
selection:
	for {
		tok, after := nextField(rest)
		switch {
		case tok == "":
			break selection
		case tok == "--":
			rest = after
			break selection
		case allowNoContinue && tok == "--no-continue":
			p.noContinue = true
		case strings.EqualFold(tok, "all") && !all && len(p.nums) == 0:
			all = true
		default:
			n, err := strconv.Atoi(tok)
			if err != nil || all {
				break selection
			}
			if n < 1 || n > total {
				return p, fmt.Errorf("no pending action %d; this session has %d (see /plan)", n, total)
			}
			p.nums = append(p.nums, n)
		}
		rest = after
	}
	p.comment = strings.TrimSpace(rest)
	if total == 0 {
		return p, errors.New("no pending actions; /plan shows the state")
	}
	if !all && len(p.nums) == 0 && p.comment != "" {
		return p, fmt.Errorf("say which actions the comment goes with: /plan <approve|reject> all %s, or numbers from /plan (start a comment that begins with a number with --)",
			textutil.OneLine(p.comment, 40))
	}
	return p, nil
}

// nextField splits off the first space-separated word of s and returns it
// with the unchanged rest.
func nextField(s string) (string, string) {
	s = strings.TrimLeft(s, " \t")
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// releaseLock gives up the current session's lock.
func (c *Controller) releaseLock() {
	if c.unlock != nil {
		c.unlock()
		c.unlock = nil
	}
}
