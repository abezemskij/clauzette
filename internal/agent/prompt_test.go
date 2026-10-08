package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clauzette/internal/config"
)

func TestPromptSharedLayoutAndProjectFiles(t *testing.T) {
	share := t.TempDir()
	own := filepath.Join(share, "clauzette-0")
	os.MkdirAll(own, 0o755)
	os.WriteFile(filepath.Join(share, "AGENTS.md"), []byte("COMMON RULES"), 0o644)
	os.WriteFile(filepath.Join(own, "AGENTS.md"), []byte("AGENT RULES"), 0o644)

	for _, access := range []string{"none", "read", "write"} {
		sys, _, err := BuildSystemPrompt(config.Defaults(), PromptData{Workspace: own, Share: share, SharedAccess: access, Tools: []string{"read_file"}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sys, "Your directory: "+own) || !strings.Contains(sys, "Shared workspace: "+share) {
			t.Fatalf("%s: layout missing:\n%s", access, sys)
		}
		want := map[string]string{"none": "not accessible", "read": "writing outside your directory is not allowed", "write": "read and change files there"}[access]
		if !strings.Contains(sys, want) {
			t.Fatalf("%s: access sentence missing (%q):\n%s", access, want, sys)
		}
		common, agent := strings.Index(sys, "COMMON RULES"), strings.Index(sys, "AGENT RULES")
		if common < 0 || agent < 0 || common > agent {
			t.Fatalf("%s: both project files, common first, expected:\n%s", access, sys)
		}
	}
}

func TestPromptSingleWorkspaceUnchanged(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("RULES"), 0o644)
	sys, _, err := BuildSystemPrompt(config.Defaults(), PromptData{Workspace: root, Tools: []string{"read_file"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sys, "- Workspace root: "+root+".") || strings.Contains(sys, "Shared workspace") {
		t.Fatalf("single-workspace environment changed:\n%s", sys)
	}
	if strings.Count(sys, "RULES") != 1 || !strings.Contains(sys, "in the workspace root)") {
		t.Fatalf("project file should appear once, from the workspace root:\n%s", sys)
	}
}

func TestPromptModes(t *testing.T) {
	root := t.TempDir()
	normal, _, err := BuildSystemPrompt(config.Defaults(), PromptData{Workspace: root, Tools: []string{"read_file"}})
	if err != nil {
		t.Fatal(err)
	}
	rogue, _, err := BuildSystemPrompt(config.Defaults(), PromptData{Workspace: root, Tools: []string{"read_file"}, Rogue: true})
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{"normal": normal, "rogue": rogue} {
		if strings.Contains(p, "{{") || strings.Contains(p, "Template comments") {
			t.Fatalf("%s prompt still contains the template comment:\n%s", name, p[:200])
		}
		if !strings.HasPrefix(p, "You are Clauzette") {
			t.Fatalf("%s prompt should start with the introduction: %q", name, p[:60])
		}
	}
	for _, want := range []string{"# Thinking", "call finish", "Nobody approves your actions"} {
		if !strings.Contains(rogue, want) {
			t.Fatalf("rogue prompt is missing %q", want)
		}
	}
	for _, unwanted := range []string{"The operator approves", "ask one focused question"} {
		if strings.Contains(rogue, unwanted) {
			t.Fatalf("rogue prompt still says %q", unwanted)
		}
	}
	if strings.Contains(normal, "rogue mode") {
		t.Fatal("the normal prompt should not mention rogue mode")
	}
}

func TestRoguePromptOverride(t *testing.T) {
	f := filepath.Join(t.TempDir(), "rogue.md")
	os.WriteFile(f, []byte("CUSTOM ROGUE for {{.Workspace}}"), 0o644)
	cfg := config.Defaults()
	cfg.Prompt.RogueBaseFile = f
	sys, notes, err := BuildSystemPrompt(cfg, PromptData{Workspace: "/w", Rogue: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sys, "CUSTOM ROGUE for /w") || len(notes) == 0 {
		t.Fatalf("prompt.rogue_base_file not used: %q %v", sys[:40], notes)
	}
	cfg.Prompt.RogueBaseFile = filepath.Join(t.TempDir(), "missing.md")
	if _, _, err := BuildSystemPrompt(cfg, PromptData{Workspace: "/w", Rogue: true}); err == nil {
		t.Fatal("a missing prompt.rogue_base_file should be an error")
	}
}
