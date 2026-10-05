package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

type ExecCommand struct {
	WS             *Workspace
	Shell          string
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
	MaxOutput      int
}

func (t *ExecCommand) Name() string { return "exec_command" }
func (t *ExecCommand) Risk() Risk   { return Exec }

func (t *ExecCommand) Description() string {
	return "Run a shell command and return its exit code and combined stdout/stderr. Runs in the " +
		"workspace root unless workdir is given, without a terminal and without input: anything that " +
		"prompts or waits for input will fail. Use it for builds, tests, git and other CLI tools. " +
		"Long output is shortened in the middle. Background processes are stopped when the command ends. " +
		"Requires operator approval."
}

func (t *ExecCommand) Parameters() map[string]any {
	return objectSchema([]string{"command"}, map[string]any{
		"command":         prop("string", "The shell command to run."),
		"workdir":         prop("string", "Directory to run in, relative to the workspace root. Default: the root."),
		"timeout_seconds": prop("integer", fmt.Sprintf("Time limit in seconds. Default %d, maximum %d.", int(t.DefaultTimeout.Seconds()), int(t.MaxTimeout.Seconds()))),
	})
}

func (t *ExecCommand) Prepare(_ context.Context, args Args) (*Action, error) {
	command, err := args.Str("command", true)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(command) == "" {
		return nil, fmt.Errorf("command is empty")
	}
	secs, err := args.Int("timeout_seconds", int(t.DefaultTimeout.Seconds()))
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(secs) * time.Second
	if timeout <= 0 {
		timeout = t.DefaultTimeout
	}
	if timeout > t.MaxTimeout {
		timeout = t.MaxTimeout
	}
	wd, err := args.Str("workdir", false)
	if err != nil {
		return nil, err
	}
	dir := t.WS.Root
	if wd != "" {
		if dir, err = t.WS.Resolve(wd); err != nil {
			return nil, err
		}
		st, err := os.Stat(dir)
		if err != nil {
			return nil, cleanErr(err, t.WS.Rel(dir))
		}
		if !st.IsDir() {
			return nil, fmt.Errorf("workdir %s is not a directory", t.WS.Rel(dir))
		}
	}
	return &Action{
		Summary: fmt.Sprintf("run in %s (timeout %s)", t.WS.Rel(dir), timeout),
		Preview: "$ " + command,
		Run: func(ctx context.Context) (string, error) {
			return t.run(ctx, command, dir, timeout)
		},
	}, nil
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func (t *ExecCommand) run(ctx context.Context, command, dir string, timeout time.Duration) (string, error) {
	cmd := exec.Command(t.Shell, "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PAGER=cat", "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0",
		"TERM=dumb", "NO_COLOR=1", "CI=1", "DEBIAN_FRONTEND=noninteractive")
	out := newCappedBuffer(t.MaxOutput)
	cmd.Stdout = out
	cmd.Stderr = out
	// stdin stays nil, i.e. /dev/null. Own process group, so the whole tree
	// (the shell and everything it started) can be signalled at once.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// If the command exits but a leftover child keeps the output pipe open,
	// stop waiting for it after a few seconds.
	cmd.WaitDelay = 3 * time.Second

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("could not start %s: %w", t.Shell, err)
	}
	pgid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	stopReason := ""
	select {
	case <-done:
	case <-ctx.Done():
		stopReason = "was cancelled by the operator"
		terminateGroup(pgid, done)
	case <-timer.C:
		stopReason = fmt.Sprintf("timed out after %s and was stopped", timeout)
		terminateGroup(pgid, done)
	}
	// Clean up anything the command left running in the background.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)

	elapsed := time.Since(start).Round(100 * time.Millisecond)
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	var sb strings.Builder
	if stopReason != "" {
		fmt.Fprintf(&sb, "The command %s (%s).\n", stopReason, elapsed)
	} else {
		fmt.Fprintf(&sb, "Exit code %d (%s).\n", code, elapsed)
	}
	text := strings.ToValidUTF8(out.String(), "\uFFFD")
	text = strings.TrimRight(ansiEscape.ReplaceAllString(text, ""), "\n")
	if text == "" {
		sb.WriteString("(no output)")
	} else {
		sb.WriteString("Output:\n")
		sb.WriteString(text)
	}
	return sb.String(), nil
}

// terminateGroup asks the process group to stop, then kills it if it has
// not exited within a few seconds.
func terminateGroup(pgid int, done <-chan error) {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-done
	}
}

// cappedBuffer keeps the first and last limit/2 bytes of the output and
// counts what it dropped in between.
type cappedBuffer struct {
	mu    sync.Mutex
	limit int
	head  []byte
	tail  []byte
	total int
}

func newCappedBuffer(limit int) *cappedBuffer {
	if limit < 1000 {
		limit = 1000
	}
	return &cappedBuffer{limit: limit}
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	c.total += n
	half := c.limit / 2
	if len(c.head) < half {
		k := half - len(c.head)
		if k > len(p) {
			k = len(p)
		}
		c.head = append(c.head, p[:k]...)
		p = p[k:]
	}
	if len(p) > 0 {
		c.tail = append(c.tail, p...)
		if len(c.tail) > half {
			c.tail = append([]byte(nil), c.tail[len(c.tail)-half:]...)
		}
	}
	return n, nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := len(c.head) + len(c.tail)
	if c.total <= kept {
		return string(c.head) + string(c.tail)
	}
	return string(c.head) + fmt.Sprintf("\n[… %d bytes omitted …]\n", c.total-kept) + string(c.tail)
}
