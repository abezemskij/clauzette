package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"clauzette/internal/config"
)

// fakeCloud is an OpenAI-compatible provider with one free model with
// tools, one free chat-only model and one paid model.
type fakeCloud struct {
	mu     sync.Mutex
	models []string // the model of each chat request
	auth   []string // the Authorization header of each chat request
}

func (f *fakeCloud) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/models":
		w.Write([]byte(`{"data": [
			{"id": "free/tooly:free", "context_length": 65536, "pricing": {"prompt": "0", "completion": "0"}, "supported_parameters": ["tools", "reasoning"]},
			{"id": "free/chat:free", "context_length": 32768, "pricing": {"prompt": "0", "completion": "0"}, "supported_parameters": []},
			{"id": "paid/big", "context_length": 200000, "pricing": {"prompt": "0.000003", "completion": "0.000015"}, "supported_parameters": ["tools"]}
		]}`))
	case "/chat/completions":
		var body struct{ Model string }
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.models = append(f.models, body.Model)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices": [{"index": 0, "delta": {"role": "assistant", "content": "hello from the cloud"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 10, "completion_tokens": 4}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeCloud) requests() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.models...), append([]string(nil), f.auth...)
}

const testKeyEnv = "CLAUZETTE_TEST_PROVIDER_KEY"

// newCloudRig is a switch rig whose openrouter provider points at a fake,
// with free_only on and a test key in the environment.
func newCloudRig(t *testing.T) (*switchRig, *fakeCloud) {
	t.Helper()
	r := newSwitchRig(t)
	cloud := &fakeCloud{}
	srv := httptest.NewServer(cloud)
	t.Cleanup(srv.Close)
	t.Setenv(testKeyEnv, "test-key")
	r.a.Cfg.Providers = map[string]config.ProviderConfig{
		"openrouter": {BaseURL: srv.URL, APIKeyEnv: testKeyEnv, FreeOnly: true},
	}
	return r, cloud
}

func TestSwitchToCloudAndBack(t *testing.T) {
	r, cloud := newCloudRig(t)
	ctx := context.Background()
	if err := r.a.SwitchModel(ctx, "openrouter:free/tooly:free", r.prompts); err != nil {
		t.Fatal(err)
	}
	if ev := r.srv.eventList(); len(ev) != 0 {
		t.Fatalf("switching to a cloud model must leave the local model alone: %v", ev)
	}
	act := r.a.Models.Active()
	if act.Cloud != "openrouter" || act.Model != "free/tooly:free" || act.NumCtx != 65536 || !act.Caps.Tools {
		t.Fatalf("cloud profile: %+v", act)
	}

	// Nothing goes out until the operator approves this endpoint.
	var notOK *CloudApprovalError
	if err := r.a.RunTurn(ctx, "hello"); !errors.As(err, &notOK) || notOK.Endpoint != act.Endpoint {
		t.Fatalf("an unapproved endpoint: %v", err)
	}
	if models, _ := cloud.requests(); len(models) != 0 {
		t.Fatalf("sent before approval: %v", models)
	}
	if r.a.CloudApprovalNeeded() != act.Endpoint {
		t.Fatalf("CloudApprovalNeeded = %q", r.a.CloudApprovalNeeded())
	}
	r.a.ApproveCloud(act.Endpoint)
	if err := r.a.RunTurn(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	models, auth := cloud.requests()
	if len(models) != 1 || models[0] != "free/tooly:free" || auth[0] != "Bearer test-key" {
		t.Fatalf("cloud requests: %v %v", models, auth)
	}
	if len(r.srv.chats) != 0 {
		t.Fatalf("the turn went to Ollama: %v", r.srv.eventList())
	}
	last := r.a.Session().Context[len(r.a.Session().Context)-1]
	if last.Content != "hello from the cloud" || last.Model != "free/tooly:free" {
		t.Fatalf("reply: %+v", last)
	}

	// Back to local: the local model is loaded; there is nothing to unload.
	if err := r.a.SwitchModel(ctx, "big", r.prompts); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.srv.eventList(), ", "); got != "load big@131072" {
		t.Fatalf("server saw: %s", got)
	}
	if a := r.a.Models.Active(); a.Cloud != "" || a.Profile != "big" {
		t.Fatalf("active after switching back: %+v", a)
	}
}

func TestCloudFreeOnly(t *testing.T) {
	r, cloud := newCloudRig(t)
	err := r.a.SwitchModel(context.Background(), "openrouter:paid/big", r.prompts)
	if err == nil || !strings.Contains(err.Error(), "free_only") {
		t.Fatalf("a paid model with free_only on: %v", err)
	}
	if a := r.a.Models.Active(); a.Profile != "big" {
		t.Fatalf("active changed to %s", a.Profile)
	}
	if models, _ := cloud.requests(); len(models) != 0 {
		t.Fatalf("nothing may be sent: %v", models)
	}

	// With free_only off, the paid model is allowed.
	r.a.Cfg.Providers["openrouter"] = config.ProviderConfig{BaseURL: r.a.Cfg.Providers["openrouter"].BaseURL, APIKeyEnv: testKeyEnv}
	if err := r.a.SwitchModel(context.Background(), "openrouter:paid/big", r.prompts); err != nil {
		t.Fatal(err)
	}
}

func TestCloudUnknownModel(t *testing.T) {
	r, _ := newCloudRig(t)
	err := r.a.SwitchModel(context.Background(), "openrouter:nobody/nothing", r.prompts)
	if err == nil || !strings.Contains(err.Error(), "does not offer") {
		t.Fatalf("err = %v", err)
	}
}

