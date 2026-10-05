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
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"clauzette/internal/agent"
	"clauzette/internal/session"
	"clauzette/internal/textutil"
)

type Options struct {
	Deps        agent.Deps
	NewSession  func() (*session.Session, []string, error)
	BuildPrompt func(date string) (string, []string, error)
	Notes       []string
	Resumed     bool
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
	busyKind   string // "turn" or "compact"
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
			if !u.busy && !u.startNextQueued() {
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

// readInput sends complete input lines (or """ blocks) to the main loop.
func (u *UI) readInput() {
	r := bufio.NewReader(os.Stdin)
	var block []string
	inBlock := false
	for {
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			close(u.lines)
			return
		}
		line = strings.TrimRight(line, "\r\n")
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
		if u.agent.SetPlan(v) && !u.busy {
			u.agent.Save()
		}
		if v {
			u.infof("plan mode on: write/exec/destructive actions are captured, not executed; /plan lists them, /plan approve [n|all] runs them")
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
		u.runBusy("compact", func(ctx context.Context) error { return u.agent.Compact(ctx) })
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
		text, err := u.agent.Undo()
		if err != nil {
			u.warnf("%v", err)
			return false
		}
		u.infof("resending: %s", textutil.OneLine(text, 80))
		u.startTurn(text)
	case "/system":
		if len(args) > 0 && strings.EqualFold(args[0], "reload") {
			s := u.agent.Session()
			sys, notes, err := u.opt.BuildPrompt(s.Date)
			if err != nil {
				u.errorf("%v", err)
				return false
			}
			u.agent.SetSystem(sys)
			u.agent.Save()
			for _, n := range notes {
				u.infof("%s", n)
			}
			u.infof("system prompt rebuilt (%d characters); the next request re-reads the whole conversation", len(sys))
		} else {
			u.block(u.agent.Session().System)
		}
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
			if len(args) != 2 {
				u.warnf("usage: /safe restore <number> (see /safe)")
				return false
			}
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 1 {
				u.warnf("the backup number must be a positive integer")
				return false
			}
			out, err := u.agent.RestoreBackup(n)
			if err != nil {
				u.errorf("%v", err)
				return false
			}
			u.infof("%s", out)
			return false
		}
		u.block(u.agent.SafeReport())
	case "/plan":
		if len(args) > 0 && strings.EqualFold(args[0], "approve") {
			nums, err := parsePlanNumbers(args[1:], u.agent.PlanPending())
			if err != nil {
				u.warnf("%v", err)
				return false
			}
			u.runBusy("plan", func(ctx context.Context) error {
				report, cont, err := u.agent.PlanApprove(ctx, nums)
				if err != nil {
					return err
				}
				u.infof("%s", report)
				if cont != "" {
					return u.agent.RunTurn(ctx, cont)
				}
				return nil
			})
			return false
		}
		if len(args) > 0 && strings.EqualFold(args[0], "reject") {
			nums, rest, err := splitPlanNumbers(args[1:])
			if err != nil {
				u.warnf("%v", err)
				return false
			}
			// validate the numbers now, before the model turn is started
			if nums != nil {
				for _, n := range nums {
					if n < 1 || n > u.agent.PlanPending() {
						u.warnf("no pending action %d; this session has %d (see /plan)", n, u.agent.PlanPending())
						return false
					}
				}
			} else if u.agent.PlanPending() == 0 {
				u.warnf("no pending actions; /plan shows the state")
				return false
			}
			reason := strings.TrimSpace(rest)
			u.runBusy("plan", func(ctx context.Context) error {
				report, cont, err := u.agent.PlanReject(nums, reason)
				if err != nil {
					return err
				}
				u.infof("%s", report)
				if cont != "" {
					return u.agent.RunTurn(ctx, cont)
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

func (u *UI) switchSession(s *session.Session, notes []string) {
	u.agent = agent.New(u.opt.Deps, s, u.events)
	u.infof("session %s: %d messages in context", s.ID, len(s.Context))
	for _, n := range notes {
		u.infof("%s", n)
	}
	if s.Model != "" && s.Model != u.opt.Deps.Cfg.Model {
		u.warnf("this session was started with %s; continuing with %s", s.Model, u.opt.Deps.Cfg.Model)
	}
	u.showLastReply(s)
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

func (u *UI) startTurn(text string) {
	u.runBusy("turn", func(ctx context.Context) error { return u.agent.RunTurn(ctx, text) })
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
	if !u.startNextQueued() {
		u.promptLine()
	}
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
	u.write(u.style(cYellow, fmt.Sprintf("Approve? y / n [reason] / a (always %s) / v (view all): ", u.pending.Tool)))
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
		u.infof("%s will run without asking for the rest of this session (/approvals reset to undo)", a.Tool)
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
	cfg := u.opt.Deps.Cfg
	parts := []string{
		fmt.Sprintf("ctx %s/%s", textutil.KTok(u.agent.UsedTokens()), textutil.KTok(cfg.NumCtx)),
		"think " + onOff(u.agent.Think()),
	}
	if s := u.opt.Deps.Guard.Status(); s != "" {
		parts = append(parts, s)
	}
	if !u.warm {
		parts = append(parts, "model loading")
	}
	return u.style(cDim, "["+strings.Join(parts, " · ")+"]") + " " + u.style(cBold, "› ")
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
	u.line(u.style(cBold, "Clauzette - Flush your thoughts.") + u.style(cDim, fmt.Sprintf(" · %s @ %s · workspace %s", cfg.Model, cfg.OllamaURL, cfg.Workspace)))
	if u.opt.Resumed {
		u.infof("resumed session %s: %d messages in context", s.ID, len(s.Context))
		if s.Model != "" && s.Model != cfg.Model {
			u.warnf("this session was started with %s; continuing with %s", s.Model, cfg.Model)
		}
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
  /compact          compact the context now
  /undo  /retry     drop the last exchange (and resend it)
  /sessions  /new  /load <id>
  /system [reload]  show or rebuild the system prompt
  /gpu              GPU guard status
  /approvals [reset]
  /safe [restore <n>]  safe mode status and backups, restore a backup
  /plan             pending actions from plan mode
  /plan approve [n ...|all]    run the pending actions in order
  /plan reject [n ...|all] [reason]  discard them and tell the model why
  /help  /exit
Input:
  """ on its own line starts and ends a multi-line message.
  Messages typed while the agent works are queued.
  Ctrl+D, Ctrl+C or a dropped connection save the session and exit.
Approvals:
  y = yes, n [reason] = no, a = always for this tool in this session, v = show the full diff`)
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

// parsePlanNumbers turns "/plan approve" arguments into 1-based pending
// numbers. Empty or "all" selects everything (nil).
func parsePlanNumbers(args []string, total int) ([]int, error) {
	if len(args) == 0 || len(args) == 1 && strings.EqualFold(args[0], "all") {
		return nil, nil
	}
	var out []int
	for _, s := range args {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > total {
			return nil, fmt.Errorf("invalid action number %q (1-%d, or \"all\")", s, total)
		}
		out = append(out, n)
	}
	return out, nil
}

// splitPlanNumbers handles "/plan reject n ... [reason]": leading numbers
// are the selection, the rest is the reason sent to the model.
func splitPlanNumbers(args []string) ([]int, string, error) {
	if len(args) == 0 || strings.EqualFold(args[0], "all") {
		return nil, strings.TrimSpace(strings.Join(args[1:], " ")), nil
	}
	var nums []int
	i := 0
	for i < len(args) {
		n, err := strconv.Atoi(args[i])
		if err != nil {
			break
		}
		nums = append(nums, n)
		i++
	}
	if len(nums) == 0 {
		return nil, "", fmt.Errorf("invalid action number %q (1-N, or \"all\")", args[0])
	}
	return nums, strings.TrimSpace(strings.Join(args[i:], " ")), nil
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
