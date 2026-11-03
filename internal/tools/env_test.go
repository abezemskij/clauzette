package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCommandsDoNotSeeHiddenEnv(t *testing.T) {
	t.Setenv("CLAUZETTE_TEST_HIDDEN", "hidden-value")
	t.Setenv("CLAUZETTE_TEST_VISIBLE", "visible-value")
	HideEnv("CLAUZETTE_TEST_HIDDEN")
	t.Cleanup(func() { HideEnv() })

	ws, _ := NewWorkspace(t.TempDir())
	ex := &ExecCommand{WS: ws, Shell: "/bin/sh", DefaultTimeout: time.Minute, MaxTimeout: time.Minute, MaxOutput: 100000}
	act, err := ex.Prepare(context.Background(), Args{"command": "env"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := act.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "hidden-value") || strings.Contains(out, "CLAUZETTE_TEST_HIDDEN") {
		t.Fatalf("a hidden variable reached the command:\n%s", out)
	}
	if !strings.Contains(out, "CLAUZETTE_TEST_VISIBLE=visible-value") || !strings.Contains(out, "PAGER=cat") {
		t.Fatalf("the rest of the environment, and the tool's own variables, must stay:\n%s", out)
	}
}

func TestChildEnv(t *testing.T) {
	t.Setenv("CLAUZETTE_TEST_A", "1")
	t.Setenv("CLAUZETTE_TEST_AB", "2") // a longer name with the same prefix stays
	HideEnv("CLAUZETTE_TEST_A", "")
	t.Cleanup(func() { HideEnv() })
	env := strings.Join(childEnv("X=y"), "\n") + "\n"
	if strings.Contains(env, "CLAUZETTE_TEST_A=") || !strings.Contains(env, "CLAUZETTE_TEST_AB=2\n") || !strings.HasSuffix(env, "X=y\n") {
		t.Fatalf("env:\n%s", env)
	}
	HideEnv()
	if !strings.Contains(strings.Join(childEnv(), "\n"), "CLAUZETTE_TEST_A=1") {
		t.Fatal("HideEnv() with no names must clear the set")
	}
}
