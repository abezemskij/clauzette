package tools

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sharedWS builds /share with the agent directory "me" and another agent's
// directory "other" holding other/notes.md.
func sharedWS(t *testing.T, access Access) (*Workspace, string) {
	t.Helper()
	share := t.TempDir()
	if err := os.MkdirAll(filepath.Join(share, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share, "other", "notes.md"), []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := OpenWorkspace(WorkspaceOptions{Share: share, AgentDir: "me", Shared: access})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws, share
}

func run(t *testing.T, tool Tool, args Args) (string, error) {
	t.Helper()
	act, err := tool.Prepare(context.Background(), args)
	if err != nil {
		return "", err
	}
	return act.Run(context.Background())
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skip("cannot create symlink:", err)
	}
}

func TestAgentDirCreatedAndRelativePathsLandThere(t *testing.T) {
	ws, share := sharedWS(t, AccessRead)
	if ws.Root != filepath.Join(share, "me") {
		t.Fatalf("agent directory is %s", ws.Root)
	}
	if st, err := os.Stat(ws.Root); err != nil || !st.IsDir() {
		t.Fatal("the agent directory was not created")
	}
	if _, err := run(t, &WriteFile{WS: ws}, Args{"path": "a/b.txt", "content": "x\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(share, "me", "a", "b.txt")); err != nil {
		t.Fatal("a relative path should land in the agent directory")
	}
}

func TestInvalidAgentDir(t *testing.T) {
	for _, name := range []string{"../x", "a/b", ".hidden", "sp ace", ""} {
		if name == "" {
			continue // empty means no agent directory
		}
		if _, err := OpenWorkspace(WorkspaceOptions{Share: t.TempDir(), AgentDir: name}); err == nil {
			t.Errorf("agent directory %q was accepted", name)
		}
	}
}

func TestSharedAccessLevels(t *testing.T) {
	read := func(ws *Workspace) error {
		_, err := run(t, &ReadFile{WS: ws, DefaultLines: 10}, Args{"path": "../other/notes.md"})
		return err
	}
	write := func(ws *Workspace) error {
		_, err := run(t, &WriteFile{WS: ws}, Args{"path": "../other/new.md", "content": "mine\n"})
		return err
	}
	list := func(ws *Workspace) error {
		_, err := run(t, &ListDir{WS: ws}, Args{"path": ".."})
		return err
	}

	ws, _ := sharedWS(t, AccessNone)
	if read(ws) == nil || list(ws) == nil || write(ws) == nil {
		t.Fatal("access none should refuse everything outside the agent directory")
	}

	ws, share := sharedWS(t, AccessRead)
	if err := read(ws); err != nil {
		t.Fatalf("access read should allow reading: %v", err)
	}
	if err := list(ws); err != nil {
		t.Fatalf("access read should allow listing: %v", err)
	}
	if err := write(ws); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("access read should refuse writing, got %v", err)
	}
	if _, err := run(t, &DeleteFile{WS: ws}, Args{"path": "../other/notes.md"}); err == nil {
		t.Fatal("access read should refuse deleting")
	}
	if _, err := os.Stat(filepath.Join(share, "other", "notes.md")); err != nil {
		t.Fatal("the other agent's file is gone")
	}

	ws, share = sharedWS(t, AccessWrite)
	if err := write(ws); err != nil {
		t.Fatalf("access write should allow writing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(share, "other", "new.md")); err != nil {
		t.Fatal("the shared write did not happen")
	}
	// An absolute path into the share works too.
	if _, err := run(t, &ReadFile{WS: ws, DefaultLines: 10}, Args{"path": filepath.Join(share, "other", "notes.md")}); err != nil {
		t.Fatalf("absolute path into the share refused: %v", err)
	}
	if _, err := ws.Resolve(filepath.Dir(share), AccessRead); err == nil {
		t.Fatal("a path above the share was accepted")
	}
}

func TestAgentDirCannotBeDeletedOrMoved(t *testing.T) {
	ws, _ := sharedWS(t, AccessWrite)
	for _, p := range []string{".", "../me", ".."} {
		if _, err := (&DeleteFile{WS: ws}).Prepare(context.Background(), Args{"path": p, "recursive": true}); err == nil {
			t.Errorf("deleting %q was accepted", p)
		}
	}
	if _, err := (&MvFile{WS: ws}).Prepare(context.Background(), Args{"from": "../me", "to": "../me2"}); err == nil {
		t.Error("moving the agent directory was accepted")
	}
}

func TestMoveBetweenAgentDirAndShare(t *testing.T) {
	ws, share := sharedWS(t, AccessWrite)
	if err := os.WriteFile(filepath.Join(ws.Root, "report.md"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, &MvFile{WS: ws}, Args{"from": "report.md", "to": "../other/report.md"}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(share, "other", "report.md")); err != nil || string(b) != "done\n" {
		t.Fatalf("move into the share failed: %q %v", b, err)
	}
	if _, err := run(t, &MvFile{WS: ws}, Args{"from": "../other/notes.md", "to": "notes.md"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws.Root, "notes.md")); err != nil {
		t.Fatal("move from the share failed")
	}
}

func TestBackupRelStaysInsideTheBackup(t *testing.T) {
	ws, _ := sharedWS(t, AccessWrite)
	act, err := (&DeleteFile{WS: ws}).Prepare(context.Background(), Args{"path": "../other/notes.md"})
	if err != nil {
		t.Fatal(err)
	}
	if got := act.Backups[0].Rel; got != filepath.Join("_shared", "other", "notes.md") {
		t.Fatalf("backup name for a shared file is %q", got)
	}
}

func TestDeleteSymlinkDeletesTheLink(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root)
	os.WriteFile(filepath.Join(root, "target.txt"), []byte("keep\n"), 0o644)
	mustSymlink(t, "target.txt", filepath.Join(root, "link"))

	out, err := run(t, &DeleteFile{WS: ws}, Args{"path": "link"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "link")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the link is still there")
	}
	if b, err := os.ReadFile(filepath.Join(root, "target.txt")); err != nil || string(b) != "keep\n" {
		t.Fatalf("the target was touched: %q %v (%s)", b, err, out)
	}
}

func TestDeleteDanglingAndOutsideLinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("secret\n"), 0o644)
	ws, _ := NewWorkspace(root)
	mustSymlink(t, "missing.txt", filepath.Join(root, "dangling"))
	mustSymlink(t, outside, filepath.Join(root, "out"))

	act, err := (&DeleteFile{WS: ws}).Prepare(context.Background(), Args{"path": "out"})
	if err != nil {
		t.Fatalf("deleting a link to outside was refused: %v", err)
	}
	if !strings.Contains(act.Summary, "symlink") || len(act.Backups) != 0 {
		t.Fatalf("summary should name the link and nothing is backed up: %q %v", act.Summary, act.Backups)
	}
	if _, err := act.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, &DeleteFile{WS: ws}, Args{"path": "dangling"}); err != nil {
		t.Fatalf("deleting a dangling link failed: %v", err)
	}
	for _, l := range []string{"out", "dangling"} {
		if _, err := os.Lstat(filepath.Join(root, l)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s is still there", l)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("the file outside the workspace was touched")
	}
}

