package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clauzette/internal/config"
	"clauzette/internal/modeldb"
	"clauzette/internal/ollama"
	"clauzette/internal/session"
	"clauzette/internal/tools"
)

// fakeServer is an Ollama with several installed models. It records loads
// (chat requests without messages), unloads (keep_alive 0) and real chat
// requests separately, so tests can check what happened in which order.
type fakeServer struct {
	mu       sync.Mutex
	events   []string // "load big@131072", "unload big", "chat big"
	chats    []ollama.ChatRequest
	failLoad map[string]bool
	reply    func(req ollama.ChatRequest) ollama.Message
}

var fakeTags = `{"models": [
	{"name": "big:latest",   "size": 17700000000, "capabilities": ["completion","tools","thinking"], "details": {"parameter_size": "27.3B", "quantization_level": "Q4_K_M", "context_length": 262144}},
	{"name": "big-65k:latest", "size": 17700000000, "capabilities": ["completion","tools","thinking"], "details": {"context_length": 262144}},
	{"name": "small:latest", "size": 2500000000, "capabilities": ["completion","tools","thinking"], "details": {"context_length": 262144}},
	{"name": "phi:latest",   "size": 2200000000, "capabilities": ["completion"], "details": {"context_length": 131072}},
	{"name": "spare:latest", "size": 6600000000, "capabilities": ["completion","tools"], "details": {"parameter_size": "9.7B", "context_length": 262144}}
]}`

var fakeWeights = map[string]string{
	"big": "aaaa", "big:latest": "aaaa", "big-65k:latest": "aaaa", // same weights
	"small": "bbbb", "small:latest": "bbbb", "phi": "cccc", "phi:latest": "cccc", "spare:latest": "dddd",
}

func (f *fakeServer) log(s string) {
	f.mu.Lock()
	f.events = append(f.events, s)
	f.mu.Unlock()
}

func (f *fakeServer) eventList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/tags":
		w.Write([]byte(fakeTags))
	case "/api/ps":
		w.Write([]byte(`{"models": []}`))
	case "/api/show":
		var in struct{ Name string }
		json.NewDecoder(r.Body).Decode(&in)
		d := strings.Repeat(fakeWeights[in.Name], 16)
		json.NewEncoder(w).Encode(map[string]any{"modelfile": "FROM /blobs/sha256-" + d + "\n", "model_info": map[string]any{}})
	case "/api/chat":
		var req ollama.ChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) == 0 {
			if req.KeepAlive == "0s" {
				f.log("unload " + req.Model)
				w.Write([]byte(`{}`))
				return
			}
			if f.failLoad[req.Model] {
				w.Write([]byte(`{"error": "out of memory"}`))
				return
			}
			f.log(fmt.Sprintf("load %s@%v", req.Model, req.Options["num_ctx"]))
			w.Write([]byte(`{"done": true}`))
			return
		}
		f.log("chat " + req.Model)
		f.mu.Lock()
		f.chats = append(f.chats, req)
		f.mu.Unlock()
		msg := ollama.Message{Role: "assistant", Content: "ok"}
		if f.reply != nil {
			msg = f.reply(req)
		}
		json.NewEncoder(w).Encode(map[string]any{"message": msg, "done": true, "done_reason": "stop", "prompt_eval_count": 1, "eval_count": 1})
	default:
		http.NotFound(w, r)
	}
}

type switchRig struct {
	a   *Agent
	srv *fakeServer
}

func (r *switchRig) prompts(date string) (Prompts, []string, error) {
	act := r.a.Models.Active()
	return Prompts{
		Normal: fmt.Sprintf("NORMAL %s window %d", act.Model, act.NumCtx),
		Rogue:  "ROGUE",
		Chat:   fmt.Sprintf("CHAT %s", act.Model),
	}, nil, nil
}

