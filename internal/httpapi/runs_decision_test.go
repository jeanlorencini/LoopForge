package httpapi

import (
	"net/http"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/tracker"
)

func TestRunDecisionsRejectAnOlderDisplayedAttempt(t *testing.T) {
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	api := engine.New(store, runtime.NewManager(t.TempDir()))
	api.ConfigureExecution("approval-api", "api-host", time.Minute, false)
	h := New(store, api, tracker.NewService(), "", "")
	run := domain.Run{ID: "decision-attempts", Status: "waiting", Nodes: map[string]*domain.NodeRun{
		"gate": {NodeID: "gate", Status: "waiting_approval", Attempt: 3, ReworkCount: 2},
	}}
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	before, _ := store.RunState(run.ID)
	base := "/api/runs/" + run.ID
	req := domain.HumanDecision{NodeID: "gate", Expected: &domain.NodeDecisionExpectation{Status: "waiting_approval", Attempt: 2, ReworkCount: 1}}
	for _, action := range []string{"approve", "reject", "retry", "decline-retry"} {
		response := requestJSON[map[string]string](t, h, http.MethodPost, base+"/"+action, req, http.StatusConflict)
		if response["code"] != "run_decision_stale" {
			t.Fatalf("%s returned unexpected conflict: %v", action, response)
		}
	}
	current, ok := store.RunState(run.ID)
	if !ok || current.Revision != before.Revision || current.Status != "waiting" || current.Nodes["gate"].Status != "waiting_approval" || current.Nodes["gate"].Attempt != 3 || current.Nodes["gate"].ReworkCount != 2 || len(current.Events) != 0 {
		t.Fatalf("stale decisions changed the current attempt: %+v", current)
	}
	req.Expected.Attempt, req.Expected.ReworkCount = 3, 2
	requestJSON[map[string]bool](t, h, http.MethodPost, base+"/approve", req, http.StatusOK)
	approved, _ := store.RunState(run.ID)
	if approved.Nodes["gate"].Status != "succeeded" || approved.Nodes["gate"].Attempt != 3 {
		t.Fatalf("current decision was not applied: %+v", approved.Nodes["gate"])
	}
}
