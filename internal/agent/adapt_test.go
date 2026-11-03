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
	"clauzette/internal/gpu"
	"clauzette/internal/modeldb"
	"clauzette/internal/ollama"
	"clauzette/internal/session"
	"clauzette/internal/tools"
)

const gib = int64(1) << 30

// simModel is a model on the simulated GPU: its size grows linearly with
// the context, like weights plus KV cache.
type simModel struct {
	base   int64 // bytes at zero context
	kv     int64 // bytes per context token
	native int
}

// gpuSim is an Ollama on one GPU. A load that does not fit spills to the
// CPU (size_vram < size), as Ollama does; the metrics endpoint reports the
// VRAM in use.
type gpuSim struct {
	mu       sync.Mutex
	capacity int64
	other    int64 // VRAM used by something outside Ollama
	models   map[string]simModel
	loaded   string
	loadedN  int
	loads    []string
	verifies int
	onLoad   func(n int) // called after each load is recorded
}

func (g *gpuSim) sizes() (size, vram int64) {
	m := g.models[g.loaded]
	size = m.base + m.kv*int64(g.loadedN)
	vram = size
	if free := g.capacity - g.other; vram > free {
		vram = free
	}
	return size, vram
}

func (g *gpuSim) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch r.URL.Path {
	case "/api/version":
		w.Write([]byte(`{"version": "0.33.0"}`))
	case "/api/tags":
		var list []map[string]any
		for name, m := range g.models {
			list = append(list, map[string]any{"name": name + ":latest", "size": m.base,
				"capabilities": []string{"completion", "tools", "thinking"}, "details": map[string]any{"context_length": m.native}})
		}
		json.NewEncoder(w).Encode(map[string]any{"models": list})
	case "/api/show":
		var in struct{ Name string }
		json.NewDecoder(r.Body).Decode(&in)
		name := strings.TrimSuffix(in.Name, ":latest")
		json.NewEncoder(w).Encode(map[string]any{"modelfile": "FROM /blobs/sha256-" + strings.Repeat(fmt.Sprintf("%x", len(name)), 64)[:64] + "\n"})
	case "/api/ps":
		if g.loaded == "" {
			w.Write([]byte(`{"models": []}`))
			return
		}
		size, vram := g.sizes()
		json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": g.loaded + ":latest", "model": g.loaded + ":latest", "size": size, "size_vram": vram, "context_length": g.loadedN}}})
	case "/metrics":
		used := g.other
		if g.loaded != "" {
			_, vram := g.sizes()
			used += vram
		}
		fmt.Fprintf(w, "nvidia_smi_memory_used_bytes{uuid=\"x\"} %d\nnvidia_smi_memory_total_bytes{uuid=\"x\"} %d\n", used, g.capacity)
	case "/api/chat":
		var req ollama.ChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		name := strings.TrimSuffix(req.Model, ":latest")
		if len(req.Messages) == 0 {
			if req.KeepAlive == "0s" {
				if g.loaded == name {
					g.loaded, g.loadedN = "", 0
				}
				w.Write([]byte(`{}`))
				return
			}
			n := 2048
			if v, ok := req.Options["num_ctx"].(float64); ok {
				n = int(v)
			}
			g.loaded, g.loadedN = name, n
			g.loads = append(g.loads, fmt.Sprintf("%s@%d", name, n))
			if g.onLoad != nil {
				g.onLoad(len(g.loads))
			}
			w.Write([]byte(`{"done": true}`))
			return
		}
		g.verifies++
		tokens := len(req.Messages[1].Content) / 4
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"role": "assistant", "content": "done"}, "done": true,
			"prompt_eval_count": tokens, "prompt_eval_duration": int64(time.Duration(tokens) * time.Millisecond),
			"eval_count": 10, "eval_duration": int64(500 * time.Millisecond)})
	default:
		http.NotFound(w, r)
	}
}

type adaptRig struct {
	a      *Agent
	sim    *gpuSim
	db     *modeldb.DB
	dbPath string
}

