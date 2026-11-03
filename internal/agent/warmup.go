package agent

import (
	"context"
	"fmt"
	"time"

	"clauzette/internal/llm"
	"clauzette/internal/ollama"
	"clauzette/internal/session"
	"clauzette/internal/textutil"
)

// Warmup prepares Ollama while the operator types the first message:
// it checks that Ollama is reachable, asks what the active model supports,
// loads the model with the same options the real requests use (unless it is
// already loaded), reports whether the model fits in VRAM, and optionally
// pre-processes the system prompt. It never adds anything to the
// conversation.
func Warmup(ctx context.Context, d Deps, sess *session.Session, emit func(Event)) {
	info := func(format string, args ...any) { emit(Event{Kind: EvInfo, Text: fmt.Sprintf(format, args...)}) }
	warn := func(format string, args ...any) { emit(Event{Kind: EvWarn, Text: fmt.Sprintf(format, args...)}) }
	if d.Models == nil {
		p, _ := d.Cfg.FindProfile(d.Cfg.DefaultProfileName())
		d.Models = NewModels(d.Cfg, nil, p)
	}
	if d.Models.Local == nil {
		d.Models.Local = d.Client
	}
	local, isLocal := d.Models.Backend(d.Models.Active()).(llm.Local)
	if !isLocal {
		// A cloud model: nothing to load, no GPU. Only ask what it supports.
		act := detect(ctx, d, info, warn)
		if act.Cloud != "" {
			info("%s runs on %s: the conversation, and any file or command output in it, is sent there", act.Model, act.Cloud)
		}
		return
	}

	var version string
	var err error
	delay := 2 * time.Second
	for attempt := 1; attempt <= 5; attempt++ {
		if version, err = local.Version(ctx); err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		if attempt < 5 {
			warn("ollama is not reachable at %s (%v); retrying in %s", local.BaseURL(), err, delay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			delay *= 2
		}
	}
	if err != nil {
		emit(Event{Kind: EvError, Text: fmt.Sprintf("ollama is not reachable at %s: %v. Requests will fail until it is.", local.BaseURL(), err)})
		return
	}

	act := detect(ctx, d, info, warn)

	// How much context the model itself supports, so the operator can see
	// whether num_ctx is capping it (and by how much).
	if n := act.Caps.NativeCtx; n > 0 {
		if act.NumCtx >= n {
			info("%s supports up to %s context tokens (num_ctx %s uses all of it)", act.Model, textutil.KTok(n), textutil.KTok(act.NumCtx))
		} else {
			info("%s supports up to %s context tokens; num_ctx caps it at %s (raise num_ctx for more headroom)", act.Model, textutil.KTok(n), textutil.KTok(act.NumCtx))
		}
	}

	loaded := false
	if running, err := local.PS(ctx); err == nil {
		for _, m := range running {
			if !m.Matches(act.Model) {
				continue
			}
			if m.ContextLength > 0 && m.ContextLength != act.NumCtx {
				warn("%s is loaded with a %d-token context; reloading it with num_ctx=%d", act.Model, m.ContextLength, act.NumCtx)
				break
			}
			loaded = true
		}
	}

	if loaded {
		info("ollama %s: %s is already loaded", version, act.Model)
	} else {
		info("ollama %s: loading %s with num_ctx=%d; this can take a while", version, act.Model, act.NumCtx)
		emit(Event{Kind: EvWaiting, Text: "loading the model"})
		took, err := local.Load(ctx, act.Model, act.RequestOptions(), act.KeepAlive)
		emit(Event{Kind: EvWaitingDone})
		if err != nil {
			if ctx.Err() == nil {
				emit(Event{Kind: EvError, Text: fmt.Sprintf("loading %s failed: %v", act.Model, err)})
			}
			return
		}
		info("%s loaded in %s", act.Model, took.Round(time.Second))
	}
	reportPlacement(ctx, local, act, info, warn)
	checkAdapted(ctx, d, local, act, warn)

	if d.Cfg.WarmupPrefill && !loaded {
		prefill(ctx, d, act, sess, emit, info)
	}
}

// detect asks the backend about the active model and reports what it found.
func detect(ctx context.Context, d Deps, info, warn func(string, ...any)) *Active {
	infos, warns := d.Models.Detect(ctx, d.Client)
	for _, s := range warns {
		warn("%s", s)
	}
	for _, s := range infos {
		info("%s", s)
	}
	act := d.Models.Active()
	for _, s := range act.Notes {
		info("%s", s)
	}
	if act.Caps.Known {
		switch {
		case act.ChatOnly():
			warn("%s has no tool support: chat only, no file or command tools, no plan or rogue mode", act.Model)
		case !act.Caps.Thinking:
			info("%s has no thinking mode; !think has no effect", act.Model)
		}
	}
	return act
}

// reportPlacement warns when part of the model runs on the CPU, which makes
// it several times slower (usually: num_ctx too large for the VRAM).
func reportPlacement(ctx context.Context, local llm.Local, act *Active, info, warn func(string, ...any)) {
	running, err := local.PS(ctx)
	if err != nil {
		return
	}
	for _, m := range running {
		if !m.Matches(act.Model) || m.Size <= 0 {
			continue
		}
		pct := int(100 * m.SizeVRAM / m.Size)
		if pct < 100 {
			warn("only %d%% of %s (%s) is in GPU memory; the rest runs on the CPU and will be much slower. Lower num_ctx or check what else uses the GPU.",
				pct, act.Model, textutil.HumanBytes(m.Size))
		} else {
			info("%s runs fully on the GPU (%s with a %s-token context)", act.Model, textutil.HumanBytes(m.Size), textutil.KTok(act.NumCtx))
		}
		return
	}
}

// prefill sends the system prompt and tool definitions once and generates a
// single token, so the first real request may find them already processed.
// Whether that cache is reused depends on Ollama's handling of the model
// (see "Cache behaviour" in the README); it is off by default.
func prefill(ctx context.Context, d Deps, act *Active, sess *session.Session, emit func(Event), info func(string, ...any)) {
	opts := act.RequestOptions()
	opts["num_predict"] = 1
	system, tools := sess.System, d.Registry.Specs()
	if act.ChatOnly() {
		tools = nil
		if sess.SystemChat != "" {
			system = sess.SystemChat
		}
	}
	req := ollama.ChatRequest{
		Model: act.Model,
		Messages: []ollama.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: "Hello."},
		},
		Tools:     tools,
		Think:     act.think(sess.Think),
		Options:   opts,
		KeepAlive: act.KeepAlive,
	}
	emit(Event{Kind: EvWaiting, Text: "pre-processing the system prompt"})
	start := time.Now()
	_, err := d.Client.ChatStream(ctx, req, nil)
	emit(Event{Kind: EvWaitingDone})
	if err == nil {
		info("system prompt pre-processed in %s", time.Since(start).Round(time.Second))
	}
}
