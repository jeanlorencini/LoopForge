package repo

import (
	"fmt"
	"strings"
	"testing"

	"agentflow/internal/domain"
	"gorm.io/gorm/logger"
)

func TestRunHistoryWritesOnlyNewEntries(t *testing.T) {
	store, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	run := domain.Run{ID: "incremental-history", Status: "running"}
	add := func(sequence int64) {
		id := fmt.Sprintf("history-%d", sequence)
		run.Events = append(run.Events, domain.RunEvent{ID: id, RunID: run.ID, Type: "node.progress"})
		run.Logs = append(run.Logs, domain.LogRecord{Sequence: sequence, ID: id, Body: id})
		run.Telemetry = append(run.Telemetry, domain.TelemetryEvent{Sequence: sequence, ID: id, Message: id})
		run.LogSequence, run.TelemetrySequence = sequence, sequence
	}
	for sequence := int64(1); sequence <= 100; sequence++ {
		add(sequence)
	}
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	run, _ = store.Run(run.ID)
	add(101)
	recorder := &queryRecorder{Interface: logger.Default}
	store.DB.Logger = recorder
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"run_events", "run_logs", "run_telemetry"} {
		writes := 0
		for _, query := range recorder.queries {
			if strings.HasPrefix(query, "INSERT INTO `"+table+"`") {
				writes++
			}
		}
		if writes != 1 {
			t.Errorf("%s writes = %d, want 1", table, writes)
		}
	}
	stored, ok := store.Run(run.ID)
	if !ok || stored.Revision != run.Revision+1 || len(stored.Events) != 101 || stored.Events[100].ID != "history-101" {
		t.Fatalf("stored run has revision %d and %d events", stored.Revision, len(stored.Events))
	}
	logs, err := store.QueryRunLogs(run.ID, "", 100, 0, true, false, 10)
	if err != nil || logs.Sequence != 101 || len(logs.Logs) != 1 || logs.Logs[0].Sequence != 101 || logs.NextAfter != 101 {
		t.Fatalf("log page after 100 = %+v, %v", logs, err)
	}
	telemetry, err := store.QueryRunTelemetry(run.ID, "", "", 100, 0, true, false, 10)
	if err != nil || telemetry.Sequence != 101 || len(telemetry.Events) != 1 || telemetry.Events[0].Sequence != 101 || telemetry.NextAfter != 101 {
		t.Fatalf("telemetry page after 100 = %+v, %v", telemetry, err)
	}
	for _, stmt := range []string{
		"DELETE FROM run_events WHERE run_id = ? AND seq = 100",
		"DELETE FROM run_logs WHERE run_id = ? AND seq = 101",
		"DELETE FROM run_telemetry WHERE run_id = ? AND seq = 101",
	} {
		if err := store.DB.Exec(stmt, run.ID).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutRun(stored); err != nil {
		t.Fatal(err)
	}
	repaired, ok := store.Run(run.ID)
	if !ok || len(repaired.Events) != 101 || len(repaired.Logs) != 101 || len(repaired.Telemetry) != 101 || repaired.LogSequence != 101 || repaired.TelemetrySequence != 101 {
		t.Fatalf("replayed tail incomplete: events=%d logs=%d/%d telemetry=%d/%d", len(repaired.Events), len(repaired.Logs), repaired.LogSequence, len(repaired.Telemetry), repaired.TelemetrySequence)
	}
}
