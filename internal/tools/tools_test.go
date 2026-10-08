package tools

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiffUnchanged(t *testing.T) {
	d, a, r := Diff("f", "a\nb\n", "a\nb\n", 3)
	if d != "" || a != 0 || r != 0 {
		t.Fatalf("expected no diff, got %q (+%d -%d)", d, a, r)
	}
}

func TestDiffReplaceLine(t *testing.T) {
	d, a, r := Diff("f.txt", "one\ntwo\nthree\n", "one\n2\nthree\n", 3)
	want := "--- a/f.txt\n+++ b/f.txt\n@@ -1,3 +1,3 @@\n one\n-two\n+2\n three\n"
	if d != want {
		t.Fatalf("diff mismatch:\n got: %q\nwant: %q", d, want)
	}
	if a != 1 || r != 1 {
		t.Fatalf("stats: got +%d -%d, want +1 -1", a, r)
	}
}

func TestDiffNewFile(t *testing.T) {
	d, a, r := Diff("n", "", "x\ny\n", 3)
	want := "--- a/n\n+++ b/n\n@@ -0,0 +1,2 @@\n+x\n+y\n"
	if d != want || a != 2 || r != 0 {
		t.Fatalf("got %q (+%d -%d)", d, a, r)
	}
}

func TestDiffSeparateHunks(t *testing.T) {
	var a, b []string
	for i := 0; i < 30; i++ {
		a = append(a, "line")
		b = append(b, "line")
	}
	a[2], b[2] = "old-top", "new-top"
	a[27], b[27] = "old-bottom", "new-bottom"
	d, _, _ := Diff("f", strings.Join(a, "\n")+"\n", strings.Join(b, "\n")+"\n", 3)
	if got := strings.Count(d, "@@ -"); got != 2 {
		t.Fatalf("expected 2 hunks, got %d:\n%s", got, d)
	}
}

func TestResolveConfinement(t *testing.T) {
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Resolve("a/new/file.txt", AccessWrite); err != nil {
		t.Fatalf("new file inside workspace rejected: %v", err)
	}
	if _, err := ws.Resolve("../outside", AccessRead); err == nil {
		t.Fatal("../outside was accepted")
	}
	if _, err := ws.Resolve("/etc/passwd", AccessRead); err == nil {
		t.Fatal("/etc/passwd was accepted")
	}
	if err := os.Symlink("/etc", filepath.Join(root, "link")); err != nil {
		t.Skip("cannot create symlink:", err)
	}
	// Resolve does not touch the disk; the escape is refused by the read itself.
	read := &ReadFile{WS: ws, DefaultLines: 10}
	act, err := read.Prepare(context.Background(), Args{"path": "link/passwd"})
	if err == nil {
		if _, err = act.Run(context.Background()); err == nil {
			t.Fatal("symlink escape was read")
		}
	}
}

