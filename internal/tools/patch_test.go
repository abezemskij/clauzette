package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// edit builds one entry of the apply_patch "edits" array.
func edit(path, oldText, newText string) map[string]any {
	return map[string]any{"path": path, "old_text": oldText, "new_text": newText}
}

func patchArgs(edits ...map[string]any) Args {
	list := make([]any, len(edits))
	for i, e := range edits {
		list[i] = e
	}
	return Args{"edits": list}
}

func TestApplyPatchMultiFile(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root)
	write := func(p, s string) {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(p)), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "hello\nworld\n")
	write("b.txt", "foo\nbar\n")

	ap := &ApplyPatch{WS: ws}
	ctx := context.Background()
	act, err := ap.Prepare(ctx, patchArgs(
		edit("a.txt", "world", "WORLD"),
		edit("b.txt", "bar", "BAR"),
		edit("c.txt", "", "brand new\n"), // create
	))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(act.Summary, "3 file") {
		t.Fatalf("summary = %q", act.Summary)
	}
	if _, err := act.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{
		"a.txt": "hello\nWORLD\n",
		"b.txt": "foo\nBAR\n",
		"c.txt": "brand new\n",
	} {
		b, _ := os.ReadFile(filepath.Join(root, p))
		if string(b) != want {
			t.Errorf("%s = %q, want %q", p, b, want)
		}
	}
}

func TestApplyPatchSequentialSameFile(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root)
	os.WriteFile(filepath.Join(root, "f.txt"), []byte("one\ntwo\n"), 0o644)

	ap := &ApplyPatch{WS: ws}
	ctx := context.Background()
	// Second edit's old_text only exists after the first edit runs.
	act, err := ap.Prepare(ctx, patchArgs(
		edit("f.txt", "one", "ONE"),
		edit("f.txt", "ONE\ntwo", "ONE\nTWO"),
	))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := act.Run(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "f.txt"))
	if string(b) != "ONE\nTWO\n" {
		t.Fatalf("content = %q", b)
	}
}

func TestApplyPatchAtomicOnFailure(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root)
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("keep me\n"), 0o644)

	ap := &ApplyPatch{WS: ws}
	// First edit is valid, second does not match: the whole batch must fail and
	// a.txt must be untouched, c.txt must not be created.
	_, err := ap.Prepare(context.Background(), patchArgs(
		edit("a.txt", "keep me", "changed"),
		edit("a.txt", "nonexistent", "x"),
	))
	if err == nil {
		t.Fatal("batch with a non-matching edit was accepted")
	}
	b, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(b) != "keep me\n" {
		t.Fatalf("a.txt was modified despite a failed batch: %q", b)
	}
}

func TestApplyPatchCreateExisting(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root)
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("here\n"), 0o644)

	ap := &ApplyPatch{WS: ws}
	if _, err := ap.Prepare(context.Background(), patchArgs(edit("a.txt", "", "new\n"))); err == nil {
		t.Fatal("creating over an existing file was accepted")
	}
}

func TestApplyPatchHashChanged(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root)
	path := filepath.Join(root, "a.txt")
	os.WriteFile(path, []byte("v1\n"), 0o644)

	ap := &ApplyPatch{WS: ws}
	ctx := context.Background()
	act, err := ap.Prepare(ctx, patchArgs(edit("a.txt", "v1", "v2")))
	if err != nil {
		t.Fatal(err)
	}
	// Someone edits the file between Prepare and Run.
	os.WriteFile(path, []byte("surprise\n"), 0o644)
	if _, err := act.Run(ctx); err == nil {
		t.Fatal("Run succeeded despite the file changing on disk")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "surprise\n" {
		t.Fatalf("file was overwritten: %q", b)
	}
}

func TestApplyPatchCRLF(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root)
	path := filepath.Join(root, "a.txt")
	os.WriteFile(path, []byte("one\r\ntwo\r\n"), 0o644)

	ap := &ApplyPatch{WS: ws}
	ctx := context.Background()
	act, err := ap.Prepare(ctx, patchArgs(edit("a.txt", "two", "TWO")))
	if err != nil {
		t.Fatalf("CRLF file edit rejected: %v", err)
	}
	if _, err := act.Run(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "one\r\nTWO\r\n" {
		t.Fatalf("content = %q", b)
	}
}

func TestApplyPatchStringifiedArray(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root)
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\n"), 0o644)

	ap := &ApplyPatch{WS: ws}
	ctx := context.Background()
	// The whole edits array arrives as a JSON-encoded string.
	act, err := ap.Prepare(ctx, Args{
		"edits": `[{"path":"a.txt","old_text":"hello","new_text":"HELLO"}]`,
	})
	if err != nil {
		t.Fatalf("stringified edits array rejected: %v", err)
	}
	if _, err := act.Run(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(b) != "HELLO\n" {
		t.Fatalf("content = %q", b)
	}
	// A string that is not a JSON array is a clear error, not a panic.
	if _, err := ap.Prepare(ctx, Args{"edits": "not json"}); err == nil {
		t.Fatal("non-JSON edits string was accepted")
	}
}

func TestApplyPatchEmptyAndConfinement(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root)
	ap := &ApplyPatch{WS: ws}
	ctx := context.Background()

	if _, err := ap.Prepare(ctx, Args{"edits": []any{}}); err == nil {
		t.Fatal("empty edits was accepted")
	}
	if _, err := ap.Prepare(ctx, patchArgs(edit("../escape.txt", "", "x\n"))); err == nil {
		t.Fatal("path escaping the workspace was accepted")
	}
	// An edit whose new_text equals old_text is rejected (nothing to change).
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("same\n"), 0o644)
	if _, err := ap.Prepare(ctx, patchArgs(edit("a.txt", "same", "same"))); err == nil {
		t.Fatal("identical old_text/new_text was accepted")
	}
}

func TestApplyPatchCreatesEmptyFile(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root)
	ap := &ApplyPatch{WS: ws}
	act, err := ap.Prepare(context.Background(), patchArgs(edit("empty.txt", "", "")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := act.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "empty.txt"))
	if err != nil {
		t.Fatalf("empty file was not created: %v", err)
	}
	if len(b) != 0 {
		t.Fatalf("expected an empty file, got %q", b)
	}
}
