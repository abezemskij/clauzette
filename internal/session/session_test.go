package session

import (
	"path/filepath"
	"testing"
)

func TestLoadFallsBackToOlderDirectory(t *testing.T) {
	base := t.TempDir()
	old, err := NewStore(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Save(&Session{ID: "20261001-120000-abcd", Title: "before"}); err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(filepath.Join(base, "clauzette-0"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load("20261001-120000-abcd"); err == nil {
		t.Fatal("found without a fallback")
	}
	st.Fallback = base
	s, err := st.Load("20261001-120000-abcd")
	if err != nil || s.Title != "before" {
		t.Fatalf("fallback load failed: %v", err)
	}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.List(); len(list) != 1 {
		t.Fatal("once saved, the session belongs to the agent's directory")
	}
}
