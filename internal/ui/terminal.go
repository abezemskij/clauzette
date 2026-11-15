// Package ui is the terminal front end. One goroutine reads input lines,
// the agent runs in another, and the main loop here multiplexes input,
// agent events, timers and signals, so the terminal is only ever written
// from one place. What the commands do lives in package control; this
// package reads the terminal, and draws what the controller reports (it
// implements control.View).
package ui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"clauzette/internal/agent"
	"clauzette/internal/config"
	"clauzette/internal/control"
	"clauzette/internal/session"
	"clauzette/internal/textutil"
	"clauzette/internal/wire"
)

// Options are the controller's options.
type Options = control.Options

type inputLine struct {
	text    string
	literal bool // a """ block, or a message that begins with pasted text: never interpreted as a command
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
	cItalic = "\x1b[3m"
	cUnder  = "\x1b[4m"
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
	c      *control.Controller
	status func() control.Status // what the prompt and banner show
	color  bool
	tty    bool
	lines  chan inputLine

	atPrompt bool
	stream   int

	waitText  string
	waitSince time.Time
	waitTick  *time.Ticker
	waitC     <-chan time.Time
	md        *mdRenderer // renders the model's replies (plain without colour or with markdown off)

	in  io.Reader // os.Stdin; tests replace it
	out io.Writer // os.Stdout; tests replace it

	// attached to a server (clauzette attach) instead of c
	remote       *wire.Client
	st           control.Status         // the latest status the server sent
	lastApproval *agent.ApprovalRequest // the approval request shown last, to ask again
}

// New is the terminal running the session in this process.
func New(opt Options, sess *session.Session) *UI {
	u := newTerminal(opt.Deps.Cfg)
	u.c = control.New(opt, sess, u)
	u.status = u.c.Status
	return u
}

// newTerminal sets up input, output and colour.
func newTerminal(cfg *config.Config) *UI {
	u := &UI{
		lines: make(chan inputLine),
		tty:   isTerminal(os.Stdout),
		in:    os.Stdin,
		out:   os.Stdout,
	}
	switch cfg.Color {
	case "always":
		u.color = true
	case "never":
		u.color = false
	default:
		u.color = u.tty && os.Getenv("NO_COLOR") == ""
	}
	u.md = newMarkdown(u.write, !u.color || !cfg.Markdown)
	return u
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Run is the main loop. It returns when the operator quits.
func (u *UI) Run(ctx context.Context) error {
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

	defer u.c.Start(ctx)()

	u.promptLine()
	for {
		select {
		case in, ok := <-u.lines:
			if !ok {
				u.atPrompt = false
				u.write("\n")
				u.c.Quit()
				return nil
			}
			u.atPrompt = false
			if u.c.Handle(control.Line{Text: in.text, Literal: in.literal}) {
				return nil
			}
		case ev := <-u.c.Events():
			u.c.HandleEvent(ev)
		case err := <-u.c.Done():
			u.c.Finish(err)
		case <-u.c.WarmDone():
			u.c.Warmed()
		case <-u.waitC:
			u.drawWaiting()
		case s := <-sigs:
			u.atPrompt = false
			u.write("\n")
			u.warnf("received %s; saving and exiting", s)
			u.c.Quit()
			return nil
		}
	}
}

// readInput sends complete messages (or """ blocks) to the main loop. A
// message ends at an Enter typed outside a bracketed paste (pasteStart..
// pasteEnd, enabled in Run on a terminal), so text typed around a paste,
// such as "download this image <pasted URL>", is one message, and a
// multi-line paste is sent on the next Enter rather than split per line.
// A message that begins with pasted text is literal: a pasted "/..." or
// "!..." is never run as a command. Typed text before a paste is read as
// usual, so "/image <pasted path>" works.
func (u *UI) readInput() { u.readFrom(u.in) }

// readFrom is readInput's body over any reader, so the paste/""" parsing can be
// tested without os.Stdin.
func (u *UI) readFrom(in io.Reader) {
	r := bufio.NewReader(in)
	var block []string // lines of an open """ block
	inBlock := false
	pasting := false

	// The message being assembled: typed and pasted fragments in order.
	var cur strings.Builder
	pasted := false // it contains pasted text
	first := ""     // "typed" or "pasted": what its first non-blank text was
	add := func(s, kind string) {
		if s == "" {
			return
		}
		if kind == "pasted" {
			pasted = true
		}
		if first == "" && strings.TrimSpace(s) != "" {
			first = kind
		}
		cur.WriteString(s)
	}
	// finish ends the message at a typed Enter (or the end of input).
	finish := func() {
		text := cur.String()
		hadPaste, literal := pasted, first == "pasted"
		cur.Reset()
		pasted, first = false, ""
		switch {
		case !hadPaste && strings.TrimSpace(text) == `"""`:
			if inBlock {
				u.lines <- inputLine{text: strings.Join(block, "\n"), literal: true}
				block = nil
			}
			inBlock = !inBlock
		case inBlock:
			block = append(block, strings.Split(text, "\n")...)
		case hadPaste && strings.TrimSpace(text) == "":
			// an empty paste and nothing typed: not a message
		default:
			u.lines <- inputLine{text: text, literal: literal}
		}
	}

	for {
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			if cur.Len() > 0 || pasted {
				finish()
			}
			close(u.lines)
			return
		}
		ended := strings.HasSuffix(line, "\n")
		line = strings.TrimRight(line, "\r\n")

		// A physical line may hold paste markers mixed with typed text; walk it
		// in fragments.
		remaining := line
		for {
			if pasting {
				if j := strings.Index(remaining, pasteEnd); j >= 0 {
					add(remaining[:j], "pasted")
					remaining = remaining[j+len(pasteEnd):]
					pasting = false
					continue
				}
				add(remaining, "pasted")
				break // the rest of the paste arrives on later reads
			}
			if i := strings.Index(remaining, pasteStart); i >= 0 {
				add(remaining[:i], "typed")
				remaining = remaining[i+len(pasteStart):]
				pasting = true
				pasted = true
				continue
			}
			add(remaining, "typed")
			break
		}
		switch {
		case pasting && ended:
			cur.WriteString("\n") // a line break inside the paste
		case ended || err != nil:
			finish()
		}

		if err != nil {
			close(u.lines)
			return
		}
	}
}

