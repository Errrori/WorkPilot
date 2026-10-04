package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

// Notifier broadcasts workflow and run events to the group room.
type Notifier interface {
	BroadcastWorkflow(event string, w *store.Workflow)
	BroadcastWorkflowRun(event string, r *store.WorkflowRun)
}

// Store is the persistence used by the workflow service.
type Store interface {
	RunStore
	StartWorkflowRun(ctx context.Context, in store.WorkflowRunInsert) (store.WorkflowRun, error)
	FinishWorkflowRun(ctx context.Context, runID int64, status, errMessage string) error
	MarkWorkflowRun(ctx context.Context, workflowID int64, status, errMessage string, ranAt, nextRun time.Time) error
}

// ServiceConfig wires the run store, engine and notifier.
type ServiceConfig struct {
	Store    Store
	Engine   *Engine
	Notifier Notifier
	// NextRun computes the following scheduled run; defaults to aitasks.NextRun.
	NextRun func(schedule, timezone string, after time.Time) (time.Time, error)
}

type Service struct {
	cfg ServiceConfig
}

func NewService(cfg ServiceConfig) *Service {
	return &Service{cfg: cfg}
}

// Run records one run, executes the definition and finalizes both the run and
// the workflow state (last run, next run), broadcasting the run lifecycle.
func (s *Service) Run(ctx context.Context, wf store.Workflow, trigger, actor string) (store.WorkflowRun, error) {
	if s.cfg.Store == nil || s.cfg.Engine == nil {
		return store.WorkflowRun{}, errors.New("workflow service is not configured")
	}
	if trigger == "" {
		trigger = store.WorkflowTriggerManual
	}
	def, err := ParseDefinition(wf.Definition)
	if err != nil {
		return store.WorkflowRun{}, err
	}

	run, err := s.cfg.Store.StartWorkflowRun(ctx, store.WorkflowRunInsert{
		WorkflowID: wf.ID,
		GroupID:    wf.GroupID,
		Trigger:    trigger,
	})
	if err != nil {
		return store.WorkflowRun{}, fmt.Errorf("start workflow run: %w", err)
	}
	s.notifyRun(ws.EventWorkflowRunStarted, &run)

	runErr := s.cfg.Engine.Run(ctx, wf, RunInfo{
		RunID:     run.ID,
		Trigger:   trigger,
		Actor:     actor,
		LastRunAt: wf.LastRunAt,
	}, def)

	status := store.WorkflowRunStatusSucceeded
	errMessage := ""
	if runErr != nil {
		status = store.WorkflowRunStatusFailed
		errMessage = truncateRunes(runErr.Error(), maxErrorRunes)
	}

	if err := s.cfg.Store.FinishWorkflowRun(ctx, run.ID, status, errMessage); err != nil {
		slog.Warn("finish workflow run", "run_id", run.ID, "error", err)
	}
	run.Status = status
	run.Error = errMessage
	finished := time.Now()
	run.FinishedAt = &finished

	nextRun := s.nextRun(wf, finished)
	if err := s.cfg.Store.MarkWorkflowRun(ctx, wf.ID, status, errMessage, finished, nextRun); err != nil {
		slog.Warn("mark workflow run", "workflow_id", wf.ID, "error", err)
	}
	s.notifyRun(ws.EventWorkflowRunFinished, &run)

	if runErr != nil {
		return run, runErr
	}
	return run, nil
}

func (s *Service) nextRun(wf store.Workflow, after time.Time) time.Time {
	if strings.TrimSpace(wf.Schedule) == "" {
		if wf.NextRunAt.IsZero() {
			return after
		}
		return wf.NextRunAt
	}
	if s.cfg.NextRun == nil {
		return after.Add(time.Hour)
	}
	next, err := s.cfg.NextRun(wf.Schedule, wf.Timezone, after)
	if err != nil {
		slog.Warn("workflow schedule invalid", "workflow_id", wf.ID, "schedule", wf.Schedule, "error", err, "retry_in", "1h")
		return after.Add(time.Hour)
	}
	return next
}

func (s *Service) notifyRun(event string, run *store.WorkflowRun) {
	if s.cfg.Notifier == nil || run == nil {
		return
	}
	s.cfg.Notifier.BroadcastWorkflowRun(event, run)
}
