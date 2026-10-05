package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"clauzette/internal/tools"
)

// The agent core never touches the terminal. It reports everything as
// events, and approvals travel back through a channel. A different front
// end (a network server, a web page) can consume the same events.

type EventKind int

const (
	EvText        EventKind = iota // a piece of the reply
	EvThinking                     // a piece of the model's reasoning
	EvStreamEnd                    // the current model response is complete
	EvWaiting                      // a long wait starts (Text says for what)
	EvWaitingDone                  // the wait is over
	EvToolCall                     // the model called a tool (Text: short argument summary)
	EvToolResult                   // a tool finished (Text: full result)
	EvApproval                     // an action needs the operator's decision
	EvInfo
	EvWarn
	EvError
	EvUsage // context usage changed
)

type Usage struct {
	Tokens     int // estimated tokens in the working context
	NumCtx     int
	PromptEval int // prompt_eval_count of the last request
	Eval       int // eval_count of the last request
	TokPerSec  float64
}

type Decision struct {
	Approve bool
	Always  bool // approve this tool for the rest of the session
	Reason  string
}

type ApprovalRequest struct {
	Tool    string
	Summary string
	Preview string
	Risk    tools.Risk
	Reply   chan<- Decision // buffered; send exactly once
}

type Event struct {
	Kind     EventKind
	Text     string
	Tool     string
	IsError  bool
	Approval *ApprovalRequest
	Usage    Usage
}

// Audit appends one JSON line per tool call to a file. Inside `kubectl exec`
// the process's output goes to your terminal, not to the pod log, so a file
// on persistent storage is the reliable place for the record.
type Audit struct {
	mu sync.Mutex
	f  *os.File
}

func OpenAudit(path string) (*Audit, error) {
	if path == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Audit{f: f}, nil
}

func (a *Audit) Log(fields map[string]any) {
	if a == nil {
		return
	}
	fields["time"] = time.Now().Format(time.RFC3339)
	b, err := json.Marshal(fields)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = a.f.Write(append(b, '\n'))
}

func (a *Audit) Close() {
	if a != nil {
		_ = a.f.Close()
	}
}
