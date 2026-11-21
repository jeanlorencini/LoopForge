package httpapi

import (
	"os"
	"testing"
)

// Tests isolate assistant state by pointing AGENTFLOW_ASSISTANT_SESSION_DIR at
// a temporary directory. Conversation roots follow it only while
// AGENTFLOW_CONVERSATION_DIR is unset, so a value from the shell would send
// them to a real directory shared by every test.
func TestMain(m *testing.M) {
	os.Unsetenv("AGENTFLOW_CONVERSATION_DIR")
	// Claude Code falls back to this platform token; tests that need it set it.
	os.Unsetenv("CLAUDE_CODE_OAUTH_TOKEN")
	os.Exit(m.Run())
}
