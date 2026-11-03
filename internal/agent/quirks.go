package agent

import (
	"strings"

	"clauzette/internal/ollama"
)

// Quirks are named fixes for models that misbehave in a known way. A profile
// turns them on ("quirks": ["inline-think"]); config.Quirks lists the names.
// A quirk only repairs what comes back from the model; the agent loop never
// checks model names.

func (a *Active) hasQuirk(name string) bool {
	for _, q := range a.src.Quirks {
		if q == name {
			return true
		}
	}
	return false
}

const thinkEnd = "</think>"

// thinkSplitter is the inline-think quirk. Some models write their reasoning
// into the reply instead of the thinking field, ending it with "</think>"
// and often without an opening tag; qwen3:4b does this when thinking is off.
//
// While a reply streams with thinking off, its text is treated as thinking
// until "</think>" arrives, and as the answer after it. If the reply ends
// without the tag, the held text was the answer after all and is shown
// then. The finished message is split the same way, so the leaked reasoning
// never enters the history or the next request. With thinking on, the model
// uses the thinking field properly, so streaming is not held back; only a
// stray tag in the finished message is cleaned up.
type thinkSplitter struct {
	on     bool // the quirk is enabled for this model
	stream bool // hold streamed text back as thinking (thinking is off)
	buf    strings.Builder
	shown  int  // bytes of buf already passed on as thinking
	done   bool // the tag was seen; the rest is the answer
}

func newThinkSplitter(act *Active, req ollama.ChatRequest) *thinkSplitter {
	on := act.hasQuirk("inline-think")
	return &thinkSplitter{on: on, stream: on && (req.Think == nil || !*req.Think)}
}

// delta takes one piece of streamed content and returns what to show as
// thinking and what to show as the answer.
func (s *thinkSplitter) delta(text string) (thinking, answer string) {
	if !s.stream || s.done {
		return "", text
	}
	s.buf.WriteString(text)
	all := s.buf.String()
	if i := strings.Index(all, thinkEnd); i >= 0 {
		s.done = true
		if i > s.shown {
			thinking = all[s.shown:i]
		}
		answer = strings.TrimLeft(all[i+len(thinkEnd):], "\r\n")
		return strings.TrimPrefix(thinking, "<think>"), answer
	}
	// Pass on all but a possible partial tag at the end.
	if safe := len(all) - (len(thinkEnd) - 1); safe > s.shown {
		thinking = all[s.shown:safe]
		if s.shown == 0 {
			thinking = strings.TrimPrefix(thinking, "<think>")
		}
		s.shown = safe
	}
	return thinking, ""
}

// late is the held text to show as the answer when the reply ended without
// the tag: it was never reasoning.
func (s *thinkSplitter) late() string {
	if !s.stream || s.done {
		return ""
	}
	return s.buf.String()
}

// fix splits the finished message at the tag: the text before it becomes
// thinking, the text after it the answer.
func (s *thinkSplitter) fix(m *ollama.Message) {
	if !s.on {
		return
	}
	i := strings.Index(m.Content, thinkEnd)
	if i < 0 {
		return
	}
	reasoning := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(m.Content[:i]), "<think>"))
	m.Content = strings.TrimSpace(m.Content[i+len(thinkEnd):])
	if reasoning != "" {
		m.Thinking = strings.TrimSpace(strings.TrimSpace(m.Thinking) + "\n\n" + reasoning)
	}
}
