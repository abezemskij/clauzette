package tools

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// Access is what the agent may do in the shared workspace outside its own
// directory.
type Access int

const (
	AccessNone Access = iota
	AccessRead
	AccessWrite
)

func (a Access) String() string {
	switch a {
	case AccessRead:
		return "read"
	case AccessWrite:
		return "write"
	}
	return "none"
}

// ParseAccess reads "none", "read" or "write".
func ParseAccess(s string) (Access, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none":
		return AccessNone, nil
	case "read":
		return AccessRead, nil
	case "write":
		return AccessWrite, nil
	}
	return AccessNone, fmt.Errorf("access must be none, read or write, not %q", s)
}

// WorkspaceOptions describe where the agent works.
type WorkspaceOptions struct {
	Share    string // the shared workspace root
	AgentDir string // the agent's own directory inside Share (created if missing); "" = the whole share is the workspace
	Shared   Access // what the agent may do in Share outside AgentDir
}

// Workspace confines file access to the agent's directory (and, as allowed,
// the rest of the shared workspace) and remembers which file versions the
// agent has seen, so it never overwrites changes made by someone else.
//
// Every file operation goes through an os.Root handle, which resolves each
// path component beneath the root at the moment of the operation. A
// directory swapped for a symlink after an action was approved cannot lead
// it outside, and symlinks are followed only while they stay inside (and
// only when relative).
type Workspace struct {
	Root   string // the agent's directory: absolute, symlinks resolved; relative paths start here
	Share  string // the shared workspace root; equal to Root without an agent directory
	Access Access // what may be done in Share outside Root

	own, share *os.Root
	ownAliases []string // Root as configured and as resolved
	shareAlias []string
	agentDir   string // name of Root inside Share, "" without one
	mu         sync.Mutex
	seen       map[string]string // absolute path -> sha256 of the content the agent last read or wrote
}

// NewWorkspace opens a workspace that is a single directory tree.
func NewWorkspace(root string) (*Workspace, error) {
	return OpenWorkspace(WorkspaceOptions{Share: root})
}

// OpenWorkspace opens the share root and, if one is named, the agent's own
// directory inside it, creating that directory when it does not exist.
func OpenWorkspace(o WorkspaceOptions) (*Workspace, error) {
	shareAbs, err := filepath.Abs(o.Share)
	if err != nil {
		return nil, err
	}
	shareReal, err := realDir(shareAbs)
	if err != nil {
		return nil, fmt.Errorf("workspace %s: %w", o.Share, err)
	}
	w := &Workspace{
		Root: shareReal, Share: shareReal, Access: o.Shared,
		shareAlias: uniq(shareReal, shareAbs), seen: map[string]string{},
	}
	if w.share, err = os.OpenRoot(shareReal); err != nil {
		return nil, fmt.Errorf("workspace %s: %w", o.Share, err)
	}
	w.own, w.ownAliases = w.share, w.shareAlias
	if o.AgentDir == "" {
		return w, nil
	}
	if !ValidAgentDir(o.AgentDir) {
		w.share.Close()
		return nil, fmt.Errorf("agent directory %q: use letters, digits, '.', '_' and '-' only", o.AgentDir)
	}
	// Created through the share handle, and refused if it is a symlink, so
	// the agent's directory is a real directory inside the share.
	if err := w.share.Mkdir(o.AgentDir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		w.share.Close()
		return nil, fmt.Errorf("agent directory: %w", err)
	}
	if st, err := w.share.Lstat(o.AgentDir); err != nil || !st.IsDir() {
		w.share.Close()
		return nil, fmt.Errorf("agent directory %s/%s is not a directory", shareReal, o.AgentDir)
	}
	if w.own, err = w.share.OpenRoot(o.AgentDir); err != nil {
		w.share.Close()
		return nil, fmt.Errorf("agent directory: %w", err)
	}
	w.agentDir = o.AgentDir
	w.Root = filepath.Join(shareReal, o.AgentDir)
	w.ownAliases = uniq(w.Root, filepath.Join(shareAbs, o.AgentDir))
	return w, nil
}

