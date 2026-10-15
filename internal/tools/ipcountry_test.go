package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIPCountryLookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/8.8.8.8" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ip":"8.8.8.8","country":"US"}`))
	}))
	defer srv.Close()

	tool := &IPCountry{Client: srv.Client(), BaseURL: srv.URL + "/"}
	act, err := tool.Prepare(context.Background(), Args{"ip": "8.8.8.8"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := act.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "8.8.8.8") || !strings.Contains(out, "US") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestIPCountryRejectsBadIP(t *testing.T) {
	tool := &IPCountry{BaseURL: "http://example.invalid/"}
	if _, err := tool.Prepare(context.Background(), Args{"ip": "not-an-ip"}); err == nil {
		t.Fatal("invalid IP was accepted")
	}
	// A path-injection attempt is not a valid IP either, so it is rejected
	// before any request is built.
	if _, err := tool.Prepare(context.Background(), Args{"ip": "../admin"}); err == nil {
		t.Fatal("path-injection IP was accepted")
	}
}

func TestIPCountryNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tool := &IPCountry{Client: srv.Client(), BaseURL: srv.URL + "/"}
	act, err := tool.Prepare(context.Background(), Args{"ip": "1.2.3.4"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := act.Run(context.Background()); err == nil {
		t.Fatal("expected an error for HTTP 404")
	}
}

func TestIPCountryRefusesRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
	}))
	defer srv.Close()

	// Default client (nil) refuses to follow redirects.
	tool := &IPCountry{BaseURL: srv.URL + "/"}
	act, err := tool.Prepare(context.Background(), Args{"ip": "1.2.3.4"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := act.Run(context.Background()); err == nil {
		t.Fatal("redirect was followed")
	}
}
