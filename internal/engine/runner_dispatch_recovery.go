package engine

import (
	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type workflowRunnerAttemptContext struct{}

const workflowRunnerTaskPrefix = "rt-wf-"

// A node may dispatch several commands and invocations. Replay consumes their
// results in the same order and checks the operation before using each one.
type workflowRunnerAttempt struct {
	runID, nodeID, prefix, attemptID string
	attempt, generation, next        int
	startedAt                        time.Time
	previous                         []domain.RunnerTask
	err                              error
}

func newWorkflowRunnerAttempt(runID string, node domain.NodeRun, generation int) *workflowRunnerAttempt {
	identity := node.RunnerAttemptID
	if identity == "" {
		identity = node.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%q/%q/%d/%s", runID, node.NodeID, node.Attempt, identity)))
	return &workflowRunnerAttempt{runID: runID, nodeID: node.NodeID, prefix: fmt.Sprintf("%s%x-", workflowRunnerTaskPrefix, sum[:20]), attemptID: node.RunnerAttemptID, attempt: node.Attempt, generation: generation, startedAt: node.StartedAt}
}

func (a *workflowRunnerAttempt) matches(node *domain.NodeRun) bool {
	return node != nil && node.Attempt == a.attempt && node.RunnerAttemptID == a.attemptID && (a.attemptID != "" || node.StartedAt.Equal(a.startedAt))
}

func (a *workflowRunnerAttempt) taskID(sequence int) string {
	return a.prefix + strconv.Itoa(sequence)
}

func (a *workflowRunnerAttempt) load(store *repo.Store) error {
	tasks, err := store.WorkflowRunnerTasks(a.runID, a.nodeID, a.prefix, a.attemptID == "")
	if err != nil {
		return a.diagnostic("could not read the durable task history")
	}
	modern := make(map[string]domain.RunnerTask)
	legacy := make(map[[3]string]bool)
	olderLegacy := false
	for _, task := range tasks {
		if strings.HasPrefix(task.ID, a.prefix) {
			modern[task.ID] = task
			continue
		}
		if strings.HasPrefix(task.ID, workflowRunnerTaskPrefix) {
			return a.diagnostic("task " + task.ID + " has a different attempt identity")
		}
		// Legacy IDs were stamped from UnixNano before the queue insert. Their
		// exact timestamp avoids losing the node-attempt boundary to SQL precision.
		stamp, _, _ := strings.Cut(strings.TrimPrefix(task.ID, "rt-"), "-")
		nanos, err := strconv.ParseInt(stamp, 10, 64)
		if err != nil || a.startedAt.IsZero() {
			return a.diagnostic("legacy task " + task.ID + " has no verifiable attempt boundary")
		}
		if time.Unix(0, nanos).Before(a.startedAt) {
			if task.CompletedAt == nil || task.CompletedAt.After(a.startedAt) {
				return a.diagnostic("legacy task " + task.ID + " overlaps this attempt")
			}
			olderLegacy = true
			continue
		}
		task.WorkflowOperation = legacyWorkflowRunnerOperation(task)
		key := [3]string{task.WorkflowOperation, task.Command, workflowRunnerInvocationID(task)}
		if task.WorkflowOperation == "" || legacy[key] {
			return a.diagnostic("legacy task " + task.ID + " has no unique operation")
		}
		legacy[key] = true
		a.previous = append(a.previous, task)
	}
	if olderLegacy && len(a.previous) == 0 && len(modern) == 0 {
		return a.diagnostic("the legacy task history predates the stored attempt start")
	}
	for sequence := len(a.previous) + 1; len(modern) > 0; sequence++ {
		id := a.taskID(sequence)
		task, ok := modern[id]
		if !ok {
			return a.diagnostic("the durable operation sequence has a gap")
		}
		a.previous = append(a.previous, task)
		delete(modern, id)
	}
	return nil
}

func legacyWorkflowRunnerOperation(task domain.RunnerTask) string {
	switch {
	case task.Command == "" && workflowRunnerInvocationID(task) != "":
		return "agent"
	case task.Provider == "terminal" && task.Command != "" && strings.HasSuffix(task.ID, "-script"):
		return "script"
	case task.Provider == "terminal" && task.Command != "" && strings.HasSuffix(task.ID, "-gate"):
		return "gate"
	default:
		return ""
	}
}

func workflowRunnerInvocationID(task domain.RunnerTask) string {
	if task.Invocation == nil {
		return ""
	}
	return task.Invocation.InvocationID
}

func sameWorkflowRunnerOperation(stored, expected domain.RunnerTask) bool {
	if inv := stored.Invocation; inv != nil && ((inv.RunID != "" && inv.RunID != expected.RunID) || (inv.NodeID != "" && inv.NodeID != expected.NodeID)) {
		return false
	}
	operation := expected.WorkflowOperation
	if !strings.HasPrefix(stored.ID, workflowRunnerTaskPrefix) {
		operation, _, _ = strings.Cut(operation, ":")
	}
	return stored.RunID == expected.RunID && stored.NodeID == expected.NodeID && stored.RunnerID == expected.RunnerID &&
		stored.Provider == expected.Provider && stored.Workspace == expected.Workspace && stored.Action == "" &&
		stored.WorkflowOperation == operation && stored.Command == expected.Command &&
		workflowRunnerInvocationID(stored) == workflowRunnerInvocationID(expected)
}

func validWorkflowRunnerStatus(task domain.RunnerTask) bool {
	switch task.Status {
	case domain.RunnerTaskQueued, domain.RunnerTaskLeased, domain.RunnerTaskRunning:
		return true
	case domain.RunnerTaskSuccess:
		return task.Attempt > 0 && task.CompletedAt != nil && !task.CompletedAt.IsZero()
	case domain.RunnerTaskFailed, domain.RunnerTaskCanceled:
		return task.CompletedAt != nil && !task.CompletedAt.IsZero()
	default:
		return false
	}
}

func nextWorkflowRunnerTask(ctx context.Context, task *domain.RunnerTask, operation string) (bool, error) {
	a, _ := ctx.Value(workflowRunnerAttemptContext{}).(*workflowRunnerAttempt)
	if a == nil {
		return false, nil
	}
	if a.err != nil {
		return false, a.err
	}
	a.next++
	task.WorkflowOperation = operation
	if a.next <= len(a.previous) {
		stored := a.previous[a.next-1]
		if !sameWorkflowRunnerOperation(stored, *task) || !validWorkflowRunnerStatus(stored) {
			return false, a.diagnostic("task " + stored.ID + " does not match the next operation")
		}
		*task = stored
		return true, nil
	}
	task.ID = a.taskID(a.next)
	return false, nil
}

func (e *Engine) enqueueWorkflowRunnerTask(ctx context.Context, task domain.RunnerTask) (domain.RunnerTask, bool, error) {
	a, _ := ctx.Value(workflowRunnerAttemptContext{}).(*workflowRunnerAttempt)
	if a == nil {
		return task, true, e.repo.PutRunnerTask(task)
	}
	if err := ctx.Err(); err != nil {
		return domain.RunnerTask{}, false, err
	}
	current, ok := e.repo.RunState(a.runID)
	if !ok || current.Generation != a.generation || !e.ownsRun(current) || !a.matches(current.Nodes[a.nodeID]) || !nodeExecutionLive(current.Nodes[a.nodeID], a.attempt) {
		return domain.RunnerTask{}, false, repo.ErrRunLeaseLost
	}
	if current.CancelRequested || current.PauseRequested {
		return domain.RunnerTask{}, false, context.Canceled
	}
	stored, inserted, err := e.repo.InsertWorkflowRunnerTask(task)
	if err == nil && !inserted && (!sameWorkflowRunnerOperation(stored, task) || !validWorkflowRunnerStatus(stored)) {
		err = a.diagnostic("task " + stored.ID + " does not match the next operation")
	}
	return stored, inserted, err
}

func (e *Engine) shouldCancelDetachedRunnerTask(ctx context.Context) bool {
	a, _ := ctx.Value(workflowRunnerAttemptContext{}).(*workflowRunnerAttempt)
	if a == nil {
		return true
	}
	current, ok := e.repo.RunState(a.runID)
	if !ok {
		return false
	}
	if current.CancelRequested || current.PauseRequested || domain.TerminalRunStatus(current.Status) {
		return true
	}
	node := current.Nodes[a.nodeID]
	return !a.matches(node) || node.Status == "pending" && !continuingInterruptedAttempt(node)
}

type runnerTaskRecoveryError string

func (e runnerTaskRecoveryError) Error() string { return string(e) }

func (a *workflowRunnerAttempt) diagnostic(detail string) error {
	a.err = runnerTaskRecoveryError(fmt.Sprintf("runner task recovery for node %q: %s; check the original runner and retry manually", a.nodeID, detail))
	return a.err
}

func isRunnerTaskRecoveryError(err error) bool {
	var recovery runnerTaskRecoveryError
	return errors.As(err, &recovery)
}
