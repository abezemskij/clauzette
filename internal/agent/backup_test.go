package agent

import (
	"encoding/json"
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

// refFor resolves a workspace path the way mv_file and delete_file do.
func refFor(t *testing.T, ws *tools.Workspace, p string) tools.BackupRef {
	t.Helper()
	loc, err := ws.Resolve(p, tools.AccessWrite)
	if err != nil {
		t.Fatal(err)
	}
	return loc.BackupRef()
}

func TestBackupAndRestoreFile(t *testing.T) {
	a, ws, root := newTestAgent(t)
	src := filepath.Join(root, "notes", "a.txt")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("notes", "a.txt")
	backup, err := a.backupTargets("delete_file", []tools.BackupRef{refFor(t, ws, rel)})
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
	if _, err := a.RestoreBackup(1, false); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(src)
	if err != nil || string(b) != "hello\nworld\n" {
		t.Fatalf("restore failed: %q %v", b, err)
	}
	if _, err := a.RestoreBackup(9, false); err == nil {
		t.Fatal("restore of a missing number was accepted")
	}
}

func TestBackupAndRestoreTree(t *testing.T) {
	a, ws, root := newTestAgent(t)
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
	if _, err := a.backupTargets("delete_file", []tools.BackupRef{refFor(t, ws, "pkg")}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RestoreBackup(1, false); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "a.txt"), filepath.Join(dir, "sub", "b.txt")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("restored tree incomplete: %v", err)
		}
	}
}