func TestCloudChatOnly(t *testing.T) {
	r, _ := newCloudRig(t)
	r.a.SetPlan(true)
	if err := r.a.SwitchModel(context.Background(), "openrouter:free/chat:free", r.prompts); err != nil {
		t.Fatal(err)
	}
	if !r.a.ChatOnly() || r.a.Plan() {
		t.Fatalf("chat-only %v, plan %v", r.a.ChatOnly(), r.a.Plan())
	}
}

func TestCloudMissingKey(t *testing.T) {
	r, cloud := newCloudRig(t)
	t.Setenv(testKeyEnv, "")
	if err := r.a.SwitchModel(context.Background(), "openrouter:free/tooly:free", r.prompts); err != nil {
		t.Fatal(err)
	}
	r.a.ApproveCloud(r.a.Models.Active().Endpoint)
	err := r.a.RunTurn(context.Background(), "hello")
	if err == nil || !strings.Contains(err.Error(), testKeyEnv) {
		t.Fatalf("a missing key must name its variable: %v", err)
	}
	if models, _ := cloud.requests(); len(models) != 0 {
		t.Fatalf("nothing may be sent without a key: %v", models)
	}
}

func TestAdaptRefusesCloud(t *testing.T) {
	r, _ := newCloudRig(t)
	err := r.a.AdaptModel(context.Background(), "openrouter:free/tooly:free", false, r.prompts)
	if err == nil || !strings.Contains(err.Error(), "local GPU") {
		t.Fatalf("err = %v", err)
	}
	if ev := r.srv.eventList(); len(ev) != 0 {
		t.Fatalf("a refused /adapt must not touch Ollama: %v", ev)
	}
}

func TestCloudApproval(t *testing.T) {
	r, _ := newCloudRig(t)
	provider, endpoint := r.a.CloudTarget("openrouter:free/tooly:free")
	if provider != "openrouter" || !strings.HasPrefix(endpoint, "openrouter (http://127.0.0.1:") {
		t.Fatalf("CloudTarget = %q %q", provider, endpoint)
	}
	if p, e := r.a.CloudTarget("big"); p != "" || e != "" {
		t.Fatalf("a local profile reported as cloud: %q %q", p, e)
	}
	// The same provider pointed at another server is another endpoint.
	r.a.Cfg.Profiles = append(r.a.Cfg.Profiles, config.Profile{Name: "proxy", Provider: "openrouter", BaseURL: "https://proxy.example/v1", Model: "free/tooly:free"})
	_, other := r.a.CloudTarget("proxy")
	if other != "openrouter (https://proxy.example/v1)" {
		t.Fatalf("proxy endpoint = %q", other)
	}

	if r.a.CloudApproved(endpoint) {
		t.Fatal("approved before asking")
	}
	r.a.ApproveCloud(endpoint)
	r.a.ApproveCloud(endpoint)
	if !r.a.CloudApproved(endpoint) || r.a.CloudApproved(other) || r.a.CloudApproved("openrouter") {
		t.Fatalf("approvals: %v", r.a.Session().CloudOK)
	}
	if n := len(r.a.Session().CloudOK); n != 1 {
		t.Fatalf("approved twice: %v", r.a.Session().CloudOK)
	}
}

// A session can reach a cloud profile without /model (-profile,
// default_profile, a resumed session). Its requests are still refused
// until the endpoint is approved.
func TestCloudStartRequiresApproval(t *testing.T) {
	r, cloud := newCloudRig(t)
	p := config.Profile{Name: "remote", Provider: "openrouter", Model: "free/tooly:free"}
	r.a.Models.set(resolve(r.a.Cfg, p, Caps{}, "", nil))
	var notOK *CloudApprovalError
	if err := r.a.RunTurn(context.Background(), "hello"); !errors.As(err, &notOK) {
		t.Fatalf("err = %v", err)
	}
	if _, err := r.a.complete(context.Background(), "summarise", "text", "summarising"); !errors.As(err, &notOK) {
		t.Fatalf("compaction request: %v", err)
	}
	if models, _ := cloud.requests(); len(models) != 0 {
		t.Fatalf("sent without approval: %v", models)
	}
}

func TestResumeCloudSession(t *testing.T) {
	r, _ := newCloudRig(t)
	if err := r.a.SwitchModel(context.Background(), "openrouter:free/tooly:free", r.prompts); err != nil {
		t.Fatal(err)
	}
	p, notes, err := SelectProfile(r.a.Cfg, "", r.a.Session())
	if err != nil || len(notes) != 0 {
		t.Fatalf("resume: %v %v", err, notes)
	}
	if p.Provider != "openrouter" || p.Model != "free/tooly:free" {
		t.Fatalf("a resumed cloud session must stay on its provider: %+v", p)
	}
}

func TestOnlineList(t *testing.T) {
	r, _ := newCloudRig(t)
	out := r.a.OnlineList(context.Background(), "", false)
	if !strings.Contains(out, "openrouter:free/tooly:free") || !strings.Contains(out, "openrouter:free/chat:free") || strings.Contains(out, "paid/big") {
		t.Fatalf("free list:\n%s", out)
	}
	out = r.a.OnlineList(context.Background(), "tooly", false)
	if !strings.Contains(out, "tooly") || strings.Contains(out, "chat:free") {
		t.Fatalf("filtered list:\n%s", out)
	}
	out = r.a.OnlineList(context.Background(), "", true)
	if !strings.Contains(out, "openrouter:paid/big") || !strings.Contains(out, "$3.00 / $15.00 per M tokens") {
		t.Fatalf("full list:\n%s", out)
	}
}
