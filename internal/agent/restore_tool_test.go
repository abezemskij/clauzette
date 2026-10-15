package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clauzette/internal/tools"
)

// TestRestoreBackupTool exercises the rogue-only restore_backup tool: it is
// refused outside rogue mode, restores the most recent backup, lists backups
// for an out-of-range number, and needs force when a path exists again.
func TestRestoreBackupTool(t *testing.T) {
	a, ws, root := newTestAgent(t)
	src := filepath.Join(root, "a.txt")
	if err := os.WriteFile(src, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.backupTargets("delete_file", []tools.BackupRef{refFor(t, ws, "a.txt")}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}

	// Outside rogue mode the tool refuses.
	if out, isErr := a.runRestoreForModel(map[string]any{}); !isErr {
		t.Fatalf("expected refusal outside rogue, got %q", out)
	}

	if _, err := a.SetRogue(true, 0); err != nil {
		t.Fatal(err)
	}
	a.startRogueTurn()

	// Default restores the most recent backup.
	out, isErr := a.runRestoreForModel(map[string]any{})
	if isErr {
		t.Fatalf("restore failed: %s", out)
	}
	if b, err := os.ReadFile(src); err != nil || string(b) != "keep\n" {
		t.Fatalf("file not restored: %q %v", b, err)
	}

	// An out-of-range number lists the available backups.
	out, isErr = a.runRestoreForModel(map[string]any{"n": 9.0})
	if !isErr || !strings.Contains(out, "Available backups") {
		t.Fatalf("expected a listing for a bad number, got %q", out)
	}

	// The file exists again, so a plain restore is refused and asks for force.
	out, isErr = a.runRestoreForModel(map[string]any{})
	if !isErr || !strings.Contains(strings.ToLower(out), "force") {
		t.Fatalf("expected conflict-needs-force, got %q", out)
	}

	// With force it replaces the current file (backing it up first).
	os.WriteFile(src, []byte("changed\n"), 0o644)
	out, isErr = a.runRestoreForModel(map[string]any{"force": true})
	if isErr {
		t.Fatalf("forced restore failed: %s", out)
	}
	if b, err := os.ReadFile(src); err != nil || string(b) != "keep\n" {
		t.Fatalf("forced restore did not put the backup back: %q %v", b, err)
	}
}

// TestRestoreBackupToolNoBackups reports cleanly when there is nothing to restore.
func TestRestoreBackupToolNoBackups(t *testing.T) {
	a, _, _ := newTestAgent(t)
	if _, err := a.SetRogue(true, 0); err != nil {
		t.Fatal(err)
	}
	a.startRogueTurn()
	out, isErr := a.runRestoreForModel(map[string]any{})
	if isErr || !strings.Contains(out, "no backups") {
		t.Fatalf("expected a no-backups message, got %q (isErr=%v)", out, isErr)
	}
}
