package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"clauzette/internal/config"
	"clauzette/internal/llm"
	"clauzette/internal/modeldb"
	"clauzette/internal/ollama"
	"clauzette/internal/textutil"
)

const (
	adaptRound   = 4096 // results are multiples of this
	adaptStep    = 8192 // precision of the refinement
	adaptMaxUse  = 0.97 // a load that leaves less VRAM free than this does not count as fitting
	adaptIdleMiB = 512  // VRAM still in use after unloading everything, above which /adapt refuses
	adaptKeep    = "5m" // keep_alive for probe loads
)

// PromptFunc rebuilds a session's system prompts for the active model.
type PromptFunc func(date string) (Prompts, []string, error)

// adaptProbe is one test load.
type adaptProbe struct {
	n           int
	fits        bool
	size, vram  int64 // the model's size in memory, and the part on the GPU
	used, total int64 // GPU memory after the load; 0 when unknown
	reason      string
}

// AdaptModel finds the largest context with which a model stays entirely
// on the GPU, and stores it for profiles with num_ctx "fit". It is a manual,
// occasional operation: it unloads every loaded model (whoever loaded it),
// checks the GPU is then free, and loads the model at several context
// sizes: 100%, 75%, 50% and 25% of its native context, largest first, then a
// binary search in 8k steps between the largest size that fit and the
// smallest that did not. With verify, it also runs one request at about 90%
// of the result. Whatever happens (success, failure, !c), the active profile
// is loaded again at the end. It must run between turns.
func (a *Agent) AdaptModel(ctx context.Context, name string, verify bool, prompts PromptFunc) (err error) {
	if a.rogue.Load() {
		return errors.New("not in rogue mode: /adapt takes the GPU away from the model for minutes")
	}
	local, ok := a.Models.Local.(llm.Local)
	if !ok {
		return errors.New("/adapt needs a local Ollama")
	}
	p, err := a.Models.target(ctx, name)
	if err != nil {
		return err
	}
	if config.IsCloud(p.Provider) {
		return fmt.Errorf("/adapt measures the local GPU; %s runs on %s, which sets its own context limit", p.Model, p.Provider)
	}
	tgt, _, warns := a.Models.probe(ctx, p)
	for _, s := range warns {
		a.warnf("%s", s)
	}
	native := tgt.Caps.NativeCtx
	if native <= 0 {
		return fmt.Errorf("Ollama does not report the native context of %s, so there is no upper size to try; pin a value with /adapt %s set <tokens>", tgt.Model, name)
	}
	if strings.HasPrefix(tgt.Key, "model:") {
		a.warnf("Ollama shows no weights digest for %s; the result is stored under its name only", tgt.Model)
	}
	version, _ := local.Version(ctx)

	a.warnf("/adapt unloads every loaded model and loads %s several times; this takes minutes, and !c stops it", tgt.Model)
	defer func() { a.restoreAfterAdapt(ctx, local, tgt.Model, prompts) }()

	if err := a.unloadAll(ctx, local); err != nil {
		return err
	}
	used, total, memErr := a.GPUMem.Read(ctx)
	if memErr != nil {
		a.warnf("%v; only checking that the model stays on the GPU, not the %d%% memory margin", memErr, int(adaptMaxUse*100))
	} else if limit := max(int64(adaptIdleMiB)<<20, total*3/100); used > limit {
		return fmt.Errorf("the GPU still uses %s of %s after unloading Ollama's models: something else holds it, so any result would be too small",
			textutil.HumanBytes(used), textutil.HumanBytes(total))
	}

	probe := func(n int) adaptProbe {
		pr := adaptProbe{n: n}
		opts := tgt.RequestOptions()
		opts["num_ctx"] = n
		a.emit(Event{Kind: EvWaiting, Text: fmt.Sprintf("loading %s with %s context", tgt.Model, textutil.KTok(n))})
		_, lerr := local.Load(ctx, tgt.Model, opts, adaptKeep)
		a.emit(Event{Kind: EvWaitingDone})
		switch {
		case lerr != nil:
			pr.reason = "load failed: " + textutil.OneLine(lerr.Error(), 120)
		default:
			m, ok := runningModel(ctx, local, tgt.Model)
			if !ok {
				pr.reason = "Ollama does not show it as loaded"
				break
			}
			pr.size, pr.vram = m.Size, m.SizeVRAM
			if memErr == nil {
				pr.used, pr.total, _ = a.GPUMem.Read(ctx)
			}
			switch {
			case m.Size > 0 && m.SizeVRAM < m.Size:
				pr.reason = fmt.Sprintf("only %d%% on the GPU", int(100*m.SizeVRAM/m.Size))
			case m.ContextLength > 0 && m.ContextLength != n:
				pr.reason = fmt.Sprintf("Ollama loaded it with %s instead", textutil.KTok(m.ContextLength))
			case pr.total > 0 && float64(pr.used) > adaptMaxUse*float64(pr.total):
				pr.reason = fmt.Sprintf("on the GPU, but %s of %s VRAM in use (above %d%%)", textutil.HumanBytes(pr.used), textutil.HumanBytes(pr.total), int(adaptMaxUse*100))
			default:
				pr.fits = true
			}
		}
		state := "fits"
		if !pr.fits {
			state = "no — " + pr.reason
		}
		mem := ""
		if pr.total > 0 {
			mem = fmt.Sprintf(", VRAM %s of %s", textutil.HumanBytes(pr.used), textutil.HumanBytes(pr.total))
		}
		if pr.size > 0 {
			mem = fmt.Sprintf(" (%s%s)", textutil.HumanBytes(pr.size), mem)
		}
		a.info("  %7s  %3d%% of native: %s%s", textutil.KTok(n), 100*n/native, state, mem)
		return pr
	}

	var probes []adaptProbe
	run := func(n int) (adaptProbe, error) {
		if err := ctx.Err(); err != nil {
			return adaptProbe{}, err
		}
		pr := probe(n)
		if err := ctx.Err(); err != nil {
			return pr, err
		}
		probes = append(probes, pr)
		return pr, nil
	}

	a.info("adapting %s (native context %s):", tgt.Model, textutil.KTok(native))
	best, failAt := 0, 0
	for _, f := range []float64{1, 0.75, 0.5, 0.25} {
		n := int(f*float64(native)) / adaptRound * adaptRound
		if n < adaptRound {
			break
		}
		pr, err := run(n)
		if err != nil {
			return err
		}
		if pr.fits {
			best = n
			break
		}
		failAt = n
	}
	if best == 0 {
		pr, err := run(adaptRound)
		if err != nil {
			return err
		}
		if !pr.fits {
			return fmt.Errorf("%s does not fit fully on the GPU even with a %d-token context: %s", tgt.Model, adaptRound, pr.reason)
		}
		best = adaptRound
	}
	if failAt > best {
		lo, hi := best, failAt
		for hi-lo > adaptStep {
			mid := lo + (hi-lo)/2/adaptStep*adaptStep
			if mid <= lo {
				break
			}
			pr, err := run(mid)
			if err != nil {
				return err
			}
			if pr.fits {
				lo = mid
			} else {
				hi = mid
			}
		}
		best = lo
	}

	kv := kvPerToken(probes)
	if kv == 0 && best/2 >= adaptRound {
		// One fitting size alone cannot show how memory grows with context.
		if _, err := run(best / 2 / adaptRound * adaptRound); err != nil {
			return err
		}
		kv = kvPerToken(probes)
	}
	var at adaptProbe
	for _, pr := range probes {
		if pr.n == best && pr.fits {
			at = pr
		}
	}
	if verify && probes[len(probes)-1].n != best {
		// the verify run needs the model loaded at the result
		pr, err := run(best)
		if err != nil {
			return err
		}
		if !pr.fits {
			return fmt.Errorf("%s fit at %s once but not on a second load (%s); run /adapt again when the GPU is idle", tgt.Model, textutil.KTok(best), pr.reason)
		}
		at = pr
	}

	rec := modeldb.Record{
		FitCtx: best, FitSource: "adapt", AdaptedAt: time.Now().UTC(),
		KVBytesPerToken: kv, SizeAtFit: at.size, VRAMUsedAtFit: at.used, VRAMTotal: at.total, Ollama: version,
	}
	if verify {
		v, err := a.adaptVerify(ctx, local, tgt, best)
		if err != nil {
			return fmt.Errorf("the verify run at %s failed, so nothing was saved: %w", textutil.KTok(best), err)
		}
		rec.VerifiedTokens, rec.PrefillTPS, rec.GenTPS = v.tokens, v.prefill, v.gen
	}
	a.Models.DB.Update(tgt.Key, tgt.Model, func(r *modeldb.Record) {
		names, cpt := r.Names, r.CharsPerToken
		*r = rec
		r.Names, r.CharsPerToken = names, cpt
	})
	if err := a.Models.DB.Save(); err != nil {
		return fmt.Errorf("could not save the result to the models file: %w", err)
	}

	pct := 100 * best / native
	kvText := ""
	if kv > 0 {
		kvText = fmt.Sprintf("; the KV cache takes about %s per 1k tokens", textutil.HumanBytes(int64(kv*1024)))
	}
	a.info("%s: %s fits fully on the GPU (%d%% of its native %s)%s", tgt.Model, textutil.KTok(best), pct, textutil.KTok(native), kvText)
	if rec.VerifiedTokens > 0 {
		refill := time.Duration(float64(best) / rec.PrefillTPS * float64(time.Second))
		a.info("verified with %s tokens: prefill %.0f tok/s (a full %s context takes ~%s to process), generation %.1f tok/s",
			textutil.KTok(rec.VerifiedTokens), rec.PrefillTPS, textutil.KTok(best), refill.Round(time.Second), rec.GenTPS)
	}
	a.info(`saved: profiles with num_ctx "fit" for %s now use %s (max_ctx caps it); /adapt lists all results`, tgt.Model, textutil.KTok(best))
	return nil
}

