package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"clauzette/internal/ollama"
	"clauzette/internal/session"
	"clauzette/internal/textutil"
)

// Token counting
//
// Ollama has no tokenize endpoint, so tokens are estimated from characters.
// After every request the estimate is calibrated against the
// prompt_eval_count Ollama reports. When that count is far below the
// estimate, Ollama probably counted only the part that was not cached, and
// the sample is ignored.

func msgChars(m ollama.Message) int {
	n := len(m.Content) + len(m.Thinking) + len(m.ToolName) + 16
	for _, tc := range m.ToolCalls {
		b, _ := json.Marshal(tc)
		n += len(b)
	}
	return n
}

func (a *Agent) tokens(chars int) int { return int(float64(chars)/a.cpt) + 1 }

// UsedTokens is the context size shown to the operator: the exact
// prompt_eval_count Ollama reported for the last request when that is
// available, otherwise the local estimate.
func (a *Agent) UsedTokens() int {
	if n := a.sess.LastPromptTokens; n > 0 {
		return n
	}
	return a.ContextTokens()
}

// ModelContext is the model's native context length in tokens (0 = unknown),
// as reported by Ollama via /api/show. The effective cap is min(this, num_ctx).
func (a *Agent) ModelContext() int { return a.Client.ModelNativeContext() }

// ContextTokens estimates the size of the next request.
func (a *Agent) ContextTokens() int {
	chars := a.fixedChars
	for _, m := range a.buildMessages()[1:] {
		chars += msgChars(m)
	}
	return a.tokens(chars)
}

func (a *Agent) entriesTokens(entries []session.Entry) int {
	chars := a.fixedChars
	for _, e := range entries {
		chars += msgChars(e.Message)
	}
	return a.tokens(chars)
}

func (a *Agent) calibrate(sent []ollama.Message, res *ollama.Result) {
	if res == nil || res.PromptEvalCount <= 0 || len(sent) == 0 {
		return
	}
	chars := a.fixedChars
	for _, m := range sent[1:] {
		chars += msgChars(m)
	}
	if float64(res.PromptEvalCount) < 0.5*float64(chars)/a.cpt {
		return // probably only the uncached part was counted
	}
	ratio := float64(chars) / float64(res.PromptEvalCount)
	cpt := 0.7*a.cpt + 0.3*ratio
	if cpt < 2 {
		cpt = 2
	}
	if cpt > 6 {
		cpt = 6
	}
	a.cpt = cpt
}

// ensureRoom compacts before a request when the context has reached the
// threshold, or when the reply might not fit in what is left of the window.
// It runs before every model call, not only between turns, because one
// large tool result can add tens of thousands of tokens at once.
func (a *Agent) ensureRoom(ctx context.Context) error {
	cc := a.Cfg.Context
	used := a.ContextTokens()
	reserve := cc.ReserveTokens
	if a.think.Load() {
		reserve = cc.ReserveTokensThinking
	}
	switch {
	case used >= cc.CompactAtTokens:
		a.info("context at ~%s tokens (threshold %s); compacting", textutil.KTok(used), textutil.KTok(cc.CompactAtTokens))
	case used+reserve > a.Cfg.NumCtx:
		a.info("context at ~%s tokens leaves less than %s for the reply; compacting", textutil.KTok(used), textutil.KTok(reserve))
	default:
		return nil
	}
	return a.Compact(ctx)
}

const summaryPreamble = "[Summary of the earlier part of this conversation, written automatically when the context was compacted. The original messages are no longer available to you.]\n\n"

const summarySystem = "You compress the history of a conversation between an operator and an AI coding agent " +
	"so that the agent can continue the work without the original messages. Be factual and specific. " +
	"Keep exact file paths, names, versions, commands and error messages. Never invent anything."

const summaryInstructions = `Write a summary of the conversation above for the agent to continue from. Use these sections and skip any that would be empty:

## Goal
## Decisions and reasons
## Files (path: current state, what was changed)
## Commands and results that matter
## Open tasks and next steps
## Operator preferences and constraints

Use at most about 1500 words. Write only the summary.`

