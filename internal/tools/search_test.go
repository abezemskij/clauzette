package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSearXNGSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" || r.URL.Query().Get("format") != "json" || r.URL.Query().Get("q") != "go slices" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[
			{"title":"Slices","url":"https://go.dev/blog/slices","content":"All about slices."},
			{"title":"Arrays","url":"https://go.dev/blog/arrays","content":"Arrays too."},
			{"title":"Third","url":"https://example.com/3","content":"x"}
		]}`)
	}))
	defer srv.Close()

	s := &SearXNG{Endpoint: srv.URL, AllowPrivate: true}
	res, err := s.Search(context.Background(), "go slices", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 { // capped to max
		t.Fatalf("got %d results, want 2", len(res))
	}
	if res[0].Title != "Slices" || res[0].URL != "https://go.dev/blog/slices" {
		t.Fatalf("first result wrong: %+v", res[0])
	}
}

func TestSearXNGNonJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden) // JSON format not enabled
	}))
	defer srv.Close()
	s := &SearXNG{Endpoint: srv.URL, AllowPrivate: true}
	if _, err := s.Search(context.Background(), "q", 5); err == nil {
		t.Fatal("expected an error on HTTP 403")
	}
}

// stubProvider lets the tool be tested without a server.
type stubProvider struct {
	results []SearchResult
	gotMax  int
}

func (s *stubProvider) Name() string { return "stub" }
func (s *stubProvider) Search(_ context.Context, _ string, max int) ([]SearchResult, error) {
	s.gotMax = max
	return s.results, nil
}

func TestWebSearchFormat(t *testing.T) {
	stub := &stubProvider{results: []SearchResult{
		{Title: "One", URL: "https://a.test/1", Snippet: "first"},
		{Title: "", URL: "https://a.test/2", Snippet: ""},
	}}
	tool := &WebSearch{Provider: stub, MaxResults: 5}
	act, err := tool.Prepare(context.Background(), Args{"query": "hello", "max_results": 3.0})
	if err != nil {
		t.Fatal(err)
	}
	out, err := act.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stub.gotMax != 3 {
		t.Fatalf("max_results not passed through: %d", stub.gotMax)
	}
	for _, want := range []string{"1. One", "https://a.test/1", "first", "2. (no title)", "fetch_url"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestWebSearchEmptyQuery(t *testing.T) {
	tool := &WebSearch{Provider: &stubProvider{}}
	if _, err := tool.Prepare(context.Background(), Args{"query": "  "}); err == nil {
		t.Fatal("empty query accepted")
	}
}
