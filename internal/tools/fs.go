package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"clauzette/internal/textutil"
)

// Directories that are listed but never descended into or searched.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, ".venv": true, "venv": true,
	"__pycache__": true, ".clauzette": true, ".cache": true,
}

const (
	maxReadOutputChars = 100_000
	maxLineChars       = 2000
)

var errStopWalk = errors.New("stop walking")

func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	return bytes.IndexByte(b[:n], 0) >= 0
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ---------------------------------------------------------------- read_file

type ReadFile struct {
	WS           *Workspace
	DefaultLines int
	MaxBytes     int64
}

func (t *ReadFile) Name() string { return "read_file" }
func (t *ReadFile) Risk() Risk   { return ReadOnly }

func (t *ReadFile) Description() string {
	return "Read a text file from the workspace. Lines are returned with line-number prefixes " +
		"(the prefixes are not part of the file). Large files are returned in pages; use offset " +
		"and limit to read further. Read a file before editing or overwriting it."
}

func (t *ReadFile) Parameters() map[string]any {
	return objectSchema([]string{"path"}, map[string]any{
		"path":   prop("string", "File path relative to the workspace root (absolute paths must be inside the workspace)."),
		"offset": prop("integer", "1-based line number to start from. Default 1."),
		"limit":  prop("integer", fmt.Sprintf("Maximum number of lines to return. Default %d.", t.DefaultLines)),
	})
}

func (t *ReadFile) Prepare(_ context.Context, args Args) (*Action, error) {
	p, err := args.Str("path", true)
	if err != nil {
		return nil, err
	}
	offset, err := args.Int("offset", 1)
	if err != nil {
		return nil, err
	}
	limit, err := args.Int("limit", t.DefaultLines)
	if err != nil {
		return nil, err
	}
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = t.DefaultLines
	}
	loc, err := t.WS.ResolveFile(p, AccessRead)
	if err != nil {
		return nil, err
	}
	rel := t.WS.Rel(loc.Abs)
	return &Action{
		Summary: "read " + rel,
		Run: func(context.Context) (string, error) {
			return t.read(loc, rel, offset, limit)
		},
	}, nil
}

