// Command clauzette is a terminal AI agent for a local Ollama model.
//
//	clauzette               start a new session
//	clauzette -resume       continue the most recent session
//	clauzette -session ID   continue a specific session
//	clauzette -profile P    use model profile P (default: the session's, else default_profile)
//	clauzette -sessions     list saved sessions
//	clauzette serve [flags] run the session in a server (same flags); clients attach to it
//	clauzette attach        attach this terminal to a running server (-server URL)
//	clauzette idle          keep a container running (pod entrypoint)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"clauzette/internal/agent"
	"clauzette/internal/config"
	"clauzette/internal/gpu"
	"clauzette/internal/modeldb"
	"clauzette/internal/ollama"
	"clauzette/internal/server"
	"clauzette/internal/session"
	"clauzette/internal/textutil"
	"clauzette/internal/tools"
	"clauzette/internal/ui"
	"clauzette/internal/wire"
)

var version = "dev"

func main() {
	// First, before anything else runs: commands the model starts run as
	// this user, and could otherwise read this process's environment (API
	// keys) from /proc/<pid>/environ.
	if err := tools.ProtectProcess(); err != nil {
		fmt.Fprintf(os.Stderr, "clauzette: warning: could not make the process non-dumpable (%v); commands may be able to read its environment\n", err)
	}
	if len(os.Args) > 1 && os.Args[1] == "idle" {
		idle()
		return
	}
	mode, args := "terminal", os.Args[1:]
	if len(args) > 0 && (args[0] == "serve" || args[0] == "attach") {
		mode, args = args[0], args[1:]
	}
	cfgFlag := flag.String("config", "", "config file (default: $CLAUZETTE_CONFIG, ./clauzette.json or /etc/clauzette/config.json)")
	resume := flag.Bool("resume", false, "resume the most recent session")
	sessionID := flag.String("session", "", "resume the session with this id")
	profile := flag.String("profile", "", "model profile to use (default: the resumed session's, else default_profile)")
	list := flag.Bool("sessions", false, "list saved sessions and exit")
	printCfg := flag.Bool("print-config", false, "print the effective configuration and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	serverURL := flag.String("server", "", "attach: the server's URL (default: $CLAUZETTE_SERVER, else http://<server.listen>)")
	takeOver := flag.Bool("take-over", false, "take the session even if another host holds its lock (in a pod: the previous pod)")
	flag.CommandLine.Parse(args)

	if *showVersion {
		fmt.Println("clauzette", version)
		return
	}
	var err error
	if mode == "attach" {
		err = attach(*cfgFlag, *serverURL)
	} else {
		err = run(mode, *cfgFlag, *resume, *sessionID, *profile, *list, *printCfg, *takeOver)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "clauzette:", err)
		os.Exit(1)
	}
}

