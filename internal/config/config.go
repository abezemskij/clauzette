// Package config loads the JSON configuration file. Every field has a
// default, so a config file only needs the values you want to change.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"clauzette/internal/tools"
)

type Config struct {
	OllamaURL     string         `json:"ollama_url"`
	Model         string         `json:"model"`
	NumCtx        int            `json:"num_ctx"`
	KeepAlive     string         `json:"keep_alive"`
	Options       map[string]any `json:"options,omitempty"` // extra Ollama options (temperature, top_p, ...)
	Think         bool           `json:"think"`
	ShowThinking  bool           `json:"show_thinking"`
	WarmupPrefill bool           `json:"warmup_prefill"`

	// Profiles are named ways of running a model. Without any, the settings
	// above form an implicit profile named "default". With profiles, the
	// settings above are the defaults a profile inherits (num_ctx, think,
	// keep_alive, options); model is then only used by the implicit profile.
	Profiles       []Profile `json:"profiles,omitempty"`
	DefaultProfile string    `json:"default_profile,omitempty"` // "" = the first profile; $CLAUZETTE_PROFILE overrides it

	// Providers configures the cloud providers ("openrouter", "openai")
	// that profiles may use. Each has built-in defaults (CloudProviders),
	// so a section is only needed to change them.
	Providers map[string]ProviderConfig `json:"providers,omitempty"`

	Workspace   string `json:"workspace"`
	SessionsDir string `json:"sessions_dir"`
	// ModelsFile keeps what clauzette learns about each model across
	// sessions (characters per token, later /adapt results). Default: next to
	// the audit log.
	ModelsFile string `json:"models_file"`

	// A shared workspace for several agents: with agent_dir, the agent works
	// in <workspace>/<agent_id>, and its sessions and backups live in
	// <sessions_dir>/<agent_id>.
	AgentID      string `json:"agent_id"`      // "" = $CLAUZETTE_AGENT_ID, then the hostname (the pod name in Kubernetes)
	AgentDir     bool   `json:"agent_dir"`     // work in <workspace>/<agent_id> instead of the whole workspace
	SharedAccess string `json:"shared_access"` // none | read | write: what the agent may do in the workspace outside its directory

	AuditLog string `json:"audit_log"`
	Color    string `json:"color"` // auto | always | never

	Prompt   PromptConfig  `json:"prompt"`
	Context  ContextConfig `json:"context"`
	Timeouts TimeoutConfig `json:"timeouts"`
	Agent    AgentConfig   `json:"agent"`
	Tools    ToolsConfig   `json:"tools"`
	Safe     SafeConfig    `json:"safe"`
	Rogue    RogueConfig   `json:"rogue"`
	GPU      GPUConfig     `json:"gpu"`
	Search   SearchConfig  `json:"search"`
}

// RogueConfig limits /rogue, where actions run without approval. The step
// budget per message is agent.max_steps unless /rogue on <steps> sets one.
type RogueConfig struct {
	MaxMinutes int `json:"max_minutes"` // wall-clock limit per message; 0 = none
}

type PromptConfig struct {
	BaseFile        string   `json:"base_file"`         // replaces the built-in base prompt
	RogueBaseFile   string   `json:"rogue_base_file"`   // replaces the built-in base prompt for rogue mode
	ChatBaseFile    string   `json:"chat_base_file"`    // replaces the built-in base prompt for models without tools
	ExtraFile       string   `json:"extra_file"`        // appended after the base prompt
	ProjectFiles    []string `json:"project_files"`     // first one found in the workspace root is included
	ProjectMaxBytes int      `json:"project_max_bytes"` //
	InternetAccess  bool     `json:"internet_access"`   // only describes the environment to the model
}

