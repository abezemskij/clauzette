package control

import (
	"fmt"

	"clauzette/internal/config"
)

// Status is what a front end shows around the conversation: the banner and
// the prompt line. It is a snapshot; ask again after anything changes.
type Status struct {
	Session        string `json:"session"`
	Model          string `json:"model"`     // "model (profile p) on cloud", for the banner
	Server         string `json:"server"`    // the model server's URL
	Workspace      string `json:"workspace"` // the workspace, with the shared part when agents share one
	UsedTokens     int    `json:"used_tokens"`
	Window         int    `json:"window"`
	Think          bool   `json:"think"`
	ThinkAvailable bool   `json:"think_available"`
	ShowThinking   bool   `json:"show_thinking"`
	ChatOnly       bool   `json:"chat_only"`
	Cloud          string `json:"cloud,omitempty"`
	GPU            string `json:"gpu,omitempty"` // the GPU guard's state, when it has something to say
	Warm           bool   `json:"warm"`
	Rogue          bool   `json:"rogue"`
	Busy           bool   `json:"busy"`
	Approval       bool   `json:"approval"` // an approval request is waiting for an answer
	CompactAt      int    `json:"compact_at"`
	Vision         bool   `json:"vision"`
	Safe           bool   `json:"safe"`
	Plan           bool   `json:"plan"`
	PlanPending    int    `json:"plan_pending"`
	FetchGate      bool   `json:"fetch_gate"`
	Todos          int    `json:"todos"` // items in the agent's task plan
}

func (c *Controller) Status() Status {
	a := c.agent
	cfg := c.opt.Deps.Cfg
	act := a.Model()
	model := act.Model
	if act.Profile != config.ImplicitProfile && act.Profile != act.Model {
		model = fmt.Sprintf("%s (profile %s)", act.Model, act.Profile)
	}
	server := cfg.OllamaURL
	if act.Cloud != "" {
		model += " on " + act.Cloud
		_, pc := cfg.ProviderFor(act.Source())
		server = pc.BaseURL
	}
	where := cfg.Workspace
	if ws := c.opt.Deps.WS; ws != nil {
		where = ws.Root
		if ws.HasAgentDir() {
			where = fmt.Sprintf("%s (shared %s: %s)", ws.Root, ws.Share, ws.Access)
		}
	}
	todos := 0
	if t := c.opt.Deps.Todos; t != nil {
		todos = len(t.Items())
	}
	return Status{
		Session:        a.Session().ID,
		Model:          model,
		Server:         server,
		Workspace:      where,
		UsedTokens:     a.UsedTokens(),
		Window:         a.Window(),
		Think:          a.Think(),
		ThinkAvailable: a.ThinkAvailable(),
		ShowThinking:   a.Show(),
		ChatOnly:       a.ChatOnly(),
		Cloud:          act.Cloud,
		GPU:            c.opt.Deps.Guard.Status(),
		Warm:           c.warm,
		Rogue:          a.Rogue(),
		Busy:           c.busy,
		Approval:       c.pending != nil,
		CompactAt:      a.CompactAt(),
		Vision:         a.Vision(),
		Safe:           a.Safe(),
		Plan:           a.Plan(),
		PlanPending:    a.PlanPending(),
		FetchGate:      a.FetchGate(),
		Todos:          todos,
	}
}
