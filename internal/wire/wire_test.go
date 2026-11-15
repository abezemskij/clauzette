package wire

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTokens(t *testing.T) {
	t.Setenv("CLAUZETTE_SERVER_TOKEN", "")
	file := filepath.Join(t.TempDir(), "sub", "server.token")
	if _, err := LoadToken(file); err == nil {
		t.Fatal("a missing token was accepted")
	}
	tok, created, err := EnsureToken(file)
	if err != nil || !created || len(tok) != 64 {
		t.Fatalf("%q %v %v", tok, created, err)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v", st.Mode().Perm())
	}
	again, created, err := EnsureToken(file)
	if err != nil || created || again != tok {
		t.Fatal("the token changed on the second start")
	}
	if got, _ := LoadToken(file); got != tok {
		t.Fatal("attach reads a different token")
	}
	t.Setenv("CLAUZETTE_SERVER_TOKEN", "from-env")
	if got, _ := LoadToken(file); got != "from-env" {
		t.Fatal("the environment does not override the file")
	}
}
