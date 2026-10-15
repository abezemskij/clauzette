package tools

import (
	"context"
	"strings"
	"testing"
)

func todoItems(items ...map[string]any) Args {
	list := make([]any, len(items))
	for i, it := range items {
		list[i] = it
	}
	return Args{"todos": list}
}

func TestTodoWriteAndRead(t *testing.T) {
	list := NewTodoList()
	w := &TodoWrite{List: list}
	r := &TodoRead{List: list}
	ctx := context.Background()

	act, err := w.Prepare(ctx, todoItems(
		map[string]any{"text": "read the code", "status": "done"},
		map[string]any{"text": "write the tool", "status": "in_progress"},
		map[string]any{"text": "add tests"}, // defaults to pending
	))
	if err != nil {
		t.Fatal(err)
	}
	out, err := act.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[x] read the code", "[~] write the tool", "[ ] add tests", "3 tasks: 1 done, 1 in progress, 1 pending"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	// todo_read returns the same list without changing it.
	ract, err := r.Prepare(ctx, Args{})
	if err != nil {
		t.Fatal(err)
	}
	rout, err := ract.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rout != out {
		t.Fatalf("todo_read differs from todo_write:\n%s\n---\n%s", rout, out)
	}
}

func TestTodoWriteReplaces(t *testing.T) {
	list := NewTodoList()
	w := &TodoWrite{List: list}
	ctx := context.Background()

	act, _ := w.Prepare(ctx, todoItems(map[string]any{"text": "first"}))
	act.Run(ctx)
	act, _ = w.Prepare(ctx, todoItems(map[string]any{"text": "second"}))
	out, _ := act.Run(ctx)
	if strings.Contains(out, "first") || !strings.Contains(out, "second") {
		t.Fatalf("list was not replaced:\n%s", out)
	}
}

func TestTodoValidation(t *testing.T) {
	w := &TodoWrite{List: NewTodoList()}
	ctx := context.Background()

	if _, err := w.Prepare(ctx, Args{}); err == nil {
		t.Fatal("missing todos accepted")
	}
	if _, err := w.Prepare(ctx, todoItems(map[string]any{"text": ""})); err == nil {
		t.Fatal("empty text accepted")
	}
	if _, err := w.Prepare(ctx, todoItems(map[string]any{"text": "x", "status": "bogus"})); err == nil {
		t.Fatal("bad status accepted")
	}
	if _, err := w.Prepare(ctx, todoItems(
		map[string]any{"text": "a", "status": "in_progress"},
		map[string]any{"text": "b", "status": "in_progress"},
	)); err == nil {
		t.Fatal("two in_progress tasks accepted")
	}
}

func TestTodoStringifiedArray(t *testing.T) {
	w := &TodoWrite{List: NewTodoList()}
	ctx := context.Background()
	act, err := w.Prepare(ctx, Args{"todos": `[{"text":"a","status":"done"},{"text":"b"}]`})
	if err != nil {
		t.Fatalf("stringified todos rejected: %v", err)
	}
	out, _ := act.Run(ctx)
	if !strings.Contains(out, "[x] a") || !strings.Contains(out, "[ ] b") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestTodoReadEmpty(t *testing.T) {
	r := &TodoRead{List: NewTodoList()}
	act, _ := r.Prepare(context.Background(), Args{})
	out, _ := act.Run(context.Background())
	if !strings.Contains(out, "empty") {
		t.Fatalf("expected empty-list message, got:\n%s", out)
	}
}
