package ui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"clauzette/internal/agent"
	"clauzette/internal/config"
	"clauzette/internal/gpu"
	"clauzette/internal/ollama"
	"clauzette/internal/server"
	"clauzette/internal/session"
	"clauzette/internal/tools"
	"clauzette/internal/wire"
)

// client is an attach client on pipes.
type client struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *syncBuf
	done chan error
}

func attachClient(t *testing.T, base, token string) *client {
	t.Helper()
	cfg := config.Defaults()
	cfg.Color = "never"
	u := NewRemote(cfg, &wire.Client{Base: base, Token: token})
	inR, inW := io.Pipe()
	c := &client{t: t, in: inW, out: &syncBuf{}, done: make(chan error, 1)}
	u.in, u.out, u.tty, u.color = inR, c.out, false, false
	u.md.plain = true
	go func() { c.done <- u.RunRemote(context.Background()) }()
	return c
}

// send types a line and waits until the output after it matches re.
func (c *client) send(line, re string) {
	c.t.Helper()
	from := len(c.out.String())
	fmt.Fprintf(c.in, "%s\n", line)
	c.wait(from, re)
}

func (c *client) wait(from int, re string) {
	c.t.Helper()
	rx := regexp.MustCompile(re)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if s := c.out.String(); len(s) >= from && rx.MatchString(s[from:]) {
			return
		}
	}
	c.t.Fatalf("timed out waiting for %q; client output:\n%s", re, c.out.String())
}

func (c *client) exited() error {
	c.t.Helper()
	select {
	case err := <-c.done:
		return err
	case <-time.After(10 * time.Second):
		c.t.Fatal("the client did not exit")
		return nil
	}
}

