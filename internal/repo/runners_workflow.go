package repo

import (
	"agentflow/internal/domain"
	"time"
)

// InsertWorkflowRunnerTask keeps the original payload, claims and result when
// two workers reach the same operation of an interrupted attempt.
func (r *RunnersRepo) InsertWorkflowRunnerTask(task domain.RunnerTask) (domain.RunnerTask, bool, error) {
	now := time.Now().UTC()
	task.CreatedAt, task.UpdatedAt = now, now
	created, err := insertIgnore(r.DB, "runner_tasks", runnerTaskRowValues(task))
	if err != nil || created {
		return task, created, err
	}
	var row runnerTaskRow
	if err := r.DB.Where("id = ?", task.ID).Take(&row).Error; err != nil {
		return domain.RunnerTask{}, false, err
	}
	return runnerTaskFromRow(row), false, nil
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
