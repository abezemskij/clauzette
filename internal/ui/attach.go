package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"clauzette/internal/config"
	"clauzette/internal/control"
	"clauzette/internal/wire"
)

// replayTail is how many records a client shows from before it attached.
const replayTail = 200

// NewRemote is the terminal for clauzette attach: the same input, paste
// handling and drawing as New, with a server in place of the in-process
// controller.
func NewRemote(cfg *config.Config, cl *wire.Client) *UI {
	u := newTerminal(cfg)
	u.remote = cl
	u.status = func() control.Status { return u.st }
	return u
}

// RunRemote attaches to the server until the operator detaches (/exit,
// !q, Ctrl+D, a signal). The session keeps running in the server.
func (u *UI) RunRemote(ctx context.Context) error {
	st, err := u.remote.State(ctx)
	if err != nil {
		return fmt.Errorf("attaching to %s: %w", u.remote.Base, err)
	}
	u.st = st
	if u.tty && isTerminal(os.Stdin) {
		u.write(pasteOn)
		defer u.write(pasteOff)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	go u.readInput()
	u.line(u.style(cBold, "Clauzette - attached to "+u.remote.Base) + u.style(cDim, fmt.Sprintf(" · %s @ %s · workspace %s", st.Model, st.Server, st.Workspace)))
	u.infof("session %s; /exit, !q or Ctrl+D detach, and the work keeps running in the server", st.Session)

	sctx, stop := context.WithCancel(ctx)
	defer stop()
	recs := make(chan wire.Record, 256)
	go u.readRecords(sctx, recs)

	for {
		select {
		case in, ok := <-u.lines:
			if !ok {
				u.atPrompt = false
				u.write("\n")
				u.detach()
				return nil
			}
			u.atPrompt = false
			if u.remoteInput(ctx, in) {
				return nil
			}
		case r := <-recs:
			if r.Kind == "fatal" {
				u.atPrompt = false
				u.endStream()
				return errors.New(r.Text)
			}
			u.apply(r)
		case <-u.waitC:
			u.drawWaiting()
		case s := <-sigs:
			u.atPrompt = false
			u.write("\n")
			u.warnf("received %s", s)
			u.detach()
			return nil
		}
	}
}

// readRecords reads the server's records, reconnecting from the last one seen
// whenever the connection drops.
func (u *UI) readRecords(ctx context.Context, recs chan<- wire.Record) {
	var last int64
	failures := 0
	deliver := func(r wire.Record) bool {
		select {
		case recs <- r:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for ctx.Err() == nil {
		tail := 0
		if last == 0 {
			tail = replayTail
		}
		got := false
		err := u.remote.Events(ctx, last, tail, func(r wire.Record) {
			if !got && failures > 0 {
				deliver(wire.Record{Kind: wire.KInfo, Text: "reconnected"})
			}
			got = true
			last = r.Seq
			deliver(r)
		})
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, wire.ErrUnauthorized) {
			deliver(wire.Record{Kind: "fatal", Text: err.Error()})
			return
		}
		if got {
			failures = 0
		}
		failures++
		if failures == 1 {
			deliver(wire.Record{Kind: wire.KWarn, Text: "the connection to the server was lost; reconnecting…"})
		}
		select {
		case <-time.After(wire.Backoff(failures)):
		case <-ctx.Done():
			return
		}
	}
}

// remoteInput sends a line to the server, or handles it here: detaching
// and the client's own commands. It reports whether to stop.
func (u *UI) remoteInput(ctx context.Context, in inputLine) bool {
	text := strings.TrimSpace(in.text)
	if !in.literal && text != "" {
		f := strings.Fields(text)
		name := strings.ToLower(f[0])
		switch name {
		case "/exit", "/quit", "!q":
			u.detach()
			return true
		case "/help", "!help", "!h", "!?", "/markdown":
			u.Command(name, f[1:], text)
			u.afterClientCommand()
			return false
		}
	}
	sctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	if err := u.remote.Send(sctx, wire.Input{Text: in.text, Literal: in.literal}); err != nil {
		u.errorf("not sent: %v", err)
		u.afterClientCommand()
	}
	return false
}

// afterClientCommand asks again or shows the prompt, as the controller does
// after a command.
func (u *UI) afterClientCommand() {
	switch {
	case u.st.Approval && u.lastApproval != nil:
		u.AskApproval(u.lastApproval)
	case !u.st.Busy:
		u.promptLine()
	}
}

// apply draws one record from the server.
func (u *UI) apply(r wire.Record) {
	if r.Status != nil {
		u.st = *r.Status
	}
	switch r.Kind {
	case wire.KInfo:
		u.Info(r.Text)
	case wire.KWarn:
		u.Warn(r.Text)
	case wire.KError:
		u.Error(r.Text)
	case wire.KBlock:
		u.Block(r.Text)
	case wire.KDim:
		u.Dim(r.Text)
	case wire.KEcho:
		u.Echo(r.Text)
	case wire.KRender:
		if r.Event != nil {
			u.Render(r.Event.Agent())
		}
	case wire.KApproval, wire.KAsk:
		if r.Approval == nil {
			return
		}
		a := r.Approval.Agent()
		u.lastApproval = a
		u.st.Approval = true
		if r.Kind == wire.KApproval {
			u.ShowApproval(a, r.Full)
		} else {
			u.AskApproval(a)
		}
	case wire.KPrompt:
		u.Prompt()
	case wire.KRefresh:
		u.RefreshPrompt()
	case wire.KWorking:
		u.Working()
	case wire.KStopWaiting:
		u.StopWaiting()
	case wire.KEndStream:
		u.EndStream()
	case wire.KGap:
		u.Warn(fmt.Sprintf("%s earlier records were dropped while this client was away", r.Text))
	}
}

func (u *UI) detach() {
	u.stopWaiting()
	u.endStream()
	u.infof("detached; session %s keeps running in the server (clauzette attach to come back)", u.st.Session)
}
