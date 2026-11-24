package engine

import (
	"errors"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

func TestRunSubscriptionRecoversFromReadErrorsAndClosesWhenDeleted(t *testing.T) {
	e, s := newRecoveryEngine(t)
	run := domain.Run{ID: "poll-transient", Status: "running", Revision: 1}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	var failRevision, failSnapshot sync.Once
	revisionFailed := make(chan struct{})
	snapshotFailed := make(chan struct{})
	if err := s.DB.Callback().Query().Before("gorm:query").Register("fail_run_poll_reads", func(tx *gorm.DB) {
		switch table := tx.Statement.Table; {
		case table == "runs" && len(tx.Statement.Selects) == 1 && tx.Statement.Selects[0] == "revision":
			failRevision.Do(func() {
				tx.AddError(errors.New("transient revision read"))
				close(revisionFailed)
			})
		case table == "run_logs":
			select {
			case <-revisionFailed:
				failSnapshot.Do(func() {
					tx.AddError(errors.New("transient snapshot read"))
					close(snapshotFailed)
				})
			default:
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	updates, cancel, err := e.Subscribe(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	initial := <-updates
	select {
	case <-revisionFailed:
	case <-time.After(3 * time.Second):
		t.Fatal("revision read error was never injected")
	}
	if err := s.DB.Exec("UPDATE runs SET revision=revision+1, status='succeeded' WHERE id=?", run.ID).Error; err != nil {
		t.Fatal(err)
	}
	select {
	case next, open := <-updates:
		if !open || next.Revision != initial.Revision+1 || next.Status != "succeeded" {
			t.Fatalf("expected terminal revision after transient reads: %+v", next)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("subscription did not observe the terminal revision after a failed poll")
	}
	select {
	case <-snapshotFailed:
	default:
		t.Fatal("snapshot read error was never injected")
	}
	if err := s.DeleteRun(run.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case next, open := <-updates:
		if open {
			t.Fatalf("deleted run still has updates: %+v", next)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("subscription remained open after deletion")
	}
}
