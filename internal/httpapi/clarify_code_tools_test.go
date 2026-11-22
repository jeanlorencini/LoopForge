package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClarifyCodeToolsStayInsideRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	mustWriteClarifyFile(t, filepath.Join(root, "a", "b.go"), "package b\nfunc Hello() {}\n")
	mustWriteClarifyFile(t, filepath.Join(root, ".git", "config"), "[core]\nfunc Hello\n")
	outside := filepath.Join(base, "secret.txt")
	mustWriteClarifyFile(t, outside, "func Hello secret\n")

	call := func(name string, args map[string]any) (string, error) {
		return callClarifyCodeTool(root, name, args)
	}
	list, err := call("code_list_files", nil)
	if err != nil || !strings.Contains(list, "a/\n") || !strings.Contains(list, "a/b.go") || strings.Contains(list, ".git") {
		t.Fatalf("list = %q, %v", list, err)
	}
	read, err := call("code_read_file", map[string]any{"path": "a/b.go", "offset": float64(2)})
	if err != nil || read != "2\tfunc Hello() {}\n" {
		t.Fatalf("read = %q, %v", read, err)
	}
	if got, err := call("code_read_file", map[string]any{"path": filepath.Join(root, "a", "b.go")}); err != nil || !strings.HasPrefix(got, "1\tpackage b") {
		t.Fatalf("absolute path inside root = %q, %v", got, err)
	}
	hits, err := call("code_search", map[string]any{"pattern": `func Hello`})
	if err != nil || hits != "a/b.go:2: func Hello() {}" {
		t.Fatalf("search = %q, %v", hits, err)
	}
	for _, p := range []string{"../secret.txt", outside, "a/../../secret.txt"} {
		if got, err := call("code_read_file", map[string]any{"path": p}); err == nil {
			t.Fatalf("read %q escaped the root: %q", p, got)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err == nil {
		if got, err := call("code_read_file", map[string]any{"path": "link.txt"}); err == nil {
			t.Fatalf("symlink escaped the root: %q", got)
		}
		if got, _ := call("code_search", map[string]any{"pattern": "secret"}); strings.Contains(got, "secret") && !strings.Contains(got, "没有匹配") {
			t.Fatalf("search followed a symlink out of the root: %q", got)
		}
	}
	if _, err := callClarifyCodeTool("", "code_list_files", nil); err == nil {
		t.Fatal("code tools must refuse without an analysis directory")
	}
}

func TestClarifyCodeToolsOnlyWithCodeRoot(t *testing.T) {
	session := &AgentToolSession{Mode: AgentToolClarify}
	if got := clarifyCodeToolDescriptors(session); len(got) != 0 {
		t.Fatalf("descriptors without a code root = %v", got)
	}
	session.SetCodeRoot(t.TempDir())
	if got := clarifyCodeToolDescriptors(session); len(got) != 3 {
		t.Fatalf("descriptors with a code root = %d, want 3", len(got))
	}
	if got := clarifyCodeToolDescriptors(&AgentToolSession{Mode: AgentToolAssistant, codeRoot: t.TempDir()}); len(got) != 0 {
		t.Fatal("code tools are for clarification only")
	}
}

func TestCanResumeClarifySessionCodexThread(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if err := writeCodexThread(filepath.Dir(home), "session-1", "thread-1"); err != nil {
		t.Fatal(err)
	}
	if !canResumeClarifySession(home, ".codex", "session-1", 1, 1) || canResumeClarifySession(home, ".codex", "session-2", 1, 1) {
		t.Fatal("Codex clarification resumes only a session with a recorded thread")
	}
	if canResumeClarifySession(home, ".codex", "session-1", 2, 1) {
		t.Fatal("another user's session must not resume")
	}
}
