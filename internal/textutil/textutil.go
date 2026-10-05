// Package textutil holds small string helpers shared by the other packages.
package textutil

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Truncate shortens s to at most n runes, adding an ellipsis when it cuts.
func Truncate(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// OneLine collapses all whitespace (including newlines) and truncates.
func OneLine(s string, n int) string {
	return Truncate(strings.Join(strings.Fields(s), " "), n)
}

// FirstLine returns s up to the first newline.
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TruncateMiddle keeps the beginning and end of s and drops the middle,
// so the result is roughly n bytes long.
func TruncateMiddle(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	half := n / 2
	return strings.ToValidUTF8(s[:half], "") +
		fmt.Sprintf("\n[… %d characters omitted …]\n", len(s)-2*half) +
		strings.ToValidUTF8(s[len(s)-half:], "")
}

// KTok formats a token count compactly: 950, 4.2k, 87k.
func KTok(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 10000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%dk", (n+500)/1000)
	}
}

// HumanBytes formats a byte count: 512 B, 3.4 KB, 17.0 GB.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
