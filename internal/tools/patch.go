package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// ---------------------------------------------------------------- apply_patch

// ApplyPatch applies several file changes as one atomic, single-approval batch.
// Every edit is validated against the current file contents in Prepare; if any
// of them fails, nothing is written. Each edit either replaces an exact piece of
// text in an existing file (the same matching as edit_file) or, with an empty
// old_text, creates a new file. Edits apply in order, so a later edit sees the
// result of an earlier one on the same file.
type ApplyPatch struct {
	WS *Workspace
}

func (t *ApplyPatch) Name() string { return "apply_patch" }
func (t *ApplyPatch) Risk() Risk   { return Write }

func (t *ApplyPatch) Description() string {
	return "Apply several file changes as one atomic batch: all are validated first, and either all " +
		"are written or none are. Each edit either replaces an exact piece of text in an existing file " +
		"(old_text, exactly as edit_file: match whitespace and indentation, without the line-number " +
		"prefixes shown by read_file) or creates a new file (empty old_text, with new_text as the " +
		"content). Edits apply in order, so a later edit sees the result of an earlier one on the same " +
		"file. Prefer this over several edit_file or write_file calls when a change spans several files " +
		"or several places. Requires one operator approval for the whole batch."
}

func (t *ApplyPatch) Parameters() map[string]any {
	return objectSchema([]string{"edits"}, map[string]any{
		"edits": map[string]any{
			"type":        "array",
			"description": "The changes to apply together, in order. Either all succeed or none are written.",
			"items": objectSchema([]string{"path", "new_text"}, map[string]any{
				"path":        prop("string", "File path relative to the workspace root."),
				"old_text":    prop("string", "The exact text to replace, as in edit_file. Omit or leave empty to create a new file; the file must not already exist."),
				"new_text":    prop("string", "The replacement text, or the full content of a new file. May be empty to delete old_text."),
				"replace_all": prop("boolean", "Replace every occurrence of old_text in this file instead of exactly one. Default false."),
			}),
		},
	})
}

type patchEdit struct {
	path       string
	oldText    string
	newText    string
	replaceAll bool
}

// pendingFile is the working state for one file touched by a patch: its original
// bytes, a buffer that each edit mutates in turn, and enough to write it (or roll
// it back) later.
type pendingFile struct {
	loc     Loc
	rel     string
	exists  bool // existed on disk before the batch
	create  bool // created by this batch
	changed bool // net content differs from orig
	perm    fs.FileMode
	orig    []byte
	buf     string
	oldHash string
}

func parseEdits(args Args) ([]patchEdit, error) {
	raw, ok := args["edits"]
	if !ok || raw == nil {
		return nil, errors.New(`missing required argument "edits"`)
	}
	// Models sometimes send the array as a JSON-encoded string; accept that too.
	if s, isStr := raw.(string); isStr {
		var decoded []any
		if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &decoded); err != nil {
			return nil, errors.New(`argument "edits" must be an array of {path, old_text, new_text}`)
		}
		raw = decoded
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, errors.New(`argument "edits" must be an array of {path, old_text, new_text}`)
	}
	if len(list) == 0 {
		return nil, errors.New("edits must not be empty")
	}
	edits := make([]patchEdit, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("edit %d must be an object with path, old_text and new_text", i+1)
		}
		ea := Args(m)
		p, err := ea.Str("path", true)
		if err != nil {
			return nil, fmt.Errorf("edit %d: %v", i+1, err)
		}
		oldText, err := ea.Str("old_text", false)
		if err != nil {
			return nil, fmt.Errorf("edit %d: %v", i+1, err)
		}
		newText, err := ea.Str("new_text", true)
		if err != nil {
			return nil, fmt.Errorf("edit %d: %v", i+1, err)
		}
		replaceAll, err := ea.Bool("replace_all", false)
		if err != nil {
			return nil, fmt.Errorf("edit %d: %v", i+1, err)
		}
		edits = append(edits, patchEdit{p, oldText, newText, replaceAll})
	}
	return edits, nil
}

// applyMatch replaces oldText within s, mirroring edit_file: exact match, a retry
// with CRLF when the file uses Windows line endings, and a single occurrence
// unless replaceAll. It returns the new text and the number of replacements.
func applyMatch(s, oldText, newText string, replaceAll bool) (string, int, error) {
	if oldText == newText {
		return "", 0, errors.New("old_text and new_text are identical; nothing to change")
	}
	n := strings.Count(s, oldText)
	if n == 0 && strings.Contains(s, "\r\n") && !strings.Contains(oldText, "\r\n") {
		crlf := strings.ReplaceAll(oldText, "\n", "\r\n")
		if c := strings.Count(s, crlf); c > 0 {
			oldText, newText, n = crlf, strings.ReplaceAll(newText, "\n", "\r\n"), c
		}
	}
	if n == 0 {
		msg := "old_text was not found; read the file again and copy the text exactly, including indentation and whitespace"
		if lineNumberPrefix.MatchString(oldText) {
			msg += "; do not include the line-number prefixes shown by read_file"
		}
		return "", 0, errors.New(msg)
	}
	if n > 1 && !replaceAll {
		return "", 0, fmt.Errorf("old_text occurs %d times (at lines %s); include more surrounding lines to make it unique, or set replace_all to true",
			n, occurrenceLines(s, oldText, 10))
	}
	count, replaced := 1, 1
	if replaceAll {
		count, replaced = -1, n
	}
	return strings.Replace(s, oldText, newText, count), replaced, nil
}

