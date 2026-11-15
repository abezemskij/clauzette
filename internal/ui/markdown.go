package ui

import (
	"strings"
	"unicode"
)

// mdRenderer renders the model's Markdown for the terminal while it
// streams. It never moves the cursor: text is styled as it arrives, and
// only short ambiguous pieces wait (the start of a line until its kind is
// known, a run of * or `, a [link](url) until it closes), so a reply
// appears as fast as before.
//
// Rendered: headings, **bold**, *italic*, `code`, fenced code blocks
// (fence lines kept, dimmed, so copied code stays clean), - * + bullets,
// > quotes, horizontal rules and [links](url). Kept as written: _ (it is
// in too many identifiers), tables, numbered lists, HTML.
//
// With plain set (no colour), text passes through unchanged. Either way
// control characters are removed, so model output cannot send escape
// sequences to the terminal.
type mdRenderer struct {
	out   func(string)
	plain bool

	// block state, kept across interruptions within one reply
	fence     string // the open code fence ("```" or "~~~"), "" outside one
	fenceLang string

	// line state
	atStart bool            // still deciding what kind of line this is
	head    strings.Builder // the buffered start of the line
	kind    int             // the line's kind once decided
	level   int             // heading level

	// inline state
	bold, italic bool
	code         int     // length of the backtick run that opened inline code; 0 = none
	stars        int     // a pending run of *
	ticks        int     // a pending run of `
	prev         rune    // the last character written ('\n' at a line start)
	link         *mdLink // a [link](url) being collected
	style        string  // the SGR sequence in effect
}

type mdLink struct {
	text, url strings.Builder
	inURL     bool // after "]("
	closed    bool // saw "]", waiting for "("
}

const (
	lineText = iota
	lineHeading
	lineQuote
	lineCode
	lineFence
	lineRule
)

const mdMaxHead = 12 // longest line start that can still change its kind

func newMarkdown(out func(string), plain bool) *mdRenderer {
	m := &mdRenderer{out: out, plain: plain}
	m.startLine()
	return m
}

// sanitize removes control characters except tab and newline.
func sanitize(s string) string {
	ok := true
	for _, r := range s {
		if r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0)) {
			ok = false
			break
		}
	}
	if ok {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0)) {
			return -1
		}
		return r
	}, s)
}

// Write renders the next piece of the stream.
func (m *mdRenderer) Write(s string) {
	s = sanitize(s)
	if m.plain {
		m.out(s)
		return
	}
	for _, r := range s {
		m.feed(r)
	}
}

// Flush writes whatever is pending and ends the current line's styles,
// because something else is about to be printed. An open code block stays
// open: the reply may continue.
func (m *mdRenderer) Flush() {
	if m.plain {
		return
	}
	if m.atStart && m.head.Len() > 0 {
		m.decide(true)
	}
	m.flushInline(' ')
	m.setStyle("")
	m.startLine()
}

// Reset ends the reply: flushes and forgets an unclosed code block.
func (m *mdRenderer) Reset() {
	m.Flush()
	m.fence, m.fenceLang = "", ""
}

func (m *mdRenderer) startLine() {
	m.atStart = true
	m.head.Reset()
	m.kind, m.level = lineText, 0
	m.bold, m.italic, m.code = false, false, 0
	m.stars, m.ticks = 0, 0
	m.prev = '\n'
	m.link = nil
}

func (m *mdRenderer) feed(r rune) {
	if r == '\n' {
		if m.atStart {
			m.decide(true)
		}
		m.flushInline('\n')
		m.endLine()
		return
	}
	if m.atStart {
		m.head.WriteRune(r)
		m.decide(false)
		return
	}
	m.inline(r)
}

func (m *mdRenderer) endLine() {
	m.setStyle("")
	m.out("\n")
	m.startLine()
}

