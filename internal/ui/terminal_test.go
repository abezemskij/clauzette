package ui

import (
	"reflect"
	"strings"
	"testing"
)

// readAll runs readFrom over input and collects the messages it emits.
func readAll(input string) []inputLine {
	u := &UI{lines: make(chan inputLine)}
	go u.readFrom(strings.NewReader(input))
	var got []inputLine
	for ln := range u.lines {
		got = append(got, ln)
	}
	return got
}

func TestReadInput(t *testing.T) {
	const (
		start = "\x1b[200~"
		end   = "\x1b[201~"
	)
	cases := []struct {
		name string
		in   string
		want []inputLine
	}{
		{"plain lines", "hello\nworld\n", []inputLine{{text: "hello"}, {text: "world"}}},
		{"empty line kept", "\n", []inputLine{{text: ""}}},
		{"triple-quote block", "\"\"\"\nx\ny\n\"\"\"\n", []inputLine{{text: "x\ny", literal: true}}},
		{"paste is one literal message", start + "a\nb\nc" + end + "\n",
			[]inputLine{{text: "a\nb\nc", literal: true}}},
		{"paste preserves a leading slash", start + "/not a command" + end + "\n",
			[]inputLine{{text: "/not a command", literal: true}}},
		{"typed text and a paste are one message", "download this image " + start + "https://x.test/a.png" + end + "\n",
			[]inputLine{{text: "download this image https://x.test/a.png"}}},
		{"text after a paste joins it", start + "https://x.test/a.png" + end + " what is it?\n",
			[]inputLine{{text: "https://x.test/a.png what is it?", literal: true}}},
		{"typed command with a pasted argument", "/image " + start + "shot.png" + end + " why?\n",
			[]inputLine{{text: "/image shot.png why?"}}},
		{"two pastes in one message", "compare " + start + "a" + end + " and " + start + "b" + end + "\n",
			[]inputLine{{text: "compare a and b"}}},
		{"multi-line paste after typed text", "fix this:\n" + start + "line1\nline2" + end + "\n",
			[]inputLine{{text: "fix this:"}, {text: "line1\nline2", literal: true}}},
		{"multi-line paste mid-line", "see " + start + "l1\nl2" + end + " ok\n",
			[]inputLine{{text: "see l1\nl2 ok"}}},
		{"pasted triple quote is not a block", start + `"""` + end + "\n",
			[]inputLine{{text: `"""`, literal: true}}},
		{"paste inside a block", "\"\"\"\nx " + start + "p1\np2" + end + "\n\"\"\"\n",
			[]inputLine{{text: "x p1\np2", literal: true}}},
		{"empty paste alone is ignored", start + end + "\n", nil},
		{"input ends without Enter", "last words", []inputLine{{text: "last words"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := readAll(c.in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("readFrom(%q):\n got %#v\nwant %#v", c.in, got, c.want)
			}
		})
	}
}
