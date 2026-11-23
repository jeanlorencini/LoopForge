package engine

import (
	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

func (e *Engine) updateRun(id string, mutate func(*domain.Run)) {
	_ = e.updateRunAt(id, false, mutate)
}

// updateRunErr is updateRun for callers that must react to a failed write,
// such as a SOP node whose artifact handoff did not reach the store.
func (e *Engine) updateRunErr(id string, mutate func(*domain.Run)) error {
	_, err := e.updateRunAtErr(id, false, mutate)
	return err
}

func (e *Engine) updateRunSettling(id string, mutate func(*domain.Run)) bool {
	return e.updateRunAt(id, true, mutate)
}

func (e *Engine) updateRunAt(id string, allowExpiredLease bool, mutate func(*domain.Run)) bool {
	updated, _ := e.updateRunAtErr(id, allowExpiredLease, mutate)
	return updated
}

// updateRunAtErr reports whether the run was stored and, when the store
// rejected the write, that error. A skipped update (missing run or a lease
// held elsewhere) is not an error.
func (e *Engine) updateRunAtErr(id string, allowExpiredLease bool, mutate func(*domain.Run)) (bool, error) {
	if mutate == nil {
		return e.updateRunProgress(id, allowExpiredLease, true, nil)
	}
	return e.updateRunProgress(id, allowExpiredLease, false, func(r *domain.Run) bool {
		mutate(r)
		return true
	})
}

// updateRunReplayable reapplies a snapshot-only mutation after a revision
// conflict. The caller must validate its decision against each new snapshot.
func (e *Engine) updateRunReplayable(id string, mutate func(*domain.Run) bool) (bool, error) {
	return e.updateRunProgress(id, false, true, mutate)
}

func (e *Engine) updateRunProgress(id string, allowExpiredLease, replay bool, mutate func(*domain.Run) bool) (bool, error) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	r, ok := e.repo.RunState(id)
	if !ok {
		_ = e.takePendingObservations(id)
		return false, nil
	}
	if isLiveStatus(r.Status) && r.OwnerInstanceID != "" && !e.ownsRun(r) && !allowExpiredLease {
		_ = e.takePendingObservations(id)
		return false, nil
	}
	pending := e.takePendingObservations(id)
	nodesAtRead := make(map[string]*domain.NodeRun)
	for i := range pending {
		item := &pending[i]
		if item.nodeID == "" || item.nodeAtRead != nil {
			continue
		}
		if nodesAtRead[item.nodeID] == nil {
			var node domain.NodeRun
			if current := r.Nodes[item.nodeID]; current != nil {
				node = *current
			}
			nodesAtRead[item.nodeID] = &node
		}
		item.nodeAtRead = nodesAtRead[item.nodeID]
	}
	owner, generation := r.OwnerInstanceID, r.Generation
	for {
		coalesce := true
		for _, item := range pending {
			var original *domain.NodeRun
			current := r.Nodes[item.nodeID]
			if item.nodeAtRead != nil && (!sameNodeAttempt(item.nodeAtRead, current) ||
				(item.nodeAtRead.Status != current.Status && current.Status != "running" && current.Status != "pending_input")) {
				original = item.nodeAtRead
				// Later progress must not coalesce into an earlier attempt's event.
				coalesce = false
			}
			e.applyObservationWithHistory(&r, item.nodeID, item.event, original, coalesce)
		}
		applied := mutate == nil || mutate(&r)
		if (mutate == nil || !applied) && len(pending) == 0 {
			return applied, nil
		}
		err := e.repo.PutRunProgress(r, allowExpiredLease)
		if err == nil {
			if current, ok := e.repo.RunStreamView(id); ok {
				e.publishRun(current)
			}
			return applied, nil
		}
		if !replay || !errors.Is(err, repo.ErrRunDecisionStale) {
			if len(pending) > 0 {
				e.obsMu.Lock()
				e.obsPending = append(pending, e.obsPending...)
				e.obsMu.Unlock()
			}
			log.Printf("put run %s: %v", id, err)
			return false, err
		}
		r, ok = e.repo.RunState(id)
		if !ok || r.OwnerInstanceID != owner || r.Generation != generation ||
			(owner != "" && !e.ownsRun(r) && !allowExpiredLease) {
			return false, nil
		}
	}
}

func sameNodeAttempt(a, b *domain.NodeRun) bool {
	return a != nil && b != nil && a.Attempt == b.Attempt && a.RunnerAttemptID == b.RunnerAttemptID &&
		a.ReworkCount == b.ReworkCount && a.StartedAt.Equal(b.StartedAt)
}

// Subscribe provides provider-neutral live run snapshots. Slow clients receive
// the newest state rather than blocking execution workers.
func (e *Engine) Subscribe(runID string) (<-chan domain.Run, func(), error) {
	current, err := e.repo.ReadRunStreamView(runID)
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan domain.Run, 1)
	ch <- current
	e.subMu.Lock()
	if e.subs[runID] == nil {
		e.subs[runID] = map[chan domain.Run]struct{}{}
	}
	e.subs[runID][ch] = struct{}{}
	e.subMu.Unlock()
	stop := make(chan struct{})
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			close(stop)
			e.subMu.Lock()
			defer e.subMu.Unlock()
			delete(e.subs[runID], ch)
			close(ch)
			if len(e.subs[runID]) == 0 {
				delete(e.subs, runID)
			}
		})
	}
	go func() {
		defer cancel()
		e.pollRun(runID, stop, current.Revision)
	}()
	return ch, cancel, nil
}

func (e *Engine) pollRun(runID string, stop <-chan struct{}, revision int) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			latestRevision, err := e.repo.ReadRunRevision(runID)
			if errors.Is(err, repo.ErrRunNotFound) {
				return
			}
			if err != nil || latestRevision == revision {
				continue
			}
			current, err := e.repo.ReadRunStreamView(runID)
			if errors.Is(err, repo.ErrRunNotFound) {
				return
			}
			if err != nil || current.Revision == revision {
				continue
			}
			revision = current.Revision
			e.publishRun(current)
		}
	}
}

func (e *Engine) StartClaimLoop(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		e.ResumeInterruptedRuns()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !e.executes() {
					return
				}
				if resumed := e.ResumeInterruptedRuns(); len(resumed) > 0 {
					log.Printf("claimed %d run(s): %v", len(resumed), resumed)
				}
			}
		}
	}()
}

func (e *Engine) publish(runID string) {
	current, ok := e.repo.RunState(runID)
	if !ok {
		return
	}
	e.publishRun(current)
}

func (e *Engine) publishRun(current domain.Run) {
	e.subMu.Lock()
	defer e.subMu.Unlock()
	for ch := range e.subs[current.ID] {
		select {
		case ch <- current:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- current:
			default:
			}
		}
	}
}
