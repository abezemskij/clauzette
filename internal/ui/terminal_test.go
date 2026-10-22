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
		{"text before a paste on the same line", "hi " + start + "p" + end + "\n",
			[]inputLine{{text: "hi "}, {text: "p", literal: true}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := readAll(c.in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("readFrom(%q):\n got %#v\nwant %#v", c.in, got, c.want)
			}
		})
	}
}

func TestParsePlanArgs(t *testing.T) {
	cases := []struct {
		in         string
		total      int // pending actions
		approve    bool
		nums       []int
		comment    string
		noContinue bool
		wantErr    bool
	}{
		{in: "", total: 1, approve: true},
		{in: "all", total: 1, approve: true},
		{in: "1 3", total: 3, approve: true, nums: []int{1, 3}},
		{in: "1 but then stop and let's review the changes", total: 1, approve: true, nums: []int{1}, comment: "but then stop and let's review the changes"},
		{in: "all looks good,  continue", total: 1, approve: true, comment: "looks good,  continue"},
		{in: "1 -- 2 files only", total: 3, approve: true, nums: []int{1}, comment: "2 files only"},
		{in: "1 2 files only", total: 3, approve: true, nums: []int{1, 2}, comment: "files only"}, // why "--" exists
		{in: "1 2 files only", total: 1, approve: true, wantErr: true},                            // 2 is out of range
		{in: "all 3 more", total: 1, approve: true, comment: "3 more"},                            // after "all", numbers are comment
		{in: "looks good", total: 1, approve: true, wantErr: true},                                // no selection
		{in: "-- 2 files", total: 1, approve: true, wantErr: true},                                // no selection
		{in: "9", total: 1, approve: true, wantErr: true},
		{in: "", total: 0, approve: true, wantErr: true},
		{in: "--no-continue 2", total: 3, approve: true, nums: []int{2}, noContinue: true},
		{in: "2 --no-continue check it", total: 3, approve: true, nums: []int{2}, noContinue: true, comment: "check it"},
		{in: "1 wrong dir", total: 1, nums: []int{1}, comment: "wrong dir"}, // reject with a reason
		{in: "all", total: 1},                          // reject all
		{in: "--no-continue", total: 1, wantErr: true}, // not a reject option
	}
	for _, c := range cases {
		p, err := parsePlanArgs(c.in, c.total, c.approve)
		if (err != nil) != c.wantErr {
			t.Errorf("%q (total %d): err = %v, want error %v", c.in, c.total, err, c.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if !reflect.DeepEqual(p.nums, c.nums) || p.comment != c.comment || p.noContinue != c.noContinue {
			t.Errorf("%q: got nums=%v comment=%q noContinue=%v, want nums=%v comment=%q noContinue=%v",
				c.in, p.nums, p.comment, p.noContinue, c.nums, c.comment, c.noContinue)
		}
	}
}
