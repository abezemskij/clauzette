package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestAgentEnvironment(t *testing.T) {
	t.Setenv("CLAUZETTE_AGENT_ID", "clauzette-0")
	t.Setenv("CLAUZETTE_AGENT_DIR", "true")
	t.Setenv("CLAUZETTE_SHARED_ACCESS", "write")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentID != "clauzette-0" || !c.AgentDir || c.SharedAccess != "write" {
		t.Fatalf("environment not applied: %q %v %q", c.AgentID, c.AgentDir, c.SharedAccess)
	}
	if got, want := c.AgentSessionsDir(), filepath.Join(c.SessionsDir, "clauzette-0"); got != want {
		t.Fatalf("sessions dir %q, want %q", got, want)
	}
}

func TestAgentDefaults(t *testing.T) {
	t.Setenv("CLAUZETTE_AGENT_ID", "")
	t.Setenv("CLAUZETTE_AGENT_DIR", "")
	t.Setenv("CLAUZETTE_SHARED_ACCESS", "")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentDir || c.SharedAccess != "read" || c.AgentID == "" {
		t.Fatalf("defaults: agent_dir %v, shared_access %q, agent_id %q (want the hostname)", c.AgentDir, c.SharedAccess, c.AgentID)
	}
	if c.AgentSessionsDir() != c.SessionsDir {
		t.Fatal("without agent_dir, sessions stay where they were")
	}
}

func TestAgentEnvironmentInvalid(t *testing.T) {
	t.Setenv("CLAUZETTE_SHARED_ACCESS", "everything")
	if _, err := Load(""); err == nil {
		t.Fatal("an invalid shared_access was accepted")
	}
	t.Setenv("CLAUZETTE_SHARED_ACCESS", "")
	t.Setenv("CLAUZETTE_AGENT_DIR", "maybe")
	if _, err := Load(""); err == nil {
		t.Fatal("an invalid CLAUZETTE_AGENT_DIR was accepted")
	}
}

func TestKeepRecentRatio(t *testing.T) {
	for _, tc := range []struct {
		ratio string
		ok    bool
	}{{"0", true}, {"0.5", true}, {"0.9", true}, {"0.95", false}, {"-0.1", false}} {
		path := filepath.Join(t.TempDir(), "c.json")
		os.WriteFile(path, []byte(fmt.Sprintf(`{"context": {"keep_recent_ratio": %s}}`, tc.ratio)), 0o644)
		if _, err := Load(path); (err == nil) != tc.ok {
			t.Errorf("keep_recent_ratio %s: err = %v, want ok %v", tc.ratio, err, tc.ok)
		}
	}
}

func TestFetchDefaults(t *testing.T) {
	// An omitted fetch section gets the built-in defaults.
	path := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(path, []byte(`{}`), 0o644)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tools.FetchMaxBytes != 128<<10 {
		t.Errorf("fetch_max_bytes default = %d, want %d", c.Tools.FetchMaxBytes, 128<<10)
	}
	if c.Tools.FetchUserAgent != DefaultFetchUserAgent {
		t.Errorf("fetch_user_agent default = %q", c.Tools.FetchUserAgent)
	}
	// A zero/empty override is normalised back to the defaults.
	os.WriteFile(path, []byte(`{"tools": {"fetch_max_bytes": 0, "fetch_user_agent": ""}}`), 0o644)
	c, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tools.FetchMaxBytes != 128<<10 || c.Tools.FetchUserAgent != DefaultFetchUserAgent {
		t.Errorf("zero overrides not normalised: %d %q", c.Tools.FetchMaxBytes, c.Tools.FetchUserAgent)
	}
}

func TestSearchValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	// searxng without an endpoint is an error.
	os.WriteFile(path, []byte(`{"search":{"provider":"searxng"}}`), 0o644)
	if _, err := Load(path); err == nil {
		t.Fatal("searxng without endpoint was accepted")
	}
	// with an endpoint it loads.
	os.WriteFile(path, []byte(`{"search":{"provider":"searxng","endpoint":"http://searxng:8080"}}`), 0o644)
	if _, err := Load(path); err != nil {
		t.Fatalf("valid searxng config rejected: %v", err)
	}
	// an unknown provider is an error.
	os.WriteFile(path, []byte(`{"search":{"provider":"bing"}}`), 0o644)
	if _, err := Load(path); err == nil {
		t.Fatal("unknown provider accepted")
	}
}