func TestServeAndAttach(t *testing.T) {
	ollamaSrv := fakeOllamaServer(t)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "ws"), 0o755)
	ws, err := tools.NewWorkspace(filepath.Join(root, "ws"))
	if err != nil {
		t.Fatal(err)
	}
	store, _ := session.NewStore(filepath.Join(root, "sessions"))
	cfg := config.Defaults()
	cfg.OllamaURL, cfg.Model, cfg.NumCtx = ollamaSrv.URL, "testmodel:latest", 32768
	client := ollama.New(cfg.OllamaURL, ollama.Timeouts{Connect: time.Second, FirstToken: 10 * time.Second, InterToken: 10 * time.Second, Load: 10 * time.Second})
	profile, _ := cfg.FindProfile(cfg.DefaultProfileName())
	models := agent.NewModels(cfg, nil, profile)
	reg := tools.NewRegistry(&tools.WriteFile{WS: ws})
	deps := agent.Deps{Cfg: cfg, Client: client, Models: models, Registry: reg, Store: store, Guard: gpu.New(cfg.GPU), WS: ws, Todos: tools.NewTodoList()}
	sess := &session.Session{ID: "SRV", Model: "testmodel:latest", System: "NORMAL", SystemRogue: "ROGUE", SystemChat: "CHAT"}
	store.Save(sess)
	unlock, err := store.Lock(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Deps: deps, Unlock: unlock,
		NewSession: func() (*session.Session, []string, error) { return nil, nil, errors.New("not in this test") },
		BuildPrompts: func(string) (agent.Prompts, []string, error) {
			return agent.Prompts{Normal: "N", Rogue: "R", Chat: "C"}, nil, nil
		},
	}

	const token = "secret-test-token"
	srv := server.New(opts, sess, token)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Run(ctx, ln) }()

	// no token, a wrong token: refused
	for _, tok := range []string{"", "wrong"} {
		req, _ := http.NewRequest("GET", base+"/api/state", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: status %d", tok, resp.StatusCode)
		}
	}
	if err := attachClient(t, base, "wrong").exited(); !errors.Is(err, wire.ErrUnauthorized) {
		t.Fatalf("attach with a wrong token: %v", err)
	}

	// the first client works through a turn with an approval, then detaches
	a := attachClient(t, base, token)
	a.wait(0, `already loaded[\s\S]*`+promptRE)
	a.send("hello", `Hi there\.[\s\S]*`+promptRE)
	a.send("write a file", `Approval needed \(write\): create a\.txt[\s\S]*\(view all\): $`)
	a.send("y", `Result noted: Created a\.txt[\s\S]*`+promptRE)
	a.send("/help", `Any time, even while the agent works:[\s\S]*`+promptRE) // the client's own help
	a.send("/context", `Context: ~[\s\S]*`+promptRE)
	if _, err := os.Stat(filepath.Join(root, "ws", "a.txt")); err != nil {
		t.Fatalf("the approved write did not run: %v", err)
	}
	a.send("/exit", `detached; session SRV keeps running in the server`)
	if err := a.exited(); err != nil {
		t.Fatal(err)
	}

	// a second client sees what happened before it attached, and can work on
	b := attachClient(t, base, token)
	b.wait(0, `Hi there\.[\s\S]*Result noted[\s\S]*`+promptRE)
	b.send("are you still there", `ok: are you still there[\s\S]*`+promptRE)

	// a client that sends /exit as plain input does not end the session
	wc := &wire.Client{Base: base, Token: token}
	if err := wc.Send(context.Background(), wire.Input{Text: "/exit"}); err != nil {
		t.Fatal(err)
	}
	b.wait(0, `the server keeps the session running`)
	st, err := wc.State(context.Background())
	if err != nil || st.Session != "SRV" || !st.Warm || st.Busy {
		t.Fatalf("state %+v %v", st, err)
	}
	if _, err := store.Lock("SRV"); err == nil {
		t.Fatal("the server's session could be locked a second time")
	}

	// the web page's data endpoints
	get := func(path string, wantCode int) string {
		t.Helper()
		req, _ := http.NewRequest("GET", base+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != wantCode {
			t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b)
		}
		return string(b)
	}
	if body := get("/api/sessions", 200); !strings.Contains(body, `"current":"SRV"`) || !strings.Contains(body, `"ID":"SRV"`) {
		t.Fatalf("sessions: %s", body)
	}
	if body := get("/api/models", 200); !strings.Contains(body, `"name":"default"`) || !strings.Contains(body, `"active":true`) {
		t.Fatalf("models: %s", body)
	}
	if body := get("/api/todos", 200); body != "{\"todos\":[]}\n" {
		t.Fatalf("todos: %q", body)
	}
	if body := get("/api/plan", 200); !strings.Contains(body, `"pending":[]`) {
		t.Fatalf("plan: %s", body)
	}
	if body := get("/api/files", 200); !strings.Contains(body, `"name":"a.txt"`) {
		t.Fatalf("files: %s", body)
	}
	if body := get("/api/file?path=a.txt", 200); !strings.HasPrefix(body, "x") {
		t.Fatalf("file: %q", body)
	}
	get("/api/file?path=../sessions/SRV.json", 400) // outside the workspace
	get("/api/files?path=..", 400)
	get("/api/file?path=nope.txt", 404)

	upload := func(name string, data []byte) (int, string) {
		req, _ := http.NewRequest("POST", base+"/api/upload?name="+url.QueryEscape(name), bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	png := pngBytes(t)
	code, body := upload("../../etc/évil shot.png", png)
	if code != 200 || !regexp.MustCompile(`"path":"uploads/\d{4}-\d{2}-\d{2}-\d{6}-vil_shot\.png"`).MatchString(body) || !strings.Contains(body, `"image":true`) {
		t.Fatalf("upload: %d %s", code, body)
	}
	code2, body2 := upload("../../etc/évil shot.png", png) // same name, same second: a new file, nothing overwritten
	if code2 != 200 || body2 == body {
		t.Fatalf("second upload: %d %s", code2, body2)
	}
	ups, _ := os.ReadDir(filepath.Join(root, "ws", "uploads"))
	if len(ups) != 2 {
		t.Fatalf("uploads dir: %v", ups)
	}
	srv.UploadMax = 10
	if code, _ := upload("big.bin", make([]byte, 11)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("upload over the limit: %d", code)
	}
	fmt.Fprintf(b.in, "!q\n")
	if err := b.exited(); err != nil {
		t.Fatal(err)
	}

	// stopping the server saves the session and releases it
	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not stop")
	}
	saved, err := store.Load("SRV")
	if err != nil || len(saved.Context) < 6 {
		t.Fatalf("session not saved: %v (%d messages)", err, len(saved.Context))
	}
	if !strings.Contains(saved.Context[len(saved.Context)-1].Content, "ok: are you still there") {
		t.Fatalf("last message %q", saved.Context[len(saved.Context)-1].Content)
	}
	unlock2, err := store.Lock("SRV")
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	unlock2()
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
