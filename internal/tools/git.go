package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Read-only git tools. They run the git binary directly (never through a
// shell, so nothing the model passes can be interpreted as a command) with a
// fixed, read-only subcommand. User-supplied refs and paths are rejected if
// they could be read as options, and paths are passed after "--". They are
// ReadOnly, so they run without approval, like read_file and list_dir; a git
// command that changes anything still goes through exec_command.

// gitRunner holds the shared configuration for the git tools.
type gitRunner struct {
	ws        *Workspace
	maxOutput int
	timeout   time.Duration
}

// gitArg validates an optional ref or path: it must not look like an option.
func gitArg(args Args, name string) (string, error) {
	v, err := args.Str(name, false)
	if err != nil {
		return "", err
	}
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "-") {
		return "", fmt.Errorf("%s must not start with '-'", name)
	}
	return v, nil
}

// dir resolves the optional workdir (relative to the workspace root), confined
// like every other path. It returns the absolute directory and its workspace-
// relative name for display.
func (g gitRunner) dir(args Args) (string, string, error) {
	wd, err := args.Str("workdir", false)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(wd) == "" {
		return g.ws.Root, ".", nil
	}
	loc, err := g.ws.Resolve(wd, AccessRead)
	if err != nil {
		return "", "", err
	}
	st, err := loc.Stat()
	if err != nil {
		return "", "", cleanErr(err, g.ws.Rel(loc.Abs))
	}
	if !st.IsDir() {
		return "", "", fmt.Errorf("workdir %s is not a directory", g.ws.Rel(loc.Abs))
	}
	return loc.Abs, g.ws.Rel(loc.Abs), nil
}

func gitAction(g gitRunner, summary, dir string, gitArgs []string) *Action {
	return &Action{
		Summary: summary,
		Preview: "$ git " + strings.Join(gitArgs, " "),
		Run: func(ctx context.Context) (string, error) {
			return g.run(ctx, dir, gitArgs)
		},
	}
}

func (g gitRunner) run(ctx context.Context, dir string, gitArgs []string) (string, error) {
	timeout := g.timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cmd := exec.Command("git", gitArgs...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_PAGER=cat", "PAGER=cat", "GIT_TERMINAL_PROMPT=0", "TERM=dumb", "NO_COLOR=1")
	out := newCappedBuffer(g.maxOutput)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 3 * time.Second

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("could not run git (is it installed and on PATH?): %w", err)
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
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			stopReason = "was stopped because the time limit was reached"
		}
		terminateGroup(pgid, done)
	case <-timer.C:
		stopReason = fmt.Sprintf("timed out after %s", timeout)
		terminateGroup(pgid, done)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)

	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	text := strings.ToValidUTF8(out.String(), "�")
	text = strings.TrimRight(ansiEscape.ReplaceAllString(text, ""), "\n")

	var sb strings.Builder
	switch {
	case stopReason != "":
		fmt.Fprintf(&sb, "git %s.\n", stopReason)
	case code != 0:
		fmt.Fprintf(&sb, "git exited with code %d.\n", code)
	}
	if text == "" {
		if sb.Len() == 0 {
			return "(no output)", nil
		}
		sb.WriteString("(no output)")
	} else {
		sb.WriteString(text)
	}
	return sb.String(), nil
}

// ---------------------------------------------------------------- git_status

type GitStatus struct {
	WS        *Workspace
	MaxOutput int
	Timeout   time.Duration
}

func (t *GitStatus) Name() string { return "git_status" }
func (t *GitStatus) Risk() Risk   { return ReadOnly }

func (t *GitStatus) Description() string {
	return "Show the working-tree status (git status --short --branch): the current branch and the " +
		"files that are staged, modified, deleted or untracked. Read-only; runs without approval."
}

func (t *GitStatus) Parameters() map[string]any {
	return objectSchema(nil, map[string]any{
		"workdir": prop("string", "Directory to run in, relative to the workspace root. Default: the root."),
		"path":    prop("string", "Limit the status to this file or directory (a git pathspec)."),
	})
}

func (t *GitStatus) Prepare(_ context.Context, args Args) (*Action, error) {
	g := gitRunner{t.WS, t.MaxOutput, t.Timeout}
	dir, rel, err := g.dir(args)
	if err != nil {
		return nil, err
	}
	p, err := gitArg(args, "path")
	if err != nil {
		return nil, err
	}
	gitArgs := []string{"status", "--short", "--branch"}
	if p != "" {
		gitArgs = append(gitArgs, "--", p)
	}
	return gitAction(g, "git status in "+rel, dir, gitArgs), nil
}

