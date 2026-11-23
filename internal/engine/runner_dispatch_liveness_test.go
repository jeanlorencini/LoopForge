package engine

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
	"gorm.io/gorm"
)

func TestQueuedGateFailsForUnavailableRunner(t *testing.T) {
	for _, reason := range []string{"missing", "stale", "offline"} {
		t.Run(reason, func(t *testing.T) {
			e, store := cvEEngine(t)
			const runnerID = "bound-runner"
			if reason != "missing" {
				runner := cvERunner(t, e, runnerID, domain.RunnerStatusOnline, "terminal")
				e.SetRunnerDispatchNotify(func(string) {
					now := time.Now().UTC()
					var err error
					if reason == "stale" {
						lastSeen := now.Add(-2 * time.Minute)
						runner.LastSeenAt = &lastSeen
						err = store.PutRunner(runner)
					} else {
						err = store.MarkRunnerOffline(runnerID, now)
					}
					if err != nil {
						t.Error(err)
					}
				})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_, err := e.dispatchRunnerCommand(ctx, runnerID, "run", "gate", "workspace", "true", nil)
			if err == nil || !strings.Contains(err.Error(), runnerID) || !strings.Contains(err.Error(), "reconnect") {
				t.Fatalf("unavailable bound runner should fail with recovery guidance, got %v", err)
			}
			tasks := store.RunnerTasks()
			if len(tasks) != 1 || tasks[0].Status != domain.RunnerTaskFailed || tasks[0].Attempt != 0 ||
				tasks[0].ExitCode != nil || tasks[0].CompletedAt == nil || tasks[0].Error != err.Error() {
				t.Fatalf("expected durable failure before claim: %+v", tasks)
			}
		})
	}
}

func TestUnavailableRunnerClassificationPreservesDatabaseReadErrors(t *testing.T) {
	e, store := cvEEngine(t)
	want := errors.New("fixture runner read failed")
	if err := store.DB.Callback().Query().Before("gorm:query").Register("fixture:runner_read_error", func(tx *gorm.DB) {
		if tx.Statement.Table == "runners" {
			tx.AddError(want)
		}
	}); err != nil {
		t.Fatal(err)
	}
	err := e.requireRunnerProvider("bound-runner", "terminal")
	if !errors.Is(err, want) || isRunnerUnavailableBeforeClaim(err) {
		t.Fatalf("expected the storage error, got %v", err)
	}
}

func TestUnavailableBoundRunnerDoesNotRetryOrRejectNode(t *testing.T) {
	for _, tc := range []struct {
		name, gateFailure string
	}{
		{name: "missing-agent"},
		{name: "stale-agent"},
		{name: "removed-gate", gateFailure: "loop"},
		{name: "offline-gate", gateFailure: "reject"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store := cvEEngine(t)
			e := New(store, runtime.NewManager(t.TempDir()))
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := e.Drain(ctx); err != nil {
					t.Error(err)
				}
			})
			runner := cvERunner(t, e, "bound-runner", domain.RunnerStatusOnline, "terminal", "mock")
			agent := domain.AgentDefinition{ID: "fixture-agent", Name: "Fixture", ExecutorID: "mock"}
			if tc.gateFailure == "loop" {
				agent.Loop = domain.AgentLoopPolicy{Enabled: true, MaxIterations: 3, OnExhausted: "human"}
			}
			if tc.gateFailure != "" {
				agent.Verification = []domain.VerificationGate{{ID: "gate-check", Name: "Gate check", Mode: "command", Commands: []string{"echo gate-check"}, ExecutionScope: "run", Required: true, OnFailure: tc.gateFailure}}
			}
			if err := store.PutAgent(agent); err != nil {
				t.Fatal(err)
			}
			wf := domain.WorkflowDefinition{ID: "runner-liveness", Name: "Runner liveness", SandboxProfileID: "local",
				Nodes: []domain.WorkflowNode{
					{ID: "prepare", Type: "script", Script: "true"},
					{ID: "verify", Type: "agent", AgentID: agent.ID, MaxAttempts: 3, MaxReworks: 1, RejectTo: "prepare", OnFailure: "reject"},
				}, Edges: []domain.WorkflowEdge{{Source: "prepare", Target: "verify", When: "success"}}}
			if err := store.PutWorkflow(wf); err != nil {
				t.Fatal(err)
			}
			var disconnected, reconnected atomic.Bool
			disconnect := func(now time.Time) {
				var err error
				switch tc.name {
				case "stale-agent":
					stale := runner
					lastSeen := now.Add(-2 * time.Minute)
					stale.LastSeenAt = &lastSeen
					err = store.PutRunner(stale)
				case "offline-gate":
					err = store.MarkRunnerOffline(runner.ID, now)
				default:
					err = store.DeregisterRunner(runner.ID, now)
				}
				if err != nil {
					t.Error(err)
				}
				disconnected.Store(true)
			}
			e.SetRunnerDispatchNotify(func(provider string) {
				if disconnected.Load() && !reconnected.Load() {
					return
				}
				now := time.Now().UTC()
				if provider == "terminal" && tc.gateFailure != "" && !reconnected.Load() {
					for _, task := range store.RunnerTasks() {
						if task.Status == domain.RunnerTaskQueued && strings.Contains(task.Command, "echo gate-check") {
							disconnect(now)
							return
						}
					}
				}
				task, claimed, err := store.ClaimRunnerTask(runner.ID, []string{provider}, now, time.Minute)
				if err != nil || !claimed {
					t.Errorf("claim %s: claimed=%v, error=%v", provider, claimed, err)
					return
				}
				var artifacts map[string]string
				var outputs *domain.RunnerTaskOutputs
				if provider == "mock" {
					path := task.Invocation.GateResultPath
					const passed = `{"decision":"pass","reason":"fixture"}`
					artifacts = map[string]string{gateResultContract(path).Name: passed}
					outputs = &domain.RunnerTaskOutputs{Files: map[string]string{path: passed}}
				}
				if err := store.CompleteRunnerTask(task.ID, runner.ID, domain.RunnerTaskSuccess, nil, "", "", artifacts, outputs, now); err != nil {
					t.Error(err)
				}
				if tc.gateFailure == "" && task.NodeID == "prepare" && !reconnected.Load() {
					disconnect(now)
				}
			})
			run, err := e.StartWithExecutionContext(wf.ID, "", domain.RepositoryContext{}, domain.TelemetryContext{ExecutionPinned: true, ExecutionRunnerID: runner.ID})
			if err != nil {
				t.Fatal(err)
			}
			failed := waitRun(t, store, run.ID, 15*time.Second, func(r domain.Run) bool {
				return domain.TerminalRunStatus(r.Status) || r.Status == "waiting"
			})
			verify, prepare := failed.Nodes["verify"], failed.Nodes["prepare"]
			if failed.Status != "failed" || verify.Status != "failed" || verify.Attempt != 1 || prepare.ReworkCount != 0 ||
				!strings.Contains(verify.Error, runner.ID) || !strings.Contains(verify.Error, "reconnect") {
				t.Fatalf("run=%s, prepare=%+v, verify=%+v", failed.Status, prepare, verify)
			}
			for _, event := range failed.Telemetry {
				if event.NodeID == "verify" && event.Name == "agent.loop" && event.Phase == "progress" {
					t.Fatal("unavailable runner caused an extra agent iteration")
				}
			}
			if err := store.PutRunner(runner); err != nil {
				t.Fatal(err)
			}
			reconnected.Store(true)
			if err := e.Retry(run.ID, "verify", "runner reconnected", ""); err != nil {
				t.Fatal(err)
			}
			waitFor(t, store, run.ID, "succeeded")
		})
	}
}
