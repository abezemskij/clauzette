package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"clauzette/internal/llm"
	"clauzette/internal/ollama"
	"clauzette/internal/textutil"
)

// SwitchModel makes the process use another profile, or a plain installed
// model, from the next request on. It must run between turns, like Compact.
//
// In order: it checks the target and what it supports; if the conversation
// is too big for the target's window, it compacts it with the current model,
// which is still loaded; it unloads the current model, so the target has the
// GPU; it loads the target with its options; and only then makes it active.
// If loading fails or is cancelled, the current profile stays active (its
// model reloads on the next request).
//
// prompts rebuilds the session's system prompts, which state the model's
// context window; the session's date stays as it was. nil keeps them.
func (a *Agent) SwitchModel(ctx context.Context, name string, prompts PromptFunc) error {
	if a.rogue.Load() {
		return errors.New("rogue mode stays on the model it started with; turn it off first (/rogue off)")
	}
	cur := a.Models.Active()
	p, err := a.Models.target(ctx, name)
	if err != nil {
		return err
	}
	if p.Name == cur.Profile && p.Model == cur.Model {
		return fmt.Errorf("already using %s", describe(cur))
	}
	next, infos, warns := a.Models.probe(ctx, p)
	for _, s := range warns {
		a.warnf("%s", s)
	}
	for _, s := range append(infos, next.Notes...) {
		a.info("%s", s)
	}

	// The conversation must fit the target's window before the switch.
	a.syncCPT(cur)
	lim, _ := contextLimits(a.Cfg.Context, next.NumCtx)
	if used := a.ContextTokens(); used >= lim.compactAt {
		a.info("the conversation (~%s tokens) is too big for the %s-token window of %s; compacting it with %s first",
			textutil.KTok(used), textutil.KTok(next.NumCtx), next.Model, cur.Model)
		if err := a.compact(ctx, 0, lim); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("could not make the conversation fit %s: %w; still using %s", next.Model, err, cur.Model)
		}
		if used := a.ContextTokens(); used >= lim.compactAt {
			return fmt.Errorf("after compacting, the conversation is still ~%s tokens, above the %s threshold for %s; still using %s (start a /new session, or compact more with /compact <percent>)",
				textutil.KTok(used), textutil.KTok(lim.compactAt), next.Model, cur.Model)
		}
	}

	// Only a local target needs the GPU. Switching to a cloud model leaves
	// the local one loaded: the in-cluster agent may be using the same Ollama.
	local, isLocal := a.Models.Backend(next).(llm.Local)
	if isLocal {
		if cur.Model != next.Model && cur.Cloud == "" {
			if err := local.Unload(ctx, cur.Model); err != nil && ctx.Err() == nil {
				a.warnf("could not unload %s (%v); loading %s anyway", cur.Model, err, next.Model)
			}
		}
		a.info("loading %s with num_ctx=%d; this can take a while", next.Model, next.NumCtx)
		a.emit(Event{Kind: EvWaiting, Text: "loading " + next.Model})
		took, err := local.Load(ctx, next.Model, next.RequestOptions(), next.KeepAlive)
		a.emit(Event{Kind: EvWaitingDone})
		if err != nil {
			if ctx.Err() != nil {
				a.warnf("switch cancelled; still using %s, which reloads on the next request", cur.Model)
				return ctx.Err()
			}
			return fmt.Errorf("loading %s failed: %v; still using %s, which reloads on the next request", next.Model, err, cur.Model)
		}
		a.info("%s loaded in %s", next.Model, took.Round(time.Second))
	}

	a.Models.set(next)
	a.syncCPT(next)
	// A !think override was for the previous model; the new profile decides.
	a.think.Store(next.Think)
	if next.ChatOnly() && a.plan.Load() {
		a.SetPlan(false)
		a.warnf("plan mode switched off: %s has no tool support", next.Model)
	}
	if prompts != nil {
		if pr, _, err := prompts(a.sess.Date); err != nil {
			a.warnf("could not rebuild the system prompts (%v); they still describe %s", err, cur.Model)
		} else {
			a.SetSystems(pr)
		}
	}
	a.sess.Profile, a.sess.Model = next.Profile, next.Model
	a.sess.Note(fmt.Sprintf("model switched from %s to %s", describe(cur), describe(next)))
	a.Audit.Log(map[string]any{"session": a.sess.ID, "event": "model", "from": cur.Model, "to": next.Model, "profile": next.Profile, "num_ctx": next.NumCtx})
	a.Save()

	if isLocal {
		reportPlacement(ctx, local, next, a.info, a.warnf)
		checkAdapted(ctx, a.Deps, local, next, a.warnf)
	}
	what := ""
	switch {
	case next.ChatOnly():
		what = "; chat only (no tools, no plan or rogue mode)"
	case !next.Caps.Thinking:
		what = "; no thinking mode"
	default:
		what = "; thinking " + map[bool]string{true: "on", false: "off"}[next.Think]
	}
	if next.Cloud != "" {
		what += "; runs on " + next.Cloud + ": the conversation, and any file or command output in it, is sent there"
	}
	a.info("now using %s: %s-token window%s", describe(next), textutil.KTok(next.NumCtx), what)
	if isLocal && a.Cfg.WarmupPrefill && len(a.sess.Context) > 0 {
		a.prefillConversation(ctx, next)
	}
	return nil
}

// describe names a profile for messages: the model, plus the profile when
// it has its own name.
func describe(a *Active) string {
	if a.Profile == "" || a.Profile == a.Model {
		return a.Model
	}
	return fmt.Sprintf("%s (profile %s)", a.Model, a.Profile)
}

// prefillConversation sends the whole conversation once and generates a
// single token, so the next real request may find it already processed
// (see warmup_prefill). A switch otherwise moves that wait to the
// operator's next message.
func (a *Agent) prefillConversation(ctx context.Context, act *Active) {
	opts := act.RequestOptions()
	opts["num_predict"] = 1
	req := ollama.ChatRequest{
		Model:     act.Model,
		Messages:  a.buildMessages(),
		Tools:     a.toolSpecs(),
		Think:     act.think(a.think.Load()),
		Options:   opts,
		KeepAlive: act.KeepAlive,
	}
	a.emit(Event{Kind: EvWaiting, Text: "processing the conversation for " + act.Model})
	start := time.Now()
	_, err := a.Models.Backend(act).ChatStream(ctx, req, nil)
	a.emit(Event{Kind: EvWaitingDone})
	if err == nil {
		a.info("conversation pre-processed in %s", time.Since(start).Round(time.Second))
	}
}
