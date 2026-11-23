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

func TestRunSignalsCommitAcrossAPIAndWorker(t *testing.T) {
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	worker := engine.New(store, runtime.NewManager(t.TempDir()))
	worker.ConfigureExecution("signal-worker", "worker-host", time.Minute, true)
	api := engine.New(store, runtime.NewManager(t.TempDir()))
	api.ConfigureExecution("signal-api", "api-host", time.Minute, false)
	h := New(store, api, tracker.NewService(), "", "")
	run := domain.Run{ID: "split-signals", Status: "running", Nodes: map[string]*domain.NodeRun{"step": {NodeID: "step", Status: "running", Attempt: 2}}}
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	if err := store.TryAcquireRunLease(run.ID, "signal-worker", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	prior, ok := store.RunState(run.ID)
	if !ok {
		t.Fatal("run missing")
	}
	source := domain.TelemetrySource{Layer: "executor", Adapter: "fixture-sdk"}
	log := domain.WorkerEnvelope{SpecVersion: domain.ObservabilitySpecVersion, Type: "log", Log: &domain.LogRecord{
		Body: "worker ready", Source: source, Context: domain.TelemetryContext{NodeID: "step"},
	}}
	observation := domain.WorkerEnvelope{SpecVersion: domain.ObservabilitySpecVersion, Type: "observation", Observation: &domain.TelemetryEvent{
		ID: "signal-fixture", Category: "process", Name: "process.stdout", Message: "step output", Source: source, Context: domain.TelemetryContext{NodeID: "step"},
	}}
	base := "/api/runs/" + run.ID
	requestJSON[map[string]bool](t, h, http.MethodPost, base+"/signals", log, http.StatusAccepted)
	requestJSON[map[string]bool](t, h, http.MethodPost, base+"/signals", observation, http.StatusAccepted)
	logs := requestJSON[repo.LogPage](t, h, http.MethodGet, base+"/logs?after=0", nil, http.StatusOK)
	if logs.Sequence != 2 || logs.NextAfter != 2 || len(logs.Logs) != 2 || logs.Logs[0].Body != "worker ready" || logs.Logs[1].Body != "step output" {
		t.Fatalf("202 did not store ordered logs: %+v", logs)
	}
	telemetry := requestJSON[repo.TelemetryPage](t, h, http.MethodGet, base+"/telemetry?after=0", nil, http.StatusOK)
	if telemetry.Sequence != 1 || telemetry.NextAfter != 1 || len(telemetry.Events) != 1 || telemetry.Events[0].ID != "signal-fixture" || telemetry.Events[0].Context.Attempt != 2 {
		t.Fatalf("202 did not store the observation: %+v", telemetry)
	}
	current, err := store.ReadRunStreamView(run.ID)
	if err != nil || current.Revision != prior.Revision+2 || current.OwnerInstanceID != prior.OwnerInstanceID || current.Generation != prior.Generation {
		t.Fatalf("unexpected stream view after signals: revision=%d owner=%s generation=%d error=%v", current.Revision, current.OwnerInstanceID, current.Generation, err)
	}
	requestJSON[map[string]bool](t, h, http.MethodPost, base+"/signals", observation, http.StatusAccepted)
	if err := store.DB.Exec("CREATE TRIGGER reject_signal_log BEFORE INSERT ON run_logs BEGIN SELECT RAISE(ABORT, 'signal insert rejected'); END").Error; err != nil {
		t.Fatal(err)
	}
	requestJSON[map[string]any](t, h, http.MethodPost, base+"/signals", log, http.StatusServiceUnavailable)
	observation.Observation.ID = "another-signal"
	requestJSON[map[string]any](t, h, http.MethodPost, base+"/signals", observation, http.StatusServiceUnavailable)
	current, err = store.ReadRunStreamView(run.ID)
	if err != nil || current.Revision != prior.Revision+2 || current.LogSequence != 2 || current.TelemetrySequence != 1 {
		t.Fatalf("duplicate or rejected signal advanced the stream: revision=%d logs=%d telemetry=%d error=%v", current.Revision, current.LogSequence, current.TelemetrySequence, err)
	}
}
