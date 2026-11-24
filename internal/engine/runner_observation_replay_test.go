package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

func TestResumedRunnerTaskDoesNotReplayObservationMetrics(t *testing.T) {
	t.Parallel()
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	first := New(store, nil)
	first.ConfigureExecution("first-worker", "worker-host", time.Minute, true)
	now := time.Now().UTC()
	node := &domain.NodeRun{NodeID: "work", Status: "running", Attempt: 1, RunnerAttemptID: "attempt-1"}
	run := domain.Run{
		ID: "run-observation-replay", Status: "running", OwnerInstanceID: "first-worker",
		Generation: 1, LeaseUntil: now.Add(time.Minute),
		Nodes: map[string]*domain.NodeRun{node.NodeID: node}, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	task := domain.RunnerTask{
		ID: "rt-observation-replay", Provider: "codebuddy", RunnerID: "runner-1",
		Status: domain.RunnerTaskRunning, RunID: run.ID, NodeID: node.NodeID,
	}
	if err := store.PutRunnerTask(task); err != nil {
		t.Fatal(err)
	}

	toolFinish := domain.TelemetryEvent{Category: "tool", Name: "tool.call", Phase: "finish"}
	usage := func(tokens int64) domain.TelemetryEvent {
		return domain.TelemetryEvent{
			Category: "usage", Name: "usage.snapshot", SessionID: "session-1",
			Usage: &domain.TokenUsage{InputTokens: tokens, TotalTokens: tokens},
		}
	}
	appendObservations := func(start int64, observations ...domain.TelemetryEvent) {
		t.Helper()
		events := make([]domain.RunnerTaskEvent, 0, len(observations))
		for i, observation := range observations {
			raw, err := json.Marshal(observation)
			if err != nil {
				t.Fatal(err)
			}
			events = append(events, domain.RunnerTaskEvent{
				Seq: start + int64(i), Type: "observation", Data: string(raw), Ts: now,
			})
		}
		if written, err := store.AppendRunnerTaskEvents(task.ID, events); err != nil || written != len(events) {
			t.Fatalf("append runner events wrote %d of %d: %v", written, len(events), err)
		}
	}
	appendObservations(2, toolFinish, usage(10), usage(20))

	attemptContext := context.WithValue(context.Background(), workflowRunnerAttemptContext{}, newWorkflowRunnerAttempt(run.ID, *node, run.Generation))
	ctx, detach := context.WithTimeout(attemptContext, 5*time.Second)
	defer detach()
	_, err = first.awaitWorkflowRunnerTask(ctx, task.ID, func(observation domain.TelemetryEvent) {
		first.observe(run.ID, node.NodeID, observation)
		if observation.Usage != nil && observation.Usage.TotalTokens == 20 {
			detach()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first waiter returned %v, want context canceled", err)
	}
	first.flushAllObservations()
	initial, ok := store.RunState(run.ID)
	if !ok || initial.ToolCalls != 1 || initial.Usage.TotalTokens != 20 {
		t.Fatalf("before handoff calls=%d tokens=%d, want 1/20", initial.ToolCalls, initial.Usage.TotalTokens)
	}
	if err := store.ReleaseRunLease(run.ID, "first-worker", run.Generation); err != nil {
		t.Fatal(err)
	}
	if err := store.TryAcquireRunLease(run.ID, "replacement-worker", run.Generation, time.Minute); err != nil {
		t.Fatal(err)
	}

	// The runner event API can fill an earlier gap after a higher sequence commits.
	appendObservations(1, toolFinish)
	appendObservations(5, usage(25))
	task.Status = domain.RunnerTaskSuccess
	if err := store.PutRunnerTask(task); err != nil {
		t.Fatal(err)
	}
	replacement := New(store, nil)
	replacement.ConfigureExecution("replacement-worker", "worker-host", time.Minute, true)
	if _, err := replacement.awaitWorkflowRunnerTask(context.Background(), task.ID, func(observation domain.TelemetryEvent) {
		replacement.observe(run.ID, node.NodeID, observation)
	}); err != nil {
		t.Fatal(err)
	}

	resumed, ok := store.Run(run.ID)
	if !ok {
		t.Fatal("run missing after reattachment")
	}
	recoveredNode := resumed.Nodes[node.NodeID]
	if recoveredNode == nil {
		t.Fatal("node missing after reattachment")
	}
	if resumed.ToolCalls != 2 || recoveredNode.ToolCalls != 2 || resumed.Usage.TotalTokens != 25 ||
		recoveredNode.Usage.TotalTokens != 25 || recoveredNode.UsageCursor.TotalTokens != 25 {
		t.Fatalf("after handoff calls=%d/%d tokens=%d/%d cursor=%d, want 2/2, 25/25, 25",
			resumed.ToolCalls, recoveredNode.ToolCalls, resumed.Usage.TotalTokens, recoveredNode.Usage.TotalTokens, recoveredNode.UsageCursor.TotalTokens)
	}
}
