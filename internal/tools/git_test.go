package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newGitRepo makes a temp git repo with one commit and returns a workspace
// rooted at it. It skips the test if git is not available.
func newGitRepo(t *testing.T) (*Workspace, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-q", "-m", "initial commit")
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return ws, root
}

func TestGitStatus(t *testing.T) {
	ws, root := newGitRepo(t)
	// Modify a tracked file and add an untracked one.
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("one\nTWO\n"), 0o644)
	os.WriteFile(filepath.Join(root, "new.txt"), []byte("hi\n"), 0o644)

	st := &GitStatus{WS: ws, MaxOutput: 100000}
	act, err := st.Prepare(context.Background(), Args{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := act.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.txt") || !strings.Contains(out, "new.txt") {
		t.Fatalf("status missing expected files:\n%s", out)
	}
}

func TestGitDiff(t *testing.T) {
	ws, root := newGitRepo(t)
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("one\nTWO\n"), 0o644)

	d := &GitDiff{WS: ws, MaxOutput: 100000}
	act, err := d.Prepare(context.Background(), Args{"path": "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := act.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "-two") || !strings.Contains(out, "+TWO") {
		t.Fatalf("diff missing the change:\n%s", out)
	}
}

func TestGitLog(t *testing.T) {
	ws, _ := newGitRepo(t)
	l := &GitLog{WS: ws, MaxOutput: 100000}
	// JSON numbers decode as float64; mimic that here.
	act, err := l.Prepare(context.Background(), Args{"max": 5.0})
	if err != nil {
		t.Fatal(err)
	}
	out, err := act.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "initial commit") {
		t.Fatalf("log missing the commit:\n%s", out)
	}
}

func TestGitRejectsOptionInjection(t *testing.T) {
	ws, _ := newGitRepo(t)
	d := &GitDiff{WS: ws, MaxOutput: 100000}
	if _, err := d.Prepare(context.Background(), Args{"ref": "--output=/tmp/x"}); err == nil {
		t.Fatal("ref that looks like an option was accepted")
	}
	st := &GitStatus{WS: ws, MaxOutput: 100000}
	if _, err := st.Prepare(context.Background(), Args{"path": "--foo"}); err == nil {
		t.Fatal("path that looks like an option was accepted")
	}
}

func TestGitNotARepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	st := &GitStatus{WS: ws, MaxOutput: 100000}
	act, err := st.Prepare(context.Background(), Args{})
	if err != nil {
		t.Fatal(err)
	}
	// Not a repo: git exits non-zero; the tool reports it rather than erroring.
	out, err := act.Run(context.Background())
	if err != nil {
		t.Fatalf("expected a reported message, got error: %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "not a git repository") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}
