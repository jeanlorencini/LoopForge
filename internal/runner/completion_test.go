package runner

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestRunnerCompletionRetriesAfterLostResponse(t *testing.T) {
	server, store, _ := newDaemonTestServer(t)
	var reports, starts atomic.Int32
	bodies := make(chan []byte, 3)
	responses := make(chan int, 2)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/start") {
			starts.Add(1)
		}
		if !strings.HasSuffix(r.URL.Path, "/complete") {
			server.Config.Handler.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		n := reports.Add(1)
		if n > 3 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bodies <- body
		r.Body = io.NopCloser(bytes.NewReader(body))
		switch n {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			recorded := httptest.NewRecorder()
			server.Config.Handler.ServeHTTP(recorded, r)
			responses <- recorded.Code
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		case 3:
			recorded := httptest.NewRecorder()
			server.Config.Handler.ServeHTTP(recorded, r)
			responses <- recorded.Code
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes())
		}
	}))
	defer proxy.Close()
	root := t.TempDir()
	daemon, err := New(Config{ServerURL: proxy.URL, StatePath: filepath.Join(root, "state.json"), DefaultWorkspaceRoot: root, BootstrapToken: "boot-secret"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := daemon.reregister(ctx); err != nil {
		t.Fatal(err)
	}
	task := domain.RunnerTask{ID: "rt-lost-ack", Provider: mockProviderKind, Prompt: "synthetic result", Status: domain.RunnerTaskQueued, MaxAttempts: 1}
	if err := store.PutRunnerTask(task); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimRunnerTask(daemon.runnerID(), []string{task.Provider}, time.Now().UTC(), time.Minute)
	if err != nil || !found {
		t.Fatalf("claim: %v, %v", found, err)
	}
	daemon.executeTask(ctx, claimed)
	if reports.Load() != 3 || starts.Load() != 1 {
		t.Fatalf("completion reports=%d starts=%d, want 3 reports of 1 execution", reports.Load(), starts.Load())
	}
	for range 2 {
		if code := <-responses; code != http.StatusOK {
			t.Fatalf("completion status=%d, want 200", code)
		}
	}
	first := <-bodies
	if !bytes.Contains(first, []byte(`"attempt":1`)) || !bytes.Equal(first, <-bodies) || !bytes.Equal(first, <-bodies) {
		t.Fatal("runner changed its completion report between attempts")
	}
	final, _ := store.RunnerTask(task.ID)
	if final.Status != domain.RunnerTaskSuccess || final.Attempt != 1 || final.Result != "MOCK synthetic result" || final.CompletedAt == nil {
		t.Fatalf("unexpected terminal result: %s, attempt %d", final.Status, final.Attempt)
	}
}

func TestRunnerCompletionFencesAttemptAndOwner(t *testing.T) {
	server, store, _ := newDaemonTestServer(t)
	ctx := t.Context()
	register := func(name string) (*Client, string) {
		t.Helper()
		client := NewClient(server.URL)
		reg, err := client.Register(ctx, "boot-secret", RegisterRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		client.SetSession(reg.SessionToken)
		return client, reg.RunnerID
	}
	client, owner := register("owner")
	other, _ := register("other")
	task := domain.RunnerTask{ID: "rt-fenced", Provider: mockProviderKind, Status: domain.RunnerTaskQueued, MaxAttempts: 2}
	if err := store.PutRunnerTask(task); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claim := func(at time.Time) int {
		t.Helper()
		claimed, found, err := store.ClaimRunnerTask(owner, []string{task.Provider}, at, time.Minute)
		if err != nil || !found {
			t.Fatalf("claim: %v, %v", found, err)
		}
		return claimed.Attempt
	}
	old := claim(now.Add(-2 * time.Minute))
	if recovered, err := store.ExpireRunnerTasks(now); err != nil || len(recovered) != 1 {
		t.Fatalf("expire: %d, %v", len(recovered), err)
	}
	current := claim(now)
	post := func(c *Client, attempt int, result string) error {
		return c.do(ctx, http.MethodPost, "/runners/tasks/"+task.ID+"/complete", struct {
			Attempt int    `json:"attempt,omitempty"`
			Result  string `json:"result"`
		}{attempt, result}, nil)
	}
	if err := post(client, old, "stale"); err != ErrTaskLeaseLost {
		t.Fatalf("old attempt result: got %v, want 409", err)
	}
	if err := post(other, current, "wrong owner"); err != ErrTaskLeaseLost {
		t.Fatalf("wrong owner while leased: got %v, want 409", err)
	}
	if err := post(client, current, "accepted"); err != nil {
		t.Fatal(err)
	}
	first, _ := store.RunnerTask(task.ID)
	if err := post(client, current, "accepted"); err != nil {
		t.Fatalf("identical completion rejected: %v", err)
	}
	if err := post(other, current, "accepted"); err != ErrTaskLeaseLost {
		t.Fatalf("wrong owner after completion: got %v, want 409", err)
	}
	if err := post(client, current, "changed"); err != ErrTaskLeaseLost {
		t.Fatalf("changed result after completion: got %v, want 409", err)
	}
	final, _ := store.RunnerTask(task.ID)
	if final.Result != "accepted" || final.LeaseOwner != "" || final.CompletedAt == nil || first.CompletedAt == nil || !first.CompletedAt.Equal(*final.CompletedAt) {
		t.Fatal("duplicate report changed the terminal task")
	}

	task.ID = "rt-legacy-retry"
	if err := store.PutRunnerTask(task); err != nil {
		t.Fatal(err)
	}
	claim(now.Add(-2 * time.Minute))
	if recovered, err := store.ExpireRunnerTasks(now); err != nil || len(recovered) != 1 {
		t.Fatalf("expire legacy task: %d, %v", len(recovered), err)
	}
	if attempt := claim(now); attempt != 2 {
		t.Fatalf("legacy task attempt=%d, want 2", attempt)
	}
	if err := post(client, 0, "legacy result"); err != nil {
		t.Fatalf("legacy runner could not finish a retry: %v", err)
	}
}