// Compact shrinks the working context in one step, because every change to
// earlier history makes Ollama re-read the whole prompt (with a hybrid
// model like Qwen3.8 possibly from the very start).
//
// Stage 1 removes old tool output and large tool-call arguments (file
// contents) outside the recent part; the model can always re-read a file.
// If that is not enough, stage 2 replaces the older part with a summary
// written by the model (thinking off). The last keep_recent_tokens of the
// conversation always stay verbatim. The full transcript is not touched.
func (a *Agent) Compact(ctx context.Context) error {
	cc := a.Cfg.Context
	before := a.ContextTokens()
	msgs := a.sess.Context
	split := a.findSplit(msgs, cc.KeepRecentTokens)
	if split <= 0 {
		return errors.New("nothing to compact yet: all messages are within the recent part that is always kept (context.keep_recent_tokens)")
	}
	older := elide(msgs[:split], cc.ElideToolResultsOver)
	recent := msgs[split:]

	candidate := make([]session.Entry, 0, len(msgs))
	candidate = append(candidate, older...)
	candidate = append(candidate, recent...)
	if a.entriesTokens(candidate) <= cc.CompactTargetTokens {
		a.sess.Context = candidate
		a.sess.Compactions++
		a.sess.Note("context compacted: old tool output removed")
		a.info("compacted ~%s → ~%s tokens by removing old tool output", textutil.KTok(before), textutil.KTok(a.ContextTokens()))
		a.Save()
		return nil
	}

	summary, err := a.summarize(ctx, older)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("compaction failed: %w", err)
	}
	now := time.Now()
	next := []session.Entry{{
		Message: ollama.Message{Role: "user", Content: summaryPreamble + summary},
		Time:    now,
		Kind:    session.KindSummary,
	}}
	if len(recent) > 0 && recent[0].Role == "user" {
		next = append(next, session.Entry{
			Message: ollama.Message{Role: "assistant", Content: "Understood. I will continue from this summary."},
			Time:    now,
			Kind:    session.KindAck,
		})
	}
	next = append(next, recent...)
	a.sess.Context = next
	a.sess.Compactions++
	a.sess.Note(fmt.Sprintf("context compacted: %d older messages replaced by a summary", split))
	after := a.ContextTokens()
	a.info("compacted ~%s → ~%s tokens (%d older messages summarized)", textutil.KTok(before), textutil.KTok(after), split)
	if after > cc.CompactTargetTokens {
		a.warnf("the context is still above context.compact_target_tokens; consider lowering context.keep_recent_tokens")
	}
	a.Save()
	return nil
}

// findSplit returns the index where the verbatim recent part begins. It
// never splits between an assistant's tool calls and their results: the
// recent part starts at an operator message or at an assistant message.
func (a *Agent) findSplit(msgs []session.Entry, keepTokens int) int {
	acc := 0
	i := len(msgs)
	for i > 0 {
		t := a.tokens(msgChars(msgs[i-1].Message))
		if acc+t > keepTokens {
			break
		}
		acc += t
		i--
	}
	for i < len(msgs) && msgs[i].Role == "tool" {
		i++
	}
	if i >= len(msgs) { // the last message alone is larger than keepTokens
		i = len(msgs) - 1
		for i > 0 && msgs[i].Role == "tool" {
			i--
		}
	}
	return i
}

// elide returns copies of the entries with long tool results and long
// string arguments of tool calls replaced by short placeholders. The
// originals (shared with the transcript) are not modified.
func elide(in []session.Entry, limit int) []session.Entry {
	out := make([]session.Entry, len(in))
	for i, e := range in {
		m := e.Message
		m.Thinking = ""
		if m.Role == "tool" && len(m.Content) > limit {
			m.Content = fmt.Sprintf("[Output of %s removed during context compaction (%d characters); run the tool again if you need it. It began with: %s]",
				m.ToolName, len(m.Content), textutil.OneLine(m.Content, 160))
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]ollama.ToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				args := make(map[string]any, len(tc.Function.Arguments))
				for key, v := range tc.Function.Arguments {
					if s, ok := v.(string); ok && len(s) > limit {
						v = fmt.Sprintf("[%d characters removed during context compaction]", len(s))
					}
					args[key] = v
				}
				calls[j] = ollama.ToolCall{Function: ollama.ToolCallFunction{Name: tc.Function.Name, Arguments: args}}
			}
			m.ToolCalls = calls
		}
		out[i] = session.Entry{Message: m, Time: e.Time, Kind: e.Kind}
	}
	return out
}

// summarize writes the summary of the older messages. If they are too long
// for one request, they are summarized in chunks, each request carrying the
// summary so far.
func (a *Agent) summarize(ctx context.Context, older []session.Entry) (string, error) {
	chunks := a.chunkTranscript(older, a.Cfg.Context.SummaryChunkTokens)
	prev := ""
	for i, chunk := range chunks {
		var user strings.Builder
		if prev != "" {
			user.WriteString("Summary of the conversation so far:\n\n" + prev + "\n\n---\n\nContinuation of the conversation:\n\n")
		}
		user.WriteString(chunk)
		user.WriteString("\n\n---\n\n" + summaryInstructions)
		if prev != "" {
			user.WriteString(" Merge the earlier summary into the new one.")
		}
		label := "compacting the context"
		if len(chunks) > 1 {
			label = fmt.Sprintf("compacting the context (part %d of %d)", i+1, len(chunks))
		}
		out, err := a.complete(ctx, label, summarySystem, user.String())
		if err != nil {
			return "", err
		}
		prev = strings.TrimSpace(out)
	}
	if prev == "" {
		return "", errors.New("the model returned an empty summary")
	}
	return prev, nil
}