// kvPerToken estimates the KV-cache bytes per context token from the two
// loads furthest apart (a spilled load still reports its full size).
func kvPerToken(probes []adaptProbe) float64 {
	var lo, hi *adaptProbe
	for i := range probes {
		pr := &probes[i]
		if pr.size <= 0 {
			continue
		}
		if lo == nil || pr.n < lo.n {
			lo = pr
		}
		if hi == nil || pr.n > hi.n {
			hi = pr
		}
	}
	if lo == nil || hi == nil || hi.n == lo.n || hi.size <= lo.size {
		return 0
	}
	return float64(hi.size-lo.size) / float64(hi.n-lo.n)
}

type verifyResult struct {
	tokens       int
	prefill, gen float64
}

// adaptVerify runs one request filling about 90% of n with filler text, and
// checks the model is still entirely on the GPU afterwards.
func (a *Agent) adaptVerify(ctx context.Context, local llm.Local, tgt *Active, n int) (verifyResult, error) {
	if err := a.Guard.Wait(ctx, func(s string) { a.info("%s", s) }); err != nil {
		return verifyResult{}, err
	}
	cpt := defaultCPT
	if r := a.Models.record(tgt.Key); r != nil && r.CharsPerToken > 0 {
		cpt = r.CharsPerToken
	}
	want := int(0.9 * float64(n))
	var sb strings.Builder
	for i := 1; sb.Len() < int(float64(want)*cpt); i++ {
		fmt.Fprintf(&sb, "Line %d of a long test document: the quick brown fox jumps over the lazy dog near river %d.\n", i, i%97)
	}
	sb.WriteString("\nThis was a load test. Reply with the single word: done")
	opts := tgt.RequestOptions()
	opts["num_ctx"] = n
	opts["num_predict"] = 32
	req := ollama.ChatRequest{
		Model: tgt.Model,
		Messages: []ollama.Message{
			{Role: "system", Content: "You are being load-tested. Answer briefly."},
			{Role: "user", Content: sb.String()},
		},
		Think:     tgt.think(false),
		Options:   opts,
		KeepAlive: adaptKeep,
	}
	a.info("verifying: one request with ~%s tokens (this is the slow part)", textutil.KTok(want))
	a.emit(Event{Kind: EvWaiting, Text: fmt.Sprintf("processing ~%s tokens", textutil.KTok(want))})
	res, err := local.ChatStream(ctx, req, nil)
	a.emit(Event{Kind: EvWaitingDone})
	if err != nil {
		return verifyResult{}, err
	}
	if m, ok := runningModel(ctx, local, tgt.Model); ok && m.Size > 0 && m.SizeVRAM < m.Size {
		return verifyResult{}, fmt.Errorf("after the request only %d%% of the model is on the GPU", int(100*m.SizeVRAM/m.Size))
	}
	v := verifyResult{tokens: res.PromptEvalCount, gen: res.TokensPerSecond()}
	if res.PromptDuration > 0 {
		v.prefill = float64(res.PromptEvalCount) / res.PromptDuration.Seconds()
	}
	if v.tokens < want/2 {
		a.warnf("only %s tokens were processed, fewer than the ~%s intended (the characters-per-token estimate was off); the measurements still hold for that size",
			textutil.KTok(v.tokens), textutil.KTok(want))
	}
	if v.prefill <= 0 {
		return verifyResult{}, errors.New("Ollama reported no prompt processing time")
	}
	return v, nil
}

