package repo

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestWorkflowRunnerTaskInsertKeepsOriginalClaimAndCompletion(t *testing.T) {
	s := newTestRunner(t)
	now := time.Now().UTC()
	task := domain.RunnerTask{
		ID: "rt-wf-fixture-1", RunnerID: "fixture", RunID: "run", NodeID: "node", Provider: "terminal",
		Status: domain.RunnerTaskQueued, MaxAttempts: 1, Command: "true", WorkflowOperation: "script",
	}
	run := putRunnerWorkflowFixture(t, s, task)
	insert := func(task domain.RunnerTask) (domain.RunnerTask, bool, error) {
		return s.InsertWorkflowRunnerTask(context.Background(), task, run.OwnerInstanceID, run.Generation, *run.Nodes[task.NodeID])
	}
	if _, inserted, err := insert(task); err != nil || !inserted {
		t.Fatalf("inserted=%t err=%v", inserted, err)
	}
	leased, claimed, err := s.ClaimRunnerTask(task.RunnerID, []string{"terminal"}, now, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claimed=%t err=%v", claimed, err)
	}
	request := task
	request.Command = "false"
	stored, inserted, err := insert(request)
	if err != nil || inserted || stored.Command != "true" || stored.LeaseOwner != leased.LeaseOwner || stored.LeaseUntil == nil || !stored.LeaseUntil.Equal(*leased.LeaseUntil) || stored.Attempt != 1 {
		t.Fatalf("second dispatch changed the lease or request: inserted=%t err=%v task=%+v", inserted, err, stored)
	}
	report := domain.RunnerTaskCompletion{Attempt: 1, Status: domain.RunnerTaskSuccess, Result: "original result"}
	if changed, err := s.CompleteRunnerTaskAttempt(task.ID, task.RunnerID, report, now); err != nil || !changed {
		t.Fatalf("completed=%t err=%v", changed, err)
	}
	stored, inserted, err = insert(request)
	if err != nil || inserted || stored.Status != domain.RunnerTaskSuccess || stored.Result != report.Result || stored.WorkflowOperation != "script" {
		t.Fatalf("second dispatch changed completion: inserted=%t err=%v task=%+v", inserted, err, stored)
	}
	if changed, err := s.CompleteRunnerTaskAttempt(task.ID, task.RunnerID, report, now); err != nil || changed {
		t.Fatalf("completion replay changed=%t err=%v", changed, err)
	}
	wire, err := json.Marshal(stored)
	if err != nil || strings.Contains(string(wire), "workflowOperation") {
		t.Fatalf("control-plane correlation leaked into runner response: %s, %v", wire, err)
	}
}

func TestWorkflowRunnerTaskInsertRequiresLiveRunningAttempt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Store, domain.Run) error
		want   error
	}{
		{"paused", func(s *Store, r domain.Run) error { _, err := s.RequestRunPause(r.ID); return err }, context.Canceled},
		{"cancel accepted", func(s *Store, r domain.Run) error { _, err := s.RequestRunCancel(r.ID); return err }, context.Canceled},
		{"lease released", func(s *Store, r domain.Run) error { return s.ReleaseRunLease(r.ID, r.OwnerInstanceID, r.Generation) }, ErrRunLeaseLost},
		{"run finished", func(s *Store, r domain.Run) error {
			r.Status = "succeeded"
			r.Nodes["node"].Status = "succeeded"
			return s.PutRun(r)
		}, ErrRunLeaseLost},
		{"attempt replaced", func(s *Store, r domain.Run) error {
			r.Nodes["node"] = &domain.NodeRun{NodeID: "node", Status: "running", Attempt: 1, RunnerAttemptID: "another-attempt"}
			return s.PutRun(r)
		}, ErrRunLeaseLost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestRunner(t)
			task := domain.RunnerTask{ID: "rt-wf-fixture-1", RunID: "run", NodeID: "node", RunnerID: "runner", Provider: "terminal",
				Status: domain.RunnerTaskQueued, MaxAttempts: 1, Command: "true", WorkflowOperation: "script"}
			run := putRunnerWorkflowFixture(t, s, task)
			node := *run.Nodes[task.NodeID]
			if err := tc.change(s, run); err != nil {
				t.Fatal(err)
			}
			if _, created, err := s.InsertWorkflowRunnerTask(context.Background(), task, run.OwnerInstanceID, run.Generation, node); created || !errors.Is(err, tc.want) {
				t.Fatalf("created=%t, err=%v, want %v", created, err, tc.want)
			}
			if _, found := s.RunnerTask(task.ID); found {
				t.Fatal("an invalid dispatch left a task in the queue")
			}
		})
	}
}

func putRunnerWorkflowFixture(t *testing.T, s *Store, task domain.RunnerTask) domain.Run {
	t.Helper()
	if err := s.PutRun(domain.Run{ID: task.RunID, Status: "running", Nodes: map[string]*domain.NodeRun{
		task.NodeID: {NodeID: task.NodeID, Status: "running", Attempt: 1, RunnerAttemptID: "fixture-attempt"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.TryAcquireRunLease(task.RunID, "worker", 0, time.Minute); err != nil {
		t.Fatal(err)
	}
	run, ok := s.RunState(task.RunID)
	if !ok {
		t.Fatal("run not found")
	}
	return run
}