func TestReadThroughOutsideLinkRefused(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("secret\n"), 0o644)
	ws, _ := NewWorkspace(root)
	mustSymlink(t, outside, filepath.Join(root, "out"))
	mustSymlink(t, "../"+filepath.Base(filepath.Dir(outside)), filepath.Join(root, "rel"))

	for _, p := range []string{"out", "rel/secret.txt"} {
		if out, err := run(t, &ReadFile{WS: ws, DefaultLines: 10}, Args{"path": p}); err == nil || strings.Contains(out, "secret") {
			t.Fatalf("reading %s through a link to outside was allowed: %q", p, out)
		}
	}
	if _, err := run(t, &WriteFile{WS: ws}, Args{"path": "out", "content": "pwned\n"}); err == nil {
		t.Fatal("writing through a link to outside was allowed")
	}
	if b, _ := os.ReadFile(outside); string(b) != "secret\n" {
		t.Fatal("the file outside was changed")
	}
}

func TestMoveSymlinkMovesTheLink(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root)
	os.WriteFile(filepath.Join(root, "target.txt"), []byte("keep\n"), 0o644)
	mustSymlink(t, "target.txt", filepath.Join(root, "link"))

	if _, err := run(t, &MvFile{WS: ws}, Args{"from": "link", "to": "link2"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(root, "link2")); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Fatal("link2 should be the moved symlink")
	}
	if _, err := os.Stat(filepath.Join(root, "target.txt")); err != nil {
		t.Fatal("the target should stay where it was")
	}
}

func TestWriteThroughRelativeLinkEditsTarget(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root)
	os.MkdirAll(filepath.Join(root, "conf"), 0o755)
	os.WriteFile(filepath.Join(root, "conf", "real.yaml"), []byte("a: 1\n"), 0o644)
	mustSymlink(t, "conf/real.yaml", filepath.Join(root, "app.yaml"))

	if _, err := run(t, &ReadFile{WS: ws, DefaultLines: 10}, Args{"path": "app.yaml"}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, &WriteFile{WS: ws}, Args{"path": "app.yaml", "content": "a: 2\n"}); err != nil {
		t.Fatalf("overwrite through the link after reading it was refused: %v", err)
	}
	if fi, _ := os.Lstat(filepath.Join(root, "app.yaml")); fi.Mode()&fs.ModeSymlink == 0 {
		t.Fatal("the link was replaced by a regular file")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "conf", "real.yaml")); string(b) != "a: 2\n" {
		t.Fatalf("the target was not updated: %q", b)
	}
}

// A directory swapped for a symlink between approval and running must not
// lead the write outside.
func TestSwapAfterApprovalCannotEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	ws, _ := NewWorkspace(root)
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)

	act, err := (&WriteFile{WS: ws}).Prepare(context.Background(), Args{"path": "sub/f.txt", "content": "x\n"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "sub")); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, filepath.Join(root, "sub"))

	_, err = act.Run(context.Background())
	if err == nil {
		t.Fatal("the write went ahead through the swapped directory")
	}
	if !strings.Contains(err.Error(), "outside") {
		t.Fatalf("expected an escape error, got %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("something was written outside: %v", entries)
	}
}