func run(mode, cfgFlag string, resume bool, sessionID, profileFlag string, list, printCfg, takeOver bool) error {
	cfg, err := config.Load(config.ResolvePath(cfgFlag))
	if err != nil {
		return err
	}
	if printCfg {
		b, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	store, err := session.NewStore(cfg.AgentSessionsDir())
	if err != nil {
		return fmt.Errorf("sessions directory: %w", err)
	}
	if cfg.AgentDir {
		store.Fallback = cfg.SessionsDir // sessions saved before per-agent directories
	}
	store.TakeOver = takeOver
	if list {
		return listSessions(store)
	}
	wsOpt := tools.WorkspaceOptions{Share: cfg.Workspace}
	if cfg.AgentDir {
		wsOpt.AgentDir = cfg.AgentID
		if wsOpt.Shared, err = tools.ParseAccess(cfg.SharedAccess); err != nil {
			return err
		}
	}
	ws, err := tools.OpenWorkspace(wsOpt)
	if err != nil {
		if cfg.AgentDir {
			return fmt.Errorf("%w (agent_id %q, from the config, $CLAUZETTE_AGENT_ID or the hostname)", err, cfg.AgentID)
		}
		return err
	}
	defer ws.Close()
	todos := tools.NewTodoList()
	fetchPolicy := tools.NewFetchPolicy(cfg.Tools.FetchAllowlist, cfg.Tools.FetchAllowedHosts)
	images := tools.NewImageStore(store.ImageDir(), cfg.Tools.ImageMaxBytes)
	reg := buildRegistry(cfg, ws, todos, fetchPolicy, images)
	client := ollama.New(cfg.OllamaURL, ollama.Timeouts{
		Connect:    secs(cfg.Timeouts.ConnectSeconds),
		FirstToken: secs(cfg.Timeouts.FirstTokenSeconds),
		InterToken: secs(cfg.Timeouts.InterTokenSeconds),
		Load:       secs(cfg.Timeouts.LoadSeconds),
	})

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	guard := gpu.New(cfg.GPU)
	guard.Start(ctx)

	var notes []string
	audit, err := agent.OpenAudit(cfg.AuditLog)
	if err != nil {
		notes = append(notes, fmt.Sprintf("audit log disabled: %v", err))
		audit = nil
	} else if cfg.AgentDir {
		audit.Agent = cfg.AgentID
	}
	defer audit.Close()

	// A resumed session is loaded before anything else: its profile decides
	// which model to use, unless -profile says otherwise.
	var sess *session.Session
	resumed := false
	switch {
	case sessionID != "":
		sess, err = store.Load(sessionID)
		resumed = true
	case resume:
		var id string
		if id, err = store.Latest(); err == nil {
			sess, err = store.Load(id)
			resumed = true
		} else if errors.Is(err, session.ErrNoSessions) {
			err = nil // nothing to resume yet (a new state volume): start a new session
			notes = append(notes, "no saved session to resume; started a new one")
		}
	}
	if err != nil {
		return err
	}
	profile, pnotes, err := agent.SelectProfile(cfg, profileFlag, sess)
	if err != nil {
		return err
	}
	notes = append(notes, pnotes...)
	db, err := modeldb.Open(cfg.ModelsFile)
	if err != nil {
		notes = append(notes, err.Error())
	}
	models := agent.NewModels(cfg, db, profile)

	deps := agent.Deps{Cfg: cfg, Client: client, Models: models, Registry: reg, Store: store, Guard: guard, GPUMem: gpu.NewMemory(cfg.GPU), Audit: audit, WS: ws, Todos: todos, Fetch: fetchPolicy, Images: images}
	// buildPrompts assembles the three system prompts (normal, rogue mode,
	// and chat-only for a model without tools), so a session can switch
	// without rebuilding.
	buildPrompts := func(date string) (agent.Prompts, []string, error) {
		act := models.Active()
		data := agent.PromptData{
			Date:         date,
			Workspace:    ws.Root,
			Share:        shareFor(ws),
			SharedAccess: ws.Access.String(),
			Model:        act.Model,
			Shell:        cfg.Tools.Shell,
			Tools:        reg.Names(),
			Internet:     cfg.Prompt.InternetAccess,
			NumCtx:       act.NumCtx,
		}
		var p agent.Prompts
		var notes []string
		seen := map[string]bool{}
		for _, v := range []struct {
			label       string
			rogue, chat bool
			out         *string
		}{{"", false, false, &p.Normal}, {"rogue mode: ", true, false, &p.Rogue}, {"chat-only: ", false, true, &p.Chat}} {
			data.Rogue, data.Chat = v.rogue, v.chat
			text, n, err := agent.BuildSystemPrompt(cfg, data)
			if err != nil {
				return agent.Prompts{}, nil, err
			}
			*v.out = text
			for _, s := range n {
				if !seen[s] {
					seen[s] = true
					notes = append(notes, v.label+s)
				}
			}
		}
		return p, notes, nil
	}
	newSession := func() (*session.Session, []string, error) {
		// The date is fixed when the session starts: a value that changes
		// inside the system prompt would defeat Ollama's prompt cache.
		date := time.Now().Format("Monday, 2 January 2006")
		p, n, err := buildPrompts(date)
		if err != nil {
			return nil, nil, err
		}
		act := models.Active()
		now := time.Now()
		s := &session.Session{
			ID: session.NewID(), Created: now, Updated: now,
			Profile: act.Profile, Model: act.Model, Date: date,
			System: p.Normal, SystemRogue: p.Rogue, SystemChat: p.Chat,
			Think: act.Think, ShowThinking: cfg.ShowThinking,
		}
		return s, n, store.Save(s)
	}
	if sess == nil {
		var n []string
		if sess, n, err = newSession(); err != nil {
			return err
		}
		notes = append(notes, n...)
	}

	unlock, err := store.Lock(sess.ID)
	if err != nil {
		return err
	}
	defer unlock() // after an error; the controller releases it on a normal exit
	opts := ui.Options{
		Deps:         deps,
		NewSession:   newSession,
		BuildPrompts: buildPrompts,
		Notes:        notes,
		Resumed:      resumed,
		Unlock:       unlock,
	}
	if mode == "serve" {
		return serve(ctx, cfg, opts, sess)
	}
	return ui.New(opts, sess).Run(ctx)
}

// shareFor is the shared workspace root for the prompt, "" when the agent
// works in the whole workspace.
func shareFor(ws *tools.Workspace) string {
	if ws.HasAgentDir() {
		return ws.Share
	}
	return ""
}

func buildRegistry(cfg *config.Config, ws *tools.Workspace, todos *tools.TodoList, fetchPolicy *tools.FetchPolicy, images *tools.ImageStore) *tools.Registry {
	t := cfg.Tools
	tools.HideEnv(append(cfg.KeyEnvNames(), wire.TokenEnv)...) // API keys and the server token stay out of commands
	all := []tools.Tool{
		&tools.ReadFile{WS: ws, DefaultLines: t.ReadDefaultLines, MaxBytes: t.ReadMaxFileBytes},
		&tools.ListDir{WS: ws},
		&tools.SearchFiles{WS: ws},
		&tools.WriteFile{WS: ws},
		&tools.EditFile{WS: ws},
		&tools.ApplyPatch{WS: ws},
		&tools.MvFile{WS: ws},
		&tools.DeleteFile{WS: ws},
		&tools.ExecCommand{
			WS:             ws,
			Shell:          t.Shell,
			DefaultTimeout: secs(t.ExecDefaultTimeoutSec),
			MaxTimeout:     secs(t.ExecMaxTimeoutSec),
			MaxOutput:      t.MaxOutputBytes,
		},
		&tools.GitStatus{WS: ws, MaxOutput: t.MaxOutputBytes, Timeout: secs(t.ExecDefaultTimeoutSec)},
		&tools.GitDiff{WS: ws, MaxOutput: t.MaxOutputBytes, Timeout: secs(t.ExecDefaultTimeoutSec)},
		&tools.GitLog{WS: ws, MaxOutput: t.MaxOutputBytes, Timeout: secs(t.ExecDefaultTimeoutSec)},
		&tools.IPCountry{},
		&tools.FetchURL{
			UserAgent:    t.FetchUserAgent,
			MaxBytes:     t.FetchMaxBytes,
			AllowHTTP:    t.FetchAllowHTTP,
			AllowPrivate: t.FetchAllowPrivate,
			Policy:       fetchPolicy,
		},
		&tools.TodoWrite{List: todos},
		&tools.TodoRead{List: todos},
		&tools.ViewImage{WS: ws, Store: images}, // offered only to a model with vision
	}
	if cfg.Search.Provider == "searxng" {
		all = append(all, &tools.WebSearch{
			Provider: &tools.SearXNG{
				Endpoint:     cfg.Search.Endpoint,
				AllowPrivate: cfg.Search.AllowPrivate,
			},
			MaxResults: cfg.Search.MaxResults,
		})
	}
	disabled := map[string]bool{}
	for _, name := range t.Disabled {
		disabled[name] = true
	}
	var enabled []tools.Tool
	for _, tool := range all {
		if !disabled[tool.Name()] {
			enabled = append(enabled, tool)
		}
	}
	return tools.NewRegistry(enabled...)
}

func listSessions(store *session.Store) error {
	list, err := store.List()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("no saved sessions in", store.Dir)
		return nil
	}
	for _, s := range list {
		fmt.Printf("%s  %s  %4d entries  %s\n", s.ID, s.Updated.Format("2006-01-02 15:04"), s.Messages, textutil.Truncate(s.Title, 60))
	}
	return nil
}

