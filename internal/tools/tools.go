// Package tools implements the functions the model can call. Each tool
// validates its arguments and prepares an Action first; the agent decides
// whether the action needs operator approval and only then runs it.
package tools

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"clauzette/internal/ollama"
)

// Risk decides whether a tool needs approval: read-only tools run
// automatically, write and exec tools ask first (unless auto-approved).
// Destructive tools (mv, delete) are irreversible: when safe mode is on
// they are always confirmed one by one and their target is backed up first.
type Risk int

const (
	ReadOnly Risk = iota
	Write
	Exec
	Destructive
)

func (r Risk) String() string {
	switch r {
	case ReadOnly:
		return "read-only"
	case Write:
		return "write"
	case Exec:
		return "exec"
	case Destructive:
		return "destructive"
	}
	return "unknown"
}

// BackupRef names a file or directory a destructive action will change. In
// safe mode the agent copies these to the backup directory before the
// action runs; if the backup fails, the action does not.
type BackupRef struct {
	Abs string // absolute path of the file or directory
	Rel string // path relative to the workspace root, for naming and display
}

// Action is a validated, ready-to-run tool invocation. Summary and Preview
// (for example a diff) are shown to the operator when approval is needed.
type Action struct {
	Summary string
	Preview string
	Backups []BackupRef // set by destructive tools; empty for all others
	Run     func(ctx context.Context) (string, error)
}

type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any // JSON schema of the arguments
	Risk() Risk
	Prepare(ctx context.Context, args Args) (*Action, error)
}

// Args are the arguments of a tool call as decoded from JSON. The getters
// are lenient (a number passed as a string is accepted) because models get
// types wrong now and then; errors are phrased so the model can fix the call.
type Args map[string]any

func (a Args) Str(name string, required bool) (string, error) {
	v, ok := a[name]
	if !ok || v == nil {
		if required {
			return "", fmt.Errorf("missing required argument %q", name)
		}
		return "", nil
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case bool:
		return strconv.FormatBool(t), nil
	}
	return "", fmt.Errorf("argument %q must be a string", name)
}

func (a Args) Int(name string, def int) (int, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return def, nil
	}
	switch t := v.(type) {
	case float64:
		return int(t), nil
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return def, nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("argument %q must be an integer", name)
		}
		return n, nil
	}
	return 0, fmt.Errorf("argument %q must be an integer", name)
}

func (a Args) Bool(name string, def bool) (bool, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return def, nil
	}
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		if err != nil {
			return false, fmt.Errorf("argument %q must be true or false", name)
		}
		return b, nil
	}
	return false, fmt.Errorf("argument %q must be true or false", name)
}

// Registry holds the enabled tools, always in name order. A stable order
// matters: the tool definitions are part of every prompt, and a prompt that
// changes from request to request defeats Ollama's prompt cache.
type Registry struct {
	tools  []Tool
	byName map[string]Tool
}

func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{byName: map[string]Tool{}}
	for _, t := range ts {
		r.byName[t.Name()] = t
	}
	for _, t := range r.byName {
		r.tools = append(r.tools, t)
	}
	sort.Slice(r.tools, func(i, j int) bool { return r.tools[i].Name() < r.tools[j].Name() })
	return r
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

func (r *Registry) Names() []string {
	names := make([]string, len(r.tools))
	for i, t := range r.tools {
		names[i] = t.Name()
	}
	return names
}

func (r *Registry) Specs() []ollama.ToolSpec {
	specs := make([]ollama.ToolSpec, 0, len(r.tools))
	for _, t := range r.tools {
		specs = append(specs, ollama.ToolSpec{
			Type: "function",
			Function: ollama.FunctionSpec{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Parameters(),
			},
		})
	}
	return specs
}

func objectSchema(required []string, props map[string]any) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}
