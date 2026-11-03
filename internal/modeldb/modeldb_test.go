package modeldb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMissingFileIsEmpty(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := db.Get("k"); ok {
		t.Fatal("empty database returned a record")
	}
	if err := db.Save(); err != nil { // nothing changed: no file needed
		t.Fatal(err)
	}
}

func TestSaveKeepsOtherWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	a, _ := Open(path)
	b, _ := Open(path) // a second clauzette process

	a.Update("w1", "qwen", func(r *Record) { r.CharsPerToken = 3.4 })
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	b.Update("w2", "gemma", func(r *Record) { r.CharsPerToken = 3.9 })
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}

	c, _ := Open(path)
	r1, ok1 := c.Get("w1")
	r2, ok2 := c.Get("w2")
	if !ok1 || !ok2 || r1.CharsPerToken != 3.4 || r2.CharsPerToken != 3.9 {
		t.Fatalf("a writer's record was lost: %+v %+v", r1, r2)
	}
}

func TestNamesShareOneRecord(t *testing.T) {
	db, _ := Open(filepath.Join(t.TempDir(), "models.json"))
	db.Update("w", "qwen3.8:latest", func(*Record) {})
	db.Update("w", "qwen3.8-65k:latest", func(*Record) {})
	db.Update("w", "qwen3.8:latest", func(*Record) {})
	r, _ := db.Get("w")
	if len(r.Names) != 2 {
		t.Fatalf("names = %v", r.Names)
	}
}

func TestCorruptFileIsReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	db, err := Open(path)
	if err == nil {
		t.Fatal("a corrupt file should be reported")
	}
	db.Update("w", "m", func(r *Record) { r.CharsPerToken = 3 })
	if err := db.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatalf("the file was not replaced: %v", err)
	}
	if r, ok := again.Get("w"); !ok || r.CharsPerToken != 3 {
		t.Fatalf("record = %+v", r)
	}
}

func TestNilDB(t *testing.T) {
	var db *DB
	db.Update("w", "m", func(r *Record) { r.CharsPerToken = 3 })
	if _, ok := db.Get("w"); ok {
		t.Fatal("a nil DB returned a record")
	}
	if err := db.Save(); err != nil {
		t.Fatal(err)
	}
}
