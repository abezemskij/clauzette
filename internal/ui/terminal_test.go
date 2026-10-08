package ui

import (
	"reflect"
	"testing"
)

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
