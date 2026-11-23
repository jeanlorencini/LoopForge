package engine

import (
	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func completedRecoveryRun(id string) domain.Run {
	wf := domain.WorkflowDefinition{ID: id, Nodes: []domain.WorkflowNode{{ID: "close", Type: "command"}}}
	return domain.Run{ID: id, WorkflowID: wf.ID, WorkflowSnapshot: wf, Status: "running", Nodes: map[string]*domain.NodeRun{
		"close": {NodeID: "close", Status: "succeeded", Attempt: 1},
	}}
}

func TestResumePausedCompletedGraphWithExecution(t *testing.T) {
	e, s := newRecoveryEngine(t)
	r := completedRecoveryRun("resume-completed")
	r.Status, r.PauseRequested = "waiting", true
	if err := s.PutRun(r); err != nil {
		t.Fatal(err)
	}
	if err := e.ResumePaused(r.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.RunState(r.ID); got.Status == "waiting" || got.PauseRequested {
		t.Fatalf("resume returned with status=%s, pause=%v", got.Status, got.PauseRequested)
	}
	got := waitRun(t, s, r.ID, 10*time.Second, func(run domain.Run) bool { return run.Status == "succeeded" && run.OwnerInstanceID == "" })
	if got.Nodes["close"].Attempt != 1 {
		t.Fatal("resume repeated the completed node")
	}
}

func TestLeasedResumeDecisionRequiresLiveOwner(t *testing.T) {
	for _, state := range []string{"live", "released", "expired", "taken over"} {
		t.Run(state, func(t *testing.T) {
			e, s := newRecoveryEngine(t)
			r := completedRecoveryRun("paused-decision")
			r.Status, r.PauseRequested = "waiting", true
			if state == "expired" {
				r.OwnerInstanceID, r.Generation, r.LeaseUntil = e.instanceID, 1, time.Now().Add(-time.Minute)
			}
			if err := s.PutRun(r); err != nil {
				t.Fatal(err)
			}
			if state != "expired" {
				if err := s.TryAcquireRunLease(r.ID, e.instanceID, 0, time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, _ := s.RunState(r.ID)
			if state == "released" || state == "taken over" {
				if err := s.ReleaseRunLease(r.ID, e.instanceID, snapshot.Generation); err != nil {
					t.Fatal(err)
				}
			}
			if state == "taken over" {
				if err := s.TryAcquireRunLease(r.ID, "replacement-worker", snapshot.Generation, time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			snapshot.Status, snapshot.PauseRequested = "running", false
			err := s.PutLeasedRunAtRevision(snapshot, e.instanceID)
			got, _ := s.RunState(r.ID)
			if state == "live" {
				if err != nil || got.Status != "running" || got.PauseRequested {
					t.Fatalf("resume commit failed: err=%v, status=%s, paused=%t", err, got.Status, got.PauseRequested)
				}
				return
			}
			if !errors.Is(err, repo.ErrRunLeaseLost) || got.Status != "waiting" || !got.PauseRequested || got.Revision != snapshot.Revision {
				t.Fatalf("stale decision changed durable state: err=%v, status=%s, paused=%t, revision=%d", err, got.Status, got.PauseRequested, got.Revision)
			}
		})
	}
}

func TestRecoverCompletedGraphHonorsAcceptedControl(t *testing.T) {
	for _, control := range []struct {
		name, status string
		request      func(*repo.Store, string) (domain.Run, error)
	}{
		{"pause", "waiting", func(s *repo.Store, id string) (domain.Run, error) { return s.RequestRunPause(id) }},
		{"cancel", "cancelled", func(s *repo.Store, id string) (domain.Run, error) { return s.RequestRunCancel(id) }},
	} {
		t.Run(control.name, func(t *testing.T) {
			e, s := newRecoveryEngine(t)
			r := completedRecoveryRun("recover-completed")
			r.OwnerInstanceID, r.LeaseUntil, r.Generation = "retired", time.Now().Add(-time.Minute), 1
			if err := s.PutRun(r); err != nil {
				t.Fatal(err)
			}
			if _, err := control.request(s, r.ID); err != nil {
				t.Fatal(err)
			}
			if resumed := e.ResumeInterruptedRuns(); len(resumed) != 0 {
				t.Fatalf("unexpected scheduler: %v", resumed)
			}
			got, _ := s.RunState(r.ID)
			if got.Status != control.status || got.Nodes["close"].Status != "succeeded" {
				t.Fatalf("accepted %s left run=%s node=%s", control.name, got.Status, got.Nodes["close"].Status)
			}
		})
	}
}

func TestFinishCompletedGraphHonorsAcceptedControl(t *testing.T) {
	e, s := newRecoveryEngine(t)
	r := completedRecoveryRun("finish-completed")
	if err := s.PutRun(r); err != nil {
		t.Fatal(err)
	}
	if err := s.TryAcquireRunLease(r.ID, e.instanceID, r.Generation, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestRunPause(r.ID); err != nil {
		t.Fatal(err)
	}
	e.finishAt(r.ID, 1, "succeeded", "graph completed")
	got, _ := s.RunState(r.ID)
	if got.Status != "waiting" || !got.PauseRequested || got.Nodes["close"].Status != "succeeded" {
		t.Fatalf("accepted pause left run=%s pause=%v node=%s", got.Status, got.PauseRequested, got.Nodes["close"].Status)
	}
}

func TestRecoverCompletedScheduledRunInExistingCheckout(t *testing.T) {
	e, s := newRecoveryEngine(t)
	work, remote := setupIntegrateRepo(t)
	r := completedRecoveryRun("recover-scheduled")
	r.ExecutionContext.ScheduleJobID = "job-fixture"
	r.Repository = domain.RepositoryContext{RepositoryID: "fixture", CloneURL: remote, WorkBranch: "feature/fix-issue-30", IntegrateBranch: "agentflow/integration", DefaultBranch: "master"}
	r.OwnerInstanceID, r.LeaseUntil, r.Generation = "retired", time.Now().Add(-time.Minute), 1
	r.WorkspaceHost, r.WorkspaceRoot = e.workspaceHost, e.sandboxes.Root()
	master := testGit(t, remote, "rev-parse", "refs/heads/master")
	workspace := e.sandboxes.RunWorkspace(r.ID)
	if err := os.Rename(work, workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "recovered.txt"), []byte("only in the recovered checkout\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testGit(t, workspace, "add", "recovered.txt")
	testGit(t, workspace, "commit", "-m", "checkpointed change")
	if err := s.PutRun(r); err != nil {
		t.Fatal(err)
	}
	if resumed := e.ResumeInterruptedRuns(); len(resumed) != 1 || resumed[0] != r.ID {
		t.Fatalf("resumed=%v", resumed)
	}
	got := waitRun(t, s, r.ID, 45*time.Second, func(run domain.Run) bool { return domain.TerminalRunStatus(run.Status) && run.OwnerInstanceID == "" })
	if got.Status != "succeeded" || got.Nodes["close"].Attempt != 1 {
		t.Fatalf("status=%s attempt=%d events=%v", got.Status, got.Nodes["close"].Attempt, got.Events)
	}
	if got := testGit(t, remote, "show", "refs/heads/agentflow/integration:recovered.txt"); got != "only in the recovered checkout" {
		t.Fatalf("integration content=%q", got)
	}
	if got := testGit(t, remote, "rev-parse", "refs/heads/master"); got != master {
		t.Fatal("integration moved the default branch")
	}
}

func TestRunnerTaskSurvivesWorkerTakeover(t *testing.T) {
	for _, state := range []string{"queued", "running", "completed"} {
		t.Run(state, func(t *testing.T) {
			a, s := newRecoveryEngine(t)
			a.ConfigureExecution("a", "recovery-host", time.Minute, true)
			b := New(s, runtime.NewManager(a.sandboxes.Root()))
			b.ConfigureExecution("b", "recovery-host", time.Minute, true)
			t.Cleanup(func() { drainRecoveryFixture(t, b) })
			const runnerID = "terminal-runner"
			if err := s.PutRunner(domain.Runner{ID: runnerID, Status: domain.RunnerStatusOnline, Runtimes: []domain.RunnerRuntime{{Provider: "terminal"}}}); err != nil {
				t.Fatal(err)
			}
			wf := domain.WorkflowDefinition{ID: "runner-wf", SandboxProfileID: "local", Nodes: []domain.WorkflowNode{{ID: "script", Type: "command", Command: "true"}}}
			r := domain.Run{ID: "runner-recovery", Status: "running", WorkflowID: wf.ID, WorkflowSnapshot: wf, Nodes: map[string]*domain.NodeRun{"script": {NodeID: "script", Status: "pending"}}, ExecutionContext: domain.TelemetryContext{
				ExecutionRunnerID: runnerID, ExecutionPinned: true, ExecutionMode: domain.ExecutionModeRunner, RunnerWorkspaceLayout: domain.RunnerWorkspaceLayoutShared,
			}}
			if err := s.PutRun(r); err != nil {
				t.Fatal(err)
			}
			a.ResumeInterruptedRuns()
			var original domain.RunnerTask
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				tasks, err := s.ActiveRunnerTasksForRun(r.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(tasks) == 1 {
					original = tasks[0]
					break
				}
			}
			if original.ID == "" {
				got, _ := s.RunState(r.ID)
				t.Fatalf("original worker did not dispatch: status=%s node=%+v", got.Status, got.Nodes["script"])
			}
			claim := func() {
				t.Helper()
				leased, ok, err := s.ClaimRunnerTask(runnerID, []string{"terminal"}, time.Now().UTC(), time.Minute)
				if err != nil || !ok || leased.ID != original.ID {
					t.Fatalf("claim=%t task=%s err=%v, want %s", ok, leased.ID, err, original.ID)
				}
				if err := s.StartRunnerTask(leased.ID, runnerID, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			complete := func() {
				t.Helper()
				changed, err := s.CompleteRunnerTaskAttempt(original.ID, runnerID, domain.RunnerTaskCompletion{Attempt: 1, Status: domain.RunnerTaskSuccess, Result: "original execution result"}, time.Now().UTC())
				if err != nil || !changed {
					t.Fatalf("complete=%t err=%v", changed, err)
				}
			}
			if state != "queued" {
				claim()
			}
			drainRecoveryFixture(t, a)
			parked, _ := s.RunnerTask(original.ID)
			if parked.DesiredState != "" || (state == "queued" && parked.Status != domain.RunnerTaskQueued) || (state != "queued" && (parked.Status != domain.RunnerTaskRunning || parked.LeaseOwner != runnerID)) {
				t.Fatalf("drain changed original task: status=%s desired=%s owner=%s", parked.Status, parked.DesiredState, parked.LeaseOwner)
			}
			if state == "completed" {
				complete()
				if err := s.MarkRunnerOffline(runnerID, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			b.ResumeInterruptedRuns()
			waitRun(t, s, r.ID, 5*time.Second, func(run domain.Run) bool {
				return run.OwnerInstanceID == "b" && run.Nodes["script"].Status == "running" || domain.TerminalRunStatus(run.Status)
			})
			if state == "queued" {
				claim()
			}
			if state != "completed" {
				complete()
				b.NotifyRunnerTaskChanged(original.ID)
			}
			got := waitRun(t, s, r.ID, 5*time.Second, func(run domain.Run) bool { return domain.TerminalRunStatus(run.Status) })
			if got.Status != "succeeded" || got.Nodes["script"].Attempt != 1 || got.Nodes["script"].RunnerAttemptID == "" || got.Nodes["script"].Output != "original execution result" {
				t.Fatalf("status=%s node=%+v", got.Status, got.Nodes["script"])
			}
			for _, task := range s.RunnerTasks() {
				if task.RunID == r.ID && task.NodeID == "script" && task.ID != original.ID {
					t.Fatalf("task %s duplicated %s", task.ID, original.ID)
				}
			}
		})
	}
}

func TestRecoverLegacyRunnerTaskOrStopForExplicitRetry(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			e, s := newRecoveryEngine(t)
			runner := cvERunner(t, e, "legacy-runner", domain.RunnerStatusOnline, "terminal")
			node := domain.WorkflowNode{ID: "script", Type: "command", Command: "true", MaxAttempts: 3}
			r := runnerRecoveryRun("legacy-recovery", runner.ID, node)
			r.WorkflowSnapshot.Nodes = append(r.WorkflowSnapshot.Nodes, domain.WorkflowNode{ID: "prepare", Type: "command"})
			r.WorkflowSnapshot.Edges = []domain.WorkflowEdge{{Source: "prepare", Target: node.ID, When: "success"}}
			r.Nodes["prepare"] = &domain.NodeRun{NodeID: "prepare", Status: "succeeded", Attempt: 1}
			r.Nodes[node.ID] = &domain.NodeRun{NodeID: node.ID, Attempt: 1, Status: "running", StartedAt: time.Now().UTC().Add(-time.Minute)}
			if err := s.PutRun(r); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < count; i++ {
				legacy := domain.RunnerTask{
					ID: fmt.Sprintf("rt-%d-script-script", time.Now().UnixNano()), RunID: r.ID, NodeID: node.ID, RunnerID: runner.ID,
					Provider: "terminal", Command: bindExecutionTarget(node.Command, executionTarget{Scope: domain.ExecutionScopeRun}),
					Workspace: runnerWorkspaceRel(r, node.ID), Invocation: &domain.RunnerInvocation{RunID: r.ID, NodeID: node.ID},
				}
				if err := s.PutRunnerTask(legacy); err != nil {
					t.Fatal(err)
				}
				task := claimRunnerRecoveryFixture(t, s, runner.ID, "terminal")
				completeRunnerRecoveryFixture(t, e, task, "verified legacy result", nil)
			}
			e.ResumeInterruptedRuns()
			got := waitRun(t, s, r.ID, 8*time.Second, func(run domain.Run) bool { return domain.TerminalRunStatus(run.Status) })
			if count == 1 {
				if got.Status != "succeeded" || got.Nodes[node.ID].Attempt != 1 || got.Nodes[node.ID].Output != "verified legacy result" {
					t.Fatalf("legacy task was not reused: %+v", got.Nodes[node.ID])
				}
				return
			}
			if got.Status != "failed" || got.Nodes[node.ID].Attempt != 1 || !strings.Contains(got.Nodes[node.ID].Error, "retry manually") {
				t.Fatalf("legacy ambiguity was not diagnosed: %+v", got.Nodes[node.ID])
			}
			if len(s.RunnerTasks()) != count {
				t.Fatal("legacy ambiguity enqueued another task")
			}
			if err := e.Retry(r.ID, node.ID, "reviewed legacy history", ""); err != nil {
				t.Fatal(err)
			}
			fresh := claimRunnerRecoveryFixture(t, s, runner.ID, "terminal")
			completeRunnerRecoveryFixture(t, e, fresh, "new attempt result", nil)
			got = waitRun(t, s, r.ID, 8*time.Second, func(run domain.Run) bool { return run.Status == "succeeded" && run.OwnerInstanceID == "" })
			if got.Nodes[node.ID].Attempt != 2 || got.Nodes[node.ID].RunnerAttemptID == "" || got.Nodes[node.ID].Output != "new attempt result" {
				t.Fatalf("explicit retry did not start a new attempt: %+v", got.Nodes[node.ID])
			}
		})
	}
}

func TestRecoverRunnerAgentAndMultipleGatesInOneAttempt(t *testing.T) {
	a, s := newRecoveryEngine(t)
	a.ConfigureExecution("worker-a", "fixture-host", time.Minute, true)
	b := New(s, runtime.NewManager(a.sandboxes.Root()))
	b.ConfigureExecution("worker-b", "fixture-host", time.Minute, true)
	t.Cleanup(func() { drainRecoveryFixture(t, b) })
	runner := cvERunner(t, a, "gate-runner", domain.RunnerStatusOnline, "terminal", "codex")
	runner.RunnerVersion = domain.RunnerVersionReportedOutputs
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	agent := domain.AgentDefinition{ID: "gate-agent", Name: "Gate agent", ExecutorID: "codex", Verification: []domain.VerificationGate{
		{ID: "first", Name: "first", Mode: "command", Commands: []string{"true"}},
		{ID: "second", Name: "second", Mode: "command", Commands: []string{"true"}},
	}}
	if err := s.PutAgent(agent); err != nil {
		t.Fatal(err)
	}
	node := domain.WorkflowNode{ID: "agent", Type: "agent", AgentID: agent.ID}
	r := runnerRecoveryRun("recover-gates", runner.ID, node)
	if err := s.PutRun(r); err != nil {
		t.Fatal(err)
	}
	a.ResumeInterruptedRuns()
	invocation := claimRunnerRecoveryFixture(t, s, runner.ID, "codex")
	completeRunnerRecoveryFixture(t, a, invocation, "original agent result", &domain.RunnerTaskOutputs{Files: map[string]string{invocation.Invocation.GateResultPath: `{"decision":"pass","reason":"fixture passed"}`}})
	first := claimRunnerRecoveryFixture(t, s, runner.ID, "terminal")
	completeRunnerRecoveryFixture(t, a, first, "first gate result", nil)
	second := claimRunnerRecoveryFixture(t, s, runner.ID, "terminal")
	if first.ID == second.ID || first.Command != second.Command {
		t.Fatal("fixture must have two distinct operations with identical commands")
	}
	drainRecoveryFixture(t, a)
	b.ResumeInterruptedRuns()
	completeRunnerRecoveryFixture(t, b, second, "second gate result", nil)
	got := waitRun(t, s, r.ID, 8*time.Second, func(run domain.Run) bool { return domain.TerminalRunStatus(run.Status) })
	if got.Status != "succeeded" || got.Nodes[node.ID].Attempt != 1 || got.Nodes[node.ID].Output != "original agent result" {
		t.Fatalf("recovered gate sequence did not finish: %+v", got.Nodes[node.ID])
	}
	want := map[string]bool{invocation.ID: true, first.ID: true, second.ID: true}
	for _, task := range s.RunnerTasks() {
		if task.RunID == r.ID && task.NodeID == node.ID && !want[task.ID] {
			t.Fatalf("replay created a duplicate operation %s", task.ID)
		}
	}
}

func TestRetryWithReusedNodeCounterGetsDistinctRunnerTask(t *testing.T) {
	e, s := newRecoveryEngine(t)
	runner := cvERunner(t, e, "retry-runner", domain.RunnerStatusOnline, "terminal")
	node := domain.WorkflowNode{ID: "script", Type: "command", Command: "true"}
	r := runnerRecoveryRun("retry-counter", runner.ID, node)
	r.WorkflowSnapshot.Nodes = append(r.WorkflowSnapshot.Nodes, domain.WorkflowNode{ID: "prepare", Type: "command"})
	r.WorkflowSnapshot.Edges = []domain.WorkflowEdge{{Source: "prepare", Target: node.ID, When: "success"}}
	r.Nodes["prepare"] = &domain.NodeRun{NodeID: "prepare", Status: "succeeded", Attempt: 1}
	if err := s.PutRun(r); err != nil {
		t.Fatal(err)
	}
	e.ResumeInterruptedRuns()
	original := claimRunnerRecoveryFixture(t, s, runner.ID, "terminal")
	changed, err := s.CompleteRunnerTaskAttempt(original.ID, runner.ID, domain.RunnerTaskCompletion{Attempt: 1, Status: domain.RunnerTaskFailed, Error: "first execution failed"}, time.Now().UTC())
	if err != nil || !changed {
		t.Fatalf("report first attempt: changed=%t err=%v", changed, err)
	}
	e.NotifyRunnerTaskChanged(original.ID)
	before := waitRun(t, s, r.ID, 8*time.Second, func(run domain.Run) bool { return run.Status == "failed" && run.OwnerInstanceID == "" })
	identity := before.Nodes[node.ID].RunnerAttemptID
	if before.Nodes[node.ID].Attempt != 1 || identity == "" {
		t.Fatalf("first attempt identity did not survive persistence: %+v", before.Nodes[node.ID])
	}
	// Model a persisted node counter reset while retaining its old task history.
	before.Nodes[node.ID].Attempt = 0
	if err := s.PutRun(before); err != nil {
		t.Fatal(err)
	}
	if err := e.Retry(r.ID, node.ID, "reviewed previous result", ""); err != nil {
		t.Fatal(err)
	}
	retry := claimRunnerRecoveryFixture(t, s, runner.ID, "terminal")
	if retry.ID == original.ID {
		t.Fatal("retry reattached the previous physical task")
	}
	completeRunnerRecoveryFixture(t, e, retry, "fresh result", nil)
	after := waitRun(t, s, r.ID, 8*time.Second, func(run domain.Run) bool { return run.Status == "succeeded" && run.OwnerInstanceID == "" })
	if nr := after.Nodes[node.ID]; nr.Attempt != 1 || nr.RunnerAttemptID == "" || nr.RunnerAttemptID == identity || nr.Output != "fresh result" {
		t.Fatalf("retry used the previous attempt identity or result: %+v", nr)
	}
}

func runnerRecoveryRun(id, runnerID string, node domain.WorkflowNode) domain.Run {
	wf := domain.WorkflowDefinition{ID: id, SandboxProfileID: "local", Nodes: []domain.WorkflowNode{node}}
	return domain.Run{ID: id, Status: "running", WorkflowID: wf.ID, WorkflowSnapshot: wf,
		Nodes: map[string]*domain.NodeRun{node.ID: {NodeID: node.ID, Status: "pending"}}, ExecutionContext: domain.TelemetryContext{
			ExecutionRunnerID: runnerID, ExecutionPinned: true, ExecutionMode: domain.ExecutionModeRunner, RunnerWorkspaceLayout: domain.RunnerWorkspaceLayoutShared,
		}}
}

func claimRunnerRecoveryFixture(t *testing.T, s *repo.Store, runnerID, provider string) domain.RunnerTask {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		task, found, err := s.ClaimRunnerTask(runnerID, []string{provider}, time.Now().UTC(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			if err := s.StartRunnerTask(task.ID, runnerID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			return task
		}
	}
	t.Fatalf("runner did not receive a %s task", provider)
	return domain.RunnerTask{}
}

func completeRunnerRecoveryFixture(t *testing.T, e *Engine, task domain.RunnerTask, result string, outputs *domain.RunnerTaskOutputs) {
	t.Helper()
	changed, err := e.repo.CompleteRunnerTaskAttempt(task.ID, task.RunnerID, domain.RunnerTaskCompletion{
		Attempt: task.Attempt, Status: domain.RunnerTaskSuccess, Result: result, Outputs: outputs,
	}, time.Now().UTC())
	if err != nil || !changed {
		t.Fatalf("complete %s changed=%t err=%v", task.ID, changed, err)
	}
	e.NotifyRunnerTaskChanged(task.ID)
}

func drainRecoveryFixture(t *testing.T, e *Engine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.Drain(ctx); err != nil {
		t.Fatal(err)
	}
}
