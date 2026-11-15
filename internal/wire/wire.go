// Package wire is the network protocol between clauzette serve and its
// clients (clauzette attach). The server turns every call the controller
// makes on its View into a Record and streams the records to clients as
// server-sent events; a client turns them back into the same calls on its
// own view. Input goes the other way as plain lines, exactly as typed, so
// approvals, y/N answers and !c need nothing extra.
//
//	GET  /api/state            current Status (banner and prompt)
//	POST /api/input            {"text": "...", "literal": false}
//	GET  /api/events?since=N   records after N (or the last tail=M), then live
//
// Every request carries "Authorization: Bearer <token>".
package wire

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"clauzette/internal/agent"
	"clauzette/internal/control"
	"clauzette/internal/tools"
)

// Record kinds, one per View method (plus gap).
const (
	KInfo        = "info"
	KWarn        = "warn"
	KError       = "error"
	KBlock       = "block"
	KDim         = "dim"
	KEcho        = "echo"
	KRender      = "render"
	KApproval    = "approval" // ShowApproval
	KAsk         = "ask"      // AskApproval
	KPrompt      = "prompt"
	KRefresh     = "refresh"
	KWorking     = "working"
	KStopWaiting = "stop_waiting"
	KEndStream   = "end_stream"
	KGap         = "gap" // records were dropped before this one (the client was away too long)
)

// Record is one View call.
type Record struct {
	Seq      int64           `json:"seq"`
	Kind     string          `json:"kind"`
	Text     string          `json:"text,omitempty"`
	Event    *Event          `json:"event,omitempty"`
	Approval *Approval       `json:"approval,omitempty"`
	Full     bool            `json:"full,omitempty"`
	Status   *control.Status `json:"status,omitempty"`
}

// Event is an agent.Event without the approval channel.
type Event struct {
	Kind    agent.EventKind `json:"kind"`
	Text    string          `json:"text,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
	Usage   agent.Usage     `json:"usage"`
}

func FromEvent(ev agent.Event) *Event {
	return &Event{Kind: ev.Kind, Text: ev.Text, Tool: ev.Tool, IsError: ev.IsError, Usage: ev.Usage}
}

func (e *Event) Agent() agent.Event {
	return agent.Event{Kind: e.Kind, Text: e.Text, Tool: e.Tool, IsError: e.IsError, Usage: e.Usage}
}

// Approval is an approval request without its reply channel: the answer is
// typed like any other input.
type Approval struct {
	Tool       string     `json:"tool"`
	Summary    string     `json:"summary"`
	Preview    string     `json:"preview,omitempty"`
	Risk       tools.Risk `json:"risk"`
	AllowScope string     `json:"allow_scope,omitempty"`
}

func FromApproval(a *agent.ApprovalRequest) *Approval {
	return &Approval{Tool: a.Tool, Summary: a.Summary, Preview: a.Preview, Risk: a.Risk, AllowScope: a.AllowScope}
}

func (a *Approval) Agent() *agent.ApprovalRequest {
	return &agent.ApprovalRequest{Tool: a.Tool, Summary: a.Summary, Preview: a.Preview, Risk: a.Risk, AllowScope: a.AllowScope}
}

// Input is one line of operator input.
type Input struct {
	Text    string `json:"text"`
	Literal bool   `json:"literal,omitempty"`
}

// MaxInput is the largest input the server accepts.
const MaxInput = 1 << 20

// ------------------------------------------------------------ the token

// TokenEnv is the environment variable that holds the server token. Commands
// the agent runs do not inherit it (tools.HideEnv): with the token, a
// command could answer the agent's own approval requests through the API.
const TokenEnv = "CLAUZETTE_SERVER_TOKEN"

// TokenFromEnv reports whether the token comes from TokenEnv.
func TokenFromEnv() bool { return strings.TrimSpace(os.Getenv(TokenEnv)) != "" }

// LoadToken returns the token from $CLAUZETTE_SERVER_TOKEN, else from file.
func LoadToken(file string) (string, error) {
	if t := strings.TrimSpace(os.Getenv(TokenEnv)); t != "" {
		return t, nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("no server token: set $CLAUZETTE_SERVER_TOKEN or create %s (clauzette serve writes it): %w", file, err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("the token file %s is empty", file)
	}
	return t, nil
}

// EnsureToken is LoadToken for the server: without a token it creates a
// random one in file (mode 0600) and says so.
func EnsureToken(file string) (token string, created bool, err error) {
	if t, err := LoadToken(file); err == nil {
		return t, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", false, err
	}
	t := hex.EncodeToString(b)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return "", false, err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", false, err
	}
	_, werr := f.WriteString(t + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return t, true, werr
}

// ------------------------------------------------------------ the client

// ErrUnauthorized is returned when the server rejects the token.
var ErrUnauthorized = errors.New("the server rejected the token (check $CLAUZETTE_SERVER_TOKEN or the token file)")

type Client struct {
	Base  string // e.g. http://127.0.0.1:8484
	Token string
	HTTP  *http.Client // nil: a default without a timeout (the event stream is long-lived)
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Base, "/")+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, ErrUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

func (c *Client) State(ctx context.Context) (control.Status, error) {
	var st control.Status
	resp, err := c.do(ctx, http.MethodGet, "/api/state", nil)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

func (c *Client) Send(ctx context.Context, in Input) error {
	b, _ := json.Marshal(in)
	resp, err := c.do(ctx, http.MethodPost, "/api/input", b)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Events streams records after since (or the last tail when since is 0
// and tail > 0) to fn until the stream ends or ctx is done.
func (c *Client) Events(ctx context.Context, since int64, tail int, fn func(Record)) error {
	q := url.Values{}
	q.Set("since", strconv.FormatInt(since, 10))
	if tail > 0 {
		q.Set("tail", strconv.Itoa(tail))
	}
	resp, err := c.do(ctx, http.MethodGet, "/api/events?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue // id:, comments (keep-alives), blank separators
		}
		var r Record
		if err := json.Unmarshal([]byte(line[len("data: "):]), &r); err != nil {
			return fmt.Errorf("bad record from the server: %w", err)
		}
		fn(r)
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return io.EOF
}

// Backoff is the wait before reconnect attempt n (1, 2, ...).
func Backoff(n int) time.Duration {
	d := time.Duration(n) * time.Second
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}
