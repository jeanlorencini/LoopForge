package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

func TestScriptExitPolicyUsesProcessStatus(t *testing.T) {
	const output = "stdout claims exit code 78 without a terminal result"
	const diagnosticTail = "end of oversized diagnostic"
	for _, tc := range []struct {
		name         string
		remote       bool
		policy       string
		exit         int
		wantAttempts int
		wantReworks  int
	}{
		{name: "local-configured", policy: `,"nonRetryableExitCodes":[78]`, exit: 78, wantAttempts: 1},
		{name: "remote-configured", remote: true, policy: `,"nonRetryableExitCodes":[78]`, exit: 78, wantAttempts: 1},
		{name: "remote-unlisted", remote: true, policy: `,"nonRetryableExitCodes":[78]`, exit: 74, wantAttempts: 3, wantReworks: 1},
		{name: "remote-prepare-failed", remote: true, policy: `,"nonRetryableExitCodes":[0,78]`, wantAttempts: 3, wantReworks: 1},
		{name: "local-legacy", exit: 78, wantAttempts: 3, wantReworks: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diagnostic := "verification tool unavailable"
			if tc.exit == 0 {
				diagnostic = "workspace preparation failed"
			}
			_, store := cvEEngine(t)
			e := New(store, runtime.NewManager(t.TempDir()))
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := e.Drain(ctx); err != nil {
					t.Error(err)
				}
			})
			var wf domain.WorkflowDefinition
			raw := fmt.Sprintf(`{
				"id":"exit-policy","name":"Exit policy","sandboxProfileId":"local",
				"nodes":[
					{"id":"prepare","name":"Prepare","type":"script","script":"true"},
					{"id":"verify","name":"Verify","type":"script","script":"printf '%s'; exit %d","maxAttempts":2,"maxReworks":1,"rejectTo":"prepare","onFailure":"reject"%s}
				],
				"edges":[{"source":"prepare","target":"verify","when":"success"}]
			}`, output, tc.exit, tc.policy)
			if err := json.Unmarshal([]byte(raw), &wf); err != nil {
				t.Fatal(err)
			}
			if err := store.PutWorkflow(wf); err != nil {
				t.Fatal(err)
			}
			binding := domain.TelemetryContext{ExecutionPinned: true}
			if tc.remote {
				runner := cvERunner(t, e, "exit-runner", domain.RunnerStatusOnline, "terminal")
				binding.ExecutionRunnerID = runner.ID
				e.SetRunnerDispatchNotify(func(provider string) {
					if provider != "terminal" {
						return
					}
					now := time.Now().UTC()
					task, claimed, err := store.ClaimRunnerTask(runner.ID, []string{provider}, now, time.Minute)
					if err != nil || !claimed {
						t.Errorf("claim = %v, error = %v", claimed, err)
						return
					}
					status, result, detail, code := domain.RunnerTaskSuccess, "", "", 0
					if task.NodeID == "verify" {
						status, result, code = domain.RunnerTaskFailed, output, tc.exit
						detail = diagnostic + strings.Repeat(" 工具", 100) + diagnosticTail
						if _, err := store.AppendRunnerTaskEvents(task.ID, []domain.RunnerTaskEvent{{Seq: 1, Type: "stdout", Data: output, Ts: now}}); err != nil {
							t.Errorf("append stdout: %v", err)
						}
					}
					if err := store.CompleteRunnerTask(task.ID, runner.ID, status, &code, result, detail, nil, nil, now); err != nil {
						t.Errorf("complete mocked result: %v", err)
					}
				})
			}
			run, err := e.StartWithExecutionContext(wf.ID, "", domain.RepositoryContext{}, binding)
			if err != nil {
				t.Fatal(err)
			}
			finished := run
			for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
				if live, ok := store.RunState(run.ID); ok {
					finished = live
					if domain.TerminalRunStatus(finished.Status) {
						break
					}
				}
			}
			state := finished.Nodes["verify"]
			upstream := finished.Nodes["prepare"]
			if finished.Status != "failed" || state == nil || upstream == nil || state.Attempt != tc.wantAttempts || upstream.ReworkCount != tc.wantReworks {
				t.Fatalf("run = %s, verify = %+v, prepare = %+v", finished.Status, state, upstream)
			}
			if tc.exit > 0 && !strings.Contains(state.Error, fmt.Sprintf("code %d", tc.exit)) {
				t.Errorf("error should give the actual exit code: %q", state.Error)
			}
			if strings.Contains(state.Error, output) || tc.remote && !strings.Contains(state.Error, diagnostic) {
				t.Errorf("error should retain the runner diagnosis apart from stdout: %q", state.Error)
			}
			if tc.remote && strings.Contains(state.Error, diagnosticTail) {
				t.Errorf("runner diagnosis exceeded the diagnostic limit: %q", state.Error)
			}
			if tc.wantAttempts == 1 && !strings.Contains(state.Error, "retry manually") {
				t.Fatalf("error should explain how to recover: %q", state.Error)
			}
			page, err := store.QueryRunTelemetry(run.ID, "", "", 0, 0, false, false, 500)
			if err != nil {
				t.Fatal(err)
			}
			var prepared, verified bool
			for _, event := range page.Events {
				if event.Name == "script.completed" {
					code, hasExit := event.Attributes["exitCode"].(float64)
					switch event.NodeID {
					case "prepare":
						prepared = true
						if !hasExit || code != 0 {
							t.Errorf("successful script completion exit code = %v", event.Attributes["exitCode"])
						}
					case "verify":
						verified = true
						if hasExit != (tc.exit > 0) || hasExit && code != float64(tc.exit) {
							t.Errorf("failed script completion exit code = %v", event.Attributes["exitCode"])
						}
					}
				}
			}
			if !prepared || !verified {
				t.Errorf("missing persisted script events: prepared = %v, verified = %v", prepared, verified)
			}
			logs, err := store.QueryRunLogs(run.ID, "verify", 0, 0, false, false, 500)
			if err != nil {
				t.Fatal(err)
			}
			var logged bool
			for _, record := range logs.Logs {
				// Login shells can append terminal control bytes to the final stdout line.
				if record.Stream == "stdout" && strings.Contains(record.Body, output) {
					logged = true
					break
				}
			}
			if !logged {
				t.Error("script stdout was missing from the stdout log stream")
			}
		})
	}
}
