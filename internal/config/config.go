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
	"strings"
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

	Workspace   string `json:"workspace"`
	SessionsDir string `json:"sessions_dir"`
	AuditLog    string `json:"audit_log"`
	Color       string `json:"color"` // auto | always | never

	Prompt   PromptConfig  `json:"prompt"`
	Context  ContextConfig `json:"context"`
	Timeouts TimeoutConfig `json:"timeouts"`
	Agent    AgentConfig   `json:"agent"`
	Tools    ToolsConfig   `json:"tools"`
	Safe     SafeConfig    `json:"safe"`
	GPU      GPUConfig     `json:"gpu"`
}

type PromptConfig struct {
	BaseFile        string   `json:"base_file"`         // replaces the built-in base prompt
	ExtraFile       string   `json:"extra_file"`        // appended after the base prompt
	ProjectFiles    []string `json:"project_files"`     // first one found in the workspace root is included
	ProjectMaxBytes int      `json:"project_max_bytes"` //
	InternetAccess  bool     `json:"internet_access"`   // only describes the environment to the model
}

type ContextConfig struct {
	CompactAtTokens       int `json:"compact_at_tokens"`
	CompactTargetTokens   int `json:"compact_target_tokens"`
	KeepRecentTokens      int `json:"keep_recent_tokens"`
	ReserveTokens         int `json:"reserve_tokens"`
	ReserveTokensThinking int `json:"reserve_tokens_thinking"`
	ElideToolResultsOver  int `json:"elide_tool_results_over_chars"`
	SummaryChunkTokens    int `json:"summary_chunk_tokens"`
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
}
/* "ollama_url": "http://127.0.0.1:11434",
  "model": "qwen3.8-128k:latest", #"qwen3.8:27b", */

func Defaults() *Config {
	return &Config{
		OllamaURL: "http://127.0.0.1:11434",
		Model:     "qwen3.8:27b",
		NumCtx:    131072,
		KeepAlive: "1h",
		Think:     true,
		Workspace: "./workspace",
		Color:     "auto",
		Prompt: PromptConfig{
			ProjectFiles:    []string{"AGENTS.md", "CLAUDE.md"},
			ProjectMaxBytes: 20000,
		},
		Context: ContextConfig{
			CompactAtTokens:       100000,
			CompactTargetTokens:   40000,
			KeepRecentTokens:      20000,
			ReserveTokens:         8192,
			ReserveTokensThinking: 24576,
			ElideToolResultsOver:  600,
			SummaryChunkTokens:    60000,
		},
		Timeouts: TimeoutConfig{
			ConnectSeconds:    5,
			FirstTokenSeconds: 1200, // model loading + processing a long prompt
			InterTokenSeconds: 600,  // Ollama sends nothing while it assembles a tool call, which can be long for big files
			LoadSeconds:       900,
		},
		Agent: AgentConfig{MaxSteps: 100},
		Safe:  SafeConfig{MaxBackups: 20},
		Tools: ToolsConfig{
			Shell:                 "/bin/sh",
			ExecDefaultTimeoutSec: 120,
			ExecMaxTimeoutSec:     1800,
			MaxOutputBytes:        30000,
			ReadDefaultLines:      400,
			ReadMaxFileBytes:      10 << 20,
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
		c.Model = v
	}
	if v := os.Getenv("CLAUZETTE_WORKSPACE"); v != "" {
		c.Workspace = v
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
	return c, c.validate()
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
	cc := c.Context
	if cc.CompactAtTokens >= c.NumCtx {
		add("context.compact_at_tokens (%d) must be below num_ctx (%d)", cc.CompactAtTokens, c.NumCtx)
	}
	if cc.CompactTargetTokens >= cc.CompactAtTokens {
		add("context.compact_target_tokens must be below context.compact_at_tokens")
	}
	if cc.KeepRecentTokens >= cc.CompactTargetTokens {
		add("context.keep_recent_tokens must be below context.compact_target_tokens")
	}
	if cc.SummaryChunkTokens <= 0 || cc.SummaryChunkTokens > c.NumCtx/2 {
		add("context.summary_chunk_tokens must be between 1 and num_ctx/2")
	}
	if c.Tools.Shell == "" {
		add("tools.shell is empty")
	}
	if c.Safe.MaxBackups < 1 {
		add("safe.max_backups must be at least 1")
	}
	switch c.GPU.Mode {
	case "temperature", "utilization", "both":
	default:
		add("gpu.mode must be temperature, utilization or both")
	}
	if c.GPU.ResumeBelowC >= c.GPU.PauseAboveC {
		add("gpu.resume_below_c must be below gpu.pause_above_c")
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
