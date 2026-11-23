package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

func TestIdleSharedDockerSandboxRetainsLayer(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Docker CLI fixture uses an executable POSIX script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	d := &Daemon{client: NewClient(server.URL), cfg: Config{DefaultWorkspaceRoot: filepath.Join(t.TempDir(), "runner workspace")}}
	task := domain.RunnerTask{ID: "rt-park-fixture", RunID: "park-fixture", Provider: "terminal", Workspace: "runs/park-fixture/workspace",
		Sandbox: &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: "docker"}, Dockerfile: &domain.DockerfileDefinition{Content: "FROM scratch"}}}
	workspace := d.taskWorkspace(task)
	state := t.TempDir()
	t.Setenv("AF_TEST_PARK_STATE", state)
	t.Setenv("AF_TEST_PARK_WORKSPACE", workspace)
	t.Setenv("AGENTFLOW_BROWSER", "on")
	t.Setenv("AGENTFLOW_BROWSER_IMAGE", "fixture-browser")
	installFakeDocker(t, parkedSandboxDockerCLI)
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(state, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(name string) {
		t.Helper()
		if err := os.Remove(filepath.Join(state, name)); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(state, name))
		return err == nil
	}
	if err := os.WriteFile(filepath.Join(workspace, "sentinel"), []byte("workspace-sentinel\n"), 0600); err != nil {
		t.Fatal(err)
	}
	write("layer", "private-sentinel\n")
	write("id", "original-container\n")
	write("label", workspace+"\n")
	requirements, resources := taskSandboxSpec(task)
	key, _, _ := runSandboxKey(task, requirements, resources)
	acquire := func() (runtime.Sandbox, func()) {
		t.Helper()
		sb, release, err := d.acquireTaskSandbox(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		return sb, release
	}
	command := runtime.CommandSpec{Provider: "terminal", Argv: []string{"sh", "-c", "cat sentinel /reserved/sentinel"}}
	readSentinels := func(sb runtime.Sandbox) {
		t.Helper()
		if out, err := sb.Exec(ctx, command, nil); err != nil || strings.TrimSpace(out) != "workspace-sentinel\nprivate-sentinel" {
			t.Fatalf("container did not retain both layers: %q (%v)", out, err)
		}
	}
	sandbox, release := acquire()
	readSentinels(sandbox)
	if runtime.EnsureSandboxBrowser(ctx, sandbox, workspace, task.RunID, nil) == nil || !exists("browser") {
		t.Fatal("browser sidecar did not start")
	}
	release()
	active, release := acquire()
	d.reapIdleRunSandboxes(ctx, 0)
	if active != sandbox || !exists("running") || !exists("browser") {
		t.Fatal("reaper stopped a sandbox with an active acquisition")
	}
	release()

	write("stop-error", "")
	d.reapIdleRunSandboxes(ctx, 0)
	if !exists("running") || !exists("layer") {
		t.Fatal("failed stop lost the container or its writable layer")
	}
	remove("stop-error")
	write("block-stop", "")
	parked := make(chan struct{})
	go func() {
		d.reapIdleRunSandboxes(ctx, 0)
		close(parked)
	}()
	waitFor(t, 3*time.Second, "docker stop entered", func() bool { return exists("stop-entered") })
	type acquisition struct {
		sandbox runtime.Sandbox
		release func()
		err     error
	}
	acquired := make(chan acquisition, 1)
	go func() {
		sb, releaseNext, err := d.acquireTaskSandbox(ctx, task)
		acquired <- acquisition{sb, releaseNext, err}
	}()
	waitFor(t, 3*time.Second, "acquirer registered", func() bool {
		d.runSandboxMu.Lock()
		defer d.runSandboxMu.Unlock()
		return d.runSandboxes[key] != nil && d.runSandboxes[key].active == 1
	})
	select {
	case <-acquired:
		t.Fatal("acquisition overtook the in-flight stop")
	default:
	}
	remove("block-stop")
	<-parked
	next := <-acquired
	if next.err != nil || next.sandbox != sandbox || exists("running") || exists("browser") || !exists("layer") {
		t.Fatalf("park did not retain the same sandbox and retire its sidecar: %v", next.err)
	}
	write("label", filepath.Join(t.TempDir(), "different-workspace")+"\n")
	if _, err := next.sandbox.Exec(ctx, command, nil); err == nil || !exists("layer") || exists("running") {
		t.Fatalf("resume used a container with a different workspace label: %v", err)
	}
	write("label", workspace+"\n")
	write("id", "different-container\n")
	if _, err := next.sandbox.Exec(ctx, command, nil); err == nil || !exists("layer") || exists("running") {
		t.Fatalf("resume substituted a different container: %v", err)
	}
	write("id", "original-container\n")
	write("start-error", "")
	if _, err := next.sandbox.Exec(ctx, command, nil); err == nil || !exists("layer") {
		t.Fatalf("failed restart removed the layer: %v", err)
	}
	remove("start-error")
	readSentinels(next.sandbox)
	if runtime.EnsureSandboxBrowser(ctx, next.sandbox, workspace, task.RunID, nil) == nil || !exists("browser") {
		t.Fatal("browser sidecar could not resume")
	}
	next.release()
	d.reapIdleRunSandboxes(ctx, 0)
	task.Workspace = "runs/park-fixture"
	if result := d.executeRunCleanup(ctx, task); result.status != domain.RunnerTaskSuccess || exists("id") || exists("layer") || exists("browser") {
		t.Fatalf("cleanup retained the stopped environment: %s %s", result.status, result.errMsg)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("cleanup retained the workspace: %v", err)
	}
}