func loadPending(loc Loc, rel string) (*pendingFile, error) {
	f := &pendingFile{loc: loc, rel: rel, perm: fs.FileMode(0o644)}
	st, err := loc.Stat()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return f, nil
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
	f.exists = true
	f.perm = st.Mode().Perm()
	f.orig = b
	f.buf = string(b)
	f.oldHash = hashBytes(b)
	return f, nil
}

func (t *ApplyPatch) Prepare(_ context.Context, args Args) (*Action, error) {
	edits, err := parseEdits(args)
	if err != nil {
		return nil, err
	}

	files := map[string]*pendingFile{}
	var order []string // rel paths in first-seen order, for a stable preview

	for i, e := range edits {
		loc, err := t.WS.ResolveFile(e.path, AccessWrite)
		if err != nil {
			return nil, fmt.Errorf("edit %d: %v", i+1, err)
		}
		rel := t.WS.Rel(loc.Abs)
		f := files[rel]
		if f == nil {
			if f, err = loadPending(loc, rel); err != nil {
				return nil, fmt.Errorf("edit %d: %v", i+1, err)
			}
			files[rel] = f
			order = append(order, rel)
		}

		if e.oldText == "" {
			// Create: the file must not exist and must not already be created
			// earlier in this same batch.
			if f.exists {
				return nil, fmt.Errorf("edit %d: %s already exists; use an edit with old_text to change it, or write_file to overwrite it", i+1, rel)
			}
			if f.create {
				return nil, fmt.Errorf("edit %d: %s is already being created earlier in this batch; combine the changes into one edit", i+1, rel)
			}
			f.create = true
			f.buf = e.newText
			continue
		}

		// Edit: the file must exist on disk or have been created earlier.
		if !f.exists && !f.create {
			return nil, fmt.Errorf("edit %d: %s does not exist; create it first with an empty old_text, or use write_file", i+1, rel)
		}
		updated, _, err := applyMatch(f.buf, e.oldText, e.newText, e.replaceAll)
		if err != nil {
			return nil, fmt.Errorf("edit %d (%s): %v", i+1, rel, err)
		}
		f.buf = updated
	}

	var preview strings.Builder
	totalAdded, totalRemoved, changed := 0, 0, 0
	var toWrite []*pendingFile
	for _, rel := range order {
		f := files[rel]
		// Skip a net no-op (edits that cancel out), but always write a create —
		// otherwise an intentionally empty new file would be dropped.
		if !f.create && f.buf == string(f.orig) {
			continue
		}
		d, added, removed := Diff(rel, string(f.orig), f.buf, 3)
		preview.WriteString(d)
		totalAdded += added
		totalRemoved += removed
		changed++
		f.changed = true
		toWrite = append(toWrite, f)
	}
	if changed == 0 {
		return nil, errors.New("the edits leave every file unchanged; nothing to apply")
	}

	summary := fmt.Sprintf("apply %d edit%s across %d file%s (+%d -%d lines)",
		len(edits), plural(len(edits)), changed, plural(changed), totalAdded, totalRemoved)

	return &Action{
		Summary: summary,
		Preview: preview.String(),
		Run: func(context.Context) (string, error) {
			// Approval can take a while; make sure nothing changed underneath us
			// before we write anything.
			for _, f := range toWrite {
				if f.create {
					if _, err := f.loc.Stat(); err == nil {
						return "", fmt.Errorf("%s was created by someone else while waiting for approval; read it first", f.rel)
					} else if !errors.Is(err, fs.ErrNotExist) {
						return "", cleanErr(err, f.rel)
					}
					continue
				}
				cur, err := f.loc.ReadFile()
				if err != nil {
					return "", cleanErr(err, f.rel)
				}
				if hashBytes(cur) != f.oldHash {
					return "", fmt.Errorf("%s changed on disk while waiting for approval; read it again and redo the patch", f.rel)
				}
			}
			// Write all. Each writeAtomic is temp+rename, so a single file is never
			// left half-written; across files we cannot be atomic, so on a failure
			// we best-effort roll back the files already written.
			var done []*pendingFile
			created, edited := 0, 0
			for _, f := range toWrite {
				if f.create {
					if err := f.loc.MkdirParents(); err != nil {
						rollbackPatch(t.WS, done)
						return "", cleanErr(err, f.rel)
					}
				}
				if err := t.WS.writeAtomic(f.loc, []byte(f.buf), f.perm); err != nil {
					rollbackPatch(t.WS, done)
					return "", cleanErr(err, f.rel)
				}
				t.WS.markSeen(f.loc.Abs, []byte(f.buf))
				done = append(done, f)
				if f.create {
					created++
				} else {
					edited++
				}
			}
			return fmt.Sprintf("Applied patch: %d file%s changed (%d created, %d edited, +%d -%d lines).",
				changed, plural(changed), created, edited, totalAdded, totalRemoved), nil
		},
	}, nil
}

// rollbackPatch best-effort undoes the files already written in a batch that
// failed partway: created files are removed, existing files are rewritten from
// the bytes read before the batch.
func rollbackPatch(ws *Workspace, done []*pendingFile) {
	for _, f := range done {
		if f.create {
			_ = f.loc.Remove()
			continue
		}
		if err := ws.writeAtomic(f.loc, f.orig, f.perm); err == nil {
			ws.markSeen(f.loc.Abs, f.orig)
		}
	}
}
