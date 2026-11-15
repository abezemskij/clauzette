package server

import (
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"clauzette/internal/agent"
	"clauzette/internal/wire"
)

func bare() *Server { return &Server{subs: map[chan wire.Record]struct{}{}} }

func TestReplay(t *testing.T) {
	s := bare()
	for i := 0; i < 10; i++ {
		s.publish(wire.Record{Kind: wire.KInfo})
	}
	back, ch := s.subscribe(7, 0)
	if len(back) != 3 || back[0].Seq != 8 || back[2].Seq != 10 {
		t.Fatalf("since 7: %+v", back)
	}
	s.publish(wire.Record{Kind: wire.KDim, Text: "live"})
	if r := <-ch; r.Seq != 11 || r.Text != "live" {
		t.Fatalf("live record %+v", r)
	}
	s.unsubscribe(ch)
	if back, _ := s.subscribe(0, 4); len(back) != 4 || back[0].Seq != 8 {
		t.Fatalf("tail 4: %+v", back)
	}
}

func TestReplayGap(t *testing.T) {
	s := bare()
	for i := 0; i < keep+50; i++ {
		s.publish(wire.Record{Kind: wire.KInfo})
	}
	back, _ := s.subscribe(10, 0)
	if back[0].Kind != wire.KGap || back[0].Text != "40" || back[1].Seq != 51 {
		t.Fatalf("gap: %+v %+v", back[0], back[1])
	}
	if len(s.ring) != keep {
		t.Fatalf("ring holds %d", len(s.ring))
	}
}

func TestSlowClientDropped(t *testing.T) {
	s := bare()
	_, ch := s.subscribe(0, 0)
	for i := 0; i < 2000; i++ { // more than the channel holds
		s.publish(wire.Record{Kind: wire.KInfo})
	}
	n := 0
	for range ch { // closed once it fell behind
		n++
	}
	if n == 0 || n >= 2000 || len(s.subs) != 0 {
		t.Fatalf("got %d records, %d subscribers left", n, len(s.subs))
	}
}

func TestExitCommandAndLoopback(t *testing.T) {
	for in, want := range map[wire.Input]bool{
		{Text: "/exit"}: true, {Text: " !Q "}: true, {Text: "/quit"}: true,
		{Text: "/exit", Literal: true}: false, {Text: "/exits"}: false, {Text: "hello"}: false,
	} {
		if exitCommand(in) != want {
			t.Errorf("%+v", in)
		}
	}
	for addr, want := range map[string]bool{"127.0.0.1:8484": true, "localhost:1": true, "[::1]:1": true, "0.0.0.0:8484": false, ":8484": false, "10.0.0.5:1": false} {
		if IsLoopback(addr) != want {
			t.Errorf("%s", addr)
		}
	}
}

func TestWebPage(t *testing.T) {
	s := bare()
	s.token = []byte("tok")
	s.Web = true
	h := s.Handler()
	get := func(path, tok string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	for path, typ := range map[string]string{"/": "text/html", "/app.js": "text/javascript", "/app.css": "text/css"} {
		w := get(path, "") // the page needs no token
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), typ) {
			t.Fatalf("%s: %d %s", path, w.Code, w.Header().Get("Content-Type"))
		}
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") || w.Header().Get("X-Frame-Options") != "DENY" {
			t.Fatalf("%s: headers %v", path, w.Header())
		}
	}
	if w := get("/api/events", ""); w.Code != 401 {
		t.Fatalf("the API without a token: %d", w.Code)
	}
	if w := get("/secret.txt", ""); w.Code != 404 {
		t.Fatalf("an unknown path: %d", w.Code)
	}
	s.Web = false
	h = s.Handler()
	if w := get("/", ""); w.Code != 404 {
		t.Fatalf("the page with web off: %d", w.Code)
	}
}

// The page draws records by agent.EventKind number; the numbers in app.js
// must match the Go constants.
func TestWebEventKinds(t *testing.T) {
	js, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]agent.EventKind{"Text": agent.EvText, "Thinking": agent.EvThinking, "StreamEnd": agent.EvStreamEnd,
		"Waiting": agent.EvWaiting, "WaitingDone": agent.EvWaitingDone, "ToolCall": agent.EvToolCall, "ToolResult": agent.EvToolResult,
		"Approval": agent.EvApproval, "Info": agent.EvInfo, "Warn": agent.EvWarn, "Error": agent.EvError, "Usage": agent.EvUsage}
	m := regexp.MustCompile(`const Ev = \{([^}]*)\}`).FindSubmatch(js)
	if m == nil {
		t.Fatal("no Ev table in app.js")
	}
	got := map[string]agent.EventKind{}
	for _, kv := range regexp.MustCompile(`(\w+): (\d+)`).FindAllSubmatch(m[1], -1) {
		n, _ := strconv.Atoi(string(kv[2]))
		got[string(kv[1])] = agent.EventKind(n)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("app.js Ev %v, Go %v", got, want)
	}
	if regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|eval\(`).Match(js) {
		t.Fatal("app.js inserts HTML: model output must only go into textContent")
	}
}

func TestUploadName(t *testing.T) {
	for in, want := range map[string]string{
		"shot.png":                  "shot.png",
		"../../etc/passwd":          "passwd",
		`C:\Users\me\report v2.pdf`: "report_v2.pdf",
		".bashrc":                   "bashrc",
		"résumé (final).docx":       "r_sum_final_.docx",
		"":                          "upload",
		"...":                       "upload",
		"a;rm -rf ~;b.sh":           "a_rm_-rf_b.sh",
	} {
		if got := uploadName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if got := uploadName(strings.Repeat("x", 300) + ".tar.gz"); len(got) != 100 || !strings.HasSuffix(got, ".gz") {
		t.Errorf("long name: %d %q", len(got), got)
	}
}