// complete runs a one-off request without tools and without thinking.
func (a *Agent) complete(ctx context.Context, label, system, user string) (string, error) {
	think := false
	req := ollama.ChatRequest{
		Model: a.Cfg.Model,
		Messages: []ollama.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Think:     &think,
		Options:   requestOptions(a.Cfg), // same options, or Ollama reloads the model
		KeepAlive: a.Cfg.KeepAlive,
	}
	a.emit(Event{Kind: EvWaiting, Text: label})
	res, err := a.Client.ChatStream(ctx, req, nil)
	a.emit(Event{Kind: EvWaitingDone})
	if err != nil {
		return "", err
	}
	return res.Message.Content, nil
}

func (a *Agent) chunkTranscript(entries []session.Entry, chunkTokens int) []string {
	limit := int(float64(chunkTokens) * a.cpt)
	var chunks []string
	var sb strings.Builder
	for _, e := range entries {
		part := renderEntry(e)
		if len(part) > limit {
			part = textutil.TruncateMiddle(part, limit)
		}
		if sb.Len() > 0 && sb.Len()+len(part) > limit {
			chunks = append(chunks, sb.String())
			sb.Reset()
		}
		sb.WriteString(part)
	}
	if sb.Len() > 0 {
		chunks = append(chunks, sb.String())
	}
	return chunks
}

func renderEntry(e session.Entry) string {
	var sb strings.Builder
	switch {
	case e.Kind == session.KindSummary:
		sb.WriteString("EARLIER SUMMARY:\n" + strings.TrimPrefix(e.Content, summaryPreamble) + "\n\n")
	case e.Kind == session.KindAck:
		// nothing worth keeping
	case e.Role == "user":
		sb.WriteString("OPERATOR:\n" + e.Content + "\n\n")
	case e.Role == "assistant":
		if strings.TrimSpace(e.Content) != "" {
			sb.WriteString("AGENT:\n" + textutil.TruncateMiddle(e.Content, 6000) + "\n\n")
		}
		for _, tc := range e.ToolCalls {
			b, _ := json.Marshal(tc.Function.Arguments)
			fmt.Fprintf(&sb, "AGENT CALLED %s %s\n\n", tc.Function.Name, textutil.Truncate(string(b), 600))
		}
	case e.Role == "tool":
		fmt.Fprintf(&sb, "RESULT OF %s:\n%s\n\n", e.ToolName, textutil.TruncateMiddle(e.Content, 1500))
	}
	return sb.String()
}

// ContextReport describes the context for the /context command.
func (a *Agent) ContextReport() string {
	type stat struct{ n, chars int }
	stats := map[string]*stat{}
	for _, m := range a.buildMessages()[1:] {
		s := stats[m.Role]
		if s == nil {
			s = &stat{}
			stats[m.Role] = s
		}
		s.n++
		s.chars += msgChars(m)
	}
	cc := a.Cfg.Context
	used := a.ContextTokens()
	var sb strings.Builder
	fmt.Fprintf(&sb, "Context: ~%s of %s tokens (%.0f%%)\n", textutil.KTok(used), textutil.KTok(a.Cfg.NumCtx), 100*float64(used)/float64(a.Cfg.NumCtx))
	if n := a.ModelContext(); n > 0 {
		if a.Cfg.NumCtx < n {
			fmt.Fprintf(&sb, "  model supports up to %s tokens; num_ctx caps this session at %s\n", textutil.KTok(n), textutil.KTok(a.Cfg.NumCtx))
		} else {
			fmt.Fprintf(&sb, "  model supports up to %s tokens (fully used)\n", textutil.KTok(n))
		}
	}
	fmt.Fprintf(&sb, "  system prompt + tool definitions  ~%s\n", textutil.KTok(a.tokens(a.fixedChars)))
	for _, role := range []string{"user", "assistant", "tool"} {
		if s := stats[role]; s != nil {
			fmt.Fprintf(&sb, "  %-9s %4d message(s)          ~%s\n", role, s.n, textutil.KTok(a.tokens(s.chars)))
		}
	}
	fmt.Fprintf(&sb, "Compaction at ~%s, down to ~%s, keeping the last ~%s verbatim. Compactions so far: %d.\n",
		textutil.KTok(cc.CompactAtTokens), textutil.KTok(cc.CompactTargetTokens), textutil.KTok(cc.KeepRecentTokens), a.sess.Compactions)
	fmt.Fprintf(&sb, "Last request: prompt_eval_count=%d, eval_count=%d. Estimates use %.2f characters per token.\n",
		a.sess.LastPromptTokens, a.sess.LastEvalTokens, a.cpt)
	fmt.Fprintf(&sb, "Full transcript: %d entries (kept in the session file, never compacted).", len(a.sess.Transcript))
	return sb.String()
}
