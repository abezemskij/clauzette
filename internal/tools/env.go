package tools

import (
	"os"
	"strings"
	"sync/atomic"
	"syscall"
)

// prSetDumpable is PR_SET_DUMPABLE from <linux/prctl.h>.
const prSetDumpable = 4

// ProtectProcess marks the process non-dumpable. Its /proc/<pid> files then
// belong to root, so a command running as the same user can no longer read
// its environment (/proc/<pid>/environ, where API keys are) or memory, or
// attach a debugger. It also disables core dumps. Children start dumpable
// again after exec, which is fine: they do not get the keys (HideEnv).
func ProtectProcess() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); e != 0 {
		return e
	}
	return nil
}

// hiddenEnv holds the environment variables that commands must not inherit
// (API keys). Set once at start with HideEnv.
var hiddenEnv atomic.Pointer[map[string]bool]

// secretValues maps the values of the hidden variables (as they were at
// HideEnv) to their names, for Redact.
var secretValues atomic.Pointer[map[string]string]

// minSecretLen is the shortest value Redact replaces; shorter ones would
// match ordinary text.
const minSecretLen = 8

// HideEnv keeps the named variables out of the environment of every
// command the tools start (exec_command, git), so an `env` or `printenv`
// cannot put an API key into the conversation. It replaces the earlier set.
//
// Together with ProtectProcess, which keeps commands from reading the
// environment from /proc/<pid>/environ, this keeps the keys from commands;
// Redact catches a key that turns up anyway (a .env file in the workspace).
func HideEnv(names ...string) {
	m := make(map[string]bool, len(names))
	vals := map[string]string{}
	for _, n := range names {
		if n != "" {
			m[n] = true
			if v := os.Getenv(n); len(v) >= minSecretLen {
				vals[v] = n
			}
		}
	}
	hiddenEnv.Store(&m)
	secretValues.Store(&vals)
}

// Redact replaces the values of the hidden variables in s with
// "[redacted NAME]". Tool output goes through it before it reaches the
// conversation, the session file and the audit log, so a key that a command
// or file read turns up (a .env file, /proc) is not stored or sent.
func Redact(s string) string {
	p := secretValues.Load()
	if p == nil {
		return s
	}
	for v, n := range *p {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, "[redacted "+n+"]")
		}
	}
	return s
}

// childEnv is the environment for a command: clauzette's own without the
// hidden variables, plus extra ("KEY=value" entries).
func childEnv(extra ...string) []string {
	var hidden map[string]bool
	if p := hiddenEnv.Load(); p != nil {
		hidden = *p
	}
	env := os.Environ()
	out := make([]string, 0, len(env)+len(extra))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !hidden[name] {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}
