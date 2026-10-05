package agent

// Safe mode: when the operator enables !safe, destructive actions
// (mv_file, delete_file) copy their target to
// <sessions_dir>/backups/<session id>/<timestamp>/ before they run.
// Each backup is one directory with a .info.json describing its content;
// /safe lists them and /safe restore <n> puts them back where they came
// from. Backups live with the sessions, on the state volume in the pod,
// so they are outside the shared workspace and never pollute it.

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"clauzette/internal/textutil"
	"clauzette/internal/tools"
)

type backupRef struct {
	Rel string `json:"rel"`
	Abs string `json:"abs"`
}

type backupMeta struct {
	Tool  string      `json:"tool"`
	Refs  []backupRef `json:"refs"`
	Files int         `json:"files"`
	Bytes int64       `json:"bytes"`
	Time  time.Time   `json:"time"`
}

func (a *Agent) backupDir() string {
	return filepath.Join(a.Store.Dir, "backups", a.sess.ID)
}

// backupTargets copies each target (a file or a whole directory tree) into
// a new timestamped directory and returns its path. On failure nothing is
// left behind, and the caller must not run the action.
func (a *Agent) backupTargets(tool string, refs []tools.BackupRef) (string, error) {
	dir := a.backupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	base := time.Now().Format("20060102-150405.000")
	name := base
	for i := 1; ; i++ {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			break
		}
		name = base + "-" + strconv.Itoa(i)
	}
	cand := filepath.Join(dir, name)
	if err := os.Mkdir(cand, 0o700); err != nil {
		return "", err
	}
	meta := backupMeta{Tool: tool, Time: time.Now()}
	var failed error
	for _, r := range refs {
		files, bytes, err := copyTreePreserving(r.Abs, filepath.Join(cand, r.Rel))
		if err != nil {
			failed = err
			break
		}
		meta.Refs = append(meta.Refs, backupRef{Rel: r.Rel, Abs: r.Abs})
		meta.Files += files
		meta.Bytes += bytes
	}
	if failed == nil {
		b, _ := json.Marshal(meta)
		failed = os.WriteFile(filepath.Join(cand, ".info.json"), b, 0o600)
	}
	if failed != nil {
		os.RemoveAll(cand)
		return "", failed
	}
	a.pruneBackups()
	return cand, nil
}

// copyTreePreserving copies a file or a directory tree and returns the
// number of files and bytes copied.
func copyTreePreserving(src, dst string) (int, int64, error) {
	fi, err := os.Stat(src)
	if err != nil {
		return 0, 0, err
	}
	if !fi.IsDir() {
		b, err := os.ReadFile(src)
		if err != nil {
			return 0, 0, err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return 0, 0, err
		}
		if err := os.WriteFile(dst, b, fi.Mode().Perm()); err != nil {
			return 0, 0, err
		}
		return 1, int64(len(b)), nil
	}
	files, bytes := 0, int64(0)
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel != "." {
				if err := os.MkdirAll(filepath.Join(dst, rel), 0o755); err != nil {
					return err
				}
			}
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dinfo, err := d.Info()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dst, rel)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, rel), b, dinfo.Mode().Perm()); err != nil {
			return err
		}
		files++
		bytes += int64(len(b))
		return nil
	})
	return files, bytes, err
}

// pruneBackups keeps only the newest safe.max_backups backups of this session.
func (a *Agent) pruneBackups() {
	max := a.Cfg.Safe.MaxBackups
	if max < 1 {
		max = 1
	}
	entries, err := os.ReadDir(a.backupDir())
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // timestamp first: oldest first
	keep := len(names) - max
	if keep < 0 {
		keep = 0
	}
	for _, n := range names[:keep] {
		os.RemoveAll(filepath.Join(a.backupDir(), n))
	}
}

type backupEntry struct {
	Name string
	backupMeta
}

