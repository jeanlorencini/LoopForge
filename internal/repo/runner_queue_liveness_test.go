package repo

import (
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

type mysqlLockingTestDialector struct{ gorm.Dialector }

func (mysqlLockingTestDialector) Name() string { return "mysql" }

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

func TestDeregisterRunnerRecordsCauseAndKeepsConcurrentOutcomes(t *testing.T) {
	store := newTestRunner(t)
	// Exercise the MySQL locking branch with SQLite executing the repository calls.
	db := store.DB.Session(&gorm.Session{})
	db.Dialector = mysqlLockingTestDialector{db.Dialector}
	runners := &RunnersRepo{deps: &deps{DB: db}}
	now := time.Now().UTC()
	const runnerID = "removed-runner"
	if err := store.PutRunner(domain.Runner{ID: runnerID, Status: domain.RunnerStatusOnline}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"queued", "claimed", "completed", "requeued"} {
		task := domain.RunnerTask{ID: id, RunnerID: runnerID, Provider: id, MaxAttempts: 2, Invocation: &domain.RunnerInvocation{GitHTTPUserID: 42}}
		if err := store.PutRunnerTask(task); err != nil {
			t.Fatal(err)
		}
	}

	report := domain.RunnerTaskCompletion{Attempt: 1, Status: domain.RunnerTaskSuccess, Result: "verified"}
	interleaved := false
	callback := "test:deregister_claim"
	if err := store.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if interleaved || tx.Statement.Table != "runner_tasks" {
			return
		}
		interleaved = true
		if _, locked := tx.Statement.Clauses["FOR"]; locked {
			t.Error("runner removal takes a locking read of the queued-task index")
		}
		// Use the same SQLite connection to advance the rows after removal has read its queue snapshot.
		activeDB := tx.Session(&gorm.Session{NewDB: true})
		activeDB.Dialector = store.DB.Dialector
		active := &RunnersRepo{deps: &deps{DB: activeDB}}
		for _, id := range []string{"claimed", "completed"} {
			if task, ok, err := active.ClaimRunnerTask(runnerID, []string{id}, now, time.Minute); err != nil || !ok || task.ID != id {
				t.Errorf("interleaved claim of %s: id = %q, claimed = %v, error = %v", id, task.ID, ok, err)
				return
			}
		}
		if changed, err := active.CompleteRunnerTaskAttempt("completed", runnerID, report, now); err != nil || !changed {
			t.Errorf("interleaved completion: changed = %v, error = %v", changed, err)
		}
		if _, ok, err := active.ClaimRunnerTask(runnerID, []string{"requeued"}, now, time.Second); err != nil || !ok {
			t.Errorf("interleaved claim before expiration: ok = %v, error = %v", ok, err)
		}
		if tasks, err := active.ExpireRunnerTasks(now.Add(2 * time.Second)); err != nil || len(tasks) != 1 || tasks[0].ID != "requeued" {
			t.Errorf("interleaved expiration: tasks = %+v, error = %v", tasks, err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := runners.DeregisterRunner(runnerID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.Callback().Query().Remove(callback); err != nil {
		t.Fatal(err)
	}
	if !interleaved {
		t.Fatal("claim interleaving was not exercised")
	}
	claimed, _ := store.RunnerTask("claimed")
	if claimed.Status != domain.RunnerTaskLeased || claimed.LeaseOwner != runnerID || claimed.Attempt != 1 {
		t.Errorf("deregister overwrote a concurrent claim: %+v", claimed)
	}
	completed, _ := store.RunnerTask("completed")
	if completed.Status != domain.RunnerTaskSuccess || completed.Result != "verified" || completed.Attempt != 1 {
		t.Errorf("deregister overwrote a concurrent result: %+v", completed)
	}
	if changed, err := store.CompleteRunnerTaskAttempt("completed", runnerID, report, now); err != nil || changed {
		t.Errorf("stored completion receipt was not preserved: changed = %v, error = %v", changed, err)
	}
	requeued, _ := store.RunnerTask("requeued")
	if requeued.Status != domain.RunnerTaskQueued || requeued.Attempt != 1 {
		t.Errorf("deregister overwrote a newer attempt: %+v", requeued)
	}
	queued, _ := store.RunnerTask("queued")
	if queued.Status != domain.RunnerTaskCanceled || queued.CompletedAt == nil || queued.Invocation.GitHTTPUserID != 0 ||
		!strings.Contains(queued.Error, runnerID) || !strings.Contains(queued.Error, "removed") {
		t.Errorf("queued task did not retain the cause of removal: %+v", queued)
	}
}
