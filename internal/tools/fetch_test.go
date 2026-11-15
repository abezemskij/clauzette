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

func TestFetchContentKinds(t *testing.T) {
	page := "<!DOCTYPE html><html><head><script>var x=1;</script></head><body><p>Hello <b>world</b></p></body></html>"
	pdf := append([]byte("%PDF-1.7\n"), make([]byte, 64)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, page)
		case "/mislabelled":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, page)
		case "/doc.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write(pdf)
		case "/blob":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte{0x7f, 'E', 'L', 'F', 0, 0, 1, 2, 3})
		case "/notes.txt":
			w.Header().Set("Content-Type", "application/octet-stream")
			fmt.Fprint(w, "plain notes served as octet-stream")
		case "/fake-html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte{'<', 'p', '>', 0, 0, 0xff, 0xfe})
		case "/shot.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("\x89PNG\r\n\x1a\n"))
		case "/icon.svg":
			w.Header().Set("Content-Type", "image/svg+xml")
			fmt.Fprint(w, `<svg xmlns="http://www.w3.org/2000/svg"><circle r="4"/></svg>`)
		}
	}))
	defer srv.Close()
	f := &FetchURL{AllowHTTP: true, AllowPrivate: true}
	get := func(p string, raw bool) string {
		t.Helper()
		act, err := f.Prepare(context.Background(), Args{"url": srv.URL + p, "raw": raw})
		if err != nil {
			t.Fatal(err)
		}
		if raw != strings.Contains(act.Summary, ", raw") {
			t.Fatalf("summary %q", act.Summary)
		}
		out, err := act.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	if out := get("/page", false); !strings.Contains(out, "Hello world") || strings.Contains(out, "<b>") || strings.Contains(out, "var x") {
		t.Errorf("HTML not converted:\n%s", out)
	}
	if out := get("/page", true); !strings.Contains(out, "<b>world</b>") || !strings.Contains(out, "<script>var x=1;</script>") || !strings.Contains(out, "(raw:") {
		t.Errorf("raw did not return the source:\n%s", out)
	}
	if out := get("/mislabelled", false); !strings.Contains(out, "Hello world") || strings.Contains(out, "<b>") || !strings.Contains(out, "the body is HTML") {
		t.Errorf("HTML labelled text/plain not converted:\n%s", out)
	}
	if out := get("/mislabelled", true); !strings.Contains(out, "<b>world</b>") {
		t.Errorf("raw converted a mislabelled page:\n%s", out)
	}
	for _, p := range []string{"/doc.pdf", "/blob", "/fake-html", "/shot.png"} {
		for _, raw := range []bool{false, true} {
			out := get(p, raw)
			if !strings.Contains(out, "Binary content") || strings.Contains(out, "PDF-1.7") || strings.Contains(out, "\x00") || strings.Contains(out, "ELF") {
				t.Errorf("%s (raw %v) returned binary bytes:\n%q", p, raw, out)
			}
		}
	}
	if out := get("/doc.pdf", false); !strings.Contains(out, "application/pdf, 73 B") || !strings.Contains(out, "curl -fsSL -o doc.pdf '"+srv.URL+"/doc.pdf'") {
		t.Errorf("binary note:\n%s", out)
	}
	if out := get("/shot.png", false); !strings.Contains(out, "view_image") {
		t.Errorf("image note does not mention view_image:\n%s", out)
	}
	if out := get("/notes.txt", false); !strings.Contains(out, "plain notes served as octet-stream") {
		t.Errorf("text served as octet-stream was refused:\n%s", out)
	}
	if out := get("/icon.svg", false); !strings.Contains(out, "<circle") {
		t.Errorf("SVG (text) was refused:\n%s", out)
	}
}