// unloadAll unloads every model Ollama has in memory and waits until none is.
func (a *Agent) unloadAll(ctx context.Context, local llm.Local) error {
	running, err := local.PS(ctx)
	if err != nil {
		return fmt.Errorf("could not list the loaded models: %w", err)
	}
	for _, m := range running {
		a.info("unloading %s", m.Name)
		if err := local.Unload(ctx, m.Name); err != nil && ctx.Err() == nil {
			a.warnf("could not unload %s: %v", m.Name, err)
		}
	}
	for i := 0; i < 20; i++ {
		running, err := local.PS(ctx)
		if err == nil && len(running) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errors.New("Ollama still shows loaded models after unloading them; try again in a moment")
}

// restoreAfterAdapt loads the active profile again, re-resolved so that a
// new "fit" value applies at once (and the prompts state the new window).
// It also runs after a failure or !c, with a fresh context.
func (a *Agent) restoreAfterAdapt(ctx context.Context, local llm.Local, probed string, prompts PromptFunc) {
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
	}
	before := a.Models.Active()
	act := a.Models.refresh()
	if act.Model != probed || act.Cloud != "" {
		_ = local.Unload(ctx, probed)
	}
	if act.Cloud != "" {
		return // the active model runs on act.Cloud: nothing to load here
	}
	if act.NumCtx != before.NumCtx {
		a.info("the active profile %s now uses %s context (was %s)", act.Profile, textutil.KTok(act.NumCtx), textutil.KTok(before.NumCtx))
		if prompts != nil {
			if pr, _, err := prompts(a.sess.Date); err == nil {
				a.SetSystems(pr)
				a.Save()
			}
		}
	}
	a.info("loading %s again with num_ctx=%d", act.Model, act.NumCtx)
	a.emit(Event{Kind: EvWaiting, Text: "loading " + act.Model})
	_, err := local.Load(ctx, act.Model, act.RequestOptions(), act.KeepAlive)
	a.emit(Event{Kind: EvWaitingDone})
	if err != nil {
		a.warnf("could not load %s again (%v); it loads on the next request", act.Model, err)
		return
	}
	reportPlacement(ctx, local, act, a.info, a.warnf)
}

