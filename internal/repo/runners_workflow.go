package repo

import (
	"context"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

// InsertWorkflowRunnerTask queues an operation under the run's lease and
// control lock. A duplicate keeps the original payload, claim and result.
func (r *RunnersRepo) InsertWorkflowRunnerTask(ctx context.Context, task domain.RunnerTask, owner string, generation int, node domain.NodeRun) (domain.RunnerTask, bool, error) {
	if task.NodeID != node.NodeID {
		return domain.RunnerTask{}, false, ErrRunLeaseLost
	}
	var stored domain.RunnerTask
	var created bool
	err := r.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := r.runs.guardWorkflowRunnerDispatch(tx, task.RunID, owner, generation, node); err != nil {
			return err
		}
		now := time.Now().UTC()
		task.CreatedAt, task.UpdatedAt = now, now
		inserted, err := insertIgnore(tx, "runner_tasks", runnerTaskRowValues(task))
		if err != nil {
			return err
		}
		created = inserted
		if inserted {
			stored = task
			return nil
		}
		var row runnerTaskRow
		if err := r.locking(tx).Where("id = ?", task.ID).Take(&row).Error; err != nil {
			return err
		}
		stored = runnerTaskFromRow(row)
		return nil
	})
	if err != nil {
		return domain.RunnerTask{}, false, err
	}
	return stored, created, nil
}

// WorkflowRunnerTasks reads an attempt's durable operations and, if its node
// predates durable identities, the legacy workflow tasks for that run and node.
func (r *RunnersRepo) WorkflowRunnerTasks(runID, nodeID, attemptPrefix string, legacy bool) ([]domain.RunnerTask, error) {
	query := r.DB.Where("id LIKE ?", attemptPrefix+"%")
	if legacy {
		query = query.Or(r.sqlPayloadJSONText("runId")+" = ? AND "+r.sqlPayloadJSONText("nodeId")+" = ? AND "+r.sqlPayloadJSONText("action")+" IS NULL", runID, nodeID)
	}
	var rows []runnerTaskRow
	if err := query.Order("created_at, id").Find(&rows).Error; err != nil {
		return nil, err
	}
	tasks := make([]domain.RunnerTask, 0, len(rows))
	for _, row := range rows {
		tasks = append(tasks, runnerTaskFromRow(row))
	}
	return tasks, nil
}
