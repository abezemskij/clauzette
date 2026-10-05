// Package session holds a conversation and saves it as a JSON file.
//
// A session keeps two message lists: Context is the working context that
// is sent to the model (and gets compacted), Transcript is the complete,
// append-only record that is never shortened.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"clauzette/internal/ollama"
)

// Entry kinds mark messages the harness wrote, as opposed to the operator or the model.
const (
	KindSummary     = "summary"     // compaction summary (role user)
	KindAck         = "ack"         // assistant acknowledgement after a summary
	KindInterrupted = "interrupted" // assistant reply cut short by cancellation or an error
	KindCancelled   = "cancelled"   // tool call that never ran
	KindNote        = "note"        // transcript-only remark, never sent to the model
)

type Entry struct {
	ollama.Message
	Time time.Time `json:"time"`
	Kind string    `json:"kind,omitempty"`
}

type Session struct {
	ID            string    `json:"id"`
	Title         string    `json:"title,omitempty"`
	Created       time.Time `json:"created"`
	Updated       time.Time `json:"updated"`
	Model         string    `json:"model"`
	Date          string    `json:"date"`   // frozen at creation so the system prompt never changes mid-session
	System        string    `json:"system"` // the assembled system prompt, frozen for the same reason
	Think         bool      `json:"think"`
	ShowThinking  bool      `json:"show_thinking"`
	Safe          bool      `json:"safe"` // safe mode: back up before destructive actions
	Plan          bool      `json:"plan"` // plan mode: capture actions instead of running them
	AlwaysApprove []string  `json:"always_approve,omitempty"`
	CharsPerToken float64   `json:"chars_per_token,omitempty"`
	Compactions   int       `json:"compactions"`

	LastPromptTokens int `json:"last_prompt_tokens"`
	LastEvalTokens   int `json:"last_eval_tokens"`

	Context    []Entry `json:"context"`
	Transcript []Entry `json:"transcript"`
}

// Append adds a message to both the working context and the transcript.
func (s *Session) Append(m ollama.Message, kind string) {
	e := Entry{Message: m, Time: time.Now(), Kind: kind}
	s.Context = append(s.Context, e)
	s.Transcript = append(s.Transcript, e)
}

// Note records something in the transcript only.
func (s *Session) Note(text string) {
	s.Transcript = append(s.Transcript, Entry{
		Message: ollama.Message{Role: "note", Content: text},
		Time:    time.Now(),
		Kind:    KindNote,
	})
}

// NewID returns a sortable, practically unique session id.
func NewID() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

type Store struct {
	Dir string
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

func (st *Store) path(id string) string { return filepath.Join(st.Dir, id+".json") }

// Save writes the session atomically (temporary file + rename).
func (st *Store) Save(s *Session) error {
	s.Updated = time.Now()
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := st.path(s.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, st.path(s.ID))
}

func (st *Store) Load(id string) (*Session, error) {
	id = strings.TrimSuffix(strings.TrimSpace(id), ".json")
	if id == "" || strings.ContainsAny(id, `/\`) {
		return nil, fmt.Errorf("invalid session id %q", id)
	}
	b, err := os.ReadFile(st.path(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no session %q in %s", id, st.Dir)
		}
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("session %s: %w", id, err)
	}
	return &s, nil
}

type Info struct {
	ID       string
	Title    string
	Updated  time.Time
	Messages int
}

// List returns saved sessions, most recently updated first.
func (st *Store) List() ([]Info, error) {
	entries, err := os.ReadDir(st.Dir)
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(st.Dir, e.Name()))
		if err != nil {
			continue
		}
		var h struct {
			ID         string            `json:"id"`
			Title      string            `json:"title"`
			Updated    time.Time         `json:"updated"`
			Transcript []json.RawMessage `json:"transcript"`
		}
		if json.Unmarshal(b, &h) != nil || h.ID == "" {
			continue
		}
		out = append(out, Info{ID: h.ID, Title: h.Title, Updated: h.Updated, Messages: len(h.Transcript)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// Latest returns the id of the most recently updated session.
func (st *Store) Latest() (string, error) {
	list, err := st.List()
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "", fmt.Errorf("no saved sessions in %s", st.Dir)
	}
	return list[0].ID, nil
}
