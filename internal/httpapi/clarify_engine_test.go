package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
)

// Issue #228: clarification runs on CodeBuddy and Claude Code, run evals on
// CodeBuddy only. A model of another CLI ("codex:…") must be refused up front,
// not silently swapped for the CodeBuddy default.
func TestClarifyModelEngineError(t *testing.T) {
	for _, id := range []string{"", "default", "glm-5.1", "public-model", "claude-code:sonnet", "claude-code:claude-opus-5-5", "codex:gpt-6.2-sol"} {
		if err := clarifyModelEngineError(id); err != nil {
			t.Fatalf("model %q refused: %v", id, err)
		}
	}
	for _, id := range []string{"codex:gpt-5.5", "codex:gpt-6.2-astra"} {
		if err := clarifyModelEngineError(id); err != errClarifyNeedsCodeBuddy {
			t.Fatalf("model %q = %v, want errClarifyNeedsCodeBuddy", id, err)
		}
	}
}

func TestChatClarifyRefusesOtherCLIModel(t *testing.T) {
	called := false
	setClarifyExecForTest(func(context.Context, domain.Issue, chatClarifyRequest, func(string, any)) (any, string, map[string]any, error) {
		called = true
		return map[string]any{}, "{}", map[string]any{}, nil
	})
	t.Cleanup(func() { setClarifyExecForTest(nil) })

	_, h := clarifyTestServer(t)
	body, _ := json.Marshal(map[string]any{"projectId": "platform", "message": "加个徽章", "model": "codex:gpt-5.5"})
	req := httptest.NewRequest(http.MethodPost, "/api/chat-clarify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "只支持 CodeBuddy") {
		t.Fatalf("error does not explain the CodeBuddy limit: %s", rec.Body.String())
	}
	if called {
		t.Fatal("clarification started with a Codex model")
	}
}

func TestRunEvalRefusesOtherCLIModel(t *testing.T) {
	f := cvBNewHTTPFixture(t)
	if _, err := f.s.UpdateUserCredentials(f.user.ID, repo.UserCredentialUpdate{CodeBuddyAPIKey: "eval-key"}); err != nil {
		t.Fatal(err)
	}
	done := domain.Run{ID: "eval-codex-run", Status: "succeeded", WorkflowID: "missing",
		ExecutionContext: domain.TelemetryContext{ProjectID: "platform"}, Nodes: map[string]*domain.NodeRun{}}
	if err := f.s.PutRun(done); err != nil {
		t.Fatal(err)
	}
	rec := cvBRequest(t, f, http.MethodPost, "/api/runs/eval-codex-run/eval", map[string]any{"model": "codex:gpt-5.5"}, true)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "只支持 CodeBuddy") {
		t.Fatalf("status = %d body = %s, want 400 explaining the CodeBuddy limit", rec.Code, rec.Body.String())
	}
	if _, busy := f.s.ActiveRunEval(done.ID); busy {
		t.Fatal("a Codex model must not start a run eval")
	}
}

