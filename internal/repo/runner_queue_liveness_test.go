package repo

import (
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestRunnerQueuedUnavailabilityKeepsHealthyAndClaimedWork(t *testing.T) {
	store := newTestRunner(t)
	now := time.Date(2026, 11, 20, 13, 0, 0, 0, time.UTC)
	lastSeen := now.Add(-90 * time.Second)
	runner := domain.Runner{ID: "bound", Name: "Bound", Status: domain.RunnerStatusOnline, LastSeenAt: &lastSeen}
	if err := store.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	task := domain.RunnerTask{ID: "busy", RunnerID: runner.ID, Provider: "terminal", CreatedAt: now.Add(-time.Hour)}
	if err := store.PutRunnerTask(task); err != nil {
		t.Fatal(err)
	}
	if failed, err := store.FailQueuedRunnerTaskIfUnavailable(task.ID, runner.ID, now); err != nil || failed {
		t.Fatalf("healthy runner at heartbeat cutoff: failed = %v, err = %v", failed, err)
	}
	providers := []string{task.Provider}
	if _, claimed, err := store.ClaimRunnerTask("replacement", providers, now, time.Minute); err != nil || claimed {
		t.Fatalf("unbound runner claim = %v, err = %v", claimed, err)
	}
	if _, claimed, err := store.ClaimRunnerTask(runner.ID, providers, now, time.Minute); err != nil || !claimed {
		t.Fatalf("bound runner claim = %v, err = %v", claimed, err)
	}
	if err := store.MarkRunnerOffline(runner.ID, now); err != nil {
		t.Fatal(err)
	}
	if failed, err := store.FailQueuedRunnerTaskIfUnavailable(task.ID, runner.ID, now); err != nil || failed {
		t.Fatalf("leased task: failed = %v, err = %v", failed, err)
	}
	if err := store.CompleteRunnerTask(task.ID, runner.ID, domain.RunnerTaskSuccess, nil, "kept result", "", nil, nil, now); err != nil {
		t.Fatal(err)
	}
	if failed, err := store.FailQueuedRunnerTaskIfUnavailable(task.ID, runner.ID, now); err != nil || failed {
		t.Fatalf("terminal task: failed = %v, err = %v", failed, err)
	}
	terminal, _ := store.RunnerTask(task.ID)
	if terminal.Status != domain.RunnerTaskSuccess || terminal.Result != "kept result" {
		t.Fatalf("terminal result changed: %+v", terminal)
	}
	task.ID = "unavailable"
	task.Invocation = &domain.RunnerInvocation{GitSSHUserID: 42}
	if err := store.PutRunnerTask(task); err != nil {
		t.Fatal(err)
	}
	if failed, err := store.FailQueuedRunnerTaskIfUnavailable(task.ID, runner.ID, now); err != nil || !failed {
		t.Fatalf("offline queue: failed = %v, err = %v", failed, err)
	}
	terminal, _ = store.RunnerTask(task.ID)
	if terminal.Status != domain.RunnerTaskFailed || terminal.Invocation == nil || terminal.Invocation.GitSSHUserID != 0 {
		t.Fatalf("unavailable task did not settle and scrub its credentials: %+v", terminal)
	}
	if _, claimed, err := store.ClaimRunnerTask(runner.ID, providers, now, time.Minute); err != nil || claimed {
		t.Fatalf("claim after unavailable queue settled = %v, err = %v", claimed, err)
	}
}
