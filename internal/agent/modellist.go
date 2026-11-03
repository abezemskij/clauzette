package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"clauzette/internal/config"
	"clauzette/internal/llm"
	"clauzette/internal/modeldb"
	"clauzette/internal/ollama"
	"clauzette/internal/textutil"
)

// ModelList describes the profiles and the installed models for /model:
// what each can do, its context, and which one is loaded. It costs one
// model list and one running-models request; the weights of a model not
// seen before are looked up once and remembered in the models file.
func (a *Agent) ModelList(ctx context.Context) string {
	cfg := a.Cfg
	act := a.Models.Active()
	list, listErr := a.Models.Local.ListModels(ctx)
	local, isLocal := a.Models.Local.(llm.Local)
	var running []ollama.RunningModel
	if isLocal {
		running, _ = local.PS(ctx)
	}
	catalogs := map[string][]ollama.ModelInfo{} // cloud provider → its models, fetched once per listing
	info := func(p config.Profile) *ollama.ModelInfo {
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

	keyOf := map[string]string{}
	if listErr == nil {
		keyOf = a.weightsKeys(ctx, local, isLocal, list)
	}
	recFor := func(p config.Profile) *modeldb.Record {
		if config.IsCloud(p.Provider) {
			return nil // nothing is adapted for a cloud model
		}
		if mi := info(p); mi != nil {
			return a.Models.record(keyOf[mi.Name])
		}
		return nil
	}

	var sb strings.Builder
	sb.WriteString("Profiles\n")
	profiles := cfg.ProfileList()
	inConfig := false
	for _, p := range profiles {
		if p.Name == act.Profile && p.Model == act.Model {
			inConfig = true
		}
	}
	if !inConfig { // a plain model chosen with /model, or a profile removed from the config
		plain := modelProfile(act.Profile).Model == act.Model
		a.writeProfile(&sb, act.src, info(act.src), running, true, plain, recFor(act.src))
	}
	for _, p := range profiles {
		a.writeProfile(&sb, p, info(p), running, inConfig && p.Name == act.Profile, false, recFor(p))
	}

	if listErr != nil {
		fmt.Fprintf(&sb, "Installed models could not be listed: %v\n", listErr)
	} else {
		used := map[string]bool{act.Model: true}
		for _, p := range profiles {
			used[p.Model] = true
		}
		first := map[string]string{} // weights key → first model shown with it
		var lines []string
		for _, mi := range list {
			name := mi.Name
			k := keyOf[name]
			if k != "" && first[k] == "" {
				first[k] = name
			}
			if used[name] || used[strings.TrimSuffix(name, ":latest")] {
				continue
			}
			if k != "" && first[k] != name {
				lines = append(lines, fmt.Sprintf("    %-34s same weights as %s", name, first[k]))
				continue
			}
			fit := ""
			if r := a.Models.record(k); r != nil && r.FitCtx > 0 {
				fit = "  adapted " + textutil.KTok(r.FitCtx)
				if r.Stale != "" {
					fit += " (stale)"
				}
			}
			lines = append(lines, fmt.Sprintf("    %-34s %8s  %-6s %-7s %-24s native %s%s%s", name, textutil.HumanBytes(mi.Size),
				mi.Details.ParameterSize, mi.Details.QuantizationLevel, capsText(&mi), textutil.KTok(mi.Details.ContextLength), fit, loadedText(running, name)))
		}
		if len(lines) > 0 {
			sb.WriteString("Installed, no profile\n")
			sb.WriteString(strings.Join(lines, "\n") + "\n")
		}
	}
	sb.WriteString("/model <profile or model> switches between turns. A plain model name uses the top-level settings;\n" +
		"a cloud model is written provider:model (openrouter:vendor/model:free). /model --online [filter] lists OpenRouter's free models.")
	return sb.String()
}

// writeProfile writes one profile line; plain is a model chosen by name with
// /model, which has no profile of its own.
func (a *Agent) writeProfile(sb *strings.Builder, p config.Profile, mi *ollama.ModelInfo, running []ollama.RunningModel, active, plain bool, rec *modeldb.Record) {
	mark := " "
	if active {
		mark = "*"
	}
	var caps Caps
	if mi != nil && len(mi.Capabilities) > 0 {
		caps = Caps{Known: true, Tools: mi.Has("tools"), Thinking: mi.Has("thinking"), Vision: mi.Has("vision"), NativeCtx: mi.Details.ContextLength}
	}
	r := resolve(a.Cfg, p, caps, "", rec)
	ctx := textutil.KTok(r.NumCtx)
	if p.NumCtx == config.CtxFit {
		switch {
		case r.Adapted != nil:
			ctx = "fit → " + ctx
		case rec != nil && rec.Stale != "":
			ctx = "fit (stale) → " + ctx
		default:
			ctx = "fit (not adapted) → " + ctx
		}
	}
	think := "think " + map[bool]string{true: "on", false: "off"}[r.Think]
	if !r.Caps.Thinking {
		think = "think n/a"
	}
	status := loadedText(running, p.Model)
	switch {
	case config.IsCloud(p.Provider) && mi == nil:
		status = "  [cloud: " + p.Provider + ", not listed]"
	case config.IsCloud(p.Provider):
		status = "  [cloud: " + p.Provider + "]"
		if mi.Free() {
			status += " free"
		}
	case mi == nil:
		status = "  [not installed]"
	}
	name := p.Name
	if plain {
		name = "(no profile)"
	}
	if len(p.Quirks) > 0 {
		status += "  quirks: " + strings.Join(p.Quirks, ", ")
	}
	fmt.Fprintf(sb, "  %s %-12s %-34s ctx %-12s %-10s %s%s\n", mark, name, p.Model, ctx, think, capsText(mi), status)
}

// capsText summarises what a model declares.
func capsText(mi *ollama.ModelInfo) string {
	if mi == nil {
		return ""
	}
	if len(mi.Capabilities) == 0 {
		return "caps unknown"
	}
	if !mi.Has("tools") {
		return "chat only"
	}
	parts := []string{"tools"}
	for _, c := range []string{"thinking", "vision"} {
		if mi.Has(c) {
			parts = append(parts, c)
		}
	}
	return strings.Join(parts, " ")
}

// loadedText says whether a model is in memory, and how much of it is on the GPU.
func loadedText(running []ollama.RunningModel, model string) string {
	for _, m := range running {
		if m.Matches(model) && m.Size > 0 {
			return fmt.Sprintf("  [loaded, %d%% GPU]", int(100*m.SizeVRAM/m.Size))
		}
	}
	return ""
}

// weightsKeys maps installed model names to their weights digest, from the
// models file where known, else by asking Ollama once and remembering it.
func (a *Agent) weightsKeys(ctx context.Context, local llm.Local, isLocal bool, list []ollama.ModelInfo) map[string]string {
	keys := map[string]string{}
	db := a.Models.DB
	changed := false
	for _, mi := range list {
		if k := db.KeyFor(mi.Name); k != "" {
			keys[mi.Name] = k
			continue
		}
		if !isLocal {
			continue
		}
		k, err := local.ShowWeights(ctx, mi.Name)
		if err != nil || k == "" {
			continue
		}
		keys[mi.Name] = k
		db.Update(k, mi.Name, func(*modeldb.Record) {})
		changed = true
	}
	if changed {
		if err := db.Save(); err != nil {
			a.warnf("could not save the models file: %v", err)
		}
	}
	return keys
}

// CloudTarget says where /model <name> would send the conversation: the
// cloud provider and its endpoint (config.Endpoint), or "", "" for a local
// model.
func (a *Agent) CloudTarget(name string) (provider, endpoint string) {
	p, ok := a.Cfg.FindProfile(name)
	if !ok {
		p = modelProfile(name)
	}
	if config.IsCloud(p.Provider) {
		return p.Provider, a.Cfg.Endpoint(p)
	}
	return "", ""
}

// CloudApproved reports whether the operator agreed, in this session, to
// send the conversation to a cloud endpoint. Consent is per endpoint, so a
// profile that points the same provider at another server asks again.
func (a *Agent) CloudApproved(endpoint string) bool {
	for _, e := range a.sess.CloudOK {
		if e == endpoint {
			return true
		}
	}
	return false
}

// ApproveCloud records that agreement for the rest of the session.
func (a *Agent) ApproveCloud(endpoint string) {
	if endpoint != "" && !a.CloudApproved(endpoint) {
		a.sess.CloudOK = append(a.sess.CloudOK, endpoint)
		a.Audit.Log(map[string]any{"session": a.sess.ID, "event": "cloud_approved", "endpoint": endpoint})
		a.Save()
	}
}

// CloudApprovalNeeded is the active profile's endpoint when it is a cloud
// one this session has not approved, else "". The UI asks before a turn
// then: a session can reach a cloud profile without /model (-profile,
// default_profile, a resumed session, /clear or /load on a cloud model).
func (a *Agent) CloudApprovalNeeded() string {
	if e := a.Models.Active().Endpoint; e != "" && !a.CloudApproved(e) {
		return e
	}
	return ""
}

// CloudApprovalError refuses a request to an endpoint the operator has not
// approved for this session.
type CloudApprovalError struct{ Endpoint string }

func (e *CloudApprovalError) Error() string {
	return fmt.Sprintf("nothing was sent: this session has not been approved for %s; send a message to be asked, or switch to a local model with /model", e.Endpoint)
}

// backend is the client for a request with act. A cloud endpoint must have
// been approved in this session; this is the one check every model request
// passes, whatever path made the profile active.
func (a *Agent) backend(act *Active) (llm.Backend, error) {
	if act.Endpoint != "" && !a.CloudApproved(act.Endpoint) {
		return nil, &CloudApprovalError{Endpoint: act.Endpoint}
	}
	return a.Models.Backend(act), nil
}

// onlineMax is how many models /model --online shows at most.
const onlineMax = 40

// OnlineList lists OpenRouter's models for /model --online: the free ones
// unless all is set, whose id contains filter (any case).
func (a *Agent) OnlineList(ctx context.Context, filter string, all bool) string {
	list, err := a.Models.Online().ListModels(ctx)
	if err != nil {
		return fmt.Sprintf("OpenRouter's model list could not be read: %v", err)
	}
	filter = strings.ToLower(filter)
	var rows []ollama.ModelInfo
	for _, mi := range list {
		if (!all && !mi.Free()) || (filter != "" && !strings.Contains(strings.ToLower(mi.Name), filter)) {
			continue
		}
		rows = append(rows, mi)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	what := "free models"
	if all {
		what = "models"
	}
	if filter != "" {
		what += fmt.Sprintf(" matching %q", filter)
	}
	if len(rows) == 0 {
		return fmt.Sprintf("OpenRouter lists no %s (of %d).", what, len(list))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "OpenRouter: %d %s (of %d)\n", len(rows), what, len(list))
	for i, mi := range rows {
		if i == onlineMax {
			fmt.Fprintf(&sb, "  … %d more; narrow it with /model --online <filter>\n", len(rows)-onlineMax)
			break
		}
		price := "free"
		if !mi.Free() {
			price = "price unknown"
			if p := mi.Pricing; p != nil {
				price = fmt.Sprintf("$%.2f / $%.2f per M tokens", p.Prompt*1e6, p.Completion*1e6)
			}
		}
		fmt.Fprintf(&sb, "  %-56s ctx %-6s %-22s %s\n", "openrouter:"+mi.Name, textutil.KTok(mi.Details.ContextLength), capsText(&mi), price)
	}
	sb.WriteString("/model openrouter:<id> switches to one (with the top-level settings); a profile with \"provider\": \"openrouter\" keeps its own.")
	return sb.String()
}
