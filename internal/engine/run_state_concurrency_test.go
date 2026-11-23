package engine

import (
	"errors"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

func TestRunProgressKeepsConcurrentApproval(t *testing.T) {
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	worker, api := leaseEngine(t, store, "progress-worker"), leaseEngine(t, store, "control-api")
	api.ConfigureExecution("control-api", "control-host", time.Minute, false)
	run := leaseRun(t, store, "approval-progress", "waiting", "waiting_approval")
	if err := store.TryAcquireRunLease(run.ID, "progress-worker", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	read, proceed, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- worker.updateRunErr(run.ID, func(r *domain.Run) {
			close(read)
			<-proceed
			r.Nodes["node"].Activity = "worker progress"
			worker.event(r, "node", "node.progress", "worker progress")
		})
	}()
	<-read
	decisionErr := api.Approve(run.ID, "node", "", NodeExpectation{NodeID: "node", Status: "waiting_approval"})
	close(proceed)
	writeErr := <-done
	if decisionErr != nil {
		t.Fatal(decisionErr)
	}
	approved, ok := store.RunState(run.ID)
	if !ok || approved.Status != "running" || approved.Nodes["node"].Status != "succeeded" || len(approved.Events) != 1 || approved.Events[0].Type != "gate.approved" {
		t.Fatalf("approval lost after old progress write: status=%s node=%+v events=%+v", approved.Status, approved.Nodes["node"], approved.Events)
	}
	if !errors.Is(writeErr, repo.ErrRunDecisionStale) {
		t.Fatalf("old progress write error = %v, want revision conflict", writeErr)
	}
	if err := store.TryAcquireRunLease(run.ID, "progress-worker", approved.Generation, time.Minute); err != nil {
		t.Fatal(err)
	}
	paused, err := store.RequestRunPause(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	if err := store.DB.Callback().Create().Before("gorm:create").Register("progress_deadlock", func(tx *gorm.DB) {
		if tx.Statement.Table == "runs" {
			writes++
			if writes == 1 {
				_ = tx.AddError(&mysqldriver.MySQLError{Number: 1213})
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	mutations := 0
	if err := worker.updateRunErr(run.ID, func(r *domain.Run) {
		mutations++
		r.Nodes["node"].Activity = "fresh progress"
	}); err != nil {
		t.Fatal(err)
	}
	current, _ := store.RunState(run.ID)
	if current.Revision != paused.Revision+1 || !current.PauseRequested || current.Nodes["node"].Activity != "fresh progress" || writes != 2 || mutations != 1 {
		t.Fatalf("fresh progress failed after a deadlock: revision=%d pause=%t activity=%q writes=%d mutations=%d", current.Revision, current.PauseRequested, current.Nodes["node"].Activity, writes, mutations)
	}
	var releaseErr error
	writeErr = worker.updateRunErr(run.ID, func(r *domain.Run) {
		releaseErr = store.ReleaseRunLease(run.ID, r.OwnerInstanceID, r.Generation)
		r.Nodes["node"].Activity = "progress after lease release"
	})
	if releaseErr != nil || !errors.Is(writeErr, repo.ErrRunLeaseLost) {
		t.Fatalf("run write after lease release = %v, lease release = %v", writeErr, releaseErr)
	}
	released, _ := store.RunState(run.ID)
	if released.Revision != current.Revision || released.Nodes["node"].Activity != "fresh progress" || released.OwnerInstanceID != "" {
		t.Fatalf("rejected progress changed the run: revision=%d activity=%q owner=%q", released.Revision, released.Nodes["node"].Activity, released.OwnerInstanceID)
	}
}
