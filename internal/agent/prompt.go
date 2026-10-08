package agent

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"clauzette/internal/config"
)

//go:embed base_prompt.md
var basePrompt string

//go:embed rogue_prompt.md
var roguePrompt string

const envTemplate = `# Environment

- Today's date: {{.Date}}
{{if .Share -}}
- Your directory: {{.Workspace}}. Relative paths are resolved against it, and exec_command starts there. Keep your work in it.
- Shared workspace: {{.Share}}, which also holds the directories of other agents (e.g. ../other-agent/). {{if eq .SharedAccess "write"}}You may read and change files there, but only when the task needs it.{{else if eq .SharedAccess "read"}}You may read, list and search it; writing outside your directory is not allowed.{{else}}It is not accessible to you.{{end}}
{{- else -}}
- Workspace root: {{.Workspace}}. Relative paths are resolved against it; files outside it are not accessible.
{{- end}}
- You run in a Linux container. exec_command runs commands with {{.Shell}} in the workspace, without a terminal or input.
- Internet access: {{if .Internet}}available to commands{{else}}not available{{end}}.
- Tools: {{join .Tools ", "}}.
- Context window: {{.NumCtx}} tokens. When a conversation gets long, its older part is replaced by a summary; re-read files instead of relying on old tool output.`

// PromptData is available to the base and extra prompt files as template
// fields, e.g. {{.Date}} or {{join .Tools ", "}}.
type PromptData struct {
	Date         string
	Workspace    string // the agent's directory
	Share        string // the shared workspace root, "" when the agent works in the whole workspace
	SharedAccess string // none | read | write, with Share
	Rogue        bool   // the prompt for rogue mode
	Model        string
	Shell        string
	Tools        []string
	Internet     bool
	NumCtx       int
}

// BuildSystemPrompt assembles the system prompt from its layers: the base
// prompt (built in, or prompt.base_file), the optional prompt.extra_file,
// the environment block, and the first project file found (AGENTS.md, ...)
// in the shared workspace root and in the agent's directory. It also
// returns notes for the operator.
//
// The result is stored in the session and reused unchanged for every
// request: any change to it would invalidate Ollama's prompt cache.
func BuildSystemPrompt(cfg *config.Config, data PromptData) (string, []string, error) {
	var notes []string
	base, file, key := basePrompt, cfg.Prompt.BaseFile, "prompt.base_file"
	if data.Rogue {
		base, file, key = roguePrompt, cfg.Prompt.RogueBaseFile, "prompt.rogue_base_file"
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", nil, fmt.Errorf("%s: %w", key, err)
		}
		base = string(b)
		notes = append(notes, "base prompt loaded from "+file)
	}
	parts := []string{render("base", base, data, &notes)}
	if cfg.Prompt.ExtraFile != "" {
		b, err := os.ReadFile(cfg.Prompt.ExtraFile)
		if err != nil {
			return "", nil, fmt.Errorf("prompt.extra_file: %w", err)
		}
		if extra := render("extra", string(b), data, &notes); extra != "" {
			parts = append(parts, extra)
			notes = append(notes, "extra prompt loaded from "+cfg.Prompt.ExtraFile)
		}
	}
	parts = append(parts, render("environment", envTemplate, data, &notes))
	// Common instructions from the shared workspace root first, then the
	// agent's own, so the agent-specific ones can refine them.
	if data.Share != "" {
		parts, notes = projectInstructions(cfg, data.Share, "the shared workspace root", parts, notes)
	}
	parts, notes = projectInstructions(cfg, data.Workspace, workspaceLabel(data), parts, notes)
	return strings.Join(parts, "\n\n") + "\n", notes, nil
}

func workspaceLabel(data PromptData) string {
	if data.Share != "" {
		return "your directory"
	}
	return "the workspace root"
}

// projectInstructions appends the first of prompt.project_files found in dir.
func projectInstructions(cfg *config.Config, dir, where string, parts, notes []string) ([]string, []string) {
	for _, name := range cfg.Prompt.ProjectFiles {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		text := string(b)
		if max := cfg.Prompt.ProjectMaxBytes; max > 0 && len(text) > max {
			text = strings.ToValidUTF8(text[:max], "") + "\n[truncated]"
			notes = append(notes, fmt.Sprintf("%s in %s truncated to %d bytes (prompt.project_max_bytes)", name, where, max))
		}
		if text = strings.TrimSpace(text); text != "" {
			parts = append(parts, fmt.Sprintf("# Project instructions (from %s in %s)\n\n%s", name, where, text))
			notes = append(notes, fmt.Sprintf("project instructions loaded from %s in %s", name, where))
		}
		break
	}
	return parts, notes
}

func render(name, text string, data PromptData, notes *[]string) string {
	tpl, err := template.New(name).Funcs(template.FuncMap{"join": strings.Join}).Parse(text)
	if err != nil {
		*notes = append(*notes, fmt.Sprintf("%s prompt is not a valid template (%v); used as plain text", name, err))
		return strings.TrimSpace(text)
	}
	var sb strings.Builder
	if err := tpl.Execute(&sb, data); err != nil {
		*notes = append(*notes, fmt.Sprintf("%s prompt template failed (%v); used as plain text", name, err))
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(sb.String())
}