// runningModel finds a model in Ollama's list of loaded models.
func runningModel(ctx context.Context, local llm.Local, model string) (ollama.RunningModel, bool) {
	running, err := local.PS(ctx)
	if err != nil {
		return ollama.RunningModel{}, false
	}
	for _, m := range running {
		if m.Matches(model) {
			return m, true
		}
	}
	return ollama.RunningModel{}, false
}

// checkAdapted compares a load that used an adapted context with what
// /adapt measured. A different setup marks the value stale, so it is not
// used from the next start or switch; a different Ollama version only warns.
func checkAdapted(ctx context.Context, d Deps, local llm.Local, act *Active, warn func(string, ...any)) {
	r := act.Adapted
	if r == nil {
		return
	}
	var reasons []string
	if m, ok := runningModel(ctx, local, act.Model); ok && m.Size > 0 {
		if m.SizeVRAM < m.Size {
			reasons = append(reasons, fmt.Sprintf("it no longer fits fully on the GPU (%d%%)", int(100*m.SizeVRAM/m.Size)))
		}
		if r.SizeAtFit > 0 && r.KVBytesPerToken > 0 {
			expected := float64(r.SizeAtFit) - r.KVBytesPerToken*float64(r.FitCtx-act.NumCtx)
			if expected > 0 && math.Abs(float64(m.Size)-expected)/expected > 0.05 {
				reasons = append(reasons, fmt.Sprintf("it loads at %s instead of the measured %s (did the server's KV cache type or flash attention change?)",
					textutil.HumanBytes(m.Size), textutil.HumanBytes(int64(expected))))
			}
		}
	}
	if r.VRAMTotal > 0 {
		if _, total, err := d.GPUMem.Read(ctx); err == nil && math.Abs(float64(total-r.VRAMTotal)) > float64(r.VRAMTotal)/100 {
			reasons = append(reasons, fmt.Sprintf("the GPU has %s, not the %s it was adapted on", textutil.HumanBytes(total), textutil.HumanBytes(r.VRAMTotal)))
		}
	}
	if v, err := local.Version(ctx); err == nil && r.Ollama != "" && v != r.Ollama {
		warn("%s was adapted with Ollama %s; this is %s. If it no longer fits fully on the GPU, run /adapt %s again", act.Model, r.Ollama, v, act.Model)
	}
	if len(reasons) == 0 {
		return
	}
	reason := strings.Join(reasons, "; ")
	d.Models.DB.Update(act.Key, act.Model, func(rec *modeldb.Record) { rec.Stale = reason })
	_ = d.Models.DB.Save()
	warn("the adapted context for %s no longer matches this setup: %s. It is not used from the next start or switch; run /adapt %s again", act.Model, reason, act.Model)
}