// listBackups returns this session's backups, newest first.
func (a *Agent) listBackups() []backupEntry {
	entries, err := os.ReadDir(a.backupDir())
	if err != nil {
		return nil
	}
	var out []backupEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var meta backupMeta
		if b, rerr := os.ReadFile(filepath.Join(a.backupDir(), e.Name(), ".info.json")); rerr == nil {
			_ = json.Unmarshal(b, &meta)
		}
		out = append(out, backupEntry{Name: e.Name(), backupMeta: meta})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out
}

// SafeReport describes safe mode and this session's backups. The numbers
// match /safe restore <n>.
func (a *Agent) SafeReport() string {
	var sb strings.Builder
	state := "off"
	if a.Safe() {
		state = "on"
	}
	fmt.Fprintf(&sb, "safe mode: %s  (toggle with !safe [on|off])\n", state)
	sb.WriteString("When on, mv_file and delete_file back up their target before running and each needs a fresh 'y';\nexec_command is not backed up (a note is shown with its approval).\n")
	entries := a.listBackups()
	if len(entries) == 0 {
		sb.WriteString("No backups yet for this session.")
		return sb.String()
	}
	for i, e := range entries {
		rel := strings.Join(metaRefs(e.Refs), ", ")
		if e.Bytes > 0 {
			rel += fmt.Sprintf("  (%d file%s, %s)", e.Files, pluralFiles(e.Files), textutil.HumanBytes(e.Bytes))
		}
		if e.Time.IsZero() {
			e.Time = time.Now()
		}
		fmt.Fprintf(&sb, "%2d  %s  %-12s %s\n", len(entries)-i, e.Time.Format("2006-01-02 15:04:05"), e.Tool, rel)
	}
	sb.WriteString("Restore one with: /safe restore <number>")
	return sb.String()
}

// RestoreBackup puts the numbered backup (1 = newest) back where it came from.
// For a delete_file that means the file or tree is recreated; for an
// mv_file the source comes back at its old path (the destination stays).
func (a *Agent) RestoreBackup(n int) (string, error) {
	entries := a.listBackups()
	if n < 1 || n > len(entries) {
		return "", fmt.Errorf("no backup %d; this session has %d (see /safe)", n, len(entries))
	}
	e := entries[n-1]
	if len(e.Refs) == 0 {
		return "", fmt.Errorf("backup %d has no record of what it contains", n)
	}
	srcRoot := filepath.Join(a.backupDir(), e.Name)
	for _, r := range e.Refs {
		abs := r.Abs
		if abs == "" && a.WS != nil {
			abs = filepath.Join(a.WS.Root, r.Rel)
		}
		if abs == "" {
			return "", fmt.Errorf("cannot determine the original location of %s", r.Rel)
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return "", err
		}
		if _, _, err := copyTreePreserving(filepath.Join(srcRoot, r.Rel), abs); err != nil {
			return "", fmt.Errorf("restoring %s: %v", r.Rel, err)
		}
		if a.WS != nil {
			markSeenTree(a.WS, abs)
		}
	}
	a.Audit.Log(map[string]any{
		"session": a.sess.ID,
		"event":   "safe_restore",
		"backup":  e.Name,
		"tool":    e.Tool,
		"refs":    e.Refs,
	})
	suffix := ""
	if e.Files != 1 {
		suffix = "s"
	}
	return fmt.Sprintf("restored backup %d (%s, %d file%s, %s) to its original location; the model was not told, it will re-read if needed",
		n, e.Time.Format("2006-01-02 15:04:05"), e.Files, suffix, textutil.HumanBytes(e.Bytes)), nil
}

func metaRefs(refs []backupRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Rel)
	}
	return out
}

// markSeenTree records restored files as read, so the agent may overwrite
// or edit them without re-reading.
func markSeenTree(ws *tools.Workspace, root string) {
	fi, err := os.Stat(root)
	if err != nil {
		return
	}
	if !fi.IsDir() {
		b, err := os.ReadFile(root)
		if err == nil {
			ws.MarkSeen(root, b)
		}
		return
	}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d != nil && !d.IsDir() {
			if b, rerr := os.ReadFile(p); rerr == nil {
				ws.MarkSeen(p, b)
			}
		}
		return nil
	})
}

func pluralFiles(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