// ContextConfig sets when and how compaction runs. The thresholds are
// fractions of the effective context window (num_ctx of the active profile),
// so they fit models with different windows. A *_tokens value set above 0
// replaces its fraction with a fixed number of tokens. The reserves are the
// smaller of their token value and their fraction of the window.
type ContextConfig struct {
	CompactAtFraction     float64 `json:"compact_at_fraction"`     // compact when the context reaches this share of the window
	CompactTargetFraction float64 `json:"compact_target_fraction"` // stage 1 alone must get below this, or the older part is summarised
	KeepRecentFraction    float64 `json:"keep_recent_fraction"`    // the newest part kept verbatim
	SummaryChunkFraction  float64 `json:"summary_chunk_fraction"`  // size of one summary request's input
	ReserveFraction       float64 `json:"reserve_fraction"`
	ReserveThinkFraction  float64 `json:"reserve_thinking_fraction"`

	CompactAtTokens     int `json:"compact_at_tokens"`     // 0 = use compact_at_fraction
	CompactTargetTokens int `json:"compact_target_tokens"` // 0 = use compact_target_fraction
	KeepRecentTokens    int `json:"keep_recent_tokens"`    // 0 = use keep_recent_fraction
	// KeepRecentRatio, when set (0 < r <= 0.9), keeps the newest share r of
	// the conversation (not of the window) verbatim instead of the keep and
	// target values above (see agent.compactLimits).
	KeepRecentRatio       float64 `json:"keep_recent_ratio"`
	ReserveTokens         int     `json:"reserve_tokens"`
	ReserveTokensThinking int     `json:"reserve_tokens_thinking"`
	ElideToolResultsOver  int     `json:"elide_tool_results_over_chars"`
	SummaryChunkTokens    int     `json:"summary_chunk_tokens"` // 0 = use summary_chunk_fraction
}

// Profile is a named way of running a model. Every field except name and
// model is optional and inherits the top-level setting.
type Profile struct {
	Name     string `json:"name"`
	Provider string `json:"provider,omitempty"` // "" or "ollama" (local), "openrouter", "openai"
	// BaseURL is the server for this profile. For Ollama it must be empty or
	// equal ollama_url: one Ollama server is supported for now. For a cloud
	// provider it replaces the provider's server.
	BaseURL string `json:"base_url,omitempty"`
	// APIKeyEnv is the name of the environment variable holding the API key
	// of a cloud provider. A key itself never goes into the config. With a
	// base_url of its own and no api_key_env, no key is sent (ProviderFor).
	APIKeyEnv string         `json:"api_key_env,omitempty"`
	Model     string         `json:"model"`
	NumCtx    CtxSize        `json:"num_ctx,omitempty"` // tokens, or "fit"; 0 = top-level num_ctx
	MaxCtx    int            `json:"max_ctx,omitempty"` // upper limit for "fit"
	Think     string         `json:"think,omitempty"`   // "" (inherit) | on | off
	Options   map[string]any `json:"options,omitempty"` // added to (and overriding) the top-level options
	KeepAlive string         `json:"keep_alive,omitempty"`
	Caps      *CapsOverride  `json:"caps,omitempty"`   // corrections to what Ollama reports
	Quirks    []string       `json:"quirks,omitempty"` // named per-model fixes, see Quirks
}

