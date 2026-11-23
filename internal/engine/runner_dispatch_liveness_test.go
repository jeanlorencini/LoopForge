package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestQueuedGateFailsForUnavailableRunner(t *testing.T) {
	for _, reason := range []string{"missing", "stale", "offline"} {
		t.Run(reason, func(t *testing.T) {
			e, store := cvEEngine(t)
			const runnerID = "bound-runner"
			if reason != "missing" {
				runner := cvERunner(t, e, runnerID, domain.RunnerStatusOnline, "terminal")
				e.SetRunnerDispatchNotify(func(string) {
					now := time.Now().UTC()
					var err error
					if reason == "stale" {
						lastSeen := now.Add(-2 * time.Minute)
						runner.LastSeenAt = &lastSeen
						err = store.PutRunner(runner)
					} else {
						err = store.MarkRunnerOffline(runnerID, now)
					}
					if err != nil {
						t.Error(err)
					}
				})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_, err := e.dispatchRunnerCommand(ctx, runnerID, "run", "gate", "workspace", "true", nil)
			if err == nil || !strings.Contains(err.Error(), runnerID) || !strings.Contains(err.Error(), "reconnect") {
				t.Fatalf("unavailable bound runner should fail with recovery guidance, got %v", err)
			}
			tasks := store.RunnerTasks()
			if len(tasks) != 1 || tasks[0].Status != domain.RunnerTaskFailed || tasks[0].Attempt != 0 ||
				tasks[0].ExitCode != nil || tasks[0].CompletedAt == nil || tasks[0].Error != err.Error() {
				t.Fatalf("expected durable failure before claim: %+v", tasks)
			}
		})
	}
}
