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

const envTemplate = `# Environment

- Today's date: {{.Date}}
- Workspace root: {{.Workspace}}. Relative paths are resolved against it; files outside it are not accessible.
- You run in a Linux container. exec_command runs commands with {{.Shell}} in the workspace, without a terminal or input.
- Internet access: {{if .Internet}}available to commands{{else}}not available{{end}}.
- Tools: {{join .Tools ", "}}.
- Context window: {{.NumCtx}} tokens. When a conversation gets long, its older part is replaced by a summary; re-read files instead of relying on old tool output.`

// PromptData is available to the base and extra prompt files as template
// fields, e.g. {{.Date}} or {{join .Tools ", "}}.
type PromptData struct {
	Date      string
	Workspace string
	Model     string
	Shell     string
	Tools     []string
	Internet  bool
	NumCtx    int
}

// BuildSystemPrompt assembles the system prompt from its layers: the base
// prompt (built in, or prompt.base_file), the optional prompt.extra_file,
// the environment block, and the first project file found in the
// workspace root (AGENTS.md, ...). It also returns notes for the operator.
//
// The result is stored in the session and reused unchanged for every
// request: any change to it would invalidate Ollama's prompt cache.
func BuildSystemPrompt(cfg *config.Config, data PromptData) (string, []string, error) {
	var notes []string
	base := basePrompt
	if cfg.Prompt.BaseFile != "" {
		b, err := os.ReadFile(cfg.Prompt.BaseFile)
		if err != nil {
			return "", nil, fmt.Errorf("prompt.base_file: %w", err)
		}
		base = string(b)
		notes = append(notes, "base prompt loaded from "+cfg.Prompt.BaseFile)
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
	for _, name := range cfg.Prompt.ProjectFiles {
		b, err := os.ReadFile(filepath.Join(data.Workspace, name))
		if err != nil {
			continue
		}
		text := string(b)
		if max := cfg.Prompt.ProjectMaxBytes; max > 0 && len(text) > max {
			text = strings.ToValidUTF8(text[:max], "") + "\n[truncated]"
			notes = append(notes, fmt.Sprintf("%s truncated to %d bytes (prompt.project_max_bytes)", name, max))
		}
		if text = strings.TrimSpace(text); text != "" {
			parts = append(parts, fmt.Sprintf("# Project instructions (from %s in the workspace root)\n\n%s", name, text))
			notes = append(notes, "project instructions loaded from "+name)
		}
		break
	}
	return strings.Join(parts, "\n\n") + "\n", notes, nil
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