func newSwitchRig(t *testing.T) *switchRig {
	t.Helper()
	srvState := &fakeServer{failLoad: map[string]bool{}}
	srv := httptest.NewServer(srvState)
	t.Cleanup(srv.Close)
	cfg := config.Defaults()
	cfg.Profiles = []config.Profile{
		{Name: "big", Model: "big", NumCtx: 131072, Think: "on"},
		{Name: "small", Model: "small", NumCtx: 16384, Think: "off"},
		{Name: "phi", Model: "phi", NumCtx: 8192},
	}
	ws, err := tools.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, _ := modeldb.Open(filepath.Join(t.TempDir(), "models.json"))
	client := ollama.New(srv.URL, ollama.Timeouts{Connect: time.Second, FirstToken: 10 * time.Second, InterToken: 10 * time.Second, Load: 10 * time.Second})
	models := NewModels(cfg, db, cfg.Profiles[0])
	models.Detect(context.Background(), client)
	events := make(chan Event, 64)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case ev := <-events:
				if ev.Kind == EvApproval {
					ev.Approval.Reply <- Decision{Approve: false}
				}
			case <-done:
				return
			}
		}
	}()
	sess := &session.Session{ID: "s", Profile: "big", Model: "big", Date: "Tuesday, 3 November 2026", System: "NORMAL big window 131072", Think: true}
	a := New(Deps{Cfg: cfg, Client: client, Models: models, Registry: tools.NewRegistry(&tools.WriteFile{WS: ws}), Store: store, WS: ws}, sess, events)
	return &switchRig{a: a, srv: srvState}
}

func TestSwitchModel(t *testing.T) {
	r := newSwitchRig(t)
	if err := r.a.SwitchModel(context.Background(), "small", r.prompts); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.srv.eventList(), ", "); got != "unload big, load small@16384" {
		t.Fatalf("server saw: %s", got)
	}
	act := r.a.Models.Active()
	if act.Profile != "small" || act.NumCtx != 16384 {
		t.Fatalf("active = %s %d", act.Profile, act.NumCtx)
	}
	s := r.a.Session()
	if s.Profile != "small" || s.Model != "small" {
		t.Fatalf("session not updated: %q %q", s.Profile, s.Model)
	}
	if s.System != "NORMAL small window 16384" || s.Date != "Tuesday, 3 November 2026" {
		t.Fatalf("prompts not rebuilt for the new window, or the date changed: %q %q", s.System, s.Date)
	}
	if r.a.Think() {
		t.Fatal("the new profile has thinking off; the old !think state must not carry over")
	}

	// The next turn uses the new model, its options and its prompt.
	if err := r.a.RunTurn(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	req := r.srv.chats[len(r.srv.chats)-1]
	if req.Model != "small" || req.Options["num_ctx"] != float64(16384) || req.Messages[0].Content != "NORMAL small window 16384" {
		t.Fatalf("next request: model %q num_ctx %v system %q", req.Model, req.Options["num_ctx"], req.Messages[0].Content)
	}
	last := s.Context[len(s.Context)-1]
	if last.Role != "assistant" || last.Model != "small" {
		t.Fatalf("the reply must record its model: %+v", last)
	}
}

func TestSwitchCompactsForSmallerWindow(t *testing.T) {
	r := newSwitchRig(t)
	r.srv.reply = func(ollama.ChatRequest) ollama.Message { return ollama.Message{Role: "assistant", Content: "SUMMARY"} }
	fillConversation(r.a, 20) // ~20k tokens: above small's threshold (75% of 16384)
	if err := r.a.SwitchModel(context.Background(), "small", r.prompts); err != nil {
		t.Fatal(err)
	}
	ev := r.srv.eventList()
	if len(ev) < 3 || ev[0] != "chat big" || ev[len(ev)-2] != "unload big" || ev[len(ev)-1] != "load small@16384" {
		t.Fatalf("compaction must run on the current model before the switch: %v", ev)
	}
	if r.a.Session().Context[0].Kind != session.KindSummary {
		t.Fatal("the older part should have been summarised")
	}
}

func TestSwitchRefusedWhenStillTooBig(t *testing.T) {
	r := newSwitchRig(t)
	// One message larger than the target window cannot be compacted away.
	r.a.sess.Append(ollama.Message{Role: "user", Content: strings.Repeat("x", 80000)}, "")
	err := r.a.SwitchModel(context.Background(), "small", r.prompts)
	if err == nil {
		t.Fatal("the switch should be refused")
	}
	if a := r.a.Models.Active(); a.Profile != "big" {
		t.Fatalf("active changed to %s", a.Profile)
	}
	for _, e := range r.srv.eventList() {
		if strings.HasPrefix(e, "unload") || strings.HasPrefix(e, "load") {
			t.Fatalf("nothing may be unloaded or loaded when the switch is refused: %v", r.srv.eventList())
		}
	}
}