func TestPrepareClarifyClaudeCommand(t *testing.T) {
	command, err := prepareClarifyCommandWith(clarifyCommandOpts{
		Bin: "claude", Model: "claude-code:claude-opus-5-5", Prompt: "澄清这条需求", RunID: "clarify-test", SessionID: "af1",
		Native: true, MCPConfig: "/tmp/claude-mcp.json", Settings: "/tmp/settings.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	argv := command.Argv
	if command.Provider != "claude-code" || argv[0] != "claude" || command.Stdin == nil || len(command.CredentialBindings) != 1 || command.CredentialBindings[0] != runtime.BindClaudeCode {
		t.Fatalf("not a claude command: provider=%q bindings=%v argv=%v", command.Provider, command.CredentialBindings, argv)
	}
	for flag, want := range map[string]string{
		"--output-format": "stream-json", "--model": "claude-opus-5-5", "--tools": "Read,Grep,Glob",
		"--mcp-config": "/tmp/claude-mcp.json", "--settings": "/tmp/settings.json", "--setting-sources": "user",
		"--session-id": assistantClaudeSessionID("af1"), "--permission-mode": "bypassPermissions",
	} {
		if got, _ := argvFlagValue(argv, flag); got != want {
			t.Fatalf("%s = %q, want %q: %v", flag, got, want, argv)
		}
	}
	for _, flag := range []string{"-p", "--verbose", "--include-partial-messages", "--strict-mcp-config"} {
		if !slices.Contains(argv, flag) {
			t.Fatalf("missing %s: %v", flag, argv)
		}
	}
	if denied, _ := argvFlagValue(argv, "--disallowedTools"); !strings.Contains(denied, "Bash") || !strings.Contains(denied, "Task") || strings.Contains(denied, "Read") {
		t.Fatalf("--disallowedTools = %q", denied)
	}
	if slices.Contains(argv, "澄清这条需求") {
		t.Fatalf("prompt must go on stdin: %v", argv)
	}
	// A captured Claude session UUID is resumed as is; submit-only denies reading.
	uuid := assistantClaudeSessionID("af1")
	command, err = prepareClarifyCommandWith(clarifyCommandOpts{Bin: "claude", Model: "claude-code:claude-opus-5-5", Prompt: "submit", SessionID: uuid, Resume: true, SubmitOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := argvFlagValue(command.Argv, "--resume"); got != uuid || slices.Contains(command.Argv, "--session-id") {
		t.Fatalf("resume argv = %v", command.Argv)
	}
	if denied, _ := argvFlagValue(command.Argv, "--disallowedTools"); !strings.Contains(denied, "Read") || !strings.Contains(denied, "Glob") {
		t.Fatalf("submit-only --disallowedTools = %q", denied)
	}
}

func TestCanResumeClarifySessionClaudeHome(t *testing.T) {
	home := t.TempDir()
	mustWriteClarifyFile(t, filepath.Join(home, ".claude", "projects", "repo", "old-session.jsonl"), "conversation\n")
	if !canResumeClarifySession(home, ".claude", "old-session", 1, 1) || canResumeClarifySession(home, ".codebuddy", "old-session", 1, 1) {
		t.Fatal("resume must look in the clarification CLI's own config directory")
	}
}

// A user key behind a loopback Base URL (a proxy on the user's PC) cannot be
// reached from the platform, so clarification falls back to the platform token.
func TestClarifyClaudeCredentialSkipsLoopbackProxy(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-test")
	for base, wantKey := range map[string]bool{"http://127.0.0.1:8317": false, "http://localhost:8317/v1": false, "http://[::1]:8317": false, "https://api.example.com": true, "": true} {
		got := clarifyClaudeCredential(domain.User{ClaudeCodeAPIKey: "user-key", ClaudeCodeBaseURL: base})
		if (got.APIKey == "user-key") != wantKey || got.OAuthToken != "sk-ant-oat01-test" {
			t.Fatalf("base %q: got key=%q base=%q token set=%v", base, got.APIKey, got.BaseURL, got.OAuthToken != "")
		}
	}
}

// A Claude Code model runs a run eval without a CodeBuddy key and keeps its id.
func TestRunEvalStartsClaudeModelWithoutCodeBuddyKey(t *testing.T) {
	f := cvBNewHTTPFixture(t)
	done := domain.Run{ID: "eval-claude-run", Status: "succeeded", WorkflowID: "missing",
		ExecutionContext: domain.TelemetryContext{ProjectID: "platform"}, Nodes: map[string]*domain.NodeRun{}}
	if err := f.s.PutRun(done); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	setRunEvalExecForTest(func(_ context.Context, rec domain.RunEvalRecord, _ domain.Run, _ domain.User, _ func(string, any)) (domain.RunEvalResult, error) {
		got <- rec.Model
		return domain.RunEvalResult{Score: 90, Passed: true}, nil
	})
	t.Cleanup(func() { setRunEvalExecForTest(nil) })
	if rec := cvBRequest(t, f, http.MethodPost, "/api/runs/eval-claude-run/eval", map[string]any{"model": "gpt-5.6-sol"}, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("CodeBuddy model without a key: status = %d, want 400", rec.Code)
	}
	rec := cvBRequest(t, f, http.MethodPost, "/api/runs/eval-claude-run/eval", map[string]any{"model": "claude-code:claude-opus-5-5"}, true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s, want 202", rec.Code, rec.Body.String())
	}
	if model := <-got; model != "claude-code:claude-opus-5-5" {
		t.Fatalf("eval model = %q", model)
	}
}
