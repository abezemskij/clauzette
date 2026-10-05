// Command clauzette is a terminal AI agent for a local Ollama model.
//
//	clauzette               start a new session
//	clauzette -resume       continue the most recent session
//	clauzette -session ID   continue a specific session
//	clauzette -sessions     list saved sessions
//	clauzette idle          keep a container running (pod entrypoint)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"clauzette/internal/agent"
	"clauzette/internal/config"
	"clauzette/internal/gpu"
	"clauzette/internal/ollama"
	"clauzette/internal/session"
	"clauzette/internal/textutil"
	"clauzette/internal/tools"
	"clauzette/internal/ui"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "idle" {
		idle()
		return
	}
	cfgFlag := flag.String("config", "", "config file (default: $CLAUZETTE_CONFIG, ./clauzette.json or /etc/clauzette/config.json)")
	resume := flag.Bool("resume", false, "resume the most recent session")
	sessionID := flag.String("session", "", "resume the session with this id")
	list := flag.Bool("sessions", false, "list saved sessions and exit")
	printCfg := flag.Bool("print-config", false, "print the effective configuration and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("clauzette", version)
		return
	}
	if err := run(*cfgFlag, *resume, *sessionID, *list, *printCfg); err != nil {
		fmt.Fprintln(os.Stderr, "clauzette:", err)
		os.Exit(1)
	}
}

func run(cfgFlag string, resume bool, sessionID string, list, printCfg bool) error {
	cfg, err := config.Load(config.ResolvePath(cfgFlag))
	if err != nil {
		return err
	}
	if printCfg {
		b, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	store, err := session.NewStore(cfg.SessionsDir)
	if err != nil {
		return fmt.Errorf("sessions directory: %w", err)
	}
	if list {
		return listSessions(store)
	}
	ws, err := tools.NewWorkspace(cfg.Workspace)
	if err != nil {
		return err
	}
	reg := buildRegistry(cfg, ws)
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
	}
	defer audit.Close()

	deps := agent.Deps{Cfg: cfg, Client: client, Registry: reg, Store: store, Guard: guard, Audit: audit, WS: ws}
	buildPrompt := func(date string) (string, []string, error) {
		return agent.BuildSystemPrompt(cfg, agent.PromptData{
			Date:      date,
			Workspace: ws.Root,
			Model:     cfg.Model,
			Shell:     cfg.Tools.Shell,
			Tools:     reg.Names(),
			Internet:  cfg.Prompt.InternetAccess,
			NumCtx:    cfg.NumCtx,
		})
	}
	newSession := func() (*session.Session, []string, error) {
		// The date is fixed when the session starts: a value that changes
		// inside the system prompt would defeat Ollama's prompt cache.
		date := time.Now().Format("Monday, 2 January 2006")
		sys, n, err := buildPrompt(date)
		if err != nil {
			return nil, nil, err
		}
		now := time.Now()
		s := &session.Session{
			ID: session.NewID(), Created: now, Updated: now,
			Model: cfg.Model, Date: date, System: sys,
			Think: cfg.Think, ShowThinking: cfg.ShowThinking,
		}
		return s, n, store.Save(s)
	}

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
		}
		resumed = true
	default:
		var n []string
		sess, n, err = newSession()
		notes = append(notes, n...)
	}
	if err != nil {
		return err
	}

	u := ui.New(ui.Options{
		Deps:        deps,
		NewSession:  newSession,
		BuildPrompt: buildPrompt,
		Notes:       notes,
		Resumed:     resumed,
	}, sess)
	return u.Run(ctx)
}

func buildRegistry(cfg *config.Config, ws *tools.Workspace) *tools.Registry {
	t := cfg.Tools
	all := []tools.Tool{
		&tools.ReadFile{WS: ws, DefaultLines: t.ReadDefaultLines, MaxBytes: t.ReadMaxFileBytes},
		&tools.ListDir{WS: ws},
		&tools.SearchFiles{WS: ws},
		&tools.WriteFile{WS: ws},
		&tools.EditFile{WS: ws},
		&tools.MvFile{WS: ws},
		&tools.DeleteFile{WS: ws},
		&tools.ExecCommand{
			WS:             ws,
			Shell:          t.Shell,
			DefaultTimeout: secs(t.ExecDefaultTimeoutSec),
			MaxTimeout:     secs(t.ExecMaxTimeoutSec),
			MaxOutput:      t.MaxOutputBytes,
		},
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