// ------------------------------------------------------------- approvals

// ShowApproval shows an approval request with its preview, then asks.
func (u *UI) ShowApproval(a *agent.ApprovalRequest, full bool) {
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
	u.AskApproval(a)
}

// AskApproval asks for the answer to a shown approval request.
func (u *UI) AskApproval(a *agent.ApprovalRequest) {
	always := fmt.Sprintf("always %s", a.Tool)
	if a.AllowScope != "" {
		always = fmt.Sprintf("allowlist %s", a.AllowScope)
	}
	u.write(u.style(cYellow, fmt.Sprintf("Approve? y / n [reason] / a (%s) / v (view all): ", always)))
}

// -------------------------------------------------------------- rendering

// Render draws an agent event.
func (u *UI) Render(ev agent.Event) {
	switch ev.Kind {
	case agent.EvWaiting:
		u.startWaiting(ev.Text)
	case agent.EvWaitingDone:
		u.stopWaiting()
	case agent.EvThinking:
		if !u.status().ShowThinking {
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
		u.write(u.style(cDim, sanitize(ev.Text)))
	case agent.EvText:
		if u.stream != streamText {
			u.endStream()
			u.stream = streamText
		}
		u.md.Write(ev.Text)
	case agent.EvStreamEnd:
		u.endStream()
		u.md.Reset()
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
	if u.stream == streamText {
		u.md.Flush()
	}
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

func (u *UI) write(s string) { _, _ = io.WriteString(u.out, s) }

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
	st := u.status()
	think := "think " + onOff(st.Think)
	if !st.ThinkAvailable {
		think = "think n/a"
	}
	parts := []string{
		fmt.Sprintf("ctx %s/%s", textutil.KTok(st.UsedTokens), textutil.KTok(st.Window)),
		think,
	}
	if st.ChatOnly {
		parts = append(parts, "chat only")
	}
	if st.Cloud != "" {
		parts = append(parts, "cloud: "+st.Cloud)
	}
	if st.GPU != "" {
		parts = append(parts, st.GPU)
	}
	if !st.Warm {
		parts = append(parts, "model loading")
	}
	mode := ""
	if st.Rogue {
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
	st := u.status()
	u.line(u.style(cBold, "Clauzette - Flush your thoughts.") + u.style(cDim, fmt.Sprintf(" · %s @ %s · workspace %s", st.Model, st.Server, st.Workspace)))
	u.c.Intro()
	u.infof("!help lists the commands. Messages typed before the model is ready are queued.")
}

func (u *UI) help() { u.block(control.HelpText) }

// ---------------------------------------------------------- control.View

func (u *UI) Info(msg string)   { u.line(u.style(cDim, "· "+msg)) }
func (u *UI) Warn(msg string)   { u.line(u.style(cYellow, "! "+msg)) }
func (u *UI) Error(msg string)  { u.line(u.style(cRed, "✗ "+msg)) }
func (u *UI) Block(text string) { u.block(text) }
func (u *UI) Dim(line string)   { u.line(u.style(cDim, line)) }
func (u *UI) Prompt()           { u.promptLine() }
func (u *UI) RefreshPrompt()    { u.refreshPrompt() }
func (u *UI) Working()          { u.atPrompt = false }
func (u *UI) StopWaiting()      { u.stopWaiting() }
func (u *UI) EndStream()        { u.endStream() }

// Echo shows a queued message as it is sent.
func (u *UI) Echo(text string) {
	if u.atPrompt && u.tty {
		u.write("\r\x1b[2K")
	}
	u.atPrompt = false
	u.line(u.style(cDim, "› "+textutil.OneLine(text, 100)))
}

// Command handles the commands only the terminal has: help and /markdown.
func (u *UI) Command(name string, args []string, raw string) bool {
	switch name {
	case "!help", "!h", "!?", "/help":
		u.help()
	case "/markdown":
		on, ok := control.ParseToggle(strings.ToLower(strings.Join(args, "")), !u.md.plain)
		switch {
		case !ok:
			u.warnf("usage: /markdown [on|off]")
		case on && !u.color:
			u.warnf("markdown rendering needs colour (color is off, or NO_COLOR is set, or the output is not a terminal)")
		default:
			u.md.plain = !on
			u.infof("markdown rendering %s", onOff(on))
		}
	default:
		return false
	}
	return true
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}