func TestSwitchLoadFailureKeepsProfile(t *testing.T) {
	r := newSwitchRig(t)
	r.srv.failLoad["small"] = true
	err := r.a.SwitchModel(context.Background(), "small", r.prompts)
	if err == nil || !strings.Contains(err.Error(), "still using big") {
		t.Fatalf("err = %v", err)
	}
	if a := r.a.Models.Active(); a.Profile != "big" || r.a.Session().Profile != "big" {
		t.Fatalf("a failed load must leave the profile as it was: %s / %s", a.Profile, r.a.Session().Profile)
	}
}

func TestSwitchRules(t *testing.T) {
	r := newSwitchRig(t)
	r.a.rogue.Store(true)
	if err := r.a.SwitchModel(context.Background(), "small", r.prompts); err == nil || !strings.Contains(err.Error(), "rogue") {
		t.Fatalf("switch in rogue mode: %v", err)
	}
	r.a.rogue.Store(false)
	if err := r.a.SwitchModel(context.Background(), "big", r.prompts); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("switch to the active profile: %v", err)
	}
	if err := r.a.SwitchModel(context.Background(), "nothing", r.prompts); err == nil {
		t.Fatal("an unknown name was accepted")
	}
	r.a.Cfg.Profiles = append(r.a.Cfg.Profiles, config.Profile{Name: "ghost", Model: "not-pulled"})
	if err := r.a.SwitchModel(context.Background(), "ghost", r.prompts); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("a profile with a missing model: %v", err)
	}
	if len(r.srv.eventList()) != 0 {
		t.Fatalf("refused switches must not touch the server's models: %v", r.srv.eventList())
	}
}

func TestSwitchToChatOnly(t *testing.T) {
	r := newSwitchRig(t)
	r.a.SetPlan(true)
	if err := r.a.SwitchModel(context.Background(), "phi", r.prompts); err != nil {
		t.Fatal(err)
	}
	if r.a.Plan() || !r.a.ChatOnly() {
		t.Fatalf("plan %v, chat-only %v", r.a.Plan(), r.a.ChatOnly())
	}
	if r.a.Session().SystemChat != "CHAT phi" {
		t.Fatalf("chat prompt = %q", r.a.Session().SystemChat)
	}
}

func TestSwitchToPlainModel(t *testing.T) {
	r := newSwitchRig(t)
	if err := r.a.SwitchModel(context.Background(), "spare:latest", r.prompts); err != nil {
		t.Fatal(err)
	}
	act := r.a.Models.Active()
	if act.Profile != "spare:latest" || act.Model != "spare:latest" || act.NumCtx != r.a.Cfg.NumCtx || act.Caps.Thinking {
		t.Fatalf("plain model: %+v", act)
	}
	// Resuming that session uses the model again, without a "profile gone" note.
	p, notes, _ := SelectProfile(r.a.Cfg, "", r.a.Session())
	if p.Model != "spare:latest" || len(notes) != 0 {
		t.Fatalf("resume of a plain-model session: %+v %v", p, notes)
	}
}

func TestModelList(t *testing.T) {
	r := newSwitchRig(t)
	out := r.a.ModelList(context.Background())
	for _, want := range []string{
		"* big", "  small", "  phi", // profiles, the active one marked
		"chat only",                  // phi has no tools
		"Installed, no profile",      //
		"big-65k:latest",             //
		"same weights as big:latest", // the 65k variant shares big's weights
		"spare:latest",               // an unreferenced model
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("/model output is missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "small:latest") != 0 {
		t.Fatalf("a model with a profile must not be listed again:\n%s", out)
	}
	if strings.Contains(out, "(no profile)") {
		t.Fatalf("configured profiles named like their model are still profiles:\n%s", out)
	}

	// After /model with a plain model name, it is listed first, as active.
	if err := r.a.SwitchModel(context.Background(), "spare:latest", r.prompts); err != nil {
		t.Fatal(err)
	}
	out = r.a.ModelList(context.Background())
	if !strings.Contains(out, "* (no profile) spare:latest") || strings.Contains(out, "* big") {
		t.Fatalf("plain model not shown as active:\n%s", out)
	}
}
