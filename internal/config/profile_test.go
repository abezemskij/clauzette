package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadJSON(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestImplicitProfileFromTopLevel(t *testing.T) {
	t.Setenv("CLAUZETTE_PROFILE", "")
	c, err := loadJSON(t, `{"model": "qwen3.8:latest", "num_ctx": 65536, "think": false}`)
	if err != nil {
		t.Fatal(err)
	}
	list := c.ProfileList()
	if len(list) != 1 || list[0].Name != ImplicitProfile || list[0].Model != "qwen3.8:latest" || list[0].NumCtx != 65536 || list[0].Think != "off" {
		t.Fatalf("implicit profile = %+v", list)
	}
	if c.DefaultProfileName() != ImplicitProfile {
		t.Fatalf("default profile = %q", c.DefaultProfileName())
	}
}

func TestProfilesParse(t *testing.T) {
	t.Setenv("CLAUZETTE_PROFILE", "")
	c, err := loadJSON(t, `{"profiles": [
		{"name": "qwen-big", "model": "qwen3.8", "num_ctx": "fit", "max_ctx": 196608, "think": "on"},
		{"name": "phi", "model": "phi3:mini", "num_ctx": 32768, "caps": {"tools": false}}
	], "default_profile": "phi"}`)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := c.FindProfile("qwen-big")
	if !ok || p.NumCtx != CtxFit || p.MaxCtx != 196608 || p.Think != "on" {
		t.Fatalf("qwen-big = %+v", p)
	}
	phi, _ := c.FindProfile("phi")
	if phi.Caps == nil || phi.Caps.Tools == nil || *phi.Caps.Tools {
		t.Fatalf("phi caps override not parsed: %+v", phi.Caps)
	}
	if c.DefaultProfileName() != "phi" {
		t.Fatalf("default profile = %q", c.DefaultProfileName())
	}
	// "fit" survives -print-config (marshal) and a reload.
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"num_ctx":"fit"`) {
		t.Fatalf("fit not marshalled: %s", b)
	}
}

func TestProfileEnvSelectsDefault(t *testing.T) {
	t.Setenv("CLAUZETTE_PROFILE", "b")
	c, err := loadJSON(t, `{"profiles": [{"name": "a", "model": "m1"}, {"name": "b", "model": "m2"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.DefaultProfileName() != "b" {
		t.Fatalf("CLAUZETTE_PROFILE not applied: %q", c.DefaultProfileName())
	}
	t.Setenv("CLAUZETTE_PROFILE", "missing")
	if _, err := loadJSON(t, `{"profiles": [{"name": "a", "model": "m1"}]}`); err == nil {
		t.Fatal("an unknown CLAUZETTE_PROFILE was accepted")
	}
}

func TestProfileValidation(t *testing.T) {
	t.Setenv("CLAUZETTE_PROFILE", "")
	for name, body := range map[string]string{
		"empty name":         `{"profiles": [{"model": "m"}]}`,
		"duplicate":          `{"profiles": [{"name": "a", "model": "m"}, {"name": "a", "model": "n"}]}`,
		"space in name":      `{"profiles": [{"name": "a b", "model": "m"}]}`,
		"no model":           `{"profiles": [{"name": "a"}]}`,
		"unknown provider":   `{"profiles": [{"name": "a", "model": "m", "provider": "anthropic"}]}`,
		"api key for ollama": `{"profiles": [{"name": "a", "model": "m", "api_key_env": "KEY"}]}`,
		"other ollama":       `{"profiles": [{"name": "a", "model": "m", "base_url": "http://elsewhere:11434"}]}`,
		"tiny num_ctx":       `{"profiles": [{"name": "a", "model": "m", "num_ctx": 1000}]}`,
		"bad num_ctx text":   `{"profiles": [{"name": "a", "model": "m", "num_ctx": "max"}]}`,
		"bad think":          `{"profiles": [{"name": "a", "model": "m", "think": "high"}]}`,
		"unknown quirk":      `{"profiles": [{"name": "a", "model": "m", "quirks": ["text-tools"]}]}`,
		"unknown default":    `{"profiles": [{"name": "a", "model": "m"}], "default_profile": "b"}`,
	} {
		if _, err := loadJSON(t, body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// base_url equal to ollama_url (trailing slash ignored) is fine.
	if _, err := loadJSON(t, `{"ollama_url": "http://o:11434", "profiles": [{"name": "a", "model": "m", "base_url": "http://o:11434/"}]}`); err != nil {
		t.Errorf("base_url equal to ollama_url rejected: %v", err)
	}
}

func TestContextDefaultsAreFractions(t *testing.T) {
	c := Defaults()
	cc := c.Context
	if cc.CompactAtTokens != 0 || cc.CompactTargetTokens != 0 || cc.KeepRecentTokens != 0 || cc.SummaryChunkTokens != 0 {
		t.Fatalf("fixed thresholds must default to unset so the fractions apply: %+v", cc)
	}
	if cc.CompactAtFraction != 0.75 {
		t.Fatalf("compact_at_fraction = %v, want 0.75", cc.CompactAtFraction)
	}
}

func TestContextValidation(t *testing.T) {
	t.Setenv("CLAUZETTE_PROFILE", "")
	for name, body := range map[string]string{
		"target above at":    `{"context": {"compact_target_fraction": 0.8}}`,
		"keep above target":  `{"context": {"keep_recent_fraction": 0.5}}`,
		"zero fraction":      `{"context": {"compact_at_fraction": 0}}`,
		"chunk above half":   `{"context": {"summary_chunk_fraction": 0.6}}`,
		"fixed target >= at": `{"context": {"compact_at_tokens": 50000, "compact_target_tokens": 60000}}`,
		"fixed at >= window": `{"num_ctx": 32768, "context": {"compact_at_tokens": 40000}}`,
		"negative tokens":    `{"context": {"keep_recent_tokens": -1}}`,
	} {
		if _, err := loadJSON(t, body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// With profiles the window is per profile, so a fixed value above the
	// top-level num_ctx is checked at runtime, not rejected here.
	if _, err := loadJSON(t, `{"num_ctx": 32768, "context": {"compact_at_tokens": 40000}, "profiles": [{"name": "a", "model": "m", "num_ctx": 131072}]}`); err != nil {
		t.Errorf("fixed threshold with profiles rejected: %v", err)
	}
}

func TestKnownQuirkAccepted(t *testing.T) {
	t.Setenv("CLAUZETTE_PROFILE", "")
	c, err := loadJSON(t, `{"profiles": [{"name": "small", "model": "qwen3:4b", "quirks": ["inline-think"]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := c.FindProfile("small"); len(p.Quirks) != 1 {
		t.Fatalf("quirks = %v", p.Quirks)
	}
}

func TestCloudProfiles(t *testing.T) {
	t.Setenv("CLAUZETTE_PROFILE", "")
	c, err := loadJSON(t, `{
		"providers": {"openrouter": {"free_only": true}},
		"profiles": [
			{"name": "or", "provider": "openrouter", "model": "vendor/model:free"},
			{"name": "vllm", "provider": "openai", "model": "m", "base_url": "http://vllm:8000/v1/", "api_key_env": "VLLM_KEY"}
		]}`)
	if err != nil {
		t.Fatal(err)
	}
	or, _ := c.FindProfile("or")
	name, pc := c.ProviderFor(or)
	if name != "openrouter" || pc.BaseURL != "https://openrouter.ai/api/v1" || pc.APIKeyEnv != "OPENROUTER_API_KEY" || !pc.FreeOnly {
		t.Fatalf("openrouter defaults + free_only: %s %+v", name, pc)
	}
	vllm, _ := c.FindProfile("vllm")
	if _, pc := c.ProviderFor(vllm); pc.BaseURL != "http://vllm:8000/v1" || pc.APIKeyEnv != "VLLM_KEY" || pc.FreeOnly {
		t.Fatalf("profile overrides: %+v", pc)
	}
	if name, _ := c.ProviderFor(Profile{Model: "local"}); name != "ollama" || IsCloud("ollama") || IsCloud("") {
		t.Fatal("a profile without a provider is local")
	}

	for label, body := range map[string]string{
		"fit for cloud":        `{"profiles": [{"name": "a", "provider": "openrouter", "model": "m", "num_ctx": "fit"}]}`,
		"max_ctx for cloud":    `{"profiles": [{"name": "a", "provider": "openrouter", "model": "m", "max_ctx": 65536}]}`,
		"key instead of name":  `{"profiles": [{"name": "a", "provider": "openrouter", "model": "m", "api_key_env": "sk-or-v1-abc123"}]}`,
		"bad base_url":         `{"profiles": [{"name": "a", "provider": "openai", "model": "m", "base_url": "vllm:8000"}]}`,
		"unknown provider key": `{"providers": {"anthropic": {}}}`,
		"ollama provider key":  `{"providers": {"ollama": {}}}`,
		"key in providers":     `{"providers": {"openrouter": {"api_key_env": "sk-or-v1-abc"}}}`,
	} {
		if _, err := loadJSON(t, body); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
}

func TestKeyEnvNames(t *testing.T) {
	t.Setenv("CLAUZETTE_PROFILE", "")
	c, err := loadJSON(t, `{
		"providers": {"openrouter": {"api_key_env": "MY_OR_KEY"}},
		"profiles": [
			{"name": "vllm", "provider": "openai", "model": "m", "base_url": "http://vllm:8000/v1", "api_key_env": "VLLM_KEY"},
			{"name": "local", "model": "m"}
		]}`)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(c.KeyEnvNames(), ",")
	if got != "MY_OR_KEY,OPENAI_API_KEY,OPENROUTER_API_KEY,VLLM_KEY" {
		t.Fatalf("KeyEnvNames = %s", got)
	}
}

// A built-in key variable goes only to the built-in server.
func TestProviderKeyOnlyForItsServer(t *testing.T) {
	t.Setenv("CLAUZETTE_PROFILE", "")
	c, err := loadJSON(t, `{
		"providers": {"openrouter": {"base_url": "https://or-proxy.example/v1"}},
		"profiles": [
			{"name": "oai", "provider": "openai", "model": "m"},
			{"name": "vllm", "provider": "openai", "model": "m", "base_url": "http://vllm:8000/v1"},
			{"name": "vllm-key", "provider": "openai", "model": "m", "base_url": "http://vllm:8000/v1", "api_key_env": "VLLM_KEY"},
			{"name": "trailing", "provider": "openai", "model": "m", "base_url": "https://api.openai.com/v1/"},
			{"name": "or", "provider": "openrouter", "model": "m"}
		]}`)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"oai": "OPENAI_API_KEY", "vllm": "", "vllm-key": "VLLM_KEY", "trailing": "OPENAI_API_KEY", "or": ""} {
		p, _ := c.FindProfile(name)
		if _, pc := c.ProviderFor(p); pc.APIKeyEnv != want {
			t.Errorf("%s: api_key_env %q, want %q", name, pc.APIKeyEnv, want)
		}
	}
	p, _ := c.FindProfile("vllm")
	if got := c.Endpoint(p); got != "openai (http://vllm:8000/v1)" {
		t.Errorf("Endpoint = %q", got)
	}
}

// A cloud profile's model may be written the way /model takes it.
func TestCloudProfileModelPrefix(t *testing.T) {
	t.Setenv("CLAUZETTE_PROFILE", "")
	c, err := loadJSON(t, `{"profiles": [
		{"name": "or", "provider": "openrouter", "model": "openrouter:openrouter/free"},
		{"name": "plain", "provider": "openrouter", "model": "vendor/model:free"},
		{"name": "local", "model": "openrouter:not-a-provider-prefix-here"}
	]}`)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"or": "openrouter/free", "plain": "vendor/model:free", "local": "openrouter:not-a-provider-prefix-here"} {
		if p, _ := c.FindProfile(name); p.Model != want {
			t.Errorf("%s: model %q, want %q", name, p.Model, want)
		}
	}
}