// ------------------------------------------------------------------ git_diff

type GitDiff struct {
	WS        *Workspace
	MaxOutput int
	Timeout   time.Duration
}

func (t *GitDiff) Name() string { return "git_diff" }
func (t *GitDiff) Risk() Risk   { return ReadOnly }

func (t *GitDiff) Description() string {
	return "Show changes as a unified diff (git diff). By default it shows unstaged changes in the " +
		"working tree; set staged to see what is staged for commit, or give a ref to compare against a " +
		"commit or branch. Read-only; runs without approval."
}

func (t *GitDiff) Parameters() map[string]any {
	return objectSchema(nil, map[string]any{
		"workdir": prop("string", "Directory to run in, relative to the workspace root. Default: the root."),
		"path":    prop("string", "Limit the diff to this file or directory (a git pathspec)."),
		"ref":     prop("string", "A commit, branch or range to compare against (e.g. HEAD~1, main)."),
		"staged":  prop("boolean", "Show staged changes (git diff --cached) instead of unstaged ones. Default false."),
		"stat":    prop("boolean", "Show only a summary of changed files and line counts (--stat). Default false."),
	})
}

func (t *GitDiff) Prepare(_ context.Context, args Args) (*Action, error) {
	g := gitRunner{t.WS, t.MaxOutput, t.Timeout}
	dir, rel, err := g.dir(args)
	if err != nil {
		return nil, err
	}
	p, err := gitArg(args, "path")
	if err != nil {
		return nil, err
	}
	ref, err := gitArg(args, "ref")
	if err != nil {
		return nil, err
	}
	staged, err := args.Bool("staged", false)
	if err != nil {
		return nil, err
	}
	stat, err := args.Bool("stat", false)
	if err != nil {
		return nil, err
	}
	gitArgs := []string{"diff", "--no-color", "-M"}
	if stat {
		gitArgs = append(gitArgs, "--stat")
	}
	if staged {
		gitArgs = append(gitArgs, "--cached")
	}
	if ref != "" {
		gitArgs = append(gitArgs, ref)
	}
	if p != "" {
		gitArgs = append(gitArgs, "--", p)
	}
	return gitAction(g, "git diff in "+rel, dir, gitArgs), nil
}

// ------------------------------------------------------------------- git_log

type GitLog struct {
	WS        *Workspace
	MaxOutput int
	Timeout   time.Duration
}

func (t *GitLog) Name() string { return "git_log" }
func (t *GitLog) Risk() Risk   { return ReadOnly }

func (t *GitLog) Description() string {
	return "Show recent commit history, most recent first (git log), one line per commit as " +
		"\"<hash> <date> <author>  <subject>\". Read-only; runs without approval."
}

func (t *GitLog) Parameters() map[string]any {
	return objectSchema(nil, map[string]any{
		"workdir": prop("string", "Directory to run in, relative to the workspace root. Default: the root."),
		"path":    prop("string", "Limit the history to commits touching this file or directory."),
		"ref":     prop("string", "Start from this commit or branch instead of the current HEAD."),
		"max":     prop("integer", "Maximum number of commits to show. Default 20, maximum 200."),
		"stat":    prop("boolean", "Also show the files changed in each commit (--stat). Default false."),
	})
}

func (t *GitLog) Prepare(_ context.Context, args Args) (*Action, error) {
	g := gitRunner{t.WS, t.MaxOutput, t.Timeout}
	dir, rel, err := g.dir(args)
	if err != nil {
		return nil, err
	}
	p, err := gitArg(args, "path")
	if err != nil {
		return nil, err
	}
	ref, err := gitArg(args, "ref")
	if err != nil {
		return nil, err
	}
	max, err := args.Int("max", 20)
	if err != nil {
		return nil, err
	}
	if max < 1 {
		max = 20
	}
	if max > 200 {
		max = 200
	}
	stat, err := args.Bool("stat", false)
	if err != nil {
		return nil, err
	}
	gitArgs := []string{"log", "--no-color", "--date=short",
		fmt.Sprintf("--max-count=%d", max), "--pretty=format:%h %ad %an  %s"}
	if stat {
		gitArgs = append(gitArgs, "--stat")
	}
	if ref != "" {
		gitArgs = append(gitArgs, ref)
	}
	if p != "" {
		gitArgs = append(gitArgs, "--", p)
	}
	return gitAction(g, "git log in "+rel, dir, gitArgs), nil
}