func TestEditFileFlow(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root)
	path := filepath.Join(root, "main.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edit := &EditFile{WS: ws}
	ctx := context.Background()

	if _, err := edit.Prepare(ctx, Args{"path": "main.txt", "old_text": "beta", "new_text": "BETA"}); err == nil {
		t.Fatal("ambiguous edit was accepted")
	}
	if _, err := edit.Prepare(ctx, Args{"path": "main.txt", "old_text": "delta", "new_text": "x"}); err == nil {
		t.Fatal("edit with missing text was accepted")
	}
	act, err := edit.Prepare(ctx, Args{"path": "main.txt", "old_text": "gamma\nbeta", "new_text": "gamma\nBETA"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := act.Run(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "alpha\nbeta\ngamma\nBETA\n" {
		t.Fatalf("unexpected content %q", b)
	}
}

func TestWriteFileRequiresRead(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root)
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	write := &WriteFile{WS: ws}
	ctx := context.Background()
	if _, err := write.Prepare(ctx, Args{"path": "x.txt", "content": "v2\n"}); err == nil {
		t.Fatal("overwrite without reading was accepted")
	}
	read := &ReadFile{WS: ws, DefaultLines: 100}
	act, err := read.Prepare(ctx, Args{"path": "x.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := act.Run(ctx); err != nil {
		t.Fatal(err)
	}
	act, err = write.Prepare(ctx, Args{"path": "x.txt", "content": "v2\n"})
	if err != nil {
		t.Fatalf("overwrite after reading rejected: %v", err)
	}
	if _, err := act.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestMvFileFlow(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root)
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"src/a.txt", "src/b.txt"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(p)), []byte("v\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mv := &MvFile{WS: ws}
	ctx := context.Background()

	if _, err := mv.Prepare(ctx, Args{"from": "nope.txt", "to": "y.txt"}); err == nil {
		t.Fatal("missing source was accepted")
	}
	if _, err := mv.Prepare(ctx, Args{"from": "src/a.txt", "to": "src/b.txt"}); err == nil {
		t.Fatal("move onto an existing file was accepted")
	}
	if _, err := mv.Prepare(ctx, Args{"from": "src/a.txt", "to": "src"}); err == nil {
		t.Fatal("move into a non-empty directory was accepted")
	}
	act, err := mv.Prepare(ctx, Args{"from": "src/a.txt", "to": "moved/a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(act.Backups) != 1 || act.Backups[0].Rel != "src/a.txt" {
		t.Fatalf("expected one backup ref for the source, got %#v", act.Backups)
	}
	out, err := act.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Moved") {
		t.Fatalf("unexpected result %q", out)
	}
	if _, err := os.Stat(filepath.Join(root, "src/a.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("source still exists after move")
	}
	b, err := os.ReadFile(filepath.Join(root, "moved/a.txt"))
	if err != nil || string(b) != "v\n" {
		t.Fatalf("moved content wrong: %q %v", b, err)
	}
}

func TestDeleteFileFlow(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root)
	if err := os.MkdirAll(filepath.Join(root, "pkg/sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"x.txt", "pkg/a.txt", "pkg/sub/b.txt"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(p)), []byte("v\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	del := &DeleteFile{WS: ws}
	ctx := context.Background()

	if _, err := del.Prepare(ctx, Args{"path": "."}); err == nil {
		t.Fatal("deleting the workspace root was accepted")
	}
	if _, err := del.Prepare(ctx, Args{"path": "pkg"}); err == nil {
		t.Fatal("deleting a non-empty directory without recursive was accepted")
	}
	if act, err := del.Prepare(ctx, Args{"path": "pkg", "recursive": "true"}); err != nil {
		t.Fatal(err)
	} else {
		if len(act.Backups) != 1 || act.Backups[0].Rel != "pkg" {
			t.Fatalf("expected one backup ref for the directory, got %#v", act.Backups)
		}
		if _, err := act.Run(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, "pkg")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("directory still exists after recursive delete")
		}
	}
	act, err := del.Prepare(ctx, Args{"path": "x.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := act.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "x.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("file still exists after delete")
	}
}

func TestCappedBuffer(t *testing.T) {
	b := newCappedBuffer(1000)
	data := strings.Repeat("a", 500) + strings.Repeat("m", 2000) + strings.Repeat("z", 500)
	if _, err := b.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	s := b.String()
	if !strings.HasPrefix(s, strings.Repeat("a", 500)) || !strings.HasSuffix(s, strings.Repeat("z", 500)) {
		t.Fatalf("head/tail not kept: %q…", s[:40])
	}
	if !strings.Contains(s, "2000 bytes omitted") {
		t.Fatalf("missing omission note in %q", s[480:560])
	}
}

func TestExecStoppedByDeadline(t *testing.T) {
	ws, _ := NewWorkspace(t.TempDir())
	ex := &ExecCommand{WS: ws, Shell: "/bin/sh", DefaultTimeout: time.Minute, MaxTimeout: time.Minute, MaxOutput: 1000}
	act, err := ex.Prepare(context.Background(), Args{"command": "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	out, _ := act.Run(ctx)
	if !strings.Contains(out, "time limit") {
		t.Fatalf("a command stopped by a deadline should say so: %q", out)
	}
}
