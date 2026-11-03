package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"clauzette/internal/config"
	"clauzette/internal/ollama"
)

func splitterFor(quirk bool, think *bool) *thinkSplitter {
	p := config.Profile{Name: "p", Model: "m"}
	if quirk {
		p.Quirks = []string{"inline-think"}
	}
	act := resolve(config.Defaults(), p, Caps{Known: true, Tools: true, Thinking: true}, "", nil)
	return newThinkSplitter(act, ollama.ChatRequest{Think: think})
}

// stream feeds chunks through a splitter and returns what was shown.
func stream(s *thinkSplitter, chunks ...string) (thinking, answer string) {
	var th, an strings.Builder
	for _, c := range chunks {
		t, a := s.delta(c)
		th.WriteString(t)
		an.WriteString(a)
	}
	an.WriteString(s.late())
	return th.String(), an.String()
}

func TestThinkSplitterTagAcrossChunks(t *testing.T) {
	off := false
	s := splitterFor(true, &off)
	// qwen3:4b with thinking off: no opening tag, the closing one split over chunks.
	th, an := stream(s, "Okay, let's see. The user wants", " a word.\n</th", "ink>\n\nhello")
	if th != "Okay, let's see. The user wants a word.\n" || an != "hello" {
		t.Fatalf("thinking %q, answer %q", th, an)
	}
	m := ollama.Message{Content: "Okay, let's see. The user wants a word.\n</think>\n\nhello"}
	s.fix(&m)
	if m.Content != "hello" || m.Thinking != "Okay, let's see. The user wants a word." {
		t.Fatalf("fixed message: %+v", m)
	}
}

func TestThinkSplitterNoTagIsTheAnswer(t *testing.T) {
	off := false
	s := splitterFor(true, &off)
	th, an := stream(s, "Just ", "an answer.")
	if an != "Just an answer." {
		t.Fatalf("a reply without the tag must still be shown as the answer: %q", an)
	}
	// What was passed on as thinking while waiting is the same text: the
	// UI shows it dimmed (or hidden) and then as the answer.
	if !strings.HasPrefix("Just an answer.", th) {
		t.Fatalf("thinking %q", th)
	}
	m := ollama.Message{Content: "Just an answer."}
	s.fix(&m)
	if m.Content != "Just an answer." || m.Thinking != "" {
		t.Fatalf("message changed without a tag: %+v", m)
	}
}

func TestThinkSplitterOpeningTag(t *testing.T) {
	s := splitterFor(true, nil) // think not sent
	th, an := stream(s, "<think>plan it</think>done")
	if th != "plan it" || an != "done" {
		t.Fatalf("thinking %q, answer %q", th, an)
	}
}

func TestThinkSplitterThinkingOnStreamsDirectly(t *testing.T) {
	on := true
	s := splitterFor(true, &on)
	if th, an := stream(s, "hello ", "world"); th != "" || an != "hello world" {
		t.Fatalf("with thinking on, text must stream as the answer: %q %q", th, an)
	}
	m := ollama.Message{Content: "stray</think>answer"}
	s.fix(&m)
	if m.Content != "answer" {
		t.Fatalf("a stray tag is still cleaned up: %+v", m)
	}
}

func TestThinkSplitterOffWithoutQuirk(t *testing.T) {
	off := false
	s := splitterFor(false, &off)
	if th, an := stream(s, "reasoning</think>hello"); th != "" || an != "reasoning</think>hello" {
		t.Fatalf("without the quirk nothing changes: %q %q", th, an)
	}
	m := ollama.Message{Content: "reasoning</think>hello"}
	s.fix(&m)
	if m.Content != "reasoning</think>hello" {
		t.Fatal("without the quirk the message must not change")
	}
}

func TestInlineThinkQuirkInAgent(t *testing.T) {
	r := newRogueRig(t, say("I should answer hello.\n</think>\n\nhello"), say("Summary first.</think>THE SUMMARY"))
	p := config.Profile{Name: "small", Model: "qwen3:4b", Quirks: []string{"inline-think"}}
	useProfile(r, p, Caps{Known: true, Tools: true, Thinking: true})
	r.a.SetThink(false)

	var mu sync.Mutex
	var text strings.Builder
	events := make(chan Event, 64)
	r.a.events = events
	go func() {
		for ev := range events {
			if ev.Kind == EvText {
				mu.Lock()
				text.WriteString(ev.Text)
				mu.Unlock()
			}
		}
	}()
	if err := r.a.RunTurn(context.Background(), "say hello"); err != nil {
		t.Fatal(err)
	}
	last := r.a.sess.Context[len(r.a.sess.Context)-1]
	if last.Content != "hello" || last.Thinking != "I should answer hello." {
		t.Fatalf("stored reply: content %q, thinking %q", last.Content, last.Thinking)
	}
	// Compaction summaries run with thinking off: the leak must not reach them.
	out, err := r.a.complete(context.Background(), "test", "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if out != "THE SUMMARY" {
		t.Fatalf("summary = %q", out)
	}
	close(events)
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(text.String(), "</think>") || !strings.Contains(text.String(), "hello") {
		t.Fatalf("shown answer: %q", text.String())
	}
}
