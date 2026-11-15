package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// A session is open in at most one clauzette at a time (a terminal, or the
// server), so two of them never write the same session file. The lock is
// a file next to the session, <id>.lock, holding the owner's process id and
// host. A lock whose process is gone (same host) is taken over; one from
// another host cannot be checked and is reported.

// LockedError says who holds a session.
type LockedError struct {
	ID   string
	PID  int
	Host string
	Path string
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("session %s is open in another clauzette (process %d on %s); attach to it, close it there, or delete %s if that process is gone",
		e.ID, e.PID, e.Host, e.Path)
}

func (st *Store) lockPath(id string) string { return filepath.Join(st.Dir, id+".lock") }

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func readLock(path string) (pid int, host string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, "", err
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return 0, "", errors.New("malformed lock file")
	}
	pid, err = strconv.Atoi(f[0])
	return pid, f[1], err
}

// alive reports whether a process exists (signal 0 checks without sending).
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Lock takes the session for this process and returns the function that
// releases it.
func (st *Store) Lock(id string) (unlock func(), err error) {
	if id == "" || strings.ContainsAny(id, `/\`) {
		return nil, fmt.Errorf("invalid session id %q", id)
	}
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		return nil, err
	}
	path := st.lockPath(id)
	me := fmt.Sprintf("%d %s\n", os.Getpid(), hostname())
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, werr := f.WriteString(me)
			cerr := f.Close()
			if werr == nil {
				werr = cerr
			}
			if werr != nil {
				os.Remove(path)
				return nil, werr
			}
			return func() {
				// remove only our own lock
				if b, err := os.ReadFile(path); err == nil && string(b) == me {
					os.Remove(path)
				}
			}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		pid, host, rerr := readLock(path)
		switch {
		case rerr != nil && errors.Is(rerr, os.ErrNotExist):
			continue // released meanwhile
		case rerr != nil:
			os.Remove(path) // a malformed lock: nobody can own it
		case pid == os.Getpid() && host == hostname():
			return nil, fmt.Errorf("session %s is already open in this clauzette", id)
		case host == hostname() && !alive(pid):
			os.Remove(path) // left behind by a process that ended without releasing it
		case host != hostname() && st.TakeOver:
			os.Remove(path) // the previous pod's (see Store.TakeOver)
		default:
			return nil, &LockedError{ID: id, PID: pid, Host: host, Path: path}
		}
	}
	return nil, fmt.Errorf("could not lock session %s", id)
}