const parkedSandboxDockerCLI = `#!/bin/sh
set -eu
s=$AF_TEST_PARK_STATE
last=''
for arg; do last=$arg; done
gone() { echo "Error: No such container: $last" >&2; exit 1; }
case "$1" in
inspect)
  case "$last" in
    *-browser) test -f "$s/browser" || gone; echo 'true|fixture-browser|park-fixture'; exit 0 ;;
  esac
  test -f "$s/id" || gone
  case "$*" in
    *'.Mounts'*'agentflow.workspace'*) printf '/vm/guest/workspace\n'; cat "$s/label" ;;
    *'.Mounts'*) printf '[{"Destination":"/workspace","Source":"/vm/guest/workspace"}]\n' ;;
    *'.Id'*) cat "$s/id" ;;
    *'agentflow.workspace'*) cat "$s/label" ;;
    *'.Config.Image'*) echo fixture-image ;;
    *) exit 1 ;;
  esac
  ;;
start)
  test -f "$s/id" || gone
  test ! -f "$s/start-error" || { echo 'fixture start failure' >&2; exit 1; }
  touch "$s/running"
  ;;
stop)
  test -f "$s/id" || gone
  test ! -f "$s/stop-error" || { echo 'fixture stop failure' >&2; exit 1; }
  touch "$s/stop-entered"
  while test -f "$s/block-stop"; do sleep .02; done
  rm -f "$s/running"
  ;;
exec)
  case "$*" in
    *'-browser'*) test -f "$s/browser" ;;
    *'cd /workspace && pwd'*) test -f "$s/running"; echo /workspace ;;
    *) test -f "$s/running"; cat "$AF_TEST_PARK_WORKSPACE/sentinel" "$s/layer" ;;
  esac
  ;;
image) exit 0 ;;
run) test -f "$s/running"; touch "$s/browser" ;;
rm)
  case "$last" in
    *-scripts) gone ;;
    *-browser) rm -f "$s/browser" ;;
    *) rm -f "$s/id" "$s/running" "$s/layer" ;;
  esac
  ;;
ps) if test -f "$s/id"; then cat "$s/id"; fi ;;
*) echo "unexpected Docker operation $1" >&2; exit 1 ;;
esac
`
