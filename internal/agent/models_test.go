package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clauzette/internal/config"
	"clauzette/internal/modeldb"
	"clauzette/internal/ollama"
	"clauzette/internal/session"
)

func boolp(b bool) *bool { return &b }

func TestResolveDefaultsAndFallbacks(t *testing.T) {
	cfg := config.Defaults()
	cfg.Options = map[string]any{"temperature": 0.2, "top_p": 0.9}

	// Caps not detected: tools and thinking are assumed, as before profiles.
	a := resolve(cfg, config.Profile{Name: "p", Model: "m"}, Caps{}, "", nil)
	if !a.Caps.Tools || !a.Caps.Thinking || a.ChatOnly() {
		t.Fatalf("unknown caps must mean capable: %+v", a.Caps)
	}
	if a.NumCtx != cfg.NumCtx || !a.Think || a.KeepAlive != cfg.KeepAlive {
		t.Fatalf("top-level fallbacks not applied: %+v", a)
	}

	// Profile options are added to, and override, the top-level ones.
	a = resolve(cfg, config.Profile{Name: "p", Model: "m", Options: map[string]any{"temperature": 0.7}}, Caps{}, "", nil)
	o := a.RequestOptions()
	if o["temperature"] != 0.7 || o["top_p"] != 0.9 || o["num_ctx"] != cfg.NumCtx {
		t.Fatalf("options = %v", o)
	}
}

