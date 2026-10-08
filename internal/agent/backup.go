package agent

// Safe mode: when the operator enables !safe, destructive actions
// (mv_file, delete_file) copy their target to
// <sessions_dir>/backups/<session id>/<timestamp>/ before they run.
// Each backup is one directory with a .info.json describing its content;
// /safe lists them and /safe restore <n> puts them back where they came
// from. Backups live with the sessions, on the state volume in the pod,
// so they are outside the shared workspace and never pollute it.
//
// Both directions go through the workspace's directory handles and never
// follow symlinks: a link is backed up and restored as a link. A restore
// takes its target from the area and relative path in .info.json, never
// from a stored absolute path, and needs the same access as a write there.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"clauzette/internal/textutil"
	"clauzette/internal/tools"
)

const (
	areaOwn    = "own"    // the agent's directory (the whole workspace without one)
	areaShared = "shared" // the shared workspace outside the agent's directory
	sharedPfx  = "_shared"
)

type backupRef struct {
	Rel     string   `json:"rel"`               // path inside the backup; for the shared area it starts with _shared/
	Area    string   `json:"area,omitempty"`    // own | shared; "" in backups made before areas, meaning own
	Abs     string   `json:"abs,omitempty"`     // original absolute path, for display only
	Skipped []string `json:"skipped,omitempty"` // entries not copied (sockets, devices, ...)
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
	dir, err := a.makeBackup(tool, refs)
	if err != nil {
		return "", err
	}
	a.pruneBackups()
	return dir, nil
}

// makeBackup is backupTargets without pruning, for a restore that must not
// prune the backup it is about to read.
func (a *Agent) makeBackup(tool string, refs []tools.BackupRef) (string, error) {
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
		if !r.Loc.Valid() || !filepath.IsLocal(r.Rel) {
			failed = fmt.Errorf("cannot back up %s: no valid location", r.Rel)
			break
		}
		files, bytes, skipped, err := copyToBackup(r.Loc, filepath.Join(cand, r.Rel))
		if err != nil {
			failed = err
			break
		}
		area := areaOwn
		if r.Loc.Shared {
			area = areaShared
		}
		meta.Refs = append(meta.Refs, backupRef{Rel: r.Rel, Area: area, Abs: r.Abs, Skipped: skipped})
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
	return cand, nil
}

// copyToBackup copies loc (a file, a symlink or a directory tree) to dst in
// the backup. It reads through the workspace handle and never follows a
// symlink: links are recorded as links. Other entry types are skipped and
// returned by name. It returns the number of files and bytes copied.
func copyToBackup(loc tools.Loc, dst string) (int, int64, []string, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, 0, nil, err
	}
	st, err := loc.Lstat()
	if err != nil {
		return 0, 0, nil, err
	}
	if !st.IsDir() {
		return copyEntry(loc, st.Mode(), dst)
	}
	files, bytes := 0, int64(0)
	var skipped []string
	err = fs.WalkDir(loc.FS(), loc.Name, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		target := filepath.Join(dst, filepath.FromSlash(relUnder(loc.Name, p)))
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		n, b, skip, err := copyEntry(loc.Child(p), info.Mode(), target)
		files, bytes, skipped = files+n, bytes+b, append(skipped, skip...)
		return err
	})
	return files, bytes, skipped, err
}

// copyEntry copies one non-directory entry.
func copyEntry(loc tools.Loc, mode fs.FileMode, dst string) (int, int64, []string, error) {
	switch {
	case mode&fs.ModeSymlink != 0:
		target, err := loc.Readlink()
		if err != nil {
			return 0, 0, nil, err
		}
		return 0, 0, nil, os.Symlink(target, dst)
	case mode.IsRegular():
		b, err := loc.ReadFile()
		if err != nil {
			return 0, 0, nil, err
		}
		if err := os.WriteFile(dst, b, mode.Perm()); err != nil {
			return 0, 0, nil, err
		}
		return 1, int64(len(b)), nil, nil
	}
	return 0, 0, []string{loc.Name}, nil
}

// relUnder returns name relative to dir, both slash-separated as passed to
// an fs.WalkDir callback; "." for dir itself.
func relUnder(dir, name string) string {
	if name == dir {
		return "."
	}
	if dir == "." {
		return name
	}
	return strings.TrimPrefix(name, dir+"/")
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
		switch ops, err := a.restorePlan(e); {
		case err != nil:
			rel += "  [cannot be restored: " + err.Error() + "]"
		case conflictCount(ops) > 0:
			rel += fmt.Sprintf("  [%d path%s exist now: restore needs force]", conflictCount(ops), pluralFiles(conflictCount(ops)))
		}
		fmt.Fprintf(&sb, "%2d  %s  %-12s %s\n", len(entries)-i, e.Time.Format("2006-01-02 15:04:05"), e.Tool, rel)
	}
	sb.WriteString("Restore one with: /safe restore <number> [force]  (force replaces what exists now, after backing it up)")
	return sb.String()
}