// newAdaptRig: "big" needs refinement on a 24 GiB GPU; "light" fits at its
// full native context. The active profile is big with num_ctx "fit".
func newAdaptRig(t *testing.T, withMetrics bool) *adaptRig {
	t.Helper()
	sim := &gpuSim{capacity: 24 * gib, models: map[string]simModel{
		"big":   {base: 18 * gib, kv: 36 << 10, native: 262144}, // fits up to ~150k
		"light": {base: 8 * gib, kv: 16 << 10, native: 262144},  // fits all 262k
	}}
	srv := httptest.NewServer(sim)
	t.Cleanup(srv.Close)
	cfg := config.Defaults()
	cfg.Profiles = []config.Profile{{Name: "big", Model: "big", NumCtx: config.CtxFit}, {Name: "light", Model: "light", NumCtx: config.CtxFit}}
	if withMetrics {
		cfg.GPU.MetricsURL = srv.URL + "/metrics"
		cfg.GPU.MemUsedMetric, cfg.GPU.MemTotalMetric, cfg.GPU.MemFreeMetric, cfg.GPU.MemScale = "nvidia_smi_memory_used_bytes", "nvidia_smi_memory_total_bytes", "", 1
	}
	ws, _ := tools.NewWorkspace(t.TempDir())
	store, _ := session.NewStore(t.TempDir())
	dbPath := filepath.Join(t.TempDir(), "models.json")
	db, _ := modeldb.Open(dbPath)
	client := ollama.New(srv.URL, ollama.Timeouts{Connect: time.Second, FirstToken: 10 * time.Second, InterToken: 10 * time.Second, Load: 10 * time.Second})
	models := NewModels(cfg, db, cfg.Profiles[0])
	models.Detect(context.Background(), client)
	events := make(chan Event, 256)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case <-events:
			case <-done:
				return
			}
		}
	}()
	sess := &session.Session{ID: "s", Profile: "big", Model: "big", Date: "d", System: "S"}
	a := New(Deps{Cfg: cfg, Client: client, Models: models, GPUMem: gpu.NewMemory(cfg.GPU), Registry: tools.NewRegistry(&tools.WriteFile{WS: ws}), Store: store, WS: ws}, sess, events)
	return &adaptRig{a: a, sim: sim, db: db, dbPath: dbPath}
}

func (r *adaptRig) prompts(string) (Prompts, []string, error) {
	return Prompts{Normal: fmt.Sprintf("window %d", r.a.Models.Active().NumCtx)}, nil, nil
}

// trueMax is the largest multiple of 4096 at which a model stays below 97%
// of the simulated VRAM.
func trueMax(m simModel, capacity int64) int {
	n := int((int64(adaptMaxUse*float64(capacity)) - m.base) / m.kv)
	return n / adaptRound * adaptRound
}

func TestAdaptRefines(t *testing.T) {
	r := newAdaptRig(t, true)
	if err := r.a.AdaptModel(context.Background(), "big", false, r.prompts); err != nil {
		t.Fatal(err)
	}
	rec, ok := r.db.Get(r.a.Models.Active().Key)
	if !ok || rec.FitCtx == 0 {
		t.Fatalf("nothing saved: %+v", rec)
	}
	want := trueMax(r.sim.models["big"], r.sim.capacity)
	if rec.FitCtx > want || rec.FitCtx <= want-adaptStep || rec.FitCtx%adaptRound != 0 {
		t.Fatalf("fit %d, want within one step below %d", rec.FitCtx, want)
	}
	if rec.KVBytesPerToken < 36<<10-1 || rec.KVBytesPerToken > 36<<10+1 {
		t.Fatalf("kv = %v, want 36 KiB per token", rec.KVBytesPerToken)
	}
	if rec.FitSource != "adapt" || rec.VRAMTotal != r.sim.capacity || rec.Ollama != "0.33.0" || rec.SizeAtFit == 0 {
		t.Fatalf("record = %+v", rec)
	}
	if len(r.sim.loads) > 10 {
		t.Fatalf("too many loads: %v", r.sim.loads)
	}
	// The active profile (num_ctx "fit") now uses the result, is loaded with
	// it, and its prompt states the new window.
	act := r.a.Models.Active()
	if act.NumCtx != rec.FitCtx || act.Adapted == nil {
		t.Fatalf("active num_ctx %d, want %d", act.NumCtx, rec.FitCtx)
	}
	if last := r.sim.loads[len(r.sim.loads)-1]; last != fmt.Sprintf("big@%d", rec.FitCtx) {
		t.Fatalf("the active profile was not reloaded with the result: %v", r.sim.loads)
	}
	if r.a.Session().System != fmt.Sprintf("window %d", rec.FitCtx) {
		t.Fatalf("prompt not rebuilt: %q", r.a.Session().System)
	}
}

