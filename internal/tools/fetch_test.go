package tools

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", "10.0.0.1", "192.168.1.1", "172.16.5.4",
		"169.254.169.254", // cloud metadata
		"0.0.0.0", "100.64.0.1", "fe80::1", "fc00::1",
	}
	for _, s := range blocked {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:2800:220:1::1"} {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestFetchSchemeRejected(t *testing.T) {
	f := &FetchURL{} // https-only by default
	ctx := context.Background()
	if _, err := f.Prepare(ctx, Args{"url": "http://example.com"}); err == nil {
		t.Fatal("plain http accepted when AllowHTTP is false")
	}
	if _, err := f.Prepare(ctx, Args{"url": "ftp://example.com/x"}); err == nil {
		t.Fatal("ftp accepted")
	}
	if _, err := f.Prepare(ctx, Args{"url": "/relative/path"}); err == nil {
		t.Fatal("relative URL accepted")
	}
}

func TestFetchSuccessAndTruncate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("no User-Agent sent")
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "0123456789abcdef")
	}))
	defer srv.Close()

	// Allow http+private so the real client (with the SSRF dialer) reaches the
	// loopback test server.
	f := &FetchURL{AllowHTTP: true, AllowPrivate: true, MaxBytes: 1000}
	act, err := f.Prepare(context.Background(), Args{"url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	out, err := act.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "HTTP 200") || !strings.Contains(out, "0123456789abcdef") {
		t.Fatalf("unexpected body:\n%s", out)
	}

	// max_bytes clamps the returned body and marks it truncated.
	act, err = f.Prepare(context.Background(), Args{"url": srv.URL, "max_bytes": 5.0})
	if err != nil {
		t.Fatal(err)
	}
	out, err = act.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "01234") || strings.Contains(out, "0123456789abcdef") {
		t.Fatalf("body not truncated to 5 bytes:\n%s", out)
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("missing truncation note:\n%s", out)
	}
}

func TestFetchBlocksLoopbackWithoutAllowPrivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "should not be reached")
	}))
	defer srv.Close()

	// AllowHTTP so the scheme passes Prepare, but AllowPrivate is false, so the
	// SSRF dialer must refuse the loopback connection.
	f := &FetchURL{AllowHTTP: true, AllowPrivate: false}
	act, err := f.Prepare(context.Background(), Args{"url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = act.Run(context.Background())
	if err == nil {
		t.Fatal("loopback was fetched despite AllowPrivate=false")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "loopback") &&
		!strings.Contains(strings.ToLower(err.Error()), "private") {
		t.Fatalf("error should explain the SSRF block: %v", err)
	}
}

func TestFetchRedirectCap(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+r.URL.Path+"x", http.StatusFound) // endless redirects
	}))
	defer srv.Close()

	f := &FetchURL{AllowHTTP: true, AllowPrivate: true, MaxRedirects: 3}
	act, err := f.Prepare(context.Background(), Args{"url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := act.Run(context.Background()); err == nil {
		t.Fatal("endless redirects were followed without limit")
	}
}

func TestFetchPolicy(t *testing.T) {
	if got := normalizeHost("Example.COM"); got != "example.com" {
		t.Errorf("normalizeHost case: %q", got)
	}
	if got := normalizeHost("https://foo.com/path"); got != "foo.com" {
		t.Errorf("normalizeHost url: %q", got)
	}
	if got := normalizeHost("foo.com:8443"); got != "foo.com" {
		t.Errorf("normalizeHost port: %q", got)
	}

	p := NewFetchPolicy(false, []string{"Example.com", "https://pkg.go.dev/"})
	if p.GateOn() {
		t.Fatal("gate should start off")
	}
	p.SetGate(true)
	if !p.Allowed("example.com") || !p.Allowed("pkg.go.dev") {
		t.Fatal("seeded hosts not allowed")
	}
	if p.Allowed("docs.example.com") {
		t.Fatal("a subdomain must be treated as a separate host")
	}
	p.Add("new.test")
	if !p.Allowed("new.test") {
		t.Fatal("Add did not take")
	}
	p.Forget("example.com")
	if p.Allowed("example.com") {
		t.Fatal("Forget did not take")
	}
	// Left: pkg.go.dev and new.test, sorted.
	if got := strings.Join(p.List(), ","); got != "new.test,pkg.go.dev" {
		t.Fatalf("List() = %q", got)
	}
}

func TestFetchRedirectAllowlist(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a":
			http.Redirect(w, r, "/b", http.StatusFound) // same host, ok
		case "/cross":
			http.Redirect(w, r, "http://example.invalid/", http.StatusFound)
		default:
			fmt.Fprint(w, "landed")
		}
	}))
	defer srv.Close()

	policy := NewFetchPolicy(true, []string{"127.0.0.1"})
	f := &FetchURL{AllowHTTP: true, AllowPrivate: true, Policy: policy}

	// Same-host redirect is allowed.
	act, err := f.Prepare(context.Background(), Args{"url": srv.URL + "/a"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := act.Run(context.Background())
	if err != nil || !strings.Contains(out, "landed") {
		t.Fatalf("same-host redirect failed: %q %v", out, err)
	}

	// A redirect to a host not in the allowlist is refused.
	act, err = f.Prepare(context.Background(), Args{"url": srv.URL + "/cross"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := act.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("cross-host redirect should be refused with an allowlist error, got %v", err)
	}
}
