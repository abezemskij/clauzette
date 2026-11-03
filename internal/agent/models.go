package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"clauzette/internal/config"
	"clauzette/internal/llm"
	"clauzette/internal/modeldb"
	"clauzette/internal/openai"
	"clauzette/internal/session"
)

// Caps is what the active model can do.
type Caps struct {
	// Known is true when Ollama reported the capabilities. When it did not
	// (unreachable at start, or an Ollama too old to report them), tools and
	// thinking are assumed, which is how clauzette always behaved; assuming
	// neither would turn every model chat-only.
	Known     bool
	Tools     bool
	Thinking  bool
	Vision    bool
	NativeCtx int // the model's own context limit; 0 = unknown
}

// Active is a profile resolved for requests: the model, its options and its
// capabilities. It is replaced as a whole, never changed, so one request
// always sees one consistent set of settings.
type Active struct {
	Profile string
	Model   string
	Cloud   string // the cloud provider ("openrouter", "openai"); "" for the local Ollama
	// Endpoint is the provider and server a cloud profile sends to
	// (config.Endpoint), the unit of the operator's consent; "" if local.
	Endpoint  string
	NumCtx    int // the effective context window
	KeepAlive string
	Think     bool // whether new sessions start with thinking on
	Caps      Caps
	// Key identifies the weights in the models file: the weights digest, or
	// "model:<name>" when Ollama does not show one. "" until detected.
	Key   string
	Notes []string // how the profile was resolved, for the operator
	// Adapted is the /adapt record NumCtx came from (num_ctx "fit"), nil
	// otherwise. Loads with it are checked against its measurements.
	Adapted *modeldb.Record
	options map[string]any
	src     config.Profile // the profile this was resolved from
}

// RequestOptions are the Ollama options for every request with this
// profile. They must be identical for all requests (warm-up, turns,
// compaction): a different num_ctx makes Ollama reload the model.
func (a *Active) RequestOptions() map[string]any {
	o := make(map[string]any, len(a.options)+1)
	for k, v := range a.options {
		o[k] = v
	}
	o["num_ctx"] = a.NumCtx
	return o
}

// Source is the profile this was resolved from.
func (a *Active) Source() config.Profile { return a.src }

// ChatOnly reports whether the model cannot call tools.
func (a *Active) ChatOnly() bool { return !a.Caps.Tools }

// think is the request's think field: nil (not sent) for a model without
// thinking, so Ollama does not reject the request.
func (a *Active) think(on bool) *bool {
	if !a.Caps.Thinking {
		return nil
	}
	return &on
}

// Models holds the active profile of one process. The agents of all its
// sessions and the warm-up share it; /model replaces it (SwitchModel).
type Models struct {
	Cfg *config.Config
	DB  *modeldb.DB // nil: nothing is remembered across sessions
	// Local is the Ollama client. Cloud clients are made when a profile
	// first needs one, one per provider setting.
	Local  llm.Backend
	active atomic.Pointer[Active]

	mu    sync.Mutex
	cloud map[string]llm.Backend
}

// Backend is the client for a resolved profile: the local Ollama, or the
// profile's cloud provider.
func (m *Models) Backend(a *Active) llm.Backend { return m.backendFor(a.src) }

// noBackend answers every request with an error; it stands in for a
// missing local client instead of a nil interface.
type noBackend struct{}

var errNoBackend = errors.New("no Ollama client is configured")

func (noBackend) ChatStream(context.Context, llm.ChatRequest, func(llm.Message)) (*llm.Result, error) {
	return nil, errNoBackend
}
func (noBackend) ListModels(context.Context) ([]llm.ModelInfo, error) { return nil, errNoBackend }

