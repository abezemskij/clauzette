package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"

	"clauzette/internal/agent"
	"clauzette/internal/session"
	"clauzette/internal/tools"
)

// Data for the web page: lists as JSON, the workspace (read-only), and
// uploads into <workspace>/uploads. Everything is behind the token.

// DefaultUploadMax is the largest upload when none is configured.
const DefaultUploadMax = 25 << 20

// maxView is the largest file the page shows.
const maxView = 2 << 20

// onLoop runs fn on the goroutine that owns the controller (Run), so it
// may read the agent's state.
func (s *Server) onLoop(ctx context.Context, fn func()) error {
	done := make(chan struct{})
	select {
	case s.calls <- func() { fn(); close(done) }:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf(format, args...)})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	store := s.opt.Deps.Store
	if store == nil {
		httpError(w, http.StatusNotFound, "no session store")
		return
	}
	list, err := store.List()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	var current string
	if err := s.onLoop(r.Context(), func() { current = s.c.Agent().Session().ID }); err != nil {
		return
	}
	if list == nil {
		list = []session.Info{}
	}
	writeJSON(w, map[string]any{"current": current, "sessions": list})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var list []agent.ModelChoice
	var lerr error
	if err := s.onLoop(ctx, func() { list, lerr = s.c.Agent().ModelChoices(ctx) }); err != nil {
		httpError(w, http.StatusGatewayTimeout, "%v", err)
		return
	}
	resp := map[string]any{"models": list}
	if lerr != nil {
		resp["error"] = lerr.Error()
	}
	writeJSON(w, resp)
}

func (s *Server) handleTodos(w http.ResponseWriter, r *http.Request) {
	items := []tools.TodoItem{}
	if t := s.opt.Deps.Todos; t != nil {
		items = t.Items()
	}
	writeJSON(w, map[string]any{"todos": items})
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	var list []agent.PendingAction
	var on bool
	if err := s.onLoop(r.Context(), func() {
		list = s.c.Agent().PendingActions()
		on = s.c.Agent().Plan()
	}); err != nil {
		return
	}
	writeJSON(w, map[string]any{"plan": on, "pending": list})
}

// ---------------------------------------------------------- the workspace

