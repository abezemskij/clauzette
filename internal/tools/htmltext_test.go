package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHTMLToText(t *testing.T) {
	base, _ := url.Parse("https://pkg.go.dev/net/http")

	t.Run("drops script/style/head-noise, decodes entities, keeps block breaks", func(t *testing.T) {
		src := `<!doctype html><html><head><title>T</title>
			<style>.x{color:red}</style><script>track()</script></head>
			<body><h1>Go&#39;s net/http</h1><p>It is&nbsp;small.</p></body></html>`
		got := htmlToText(src, base, 0)
		if strings.Contains(got, "color:red") || strings.Contains(got, "track()") {
			t.Fatalf("script/style leaked:\n%s", got)
		}
		if !strings.Contains(got, "Go's net/http") {
			t.Fatalf("entity not decoded or heading lost:\n%s", got)
		}
		if !strings.Contains(got, "It is small.") && !strings.Contains(got, "It is small.") {
			t.Fatalf("paragraph text lost:\n%s", got)
		}
		// heading and paragraph must be on separate lines
		if !strings.Contains(got, "net/http\n") {
			t.Fatalf("block break missing between heading and paragraph:\n%s", got)
		}
	})

	t.Run("links become refs, resolved, deduped, listed", func(t *testing.T) {
		src := `<p>See <a href="/pkg/io">io</a> and <a href="https://example.com/x">x</a>,
			and <a href="/pkg/io">io again</a>.</p>`
		got := htmlToText(src, base, 0)
		if !strings.Contains(got, "io[1]") || !strings.Contains(got, "x[2]") {
			t.Fatalf("inline refs missing:\n%s", got)
		}
		if !strings.Contains(got, "io again[1]") {
			t.Fatalf("duplicate link should reuse ref [1]:\n%s", got)
		}
		if !strings.Contains(got, "Links:") ||
			!strings.Contains(got, "[1] https://pkg.go.dev/pkg/io") ||
			!strings.Contains(got, "[2] https://example.com/x") {
			t.Fatalf("reference list wrong:\n%s", got)
		}
		// a deduped link appears once in the list
		if strings.Count(got, "https://pkg.go.dev/pkg/io") != 1 {
			t.Fatalf("link not deduped:\n%s", got)
		}
	})

	t.Run("non-navigable and in-page links are dropped", func(t *testing.T) {
		src := `<a href="#top">top</a> <a href="mailto:a@b.c">mail</a> <a href="javascript:void(0)">x</a>`
		got := htmlToText(src, base, 0)
		if strings.Contains(got, "Links:") || strings.Contains(got, "[1]") {
			t.Fatalf("should keep no links:\n%s", got)
		}
	})

	t.Run("link cap limits the list", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 10; i++ {
			b.WriteString(`<a href="https://example.com/` + string(rune('a'+i)) + `">L</a> `)
		}
		got := htmlToText(b.String(), base, 3)
		if strings.Count(got, "https://example.com/") != 3 {
			t.Fatalf("expected 3 links kept, got:\n%s", got)
		}
	})

	t.Run("unterminated comment and stray angle bracket are safe", func(t *testing.T) {
		if got := htmlToText("a <!-- oops no end", base, 0); !strings.Contains(got, "a") {
			t.Fatalf("lost leading text: %q", got)
		}
		if got := htmlToText("1 < 2 is true", base, 0); !strings.Contains(got, "2 is true") {
			t.Fatalf("stray '<' mishandled: %q", got)
		}
	})

	t.Run("pre preserves internal whitespace", func(t *testing.T) {
		src := "<pre>func main() {\n    x := 1\n}</pre>"
		got := htmlToText(src, base, 0)
		if !strings.Contains(got, "    x := 1") {
			t.Fatalf("pre indentation lost:\n%q", got)
		}
	})
}

// TestFetchCleansHTML exercises the content-type gate end to end through the
// real fetch path (SSRF dialer, two-limit read) against a loopback server.
func TestFetchCleansHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><style>b{}</style></head>
			<body><h1>Title</h1><p>Hello <a href="/next">next</a>.</p></body></html>`))
	}))
	defer srv.Close()

	f := &FetchURL{AllowHTTP: true, AllowPrivate: true}
	act, err := f.Prepare(context.Background(), Args{"url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	out, err := act.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "<h1>") || strings.Contains(out, "b{}") {
		t.Fatalf("HTML not cleaned:\n%s", out)
	}
	if !strings.Contains(out, "Title") || !strings.Contains(out, "Hello next[1]") {
		t.Fatalf("cleaned text wrong:\n%s", out)
	}
	if !strings.Contains(out, "Links:") || !strings.Contains(out, srv.URL+"/next") {
		t.Fatalf("link list missing or not absolute:\n%s", out)
	}
}