// conflictPolicy decides what a restore does when a path it would write
// already exists. It is the single place to change that behaviour: a new
// policy (restore alongside, replace the whole target, ...) is a new value
// handled in applyPolicy.
type conflictPolicy int

const (
	// conflictRefuse refuses the whole restore and lists the conflicts;
	// nothing is written.
	conflictRefuse conflictPolicy = iota
	// conflictOverwrite replaces each conflicting entry with the backup's
	// version and keeps everything else; the replaced entries are backed up
	// first, so the restore itself can be undone.
	conflictOverwrite
)

// restoreOp is one entry a restore writes.
type restoreOp struct {
	kind     string // "dir", "file" or "symlink"
	src      string // the entry in the backup
	link     string // symlink target, for kind symlink
	perm     fs.FileMode
	dst      tools.Loc // where it goes
	conflict bool      // something is at dst now that this op would have to replace
	replace  bool      // set by the policy: remove what is at dst first
}

func conflictCount(ops []restoreOp) int {
	n := 0
	for _, op := range ops {
		if op.conflict {
			n++
		}
	}
	return n
}

// RestoreBackup puts the numbered backup (1 = newest) back where it came
// from. For a delete_file that means the file or tree is recreated; for an
// mv_file the source comes back at its old path (the destination stays).
// Without force, a restore that would replace anything is refused; with
// force, conflicting entries are backed up and then replaced.
func (a *Agent) RestoreBackup(n int, force bool) (string, error) {
	entries := a.listBackups()
	if n < 1 || n > len(entries) {
		return "", fmt.Errorf("no backup %d; this session has %d (see /safe)", n, len(entries))
	}
	e := entries[n-1]
	ops, err := a.restorePlan(e)
	if err != nil {
		return "", fmt.Errorf("backup %d: %v", n, err)
	}
	policy := conflictRefuse
	if force {
		policy = conflictOverwrite
	}
	replaced, err := applyPolicy(ops, policy)
	if err != nil {
		return "", fmt.Errorf("backup %d: %v", n, err)
	}

	// Back up what will be replaced before touching anything. Pruning waits
	// until the end, so it cannot remove the backup being restored.
	preBackup := ""
	if len(replaced) > 0 {
		refs := make([]tools.BackupRef, 0, len(replaced))
		for _, l := range replaced {
			refs = append(refs, l.BackupRef())
		}
		if preBackup, err = a.makeBackup("safe_restore", refs); err != nil {
			return "", fmt.Errorf("could not back up what the restore would replace, so nothing was restored: %v", err)
		}
	}
	written, err := a.runRestore(ops)
	a.pruneBackups()
	rec := map[string]any{
		"session":  a.sess.ID,
		"event":    "safe_restore",
		"backup":   e.Name,
		"tool":     e.Tool,
		"refs":     e.Refs,
		"force":    force,
		"replaced": len(replaced),
		"written":  written,
	}
	if preBackup != "" {
		rec["pre_restore_backup"] = preBackup
	}
	if err != nil {
		rec["error"] = err.Error()
	}
	a.Audit.Log(rec)
	if err != nil {
		return "", fmt.Errorf("restoring backup %d stopped after %d entr%s: %v", n, written, pluralY(written), err)
	}
	suffix := ""
	if e.Files != 1 {
		suffix = "s"
	}
	msg := fmt.Sprintf("restored backup %d (%s, %d file%s, %s) to its original location; the model was not told, it will re-read if needed",
		n, e.Time.Format("2006-01-02 15:04:05"), e.Files, suffix, textutil.HumanBytes(e.Bytes))
	if len(replaced) > 0 {
		msg += fmt.Sprintf(". %d existing entr%s replaced; they were saved first as backup 1 (/safe restore 1 undoes this restore)", len(replaced), pluralY(len(replaced)))
	}
	return msg, nil
}