func TestResolveNumCtx(t *testing.T) {
	cfg := config.Defaults() // num_ctx 131072
	for _, tc := range []struct {
		name string
		p    config.Profile
		caps Caps
		want int
	}{
		{"explicit", config.Profile{NumCtx: 65536}, Caps{}, 65536},
		{"inherit", config.Profile{}, Caps{}, 131072},
		{"fit falls back to num_ctx", config.Profile{NumCtx: config.CtxFit}, Caps{}, 131072},
		{"fit capped by max_ctx", config.Profile{NumCtx: config.CtxFit, MaxCtx: 98304}, Caps{}, 98304},
		{"max_ctx ignored for a number", config.Profile{NumCtx: 131072, MaxCtx: 98304}, Caps{}, 131072},
		{"capped by native", config.Profile{NumCtx: 262144}, Caps{Known: true, Tools: true, NativeCtx: 131072}, 131072},
		{"caps.context override caps it", config.Profile{Caps: &config.CapsOverride{Context: 65536}}, Caps{}, 65536},
	} {
		tc.p.Name, tc.p.Model = "p", "m"
		if got := resolve(cfg, tc.p, tc.caps, "", nil).NumCtx; got != tc.want {
			t.Errorf("%s: num_ctx %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestResolveThinkChain(t *testing.T) {
	cfg := config.Defaults()
	cfg.Think = true
	cfg.Profiles = []config.Profile{{Name: "base", Model: "a", Think: "off"}, {Name: "other", Model: "b"}}
	cfg.DefaultProfile = "base"
	other, _ := cfg.FindProfile("other")
	if resolve(cfg, other, Caps{}, "", nil).Think {
		t.Fatal("a profile without think must inherit the default profile's (off)")
	}
	other.Think = "on"
	if !resolve(cfg, other, Caps{}, "", nil).Think {
		t.Fatal("the profile's own think must win")
	}
	cfg.Profiles[0].Think = ""
	other.Think = ""
	if !resolve(cfg, other, Caps{}, "", nil).Think {
		t.Fatal("with no profile setting it, the top-level think applies")
	}
}

func TestCapsOverride(t *testing.T) {
	cfg := config.Defaults()
	detected := Caps{Known: true, Tools: true, Thinking: true}
	a := resolve(cfg, config.Profile{Name: "p", Model: "m", Caps: &config.CapsOverride{Tools: boolp(false)}}, detected, "", nil)
	if !a.ChatOnly() || !a.Caps.Thinking {
		t.Fatalf("override must only change tools: %+v", a.Caps)
	}
}

// fakeOllama answers /api/tags and /api/show for Detect.
func fakeOllama(t *testing.T, tags string, modelfile string) *ollama.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Write([]byte(tags))
		case "/api/show":
			json.NewEncoder(w).Encode(map[string]any{"modelfile": modelfile, "model_info": map[string]any{"x.context_length": 131072}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return ollama.New(srv.URL, ollama.Timeouts{Connect: time.Second})
}

const testWeights = "sha256-f5f1dd8920d4aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestDetect(t *testing.T) {
	cfg := config.Defaults()
	tags := `{"models": [
		{"name": "qwen3.8:latest", "capabilities": ["completion","tools","thinking","vision"], "details": {"context_length": 262144}},
		{"name": "phi3:mini", "capabilities": ["completion"], "details": {"context_length": 131072}},
		{"name": "old:latest", "details": {}}
	]}`
	modelfile := "# Modelfile\nFROM /root/.ollama/models/blobs/" + testWeights + "\nPARAMETER num_ctx 65536\n"
	client := fakeOllama(t, tags, modelfile)

	m := NewModels(cfg, nil, config.Profile{Name: "q", Model: "qwen3.8"})
	if _, warn := m.Detect(context.Background(), client); len(warn) > 0 {
		t.Fatalf("unexpected warnings: %v", warn)
	}
	a := m.Active()
	if !a.Caps.Known || !a.Caps.Tools || !a.Caps.Thinking || !a.Caps.Vision || a.Caps.NativeCtx != 262144 {
		t.Fatalf("qwen caps = %+v", a.Caps)
	}
	if a.Key != testWeights {
		t.Fatalf("key = %q, want the weights digest", a.Key)
	}

	m = NewModels(cfg, nil, config.Profile{Name: "phi", Model: "phi3:mini"})
	m.Detect(context.Background(), client)
	if a := m.Active(); !a.ChatOnly() || a.Caps.Thinking {
		t.Fatalf("phi3 must be chat-only without thinking: %+v", a.Caps)
	}

	// An Ollama that reports no capabilities: assume capable, say so.
	m = NewModels(cfg, nil, config.Profile{Name: "o", Model: "old"})
	info, _ := m.Detect(context.Background(), client)
	if a := m.Active(); a.Caps.Known || a.ChatOnly() || len(info) == 0 {
		t.Fatalf("no reported caps must assume tools: %+v, info %v", a.Caps, info)
	}

	m = NewModels(cfg, nil, config.Profile{Name: "x", Model: "missing"})
	if _, warn := m.Detect(context.Background(), client); len(warn) == 0 || !strings.Contains(warn[0], "not installed") {
		t.Fatalf("a missing model must be reported: %v", warn)
	}
}

func TestSelectProfile(t *testing.T) {
	cfg := config.Defaults()
	cfg.Profiles = []config.Profile{{Name: "a", Model: "m1"}, {Name: "b", Model: "m2"}}
	sess := &session.Session{Profile: "b", Model: "m2"}

	if p, _, _ := SelectProfile(cfg, "", nil); p.Name != "a" {
		t.Fatalf("new session: %q, want the default", p.Name)
	}
	if p, _, _ := SelectProfile(cfg, "", sess); p.Name != "b" {
		t.Fatalf("resume: %q, want the session's profile", p.Name)
	}
	if p, _, _ := SelectProfile(cfg, "a", sess); p.Name != "a" {
		t.Fatalf("flag: %q, want the flag to win", p.Name)
	}
	if _, _, err := SelectProfile(cfg, "zzz", nil); err == nil {
		t.Fatal("an unknown -profile was accepted")
	}
	gone := &session.Session{Profile: "deleted", Model: "m9"}
	p, notes, _ := SelectProfile(cfg, "", gone)
	if p.Model != "m9" || len(notes) == 0 {
		t.Fatalf("a session whose profile is gone keeps its model: %+v %v", p, notes)
	}
}

// useProfile switches the rig's agent to a profile with the given caps.
func useProfile(r *rogueRig, p config.Profile, caps Caps) {
	m := NewModels(r.a.Cfg, nil, p)
	m.Local = r.a.Client
	m.active.Store(resolve(r.a.Cfg, p, caps, "", nil))
	r.a.Models = m
	r.a.recomputeFixed()
}

func TestChatOnlyModel(t *testing.T) {
	r := newRogueRig(t, say("hello"))
	r.a.SetSystems(Prompts{Normal: "NORMAL", Rogue: "ROGUE", Chat: "CHAT ONLY"})
	useProfile(r, config.Profile{Name: "phi", Model: "phi3:mini"}, Caps{Known: true})

	if _, err := r.a.SetRogue(true, 0); err == nil {
		t.Fatal("rogue mode was allowed for a model without tools")
	}
	if r.a.SetPlan(true) {
		t.Fatal("plan mode was allowed for a model without tools")
	}
	if err := r.a.RunTurn(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	req := r.model.request(0)
	if len(req.Tools) != 0 {
		t.Fatalf("tools sent to a chat-only model: %d", len(req.Tools))
	}
	if req.Think != nil {
		t.Fatal("think sent to a model without thinking")
	}
	if req.Model != "phi3:mini" || req.Messages[0].Content != "CHAT ONLY" {
		t.Fatalf("model %q, system %q", req.Model, req.Messages[0].Content)
	}
}

func TestChatOnlySwitchesModesOff(t *testing.T) {
	r := newRogueRig(t, say("hello"))
	r.a.rogue.Store(true) // a session resumed in rogue mode
	useProfile(r, config.Profile{Name: "phi", Model: "phi3:mini"}, Caps{Known: true})
	if err := r.a.RunTurn(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if r.a.Rogue() {
		t.Fatal("rogue mode must switch off for a model without tools")
	}
}

func TestThinkingModelSendsThink(t *testing.T) {
	r := newRogueRig(t, say("hello"))
	r.a.SetThink(true)
	if err := r.a.RunTurn(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if req := r.model.request(0); req.Think == nil || !*req.Think || len(req.Tools) == 0 {
		t.Fatalf("a capable model gets think and tools: think %v, %d tools", req.Think, len(req.Tools))
	}
}

func TestToolCallIDs(t *testing.T) {
	r := newRogueRig(t, call("write_file", map[string]any{"path": "a.txt", "content": "x\n"}), say("done"))
	if err := r.a.RunTurn(context.Background(), "write it"); err != nil {
		t.Fatal(err)
	}
	var id string
	for _, e := range r.a.sess.Context {
		if e.Role == "assistant" && len(e.ToolCalls) > 0 {
			id = e.ToolCalls[0].ID
		}
		if e.Role == "tool" && (id == "" || e.ToolCallID != id) {
			t.Fatalf("tool result id %q does not match call id %q", e.ToolCallID, id)
		}
	}
	if !strings.HasPrefix(id, "call_") {
		t.Fatalf("call id = %q", id)
	}
	// The id reaches the model with the history.
	found := false
	for _, m := range r.model.request(1).Messages {
		if m.Role == "tool" && m.ToolCallID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("the tool result sent to the model has no tool_call_id")
	}
	// Compaction keeps it.
	out := elide([]session.Entry{{Message: ollama.Message{Role: "assistant", ToolCalls: []ollama.ToolCall{{ID: "call_x", Function: ollama.ToolCallFunction{Name: "w", Arguments: map[string]any{"content": strings.Repeat("z", 2000)}}}}}}}, 600)
	if out[0].ToolCalls[0].ID != "call_x" {
		t.Fatal("elide dropped the tool call id")
	}
}

func TestContextLimits(t *testing.T) {
	cc := config.Defaults().Context
	l, warn := contextLimits(cc, 131072)
	if warn != "" || l.compactAt != 98304 || l.target != 39321 || l.keepRecent != 19660 || l.summaryChunk != 60293 || l.reserve != 8192 || l.reserveThinking != 24576 {
		t.Fatalf("defaults at 131072: %+v %q", l, warn)
	}
	// Reserves are the smaller of tokens and fraction: a small window keeps room to work.
	if l, _ := contextLimits(cc, 32768); l.reserve != 2048 || l.reserveThinking != 6144 || l.compactAt != 24576 {
		t.Fatalf("at 32768: %+v", l)
	}
	// A fixed value replaces its fraction.
	cc.CompactAtTokens = 100000
	if l, warn := contextLimits(cc, 131072); l.compactAt != 100000 || warn != "" {
		t.Fatalf("fixed compact_at: %+v %q", l, warn)
	}
	// A fixed value that does not fit the window: fractions instead, with a warning.
	if l, warn := contextLimits(cc, 65536); l.compactAt != 49152 || warn == "" {
		t.Fatalf("fixed compact_at above a 65536 window: %+v %q", l, warn)
	}
}

func TestCalibrationIsGlobal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	db, err := modeldb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Update(testWeights, "qwen3.8:latest", func(r *modeldb.Record) { r.CharsPerToken = 4.2 })
	if err := db.Save(); err != nil {
		t.Fatal(err)
	}

	r := newRogueRig(t)
	p := config.Profile{Name: "q", Model: "qwen3.8:latest"}
	m := NewModels(r.a.Cfg, db, p)
	m.Local = r.a.Client
	m.active.Store(resolve(r.a.Cfg, p, Caps{}, testWeights, nil))
	r.a.Models = m
	r.a.cptKey = "" // as a new agent: read the record
	r.a.syncCPT(m.Active())
	if r.a.cpt != 4.2 {
		t.Fatalf("cpt = %v, want the stored 4.2", r.a.cpt)
	}

	r.a.cpt = 3.1
	r.a.Save()
	again, _ := modeldb.Open(path)
	if rec, ok := again.Get(testWeights); !ok || rec.CharsPerToken != 3.1 {
		t.Fatalf("cpt not written back: %+v", rec)
	}
	if r.a.sess.FormatVersion != 0 && r.a.sess.FormatVersion != session.FormatVersion {
		t.Fatalf("format version %d", r.a.sess.FormatVersion)
	}
}