// ValidAgentDir reports whether name is usable as an agent directory: one
// path component of letters, digits, '.', '_' and '-', not starting with '.'.
func ValidAgentDir(name string) bool {
	if name == "" || len(name) > 128 || name[0] == '.' {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func realDir(abs string) (string, error) {
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", errors.New("not a directory")
	}
	return real, nil
}

func uniq(a, b string) []string {
	if a == b {
		return []string{a}
	}
	return []string{a, b}
}

func randomSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Close releases the directory handles.
func (w *Workspace) Close() error {
	if w.own != w.share {
		w.own.Close()
	}
	return w.share.Close()
}

// HasAgentDir reports whether the agent works in its own directory inside
// a larger shared workspace.
func (w *Workspace) HasAgentDir() bool { return w.agentDir != "" }

// Loc is a resolved workspace path: the directory handle to use and the
// name relative to it.
type Loc struct {
	root   *os.Root
	base   string // absolute path of the handle's directory
	Name   string // slash-separated and relative to the handle; "." is the directory itself
	Abs    string // absolute path, for display, exec and backups
	Shared bool   // outside the agent's own directory
	agent  string // the agent's directory name, for a Loc in the share
}

// Resolve maps a path from the model (relative to the agent's directory,
// or absolute) to a Loc. Paths in the shared workspace outside the agent's
// directory are allowed when the access level covers need. Nothing is
// touched on disk; symlinks are resolved by each operation, beneath the
// handle, so they cannot lead outside.
func (w *Workspace) Resolve(p string, need Access) (Loc, error) {
	if strings.TrimSpace(p) == "" {
		return Loc{}, errors.New("path is empty")
	}
	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Join(w.Root, p)
	}
	if name, ok := within(w.ownAliases, abs); ok {
		return Loc{root: w.own, base: w.Root, Name: name, Abs: filepath.Join(w.Root, filepath.FromSlash(name))}, nil
	}
	if w.HasAgentDir() {
		if name, ok := within(w.shareAlias, abs); ok {
			switch {
			case w.Access == AccessNone:
				return Loc{}, fmt.Errorf("%s is outside your directory (%s), and the rest of the shared workspace is not accessible", p, w.Root)
			case w.Access < need:
				return Loc{}, fmt.Errorf("%s is outside your directory (%s); the rest of the shared workspace is read-only for you", p, w.Root)
			}
			return Loc{root: w.share, base: w.Share, Name: name, Abs: filepath.Join(w.Share, filepath.FromSlash(name)), Shared: true, agent: w.agentDir}, nil
		}
		return Loc{}, fmt.Errorf("path %q is outside the shared workspace (%s)", p, w.Share)
	}
	return Loc{}, fmt.Errorf("path %q is outside the workspace (%s)", p, w.Root)
}

// ResolveFile is Resolve for tools that work on file contents: when the
// path is a symlink, it follows it (relative links only, staying beneath
// the same handle), so reading and writing through a link both use the
// target, and "read before overwrite" matches across the two.
func (w *Workspace) ResolveFile(p string, need Access) (Loc, error) {
	l, err := w.Resolve(p, need)
	if err != nil {
		return l, err
	}
	return l.followLinks()
}

func within(aliases []string, abs string) (string, bool) {
	for _, a := range aliases {
		rel, err := filepath.Rel(a, abs)
		if err != nil {
			continue
		}
		if rel == "." {
			return ".", true
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(rel), true
		}
	}
	return "", false
}

// IsAgentDir reports whether l is the agent's own directory seen from the share.
func (l Loc) IsAgentDir() bool { return l.Shared && l.Name == l.agent }

// IsRoot reports whether l is the directory of its handle (the agent's
// directory or the share root).
func (l Loc) IsRoot() bool { return l.Name == "." }

// Child returns the Loc of a slash-separated name under the same handle,
// as passed to an fs.WalkDir callback on l.FS().
func (l Loc) Child(name string) Loc {
	c := l
	c.Name = name
	c.Abs = filepath.Join(l.base, filepath.FromSlash(name))
	return c
}

// FS is the handle as an fs.FS, for walking; names are relative to the handle.
func (l Loc) FS() fs.FS { return l.root.FS() }

func (l Loc) Stat() (fs.FileInfo, error)  { return l.root.Stat(l.Name) }
func (l Loc) Lstat() (fs.FileInfo, error) { return l.root.Lstat(l.Name) }
func (l Loc) ReadFile() ([]byte, error)   { return l.root.ReadFile(l.Name) }
func (l Loc) Readlink() (string, error)   { return l.root.Readlink(l.Name) }
func (l Loc) ReadDir() ([]fs.DirEntry, error) {
	return fs.ReadDir(l.root.FS(), l.Name)
}
func (l Loc) Remove() error    { return l.root.Remove(l.Name) }
func (l Loc) RemoveAll() error { return l.root.RemoveAll(l.Name) }

// MkdirParents creates the parent directories of l. An existing parent is
// checked first, so a symlink leading outside reports that, not "exists".
func (l Loc) MkdirParents() error {
	dir := path.Dir(l.Name)
	if dir == "." {
		return nil
	}
	_, err := l.root.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return l.root.MkdirAll(dir, 0o755)
	}
	return err
}

// BackupRel names l inside a safe-mode backup: its path in the agent's
// directory, or "_shared/<path in the share>" for a shared one, so it can
// never point outside the backup.
func (l Loc) BackupRel() string {
	if l.Shared {
		return filepath.FromSlash(path.Join("_shared", l.Name))
	}
	return filepath.FromSlash(l.Name)
}