// restorePlan lists what restoring e would write, and where something
// already exists. It reads only the backup and checks the workspace; it
// writes nothing.
func (a *Agent) restorePlan(e backupEntry) ([]restoreOp, error) {
	if a.WS == nil {
		return nil, errors.New("no workspace")
	}
	if len(e.Refs) == 0 {
		return nil, errors.New("no record of what it contains")
	}
	srcRoot := filepath.Join(a.backupDir(), e.Name)
	var ops []restoreOp
	for _, r := range e.Refs {
		target, err := a.restoreTarget(r)
		if err != nil {
			return nil, err
		}
		src := filepath.Join(srcRoot, r.Rel)
		err = filepath.WalkDir(src, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			rel, err := filepath.Rel(src, p)
			if err != nil {
				return err
			}
			dst := target
			if rel != "." {
				dst = target.Child(path.Join(target.Name, filepath.ToSlash(rel)))
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			op := restoreOp{src: p, perm: info.Mode().Perm(), dst: dst}
			switch {
			case d.IsDir():
				op.kind = "dir"
			case info.Mode()&fs.ModeSymlink != 0:
				op.kind = "symlink"
				if op.link, err = os.Readlink(p); err != nil {
					return err
				}
			case info.Mode().IsRegular():
				op.kind = "file"
			default:
				return nil
			}
			cur, err := dst.Lstat()
			switch {
			case err == nil:
				// An existing directory where a directory goes is merged into, not replaced.
				op.conflict = !(op.kind == "dir" && cur.IsDir())
			case !errors.Is(err, fs.ErrNotExist):
				return fmt.Errorf("%s: %v", a.WS.Rel(dst.Abs), err)
			}
			ops = append(ops, op)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("reading %s: %v", r.Rel, err)
		}
	}
	return ops, nil
}

// restoreTarget turns a backup record into the location it is restored
// to: from its area and relative path, never from the stored absolute
// path, and with the access a write there needs.
func (a *Agent) restoreTarget(r backupRef) (tools.Loc, error) {
	if !filepath.IsLocal(r.Rel) {
		return tools.Loc{}, fmt.Errorf("invalid path %q in the backup record", r.Rel)
	}
	switch r.Area {
	case areaOwn, "":
		first := strings.SplitN(filepath.ToSlash(r.Rel), "/", 2)[0]
		if r.Area == areaOwn && first == sharedPfx {
			return tools.Loc{}, fmt.Errorf("invalid path %q in the backup record", r.Rel)
		}
		return a.WS.Resolve(r.Rel, tools.AccessWrite)
	case areaShared:
		rest, ok := strings.CutPrefix(filepath.ToSlash(r.Rel), sharedPfx+"/")
		if !ok || !a.WS.HasAgentDir() {
			return tools.Loc{}, fmt.Errorf("invalid shared path %q in the backup record", r.Rel)
		}
		return a.WS.Resolve(filepath.Join(a.WS.Share, filepath.FromSlash(rest)), tools.AccessWrite)
	}
	return tools.Loc{}, fmt.Errorf("unknown area %q in the backup record", r.Area)
}

// applyPolicy settles the conflicts in ops according to policy and returns
// the existing entries that will be replaced (to be backed up first).
func applyPolicy(ops []restoreOp, policy conflictPolicy) ([]tools.Loc, error) {
	var conflicts []string
	var replaced []tools.Loc
	for i := range ops {
		if !ops[i].conflict {
			continue
		}
		switch policy {
		case conflictOverwrite:
			ops[i].replace = true
			replaced = append(replaced, ops[i].dst)
		default:
			conflicts = append(conflicts, ops[i].dst.Name)
		}
	}
	if len(conflicts) > 0 {
		shown := conflicts
		if len(shown) > 10 {
			shown = shown[:10]
		}
		more := ""
		if len(conflicts) > len(shown) {
			more = fmt.Sprintf(" and %d more", len(conflicts)-len(shown))
		}
		return nil, fmt.Errorf("%d path%s exist now (%s%s); nothing was restored. Use /safe restore <n> force to replace them (they are backed up first)",
			len(conflicts), pluralFiles(len(conflicts)), strings.Join(shown, ", "), more)
	}
	return replaced, nil
}

// runRestore writes the planned entries in order (parents before their
// contents) through the workspace handles, and returns how many it wrote.
func (a *Agent) runRestore(ops []restoreOp) (int, error) {
	written := 0
	for _, op := range ops {
		if err := op.dst.MkdirParents(); err != nil {
			return written, fmt.Errorf("%s: %v", a.WS.Rel(op.dst.Abs), err)
		}
		if op.replace {
			if err := op.dst.RemoveAll(); err != nil {
				return written, fmt.Errorf("%s: %v", a.WS.Rel(op.dst.Abs), err)
			}
		}
		var err error
		switch op.kind {
		case "dir":
			if err = op.dst.Mkdir(op.perm | 0o700); errors.Is(err, fs.ErrExist) && !op.replace {
				err = nil // merging into an existing directory
			}
		case "symlink":
			err = op.dst.Symlink(op.link)
		case "file":
			var b []byte
			if b, err = os.ReadFile(op.src); err == nil {
				if err = op.dst.CreateFile(b, op.perm); err == nil {
					a.WS.MarkSeen(op.dst.Abs, b)
				}
			}
		}
		if err != nil {
			return written, fmt.Errorf("%s: %v", a.WS.Rel(op.dst.Abs), err)
		}
		written++
	}
	return written, nil
}

func metaRefs(refs []backupRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Rel)
	}
	return out
}

func pluralFiles(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
