package session

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestLock(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	unlock, err := st.Lock("s1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Lock("s1"); err == nil || !strings.Contains(err.Error(), "already open in this clauzette") {
		t.Fatalf("second lock by the same process: %v", err)
	}
	unlock()
	if _, err := os.Stat(st.lockPath("s1")); !os.IsNotExist(err) {
		t.Fatal("unlock left the lock file")
	}

	// another live process on this host holds it (pid 1 always exists)
	os.WriteFile(st.lockPath("s2"), []byte(fmt.Sprintf("1 %s\n", hostname())), 0o600)
	var le *LockedError
	if _, err := st.Lock("s2"); !errors.As(err, &le) || le.PID != 1 {
		t.Fatalf("lock held by a live process: %v", err)
	}
	// another host: cannot be checked, so it is respected
	os.WriteFile(st.lockPath("s3"), []byte("12345 some-other-host\n"), 0o600)
	if _, err := st.Lock("s3"); !errors.As(err, &le) || le.Host != "some-other-host" {
		t.Fatalf("lock from another host: %v", err)
	}
	// a process that is gone: taken over
	os.WriteFile(st.lockPath("s4"), []byte(fmt.Sprintf("%d %s\n", 1<<22+12345, hostname())), 0o600)
	unlock4, err := st.Lock("s4")
	if err != nil {
		t.Fatalf("stale lock not taken over: %v", err)
	}
	// unlock must not remove a lock someone else took meanwhile
	os.WriteFile(st.lockPath("s4"), []byte("1 elsewhere\n"), 0o600)
	unlock4()
	if _, err := os.Stat(st.lockPath("s4")); err != nil {
		t.Fatal("unlock removed another owner's lock")
	}
	if list, _ := st.List(); len(list) != 0 {
		t.Fatalf("lock files listed as sessions: %v", list)
	}

	// with TakeOver, another host's lock is taken (a previous pod); a live
	// process on this host still is not
	st.TakeOver = true
	unlock3, err := st.Lock("s3")
	if err != nil {
		t.Fatalf("take-over of another host's lock: %v", err)
	}
	unlock3()
	if _, err := st.Lock("s2"); !errors.As(err, &le) {
		t.Fatalf("take-over must not take a live local process's lock: %v", err)
	}
}