// BackupRef describes l for a safe-mode backup.
func (l Loc) BackupRef() BackupRef { return BackupRef{Abs: l.Abs, Rel: l.BackupRel(), Loc: l} }

// Valid reports whether l came from Resolve (the zero Loc has no handle).
func (l Loc) Valid() bool { return l.root != nil }

// Mkdir creates the directory l.
func (l Loc) Mkdir(perm fs.FileMode) error { return l.root.Mkdir(l.Name, perm) }

// Symlink creates l as a symlink to target.
func (l Loc) Symlink(target string) error { return l.root.Symlink(target, l.Name) }

// CreateFile creates l, which must not exist yet, with data and perm.
func (l Loc) CreateFile(data []byte, perm fs.FileMode) error {
	f, err := l.root.OpenFile(l.Name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Chmod(perm)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		l.root.Remove(l.Name)
	}
	return werr
}

// Rename moves l to dst. Both must be under the same handle, or both in
// the shared workspace (an agent-directory Loc is re-expressed through the
// share handle when the other side is shared).
func (l Loc) Rename(dst Loc) error {
	src := l
	if src.root != dst.root {
		var ok bool
		if src, ok = src.viaShare(dst); !ok {
			return errors.New("cannot move between these locations")
		}
		if dst, ok = dst.viaShare(src); !ok {
			return errors.New("cannot move between these locations")
		}
	}
	return src.root.Rename(src.Name, dst.Name)
}

// viaShare re-expresses an agent-directory Loc through the share handle of
// other (a shared Loc). A shared Loc is returned unchanged.
func (l Loc) viaShare(other Loc) (Loc, bool) {
	if l.Shared {
		return l, true
	}
	if !other.Shared {
		return l, false
	}
	name := other.agent
	if l.Name != "." {
		name = path.Join(other.agent, l.Name)
	}
	return Loc{root: other.root, base: other.base, Name: name, Abs: l.Abs, Shared: true, agent: other.agent}, true
}

// followLinks resolves l while its last component is a symlink. Links are
// followed only while relative and beneath the same handle.
func (l Loc) followLinks() (Loc, error) {
	for i := 0; i < 40; i++ {
		st, err := l.root.Lstat(l.Name)
		if err != nil || st.Mode()&fs.ModeSymlink == 0 {
			return l, nil // a missing path is fine here (a new file)
		}
		target, err := l.root.Readlink(l.Name)
		if err != nil {
			return l, err
		}
		if path.IsAbs(target) {
			return l, fmt.Errorf("%s is a symlink to an absolute path (%s); only relative links inside the workspace are followed", l.Name, target)
		}
		next := path.Join(path.Dir(l.Name), target)
		if next == ".." || strings.HasPrefix(next, "../") {
			return l, fmt.Errorf("%s is a symlink that points outside the workspace (%s)", l.Name, target)
		}
		l = l.Child(next)
	}
	return l, fmt.Errorf("%s: too many levels of symlinks", l.Name)
}

// Rel returns a path relative to the agent's directory for display.
func (w *Workspace) Rel(abs string) string {
	rel, err := filepath.Rel(w.Root, abs)
	if err != nil {
		return abs
	}
	return rel
}

func (w *Workspace) markSeen(path string, content []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seen[path] = hashBytes(content)
}

// MarkSeen records the file as read. Used by the agent's safe-mode restore
// so the agent may edit a restored file without reading it again.
func (w *Workspace) MarkSeen(path string, content []byte) { w.markSeen(path, content) }

func (w *Workspace) seenHash(path string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	h, ok := w.seen[path]
	return h, ok
}

// unmark forgets that the agent saw these paths, for example after they
// were deleted or moved away, so a later file at the same path must be
// read before it can be overwritten.
func (w *Workspace) unmark(paths ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range paths {
		delete(w.seen, p)
	}
}

func hashBytes(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// cleanErr makes common file errors readable for the model.
func cleanErr(err error, rel string) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s does not exist", rel)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("permission denied: %s", rel)
	case err != nil && strings.Contains(err.Error(), "path escapes from parent"):
		return fmt.Errorf("%s leads outside the allowed directory (through a symlink, or a link with an absolute target)", rel)
	}
	return err
}

// writeAtomic writes to a temporary file in the same directory and renames
// it over the target, so readers never see a half-written file. Both steps
// go through the handle.
func (w *Workspace) writeAtomic(l Loc, data []byte, perm fs.FileMode) error {
	tmp := path.Join(path.Dir(l.Name), "."+path.Base(l.Name)+".tmp-"+randomSuffix())
	f, err := l.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Chmod(perm)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = l.root.Rename(tmp, l.Name)
	}
	if werr != nil {
		l.root.Remove(tmp)
		return werr
	}
	return nil
}