func (m *Models) backendFor(p config.Profile) llm.Backend {
	name, pc := m.Cfg.ProviderFor(p)
	if name == "ollama" {
		if m.Local == nil {
			return noBackend{}
		}
		return m.Local
	}
	key := fmt.Sprintf("%s|%s|%s|%v", name, pc.BaseURL, pc.APIKeyEnv, pc.FreeOnly)
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.cloud[key]; ok {
		return b
	}
	t := m.Cfg.Timeouts
	secs := func(n int) time.Duration { return time.Duration(n) * time.Second }
	b := openai.New(openai.Options{
		Name: name, BaseURL: pc.BaseURL, APIKey: os.Getenv(pc.APIKeyEnv), KeyEnv: pc.APIKeyEnv, FreeOnly: pc.FreeOnly,
		Timeouts: llm.Timeouts{Connect: secs(t.ConnectSeconds), FirstToken: secs(t.FirstTokenSeconds), InterToken: secs(t.InterTokenSeconds)},
	})
	if m.cloud == nil {
		m.cloud = map[string]llm.Backend{}
	}
	m.cloud[key] = b
	return b
}

// Online is the OpenRouter client, for /model --online; it needs no key to
// list models.
func (m *Models) Online() llm.Backend {
	return m.backendFor(config.Profile{Provider: "openrouter"})
}

// NewModels starts with profile p. Its capabilities are assumed until
// Detect has asked Ollama.
func NewModels(cfg *config.Config, db *modeldb.DB, p config.Profile) *Models {
	m := &Models{Cfg: cfg, DB: db}
	m.active.Store(resolve(cfg, p, Caps{}, "", nil))
	return m
}

// record returns the models-file record for a key, or nil.
func (m *Models) record(key string) *modeldb.Record {
	if r, ok := m.DB.Get(key); ok {
		return &r
	}
	return nil
}

// refresh re-resolves the active profile with the current models file, after
// /adapt changed what "fit" means.
func (m *Models) refresh() *Active {
	a := m.Active()
	next := resolve(m.Cfg, a.src, a.Caps, a.Key, m.record(a.Key))
	m.set(next)
	return next
}

// Active is the profile for the next request.
func (m *Models) Active() *Active { return m.active.Load() }

// set makes a resolved profile the active one.
func (m *Models) set(a *Active) { m.active.Store(a) }

// Detect asks the active profile's backend what its model supports and how
// its weights are identified, then re-resolves the profile. local is the
// Ollama client, used when Models has none yet. It returns notes for the
// operator; problems are warnings, never fatal.
func (m *Models) Detect(ctx context.Context, local llm.Backend) (info, warn []string) {
	if m.Local == nil {
		m.Local = local
	}
	a, info, warn := m.probe(ctx, m.Active().src)
	m.set(a)
	return info, warn
}

// probe resolves profile p with what its backend reports about its model,
// without making it active.
func (m *Models) probe(ctx context.Context, p config.Profile) (_ *Active, info, warn []string) {
	model := p.Model
	provider, _ := m.Cfg.ProviderFor(p)
	b := m.backendFor(p)
	var caps Caps
	found := false
	list, err := b.ListModels(ctx)
	switch {
	case err != nil:
		warn = append(warn, fmt.Sprintf("could not list the models (%v); assuming %s supports tools and thinking", err, model))
	default:
		for _, mi := range list {
			if !mi.Matches(model) {
				continue
			}
			found = true
			caps.NativeCtx = mi.Details.ContextLength
			if len(mi.Capabilities) == 0 {
				info = append(info, fmt.Sprintf("%s does not report capabilities for %s; assuming it supports tools and thinking", providerLabel(provider), model))
				break
			}
			caps.Known = true
			caps.Tools, caps.Thinking, caps.Vision = mi.Has("tools"), mi.Has("thinking"), mi.Has("vision")
			break
		}
		if !found {
			if provider == "ollama" {
				warn = append(warn, fmt.Sprintf("%s is not installed in Ollama (ollama pull %s)", model, model))
			} else {
				warn = append(warn, fmt.Sprintf("%s does not offer %s (/model --online lists its models)", provider, model))
			}
		}
	}
	if provider != "ollama" {
		key := "cloud:" + provider + ":" + model
		return resolve(m.Cfg, p, caps, key, m.record(key)), info, warn
	}
	key := ""
	if local, ok := b.(llm.Local); ok && (found || err != nil) {
		if caps.NativeCtx == 0 {
			if n, err := local.ShowModelContext(ctx, model); err == nil {
				caps.NativeCtx = n
			}
		}
		if d, err := local.ShowWeights(ctx, model); err == nil {
			key = d
		}
	}
	if key == "" {
		key = "model:" + model
	}
	if found && m.DB != nil && key != "model:"+model {
		m.DB.Update(key, model, func(*modeldb.Record) {}) // remember which names share these weights
	}
	return resolve(m.Cfg, p, caps, key, m.record(key)), info, warn
}

