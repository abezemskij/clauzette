package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormatVersion(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{ID: "new"}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load("new")
	if err != nil {
		t.Fatal(err)
	}
	if got.FormatVersion != FormatVersion {
		t.Fatalf("format_version = %d, want %d", got.FormatVersion, FormatVersion)
	}
}

func TestOldSessionLoads(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(dir)
	// Format 1: no format_version, a per-session chars_per_token.
	old := `{"id": "old", "model": "qwen3.8:27b", "system": "S", "chars_per_token": 3.3,
		"context": [{"role": "tool", "content": "ok", "tool_name": "read_file", "time": "2026-10-01T10:00:00Z"}]}`
	os.WriteFile(filepath.Join(dir, "old.json"), []byte(old), 0o600)
	s, err := st.Load("old")
	if err != nil {
		t.Fatal(err)
	}
	if s.FormatVersion != 1 || s.Profile != "" || s.Context[0].ToolCallID != "" {
		t.Fatalf("old session: %+v", s)
	}
}

func TestNewerFormatRejected(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(dir)
	os.WriteFile(filepath.Join(dir, "future.json"), []byte(`{"id": "future", "format_version": 99}`), 0o600)
	if _, err := st.Load("future"); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("a newer format was accepted: %v", err)
	}
}

func TestDelete(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keep, drop := &Session{ID: "keep"}, &Session{ID: "drop"}
	if err := st.Save(keep); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(drop); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete("drop"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load("drop"); err == nil {
		t.Fatal("the deleted session still loads")
	}
	if _, err := st.Load("keep"); err != nil {
		t.Fatalf("delete removed the wrong session: %v", err)
	}
	list, _ := st.List()
	if len(list) != 1 || list[0].ID != "keep" {
		t.Fatalf("after delete, /sessions shows %+v", list)
	}
	// Deleting a session that is already gone is not an error (idempotent).
	if err := st.Delete("drop"); err != nil {
		t.Fatalf("deleting a missing session errored: %v", err)
	}
	// An id with a path separator is refused, so /clear cannot touch other files.
	if err := st.Delete("../secret"); err == nil {
		t.Fatal("a path-like id was accepted")
	}
	if got := st.BackupDir("keep"); got == "" || !strings.HasSuffix(got, "backups/keep") {
		t.Fatalf("BackupDir = %q", got)
	}
}