func (t *ReadFile) read(loc Loc, rel string, offset, limit int) (string, error) {
	st, err := loc.Stat()
	if err != nil {
		return "", cleanErr(err, rel)
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s is a directory; use list_dir", rel)
	}
	if t.MaxBytes > 0 && st.Size() > t.MaxBytes {
		return "", fmt.Errorf("%s is %s, over the %s read limit; inspect parts of it with search_files or exec_command (head, sed -n)",
			rel, textutil.HumanBytes(st.Size()), textutil.HumanBytes(t.MaxBytes))
	}
	b, err := loc.ReadFile()
	if err != nil {
		return "", cleanErr(err, rel)
	}
	if isBinary(b) {
		return fmt.Sprintf("%s is a binary file (%s); contents not shown.", rel, textutil.HumanBytes(int64(len(b)))), nil
	}
	t.WS.markSeen(loc.Abs, b)
	lines := splitLines(string(b))
	total := len(lines)
	if total == 0 {
		return rel + " is empty.", nil
	}
	if offset > total {
		return "", fmt.Errorf("offset %d is past the end of %s (%d lines)", offset, rel, total)
	}
	end := offset - 1 + limit
	if end > total {
		end = total
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (lines %d-%d of %d)\n", rel, offset, end, total)
	i := offset - 1
	for ; i < end && sb.Len() < maxReadOutputChars; i++ {
		line := strings.TrimRight(lines[i], "\r")
		if len(line) > maxLineChars {
			line = textutil.Truncate(line, maxLineChars) + " [line truncated]"
		}
		fmt.Fprintf(&sb, "%6d\t%s\n", i+1, line)
	}
	if i < total {
		fmt.Fprintf(&sb, "[%d more lines; continue with offset=%d]\n", total-i, i+1)
	}
	return sb.String(), nil
}

// ----------------------------------------------------------------- list_dir

type ListDir struct {
	WS *Workspace
}

func (t *ListDir) Name() string { return "list_dir" }
func (t *ListDir) Risk() Risk   { return ReadOnly }

func (t *ListDir) Description() string {
	return "List files and directories as an indented tree with file sizes. " +
		"Large tool and dependency directories (.git, node_modules, ...) are shown but not expanded."
}

func (t *ListDir) Parameters() map[string]any {
	return objectSchema(nil, map[string]any{
		"path":  prop("string", "Directory to list, relative to the workspace root. Default: the root."),
		"depth": prop("integer", "How many levels to descend (1-6). Default 2."),
	})
}

func (t *ListDir) Prepare(_ context.Context, args Args) (*Action, error) {
	p, err := args.Str("path", false)
	if err != nil {
		return nil, err
	}
	if p == "" {
		p = "."
	}
	depth, err := args.Int("depth", 2)
	if err != nil {
		return nil, err
	}
	if depth < 1 {
		depth = 1
	}
	if depth > 6 {
		depth = 6
	}
	loc, err := t.WS.Resolve(p, AccessRead)
	if err != nil {
		return nil, err
	}
	rel := t.WS.Rel(loc.Abs)
	return &Action{
		Summary: "list " + rel,
		Run: func(ctx context.Context) (string, error) {
			return t.list(ctx, loc, rel, depth)
		},
	}, nil
}

func (t *ListDir) list(ctx context.Context, loc Loc, rel string, maxDepth int) (string, error) {
	st, err := loc.Stat()
	if err != nil {
		return "", cleanErr(err, rel)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s is a file, not a directory", rel)
	}
	const maxEntries = 500
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s/\n", rel)
	count := 0
	truncated := false
	err = fs.WalkDir(loc.FS(), loc.Name, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p == loc.Name {
			return err
		}
		r := relName(loc.Name, p)
		depth := strings.Count(r, "/")
		indent := strings.Repeat("  ", depth+1)
		if err != nil {
			fmt.Fprintf(&sb, "%s%s [unreadable: %v]\n", indent, path.Base(p), err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if count >= maxEntries {
			truncated = true
			return fs.SkipAll
		}
		count++
		switch {
		case d.IsDir() && skipDirs[d.Name()]:
			fmt.Fprintf(&sb, "%s%s/ (not expanded)\n", indent, d.Name())
			return fs.SkipDir
		case d.IsDir() && depth+1 >= maxDepth:
			fmt.Fprintf(&sb, "%s%s/ …\n", indent, d.Name())
			return fs.SkipDir
		case d.IsDir():
			fmt.Fprintf(&sb, "%s%s/\n", indent, d.Name())
		case d.Type()&fs.ModeSymlink != 0:
			target, _ := loc.Child(p).Readlink()
			fmt.Fprintf(&sb, "%s%s -> %s\n", indent, d.Name(), target)
		default:
			size := ""
			if info, ierr := d.Info(); ierr == nil {
				size = " (" + textutil.HumanBytes(info.Size()) + ")"
			}
			fmt.Fprintf(&sb, "%s%s%s\n", indent, d.Name(), size)
		}
		return nil
	})
	if err != nil {
		return "", cleanErr(err, rel)
	}
	if count == 0 {
		sb.WriteString("  (empty)\n")
	}
	if truncated {
		fmt.Fprintf(&sb, "[stopped after %d entries; list a subdirectory or use a smaller depth]\n", maxEntries)
	}
	return sb.String(), nil
}

// ------------------------------------------------------------- search_files

type SearchFiles struct {
	WS *Workspace
}

func (t *SearchFiles) Name() string { return "search_files" }
func (t *SearchFiles) Risk() Risk   { return ReadOnly }

func (t *SearchFiles) Description() string {
	return "Search file contents in the workspace with a regular expression (RE2 syntax) and return " +
		"matching lines as path:line: text. Binary files, files over 2 MB and dependency directories are skipped."
}

func (t *SearchFiles) Parameters() map[string]any {
	return objectSchema([]string{"pattern"}, map[string]any{
		"pattern":          prop("string", "Regular expression to search for."),
		"path":             prop("string", "Directory or file to search. Default: the workspace root."),
		"glob":             prop("string", "Only search files whose name matches this glob, e.g. *.go"),
		"case_insensitive": prop("boolean", "Ignore case. Default false."),
		"max_results":      prop("integer", "Maximum number of matching lines. Default 100, at most 500."),
	})
}

func (t *SearchFiles) Prepare(_ context.Context, args Args) (*Action, error) {
	pattern, err := args.Str("pattern", true)
	if err != nil {
		return nil, err
	}
	p, err := args.Str("path", false)
	if err != nil {
		return nil, err
	}
	if p == "" {
		p = "."
	}
	glob, err := args.Str("glob", false)
	if err != nil {
		return nil, err
	}
	ci, err := args.Bool("case_insensitive", false)
	if err != nil {
		return nil, err
	}
	max, err := args.Int("max_results", 100)
	if err != nil {
		return nil, err
	}
	if max <= 0 {
		max = 100
	}
	if max > 500 {
		max = 500
	}
	expr := pattern
	if ci {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid regular expression: %v", err)
	}
	if glob != "" {
		if _, err := filepath.Match(glob, "x"); err != nil {
			return nil, fmt.Errorf("invalid glob %q: %v", glob, err)
		}
	}
	loc, err := t.WS.Resolve(p, AccessRead)
	if err != nil {
		return nil, err
	}
	return &Action{
		Summary: fmt.Sprintf("search /%s/ in %s", pattern, t.WS.Rel(loc.Abs)),
		Run: func(ctx context.Context) (string, error) {
			return t.search(ctx, loc, re, pattern, glob, max)
		},
	}, nil
}

func (t *SearchFiles) search(ctx context.Context, loc Loc, re *regexp.Regexp, pattern, glob string, max int) (string, error) {
	var sb strings.Builder
	count, files := 0, 0
	fsys := loc.FS()
	err := fs.WalkDir(fsys, loc.Name, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if p == loc.Name {
				return cleanErr(err, t.WS.Rel(loc.Abs))
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != loc.Name && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if glob != "" {
			if ok, _ := filepath.Match(glob, d.Name()); !ok {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || info.Size() > 2<<20 {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil || isBinary(b) {
			return nil
		}
		files++
		rel := t.WS.Rel(loc.Child(p).Abs)
		for i, line := range splitLines(string(b)) {
			if re.MatchString(line) {
				fmt.Fprintf(&sb, "%s:%d: %s\n", rel, i+1, textutil.Truncate(strings.TrimSpace(line), 300))
				count++
				if count >= max {
					return errStopWalk
				}
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return "", err
	}
	if count == 0 {
		return fmt.Sprintf("No matches for /%s/ in %d file%s.", pattern, files, plural(files)), nil
	}
	if count >= max {
		fmt.Fprintf(&sb, "[stopped at max_results=%d; narrow the search with path or glob]\n", max)
	}
	return sb.String(), nil
}

// --------------------------------------------------------------- write_file

type WriteFile struct {
	WS *Workspace
}

func (t *WriteFile) Name() string { return "write_file" }
func (t *WriteFile) Risk() Risk   { return Write }

func (t *WriteFile) Description() string {
	return "Create a new file, or replace the entire contents of an existing file. Parent directories " +
		"are created as needed. An existing file must be read with read_file first. To change part of " +
		"an existing file, use edit_file instead. Requires operator approval."
}

func (t *WriteFile) Parameters() map[string]any {
	return objectSchema([]string{"path", "content"}, map[string]any{
		"path":    prop("string", "File path relative to the workspace root."),
		"content": prop("string", "The complete new content of the file."),
	})
}

func (t *WriteFile) Prepare(_ context.Context, args Args) (*Action, error) {
	p, err := args.Str("path", true)
	if err != nil {
		return nil, err
	}
	content, err := args.Str("content", true)
	if err != nil {
		return nil, err
	}
	loc, err := t.WS.ResolveFile(p, AccessWrite)
	if err != nil {
		return nil, err
	}
	path, rel := loc.Abs, t.WS.Rel(loc.Abs)

	st, statErr := loc.Stat()
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return nil, cleanErr(statErr, rel)
	}
	var old []byte
	perm := fs.FileMode(0o644)
	if exists {
		if st.IsDir() {
			return nil, fmt.Errorf("%s is a directory", rel)
		}
		perm = st.Mode().Perm()
		if old, err = loc.ReadFile(); err != nil {
			return nil, cleanErr(err, rel)
		}
		h, seen := t.WS.seenHash(path)
		if !seen {
			return nil, fmt.Errorf("%s already exists. Read it with read_file before overwriting it, or use edit_file to change part of it", rel)
		}
		if h != hashBytes(old) {
			return nil, fmt.Errorf("%s has changed since you last read it (possibly edited by someone else). Read it again before overwriting it", rel)
		}
		if string(old) == content {
			return nil, fmt.Errorf("%s already has exactly this content; nothing to write", rel)
		}
	}

	newLines := len(splitLines(content))
	summary := fmt.Sprintf("create %s (%d line%s)", rel, newLines, plural(newLines))
	if exists {
		summary = fmt.Sprintf("overwrite %s (%d → %d lines)", rel, len(splitLines(string(old))), newLines)
	}
	preview, _, _ := Diff(rel, string(old), content, 3)
	oldHash := hashBytes(old)

	return &Action{
		Summary: summary,
		Preview: preview,
		Run: func(context.Context) (string, error) {
			// Approval can take a while; make sure nobody changed the file meanwhile.
			cur, err := loc.ReadFile()
			switch {
			case exists && err != nil:
				return "", fmt.Errorf("%s could not be re-read before writing: %v", rel, err)
			case exists && hashBytes(cur) != oldHash:
				return "", fmt.Errorf("%s changed on disk while waiting for approval; read it again", rel)
			case !exists && err == nil:
				return "", fmt.Errorf("%s was created by someone else while waiting for approval; read it first", rel)
			}
			if err := loc.MkdirParents(); err != nil {
				return "", cleanErr(err, rel)
			}
			if err := t.WS.writeAtomic(loc, []byte(content), perm); err != nil {
				return "", cleanErr(err, rel)
			}
			t.WS.markSeen(path, []byte(content))
			verb := "Created"
			if exists {
				verb = "Overwrote"
			}
			return fmt.Sprintf("%s %s (%d line%s, %s).", verb, rel, newLines, plural(newLines),
				textutil.HumanBytes(int64(len(content)))), nil
		},
	}, nil
}

// ---------------------------------------------------------------- edit_file

var lineNumberPrefix = regexp.MustCompile(`^\s*\d+\t`)

type EditFile struct {
	WS *Workspace
}

func (t *EditFile) Name() string { return "edit_file" }
func (t *EditFile) Risk() Risk   { return Write }

func (t *EditFile) Description() string {
	return "Replace an exact piece of text in an existing file. old_text must match the file exactly, " +
		"including whitespace and indentation, and without the line-number prefixes shown by read_file. " +
		"It must occur exactly once unless replace_all is true; include enough surrounding lines to make " +
		"it unique. Requires operator approval."
}

func (t *EditFile) Parameters() map[string]any {
	return objectSchema([]string{"path", "old_text", "new_text"}, map[string]any{
		"path":        prop("string", "File path relative to the workspace root."),
		"old_text":    prop("string", "The exact text to replace."),
		"new_text":    prop("string", "The replacement text (may be empty to delete old_text)."),
		"replace_all": prop("boolean", "Replace every occurrence instead of exactly one. Default false."),
	})
}

func (t *EditFile) Prepare(_ context.Context, args Args) (*Action, error) {
	p, err := args.Str("path", true)
	if err != nil {
		return nil, err
	}
	oldText, err := args.Str("old_text", true)
	if err != nil {
		return nil, err
	}
	newText, err := args.Str("new_text", true)
	if err != nil {
		return nil, err
	}
	replaceAll, err := args.Bool("replace_all", false)
	if err != nil {
		return nil, err
	}
	if oldText == "" {
		return nil, errors.New("old_text must not be empty; use write_file to create a file")
	}
	if oldText == newText {
		return nil, errors.New("old_text and new_text are identical; nothing to change")
	}
	loc, err := t.WS.ResolveFile(p, AccessWrite)
	if err != nil {
		return nil, err
	}
	path, rel := loc.Abs, t.WS.Rel(loc.Abs)
	st, err := loc.Stat()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s does not exist; use write_file to create it", rel)
		}
		return nil, cleanErr(err, rel)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%s is a directory", rel)
	}
	b, err := loc.ReadFile()
	if err != nil {
		return nil, cleanErr(err, rel)
	}
	if isBinary(b) {
		return nil, fmt.Errorf("%s is a binary file", rel)
	}
	s := string(b)
	n := strings.Count(s, oldText)
	// Files with Windows line endings: retry with CRLF if the model sent LF.
	if n == 0 && strings.Contains(s, "\r\n") && !strings.Contains(oldText, "\r\n") {
		crlf := strings.ReplaceAll(oldText, "\n", "\r\n")
		if c := strings.Count(s, crlf); c > 0 {
			oldText, newText, n = crlf, strings.ReplaceAll(newText, "\n", "\r\n"), c
		}
	}
	if n == 0 {
		msg := fmt.Sprintf("old_text was not found in %s. Read the file again and copy the text exactly, including indentation and whitespace", rel)
		if lineNumberPrefix.MatchString(oldText) {
			msg += "; do not include the line-number prefixes shown by read_file"
		}
		return nil, errors.New(msg)
	}
	if n > 1 && !replaceAll {
		return nil, fmt.Errorf("old_text occurs %d times in %s (at lines %s). Include more surrounding lines to make it unique, or set replace_all to true",
			n, rel, occurrenceLines(s, oldText, 10))
	}
	count, replaced := 1, 1
	if replaceAll {
		count, replaced = -1, n
	}
	updated := strings.Replace(s, oldText, newText, count)
	preview, added, removed := Diff(rel, s, updated, 3)
	oldHash := hashBytes(b)
	perm := st.Mode().Perm()

	return &Action{
		Summary: fmt.Sprintf("edit %s (%d replacement%s, +%d -%d lines)", rel, replaced, plural(replaced), added, removed),
		Preview: preview,
		Run: func(context.Context) (string, error) {
			cur, err := loc.ReadFile()
			if err != nil {
				return "", cleanErr(err, rel)
			}
			if hashBytes(cur) != oldHash {
				return "", fmt.Errorf("%s changed on disk while waiting for approval; read it again and redo the edit", rel)
			}
			if err := t.WS.writeAtomic(loc, []byte(updated), perm); err != nil {
				return "", cleanErr(err, rel)
			}
			t.WS.markSeen(path, []byte(updated))
			return fmt.Sprintf("Edited %s: %d replacement%s (+%d -%d lines).", rel, replaced, plural(replaced), added, removed), nil
		},
	}, nil
}

func occurrenceLines(s, sub string, max int) string {
	var lines []string
	offset := 0
	for len(lines) < max {
		i := strings.Index(s[offset:], sub)
		if i < 0 {
			break
		}
		pos := offset + i
		lines = append(lines, fmt.Sprint(strings.Count(s[:pos], "\n")+1))
		offset = pos + len(sub)
	}
	return strings.Join(lines, ", ")
}

// dirFileCount reports how many regular files and how many total bytes are
// under a directory.
func dirFileCount(loc Loc) (int, int64) {
	n, size := 0, int64(0)
	fs.WalkDir(loc.FS(), loc.Name, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d != nil && !d.IsDir() {
			n++
			if info, ierr := d.Info(); ierr == nil {
				size += info.Size()
			}
		}
		return nil
	})
	return n, size
}

func kindName(isDir bool) string {
	if isDir {
		return "directory"
	}
	return "file"
}

// relName returns name relative to the walk root dir, both as passed to an
// fs.WalkDir callback.
func relName(dir, name string) string {
	if dir == "." {
		return name
	}
	return strings.TrimPrefix(name, dir+"/")
}

// entryKind describes what an Lstat result is, for comparing it before and
// after an approval.
func entryKind(fi fs.FileInfo) string {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return "symlink"
	case fi.IsDir():
		return "directory"
	}
	return "file"
}

// walkFiles lists the absolute paths of the non-directory entries under loc
// (loc itself if it is not a directory), without following symlinks.
func walkFiles(loc Loc) []string {
	var out []string
	fs.WalkDir(loc.FS(), loc.Name, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d != nil && !d.IsDir() {
			out = append(out, loc.Child(p).Abs)
		}
		return nil
	})
	return out
}

// ------------------------------------------------------------------ mv_file

type MvFile struct {
	WS *Workspace
}

func (t *MvFile) Name() string { return "mv_file" }
func (t *MvFile) Risk() Risk   { return Destructive }

func (t *MvFile) Description() string {
	return "Move (rename) a file or directory inside the workspace. The target must not already " +
		"exist; if it is an empty directory, the item is moved into it. Parent directories of the " +
		"target are created as needed. This is irreversible — requires operator approval, and in " +
		"safe mode the source is backed up before it runs."
}

func (t *MvFile) Parameters() map[string]any {
	return objectSchema([]string{"from", "to"}, map[string]any{
		"from": prop("string", "Existing file or directory to move."),
		"to":   prop("string", "New path."),
	})
}

func (t *MvFile) Prepare(_ context.Context, args Args) (*Action, error) {
	from, err := args.Str("from", true)
	if err != nil {
		return nil, err
	}
	to, err := args.Str("to", true)
	if err != nil {
		return nil, err
	}
	// Neither side follows a final symlink: moving a link moves the link.
	src, err := t.WS.Resolve(from, AccessWrite)
	if err != nil {
		return nil, err
	}
	dst, err := t.WS.Resolve(to, AccessWrite)
	if err != nil {
		return nil, err
	}
	relSrc := t.WS.Rel(src.Abs)
	if src.IsRoot() || src.IsAgentDir() {
		return nil, errors.New("that is the workspace root itself; choose a path inside it")
	}
	st, err := src.Lstat()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s does not exist", relSrc)
		}
		return nil, cleanErr(err, relSrc)
	}
	kind := entryKind(st)
	if dstSt, err := dst.Lstat(); err == nil {
		if src.Abs == dst.Abs {
			return nil, errors.New("source and destination are the same path")
		}
		if dstSt.IsDir() {
			entries, rerr := dst.ReadDir()
			if rerr != nil {
				return nil, cleanErr(rerr, t.WS.Rel(dst.Abs))
			}
			if len(entries) != 0 {
				return nil, fmt.Errorf("%s exists and is not empty; to move into it, use a new name inside it, e.g. %s/%s",
					t.WS.Rel(dst.Abs), t.WS.Rel(dst.Abs), path.Base(src.Name))
			}
			dst = dst.Child(path.Join(dst.Name, path.Base(src.Name)))
		} else {
			return nil, fmt.Errorf("%s already exists; choose a different name or delete it first", t.WS.Rel(dst.Abs))
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, cleanErr(err, t.WS.Rel(dst.Abs))
	}
	relDst := t.WS.Rel(dst.Abs)

	var content []byte
	var summary, linkTarget string
	var backups []BackupRef
	switch kind {
	case "directory":
		n, size := dirFileCount(src)
		summary = fmt.Sprintf("move %s/ to %s/ (%d file%s, %s)", relSrc, relDst, n, plural(n), textutil.HumanBytes(size))
		backups = []BackupRef{src.BackupRef()}
	case "symlink":
		linkTarget, _ = src.Readlink()
		summary = fmt.Sprintf("move the symlink %s (-> %s) to %s; its target is not touched", relSrc, linkTarget, relDst)
	default:
		b, err := src.ReadFile()
		if err != nil {
			return nil, cleanErr(err, relSrc)
		}
		content = b
		lines := len(splitLines(string(b)))
		summary = fmt.Sprintf("move %s to %s (%d line%s)", relSrc, relDst, lines, plural(lines))
		backups = []BackupRef{src.BackupRef()}
	}
	oldHash := hashBytes(content)

	return &Action{
		Summary: summary,
		Backups: backups,
		Run: func(context.Context) (string, error) {
			curSt, err := src.Lstat()
			if err != nil {
				return "", cleanErr(err, relSrc)
			}
			if entryKind(curSt) != kind {
				return "", fmt.Errorf("%s is no longer a %s; look at it again", relSrc, kind)
			}
			switch kind {
			case "file":
				cur, err := src.ReadFile()
				if err != nil {
					return "", cleanErr(err, relSrc)
				}
				if hashBytes(cur) != oldHash {
					return "", fmt.Errorf("%s changed on disk while waiting for approval; look at it again", relSrc)
				}
			case "symlink":
				if cur, _ := src.Readlink(); cur != linkTarget {
					return "", fmt.Errorf("the symlink %s changed while waiting for approval; look at it again", relSrc)
				}
			}
			if dstSt, err := dst.Lstat(); err == nil {
				if dstSt.IsDir() {
					entries, rerr := dst.ReadDir()
					if rerr != nil {
						return "", cleanErr(rerr, relDst)
					}
					if len(entries) != 0 {
						return "", fmt.Errorf("%s is no longer empty; choose a different target", relDst)
					}
				} else {
					return "", fmt.Errorf("%s was created while waiting for approval; choose a different target", relDst)
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				return "", cleanErr(err, relDst)
			}
			oldFiles := walkFiles(src)
			if err := dst.MkdirParents(); err != nil {
				return "", cleanErr(err, relDst)
			}
			if err := src.Rename(dst); err != nil {
				return "", cleanErr(err, relDst)
			}
			t.WS.unmark(oldFiles...)
			if kind == "file" {
				t.WS.markSeen(dst.Abs, content)
			}
			return fmt.Sprintf("Moved %s to %s.", relSrc, relDst), nil
		},
	}, nil
}

// ------------------------------------------------------------- delete_file

type DeleteFile struct {
	WS *Workspace
}

func (t *DeleteFile) Name() string { return "delete_file" }
func (t *DeleteFile) Risk() Risk   { return Destructive }

func (t *DeleteFile) Description() string {
	return "Delete a file, or an empty directory. A non-empty directory requires recursive=true. " +
		"This is irreversible — requires operator approval, and in safe mode the target is backed " +
		"up before it runs."
}

func (t *DeleteFile) Parameters() map[string]any {
	return objectSchema([]string{"path"}, map[string]any{
		"path":      prop("string", "File or directory to delete."),
		"recursive": prop("boolean", "Delete a non-empty directory and everything inside it. Default false."),
	})
}

func (t *DeleteFile) Prepare(_ context.Context, args Args) (*Action, error) {
	p, err := args.Str("path", true)
	if err != nil {
		return nil, err
	}
	recursive, err := args.Bool("recursive", false)
	if err != nil {
		return nil, err
	}
	// The final component is not followed: deleting a link deletes the link.
	loc, err := t.WS.Resolve(p, AccessWrite)
	if err != nil {
		return nil, err
	}
	rel := t.WS.Rel(loc.Abs)
	if loc.IsRoot() || loc.IsAgentDir() {
		return nil, errors.New("that is the workspace root itself; choose a path inside it")
	}
	st, err := loc.Lstat()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s does not exist", rel)
		}
		return nil, cleanErr(err, rel)
	}
	kind := entryKind(st)
	var content []byte
	var summary, linkTarget string
	var backups []BackupRef
	switch kind {
	case "directory":
		if !recursive {
			entries, rerr := loc.ReadDir()
			if rerr != nil {
				return nil, cleanErr(rerr, rel)
			}
			if len(entries) != 0 {
				return nil, fmt.Errorf("%s is a directory with %d entries; pass recursive=true to delete it and everything inside", rel, len(entries))
			}
		}
		n, size := dirFileCount(loc)
		summary = fmt.Sprintf("delete %s/ (%d file%s, %s)", rel, n, plural(n), textutil.HumanBytes(size))
		backups = []BackupRef{loc.BackupRef()}
	case "symlink":
		linkTarget, _ = loc.Readlink()
		summary = fmt.Sprintf("delete the symlink %s (-> %s); its target is not touched", rel, linkTarget)
	default:
		b, err := loc.ReadFile()
		if err != nil {
			return nil, cleanErr(err, rel)
		}
		content = b
		lines := len(splitLines(string(b)))
		summary = fmt.Sprintf("delete %s (%d line%s, %s)", rel, lines, plural(lines), textutil.HumanBytes(int64(len(b))))
		backups = []BackupRef{loc.BackupRef()}
	}
	oldHash := hashBytes(content)

	return &Action{
		Summary: summary,
		Backups: backups,
		Run: func(context.Context) (string, error) {
			curSt, err := loc.Lstat()
			if err != nil {
				return "", cleanErr(err, rel)
			}
			if entryKind(curSt) != kind {
				return "", fmt.Errorf("%s is no longer a %s; look at it again", rel, kind)
			}
			switch kind {
			case "directory":
				if !recursive {
					entries, rerr := loc.ReadDir()
					if rerr != nil {
						return "", cleanErr(rerr, rel)
					}
					if len(entries) != 0 {
						return "", fmt.Errorf("%s is no longer empty; pass recursive=true", rel)
					}
				}
			case "symlink":
				if cur, _ := loc.Readlink(); cur != linkTarget {
					return "", fmt.Errorf("the symlink %s changed while waiting for approval; look at it again", rel)
				}
			default:
				cur, err := loc.ReadFile()
				if err != nil {
					return "", cleanErr(err, rel)
				}
				if hashBytes(cur) != oldHash {
					return "", fmt.Errorf("%s changed on disk while waiting for approval; look at it again", rel)
				}
			}
			files := walkFiles(loc)
			if kind == "directory" {
				err = loc.RemoveAll()
			} else {
				err = loc.Remove()
			}
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", cleanErr(err, rel)
			}
			t.WS.unmark(files...)
			return fmt.Sprintf("Deleted %s.", rel), nil
		},
	}, nil
}
