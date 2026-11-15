package control

import (
	"reflect"
	"strings"
	"testing"

	"clauzette/internal/agent"
	"clauzette/internal/config"
	"clauzette/internal/session"
	"clauzette/internal/tools"
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

func TestSplitPath(t *testing.T) {
	cases := []struct{ in, path, rest string }{
		{"shot.png", "shot.png", ""},
		{"shot.png what is wrong here?", "shot.png", "what is wrong here?"},
		{`"my shot.png" why`, "my shot.png", "why"},
		{`'a b.png'`, "a b.png", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		p, r, err := splitPath(c.in)
		if err != nil || p != c.path || r != c.rest {
			t.Errorf("%q: %q %q %v", c.in, p, r, err)
		}
	}
	if _, _, err := splitPath(`"open.png`); err == nil {
		t.Error("unclosed quote accepted")
	}
}

// recView records what the controller reports.
type recView struct {
	calls    []string
	commands []string // names offered to Command
	claim    map[string]bool
}

func (v *recView) add(s string)                                     { v.calls = append(v.calls, s) }
func (v *recView) Render(ev agent.Event)                            { v.add("render " + ev.Text) }
func (v *recView) Info(m string)                                    { v.add("info " + m) }
func (v *recView) Warn(m string)                                    { v.add("warn " + m) }
func (v *recView) Error(m string)                                   { v.add("error " + m) }
func (v *recView) Block(t string)                                   { v.add("block " + t) }
func (v *recView) Dim(l string)                                     { v.add("dim " + l) }
func (v *recView) Echo(t string)                                    { v.add("echo " + t) }
func (v *recView) ShowApproval(r *agent.ApprovalRequest, full bool) { v.add("approval " + r.Summary) }
func (v *recView) AskApproval(r *agent.ApprovalRequest)             { v.add("ask " + r.Summary) }
func (v *recView) Prompt()                                          { v.add("prompt") }
func (v *recView) RefreshPrompt()                                   { v.add("refresh") }
func (v *recView) Working()                                         { v.add("working") }
func (v *recView) StopWaiting()                                     {}
func (v *recView) EndStream()                                       {}
func (v *recView) Command(name string, args []string, raw string) bool {
	v.commands = append(v.commands, name)
	return v.claim[name]
}

func (v *recView) has(prefix string) bool {
	for _, c := range v.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// The controller works without any terminal: commands, front-end
// commands, the queue before the model is ready, and approvals all go
// through the View.
func TestControllerWithoutTerminal(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	deps := agent.Deps{Cfg: cfg, Registry: tools.NewRegistry(), Store: store}
	v := &recView{claim: map[string]bool{"/help": true}}
	c := New(Options{Deps: deps}, &session.Session{ID: "s"}, v)

	c.Handle(Line{Text: "/help"})
	if len(v.commands) != 1 || v.commands[0] != "/help" || v.has("warn") {
		t.Fatalf("/help was not left to the front end: %v %v", v.commands, v.calls)
	}
	c.Handle(Line{Text: "/bogus"})
	if !v.has("warn unknown command /bogus") {
		t.Fatalf("unknown command: %v", v.calls)
	}
	c.Handle(Line{Text: "!c"})
	if !v.has("info nothing to cancel") {
		t.Fatalf("!c while idle: %v", v.calls)
	}
	c.Handle(Line{Text: "/notacommand", Literal: true})
	if !v.has("info queued; it will be sent once the model is ready") {
		t.Fatalf("a literal message before warm-up was not queued: %v", v.calls)
	}
	if c.Busy() || c.Warm() {
		t.Fatal("nothing should be running")
	}

	reply := make(chan agent.Decision, 1)
	c.HandleEvent(agent.Event{Kind: agent.EvApproval, Approval: &agent.ApprovalRequest{Tool: "write_file", Summary: "create a.txt", Reply: reply}})
	if !v.has("approval create a.txt") {
		t.Fatalf("approval not shown: %v", v.calls)
	}
	c.Handle(Line{Text: "maybe"})
	if !v.has("ask create a.txt") {
		t.Fatalf("an unclear answer did not ask again: %v", v.calls)
	}
	c.Handle(Line{Text: "n not like this"})
	if d := <-reply; d.Approve || d.Reason != "not like this" {
		t.Fatalf("decision %+v", d)
	}
}
