package agent

import (
	"path/filepath"
	"testing"

	"clauzette/internal/config"
	"clauzette/internal/session"
	"clauzette/internal/tools"
)

// TestTodosSurviveReload checks that the task plan is persisted in the session
// and restored into a fresh agent (and its shared TodoList) on reload.
func TestTodosSurviveReload(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	todos := tools.NewTodoList()
	deps := Deps{
		Cfg:      config.Defaults(),
		Registry: tools.NewRegistry(&tools.TodoWrite{List: todos}, &tools.TodoRead{List: todos}),
		Store:    store,
		Todos:    todos,
	}
	a := New(deps, &session.Session{ID: "todosession"}, make(chan Event, 8))
	todos.Set([]tools.TodoItem{
		{Text: "done thing", Status: tools.TodoDone},
		{Text: "current thing", Status: tools.TodoInProgress},
	})
	a.Save()

	loaded, err := a.Store.Load("todosession")
	if err != nil {
		t.Fatal(err)
	}
	// A fresh agent with its own (empty) list should recover the plan on New.
	todos2 := tools.NewTodoList()
	deps2 := deps
	deps2.Todos = todos2
	New(deps2, loaded, make(chan Event, 8))

	got := todos2.Items()
	if len(got) != 2 || got[0].Text != "done thing" || got[1].Status != tools.TodoInProgress {
		t.Fatalf("todos not restored on reload: %+v", got)
	}
}
