package agent

import (
	"context"
	"fmt"
	"time"

	"clauzette/internal/ollama"
	"clauzette/internal/session"
	"clauzette/internal/textutil"
)

// Warmup prepares Ollama while the operator types the first message:
// it checks that Ollama is reachable, loads the model with the same
// options the real requests use (unless it is already loaded), reports
// whether the model fits in VRAM, and optionally pre-processes the system
// prompt. It never adds anything to the conversation.
func Warmup(ctx context.Context, d Deps, sess *session.Session, emit func(Event)) {
	info := func(format string, args ...any) { emit(Event{Kind: EvInfo, Text: fmt.Sprintf(format, args...)}) }
	warn := func(format string, args ...any) { emit(Event{Kind: EvWarn, Text: fmt.Sprintf(format, args...)}) }
	cfg := d.Cfg

	var version string
	var err error
	delay := 2 * time.Second
	for attempt := 1; attempt <= 5; attempt++ {
		if version, err = d.Client.Version(ctx); err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		if attempt < 5 {
			warn("ollama is not reachable at %s (%v); retrying in %s", d.Client.BaseURL(), err, delay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			delay *= 2
		}
	}
	if err != nil {
		emit(Event{Kind: EvError, Text: fmt.Sprintf("ollama is not reachable at %s: %v. Requests will fail until it is.", d.Client.BaseURL(), err)})
		return
	}

	// How much context the model itself supports, so the operator can see
	// whether num_ctx is capping it (and by how much).
	if n, err := d.Client.ShowModelContext(ctx, cfg.Model); err == nil && n > 0 {
		if cfg.NumCtx >= n {
			info("%s supports up to %s context tokens (num_ctx %s uses all of it)", cfg.Model, textutil.KTok(n), textutil.KTok(cfg.NumCtx))
		} else {
			info("%s supports up to %s context tokens; num_ctx caps it at %s (raise num_ctx for more headroom)", cfg.Model, textutil.KTok(n), textutil.KTok(cfg.NumCtx))
		}
	}

	loaded := false
	if running, err := d.Client.PS(ctx); err == nil {
		for _, m := range running {
			if !m.Matches(cfg.Model) {
				continue
			}
			if m.ContextLength > 0 && m.ContextLength != cfg.NumCtx {
				warn("%s is loaded with a %d-token context; reloading it with num_ctx=%d", cfg.Model, m.ContextLength, cfg.NumCtx)
				break
			}
			loaded = true
		}
	}

	if loaded {
		info("ollama %s: %s is already loaded", version, cfg.Model)
	} else {
		info("ollama %s: loading %s with num_ctx=%d; this can take a while", version, cfg.Model, cfg.NumCtx)
		emit(Event{Kind: EvWaiting, Text: "loading the model"})
		took, err := d.Client.Load(ctx, cfg.Model, requestOptions(cfg), cfg.KeepAlive)
		emit(Event{Kind: EvWaitingDone})
		if err != nil {
			if ctx.Err() == nil {
				emit(Event{Kind: EvError, Text: fmt.Sprintf("loading %s failed: %v", cfg.Model, err)})
			}
			return
		}
		info("%s loaded in %s", cfg.Model, took.Round(time.Second))
	}
	reportPlacement(ctx, d, info, warn)

	if cfg.WarmupPrefill && !loaded {
		prefill(ctx, d, sess, emit, info)
	}
}

// reportPlacement warns when part of the model runs on the CPU, which makes
// it several times slower (usually: num_ctx too large for the VRAM).
func reportPlacement(ctx context.Context, d Deps, info, warn func(string, ...any)) {
	running, err := d.Client.PS(ctx)
	if err != nil {
		return
	}
	for _, m := range running {
		if !m.Matches(d.Cfg.Model) || m.Size <= 0 {
			continue
		}
		pct := int(100 * m.SizeVRAM / m.Size)
		if pct < 100 {
			warn("only %d%% of %s (%s) is in GPU memory; the rest runs on the CPU and will be much slower. Lower num_ctx or check what else uses the GPU.",
				pct, d.Cfg.Model, textutil.HumanBytes(m.Size))
		} else {
			info("%s runs fully on the GPU (%s with a %s-token context)", d.Cfg.Model, textutil.HumanBytes(m.Size), textutil.KTok(d.Cfg.NumCtx))
		}
		return
	}
}

// prefill sends the system prompt and tool definitions once and generates a
// single token, so the first real request may find them already processed.
// Whether that cache is reused depends on Ollama's handling of the model
// (see "Cache behaviour" in the README); it is off by default.
func prefill(ctx context.Context, d Deps, sess *session.Session, emit func(Event), info func(string, ...any)) {
	think := sess.Think
	opts := requestOptions(d.Cfg)
	opts["num_predict"] = 1
	req := ollama.ChatRequest{
		Model: d.Cfg.Model,
		Messages: []ollama.Message{
			{Role: "system", Content: sess.System},
			{Role: "user", Content: "Hello."},
		},
		Tools:     d.Registry.Specs(),
		Think:     &think,
		Options:   opts,
		KeepAlive: d.Cfg.KeepAlive,
	}
	emit(Event{Kind: EvWaiting, Text: "pre-processing the system prompt"})
	start := time.Now()
	_, err := d.Client.ChatStream(ctx, req, nil)
	emit(Event{Kind: EvWaitingDone})
	if err == nil {
		info("system prompt pre-processed in %s", time.Since(start).Round(time.Second))
	}
}