func TestBackupPrunesOldest(t *testing.T) {
	a, ws, root := newTestAgent(t)
	a.Cfg.Safe.MaxBackups = 2
	src := filepath.Join(root, "f.txt")
	if err := os.WriteFile(src, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := a.backupTargets("delete_file", []tools.BackupRef{refFor(t, ws, "f.txt")}); err != nil {
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
	a, ws, root := newTestAgent(t)
	src := filepath.Join(root, "f.txt")
	if err := os.WriteFile(src, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.backupTargets("delete_file", []tools.BackupRef{refFor(t, ws, "f.txt")}); err != nil {
		t.Fatal(err)
	}
	report := a.SafeReport()
	for _, sub := range []string{"safe mode: on", "delete_file", "f.txt"} {
		if !strings.Contains(report, sub) {
			t.Fatalf("report missing %q:\n%s", sub, report)
		}
	}
}

func newAgentFor(t *testing.T, ws *tools.Workspace) *Agent {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	s := &session.Session{ID: "testsession", Safe: true}
	return New(Deps{Cfg: config.Defaults(), Registry: tools.NewRegistry(), Store: store, WS: ws}, s, make(chan Event, 64))
}

func readString(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func isLink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

func TestBackupKeepsSymlinksAsLinks(t *testing.T) {
	a, ws, root := newTestAgent(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("SECRET\n"), 0o644)
	pkg := filepath.Join(root, "venv")
	os.MkdirAll(filepath.Join(pkg, "lib"), 0o755)
	os.WriteFile(filepath.Join(pkg, "lib", "x.py"), []byte("print(1)\n"), 0o644)
	for link, target := range map[string]string{"lib64": "lib", "dangling": "missing", "out": outside} {
		if err := os.Symlink(target, filepath.Join(pkg, link)); err != nil {
			t.Skip("cannot create symlink:", err)
		}
	}

	backup, err := a.backupTargets("delete_file", []tools.BackupRef{refFor(t, ws, "venv")})
	if err != nil {
		t.Fatalf("a tree with symlinks should back up: %v", err)
	}
	for _, l := range []string{"lib64", "dangling", "out"} {
		if !isLink(filepath.Join(backup, "venv", l)) {
			t.Fatalf("%s should be a link in the backup", l)
		}
	}
	filepath.WalkDir(backup, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && strings.Contains(readString(t, p), "SECRET") {
			t.Fatalf("content from outside the workspace was copied into %s", p)
		}
		return nil
	})

	os.RemoveAll(pkg)
	if _, err := a.RestoreBackup(1, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(filepath.Join(pkg, "lib64")); got != "lib" {
		t.Fatalf("lib64 should be restored as a link to lib, got %q", got)
	}
	if !isLink(filepath.Join(pkg, "dangling")) || !isLink(filepath.Join(pkg, "out")) {
		t.Fatal("the other links should be restored as links")
	}
	if readString(t, filepath.Join(pkg, "lib", "x.py")) != "print(1)\n" {
		t.Fatal("file content not restored")
	}
	if readString(t, outside) != "SECRET\n" {
		t.Fatal("the file outside was touched")
	}
}

func TestRestoreConflictRefusedWithoutForce(t *testing.T) {
	a, ws, root := newTestAgent(t)
	f := filepath.Join(root, "f.txt")
	os.WriteFile(f, []byte("old\n"), 0o644)
	if _, err := a.backupTargets("delete_file", []tools.BackupRef{refFor(t, ws, "f.txt")}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(f, []byte("newer\n"), 0o644)

	if !strings.Contains(a.SafeReport(), "restore needs force") {
		t.Fatalf("/safe should flag the conflict:\n%s", a.SafeReport())
	}
	_, err := a.RestoreBackup(1, false)
	if err == nil || !strings.Contains(err.Error(), "f.txt") || !strings.Contains(err.Error(), "force") {
		t.Fatalf("expected a conflict naming f.txt, got %v", err)
	}
	if readString(t, f) != "newer\n" {
		t.Fatal("a refused restore changed the file")
	}
	if n := len(a.listBackups()); n != 1 {
		t.Fatalf("a refused restore should not create a backup, have %d", n)
	}
}

func TestForcedRestoreReplacesAndCanBeUndone(t *testing.T) {
	a, ws, root := newTestAgent(t)
	a.Cfg.Safe.MaxBackups = 1 // the pre-restore backup must not prune the one being restored
	dir := filepath.Join(root, "pkg")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old a\n"), 0o644)
	if _, err := a.backupTargets("delete_file", []tools.BackupRef{refFor(t, ws, "pkg")}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("new a\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("keep me\n"), 0o644)

	out, err := a.RestoreBackup(1, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 existing entry replaced") {
		t.Fatalf("the result should say what was replaced: %q", out)
	}
	if readString(t, filepath.Join(dir, "a.txt")) != "old a\n" {
		t.Fatal("the conflicting file was not replaced")
	}
	if readString(t, filepath.Join(dir, "extra.txt")) != "keep me\n" {
		t.Fatal("files not in the backup must be kept")
	}

	// Backup 1 is now the pre-restore copy; restoring it undoes the restore.
	if _, err := a.RestoreBackup(1, true); err != nil {
		t.Fatal(err)
	}
	if readString(t, filepath.Join(dir, "a.txt")) != "new a\n" {
		t.Fatal("undoing the forced restore did not bring back the replaced version")
	}
}

func TestForcedRestoreReplacesLinkNotItsTarget(t *testing.T) {
	a, ws, root := newTestAgent(t)
	f := filepath.Join(root, "f.txt")
	os.WriteFile(f, []byte("old\n"), 0o644)
	if _, err := a.backupTargets("delete_file", []tools.BackupRef{refFor(t, ws, "f.txt")}); err != nil {
		t.Fatal(err)
	}
	os.Remove(f)
	os.WriteFile(filepath.Join(root, "other.txt"), []byte("other\n"), 0o644)
	if err := os.Symlink("other.txt", f); err != nil {
		t.Skip("cannot create symlink:", err)
	}
	if _, err := a.RestoreBackup(1, true); err != nil {
		t.Fatal(err)
	}
	if isLink(f) || readString(t, f) != "old\n" {
		t.Fatal("f.txt should be a regular file again")
	}
	if readString(t, filepath.Join(root, "other.txt")) != "other\n" {
		t.Fatal("the restore wrote through the link")
	}
}

func writeInfo(t *testing.T, a *Agent, name string, meta backupMeta, files map[string]string) {
	t.Helper()
	dir := filepath.Join(a.backupDir(), name)
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(content), 0o644)
	}
	os.MkdirAll(dir, 0o700)
	b, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, ".info.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTamperedBackupRecord(t *testing.T) {
	a, _, root := newTestAgent(t)
	victim := filepath.Join(t.TempDir(), "victim.txt")
	writeInfo(t, a, "20260101-000000.000", backupMeta{Tool: "delete_file", Refs: []backupRef{{Rel: "../../escape.txt", Abs: victim}}}, nil)
	if _, err := a.RestoreBackup(1, true); err == nil {
		t.Fatal("a record with ../ was accepted")
	}

	// A stored absolute path is ignored: the target comes from rel.
	os.RemoveAll(a.backupDir())
	writeInfo(t, a, "20260101-000000.000", backupMeta{Tool: "delete_file", Refs: []backupRef{{Rel: "f.txt", Area: "own", Abs: victim}}}, map[string]string{"f.txt": "x\n"})
	if _, err := a.RestoreBackup(1, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(victim); err == nil {
		t.Fatal("the restore wrote to the stored absolute path")
	}
	if readString(t, filepath.Join(root, "f.txt")) != "x\n" {
		t.Fatal("the file should be restored inside the workspace")
	}
}

func TestLegacyBackupRecordRestores(t *testing.T) {
	a, _, root := newTestAgent(t)
	writeInfo(t, a, "20260101-000000.000", backupMeta{Tool: "delete_file", Refs: []backupRef{{Rel: filepath.Join("notes", "old.txt"), Abs: "/somewhere/else"}}},
		map[string]string{filepath.Join("notes", "old.txt"): "legacy\n"})
	if _, err := a.RestoreBackup(1, false); err != nil {
		t.Fatal(err)
	}
	if readString(t, filepath.Join(root, "notes", "old.txt")) != "legacy\n" {
		t.Fatal("a backup made before areas should restore into the agent's directory")
	}
}

func TestSharedBackupNeedsWriteAccess(t *testing.T) {
	share := t.TempDir()
	os.MkdirAll(filepath.Join(share, "other"), 0o755)
	os.WriteFile(filepath.Join(share, "other", "n.md"), []byte("theirs\n"), 0o644)
	ws, err := tools.OpenWorkspace(tools.WorkspaceOptions{Share: share, AgentDir: "me", Shared: tools.AccessWrite})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	a := newAgentFor(t, ws)
	if _, err := a.backupTargets("delete_file", []tools.BackupRef{refFor(t, ws, "../other/n.md")}); err != nil {
		t.Fatal(err)
	}
	if e := a.listBackups()[0]; e.Refs[0].Area != areaShared || !strings.HasPrefix(filepath.ToSlash(e.Refs[0].Rel), "_shared/") {
		t.Fatalf("shared backup recorded as %+v", e.Refs[0])
	}
	os.Remove(filepath.Join(share, "other", "n.md"))

	ws.Access = tools.AccessRead
	if _, err := a.RestoreBackup(1, false); err == nil {
		t.Fatal("restoring into the share with read access was accepted")
	}
	ws.Access = tools.AccessWrite
	if _, err := a.RestoreBackup(1, false); err != nil {
		t.Fatal(err)
	}
	if readString(t, filepath.Join(share, "other", "n.md")) != "theirs\n" {
		t.Fatal("the shared file was not restored")
	}
}
