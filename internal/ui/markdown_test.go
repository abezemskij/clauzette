package ui

import (
	"math/rand"
	"strings"
	"testing"
)

// visible makes the escape sequences readable in test output.
var visible = strings.NewReplacer(
	cReset, "{/}", cBold, "{b}", cDim, "{dim}", cItalic, "{i}", cUnder, "{u}", cCyan, "{code}",
)

// renderChunks renders src split into pieces of the given sizes (cycled),
// then ends the reply.
func renderChunks(src string, plain bool, sizes ...int) string {
	var sb strings.Builder
	m := newMarkdown(func(s string) { sb.WriteString(s) }, plain)
	rs := []rune(src)
	for i, k := 0, 0; i < len(rs); k++ {
		n := 1 << 30
		if len(sizes) > 0 {
			n = sizes[k%len(sizes)]
		}
		if i+n > len(rs) {
			n = len(rs) - i
		}
		m.Write(string(rs[i : i+n]))
		i += n
	}
	m.Reset()
	return sb.String()
}

func render(src string) string { return visible.Replace(renderChunks(src, false)) }

func TestMarkdownBlocks(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain text", "hello world\n", "hello world\n"},
		{"heading", "# Title\n## Sub\n", "{/}{b}{u}Title{/}\n{/}{b}Sub{/}\n"},
		{"not a heading", "#hashtag and #1\n", "#hashtag and #1\n"},
		{"bullets", "- one\n* two\n  + three\n", "{/}{dim}•{/} one\n{/}{dim}•{/} two\n  {/}{dim}•{/} three\n"},
		{"numbered list as written", "1. first\n2. second\n", "1. first\n2. second\n"},
		{"quote", "> wise words\n", "{/}{dim}│ {/}{i}wise words{/}\n"},
		{"rule", "---\n", "{/}{dim}" + strings.Repeat("─", 40) + "{/}\n"},
		{"dashes in text", "-- not a rule\n-x\n", "-- not a rule\n-x\n"},
		{"code block", "```go\nx := *p // **not bold**\n```\nafter\n",
			"{/}{dim}```go{/}\n{/}{code}x := *p // **not bold**{/}\n{/}{dim}```{/}\nafter\n"},
		{"fence-like line inside code", "```\n```go\n```\n", "{/}{dim}```{/}\n{/}{code}```go{/}\n{/}{dim}```{/}\n"},
		{"unclosed fence ends with the reply", "```\ncode", "{/}{dim}```{/}\n{/}{code}code{/}"},
		{"table as written", "| a | b |\n|---|---|\n", "| a | b |\n|---|---|\n"},
	}
	for _, c := range cases {
		if got := render(c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestMarkdownInline(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"bold", "a **b** c\n", "a {/}{b}b{/} c\n"},
		{"italic", "a *b* c\n", "a {/}{i}b{/} c\n"},
		{"bold italic", "***x***\n", "{/}{b}{i}x{/}\n"},
		{"code", "run `go test ./...` now\n", "run {/}{code}go test ./...{/} now\n"},
		{"stars in code", "`a*b**c`\n", "{/}{code}a*b**c{/}\n"},
		{"double backticks", "``a ` b``\n", "{/}{code}a ` b{/}\n"},
		{"arithmetic stays", "2 * 3 * 4 and a*b\n", "2 * 3 * 4 and a*b\n"},
		{"glob stays", "rm *.tmp and ls src/*/main.go\n", "rm *.tmp and ls src/*/main.go\n"},
		{"emphasis opens before code", "**`go vet`** passes\n", "{/}{b}{/}{code}go vet{/}{b}{/} passes\n"},
		{"underscores stay", "snake_case_name and _x_\n", "snake_case_name and _x_\n"},
		{"unclosed bold ends at the line", "**open\nnext\n", "{/}{b}open{/}\nnext\n"},
		{"link", "see [the docs](https://go.dev/doc) here\n", "see {/}{u}the docs{/}{dim} (https://go.dev/doc){/} here\n"},
		{"bare link text", "[https://x.dev](https://x.dev)\n", "{/}{u}https://x.dev{/}\n"},
		{"not a link", "a[0] and [x] (y)\n", "a[0] and [x] (y)\n"},
		{"bold in a heading", "## The **key** part\n", "{/}{b}The {/}{b}{b}key{/}{b} part{/}\n"},
		{"bold in a bullet", "- **Fix:** done\n", "{/}{dim}•{/} {/}{b}Fix:{/} done\n"},
	}
	for _, c := range cases {
		if got := render(c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// The stream can be cut anywhere; the result must not depend on where.
func TestMarkdownChunking(t *testing.T) {
	doc := "# Plan\n\nSome *intro* text with `code` and a [link](https://example.com).\n\n" +
		"- **First:** do it\n- second * item\n  * nested\n\n> quoted **bold**\n\n---\n\n" +
		"```sh\nls -la *.go\n```\n\n1. one\n2. two\n| t | u |\n" + "trailing **bold"
	whole := renderChunks(doc, false)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		sizes := []int{1 + rng.Intn(7), 1 + rng.Intn(3), 1 + rng.Intn(11)}
		if got := renderChunks(doc, false, sizes...); got != whole {
			t.Fatalf("chunks %v differ:\n got %q\nwant %q", sizes, visible.Replace(got), visible.Replace(whole))
		}
	}
	if got := renderChunks(doc, false, 1); got != whole {
		t.Fatal("rune by rune differs")
	}
}

func TestMarkdownPlainAndSanitize(t *testing.T) {
	in := "# Title **b**\n"
	if got := renderChunks(in, true, 3); got != in {
		t.Fatalf("plain mode changed the text: %q", got)
	}
	evil := "ok\x1b]0;pwned\x07 \x1b[2J\rdone\n"
	for _, plain := range []bool{true, false} {
		got := renderChunks(evil, plain)
		if strings.ContainsAny(strings.ReplaceAll(got, "\x1b[0m", ""), "\x1b\x07\r") {
			t.Fatalf("control characters passed through (plain %v): %q", plain, got)
		}
		if !strings.Contains(got, "ok]0;pwned [2Jdone") {
			t.Fatalf("text lost (plain %v): %q", plain, got)
		}
	}
}

// An interruption (an info line while text streams) flushes pending text,
// keeps an open code block, and starts a fresh line afterwards.
func TestMarkdownFlush(t *testing.T) {
	var sb strings.Builder
	m := newMarkdown(func(s string) { sb.WriteString(s) }, false)
	m.Write("```\nline one\nhalf")
	m.Flush()
	sb.WriteString("|INFO|\n")
	m.Write("still code\n```\n**b")
	m.Reset()
	want := "{/}{dim}```{/}\n{/}{code}line one{/}\n{/}{code}half{/}|INFO|\n{/}{code}still code{/}\n{/}{dim}```{/}\n{/}{b}b{/}"
	if got := visible.Replace(sb.String()); got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}