// idle keeps the container alive until Kubernetes stops it; sessions are
// started with kubectl exec. As PID 1 it must handle SIGTERM itself.
func idle() {
	fmt.Println("clauzette " + version + ": container ready. Start a session with: kubectl exec -it <pod> -- clauzette")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
}

func secs(n int) time.Duration { return time.Duration(n) * time.Second }

// serve runs the session behind the HTTP API until SIGINT or SIGTERM.
func serve(ctx context.Context, cfg *config.Config, opts ui.Options, sess *session.Session) error {
	token, created, err := wire.EnsureToken(cfg.Server.TokenFile)
	if err != nil {
		return fmt.Errorf("server token: %w", err)
	}
	tools.HideValue("server token", token) // redacted wherever it turns up in tool output
	if !wire.TokenFromEnv() {
		fmt.Fprintf(os.Stderr, "clauzette serve: warning: the token is in a file, and commands the agent runs (as the same user) can read files; with it, a command could answer the agent's own approvals through the API. Set $%s instead (in Kubernetes: from a Secret); commands do not inherit it\n", wire.TokenEnv)
	}
	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return err
	}
	if !server.IsLoopback(cfg.Server.Listen) {
		fmt.Fprintf(os.Stderr, "clauzette serve: warning: %s accepts connections from other machines over plain HTTP, token included; prefer 127.0.0.1 with kubectl port-forward or an SSH tunnel, or put TLS in front\n", cfg.Server.Listen)
	}
	where := "token in " + cfg.Server.TokenFile
	switch {
	case wire.TokenFromEnv():
		where = "token from $" + wire.TokenEnv
	case created:
		where = "new token written to " + cfg.Server.TokenFile
	}
	fmt.Fprintf(os.Stderr, "clauzette serve: session %s on http://%s (%s); clauzette attach connects\n", sess.ID, ln.Addr(), where)
	sctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := server.New(opts, sess, token)
	srv.Web = cfg.Server.Web
	srv.UploadMax = cfg.Server.UploadMaxBytes
	if srv.Web {
		fmt.Fprintf(os.Stderr, "clauzette serve: web page at http://%s/ (it asks for the token)\n", ln.Addr())
	}
	err = srv.Run(sctx, ln)
	fmt.Fprintf(os.Stderr, "clauzette serve: stopped; session %s saved\n", sess.ID)
	return err
}

// attach connects this terminal to a running server.
func attach(cfgFlag, serverURL string) error {
	cfg, err := config.Load(config.ResolvePath(cfgFlag))
	if err != nil {
		return err
	}
	base := serverURL
	if base == "" {
		base = os.Getenv("CLAUZETTE_SERVER")
	}
	if base == "" {
		base = "http://" + cfg.Server.Listen
	}
	token, err := wire.LoadToken(cfg.Server.TokenFile)
	if err != nil {
		return err
	}
	return ui.NewRemote(cfg, &wire.Client{Base: base, Token: token}).RunRemote(context.Background())
}
