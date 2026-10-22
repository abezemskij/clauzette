package tools

import (
	"fmt"
	"html"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// htmltext.go converts an HTML page to readable plain text, for fetch_url. The
// Go standard library has no HTML parser (that is golang.org/x/net/html, a
// dependency clauzette does not take), so this is a small tolerant scanner, not
// a DOM parse: it drops tag markup, skips whole noise elements (script, style,
// ...), decodes entities, and collapses whitespace. Links are preserved as a
// reference list so an agent -- including one running unattended in rogue mode --
// still has real addresses to follow: <a href> text becomes `text[n]`, with the
// resolved, absolute, deduped URLs listed under a trailing "Links:" section.

const defaultMaxLinks = 50

type linkRef struct {
	ref int
	url string
}

// skipElement names elements whose entire contents are dropped. They all have
// reliable close tags in practice; <head> is deliberately NOT here, because its
// close tag is optional and often omitted, which would eat the whole document.
func skipElement(name string) bool {
	switch name {
	case "script", "style", "noscript", "template", "svg", "iframe", "object", "canvas":
		return true
	}
	return false
}

// blockElement names elements that should force a line break, so paragraphs and
// list items do not smear onto one line. (br, a, img, pre are handled on their
// own and are not listed here.)
func blockElement(name string) bool {
	switch name {
	case "p", "div", "section", "article", "header", "footer", "nav", "aside",
		"main", "ul", "ol", "li", "dl", "dt", "dd", "table", "tr", "thead",
		"tbody", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "figure",
		"figcaption", "form", "fieldset", "hr":
		return true
	}
	return false
}

// htmlToText renders src (an HTML document) as plain text. base is the page's
// final URL, used to resolve relative links to absolute; maxLinks (<=0 means the
// default) caps how many unique links are kept so a nav-heavy page cannot flood
// the output.
func htmlToText(src string, base *url.URL, maxLinks int) string {
	if maxLinks <= 0 {
		maxLinks = defaultMaxLinks
	}
	var buf []byte
	var links []linkRef
	linkIdx := map[string]int{}
	inPre := 0
	pendingRef := 0

	writeBreak := func() {
		for len(buf) > 0 && (buf[len(buf)-1] == ' ' || buf[len(buf)-1] == '\t') {
			buf = buf[:len(buf)-1]
		}
		// at most one blank line
		if len(buf) >= 2 && buf[len(buf)-1] == '\n' && buf[len(buf)-2] == '\n' {
			return
		}
		buf = append(buf, '\n')
	}
	// writeText appends already-unescaped text, collapsing whitespace runs to a
	// single space unless inside <pre>.
	writeText := func(seg string) {
		if seg == "" {
			return
		}
		if inPre > 0 {
			buf = append(buf, seg...)
			return
		}
		for _, r := range seg {
			if unicode.IsSpace(r) {
				if len(buf) == 0 {
					continue
				}
				if last := buf[len(buf)-1]; last == ' ' || last == '\n' {
					continue
				}
				buf = append(buf, ' ')
			} else {
				buf = utf8.AppendRune(buf, r)
			}
		}
	}

	i, n := 0, len(src)
	for i < n {
		if src[i] == '<' {
			if strings.HasPrefix(src[i:], "<!--") {
				e := strings.Index(src[i+4:], "-->")
				if e < 0 {
					break // unterminated comment: drop the rest
				}
				i += 4 + e + 3
				continue
			}
			inner, adv := readTag(src[i:])
			if adv == 0 { // a stray '<' with no closing '>'
				writeText("<")
				i++
				continue
			}
			name, isClose, attrs := parseTag(inner)
			i += adv
			if name == "" { // doctype, processing instruction, etc.
				continue
			}
			lname := strings.ToLower(name)
			switch {
			case skipElement(lname) && !isClose:
				i = findClose(src, i, lname)
			case lname == "pre":
				if isClose {
					if inPre > 0 {
						inPre--
					}
					writeBreak()
				} else {
					writeBreak()
					inPre++
				}
			case lname == "br" && !isClose:
				writeBreak()
			case lname == "a" && !isClose:
				pendingRef = 0
				if abs := resolveLink(base, attrs["href"]); abs != "" {
					if ref, ok := linkIdx[abs]; ok {
						pendingRef = ref
					} else if len(links) < maxLinks {
						ref = len(links) + 1
						linkIdx[abs] = ref
						links = append(links, linkRef{ref, abs})
						pendingRef = ref
					}
				}
			case lname == "a" && isClose:
				if pendingRef > 0 {
					buf = append(buf, fmt.Sprintf("[%d]", pendingRef)...)
				}
				pendingRef = 0
			case lname == "img" && !isClose:
				if alt := strings.TrimSpace(html.UnescapeString(attrs["alt"])); alt != "" {
					writeText(" " + alt + " ")
				}
			case blockElement(lname):
				writeBreak()
			}
			continue
		}
		// text run: copy up to the next '<'
		j := strings.IndexByte(src[i:], '<')
		if j < 0 {
			j = n - i
		}
		writeText(html.UnescapeString(src[i : i+j]))
		i += j
	}

	text := strings.TrimSpace(string(buf))
	if len(links) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString(text)
	b.WriteString("\n\nLinks:\n")
	for _, l := range links {
		fmt.Fprintf(&b, "[%d] %s\n", l.ref, l.url)
	}
	return strings.TrimRight(b.String(), "\n")
}

// readTag takes a string starting at '<' and returns the text between the
// brackets and the number of bytes up to and including '>'. adv is 0 when there
// is no closing '>'. Quotes are respected so a '>' inside an attribute value
// does not end the tag early.
func readTag(s string) (inner string, adv int) {
	var q byte
	for i := 1; i < len(s); i++ {
		c := s[i]
		if q != 0 {
			if c == q {
				q = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			q = c
		case '>':
			return s[1:i], i + 1
		}
	}
	return "", 0
}

// parseTag splits a tag's inner text into its name, whether it is a close tag,
// and its attributes (lowercased keys). Attributes are only parsed for open
// tags, since that is all the caller needs.
func parseTag(inner string) (name string, isClose bool, attrs map[string]string) {
	s := strings.TrimSpace(inner)
	if s == "" {
		return "", false, nil
	}
	if s[0] == '/' {
		isClose = true
		s = strings.TrimSpace(s[1:])
	}
	i := 0
	for i < len(s) && isNameByte(s[i]) {
		i++
	}
	name = s[:i]
	if name == "" || isClose {
		return name, isClose, nil
	}
	return name, false, parseAttrs(s[i:])
}

func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == ':'
}

func isSpaceByte(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }

// parseAttrs reads name="v" / name='v' / name=v / name from a tag's attribute
// text. It is lenient: malformed attributes are skipped rather than failing.
func parseAttrs(s string) map[string]string {
	m := map[string]string{}
	i, n := 0, len(s)
	for i < n {
		for i < n && (isSpaceByte(s[i]) || s[i] == '/') {
			i++
		}
		if i >= n {
			break
		}
		ks := i
		for i < n && !isSpaceByte(s[i]) && s[i] != '=' && s[i] != '/' {
			i++
		}
		key := strings.ToLower(s[ks:i])
		for i < n && isSpaceByte(s[i]) {
			i++
		}
		if i < n && s[i] == '=' {
			i++
			for i < n && isSpaceByte(s[i]) {
				i++
			}
			var val string
			if i < n && (s[i] == '"' || s[i] == '\'') {
				q := s[i]
				i++
				vs := i
				for i < n && s[i] != q {
					i++
				}
				val = s[vs:i]
				if i < n {
					i++
				}
			} else {
				vs := i
				for i < n && !isSpaceByte(s[i]) {
					i++
				}
				val = s[vs:i]
			}
			if key != "" {
				m[key] = val
			}
		} else if key != "" {
			m[key] = ""
		}
	}
	return m
}

// findClose returns the index just past the matching </name> close tag, scanning
// case-insensitively without allocating, so byte indices stay aligned with src
// even when the document contains multi-byte text. If there is no close tag, it
// returns len(s) (skip to the end).
func findClose(s string, from int, name string) int {
	for i := from; i < len(s); i++ {
		if s[i] != '<' || i+1 >= len(s) || s[i+1] != '/' {
			continue
		}
		j := i + 2
		if hasPrefixFold(s[j:], name) {
			j += len(name)
			if j >= len(s) || s[j] == '>' || isSpaceByte(s[j]) || s[j] == '/' {
				for j < len(s) && s[j] != '>' {
					j++
				}
				if j < len(s) {
					j++
				}
				return j
			}
		}
	}
	return len(s)
}

// hasPrefixFold reports whether s begins with prefix, ignoring ASCII case.
// prefix is expected to be lowercase ASCII (an element name).
func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for k := 0; k < len(prefix); k++ {
		c := s[k]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != prefix[k] {
			return false
		}
	}
	return true
}

