package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWeightsDigest(t *testing.T) {
	hex := "f5f1dd8920d4aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tc := range []struct{ modelfile, want string }{
		{"# comment\nFROM /root/.ollama/models/blobs/sha256-" + hex + "\nPARAMETER num_ctx 65536\n", "sha256-" + hex},
		{"FROM /models/blobs/sha256:" + hex, "sha256-" + hex},
		{"FROM qwen3.8:latest\n", ""},               // not a blob path
		{"PARAMETER stop sha256-" + hex + "\n", ""}, // only the FROM line counts
	} {
		if got := weightsDigest(tc.modelfile); got != tc.want {
			t.Errorf("weightsDigest(%q) = %q, want %q", tc.modelfile, got, tc.want)
		}
	}
}

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"models": [{"name": "gemma4:12b", "model": "gemma4:12b", "digest": "4eb23ef187e2", "size": 7600000000,
			"capabilities": ["completion", "vision", "tools", "thinking"],
			"details": {"family": "gemma4", "parameter_size": "11.9B", "quantization_level": "Q4_K_M", "context_length": 262144}}]}`))
	}))
	defer srv.Close()
	list, err := New(srv.URL, Timeouts{Connect: time.Second}).ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("%d models", len(list))
	}
	m := list[0]
	if !m.Matches("gemma4:12b") || !m.Has("tools") || m.Has("audio") || m.Details.ContextLength != 262144 || m.Details.ParameterSize != "11.9B" {
		t.Fatalf("model = %+v", m)
	}
}