// resolve turns a profile into request settings. Each setting comes from
// the profile, else the default profile (think only), else the top-level
// config. rec is the models-file record for the weights (nil if none); it
// gives num_ctx "fit" its value.
func resolve(cfg *config.Config, p config.Profile, detected Caps, key string, rec *modeldb.Record) *Active {
	a := &Active{Profile: p.Name, Model: p.Model, Key: key, Caps: detected, src: p}
	if config.IsCloud(p.Provider) {
		a.Cloud = p.Provider
		a.Endpoint = cfg.Endpoint(p)
	}
	if !detected.Known {
		a.Caps.Tools, a.Caps.Thinking = true, true
	}
	if o := p.Caps; o != nil {
		if o.Tools != nil {
			a.Caps.Tools = *o.Tools
		}
		if o.Thinking != nil {
			a.Caps.Thinking = *o.Thinking
		}
		if o.Vision != nil {
			a.Caps.Vision = *o.Vision
		}
		if o.Context > 0 {
			a.Caps.NativeCtx = o.Context
		}
	}

	switch {
	case p.NumCtx == config.CtxFit:
		a.NumCtx = cfg.NumCtx
		switch {
		case rec != nil && rec.FitCtx > 0 && rec.Stale == "":
			a.NumCtx = rec.FitCtx
			a.Adapted = rec
		case rec != nil && rec.FitCtx > 0:
			a.Notes = append(a.Notes, fmt.Sprintf(`profile %s: the adapted context for %s no longer applies (%s); using num_ctx %d until /adapt %s runs again`, p.Name, p.Model, rec.Stale, a.NumCtx, p.Model))
		case key != "":
			a.Notes = append(a.Notes, fmt.Sprintf(`profile %s: num_ctx "fit" has no adapted value for %s yet; using num_ctx %d (run /adapt %s)`, p.Name, p.Model, a.NumCtx, p.Model))
		}
		if p.MaxCtx > 0 && a.NumCtx > p.MaxCtx {
			a.NumCtx = p.MaxCtx
		}
	case p.NumCtx > 0:
		a.NumCtx = int(p.NumCtx)
	default:
		a.NumCtx = cfg.NumCtx
	}
	if n := a.Caps.NativeCtx; n > 0 && a.NumCtx > n {
		// A cloud model left at the top-level default just uses its own
		// smaller limit; that is not worth a note.
		if a.Cloud == "" || p.NumCtx > 0 {
			a.Notes = append(a.Notes, fmt.Sprintf("num_ctx %d is more than %s supports; using its limit of %d", a.NumCtx, p.Model, n))
		}
		a.NumCtx = n
	}

	a.Think = cfg.Think
	think := p.Think
	if think == "" {
		if dp, ok := cfg.FindProfile(cfg.DefaultProfileName()); ok {
			think = dp.Think
		}
	}
	switch think {
	case "on":
		a.Think = true
	case "off":
		a.Think = false
	}

	a.KeepAlive = cfg.KeepAlive
	if p.KeepAlive != "" {
		a.KeepAlive = p.KeepAlive
	}
	a.options = make(map[string]any, len(cfg.Options)+len(p.Options))
	for k, v := range cfg.Options {
		a.options[k] = v
	}
	for k, v := range p.Options {
		a.options[k] = v
	}
	return a
}

