// Package server is clauzette serve: the controller of one session (as in
// the terminal: /new and /load switch it) behind a small HTTP API, so the
// work keeps running when a client disconnects and several clients can
// watch it. The protocol is in package wire.
//
// One goroutine (Run) owns the controller, as the terminal's main loop
// does; HTTP handlers only pass input in and read published records.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"clauzette/internal/agent"
	"clauzette/internal/control"
	"clauzette/internal/session"
	"clauzette/internal/wire"
)

// keep is how many records the server holds for clients that reconnect.
const keep = 5000

type Server struct {
	// Web serves the web page at / (on by default in clauzette serve).
	Web bool
	// UploadMax is the largest upload in bytes (0: DefaultUploadMax).
	UploadMax int64

	opt   control.Options
	calls chan func() // run on the controller's goroutine

	c        *control.Controller
	token    []byte
	input    chan wire.Input
	stateReq chan chan control.Status
	audit    *agent.Audit

	mu   sync.Mutex
	seq  int64
	ring []wire.Record
	subs map[chan wire.Record]struct{}
}

func New(opt control.Options, sess *session.Session, token string) *Server {
	s := &Server{
		token:    []byte(token),
		input:    make(chan wire.Input),
		stateReq: make(chan chan control.Status),
		calls:    make(chan func()),
		opt:      opt,
		subs:     map[chan wire.Record]struct{}{},
		audit:    opt.Deps.Audit,
	}
	s.c = control.New(opt, sess, s)
	return s
}

// Run serves on ln until ctx ends, then stops the work, saves the session
// and releases it.
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	hs := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()

	defer s.c.Start(ctx)()
	s.c.Intro()
	s.Info("!help lists the commands. Messages typed before the model is ready are queued.")
	s.Prompt()
	for {
		select {
		case in := <-s.input:
			if exitCommand(in) {
				s.Info("the server keeps the session running; detach your client (/exit there, or Ctrl+D), or stop the server to end it")
				s.afterLocal()
				continue
			}
			s.c.Handle(control.Line{Text: in.Text, Literal: in.Literal})
		case ev := <-s.c.Events():
			s.c.HandleEvent(ev)
		case err := <-s.c.Done():
			s.c.Finish(err)
		case <-s.c.WarmDone():
			s.c.Warmed()
		case reply := <-s.stateReq:
			reply <- s.c.Status()
		case fn := <-s.calls:
			fn()
		case err := <-served:
			s.c.Quit()
			return err
		case <-ctx.Done():
			s.c.Quit()
			shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			hs.Shutdown(shut) // ends the event streams
			return nil
		}
	}
}

// exitCommand reports /exit, /quit and !q: they would end the session for
// every client, so the server does not take them from one.
func exitCommand(in wire.Input) bool {
	if in.Literal {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(in.Text)) {
	case "/exit", "/quit", "!q":
		return true
	}
	return false
}

// afterLocal shows the prompt again after the server answered input itself.
func (s *Server) afterLocal() {
	if st := s.c.Status(); !st.Busy && !st.Approval {
		s.Prompt()
	}
}

// ------------------------------------------------------------- records

func (s *Server) publish(r wire.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	r.Seq = s.seq
	s.ring = append(s.ring, r)
	if len(s.ring) > keep {
		s.ring = append([]wire.Record(nil), s.ring[len(s.ring)-keep:]...)
	}
	for ch := range s.subs {
		select {
		case ch <- r:
		default:
			// a client that does not keep up is dropped; it reconnects from its last record
			delete(s.subs, ch)
			close(ch)
		}
	}
}

// subscribe returns the kept records after since (or the last tail when
// since is 0) and a channel with every later one.
func (s *Server) subscribe(since int64, tail int) ([]wire.Record, chan wire.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var back []wire.Record
	switch {
	case since > 0:
		if len(s.ring) > 0 && s.ring[0].Seq > since+1 {
			back = append(back, wire.Record{Seq: s.ring[0].Seq - 1, Kind: wire.KGap, Text: strconv.FormatInt(s.ring[0].Seq-since-1, 10)})
		}
		for _, r := range s.ring {
			if r.Seq > since {
				back = append(back, r)
			}
		}
	case tail > 0:
		i := len(s.ring) - tail
		if i < 0 {
			i = 0
		}
		back = append(back, s.ring[i:]...)
	default:
		back = append(back, s.ring...)
	}
	ch := make(chan wire.Record, 1024)
	s.subs[ch] = struct{}{}
	return back, ch
}

func (s *Server) unsubscribe(ch chan wire.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[ch]; ok {
		delete(s.subs, ch)
		close(ch)
	}
}

func (s *Server) status() *control.Status { st := s.c.Status(); return &st }

// ------------------------------------------------------------ the View

