// Package ui is the terminal front end. One goroutine reads input lines,
// the agent runs in another, and the main loop here multiplexes input,
// agent events, timers and signals, so the terminal is only ever written
// from one place.
package ui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"clauzette/internal/agent"
	"clauzette/internal/config"
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
}

type inputLine struct {
	text    string
	literal bool // came from a """ block: never interpreted as a command
}

const (
	streamNone = iota
	streamThinking
	streamThinkingHidden
	streamText
)

const (
	cReset  = "\x1b[0m"
	cBold   = "\x1b[1m"
	cDim    = "\x1b[2m"
	cRed    = "\x1b[31m"
	cGreen  = "\x1b[32m"
	cYellow = "\x1b[33m"
	cCyan   = "\x1b[36m"
)

// Bracketed paste. When enabled (pasteOn, emitted in Run on a terminal), the
// terminal wraps pasted text in pasteStart..pasteEnd, letting readInput take a
// multi-line paste as one message instead of one message per line.
const (
	pasteOn    = "\x1b[?2004h"
	pasteOff   = "\x1b[?2004l"
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

type UI struct {
	opt    Options
	color  bool
	tty    bool
	agent  *agent.Agent
	ctx    context.Context
	events chan agent.Event
	lines  chan inputLine

	turnDone   chan error
	busy       bool
	busyKind   string     // "turn", "compact" or "model"
	nextModel  string     // a /model switch typed while busy, applied when the operation ends
	nextOK     string     // the cloud endpoint approved for nextModel, recorded once the switch succeeds
	clearID    string     // session id awaiting the y/N answer to /clear
	cloudModel string     // /model target awaiting the y/N answer to sending the session to a cloud provider
	cloudTurn  *cloudTurn // a message held until the operator approves the active cloud endpoint
	cancelBusy context.CancelFunc
	queue      []string
	pending    *agent.ApprovalRequest
	warm       bool
	warmDone   chan struct{}
	atPrompt   bool
	stream     int

	waitText  string
	waitSince time.Time
	waitTick  *time.Ticker
	waitC     <-chan time.Time
}

func New(opt Options, sess *session.Session) *UI {
	u := &UI{
		opt:      opt,
		events:   make(chan agent.Event, 256),
		lines:    make(chan inputLine),
		turnDone: make(chan error, 1),
		warmDone: make(chan struct{}),
		tty:      isTerminal(os.Stdout),
	}
	switch opt.Deps.Cfg.Color {
	case "always":
		u.color = true
	case "never":
		u.color = false
	default:
		u.color = u.tty && os.Getenv("NO_COLOR") == ""
	}
	u.agent = agent.New(opt.Deps, sess, u.events)
	return u
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Run is the main loop. It returns when the operator quits.
func (u *UI) Run(ctx context.Context) error {
	u.ctx = ctx
	// Turn on bracketed paste so a multi-line paste arrives as one message; the
	// defer restores the terminal on every exit path. Only on a real terminal
	// (both streams), never when stdin/stdout is piped.
	if u.tty && isTerminal(os.Stdin) {
		u.write(pasteOn)
		defer u.write(pasteOff)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	go u.readInput()
	u.banner()

	warmCtx, cancelWarm := context.WithCancel(ctx)
	defer cancelWarm()
	sess := u.agent.Session()
	go func() {
		agent.Warmup(warmCtx, u.opt.Deps, sess, func(e agent.Event) { u.events <- e })
		close(u.warmDone)
	}()

	u.promptLine()
	for {
		select {
		case in, ok := <-u.lines:
			if !ok {
				u.atPrompt = false
				u.write("\n")
				u.quit()
				return nil
			}
			if u.handleInput(in) {
				return nil
			}
		case ev := <-u.events:
			u.render(ev)
		case err := <-u.turnDone:
			u.finishBusy(err)
		case <-u.warmDone:
			u.warm = true
			u.warmDone = nil // a nil channel never fires again
			if u.askCloudStart() {
				u.refreshPrompt()
			} else if !u.busy && !u.startNextQueued() {
				u.refreshPrompt()
			}
		case <-u.waitC:
			u.drawWaiting()
		case s := <-sigs:
			u.atPrompt = false
			u.write("\n")
			u.warnf("received %s; saving and exiting", s)
			u.quit()
			return nil
		}
	}
}

// readInput sends complete input lines (or """ blocks) to the main loop. A
// bracketed paste (pasteStart..pasteEnd, enabled in Run on a terminal) is
// gathered into one literal message, so a multi-line paste is sent on the next
// Enter rather than split into a message per line or parsed as commands.
func (u *UI) readInput() { u.readFrom(os.Stdin) }

// readFrom is readInput's body over any reader, so the paste/""" parsing can be
// tested without os.Stdin.
func (u *UI) readFrom(in io.Reader) {
	r := bufio.NewReader(in)
	var block []string // lines of an open """ block
	var paste []string // lines of an open bracketed paste
	inBlock := false
	pasting := false

	// feed runs the ordinary (non-paste) handling for one line: the """ toggle,
	// or an ordinary line sent as-is.
	feed := func(line string) {
		switch {
		case strings.TrimSpace(line) == `"""`:
			if inBlock {
				u.lines <- inputLine{text: strings.Join(block, "\n"), literal: true}
				block = nil
			}
			inBlock = !inBlock
		case inBlock:
			block = append(block, line)
		default:
			u.lines <- inputLine{text: line}
		}
	}
	// endPaste closes the current paste: inside a """ block it joins the block,
	// otherwise it becomes its own literal message.
	endPaste := func() {
		text := strings.Join(paste, "\n")
		paste = nil
		pasting = false
		if inBlock {
			block = append(block, strings.Split(text, "\n")...)
		} else if strings.TrimSpace(text) != "" {
			u.lines <- inputLine{text: text, literal: true}
		}
	}

	for {
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			close(u.lines)
			return
		}
		line = strings.TrimRight(line, "\r\n")

		// A physical line may hold paste markers mixed with ordinary text; walk it
		// in fragments. consumed guards against emitting a spurious empty line once
		// a marker has been stripped.
		remaining := line
		consumed := false
		for {
			if pasting {
				if j := strings.Index(remaining, pasteEnd); j >= 0 {
					paste = append(paste, remaining[:j])
					remaining = remaining[j+len(pasteEnd):]
					consumed = true
					endPaste()
					continue
				}
				paste = append(paste, remaining)
				consumed = true
				break // the rest of the paste arrives on later reads
			}
			if i := strings.Index(remaining, pasteStart); i >= 0 {
				if before := remaining[:i]; before != "" {
					feed(before)
				}
				remaining = remaining[i+len(pasteStart):]
				pasting = true
				consumed = true
				continue
			}
			if remaining != "" || !consumed {
				feed(remaining)
			}
			break
		}

		if err != nil {
			close(u.lines)
			return
		}
	}
}

// handleInput routes one line; it returns true when the program should exit.
func (u *UI) handleInput(in inputLine) bool {
	u.atPrompt = false
	text := in.text
	trimmed := strings.TrimSpace(text)
	if !in.literal {
		switch {
		case strings.HasPrefix(trimmed, "!!"):
			text = trimmed[1:]
			trimmed = text
		case strings.HasPrefix(trimmed, "!"):
			if u.control(trimmed) {
				return true
			}
			u.afterCommand()
			return false
		}
	}
	if ct := u.cloudTurn; ct != nil {
		u.cloudTurn = nil
		yes := !in.literal && (strings.EqualFold(trimmed, "y") || strings.EqualFold(trimmed, "yes"))
		switch {
		case yes && ct.text == "" && !ct.retry: // asked at the start of the session
			u.agent.ApproveCloud(ct.endpoint)
			u.infof("approved %s for this session", ct.endpoint)
			if u.busy || !u.startNextQueued() {
				u.promptLine()
			}
		case yes:
			u.agent.ApproveCloud(ct.endpoint)
			u.sendTurn(ct.text, ct.retry)
		default:
			u.infof("nothing was sent; you will be asked again with your next message, or /model <local profile> switches to a local model")
			if ct.text != "" && !ct.retry {
				u.infof("your message: %s", textutil.OneLine(ct.text, 80))
			}
			if n := len(u.queue); n > 0 {
				u.infof("dropped %d queued message(s) as well", n)
				u.queue = nil
			}
			u.promptLine()
		}
		return false
	}
	if u.cloudModel != "" {
		name := u.cloudModel
		u.cloudModel = ""
		if !in.literal && (strings.EqualFold(trimmed, "y") || strings.EqualFold(trimmed, "yes")) {
			_, endpoint := u.agent.CloudTarget(name)
			u.switchToApproved(name, endpoint)
		} else {
			u.infof("not switched")
		}
		if !u.busy {
			u.promptLine()
		}
		return false
	}
	if u.clearID != "" {
		id := u.clearID
		u.clearID = ""
		answer := !in.literal && (strings.EqualFold(trimmed, "y") || strings.EqualFold(trimmed, "yes"))
		if answer && u.agent.Session().ID == id {
			u.clearSession()
		} else {
			u.infof("kept the session")
		}
		if !u.busy {
			u.promptLine()
		}
		return false
	}
	if u.pending != nil {
		u.answerApproval(trimmed, text)
		return false
	}
	if trimmed == "" {
		if !u.busy {
			u.promptLine()
		}
		return false
	}
	if !in.literal && strings.HasPrefix(trimmed, "/") {
		// /rogue off is the one slash command that works mid-task: it is
		// how the operator takes back control without cancelling.
		if u.busy && strings.EqualFold(strings.Join(strings.Fields(trimmed), " "), "/rogue off") {
			u.rogue([]string{"off"})
			return false
		}
		// /model works mid-task too: the list is read-only, and a switch is
		// queued until the current operation ends.
		if f := strings.Fields(trimmed); u.busy && len(f) > 0 && strings.EqualFold(f[0], "/model") {
			u.model(f[1:])
			return false
		}
		if u.busy {
			u.warnf("/ commands work while the agent is idle; use ! commands while it works (!help)")
			return false
		}
		if u.slash(trimmed) {
			return true
		}
		if !u.busy {
			u.promptLine()
		}
		return false
	}
	if u.busy || !u.warm {
		u.queue = append(u.queue, text)
		if u.busy {
			u.infof("queued; it will be sent when the agent finishes (!c cancels the current work and the queue)")
		} else {
			u.infof("queued; it will be sent once the model is ready")
		}
		return false
	}
	u.startTurn(text)
	return false
}

func (u *UI) afterCommand() {
	switch {
	case u.pending != nil:
		u.askApproval()
	case !u.busy:
		u.promptLine()
	}
}

// control handles ! commands, which work at any time.
func (u *UI) control(cmd string) bool {
	f := strings.Fields(cmd)
	name := strings.ToLower(f[0])
	arg := ""
	if len(f) > 1 {
		arg = strings.ToLower(f[1])
	}
	switch name {
	case "!c":
		if !u.busy {
			u.infof("nothing to cancel")
			return false
		}
		dropped := len(u.queue)
		u.queue = nil
		u.pending = nil
		u.cancelBusy()
		msg := "cancelling…"
		if dropped > 0 {
			msg += fmt.Sprintf(" (%d queued message%s discarded)", dropped, plural(dropped))
		}
		u.warnf("%s", msg)
	case "!q":
		u.quit()
		return true
	case "!think":
		v, ok := parseToggle(arg, u.agent.Think())
		if !ok {
			u.warnf("usage: !think [on|off]")
			return false
		}
		if !u.agent.ThinkAvailable() {
			u.warnf("%s has no thinking mode; !think has no effect", u.agent.Model().Model)
			return false
		}
		u.agent.SetThink(v)
		when := ""
		if u.busy {
			when = " (applies from the next model request)"
		} else {
			u.agent.Save()
		}
		u.infof("thinking %s%s", onOff(v), when)
	case "!show":
		v, ok := parseToggle(arg, u.agent.Show())
		if !ok {
			u.warnf("usage: !show [on|off]")
			return false
		}
		u.agent.SetShow(v)
		if !u.busy {
			u.agent.Save()
		}
		u.infof("thinking text %s", map[bool]string{true: "shown", false: "hidden"}[v])
	case "!safe":
		v, ok := parseToggle(arg, u.agent.Safe())
		if !ok {
			u.warnf("usage: !safe [on|off]")
			return false
		}
		u.agent.SetSafe(v)
		if !u.busy {
			u.agent.Save()
		}
		if v {
			u.infof("safe mode on: mv_file and delete_file back up their target first, and each one needs a fresh 'y'")
		} else {
			u.infof("safe mode off: destructive actions behave like other write actions")
		}
	case "!plan":
		v, ok := parseToggle(arg, u.agent.Plan())
		if !ok {
			u.warnf("usage: !plan [on|off]")
			return false
		}
		if v && u.agent.ChatOnly() {
			u.warnf("plan mode needs a model with tool support; %s has none", u.agent.Model().Model)
			return false
		}
		wasRogue := u.agent.Rogue()
		if u.agent.SetPlan(v) && !u.busy {
			u.agent.Save()
		}
		if wasRogue && !u.agent.Rogue() {
			u.warnf("rogue mode switched off: it cannot be combined with plan mode")
		}
		if v {
			u.infof("plan mode on: write/exec/destructive actions are captured, not executed; /plan lists them, /plan approve [n|all] [comment] runs them")
		} else {
			u.infof("plan mode off: actions run as usual (with approval); %d pending action%s stay in the list", u.agent.PlanPending(), plural(u.agent.PlanPending()))
		}
	case "!skip":
		if u.opt.Deps.Guard == nil {
			u.infof("the GPU guard is disabled")
		} else {
			u.opt.Deps.Guard.Skip()
			u.infof("skipping the cool-down pause, if one is active")
		}
	case "!help", "!h", "!?":
		u.help()
	default:
		u.warnf("unknown command %s; type !help for the list, or start a message with !! to send a literal '!'", name)
	}
	return false
}

// slash handles / commands, which work only while the agent is idle.
func (u *UI) slash(cmd string) bool {
	f := strings.Fields(cmd)
	name := strings.ToLower(f[0])
	args := f[1:]
	switch name {
	case "/help":
		u.help()
	case "/exit", "/quit":
		u.quit()
		return true
	case "/context":
		u.block(u.agent.ContextReport())
	case "/compact":
		// /compact <percent> summarises that share of the oldest conversation once
		ratio := 0.0
		if len(args) > 0 {
			p, err := strconv.Atoi(strings.TrimSuffix(args[0], "%"))
			if err != nil || p < 10 || p > 90 || len(args) > 1 {
				u.warnf("usage: /compact [percent]  (10-90: the share of the oldest conversation to summarise)")
				return false
			}
			ratio = 1 - float64(p)/100
		}
		u.runBusy("compact", func(ctx context.Context) error { return u.agent.Compact(ctx, ratio) })
	case "/sessions":
		u.listSessions()
	case "/new":
		u.agent.Save()
		s, notes, err := u.opt.NewSession()
		if err != nil {
			u.errorf("%v", err)
			return false
		}
		u.switchSession(s, notes)
	case "/clear":
		s := u.agent.Session()
		msgs := 0
		for _, e := range s.Transcript {
			if e.Kind != session.KindNote {
				msgs++
			}
		}
		if msgs == 0 {
			u.infof("this session has no messages yet; nothing to clear")
			return false
		}
		u.clearID = s.ID
		u.warnf("/clear discards this session's %d-message conversation and starts a new one; files you worked on and safe-mode backups are kept. Clear it? [y/N]", msgs)
	case "/load":
		if len(args) == 0 {
			u.warnf("usage: /load <session id> (see /sessions)")
			return false
		}
		u.agent.Save()
		s, err := u.opt.Deps.Store.Load(args[0])
		if err != nil {
			u.errorf("%v", err)
			return false
		}
		u.switchSession(s, nil)
	case "/undo":
		if _, err := u.agent.Undo(); err != nil {
			u.warnf("%v", err)
			return false
		}
		u.infof("removed the last exchange from the context. Files changed by tools are not restored. The next request re-reads the conversation.")
	case "/retry":
		if e := u.agent.CloudApprovalNeeded(); e != "" {
			u.askCloudTurn(&cloudTurn{endpoint: e, retry: true})
			return false
		}
		text, err := u.agent.Undo()
		if err != nil {
			u.warnf("%v", err)
			return false
		}
		u.infof("resending: %s", textutil.OneLine(text, 80))
		u.startTurn(text)
	case "/system":
		u.warnf("/system is now /system-prompt [reload]")
	case "/system-prompt":
		if len(args) > 0 && strings.EqualFold(args[0], "reload") {
			s := u.agent.Session()
			p, notes, err := u.opt.BuildPrompts(s.Date)
			if err != nil {
				u.errorf("%v", err)
				return false
			}
			u.agent.SetSystems(p)
			u.agent.Save()
			for _, n := range notes {
				u.infof("%s", n)
			}
			u.infof("system prompts rebuilt (normal %d, rogue mode %d, chat-only %d characters); the next request re-reads the whole conversation", len(p.Normal), len(p.Rogue), len(p.Chat))
		} else {
			s := u.agent.Session()
			if u.agent.ChatOnly() {
				u.infof("the system prompt for a model without tools (in use now):")
				if s.SystemChat != "" {
					u.block(s.SystemChat)
				} else {
					u.block(s.System)
				}
			} else if u.agent.Rogue() {
				u.infof("the system prompt for rogue mode (in use now):")
				if s.SystemRogue != "" {
					u.block(s.SystemRogue)
				} else {
					u.block(s.System)
				}
			} else {
				u.infof("the system prompt (rogue mode uses its own; /rogue on, then /system-prompt shows it):")
				u.block(s.System)
			}
		}
	case "/model":
		u.model(args)
	case "/adapt":
		u.adapt(args)
	case "/gpu":
		u.block(u.opt.Deps.Guard.Report())
	case "/approvals":
		if len(args) > 0 && strings.EqualFold(args[0], "reset") {
			u.agent.ResetApprovals()
			u.infof("session approvals reset; only tools.auto_approve from the config still run without asking")
		} else if list := u.agent.AutoApproved(); len(list) == 0 {
			u.infof("every write and exec action asks for approval")
		} else {
			u.infof("running without approval: %s (/approvals reset to undo this session's choices)", strings.Join(list, ", "))
		}
	case "/safe":
		if len(args) > 0 && strings.EqualFold(args[0], "restore") {
			force := len(args) == 3 && strings.EqualFold(args[2], "force")
			if len(args) != 2 && !force {
				u.warnf("usage: /safe restore <number> [force] (see /safe)")
				return false
			}
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 1 {
				u.warnf("the backup number must be a positive integer")
				return false
			}
			out, err := u.agent.RestoreBackup(n, force)
			if err != nil {
				u.errorf("%v", err)
				return false
			}
			u.infof("%s", out)
			return false
		}
		u.block(u.agent.SafeReport())
	case "/rogue":
		u.rogue(args)
	case "/fetch":
		u.fetch(args)
	case "/plan":
		if len(args) > 0 && (strings.EqualFold(args[0], "approve") || strings.EqualFold(args[0], "reject")) {
			approve := strings.EqualFold(args[0], "approve")
			// the comment is taken from the raw line, so its spacing is kept
			_, rest := nextField(cmd)
			_, rest = nextField(rest)
			p, err := parsePlanArgs(rest, u.agent.PlanPending(), approve)
			if err != nil {
				u.warnf("%v", err)
				return false
			}
			u.runBusy("plan", func(ctx context.Context) error {
				var report, cont string
				var err error
				if approve {
					report, cont, err = u.agent.PlanApprove(ctx, p.nums, agent.PlanApproveOptions{Comment: p.comment, NoContinue: p.noContinue})
				} else {
					report, cont, err = u.agent.PlanReject(p.nums, p.comment)
				}
				if err != nil {
					return err
				}
				u.infof("%s", report)
				if cont != "" {
					return u.agent.RunHarnessTurn(ctx, cont)
				}
				return nil
			})
			return false
		}
		u.block(u.agent.PlanReport())
	default:
		u.warnf("unknown command %s; type /help", name)
	}
	return false
}

// rogue handles /rogue [on [steps]|off].
// fetch controls the fetch_url allowlist: /fetch shows it, /fetch on|off
// toggles the gate, /fetch allow|forget <host> edits the list. When the gate
// is on, fetch_url to a listed host runs without asking (even in rogue mode)
// and an unlisted host is always asked about.
func (u *UI) fetch(args []string) {
	a := u.agent
	show := func() {
		state := "off"
		if a.FetchGate() {
			state = "on"
		}
		u.infof("fetch allowlist gate: %s", state)
		if hosts := a.FetchAllowed(); len(hosts) > 0 {
			u.infof("allowed hosts: %s", strings.Join(hosts, ", "))
		} else {
			u.infof("allowed hosts: (none yet; approve a fetch with 'a', or /fetch allow <host>)")
		}
	}
	if len(args) == 0 {
		show()
		return
	}
	switch strings.ToLower(args[0]) {
	case "on":
		a.SetFetchGate(true)
		u.infof("fetch allowlist on: fetch_url to an unlisted host now asks for approval, even in rogue mode")
	case "off":
		a.SetFetchGate(false)
		u.infof("fetch allowlist off: fetch_url follows the normal rules (manual approves, rogue runs)")
	case "allow":
		if len(args) != 2 {
			u.warnf("usage: /fetch allow <host>")
			return
		}
		a.FetchAllow(args[1])
		a.Save()
		u.infof("added %s to the fetch allowlist", args[1])
	case "forget":
		if len(args) != 2 {
			u.warnf("usage: /fetch forget <host>")
			return
		}
		a.FetchForget(args[1])
		a.Save()
		u.infof("removed %s from the fetch allowlist", args[1])
	default:
		u.warnf("usage: /fetch [on|off|allow <host>|forget <host>]")
	}
}

func (u *UI) rogue(args []string) {
	a := u.agent
	if len(args) == 0 {
		if !a.Rogue() {
			u.infof("rogue mode is off. /rogue on [steps] runs every action without approval until the model finishes each task")
			return
		}
		u.infof("rogue mode is on: %s", u.rogueLimits())
		return
	}
	switch strings.ToLower(args[0]) {
	case "on":
		steps := 0
		if len(args) > 2 {
			u.warnf("usage: /rogue on [steps]")
			return
		}
		if len(args) == 2 {
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 1 {
				u.warnf("the step budget must be a positive number")
				return
			}
			steps = n
		}
		if !a.HasRogueSystem() {
			// a session saved before rogue mode had its own system prompt
			s := a.Session()
			p, _, err := u.opt.BuildPrompts(s.Date)
			if err != nil {
				u.errorf("could not build the rogue-mode system prompt: %v", err)
				return
			}
			a.SetSystems(agent.Prompts{Normal: s.System, Rogue: p.Rogue, Chat: s.SystemChat}) // the other prompts stay as they were frozen
		}
		if _, err := a.SetRogue(true, steps); err != nil {
			u.warnf("%v", err)
			return
		}
		a.Save()
		var unattended []string
		for _, n := range u.opt.Deps.Registry.Names() {
			if t, ok := u.opt.Deps.Registry.Get(n); ok && t.Risk() != tools.ReadOnly {
				unattended = append(unattended, n)
			}
		}
		u.warnf("ROGUE MODE ON: %s now run without approval", strings.Join(unattended, ", "))
		u.warnf("per message: %s; mv/delete are always backed up first (/safe restore)", u.rogueLimits())
		if ws := u.opt.Deps.WS; ws != nil && ws.HasAgentDir() {
			u.warnf("outside %s the file tools have %s access; exec_command is not confined", ws.Root, ws.Access)
		}
		u.warnf("stop the current task with !c, take back approvals with /rogue off (works mid-task)")
		if !a.Think() {
			u.infof("thinking is off; the rogue-mode prompt asks the model to think before acting (!think on)")
		}
	case "off":
		if changed, _ := a.SetRogue(false, 0); !changed {
			u.infof("rogue mode is already off")
			return
		}
		if !u.busy {
			a.Save()
		}
		u.infof("rogue mode off: actions need approval again from the next one")
	default:
		u.warnf("usage: /rogue [on [steps]|off]")
	}
}

func (u *UI) rogueLimits() string {
	s := fmt.Sprintf("up to %d steps", u.agent.RogueBudget())
	if m := u.opt.Deps.Cfg.Rogue.MaxMinutes; m > 0 {
		s += fmt.Sprintf(" and %d minutes", m)
	}
	return s + ", until the model calls finish"
}

// clearSession discards the current session and starts a new one in its
// place, so a throwaway conversation does not linger in /sessions. It
// removes only the session's own record: files the agent worked on, the
// session's safe-mode backups and the audit log are left as they are.
func (u *UI) clearSession() {
	old := u.agent.Session()
	store := u.opt.Deps.Store
	s, notes, err := u.opt.NewSession()
	if err != nil {
		u.errorf("could not start a new session, so the old one was kept: %v", err)
		return
	}
	if err := store.Delete(old.ID); err != nil {
		u.warnf("started a new session, but the old one could not be removed from %s: %v", store.Dir, err)
	}
	if u.opt.Deps.Audit != nil {
		u.opt.Deps.Audit.Log(map[string]any{"session": old.ID, "event": "cleared"})
	}
	u.switchSession(s, notes)
	u.infof("cleared the previous session (%s)", old.ID)
}

func (u *UI) switchSession(s *session.Session, notes []string) {
	u.agent = agent.New(u.opt.Deps, s, u.events)
	u.infof("session %s: %d messages in context", s.ID, len(s.Context))
	for _, n := range notes {
		u.infof("%s", n)
	}
	u.warnModelChange(s)
	u.ensureChatPrompt()
	u.askCloudStart()
	u.showLastReply(s)
}

// warnModelChange says when a session continues with a different model
// than it last used (/load keeps the model that is loaded now), and how to
// switch back.
func (u *UI) warnModelChange(s *session.Session) {
	m := u.agent.Model().Model
	if s.Model == "" || s.Model == m {
		return
	}
	u.warnf("this session last used %s; continuing with %s", s.Model, m)
	back := s.Profile
	if back == "" {
		back = s.Model
	}
	u.infof("/model %s switches back", back)
}

// ensureChatPrompt adds the chat-only system prompt to a session saved
// before it existed, so a model without tools never gets the prompt that
// describes tools. The normal and rogue-mode prompts stay as they were
// frozen.
func (u *UI) ensureChatPrompt() {
	a := u.agent
	if a.HasChatSystem() || u.opt.BuildPrompts == nil {
		return
	}
	s := a.Session()
	p, _, err := u.opt.BuildPrompts(s.Date)
	if err != nil {
		u.warnf("could not build the chat-only system prompt: %v", err)
		return
	}
	a.SetSystems(agent.Prompts{Normal: s.System, Rogue: s.SystemRogue, Chat: p.Chat})
	a.Save()
}

func (u *UI) listSessions() {
	list, err := u.opt.Deps.Store.List()
	if err != nil {
		u.errorf("%v", err)
		return
	}
	if len(list) == 0 {
		u.infof("no saved sessions")
		return
	}
	current := u.agent.Session().ID
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
	u.block(strings.TrimRight(sb.String(), "\n"))
}

// cloudTurn is a message (or a /retry) waiting for the operator to approve
// sending the session to the active cloud endpoint.
type cloudTurn struct {
	endpoint string
	text     string
	retry    bool
}

func (u *UI) askCloudTurn(ct *cloudTurn) {
	u.cloudTurn = ct
	u.warnf("the active model runs on %s: this conversation, including the file contents and command output in it, is sent there. Send? [y/N]", ct.endpoint)
}

// askCloudStart asks up front when the session starts on a cloud endpoint
// it has not approved (-profile, default_profile, -resume, /new, /clear,
// /load), instead of waiting for the first message. It reports whether it
// asked.
func (u *UI) askCloudStart() bool {
	if u.cloudTurn != nil || !u.warm {
		return false
	}
	if e := u.agent.CloudApprovalNeeded(); e != "" {
		u.askCloudTurn(&cloudTurn{endpoint: e})
		return true
	}
	return false
}

// startTurn sends a message, after asking if the active model is a cloud
// one this session has not approved yet.
func (u *UI) startTurn(text string) {
	if e := u.agent.CloudApprovalNeeded(); e != "" {
		u.askCloudTurn(&cloudTurn{endpoint: e, text: text})
		return
	}
	u.runBusy("turn", func(ctx context.Context) error { return u.agent.RunTurn(ctx, text) })
}

// sendTurn runs an approved held message, or the held /retry.
func (u *UI) sendTurn(text string, retry bool) {
	if retry {
		t, err := u.agent.Undo()
		if err != nil {
			u.warnf("%v", err)
			u.promptLine()
			return
		}
		u.infof("resending: %s", textutil.OneLine(t, 80))
		text = t
	}
	u.startTurn(text)
}

func (u *UI) runBusy(kind string, fn func(context.Context) error) {
	ctx, cancel := context.WithCancel(u.ctx)
	u.atPrompt = false
	u.busy, u.busyKind, u.cancelBusy = true, kind, cancel
	go func() {
		err := fn(ctx)
		cancel()
		u.turnDone <- err
	}()
}

func (u *UI) finishBusy(err error) {
	u.drainEvents() // the agent sent all its events before finishing
	u.stopWaiting()
	u.endStream()
	kind := u.busyKind
	u.busy, u.busyKind, u.cancelBusy, u.pending = false, "", nil, nil
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		u.warnf("cancelled")
	default:
		u.errorf("%v", err)
		if kind == "turn" {
			u.infof("/retry resends your last message")
		}
	}
	if name := u.nextModel; name != "" {
		ok := u.nextOK
		u.nextModel, u.nextOK = "", ""
		if errors.Is(err, context.Canceled) {
			u.infof("the switch to %s was dropped with the cancelled operation", name)
		} else {
			u.startSwitch(name, ok) // queued messages wait until the switch is done
			return
		}
	}
	if !u.startNextQueued() {
		u.promptLine()
	}
}

// model lists the profiles and models, or switches to one. A switch typed
// while the agent works is applied when the current operation ends.
func (u *UI) model(args []string) {
	if len(args) == 0 {
		ctx, cancel := context.WithTimeout(u.ctx, 20*time.Second)
		defer cancel()
		u.block(u.agent.ModelList(ctx))
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
		ctx, cancel := context.WithTimeout(u.ctx, 30*time.Second)
		defer cancel()
		u.block(u.agent.OnlineList(ctx, filter, all))
		return
	}
	if len(args) > 1 {
		u.warnf("usage: /model [profile or model | --online [filter] [--all]]")
		return
	}
	name := args[0]
	if _, endpoint := u.agent.CloudTarget(name); endpoint != "" && !u.agent.CloudApproved(endpoint) && u.warm && !u.agent.Rogue() {
		u.cloudModel = name
		u.warnf("%s runs on %s: from now on this conversation, including the file contents and command output in it, is sent there. Switch? [y/N]", name, endpoint)
		return
	}
	u.switchTo(name)
}

// switchTo switches models now, or when the current operation ends.
func (u *UI) switchTo(name string) { u.switchToApproved(name, "") }

// switchToApproved is switchTo with the cloud endpoint the operator just
// approved; the approval is recorded only if the switch succeeds.
func (u *UI) switchToApproved(name, endpoint string) {
	switch {
	case !u.warm:
		u.warnf("the model is still loading; switch once it is ready")
	case u.agent.Rogue():
		u.warnf("rogue mode stays on the model it started with; turn it off first (/rogue off)")
	case u.busy:
		u.nextModel, u.nextOK = name, endpoint
		u.infof("switching to %s when the current operation ends", name)
	default:
		u.startSwitch(name, endpoint)
	}
}

// adapt lists adapted models, measures one, or pins or forgets a value.
func (u *UI) adapt(args []string) {
	if len(args) == 0 {
		u.block(u.agent.AdaptReport())
		return
	}
	name, rest := args[0], args[1:]
	ctx, cancel := context.WithTimeout(u.ctx, 20*time.Second)
	defer cancel()
	switch {
	case len(rest) == 2 && strings.EqualFold(rest[0], "set"):
		n, err := strconv.Atoi(rest[1])
		if err != nil {
			u.warnf("usage: /adapt <name> set <tokens>")
			return
		}
		if err := u.agent.AdaptSet(ctx, name, n); err != nil {
			u.warnf("%v", err)
		}
	case len(rest) == 1 && strings.EqualFold(rest[0], "forget"):
		if err := u.agent.AdaptForget(ctx, name); err != nil {
			u.warnf("%v", err)
		}
	case len(rest) == 0 || (len(rest) == 1 && rest[0] == "--verify"):
		switch {
		case !u.warm:
			u.warnf("the model is still loading; run /adapt once it is ready")
		case u.agent.Rogue():
			u.warnf("not in rogue mode: /adapt takes the GPU away from the model for minutes")
		default:
			verify := len(rest) == 1
			u.runBusy("adapt", func(ctx context.Context) error {
				return u.agent.AdaptModel(ctx, name, verify, u.opt.BuildPrompts)
			})
		}
	default:
		u.warnf("usage: /adapt [<name> [--verify] | <name> set <tokens> | <name> forget]")
	}
}

// startSwitch runs a switch; approved is the cloud endpoint the operator
// agreed to, recorded only if the switch lands on it.
func (u *UI) startSwitch(name, approved string) {
	u.runBusy("model", func(ctx context.Context) error {
		err := u.agent.SwitchModel(ctx, name, u.opt.BuildPrompts)
		if err == nil && approved != "" && u.agent.Models.Active().Endpoint == approved {
			u.agent.ApproveCloud(approved)
		}
		return err
	})
}

func (u *UI) startNextQueued() bool {
	if len(u.queue) == 0 || !u.warm || u.busy {
		return false
	}
	text := u.queue[0]
	u.queue = u.queue[1:]
	if u.atPrompt && u.tty {
		u.write("\r\x1b[2K")
	}
	u.atPrompt = false
	u.line(u.style(cDim, "› "+textutil.OneLine(text, 100)))
	u.startTurn(text)
	return true
}

func (u *UI) drainEvents() {
	for {
		select {
		case ev := <-u.events:
			u.render(ev)
		default:
			return
		}
	}
}

// quit stops any running work, saves the session and says how to resume.
func (u *UI) quit() {
	if u.busy {
		u.cancelBusy()
		u.infof("stopping the current work…")
		timeout := time.After(20 * time.Second)
	wait:
		for {
			select {
			case ev := <-u.events:
				u.render(ev)
			case <-u.turnDone:
				u.busy = false
				break wait
			case <-timeout:
				u.warnf("the agent did not stop in time; saving anyway")
				break wait
			}
		}
	}
	u.stopWaiting()
	u.drainEvents()
	u.endStream()
	u.agent.Save()
	u.drainEvents()
	id := u.agent.Session().ID
	u.infof("session %s saved; resume with: clauzette -session %s (or clauzette -resume)", id, id)
}

// ------------------------------------------------------------- approvals

func (u *UI) showApproval(full bool) {
	a := u.pending
	u.line(u.style(cYellow+cBold, fmt.Sprintf("Approval needed (%s): %s", a.Risk, a.Summary)))
	if a.Preview != "" {
		lines := strings.Split(strings.TrimRight(a.Preview, "\n"), "\n")
		const limit = 60
		shown := lines
		if !full && len(lines) > limit {
			shown = lines[:limit]
		}
		for _, l := range shown {
			u.line(u.colorDiff(l))
		}
		if len(shown) < len(lines) {
			u.line(u.style(cDim, fmt.Sprintf("… %d more lines (v shows everything)", len(lines)-len(shown))))
		}
	}
	u.askApproval()
}

func (u *UI) askApproval() {
	always := fmt.Sprintf("always %s", u.pending.Tool)
	if u.pending.AllowScope != "" {
		always = fmt.Sprintf("allowlist %s", u.pending.AllowScope)
	}
	u.write(u.style(cYellow, fmt.Sprintf("Approve? y / n [reason] / a (%s) / v (view all): ", always)))
}

func (u *UI) answerApproval(trimmed, raw string) {
	a := u.pending
	lower := strings.ToLower(trimmed)
	reply := func(d agent.Decision) {
		a.Reply <- d
		u.pending = nil
	}
	switch {
	case lower == "y" || lower == "yes":
		reply(agent.Decision{Approve: true})
	case lower == "a" || lower == "always":
		reply(agent.Decision{Approve: true, Always: true})
		if a.AllowScope != "" {
			u.infof("%s added to the fetch allowlist; fetches to it now run without asking (/fetch forget %s to undo)", a.AllowScope, a.AllowScope)
		} else {
			u.infof("%s will run without asking for the rest of this session (/approvals reset to undo)", a.Tool)
		}
	case lower == "n" || lower == "no":
		reply(agent.Decision{Approve: false})
	case strings.HasPrefix(lower, "n ") || strings.HasPrefix(lower, "no "):
		reason := strings.TrimSpace(raw)
		reason = strings.TrimSpace(reason[strings.IndexByte(reason, ' ')+1:])
		reply(agent.Decision{Approve: false, Reason: reason})
	case lower == "v" || lower == "view":
		u.showApproval(true)
	default:
		u.warnf("answer y, n (optionally followed by a reason), a or v; or !c to stop the whole turn")
		u.askApproval()
	}
}

// -------------------------------------------------------------- rendering

func (u *UI) render(ev agent.Event) {
	switch ev.Kind {
	case agent.EvWaiting:
		u.startWaiting(ev.Text)
	case agent.EvWaitingDone:
		u.stopWaiting()
	case agent.EvThinking:
		if !u.agent.Show() {
			if u.stream != streamThinkingHidden {
				u.endStream()
				u.write(u.style(cDim, "(thinking…)"))
				u.stream = streamThinkingHidden
			}
			return
		}
		if u.stream != streamThinking {
			u.endStream()
			u.write(u.style(cDim, "thinking: "))
			u.stream = streamThinking
		}
		u.write(u.style(cDim, ev.Text))
	case agent.EvText:
		if u.stream != streamText {
			u.endStream()
			u.stream = streamText
		}
		u.write(ev.Text)
	case agent.EvStreamEnd:
		u.endStream()
	case agent.EvToolCall:
		u.line(u.style(cCyan, "→ "+ev.Tool) + " " + u.style(cDim, ev.Text))
	case agent.EvToolResult:
		if ev.IsError {
			u.line(u.style(cRed, "  ✗ "+textutil.OneLine(ev.Text, 200)))
		} else {
			first := textutil.Truncate(strings.TrimSpace(textutil.FirstLine(ev.Text)), 120)
			if n := strings.Count(strings.TrimRight(ev.Text, "\n"), "\n") + 1; n > 1 {
				first += fmt.Sprintf(" (%d lines)", n)
			}
			u.line(u.style(cDim, "  ✓ "+first))
		}
	case agent.EvApproval:
		u.pending = ev.Approval
		u.showApproval(false)
	case agent.EvInfo:
		u.infof("%s", ev.Text)
	case agent.EvWarn:
		u.warnf("%s", ev.Text)
	case agent.EvError:
		u.errorf("%s", ev.Text)
	case agent.EvUsage:
		if ev.Usage.TokPerSec > 0 {
			u.line(u.style(cDim, fmt.Sprintf("  [%d tokens, %.1f tok/s · context ~%s/%s]",
				ev.Usage.Eval, ev.Usage.TokPerSec, textutil.KTok(ev.Usage.Tokens), textutil.KTok(ev.Usage.NumCtx))))
		}
	}
}

func (u *UI) endStream() {
	if u.stream != streamNone {
		u.write("\n")
		u.stream = streamNone
	}
}

// line prints a complete line without disturbing the prompt or the
// waiting indicator: both are cleared and redrawn around it.
func (u *UI) line(s string) {
	u.endStream()
	if u.tty && (u.atPrompt || u.waitC != nil) {
		u.write("\r\x1b[2K")
	}
	u.write(s + "\n")
	if u.waitC != nil {
		u.drawWaiting()
	}
	if u.atPrompt {
		u.write(u.promptString())
	}
}

func (u *UI) block(s string) { u.line(strings.TrimRight(s, "\n")) }

func (u *UI) infof(format string, args ...any) {
	u.line(u.style(cDim, "· "+fmt.Sprintf(format, args...)))
}

func (u *UI) warnf(format string, args ...any) {
	u.line(u.style(cYellow, "! "+fmt.Sprintf(format, args...)))
}

func (u *UI) errorf(format string, args ...any) {
	u.line(u.style(cRed, "✗ "+fmt.Sprintf(format, args...)))
}

func (u *UI) write(s string) { _, _ = os.Stdout.WriteString(s) }

func (u *UI) style(code, s string) string {
	if !u.color {
		return s
	}
	return code + s + cReset
}

func (u *UI) colorDiff(l string) string {
	switch {
	case strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---") || strings.HasPrefix(l, "$ "):
		return u.style(cBold, l)
	case strings.HasPrefix(l, "@@"):
		return u.style(cCyan, l)
	case strings.HasPrefix(l, "+"):
		return u.style(cGreen, l)
	case strings.HasPrefix(l, "-"):
		return u.style(cRed, l)
	}
	return l
}

// The waiting indicator shows that a long silent phase is in progress
// (loading the model, reading a long prompt, building a tool call,
// compacting) and how long it has been going.
func (u *UI) startWaiting(text string) {
	u.endStream()
	u.waitText, u.waitSince = text, time.Now()
	if !u.tty {
		u.write(u.style(cDim, "⋯ "+text+"\n"))
		return
	}
	if u.waitTick == nil {
		u.waitTick = time.NewTicker(time.Second)
	}
	u.waitC = u.waitTick.C
	u.drawWaiting()
}

func (u *UI) drawWaiting() {
	if !u.tty || u.waitC == nil {
		return
	}
	elapsed := time.Since(u.waitSince).Round(time.Second)
	u.write("\r\x1b[2K" + u.style(cDim, fmt.Sprintf("⋯ %s %s", u.waitText, elapsed)))
}

func (u *UI) stopWaiting() {
	if u.waitC == nil {
		return
	}
	u.waitTick.Stop()
	u.waitTick = nil
	u.waitC = nil
	u.write("\r\x1b[2K")
}

func (u *UI) promptString() string {
	think := "think " + onOff(u.agent.Think())
	if !u.agent.ThinkAvailable() {
		think = "think n/a"
	}
	parts := []string{
		fmt.Sprintf("ctx %s/%s", textutil.KTok(u.agent.UsedTokens()), textutil.KTok(u.agent.Window())),
		think,
	}
	if u.agent.ChatOnly() {
		parts = append(parts, "chat only")
	}
	if c := u.agent.Model().Cloud; c != "" {
		parts = append(parts, "cloud: "+c)
	}
	if s := u.opt.Deps.Guard.Status(); s != "" {
		parts = append(parts, s)
	}
	if !u.warm {
		parts = append(parts, "model loading")
	}
	mode := ""
	if u.agent.Rogue() {
		mode = u.style(cRed+cBold, "ROGUE") + " "
	}
	return mode + u.style(cDim, "["+strings.Join(parts, " · ")+"]") + " " + u.style(cBold, "› ")
}

func (u *UI) promptLine() {
	u.endStream()
	u.write(u.promptString())
	u.atPrompt = true
}

func (u *UI) refreshPrompt() {
	if u.atPrompt && u.tty {
		u.write("\r\x1b[2K" + u.promptString())
	}
}

func (u *UI) banner() {
	cfg := u.opt.Deps.Cfg
	s := u.agent.Session()
	where := cfg.Workspace
	if ws := u.opt.Deps.WS; ws != nil {
		where = ws.Root
		if ws.HasAgentDir() {
			where = fmt.Sprintf("%s (shared %s: %s)", ws.Root, ws.Share, ws.Access)
		}
	}
	act := u.agent.Model()
	model := act.Model
	if act.Profile != config.ImplicitProfile && act.Profile != act.Model {
		model = fmt.Sprintf("%s (profile %s)", act.Model, act.Profile)
	}
	server := cfg.OllamaURL
	if act.Cloud != "" {
		model += " on " + act.Cloud
		_, pc := cfg.ProviderFor(act.Source())
		server = pc.BaseURL
	}
	u.line(u.style(cBold, "Clauzette - Flush your thoughts.") + u.style(cDim, fmt.Sprintf(" · %s @ %s · workspace %s", model, server, where)))
	u.ensureChatPrompt()
	if u.opt.Resumed {
		u.infof("resumed session %s: %d messages in context", s.ID, len(s.Context))
		u.warnModelChange(s)
		u.showLastReply(s)
	} else {
		u.infof("new session %s", s.ID)
	}
	for _, n := range u.opt.Notes {
		u.infof("%s", n)
	}
	u.infof("!help lists the commands. Messages typed before the model is ready are queued.")
}

func (u *UI) showLastReply(s *session.Session) {
	for i := len(s.Context) - 1; i >= 0; i-- {
		e := s.Context[i]
		if e.Role == "assistant" && strings.TrimSpace(e.Content) != "" {
			u.line(u.style(cDim, "last reply: "+textutil.Truncate(strings.TrimSpace(e.Content), 400)))
			return
		}
	}
}

func (u *UI) help() {
	u.block(`Any time, even while the agent works:
  !c                cancel the current operation (and discard queued messages)
  !q                save and exit
  !think [on|off]   toggle model thinking (applies from the next model request)
  !show [on|off]    toggle displaying the thinking text
  !safe [on|off]    safe mode: back up before mv/delete, and confirm each one separately
  !plan [on|off]    plan mode: capture write/exec actions instead of running them
  !skip             skip a GPU cool-down pause
  !!text            send a message that starts with '!'
While idle:
  /context          context usage and compaction settings
  /compact [percent]  compact the context now; with a percent (10-90),
                    summarise that share of the oldest conversation this once
  /undo  /retry     drop the last exchange (and resend it)
  /sessions  /new  /load <id>
  /clear            discard this session's conversation and start a new one
                    (files and safe-mode backups are kept; asks first)
  /model [name]     list profiles and installed models, or switch to a profile
                    or model (also mid-task: then it waits for the task to end;
                    not in rogue mode). A cloud model is provider:model, e.g.
                    openrouter:vendor/model:free; the first switch to a cloud
                    provider in a session asks first
  /model --online [filter] [--all]  OpenRouter's free models (--all: paid too)
  /adapt [name [--verify]]  list adapted models, or find the largest context
                    that keeps a model fully on the GPU (unloads all models,
                    takes minutes; --verify also runs one near-full request)
  /adapt <name> set <tokens> | forget   pin or drop a model's value
  /system-prompt [reload]  show the system prompt in use, or rebuild all three
                    (normal, rogue mode, chat-only)
  /gpu              GPU guard status
  /approvals [reset]
  /safe [restore <n> [force]]  safe mode status and backups, restore a backup
                    (force replaces what exists now, after backing it up)
  /rogue [on [steps]|off]  rogue mode: no approvals, work until the model calls
                    finish (budget: steps or agent.max_steps, rogue.max_minutes);
                    /rogue off also works mid-task
  /fetch [on|off|allow <host>|forget <host>]  fetch_url allowlist: when on, a
                    listed host runs without asking (even in rogue), an unlisted
                    one is always asked about
  /plan             pending actions from plan mode
  /plan approve [n ...|all] [--no-continue] [comment]
                    run pending actions in order; the comment goes to the model
                    with the results, --no-continue holds them for your next message
  /plan reject [n ...|all] [reason]  discard them and tell the model why
                    (use -- before a comment or reason that starts with a number)
  /help  /exit
Input:
  """ on its own line starts and ends a multi-line message.
  Pasting multiple lines is taken as one message and sent on the next Enter,
  not split per line (needs a terminal with bracketed paste; most have it).
  Messages typed while the agent works are queued.
  Ctrl+D, Ctrl+C or a dropped connection save the session and exit.
Approvals:
  y = yes, n [reason] = no, a = always for this tool in this session
      (for a gated fetch, a adds the domain to the allowlist instead), v = show the full diff`)
}

func parseToggle(arg string, current bool) (bool, bool) {
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

// planArgs is the parsed tail of "/plan approve" or "/plan reject".
type planArgs struct {
	nums       []int  // nil selects all
	comment    string // the comment (approve) or reason (reject), as typed
	noContinue bool
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