func TestAdaptFitsAtFullContext(t *testing.T) {
	r := newAdaptRig(t, true)
	if err := r.a.AdaptModel(context.Background(), "light", true, r.prompts); err != nil {
		t.Fatal(err)
	}
	tgt, _ := r.a.adaptTarget(context.Background(), "light")
	rec, _ := r.db.Get(tgt.Key)
	if rec.FitCtx != 262144 {
		t.Fatalf("fit = %d, want the full 262144", rec.FitCtx)
	}
	if rec.KVBytesPerToken == 0 {
		t.Fatal("a second load should have measured the KV cost")
	}
	if r.sim.verifies != 1 || rec.PrefillTPS <= 0 || rec.GenTPS <= 0 || rec.VerifiedTokens == 0 {
		t.Fatalf("verify: %d runs, %+v", r.sim.verifies, rec)
	}
	// The probed model is unloaded and the active one reloaded.
	if r.sim.loaded != "big" {
		t.Fatalf("loaded after adapt: %q", r.sim.loaded)
	}
}

func TestAdaptRefusesBusyGPU(t *testing.T) {
	r := newAdaptRig(t, true)
	r.sim.other = 6 * gib // e.g. a desktop session on the same card
	err := r.a.AdaptModel(context.Background(), "big", false, r.prompts)
	if err == nil || !strings.Contains(err.Error(), "something else holds it") {
		t.Fatalf("err = %v", err)
	}
	for _, l := range r.sim.loads[:len(r.sim.loads)-1] {
		t.Fatalf("no probe may run on a busy GPU: %v (%s)", r.sim.loads, l)
	}
}

func TestAdaptWithoutMetrics(t *testing.T) {
	r := newAdaptRig(t, false) // placement only
	if err := r.a.AdaptModel(context.Background(), "big", false, r.prompts); err != nil {
		t.Fatal(err)
	}
	rec, _ := r.db.Get(r.a.Models.Active().Key)
	// Without the 97% margin it may go up to full VRAM.
	full := int((r.sim.capacity - r.sim.models["big"].base) / r.sim.models["big"].kv)
	if rec.FitCtx == 0 || rec.FitCtx > full || rec.VRAMTotal != 0 {
		t.Fatalf("fit %d (limit %d), vram_total %d", rec.FitCtx, full, rec.VRAMTotal)
	}
}

