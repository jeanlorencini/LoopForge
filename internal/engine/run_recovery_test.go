package engine

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
	"gorm.io/gorm"
)

func TestRunRecoveryReadsOnlyCandidateState(t *testing.T) {
	e, s := newRecoveryEngine(t)
	// The approval-only fixtures have no workspace to destroy.
	e.sandboxes = runtime.NewManager("")
	wf := domain.WorkflowDefinition{ID: "sop", Nodes: []domain.WorkflowNode{{ID: "gate", Type: "approval"}}}
	runs := []domain.Run{
		{ID: "healthy", Status: "running", OwnerInstanceID: "worker", LeaseUntil: time.Now().Add(time.Minute), Generation: 2},
		{ID: "cancel", Status: "waiting", CancelRequested: true},
		{ID: "human", Status: "waiting"},
		{ID: "workspace", Kind: domain.RunKindWorkspace, Status: "running"},
		{ID: "group", Status: "running", WorkflowSnapshot: domain.WorkflowDefinition{ID: "roles", CollaborationMode: "group"}},
		{ID: "align-workflow", Status: "waiting", WorkflowID: BaselineAlignWorkflowID},
		{ID: "align-snapshot", Status: "waiting", WorkflowSnapshot: domain.WorkflowDefinition{ID: BaselineAlignWorkflowID, Nodes: wf.Nodes}},
		{ID: "align-legacy", Status: "waiting", Repository: domain.RepositoryContext{AlignOnly: true}},
		{ID: "align-repositories", Status: "waiting", Repositories: []domain.RepositoryContext{{RepositoryID: "first"}, {RepositoryID: "second", AlignOnly: true}}},
	}
	for i := 0; i < 25; i++ {
		runs = append(runs, domain.Run{ID: fmt.Sprintf("terminal-%d", i), Status: "succeeded"})
	}
	for _, run := range runs {
		if run.WorkflowSnapshot.ID == "" {
			run.WorkflowSnapshot = wf
		}
		status := map[string]string{"running": "running", "waiting": "waiting_approval", "succeeded": "succeeded"}[run.Status]
		run.Nodes = map[string]*domain.NodeRun{"gate": {NodeID: "gate", Status: status}}
		run.Artifacts = []domain.Artifact{{Name: "output", Content: "sample"}}
		if err := s.PutRun(run); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	reads := map[string]map[string]int{}
	if err := s.DB.Callback().Query().After("gorm:query").Register("measure_recovery_reads", func(tx *gorm.DB) {
		table := tx.Statement.Table
		if (table == "run_nodes" || table == "run_artifacts") && len(tx.Statement.Vars) == 1 {
			id, _ := tx.Statement.Vars[0].(string)
			mu.Lock()
			defer mu.Unlock()
			if reads[id] == nil {
				reads[id] = map[string]int{}
			}
			reads[id][table]++
		}
	}); err != nil {
		t.Fatal(err)
	}
	e.ResumeInterruptedRuns()
	mu.Lock()
	for _, run := range runs {
		if run.ID == "human" || run.IsWorkspace() || run.HistoricalGroup() || run.Status == "succeeded" {
			if len(reads[run.ID]) != 0 {
				t.Errorf("recovery loaded %s children: %v", run.ID, reads[run.ID])
			}
		}
	}
	if counts := reads["healthy"]; counts["run_nodes"] != 1 || counts["run_artifacts"] != 1 {
		t.Errorf("healthy running candidate should be loaded once, reads: %v", counts)
	}
	mu.Unlock()
	if run, ok := s.RunState("cancel"); !ok || run.Status != "cancelled" {
		t.Fatalf("waiting cancellation was not settled: %+v", run)
	}
	for _, id := range []string{"align-workflow", "align-snapshot", "align-legacy", "align-repositories"} {
		run := waitRun(t, s, id, 10*time.Second, func(run domain.Run) bool {
			return run.Status == "succeeded" && !e.isRunSchedulerActive(id)
		})
		if run.Nodes["gate"] == nil || run.Nodes["gate"].Status != "succeeded" {
			t.Errorf("legacy align approval was not resumed for %s: %+v", id, run)
		}
	}
}
