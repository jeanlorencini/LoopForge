package runner

import (
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"strings"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

func TestRunnerPreservesProcessExitWhenDockerCleanupFails(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Docker CLI fixture uses an executable POSIX script")
	}
	installFakeDocker(t, `#!/bin/sh
case "$1" in
  inspect) printf '[{"Destination":"/workspace","Source":"%s"}]\n' "$AF_TEST_DOCKER_WORKSPACE" ;;
  start) exit 0 ;;
  exec)
    case "$*" in
      *'cd /workspace && pwd'*) echo /workspace ;;
      *'fixture-command'*)
        if [ "$AF_TEST_DOCKER_EXIT" = missing ]; then
          echo 'Error response from daemon: No such exec instance' >&2
          exit 1
        fi
        exit "$AF_TEST_DOCKER_EXIT"
        ;;
      *) exit 126 ;;
    esac
    ;;
  *) echo "unexpected Docker operation $1" >&2; exit 1 ;;
esac
`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	d := &Daemon{client: NewClient(server.URL), controls: map[string]chan string{}, pendingControls: map[string]string{}}
	for _, tc := range []struct {
		exit string
		want int
	}{{exit: "0", want: 0}, {exit: "78", want: 78}, {exit: "missing", want: -1}} {
		t.Run(tc.exit, func(t *testing.T) {
			workspace := t.TempDir()
			t.Setenv("AF_TEST_DOCKER_WORKSPACE", workspace)
			t.Setenv("AF_TEST_DOCKER_EXIT", tc.exit)
			sandbox, err := runtime.OpenRemoteSandbox(t.Context(), "fixture", workspace, domain.SandboxProfile{Driver: "docker"}, runtime.SandboxRequirements{}, runtime.SandboxResources{Dockerfile: domain.DockerfileDefinition{Content: "FROM scratch"}})
			if err != nil {
				t.Fatal(err)
			}
			result := d.executeSandboxProcess(t.Context(), domain.RunnerTask{ID: "cleanup-fixture"}, sandbox, runtime.CommandSpec{Argv: []string{"fixture-command"}, OwnProcessGroup: true})
			if result.status != domain.RunnerTaskFailed || result.exitCode != tc.want || result.exitCodeObserved != (tc.want >= 0) ||
				!strings.Contains(result.errMsg, "停止 CLI 子进程失败: exit status 126") || strings.Contains(result.errMsg, "outside of container mount namespace") {
				t.Fatalf("status=%s, exit code=%d, observed=%v, error=%q", result.status, result.exitCode, result.exitCodeObserved, result.errMsg)
			}
		})
	}
}