// ProviderConfig is a cloud provider's connection: where it is, which
// environment variable holds its API key (never the key itself), and
// whether only free models may be used.
type ProviderConfig struct {
	BaseURL   string `json:"base_url,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// FreeOnly refuses every model with a price above zero. The client
	// enforces it, so no request can get past it.
	FreeOnly bool `json:"free_only,omitempty"`
}

// CloudProviders are the supported cloud providers with their defaults.
// Both speak the OpenAI Chat Completions API; "openai" with another
// base_url also fits other compatible servers (vLLM, LM Studio, ...).
var CloudProviders = map[string]ProviderConfig{
	"openrouter": {BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY"},
	"openai":     {BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
}

// IsCloud reports whether a provider name is a cloud provider ("" and
// "ollama" are the local Ollama).
func IsCloud(provider string) bool {
	_, ok := CloudProviders[provider]
	return ok
}

// KeyEnvNames are the environment variables that may hold an API key: the
// built-in defaults and every api_key_env in the providers section and the
// profiles, sorted. Commands the model runs do not get them.
func (c *Config) KeyEnvNames() []string {
	seen := map[string]bool{}
	for _, pc := range CloudProviders {
		seen[pc.APIKeyEnv] = true
	}
	for _, pc := range c.Providers {
		seen[pc.APIKeyEnv] = true
	}
	for _, p := range c.Profiles {
		seen[p.APIKeyEnv] = true
	}
	delete(seen, "")
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ProviderFor returns a profile's provider name ("ollama" for local) and,
// for a cloud provider, its settings: the built-in defaults, overridden by
// the providers section, overridden by the profile's base_url and
// api_key_env.
//
// The built-in key variable belongs to the built-in server only: with
// another base_url and no api_key_env of its own, no key is sent, so a
// profile pointed at vLLM or a proxy never receives the OpenAI or
// OpenRouter key.
func (c *Config) ProviderFor(p Profile) (string, ProviderConfig) {
	name := p.Provider
	if !IsCloud(name) {
		return "ollama", ProviderConfig{BaseURL: c.OllamaURL}
	}
	def := CloudProviders[name]
	pc := def
	explicitKey := false
	if o, ok := c.Providers[name]; ok {
		if o.BaseURL != "" {
			pc.BaseURL = o.BaseURL
		}
		if o.APIKeyEnv != "" {
			pc.APIKeyEnv, explicitKey = o.APIKeyEnv, true
		}
		pc.FreeOnly = o.FreeOnly
	}
	if p.BaseURL != "" {
		pc.BaseURL = p.BaseURL
	}
	if p.APIKeyEnv != "" {
		pc.APIKeyEnv, explicitKey = p.APIKeyEnv, true
	}
	pc.BaseURL = strings.TrimRight(pc.BaseURL, "/")
	if !explicitKey && pc.BaseURL != def.BaseURL {
		pc.APIKeyEnv = ""
	}
	return name, pc
}

// Endpoint names where a cloud profile's requests go, for the operator's
// consent: the provider and its server. "" for the local Ollama.
func (c *Config) Endpoint(p Profile) string {
	name, pc := c.ProviderFor(p)
	if name == "ollama" {
		return ""
	}
	return name + " (" + pc.BaseURL + ")"
}

// CapsOverride corrects the capabilities Ollama reports for a model. Unset
// fields keep the detected value.
type CapsOverride struct {
	Tools    *bool `json:"tools,omitempty"`
	Thinking *bool `json:"thinking,omitempty"`
	Vision   *bool `json:"vision,omitempty"`
	Context  int   `json:"context,omitempty"` // native context length
}

// Quirks are named fixes for models that misbehave in a known way, turned
// on per profile. The agent implements them; this is the list of names a
// config may use, with what each does.
var Quirks = map[string]string{
	"inline-think": `the model writes its reasoning into the reply, ending it with "</think>", even with thinking off (seen with qwen3:4b); the text up to the tag is treated as thinking`,
}

// CtxSize is a profile's num_ctx: a number of tokens, or CtxFit for the
// largest context that fits on the GPU (written as "fit").
type CtxSize int

// CtxFit is num_ctx "fit". Until /adapt has measured a model, "fit" falls
// back to the top-level num_ctx.
const CtxFit CtxSize = -1

func (c *CtxSize) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		if s != "fit" {
			return fmt.Errorf(`num_ctx must be a number or "fit", not %q`, s)
		}
		*c = CtxFit
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf(`num_ctx must be a number or "fit"`)
	}
	*c = CtxSize(n)
	return nil
}

func (c CtxSize) MarshalJSON() ([]byte, error) {
	if c == CtxFit {
		return []byte(`"fit"`), nil
	}
	return json.Marshal(int(c))
}

// ImplicitProfile is the name of the profile built from the top-level
// settings when no profiles are configured.
const ImplicitProfile = "default"

// ProfileList returns the configured profiles, or the implicit one.
func (c *Config) ProfileList() []Profile {
	if len(c.Profiles) > 0 {
		return c.Profiles
	}
	think := "off"
	if c.Think {
		think = "on"
	}
	return []Profile{{Name: ImplicitProfile, Model: c.Model, NumCtx: CtxSize(c.NumCtx), Think: think}}
}

// FindProfile returns the profile with this name.
func (c *Config) FindProfile(name string) (Profile, bool) {
	for _, p := range c.ProfileList() {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}

// DefaultProfileName is the profile used when nothing else chooses one.
func (c *Config) DefaultProfileName() string {
	if c.DefaultProfile != "" {
		return c.DefaultProfile
	}
	return c.ProfileList()[0].Name
}

type TimeoutConfig struct {
	ConnectSeconds    int `json:"connect_seconds"`
	FirstTokenSeconds int `json:"first_token_seconds"`
	InterTokenSeconds int `json:"inter_token_seconds"`
	LoadSeconds       int `json:"load_seconds"`
}

type AgentConfig struct {
	MaxSteps int `json:"max_steps"`
}

type ToolsConfig struct {
	Shell                 string   `json:"shell"`
	ExecDefaultTimeoutSec int      `json:"exec_default_timeout_seconds"`
	ExecMaxTimeoutSec     int      `json:"exec_max_timeout_seconds"`
	MaxOutputBytes        int      `json:"max_output_bytes"`
	ReadDefaultLines      int      `json:"read_default_lines"`
	ReadMaxFileBytes      int64    `json:"read_max_file_bytes"`
	AutoApprove           []string `json:"auto_approve"`
	Disabled              []string `json:"disabled"`

	// fetch_url settings.
	FetchMaxBytes     int    `json:"fetch_max_bytes"`     // ceiling on bytes read from a response; default 131072 (128 KB)
	FetchUserAgent    string `json:"fetch_user_agent"`    // User-Agent header; default a Lynx-like string
	FetchAllowHTTP    bool   `json:"fetch_allow_http"`    // allow plain http:// as well as https://
	FetchAllowPrivate bool   `json:"fetch_allow_private"` // allow private/loopback/link-local addresses (internal doc servers)

	FetchAllowlist    bool     `json:"fetch_allowlist"`     // start with the fetch allowlist gate on (toggle at runtime with /fetch)
	FetchAllowedHosts []string `json:"fetch_allowed_hosts"` // hosts allowed without prompting when the gate is on
}

// DefaultFetchUserAgent is the fetch User-Agent default, defined once in the
// tools package (which owns the fetch tool) and re-exported here for Defaults.
const DefaultFetchUserAgent = tools.DefaultUserAgent

// SearchConfig configures the web_search tool. The tool is enabled only when
// Provider is set (currently "searxng").
type SearchConfig struct {
	Provider     string `json:"provider"`      // "" (off) | "searxng"
	Endpoint     string `json:"endpoint"`      // base URL of the SearXNG instance
	MaxResults   int    `json:"max_results"`   // results to return; default 5
	AllowPrivate bool   `json:"allow_private"` // the endpoint may be an in-cluster (private) address
}

type SafeConfig struct {
	MaxBackups int `json:"max_backups"` // safe mode: how many backups to keep per session
}

type GPUConfig struct {
	Enabled       bool    `json:"enabled"`
	MetricsURL    string  `json:"metrics_url"`
	Mode          string  `json:"mode"` // temperature | utilization | both
	TempMetric    string  `json:"temp_metric"`
	UtilMetric    string  `json:"util_metric"`
	UtilScale     float64 `json:"util_scale"` // 100 if the exporter reports utilization as 0..1
	PauseAboveC   float64 `json:"pause_above_c"`
	ResumeBelowC  float64 `json:"resume_below_c"`
	UtilThreshold float64 `json:"util_threshold"`
	UtilWindowSec int     `json:"util_window_seconds"`
	UtilPauseSec  int     `json:"util_pause_seconds"`
	SampleSec     int     `json:"sample_seconds"`
	MaxPauseSec   int     `json:"max_pause_seconds"`

	// GPU memory, read on demand by /adapt and to check adapted values. Only
	// metrics_url is needed (enabled is about pausing). Give used and either
	// total or free; mem_scale converts the exporter's unit to bytes
	// (1048576 for DCGM's MiB, 1 for nvidia_gpu_exporter's bytes).
	MemUsedMetric  string  `json:"mem_used_metric"`
	MemTotalMetric string  `json:"mem_total_metric"`
	MemFreeMetric  string  `json:"mem_free_metric"`
	MemScale       float64 `json:"mem_scale"`
}

func Defaults() *Config {
	return &Config{
		OllamaURL:    "http://127.0.0.1:11434",
		Model:        "qwen3.8:27b",
		NumCtx:       131072,
		KeepAlive:    "1h",
		Think:        true,
		Workspace:    "./workspace",
		SharedAccess: "read",
		Color:        "auto",
		Prompt: PromptConfig{
			ProjectFiles:    []string{"AGENTS.md", "CLAUDE.md"},
			ProjectMaxBytes: 20000,
		},
		Context: ContextConfig{
			// At a 131072 window: compact at 98304, down to 39321, keeping
			// 19660 verbatim, summary chunks of 60293; reserves 8192/24576.
			CompactAtFraction:     0.75,
			CompactTargetFraction: 0.30,
			KeepRecentFraction:    0.15,
			SummaryChunkFraction:  0.46,
			ReserveFraction:       0.0625,
			ReserveThinkFraction:  0.1875,
			ReserveTokens:         8192,
			ReserveTokensThinking: 24576,
			ElideToolResultsOver:  600,
		},
		Timeouts: TimeoutConfig{
			ConnectSeconds:    5,
			FirstTokenSeconds: 1200, // model loading + processing a long prompt
			InterTokenSeconds: 600,  // Ollama sends nothing while it assembles a tool call, which can be long for big files
			LoadSeconds:       900,
		},
		Agent:  AgentConfig{MaxSteps: 100},
		Safe:   SafeConfig{MaxBackups: 20},
		Search: SearchConfig{MaxResults: 5},
		Rogue:  RogueConfig{MaxMinutes: 60},
		Tools: ToolsConfig{
			Shell:                 "/bin/sh",
			ExecDefaultTimeoutSec: 120,
			ExecMaxTimeoutSec:     1800,
			MaxOutputBytes:        30000,
			ReadDefaultLines:      400,
			ReadMaxFileBytes:      10 << 20,
			FetchMaxBytes:         128 << 10,
			FetchUserAgent:        DefaultFetchUserAgent,
		},
		GPU: GPUConfig{
			Mode:          "temperature",
			TempMetric:    "DCGM_FI_DEV_GPU_TEMP",
			UtilMetric:    "DCGM_FI_DEV_GPU_UTIL",
			UtilScale:     1,
			PauseAboveC:   80,
			ResumeBelowC:  70,
			UtilThreshold: 95,
			UtilWindowSec: 60,
			UtilPauseSec:  60,
			SampleSec:     5,
			MaxPauseSec:   900,
			MemUsedMetric: "DCGM_FI_DEV_FB_USED",
			MemFreeMetric: "DCGM_FI_DEV_FB_FREE",
			MemScale:      1 << 20,
		},
	}
}

// ResolvePath picks the config file: the -config flag, then $CLAUZETTE_CONFIG,
// then ./clauzette.json, then /etc/clauzette/config.json. Empty means defaults only.
func ResolvePath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("CLAUZETTE_CONFIG"); v != "" {
		return v
	}
	for _, p := range []string{"clauzette.json", "/etc/clauzette/config.json"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// Load reads the config file over the defaults, applies environment
// overrides and validates the result.
func Load(path string) (*Config, error) {
	c := Defaults()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields() // catches typos in field names
		if err := dec.Decode(c); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
	}
	if v := os.Getenv("CLAUZETTE_OLLAMA_URL"); v != "" {
		c.OllamaURL = v
	}
	if v := os.Getenv("CLAUZETTE_MODEL"); v != "" {
		c.Model = v // used by the implicit profile only
	}
	if v := os.Getenv("CLAUZETTE_PROFILE"); v != "" {
		c.DefaultProfile = v
	}
	if v := os.Getenv("CLAUZETTE_WORKSPACE"); v != "" {
		c.Workspace = v
	}
	if v := os.Getenv("CLAUZETTE_AGENT_ID"); v != "" {
		c.AgentID = v
	}
	if v := os.Getenv("CLAUZETTE_AGENT_DIR"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("CLAUZETTE_AGENT_DIR must be true or false, not %q", v)
		}
		c.AgentDir = b
	}
	if v := os.Getenv("CLAUZETTE_SHARED_ACCESS"); v != "" {
		c.SharedAccess = v
	}
	if c.AgentID == "" {
		c.AgentID, _ = os.Hostname()
	}
	c.OllamaURL = strings.TrimRight(c.OllamaURL, "/")
	if c.SessionsDir == "" {
		home, _ := os.UserHomeDir()
		if home == "" {
			home = "."
		}
		c.SessionsDir = filepath.Join(home, ".clauzette", "sessions")
	}
	if c.AuditLog == "" {
		c.AuditLog = filepath.Join(filepath.Dir(c.SessionsDir), "audit.jsonl")
	}
	if c.ModelsFile == "" {
		c.ModelsFile = filepath.Join(filepath.Dir(c.AuditLog), "models.json")
	}
	if c.Tools.FetchMaxBytes < 1 {
		c.Tools.FetchMaxBytes = 128 << 10
	}
	if c.Tools.FetchUserAgent == "" {
		c.Tools.FetchUserAgent = DefaultFetchUserAgent
	}
	return c, c.validate()
}

// AgentSessionsDir is where this agent keeps its sessions and backups.
func (c *Config) AgentSessionsDir() string {
	if c.AgentDir {
		return filepath.Join(c.SessionsDir, c.AgentID)
	}
	return c.SessionsDir
}

func (c *Config) validate() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	if c.Model == "" {
		add("model is empty")
	}
	if c.NumCtx < 2048 {
		add("num_ctx must be at least 2048")
	}
	c.validateContext(add)
	c.validateProfiles(add)
	switch c.SharedAccess {
	case "none", "read", "write":
	default:
		add("shared_access must be none, read or write")
	}
	if c.Tools.Shell == "" {
		add("tools.shell is empty")
	}
	if c.Rogue.MaxMinutes < 0 {
		add("rogue.max_minutes must be 0 (no limit) or more")
	}
	if c.Safe.MaxBackups < 1 {
		add("safe.max_backups must be at least 1")
	}
	switch c.Search.Provider {
	case "", "searxng":
		if c.Search.Provider == "searxng" && strings.TrimSpace(c.Search.Endpoint) == "" {
			add("search.endpoint is required when search.provider is searxng")
		}
	default:
		add(`search.provider must be "" (off) or "searxng"`)
	}
	switch c.GPU.Mode {
	case "temperature", "utilization", "both":
	default:
		add("gpu.mode must be temperature, utilization or both")
	}
	if c.GPU.ResumeBelowC >= c.GPU.PauseAboveC {
		add("gpu.resume_below_c must be below gpu.pause_above_c")
	}
	if c.GPU.MemScale <= 0 {
		add("gpu.mem_scale must be above 0")
	}
	switch c.Color {
	case "auto", "always", "never":
	default:
		add("color must be auto, always or never")
	}
	if len(errs) > 0 {
		return errors.New("invalid config: " + strings.Join(errs, "; "))
	}
	return nil
}

// validateContext checks what can be checked without knowing the window.
// Fixed token values are checked against the window when a profile is
// resolved (agent.contextLimits), because each profile can have its own.
func (c *Config) validateContext(add func(string, ...any)) {
	cc := c.Context
	frac := func(name string, v, max float64) {
		if v <= 0 || v > max {
			add("context.%s must be above 0 and at most %g", name, max)
		}
	}
	frac("compact_at_fraction", cc.CompactAtFraction, 0.95)
	frac("compact_target_fraction", cc.CompactTargetFraction, 0.9)
	frac("keep_recent_fraction", cc.KeepRecentFraction, 0.9)
	frac("summary_chunk_fraction", cc.SummaryChunkFraction, 0.5)
	frac("reserve_fraction", cc.ReserveFraction, 0.5)
	frac("reserve_thinking_fraction", cc.ReserveThinkFraction, 0.5)
	if cc.CompactTargetFraction >= cc.CompactAtFraction {
		add("context.compact_target_fraction must be below context.compact_at_fraction")
	}
	if cc.KeepRecentFraction >= cc.CompactTargetFraction {
		add("context.keep_recent_fraction must be below context.compact_target_fraction")
	}
	for name, v := range map[string]int{
		"compact_at_tokens": cc.CompactAtTokens, "compact_target_tokens": cc.CompactTargetTokens,
		"keep_recent_tokens": cc.KeepRecentTokens, "summary_chunk_tokens": cc.SummaryChunkTokens,
		"reserve_tokens": cc.ReserveTokens, "reserve_tokens_thinking": cc.ReserveTokensThinking,
	} {
		if v < 0 {
			add("context.%s must be 0 or more", name)
		}
	}
	if cc.CompactAtTokens > 0 && cc.CompactTargetTokens > 0 && cc.CompactTargetTokens >= cc.CompactAtTokens {
		add("context.compact_target_tokens must be below context.compact_at_tokens")
	}
	if cc.CompactTargetTokens > 0 && cc.KeepRecentTokens > 0 && cc.KeepRecentTokens >= cc.CompactTargetTokens {
		add("context.keep_recent_tokens must be below context.compact_target_tokens")
	}
	if cc.KeepRecentRatio < 0 || cc.KeepRecentRatio > 0.9 {
		add("context.keep_recent_ratio must be between 0 (off) and 0.9")
	}
	// Without profiles the window is known now, so report it here rather
	// than as a warning at runtime.
	if len(c.Profiles) == 0 && cc.CompactAtTokens >= c.NumCtx {
		add("context.compact_at_tokens (%d) must be below num_ctx (%d)", cc.CompactAtTokens, c.NumCtx)
	}
}

func (c *Config) validateProfiles(add func(string, ...any)) {
	seen := map[string]bool{}
	for i := range c.Profiles {
		// "openrouter:vendor/model" is how /model names a cloud model; in a
		// profile, provider already says where it runs.
		if p := &c.Profiles[i]; IsCloud(p.Provider) {
			p.Model = strings.TrimPrefix(p.Model, p.Provider+":")
		}
	}
	for i, p := range c.Profiles {
		label := fmt.Sprintf("profiles[%d]", i)
		if p.Name != "" {
			label = fmt.Sprintf("profile %q", p.Name)
		}
		switch {
		case p.Name == "":
			add("%s: name is empty", label)
		case strings.ContainsAny(p.Name, " \t/\\"):
			add("%s: the name must not contain spaces or slashes", label)
		case seen[p.Name]:
			add("%s: the name is used twice", label)
		}
		seen[p.Name] = true
		if p.Model == "" {
			add("%s: model is empty", label)
		}
		switch {
		case p.Provider == "" || p.Provider == "ollama":
			if p.BaseURL != "" && strings.TrimRight(p.BaseURL, "/") != c.OllamaURL {
				add("%s: base_url must be empty or equal ollama_url (one Ollama server is supported for now)", label)
			}
			if p.APIKeyEnv != "" {
				add("%s: api_key_env is for cloud providers; the local Ollama needs no key", label)
			}
		case IsCloud(p.Provider):
			if p.NumCtx == CtxFit || p.MaxCtx != 0 {
				add(`%s: num_ctx "fit" and max_ctx are for local models (/adapt measures the local GPU); give a cloud model a number, or leave num_ctx out to use the top-level num_ctx (capped at the provider's limit)`, label)
			}
			if p.BaseURL != "" && !validURL(p.BaseURL) {
				add("%s: base_url must be an http(s) URL", label)
			}
			if p.APIKeyEnv != "" && !envName.MatchString(p.APIKeyEnv) {
				add("%s: api_key_env must be the name of an environment variable (e.g. OPENROUTER_API_KEY), not a key", label)
			}
		default:
			add("%s: unknown provider %q (ollama, openrouter or openai)", label, p.Provider)
		}
		if p.NumCtx != CtxFit && p.NumCtx != 0 && p.NumCtx < 2048 {
			add("%s: num_ctx must be at least 2048, or \"fit\"", label)
		}
		if p.MaxCtx != 0 && p.MaxCtx < 2048 {
			add("%s: max_ctx must be at least 2048", label)
		}
		switch p.Think {
		case "", "on", "off":
		default:
			add("%s: think must be on or off", label)
		}
		if p.Caps != nil && p.Caps.Context < 0 {
			add("%s: caps.context must be 0 or more", label)
		}
		for _, q := range p.Quirks {
			if _, ok := Quirks[q]; !ok {
				var known []string
				for k := range Quirks {
					known = append(known, k)
				}
				sort.Strings(known)
				add("%s: unknown quirk %q (known: %s)", label, q, strings.Join(known, ", "))
			}
		}
	}
	if c.DefaultProfile != "" {
		if _, ok := c.FindProfile(c.DefaultProfile); !ok {
			add("default_profile %q is not a configured profile", c.DefaultProfile)
		}
	}
	for name, pc := range c.Providers {
		switch {
		case !IsCloud(name):
			add("providers: unknown provider %q (openrouter or openai; the local Ollama is set with ollama_url)", name)
		case pc.BaseURL != "" && !validURL(pc.BaseURL):
			add("providers.%s.base_url must be an http(s) URL", name)
		case pc.APIKeyEnv != "" && !envName.MatchString(pc.APIKeyEnv):
			add("providers.%s.api_key_env must be the name of an environment variable (e.g. OPENROUTER_API_KEY), not a key", name)
		}
	}
}

// envName is what an environment variable name looks like. An API key does
// not (it has lower case letters or dashes), which catches a key pasted
// where its variable's name belongs.
var envName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func validURL(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}
