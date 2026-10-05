package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Workspace confines file access to one directory tree and remembers which
// file versions the agent has seen, so it never overwrites changes made by
// someone else in the shared space.
type Workspace struct {
	Root string // absolute, with symlinks resolved

	mu   sync.Mutex
	seen map[string]string // absolute path -> sha256 of the content the agent last read or wrote
}

func NewWorkspace(root string) (*Workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace %s: %w", root, err)
	}
	st, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Errorf("workspace %s: %w", root, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("workspace %s is not a directory", root)
	}
	return &Workspace{Root: real, seen: map[string]string{}}, nil
}

// Resolve maps a path from the model (relative to the root, or absolute) to
// an absolute path inside the workspace. Symlinks are resolved on the deepest
// part of the path that exists, so a link pointing outside cannot be used to
// escape; paths that do not exist yet (new files) are allowed.
func (w *Workspace) Resolve(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("path is empty")
	}
	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Join(w.Root, p)
	}
	existing := abs
	var rest []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = append([]string{filepath.Base(existing)}, rest...)
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %q: %v", p, err)
	}
	full := filepath.Join(append([]string{real}, rest...)...)
	if !w.inside(full) {
		return "", fmt.Errorf("path %q is outside the workspace (%s)", p, w.Root)
	}
	return full, nil
}

func (w *Workspace) inside(p string) bool {
	rel, err := filepath.Rel(w.Root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// Rel returns a path relative to the workspace root for display.
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
	}
	return err
}

// writeAtomic writes to a temporary file in the same directory and renames
// it over the target, so readers never see a half-written file.
func writeAtomic(path string, data []byte, perm fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, perm)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		os.Remove(tmp)
		return werr
	}
	return nil
}