func (s *Server) Render(ev agent.Event) {
	s.publish(wire.Record{Kind: wire.KRender, Event: wire.FromEvent(ev)})
}
func (s *Server) Info(msg string) {
	s.publish(wire.Record{Kind: wire.KInfo, Text: msg, Status: s.status()})
}
func (s *Server) Warn(msg string) {
	s.publish(wire.Record{Kind: wire.KWarn, Text: msg, Status: s.status()})
}
func (s *Server) Error(msg string)  { s.publish(wire.Record{Kind: wire.KError, Text: msg}) }
func (s *Server) Block(text string) { s.publish(wire.Record{Kind: wire.KBlock, Text: text}) }
func (s *Server) Dim(line string)   { s.publish(wire.Record{Kind: wire.KDim, Text: line}) }
func (s *Server) Echo(text string)  { s.publish(wire.Record{Kind: wire.KEcho, Text: text}) }
func (s *Server) ShowApproval(req *agent.ApprovalRequest, full bool) {
	s.publish(wire.Record{Kind: wire.KApproval, Approval: wire.FromApproval(req), Full: full})
}
func (s *Server) AskApproval(req *agent.ApprovalRequest) {
	s.publish(wire.Record{Kind: wire.KAsk, Approval: wire.FromApproval(req)})
}
func (s *Server) Prompt()        { s.publish(wire.Record{Kind: wire.KPrompt, Status: s.status()}) }
func (s *Server) RefreshPrompt() { s.publish(wire.Record{Kind: wire.KRefresh, Status: s.status()}) }
func (s *Server) Working()       { s.publish(wire.Record{Kind: wire.KWorking, Status: s.status()}) }
func (s *Server) StopWaiting()   { s.publish(wire.Record{Kind: wire.KStopWaiting}) }
func (s *Server) EndStream()     { s.publish(wire.Record{Kind: wire.KEndStream}) }

// Command answers the commands that belong to each client.
func (s *Server) Command(name string, args []string, raw string) bool {
	switch name {
	case "!help", "!h", "!?", "/help":
		s.Block(control.HelpText)
	case "/markdown":
		s.Info("/markdown is a setting of each client; clauzette attach handles it itself")
	default:
		return false
	}
	return true
}

// ---------------------------------------------------------------- HTTP

// Handler is the HTTP API (see package wire), behind the token, and the
// web page when Web is set.
func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.handleState)
	api.HandleFunc("POST /api/input", s.handleInput)
	api.HandleFunc("GET /api/events", s.handleEvents)
	api.HandleFunc("GET /api/sessions", s.handleSessions)
	api.HandleFunc("GET /api/models", s.handleModels)
	api.HandleFunc("GET /api/todos", s.handleTodos)
	api.HandleFunc("GET /api/plan", s.handlePlan)
	api.HandleFunc("GET /api/files", s.handleFiles)
	api.HandleFunc("GET /api/file", s.handleFile)
	api.HandleFunc("GET /api/diff", s.handleDiff)
	api.HandleFunc("POST /api/upload", s.handleUpload)
	mux := http.NewServeMux()
	mux.Handle("/api/", s.auth(api))
	if s.Web {
		mux.HandleFunc("GET /{$}", webFile("index.html", "text/html; charset=utf-8"))
		mux.HandleFunc("GET /app.js", webFile("app.js", "text/javascript; charset=utf-8"))
		mux.HandleFunc("GET /app.css", webFile("app.css", "text/css; charset=utf-8"))
	}
	return mux
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || len(s.token) == 0 || subtle.ConstantTimeCompare([]byte(got), s.token) != 1 {
			s.audit.Log(map[string]any{"event": "server_unauthorized", "remote": r.RemoteAddr, "path": r.URL.Path})
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	reply := make(chan control.Status, 1)
	select {
	case s.stateReq <- reply:
	case <-r.Context().Done():
		return
	}
	select {
	case st := <-reply:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(st)
	case <-r.Context().Done():
	}
}

func (s *Server) handleInput(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, wire.MaxInput+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > wire.MaxInput {
		http.Error(w, "input too large", http.StatusRequestEntityTooLarge)
		return
	}
	var in wire.Input
	if err := json.Unmarshal(body, &in); err != nil {
		http.Error(w, "bad input: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.audit.Log(map[string]any{"event": "server_input", "remote": r.RemoteAddr, "chars": len(in.Text)})
	select {
	case s.input <- in:
		w.WriteHeader(http.StatusAccepted)
	case <-r.Context().Done():
	case <-time.After(30 * time.Second):
		http.Error(w, "the server is not taking input", http.StatusServiceUnavailable)
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		since, _ = strconv.ParseInt(v, 10, 64)
	}
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	back, ch := s.subscribe(since, tail)
	defer s.unsubscribe(ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	send := func(rec wire.Record) error {
		b, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", rec.Seq, b)
		return err
	}
	for _, rec := range back {
		if send(rec) != nil {
			return
		}
	}
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case rec, ok := <-ch:
			if !ok {
				return // dropped for falling behind; the client reconnects
			}
			if send(rec) != nil {
				return
			}
			fl.Flush()
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// IsLoopback reports whether a listen address only accepts local
// connections.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var _ control.View = (*Server)(nil)
