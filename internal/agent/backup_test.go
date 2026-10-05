package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clauzette/internal/config"
	"clauzette/internal/session"
	"clauzette/internal/tools"
)

func newTestAgent(t *testing.T) (*Agent, *tools.Workspace, string) {
	t.Helper()
	ws, err := tools.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	s := &session.Session{ID: "testsession", Safe: true}
	a := New(Deps{Cfg: cfg, Registry: tools.NewRegistry(), Store: store, WS: ws, Audit: nil}, s, make(chan Event, 64))
	return a, ws, ws.Root
}

func TestBackupAndRestoreFile(t *testing.T) {
	a, _, root := newTestAgent(t)
	src := filepath.Join(root, "notes", "a.txt")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("notes", "a.txt")
	backup, err := a.backupTargets("delete_file", []tools.BackupRef{{Abs: src, Rel: rel}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(backup, rel))
	if err != nil || string(b) != "hello\nworld\n" {
		t.Fatalf("backup missing or wrong: %q %v", b, err)
	}
	if err := os.RemoveAll(filepath.Dir(src)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RestoreBackup(1); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(src)
	if err != nil || string(b) != "hello\nworld\n" {
		t.Fatalf("restore failed: %q %v", b, err)
	}
	if _, err := a.RestoreBackup(9); err == nil {
		t.Fatal("restore of a missing number was accepted")
	}
}

func TestBackupAndRestoreTree(t *testing.T) {
	a, _, root := newTestAgent(t)
	dir := filepath.Join(root, "pkg")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.backupTargets("delete_file", []tools.BackupRef{{Abs: dir, Rel: "pkg"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RestoreBackup(1); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "a.txt"), filepath.Join(dir, "sub", "b.txt")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("restored tree incomplete: %v", err)
		}
	}
}

func TestBackupPrunesOldest(t *testing.T) {
	a, _, root := newTestAgent(t)
	a.Cfg.Safe.MaxBackups = 2
	src := filepath.Join(root, "f.txt")
	if err := os.WriteFile(src, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := a.backupTargets("delete_file", []tools.BackupRef{{Abs: src, Rel: "f.txt"}}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(a.backupDir())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("expected 2 backups after pruning, got %d", n)
	}
}

func TestSafeReportListsBackups(t *testing.T) {
	a, _, root := newTestAgent(t)
	src := filepath.Join(root, "f.txt")
	if err := os.WriteFile(src, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.backupTargets("delete_file", []tools.BackupRef{{Abs: src, Rel: "f.txt"}}); err != nil {
		t.Fatal(err)
	}
	report := a.SafeReport()
	for _, sub := range []string{"safe mode: on", "delete_file", "f.txt"} {
		if !strings.Contains(report, sub) {
			t.Fatalf("report missing %q:\n%s", sub, report)
		}
	}
}