// decide classifies the buffered line start once it can. final means the
// line ended (or output is interrupted), so it must decide now.
func (m *mdRenderer) decide(final bool) {
	h := m.head.String()
	indent := len(h) - len(strings.TrimLeft(h, " \t"))
	body := h[indent:]

	if m.fence != "" { // inside a code block: a code line or the closing fence
		trimmed := strings.TrimRight(body, " \t")
		if strings.HasPrefix(trimmed, m.fence) && strings.Trim(trimmed, m.fence[:1]) == "" {
			if !final {
				return // may still be the closing fence; wait for the newline
			}
			m.kind = lineFence
			m.atStart = false
			m.setStyle(cDim)
			m.out(h)
			m.fence, m.fenceLang = "", ""
			return
		}
		if !final && len(trimmed) < len(m.fence) && strings.HasPrefix(m.fence, trimmed) && indent < 4 {
			return
		}
		m.kind = lineCode
		m.atStart = false
		m.setStyle(cCyan)
		m.out(h)
		return
	}

	switch {
	case strings.HasPrefix(body, "```") || strings.HasPrefix(body, "~~~"):
		if !final {
			return // the language follows; show the fence line once complete
		}
		m.fence = body[:3]
		m.fenceLang = strings.TrimSpace(strings.TrimLeft(body, body[:1]))
		m.kind = lineFence
		m.atStart = false
		m.setStyle(cDim)
		m.out(h)
		return
	case !final && (body == "`" || body == "``" || body == "~" || body == "~~"):
		return
	}

	if n := headingLevel(body); n > 0 {
		m.kind, m.level = lineHeading, n
		m.atStart = false
		m.out(h[:indent])
		m.setStyle(m.inlineStyle())
		m.replay(strings.TrimLeft(body[n:], " "))
		return
	} else if n < 0 && !final {
		return // "#", "##" ... without the space yet
	}

	if isRule(body) {
		if !final {
			return // "---" may still be a rule or turn into text
		}
		m.kind = lineRule
		m.atStart = false
		m.out(h[:indent])
		m.setStyle(cDim)
		m.out(strings.Repeat("─", 40))
		return
	}

	if len(body) >= 2 && strings.ContainsRune("-*+", rune(body[0])) && body[1] == ' ' {
		// "- x", "* x", "+ x" are bullets; "- -" or "* *" may still become a rule
		if strings.Trim(strings.TrimSpace(body), string(body[0])+" ") == "" && !final && len(body) < mdMaxHead {
			return // "- -" or "* *" may become a rule
		}
		m.atStart = false
		m.out(h[:indent])
		m.setStyle(cDim)
		m.out("•")
		m.setStyle("")
		m.out(" ")
		m.replay(body[2:])
		return
	}
	if !final && (body == "-" || body == "*" || body == "+" || body == "") {
		return
	}

	if strings.HasPrefix(body, ">") {
		if !final && body == ">" {
			return
		}
		m.kind = lineQuote
		m.atStart = false
		m.out(h[:indent])
		m.setStyle(cDim)
		m.out("│ ")
		m.setStyle(m.inlineStyle())
		m.replay(strings.TrimPrefix(strings.TrimPrefix(body, ">"), " "))
		return
	}

	if !final && len(body) < mdMaxHead && couldBeMarker(body) {
		return
	}
	m.atStart = false
	m.out(h[:indent])
	m.replay(body)
}

// replay runs buffered text through the inline renderer.
func (m *mdRenderer) replay(s string) {
	for _, r := range s {
		m.inline(r)
	}
}

// headingLevel is n for "#…# " (1-6 #s and a space), -n for #s still
// waiting for their space, 0 otherwise.
func headingLevel(s string) int {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0
	}
	if n == len(s) {
		return -n
	}
	if s[n] == ' ' {
		return n
	}
	return 0
}

