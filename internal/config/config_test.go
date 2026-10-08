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
