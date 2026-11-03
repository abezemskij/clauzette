package tools

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRedact(t *testing.T) {
	t.Setenv("CLAUZETTE_TEST_KEY", "test-secret-value-123")
	t.Setenv("CLAUZETTE_TEST_SHORT", "abc") // too short to redact safely
	HideEnv("CLAUZETTE_TEST_KEY", "CLAUZETTE_TEST_SHORT")
	t.Cleanup(func() { HideEnv() })
	got := Redact("KEY=test-secret-value-123\nagain test-secret-value-123; abc stays")
	if strings.Contains(got, "test-secret-value-123") || strings.Count(got, "[redacted CLAUZETTE_TEST_KEY]") != 2 || !strings.Contains(got, "abc stays") {
		t.Fatalf("Redact = %q", got)
	}
	HideEnv()
	if Redact("test-secret-value-123") != "test-secret-value-123" {
		t.Fatal("HideEnv() must clear the values too")
	}
}

// The child helper: protect the process, say so, then wait to be inspected.
func TestMain(m *testing.M) {
	if os.Getenv("CLAUZETTE_PROTECT_CHILD") == "1" {
		if err := ProtectProcess(); err != nil {
			os.Stdout.WriteString("error " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("ready\n")
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestProtectProcessHidesEnviron(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read every process's environ; run as a normal user")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "CLAUZETTE_PROTECT_CHILD=1", "CLAUZETTE_TEST_SECRET=not-for-you")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	buf := make([]byte, 64)
	n, _ := out.Read(buf)
	if !strings.HasPrefix(string(buf[:n]), "ready") {
		t.Fatalf("child: %q", buf[:n])
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/environ")
	if err == nil {
		t.Fatalf("a same-user process could read the protected environ (%d bytes)", len(b))
	}
	if !os.IsPermission(err) {
		t.Fatalf("unexpected error: %v", err)
	}
}