// resolveLink turns an href into an absolute http(s) URL against base, or ""
// when it is empty, an in-page anchor, or a non-navigable scheme.
func resolveLink(base *url.URL, href string) string {
	href = strings.TrimSpace(html.UnescapeString(href))
	if href == "" || strings.HasPrefix(href, "#") {
		return ""
	}
	switch {
	case hasSchemePrefix(href, "javascript:"), hasSchemePrefix(href, "mailto:"),
		hasSchemePrefix(href, "tel:"), hasSchemePrefix(href, "data:"):
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	u.Fragment = "" // the fragment does not change what fetch_url would retrieve
	return u.String()
}

func hasSchemePrefix(s, scheme string) bool {
	return len(s) >= len(scheme) && strings.EqualFold(s[:len(scheme)], scheme)
}

// isHTMLType reports whether a Content-Type header names an HTML document.
func isHTMLType(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml")
}

// htmlReadCap is how many raw HTML bytes to read for a given cleaned-text output
// cap. HTML shrinks several-fold once stripped, so read generously (bounded) to
// avoid truncating the source mid-document before it can be cleaned.
func htmlReadCap(outLimit int) int {
	c := outLimit * 8
	if c < 512<<10 {
		c = 512 << 10
	}
	if c > 2<<20 {
		c = 2 << 20
	}
	return c
}

// cutRunes truncates s to at most limit bytes, stepping back to a rune boundary.
func cutRunes(s string, limit int) string {
	if limit >= len(s) {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}