type fileEntry struct {
	Name     string    `json:"name"`
	Dir      bool      `json:"dir"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	Link     bool      `json:"link,omitempty"`
}

func (s *Server) workspace(w http.ResponseWriter) *tools.Workspace {
	ws := s.opt.Deps.WS
	if ws == nil {
		httpError(w, http.StatusNotFound, "no workspace")
	}
	return ws
}

func cleanErr(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "not found"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	}
	return err.Error()
}

// handleFiles lists a directory of the workspace.
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	ws := s.workspace(w)
	if ws == nil {
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" {
		p = "."
	}
	loc, err := ws.Resolve(p, tools.AccessRead)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	des, err := loc.ReadDir()
	if err != nil {
		httpError(w, http.StatusNotFound, "%s: %s", p, cleanErr(err))
		return
	}
	out := []fileEntry{}
	for _, de := range des {
		e := fileEntry{Name: de.Name(), Dir: de.IsDir(), Link: de.Type()&fs.ModeSymlink != 0}
		if info, err := de.Info(); err == nil {
			e.Size, e.Modified = info.Size(), info.ModTime()
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, map[string]any{"path": ws.Rel(loc.Abs), "entries": out})
}

// handleFile returns a workspace file: text as text, an image as itself,
// anything else refused (it is not something to look at in a page).
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	ws := s.workspace(w)
	if ws == nil {
		return
	}
	p := r.URL.Query().Get("path")
	loc, err := ws.ResolveFile(p, tools.AccessRead)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	st, err := loc.Stat()
	if err != nil {
		httpError(w, http.StatusNotFound, "%s: %s", p, cleanErr(err))
		return
	}
	if st.IsDir() {
		httpError(w, http.StatusBadRequest, "%s is a directory", p)
		return
	}
	if st.Size() > maxView {
		httpError(w, http.StatusRequestEntityTooLarge, "%s is %d bytes, more than the page shows (%d); open it in the terminal", p, st.Size(), maxView)
		return
	}
	b, err := loc.ReadFile()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s: %s", p, cleanErr(err))
		return
	}
	// The response is read by the page's script, never opened as a page:
	// sandbox it and forbid guessing the type, whatever the file holds.
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Content-Disposition", "attachment")
	if mime, _, _, err := tools.SniffImage(b); err == nil {
		w.Header().Set("Content-Type", mime)
		w.Write(b)
		return
	}
	if isBinary(b) {
		httpError(w, http.StatusUnsupportedMediaType, "%s is a binary file", p)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(b)
}

func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	for _, c := range b[:n] {
		if c == 0 {
			return true
		}
	}
	return false
}

// handleDiff shows git diff for a path (or the whole workspace).
func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	ws := s.workspace(w)
	if ws == nil {
		return
	}
	args := tools.Args{}
	if p := r.URL.Query().Get("path"); p != "" {
		args["path"] = p
	}
	if r.URL.Query().Get("stat") == "1" {
		args["stat"] = true
	}
	cfg := s.opt.Deps.Cfg
	t := &tools.GitDiff{WS: ws, MaxOutput: cfg.Tools.MaxOutputBytes, Timeout: 30 * time.Second}
	act, err := t.Prepare(r.Context(), args)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	out, err := act.Run(r.Context())
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, tools.Redact(out))
}

// ------------------------------------------------------------- uploads

// uploadName turns a file name from the browser into a safe one: the base
// name only, letters, digits, '.', '-' and '_', no leading dot.
func uploadName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_'):
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "_"):
			b.WriteByte('_')
		}
	}
	out := strings.TrimLeft(b.String(), "._-")
	if len(out) > 100 {
		ext := path.Ext(out)
		if len(ext) > 10 {
			ext = ""
		}
		out = out[:100-len(ext)] + ext
	}
	if out == "" {
		out = "upload"
	}
	return out
}

// handleUpload stores the request body as <workspace>/uploads/<date>-<name>.
// It never overwrites, unpacks or runs anything; the agent sees the file
// like any other in the workspace.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	ws := s.workspace(w)
	if ws == nil {
		return
	}
	limit := s.UploadMax
	if limit <= 0 {
		limit = DefaultUploadMax
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if int64(len(data)) > limit {
		httpError(w, http.StatusRequestEntityTooLarge, "the file is larger than the upload limit (%d bytes, server.upload_max_bytes)", limit)
		return
	}
	base := time.Now().Format("2006-01-02-150405") + "-" + uploadName(r.URL.Query().Get("name"))
	var rel string
	for i := 1; ; i++ {
		rel = "uploads/" + base
		if i > 1 {
			ext := path.Ext(base)
			rel = "uploads/" + strings.TrimSuffix(base, ext) + fmt.Sprintf("-%d", i) + ext
		}
		loc, err := ws.Resolve(rel, tools.AccessWrite)
		if err != nil {
			httpError(w, http.StatusBadRequest, "%v", err)
			return
		}
		if err := loc.MkdirParents(); err != nil {
			httpError(w, http.StatusInternalServerError, "%v", cleanErr(err))
			return
		}
		err = loc.CreateFile(data, 0o644)
		if errors.Is(err, fs.ErrExist) && i < 100 {
			continue
		}
		if err != nil {
			httpError(w, http.StatusInternalServerError, "%v", cleanErr(err))
			return
		}
		rel = ws.Rel(loc.Abs)
		break
	}
	sum := sha256.Sum256(data)
	resp := map[string]any{"path": rel, "size": len(data)}
	if mime, wd, ht, err := tools.SniffImage(data); err == nil {
		resp["image"], resp["mime"], resp["width"], resp["height"] = true, mime, wd, ht
	}
	s.audit.Log(map[string]any{"event": "upload", "remote": r.RemoteAddr, "path": rel, "bytes": len(data), "sha256": hex.EncodeToString(sum[:])})
	writeJSON(w, resp)
}
