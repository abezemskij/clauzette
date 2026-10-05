package tools

import (
	"fmt"
	"strings"
)

type diffOp struct {
	kind byte // ' ' unchanged, '-' removed, '+' added
	text string
}

// maxLCSCells bounds the memory of the LCS table (int32 cells, ~16 MB).
// Larger changed regions fall back to "remove all, add all", which is still
// a correct diff, just not a minimal one.
const maxLCSCells = 4_000_000

func splitForDiff(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// diffLines strips the common prefix and suffix (edits are usually local)
// and runs a longest-common-subsequence diff on what remains.
func diffLines(a, b []string) []diffOp {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ops := make([]diffOp, 0, len(a)+len(b))
	for i := 0; i < pre; i++ {
		ops = append(ops, diffOp{' ', a[i]})
	}
	ops = append(ops, lcsDiff(a[pre:len(a)-suf], b[pre:len(b)-suf])...)
	for i := len(a) - suf; i < len(a); i++ {
		ops = append(ops, diffOp{' ', a[i]})
	}
	return ops
}

func lcsDiff(a, b []string) []diffOp {
	n, m := len(a), len(b)
	var ops []diffOp
	if n == 0 || m == 0 || n*m > maxLCSCells {
		for _, l := range a {
			ops = append(ops, diffOp{'-', l})
		}
		for _, l := range b {
			ops = append(ops, diffOp{'+', l})
		}
		return ops
	}
	// dp[i*w+j] = length of the LCS of a[i:] and b[j:]
	w := m + 1
	dp := make([]int32, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				dp[i*w+j] = dp[(i+1)*w+j+1] + 1
			case dp[(i+1)*w+j] >= dp[i*w+j+1]:
				dp[i*w+j] = dp[(i+1)*w+j]
			default:
				dp[i*w+j] = dp[i*w+j+1]
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case dp[(i+1)*w+j] >= dp[i*w+j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

// Diff returns a unified diff between two versions of a file plus the
// number of added and removed lines. It returns "" when nothing changed.
func Diff(name, oldText, newText string, context int) (string, int, int) {
	ops := diffLines(splitForDiff(oldText), splitForDiff(newText))
	var changes []int
	added, removed := 0, 0
	for i, op := range ops {
		switch op.kind {
		case '+':
			added++
			changes = append(changes, i)
		case '-':
			removed++
			changes = append(changes, i)
		}
	}
	if len(changes) == 0 {
		return "", 0, 0
	}
	// oldPos[i] / newPos[i]: number of old / new lines before ops[i].
	oldPos := make([]int, len(ops)+1)
	newPos := make([]int, len(ops)+1)
	for i, op := range ops {
		oldPos[i+1], newPos[i+1] = oldPos[i], newPos[i]
		if op.kind != '+' {
			oldPos[i+1]++
		}
		if op.kind != '-' {
			newPos[i+1]++
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", name, name)
	for k := 0; k < len(changes); {
		j := k
		for j+1 < len(changes) && changes[j+1]-changes[j]-1 <= 2*context {
			j++
		}
		start := changes[k] - context
		if start < 0 {
			start = 0
		}
		end := changes[j] + context
		if end > len(ops)-1 {
			end = len(ops) - 1
		}
		oldCount := oldPos[end+1] - oldPos[start]
		newCount := newPos[end+1] - newPos[start]
		oldStart, newStart := oldPos[start], newPos[start]
		if oldCount > 0 {
			oldStart++
		}
		if newCount > 0 {
			newStart++
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for i := start; i <= end; i++ {
			sb.WriteByte(ops[i].kind)
			sb.WriteString(ops[i].text)
			sb.WriteByte('\n')
		}
		k = j + 1
	}
	return sb.String(), added, removed
}
