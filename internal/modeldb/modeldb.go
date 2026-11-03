// Package modeldb keeps what clauzette learns about each model across
// sessions, in one JSON file (config models_file). Records are keyed by the
// model's weights digest, so models that share weights (variants that only
// change Modelfile parameters) share one record.
//
// Several clauzette processes may use the same file (two kubectl exec
// sessions in one pod). Save re-reads the file and only replaces the records
// this process changed, and writes atomically, so a crash or a concurrent
// writer cannot leave a broken file; the last writer of a record wins.
package modeldb

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Record is what is known about one set of weights.
type Record struct {
	Names         []string  `json:"names"`                     // model names seen with these weights
	CharsPerToken float64   `json:"chars_per_token,omitempty"` // calibrated estimate, see agent.calibrate
	Updated       time.Time `json:"updated"`

	// The largest context that stays fully on the GPU (/adapt), used by
	// profiles with num_ctx "fit". FitSource is "adapt" (measured) or "set"
	// (pinned by the operator). Stale, when set, says why the value no longer
	// applies to this setup; it is then not used until /adapt runs again.
	FitCtx    int       `json:"fit_ctx,omitempty"`
	FitSource string    `json:"fit_source,omitempty"`
	Stale     string    `json:"stale,omitempty"`
	AdaptedAt time.Time `json:"adapted_at,omitempty"`

	// Measured by /adapt: KV-cache bytes per context token (the difference
	// between two loads), the model's loaded size at FitCtx, and the GPU it
	// was measured on. They let later loads detect a changed setup.
	KVBytesPerToken float64 `json:"kv_bytes_per_token,omitempty"`
	SizeAtFit       int64   `json:"size_at_fit,omitempty"`
	VRAMUsedAtFit   int64   `json:"vram_used_at_fit,omitempty"`
	VRAMTotal       int64   `json:"vram_total,omitempty"`
	Ollama          string  `json:"ollama,omitempty"`

	// Measured by /adapt --verify at about 90% of FitCtx.
	VerifiedTokens int     `json:"verified_tokens,omitempty"`
	PrefillTPS     float64 `json:"prefill_tps,omitempty"`
	GenTPS         float64 `json:"gen_tps,omitempty"`
}

// ClearFit drops the context fit and its measurements, keeping the names and
// the token calibration.
func (r *Record) ClearFit() {
	names, cpt := r.Names, r.CharsPerToken
	*r = Record{Names: names, CharsPerToken: cpt}
}

type file struct {
	Models map[string]*Record `json:"models"`
}

// DB is the in-memory copy of the file. The zero value and a nil *DB are
// usable and keep nothing on disk.
type DB struct {
	path  string
	mu    sync.Mutex
	recs  map[string]*Record
	dirty map[string]bool
}

// Open reads the file at path; a missing file is an empty database. A file
// that cannot be parsed is reported, and the database starts empty (it is
// replaced on the next Save).
func Open(path string) (*DB, error) {
	db := &DB{path: path, recs: map[string]*Record{}, dirty: map[string]bool{}}
	recs, err := read(path)
	if err != nil {
		return db, fmt.Errorf("models file %s: %w (starting with an empty one)", path, err)
	}
	db.recs = recs
	return db, nil
}

func read(path string) (map[string]*Record, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]*Record{}, nil
	}
	if err != nil {
		return map[string]*Record{}, err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return map[string]*Record{}, err
	}
	if f.Models == nil {
		f.Models = map[string]*Record{}
	}
	return f.Models, nil
}

// Get returns a copy of the record for key.
func (db *DB) Get(key string) (Record, bool) {
	if db == nil || key == "" {
		return Record{}, false
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	r, ok := db.recs[key]
	if !ok {
		return Record{}, false
	}
	out := *r
	out.Names = append([]string(nil), r.Names...)
	return out, true
}

// KeyedRecord is a record with its key.
type KeyedRecord struct {
	Key    string
	Record Record
}

// All returns copies of all records, sorted by their first model name.
func (db *DB) All() []KeyedRecord {
	if db == nil {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	out := make([]KeyedRecord, 0, len(db.recs))
	for k, r := range db.recs {
		c := *r
		c.Names = append([]string(nil), r.Names...)
		out = append(out, KeyedRecord{Key: k, Record: c})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := "", ""
		if len(out[i].Record.Names) > 0 {
			a = out[i].Record.Names[0]
		}
		if len(out[j].Record.Names) > 0 {
			b = out[j].Record.Names[0]
		}
		return a < b
	})
	return out
}

// KeyFor returns the key of the record that lists this model name, or "".
func (db *DB) KeyFor(name string) string {
	if db == nil || name == "" {
		return ""
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	for k, r := range db.recs {
		if contains(r.Names, name) {
			return k
		}
	}
	return ""
}

// Update changes the record for key (creating it), records name as one of
// its model names, and marks it for the next Save.
func (db *DB) Update(key, name string, change func(*Record)) {
	if db == nil || key == "" {
		return
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.recs == nil {
		db.recs, db.dirty = map[string]*Record{}, map[string]bool{}
	}
	r := db.recs[key]
	if r == nil {
		r = &Record{}
		db.recs[key] = r
	}
	if name != "" && !contains(r.Names, name) {
		r.Names = append(r.Names, name)
		sort.Strings(r.Names)
	}
	change(r)
	r.Updated = time.Now().UTC()
	db.dirty[key] = true
}

// Save writes the changed records. Records other processes wrote in the
// meantime are kept.
func (db *DB) Save() error {
	if db == nil || db.path == "" {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if len(db.dirty) == 0 {
		return nil
	}
	onDisk, err := read(db.path)
	if err != nil {
		onDisk = map[string]*Record{} // unreadable: replace it with what we know
	}
	for key := range db.dirty {
		onDisk[key] = db.recs[key]
	}
	b, err := json.MarshalIndent(file{Models: onDisk}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(db.path), 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", db.path, os.Getpid())
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, db.path); err != nil {
		os.Remove(tmp)
		return err
	}
	db.recs = onDisk
	db.dirty = map[string]bool{}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