func TestAdaptCancelRestores(t *testing.T) {
	r := newAdaptRig(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.sim.onLoad = func(n int) {
		if n == 2 { // !c during the second probe load
			cancel()
		}
	}
	err := r.a.AdaptModel(ctx, "big", false, r.prompts)
	if err == nil {
		t.Fatal("a cancelled adapt reported success")
	}
	r.sim.onLoad = nil
	if r.sim.loaded != "big" || r.sim.loadedN != r.a.Cfg.NumCtx {
		t.Fatalf("the active profile must be loaded again after a cancel: %q@%d (%v)", r.sim.loaded, r.sim.loadedN, r.sim.loads)
	}
	if rec, _ := r.db.Get(r.a.Models.Active().Key); rec.FitCtx != 0 {
		t.Fatalf("a cancelled adapt saved a result: %+v", rec)
	}

	// Cancelled before it starts: nothing is unloaded, so nothing to restore.
	before := len(r.sim.loads)
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := r.a.AdaptModel(ctx2, "big", false, r.prompts); err == nil {
		t.Fatal("expected an error")
	}
	if len(r.sim.loads) != before {
		t.Fatalf("an adapt cancelled before starting loaded models: %v", r.sim.loads[before:])
	}
}

func TestCheckAdaptedMarksStale(t *testing.T) {
	r := newAdaptRig(t, true)
	if err := r.a.AdaptModel(context.Background(), "big", false, r.prompts); err != nil {
		t.Fatal(err)
	}
	act := r.a.Models.Active()
	local := r.a.Client.(*ollama.Client)

	// Same setup: nothing changes.
	checkAdapted(context.Background(), r.a.Deps, local, act, func(string, ...any) {})
	if rec, _ := r.db.Get(act.Key); rec.Stale != "" {
		t.Fatalf("marked stale on an unchanged setup: %s", rec.Stale)
	}

	// The server's KV cache grows (q8_0 → f16): the load no longer matches.
	m := r.sim.models["big"]
	m.kv *= 2
	r.sim.models["big"] = m
	local.Load(context.Background(), "big", act.RequestOptions(), "5m")
	warned := ""
	checkAdapted(context.Background(), r.a.Deps, local, act, func(f string, args ...any) { warned = fmt.Sprintf(f, args...) })
	rec, _ := r.db.Get(act.Key)
	if rec.Stale == "" || !strings.Contains(warned, "no longer matches") {
		t.Fatalf("stale %q, warning %q", rec.Stale, warned)
	}
	// From the next resolution, "fit" falls back and says why.
	next := r.a.Models.refresh()
	if next.Adapted != nil || next.NumCtx != r.a.Cfg.NumCtx || len(next.Notes) == 0 {
		t.Fatalf("a stale value was used: %d %+v", next.NumCtx, next.Notes)
	}
}

func TestAdaptSetAndForget(t *testing.T) {
	r := newAdaptRig(t, true)
	ctx := context.Background()
	if err := r.a.AdaptSet(ctx, "light", 100000); err != nil {
		t.Fatal(err)
	}
	if err := r.a.AdaptSet(ctx, "light", 999999); err == nil {
		t.Fatal("a value above the native context was accepted")
	}
	tgt, _ := r.a.adaptTarget(ctx, "light")
	if rec, _ := r.db.Get(tgt.Key); rec.FitCtx != 100000 || rec.FitSource != "set" {
		t.Fatalf("after set: %+v", rec)
	}
	if !strings.Contains(r.a.AdaptReport(), "light") {
		t.Fatalf("report:\n%s", r.a.AdaptReport())
	}
	if err := r.a.AdaptForget(ctx, "light"); err != nil {
		t.Fatal(err)
	}
	if rec, _ := r.db.Get(tgt.Key); rec.FitCtx != 0 {
		t.Fatalf("after forget: %+v", rec)
	}
	if err := r.a.AdaptForget(ctx, "light"); err == nil {
		t.Fatal("forgetting twice should say there is nothing to forget")
	}
}

func TestMemoryReader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// DCGM style: MiB, used and free, two GPUs.
		w.Write([]byte("DCGM_FI_DEV_FB_USED{gpu=\"0\"} 1024\nDCGM_FI_DEV_FB_USED{gpu=\"1\"} 1024\nDCGM_FI_DEV_FB_FREE{gpu=\"0\"} 3072\nDCGM_FI_DEV_FB_FREE{gpu=\"1\"} 3072\n"))
	}))
	defer srv.Close()
	cfg := config.Defaults().GPU
	cfg.MetricsURL = srv.URL
	used, total, err := gpu.NewMemory(cfg).Read(context.Background())
	if err != nil || used != 2*gib || total != 8*gib {
		t.Fatalf("used %d total %d err %v", used, total, err)
	}
	var none *gpu.Memory
	if _, _, err := none.Read(context.Background()); err == nil {
		t.Fatal("a nil reader returned memory")
	}
}
