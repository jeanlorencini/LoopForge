package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Platform-side clarification stages the control plane's own Codex login
// through the refresh lease: the child never sees the host refresh token.
func TestPrepareHostCodexLoginUsesLease(t *testing.T) {
	const original = `{"auth_mode":"chatgpt","tokens":{"account_id":"acct-1","access_token":"fake-access","refresh_token":"host-refresh"},"last_refresh":"2026-11-01"}`
	host := t.TempDir()
	t.Setenv("CODEX_HOME", host)
	t.Setenv("AGENTFLOW_RUNNER_AUTH_DIR", t.TempDir())
	if HostCodexLoginAvailable() {
		t.Fatal("no login file yet")
	}
	if err := os.WriteFile(filepath.Join(host, "auth.json"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if !HostCodexLoginAvailable() {
		t.Fatal("login file not detected")
	}
	home := filepath.Join(t.TempDir(), "home")
	env, cleanup, err := PrepareHostCodexLogin(home)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(home, ".codex", "auth.json")
	data, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "host-refresh") {
		t.Fatal("the host refresh token reached the private HOME")
	}
	if !strings.HasPrefix(env["CODEX_REFRESH_TOKEN_URL_OVERRIDE"], "http://127.0.0.1:") {
		t.Fatalf("refresh endpoint = %q", env["CODEX_REFRESH_TOKEN_URL_OVERRIDE"])
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatal("staged login was not removed")
	}
	if got, _ := os.ReadFile(filepath.Join(host, "auth.json")); string(got) != original {
		t.Fatal("host login changed")
	}
}
