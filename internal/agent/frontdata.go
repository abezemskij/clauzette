package agent

import (
	"context"
	"strings"

	"clauzette/internal/config"
	"clauzette/internal/llm"
	"clauzette/internal/ollama"
)

// Data for front ends that show lists as data rather than as text (the web
// page). The terminal uses the text reports (ModelList, PlanReport).

// ModelChoice is one entry of a model picker: a profile, or an installed
// model without one. Name is what /model takes.
type ModelChoice struct {
	Name     string `json:"name"`
	Model    string `json:"model"`
	Profile  bool   `json:"profile"`         // a profile from the config (else an installed model)
	Cloud    string `json:"cloud,omitempty"` // the provider, for a cloud model
	Known    bool   `json:"known"`           // the capabilities below were reported
	Tools    bool   `json:"tools"`
	Thinking bool   `json:"thinking"`
	Vision   bool   `json:"vision"`
	Context  int    `json:"context,omitempty"` // native context length
	Loaded   bool   `json:"loaded"`
	Active   bool   `json:"active"`
}

// ModelChoices lists the profiles, then the installed models that have no
// profile.
func (a *Agent) ModelChoices(ctx context.Context) ([]ModelChoice, error) {
	act := a.Models.Active()
	list, listErr := a.Models.Local.ListModels(ctx)
	var running []ollama.RunningModel
	if local, ok := a.Models.Local.(llm.Local); ok {
		running, _ = local.PS(ctx)
	}
	loaded := func(model string) bool {
		for _, m := range running {
			if m.Matches(model) {
				return true
			}
		}
		return false
	}
	catalogs := map[string][]ollama.ModelInfo{}
	find := func(p config.Profile) *ollama.ModelInfo {
		models := list
		if config.IsCloud(p.Provider) {
			c, ok := catalogs[p.Provider]
			if !ok {
				c, _ = a.Models.backendFor(p).ListModels(ctx)
				catalogs[p.Provider] = c
			}
			models = c
		}
		for i := range models {
			if models[i].Matches(p.Model) {
				return &models[i]
			}
		}
		return nil
	}
	choice := func(name string, p config.Profile, profile bool) ModelChoice {
		c := ModelChoice{Name: name, Model: p.Model, Profile: profile, Active: act.Profile == name && act.Model == p.Model}
		if config.IsCloud(p.Provider) {
			c.Cloud = p.Provider
		} else {
			c.Loaded = loaded(p.Model)
		}
		if mi := find(p); mi != nil && len(mi.Capabilities) > 0 {
			c.Known, c.Tools, c.Thinking, c.Vision, c.Context = true, mi.Has("tools"), mi.Has("thinking"), mi.Has("vision"), mi.Details.ContextLength
		}
		return c
	}
	var out []ModelChoice
	used := map[string]bool{}
	for _, p := range a.Cfg.ProfileList() {
		out = append(out, choice(p.Name, p, true))
		used[p.Model] = true
	}
	if listErr != nil {
		return out, listErr
	}
	for _, mi := range list {
		if used[mi.Name] || used[strings.TrimSuffix(mi.Name, ":latest")] {
			continue
		}
		out = append(out, choice(mi.Name, config.Profile{Name: mi.Name, Model: mi.Name}, false))
	}
	return out, nil
}

// PendingAction is one action captured in plan mode, numbered as /plan
// approve and /plan reject number them.
type PendingAction struct {
	N       int    `json:"n"`
	Tool    string `json:"tool"`
	Summary string `json:"summary"`
}

func (a *Agent) PendingActions() []PendingAction {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]PendingAction, len(a.sess.Pending))
	for i, p := range a.sess.Pending {
		out[i] = PendingAction{N: i + 1, Tool: p.Name, Summary: p.Summary}
	}
	return out
}

// CompactAt is the context size, in tokens, at which compaction runs.
func (a *Agent) CompactAt() int {
	l, _ := contextLimits(a.Cfg.Context, a.Window())
	return l.compactAt
}