// SelectProfile picks the profile to start with: the -profile flag, else
// the resumed session's profile, else the default profile. A resumed
// session whose profile is no longer configured keeps its model with the
// default settings.
func SelectProfile(cfg *config.Config, flag string, sess *session.Session) (config.Profile, []string, error) {
	if flag != "" {
		p, ok := cfg.FindProfile(flag)
		if !ok {
			var names []string
			for _, p := range cfg.ProfileList() {
				names = append(names, p.Name)
			}
			return config.Profile{}, nil, fmt.Errorf("no profile %q (configured: %s)", flag, strings.Join(names, ", "))
		}
		return p, nil, nil
	}
	if sess != nil && sess.Profile != "" {
		if p, ok := cfg.FindProfile(sess.Profile); ok {
			return p, nil, nil
		}
		if sess.Model != "" {
			if p := modelProfile(sess.Profile); p.Model == sess.Model {
				// /model with a plain model name: it has no profile of its own
				return p, nil, nil
			}
			note := fmt.Sprintf("this session's profile %q is no longer configured; using its model %s with the default settings", sess.Profile, sess.Model)
			return config.Profile{Name: sess.Profile, Model: sess.Model}, []string{note}, nil
		}
	}
	p, _ := cfg.FindProfile(cfg.DefaultProfileName())
	return p, nil, nil
}

// modelProfile is the profile for a plain model name given to /model: the
// model with the top-level settings, named after the model. A name such as
// "openrouter:vendor/model:free" is that provider's model.
func modelProfile(name string) config.Profile {
	if provider, model, ok := strings.Cut(name, ":"); ok && config.IsCloud(provider) && model != "" {
		return config.Profile{Name: name, Provider: provider, Model: model}
	}
	return config.Profile{Name: name, Model: name}
}

func providerLabel(provider string) string {
	if provider == "ollama" {
		return "Ollama"
	}
	return provider
}

// target finds what /model <name> means: a configured profile, else a model
// by name (a profile wins when a name is both). It checks that the provider
// has the model, so a switch never unloads the current model for one that
// cannot be used.
func (m *Models) target(ctx context.Context, name string) (config.Profile, error) {
	p, isProfile := m.Cfg.FindProfile(name)
	if !isProfile {
		p = modelProfile(name)
	}
	provider, pc := m.Cfg.ProviderFor(p)
	b := m.backendFor(p)
	list, err := b.ListModels(ctx)
	if err != nil {
		if isProfile {
			return p, nil // cannot check; the first request will tell
		}
		return config.Profile{}, fmt.Errorf("%q is not a profile, and %s's models could not be listed: %v", name, providerLabel(provider), err)
	}
	for _, mi := range list {
		if mi.Matches(p.Model) {
			if provider != "ollama" && pc.FreeOnly && !mi.Free() {
				return config.Profile{}, fmt.Errorf("%s is not free, and providers.%s.free_only is on", p.Model, provider)
			}
			return p, nil
		}
	}
	switch {
	case isProfile && provider == "ollama":
		return config.Profile{}, fmt.Errorf("profile %q uses %s, which is not installed in Ollama (ollama pull %s)", name, p.Model, p.Model)
	case isProfile:
		return config.Profile{}, fmt.Errorf("profile %q uses %s, which %s does not offer", name, p.Model, provider)
	case provider != "ollama":
		return config.Profile{}, fmt.Errorf("%s does not offer %s (/model --online lists its models)", provider, p.Model)
	}
	return config.Profile{}, fmt.Errorf("%q is neither a profile nor an installed model (/model lists both; a cloud model is written provider:model, e.g. openrouter:vendor/model:free)", name)
}
