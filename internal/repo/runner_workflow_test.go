package repo

import (
	"encoding/json"
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
	if _, inserted, err := s.InsertWorkflowRunnerTask(task); err != nil || !inserted {
		t.Fatalf("inserted=%t err=%v", inserted, err)
	}
	leased, claimed, err := s.ClaimRunnerTask(task.RunnerID, []string{"terminal"}, now, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claimed=%t err=%v", claimed, err)
	}
	request := task
	request.Command = "false"
	stored, inserted, err := s.InsertWorkflowRunnerTask(request)
	if err != nil || inserted || stored.Command != "true" || stored.LeaseOwner != leased.LeaseOwner || stored.LeaseUntil == nil || !stored.LeaseUntil.Equal(*leased.LeaseUntil) || stored.Attempt != 1 {
		t.Fatalf("second dispatch changed the lease or request: inserted=%t err=%v task=%+v", inserted, err, stored)
	}
	report := domain.RunnerTaskCompletion{Attempt: 1, Status: domain.RunnerTaskSuccess, Result: "original result"}
	if changed, err := s.CompleteRunnerTaskAttempt(task.ID, task.RunnerID, report, now); err != nil || !changed {
		t.Fatalf("completed=%t err=%v", changed, err)
	}
	stored, inserted, err = s.InsertWorkflowRunnerTask(request)
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
