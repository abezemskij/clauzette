package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Todo statuses.
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoDone       = "done"
)

// TodoItem is one task in the plan.
type TodoItem struct {
	Text   string `json:"text"`
	Status string `json:"status"`
}

// TodoList is the agent's task plan: a short, ordered checklist it maintains
// across a turn (and, through the session, across restarts). It is shared
// between the todo_write and todo_read tools and read back by the agent for
// persistence, so it is guarded by a mutex. Unlike a transcript message, it
// survives context compaction, so the agent can recover its plan with
// todo_read after old tool output has been elided.
type TodoList struct {
	mu    sync.Mutex
	items []TodoItem
}

func NewTodoList() *TodoList { return &TodoList{} }

// Set replaces the whole list with a copy of items.
func (l *TodoList) Set(items []TodoItem) {
	cp := make([]TodoItem, len(items))
	copy(cp, items)
	l.mu.Lock()
	l.items = cp
	l.mu.Unlock()
}

// Items returns a copy of the current list.
func (l *TodoList) Items() []TodoItem {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make([]TodoItem, len(l.items))
	copy(cp, l.items)
	return cp
}

// Render formats the list for a tool result (and the terminal).
func (l *TodoList) Render() string {
	items := l.Items()
	if len(items) == 0 {
		return "The todo list is empty."
	}
	mark := map[string]string{TodoPending: "[ ]", TodoInProgress: "[~]", TodoDone: "[x]"}
	done, inProgress, pending := 0, 0, 0
	var sb strings.Builder
	for _, it := range items {
		m := mark[it.Status]
		if m == "" {
			m = "[ ]"
		}
		fmt.Fprintf(&sb, "%s %s\n", m, it.Text)
		switch it.Status {
		case TodoDone:
			done++
		case TodoInProgress:
			inProgress++
		default:
			pending++
		}
	}
	fmt.Fprintf(&sb, "\n%d task%s: %d done, %d in progress, %d pending.",
		len(items), plural(len(items)), done, inProgress, pending)
	return sb.String()
}

// -------------------------------------------------------------- todo_write

type TodoWrite struct {
	List *TodoList
}

func (t *TodoWrite) Name() string { return "todo_write" }
func (t *TodoWrite) Risk() Risk   { return ReadOnly }

func (t *TodoWrite) Description() string {
	return "Record or update your task plan for the current work. Pass the whole list each time: it " +
		"replaces the previous one. Use it for multi-step tasks to track what is done and what is left, " +
		"and keep exactly one task in_progress at a time. The plan survives context compaction, so you " +
		"can recover it later with todo_read. Bookkeeping only; it changes no files and needs no approval."
}

func (t *TodoWrite) Parameters() map[string]any {
	return objectSchema([]string{"todos"}, map[string]any{
		"todos": map[string]any{
			"type":        "array",
			"description": "The complete task list, in order. Replaces the previous list.",
			"items": objectSchema([]string{"text"}, map[string]any{
				"text":   prop("string", "What the task is, in a few words."),
				"status": map[string]any{"type": "string", "enum": []string{TodoPending, TodoInProgress, TodoDone}, "description": "pending (default), in_progress, or done."},
			}),
		},
	})
}

func (t *TodoWrite) Prepare(_ context.Context, args Args) (*Action, error) {
	items, err := parseTodos(args)
	if err != nil {
		return nil, err
	}
	return &Action{
		Summary: fmt.Sprintf("update the todo list (%d task%s)", len(items), plural(len(items))),
		Run: func(context.Context) (string, error) {
			t.List.Set(items)
			return t.List.Render(), nil
		},
	}, nil
}

func parseTodos(args Args) ([]TodoItem, error) {
	raw, ok := args["todos"]
	if !ok || raw == nil {
		return nil, errors.New(`missing required argument "todos"`)
	}
	// Models sometimes send the array as a JSON-encoded string; accept that too.
	if s, isStr := raw.(string); isStr {
		var decoded []any
		if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &decoded); err != nil {
			return nil, errors.New(`argument "todos" must be an array of {text, status}`)
		}
		raw = decoded
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, errors.New(`argument "todos" must be an array of {text, status}`)
	}
	items := make([]TodoItem, 0, len(list))
	inProgress := 0
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("todo %d must be an object with text and status", i+1)
		}
		ta := Args(m)
		text, err := ta.Str("text", true)
		if err != nil {
			return nil, fmt.Errorf("todo %d: %v", i+1, err)
		}
		if strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("todo %d: text must not be empty", i+1)
		}
		status, err := ta.Str("status", false)
		if err != nil {
			return nil, fmt.Errorf("todo %d: %v", i+1, err)
		}
		status = strings.TrimSpace(status)
		if status == "" {
			status = TodoPending
		}
		switch status {
		case TodoPending, TodoDone:
		case TodoInProgress:
			inProgress++
		default:
			return nil, fmt.Errorf("todo %d: status must be %q, %q or %q", i+1, TodoPending, TodoInProgress, TodoDone)
		}
		items = append(items, TodoItem{Text: text, Status: status})
	}
	if inProgress > 1 {
		return nil, fmt.Errorf("only one task may be in_progress at a time (%d were given)", inProgress)
	}
	return items, nil
}

// --------------------------------------------------------------- todo_read

type TodoRead struct {
	List *TodoList
}

func (t *TodoRead) Name() string { return "todo_read" }
func (t *TodoRead) Risk() Risk   { return ReadOnly }

func (t *TodoRead) Description() string {
	return "Show your current task plan. Useful after a long stretch of work or once the context has " +
		"been compacted, to recover what you were doing and what is left."
}

func (t *TodoRead) Parameters() map[string]any {
	return objectSchema(nil, map[string]any{})
}

func (t *TodoRead) Prepare(_ context.Context, _ Args) (*Action, error) {
	return &Action{
		Summary: "read the todo list",
		Run: func(context.Context) (string, error) {
			return t.List.Render(), nil
		},
	}, nil
}