// isRule reports a line of three or more -, * or _ (spaces allowed).
func isRule(s string) bool {
	t := strings.ReplaceAll(strings.TrimSpace(s), " ", "")
	if len(t) < 3 {
		return false
	}
	c := t[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	return strings.Trim(t, string(c)) == ""
}

// couldBeMarker reports a line start that a later character could still
// turn into a rule or a bullet.
func couldBeMarker(s string) bool {
	t := strings.ReplaceAll(s, " ", "")
	if t == "" {
		return true
	}
	c := t[0]
	return (c == '-' || c == '*' || c == '_') && strings.Trim(t, string(c)) == ""
}

// ------------------------------------------------------------ inline

func (m *mdRenderer) inline(r rune) {
	if m.kind == lineCode || m.kind == lineFence || m.kind == lineRule {
		m.emit(r)
		return
	}
	if m.link != nil {
		m.linkRune(r)
		return
	}
	if r == '`' {
		if m.stars > 0 {
			m.resolveStars(r)
		}
		m.ticks++
		return
	}
	if m.ticks > 0 {
		m.resolveTicks()
	}
	if m.code > 0 {
		m.emit(r)
		return
	}
	if r == '*' {
		m.stars++
		return
	}
	if m.stars > 0 {
		m.resolveStars(r)
	}
	if r == '[' {
		m.link = &mdLink{}
		return
	}
	m.emit(r)
}

// flushInline settles pending runs before a line break (next = '\n') or
// an interruption.
func (m *mdRenderer) flushInline(next rune) {
	if m.link != nil {
		m.abandonLink()
	}
	if m.ticks > 0 {
		m.resolveTicks()
	}
	if m.stars > 0 {
		m.resolveStars(next)
	}
}

func (m *mdRenderer) resolveTicks() {
	n := m.ticks
	m.ticks = 0
	switch {
	case m.code == 0:
		m.code = n
		m.setStyle(m.inlineStyle())
	case n == m.code:
		m.code = 0
		m.setStyle(m.inlineStyle())
	default:
		m.out(strings.Repeat("`", n)) // backticks inside code
	}
	m.prev = '`'
}

// resolveStars turns a run of * into bold/italic toggles or literal stars,
// judged by the characters on either side: "2 * 3" and "a*b" stay as they
// are.
func (m *mdRenderer) resolveStars(next rune) {
	n := m.stars
	m.stars = 0
	before := unicode.IsSpace(m.prev) || strings.ContainsRune("([{\"'", m.prev)
	// an opening run must be followed by a word, as in **Fix:** or *this*;
	// "*.tmp" and "* 3" are not emphasis
	after := unicode.IsLetter(next) || unicode.IsDigit(next) || strings.ContainsRune("`[(\"'", next)
	closeOK := m.prev != '\n' && !unicode.IsSpace(m.prev)

	toggle := func(on *bool) bool {
		switch {
		case *on && closeOK:
			*on = false
			return true
		case !*on && before && after:
			*on = true
			return true
		}
		return false
	}
	changed := false
	switch {
	case n >= 3:
		b, i := toggle(&m.bold), false
		if b {
			i = toggle(&m.italic)
		}
		changed = b || i
		if !changed {
			m.out(strings.Repeat("*", n))
		} else if !i {
			m.out("*")
		}
	case n == 2:
		changed = toggle(&m.bold)
		if !changed {
			m.out("**")
		}
	default:
		changed = toggle(&m.italic)
		if !changed {
			m.out("*")
		}
	}
	if changed {
		m.setStyle(m.inlineStyle())
	}
	m.prev = '*'
}

func (m *mdRenderer) linkRune(r rune) {
	l := m.link
	switch {
	case l.inURL:
		if r == ')' {
			m.writeLink()
			return
		}
		if r == ' ' || l.url.Len() > 500 {
			m.abandonLink()
			m.inline(r)
			return
		}
		l.url.WriteRune(r)
	case l.closed:
		if r == '(' {
			l.inURL = true
			return
		}
		m.abandonLink()
		m.inline(r)
	default:
		if r == ']' {
			l.closed = true
			return
		}
		if r == '[' || l.text.Len() > 300 {
			m.abandonLink()
			m.inline(r)
			return
		}
		l.text.WriteRune(r)
	}
}

// writeLink shows a complete link as its text, underlined, and the URL.
func (m *mdRenderer) writeLink() {
	l := m.link
	m.link = nil
	text, url := l.text.String(), l.url.String()
	base := m.inlineStyle()
	m.setStyle(base + cUnder)
	m.out(text)
	if url != text && url != "" {
		m.setStyle(base + cDim)
		m.out(" (" + url + ")")
	}
	m.setStyle(base)
	m.prev = ')'
}

// abandonLink writes a [ that did not become a link as plain text.
func (m *mdRenderer) abandonLink() {
	l := m.link
	m.link = nil
	s := "[" + l.text.String()
	if l.closed {
		s += "]"
	}
	if l.inURL {
		s += "(" + l.url.String()
	}
	m.emitString(s)
}

func (m *mdRenderer) emitString(s string) {
	for _, r := range s {
		m.emit(r)
	}
}

func (m *mdRenderer) emit(r rune) {
	m.out(string(r))
	m.prev = r
}

// inlineStyle is the SGR sequence for the current line and inline state.
func (m *mdRenderer) inlineStyle() string {
	var s string
	switch m.kind {
	case lineHeading:
		s = cBold
		if m.level == 1 {
			s += cUnder
		}
	case lineQuote:
		s = cItalic
	}
	if m.code > 0 {
		return s + cCyan
	}
	if m.bold {
		s += cBold
	}
	if m.italic {
		s += cItalic
	}
	return s
}

// setStyle switches to an SGR sequence ("" = plain), emitting only changes.
func (m *mdRenderer) setStyle(s string) {
	if s == m.style {
		return
	}
	m.out(cReset + s)
	m.style = s
}