// AdaptSet pins a context size for a model, as if /adapt had measured it.
// It takes effect from the next start or /model switch.
func (a *Agent) AdaptSet(ctx context.Context, name string, n int) error {
	tgt, err := a.adaptTarget(ctx, name)
	if err != nil {
		return err
	}
	if n < 2048 {
		return errors.New("the context must be at least 2048 tokens")
	}
	if nat := tgt.Caps.NativeCtx; nat > 0 && n > nat {
		return fmt.Errorf("%s supports at most %s", tgt.Model, textutil.KTok(nat))
	}
	a.Models.DB.Update(tgt.Key, tgt.Model, func(r *modeldb.Record) {
		if r.KVBytesPerToken > 0 && r.SizeAtFit > 0 && r.FitCtx > 0 {
			r.SizeAtFit += int64(r.KVBytesPerToken * float64(n-r.FitCtx)) // keep the size check valid
		} else {
			r.SizeAtFit = 0
		}
		r.FitCtx, r.FitSource, r.Stale, r.AdaptedAt = n, "set", "", time.Now().UTC()
		r.VerifiedTokens, r.PrefillTPS, r.GenTPS = 0, 0, 0
	})
	if err := a.Models.DB.Save(); err != nil {
		return err
	}
	a.info(`%s: num_ctx "fit" now means %s (pinned); it applies from the next start or /model switch`, tgt.Model, textutil.KTok(n))
	return nil
}

// AdaptForget drops a model's adapted context.
func (a *Agent) AdaptForget(ctx context.Context, name string) error {
	tgt, err := a.adaptTarget(ctx, name)
	if err != nil {
		return err
	}
	r, ok := a.Models.DB.Get(tgt.Key)
	if !ok || r.FitCtx == 0 {
		return fmt.Errorf("%s has no adapted context", tgt.Model)
	}
	a.Models.DB.Update(tgt.Key, tgt.Model, func(r *modeldb.Record) { r.ClearFit() })
	if err := a.Models.DB.Save(); err != nil {
		return err
	}
	a.info(`%s: adapted context forgotten; num_ctx "fit" falls back to num_ctx from the next start or /model switch`, tgt.Model)
	return nil
}

func (a *Agent) adaptTarget(ctx context.Context, name string) (*Active, error) {
	p, err := a.Models.target(ctx, name)
	if err != nil {
		return nil, err
	}
	if config.IsCloud(p.Provider) {
		return nil, fmt.Errorf("/adapt values are for local models; %s runs on %s", p.Model, p.Provider)
	}
	tgt, _, _ := a.Models.probe(ctx, p)
	return tgt, nil
}

// AdaptReport lists the adapted models for /adapt.
func (a *Agent) AdaptReport() string {
	recs := a.Models.DB.All()
	var sb strings.Builder
	n := 0
	for _, kr := range recs {
		r := kr.Record
		if r.FitCtx == 0 {
			continue
		}
		n++
		fmt.Fprintf(&sb, "  %-40s fit %-6s (%s, %s)", strings.Join(r.Names, ", "), textutil.KTok(r.FitCtx), r.FitSource, r.AdaptedAt.Local().Format("2006-01-02"))
		if r.KVBytesPerToken > 0 {
			fmt.Fprintf(&sb, "  KV %s/1k tokens", textutil.HumanBytes(int64(r.KVBytesPerToken*1024)))
		}
		if r.PrefillTPS > 0 {
			fmt.Fprintf(&sb, "  prefill %.0f tok/s, generation %.1f tok/s", r.PrefillTPS, r.GenTPS)
		}
		if r.Stale != "" {
			fmt.Fprintf(&sb, "\n    STALE: %s", r.Stale)
		}
		sb.WriteString("\n")
	}
	if n == 0 {
		return `No adapted models yet. /adapt <profile or model> [--verify] measures one; profiles with num_ctx "fit" use the result.`
	}
	return "Adapted models\n" + sb.String() + `/adapt <name> [--verify] measures again; /adapt <name> set <tokens> pins a value; /adapt <name> forget drops it.`
}
