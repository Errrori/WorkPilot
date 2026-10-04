package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"

	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

const (
	// TaskTypeScan looks for due workflows and enqueues their runs.
	TaskTypeScan = "workflow:scan"
	// TaskTypeRun executes one workflow run.
	TaskTypeRun = "workflow:run"
	// QueueWorkflow is the asynq queue used for workflow runs.
	QueueWorkflow = "workflow"

	scanInterval = "@every 1m"
	scanLimit    = 100
)

type runPayload struct {
	WorkflowID int64  `json:"workflow_id"`
	Trigger    string `json:"trigger"`
	Actor      string `json:"actor,omitempty"`
}

// SchedulerStore lists due workflows and loads one for a run.
type SchedulerStore interface {
	ListDueWorkflows(ctx context.Context, limit int) ([]store.Workflow, error)
	GetWorkflowByID(ctx context.Context, workflowID int64) (store.Workflow, error)
}

// SchedulerConfig wires the asynq workers around the workflow service.
type SchedulerConfig struct {
	RedisAddr     string
	RedisPassword string
	Concurrency   int
	Store         SchedulerStore
	Service       *Service
	Notifier      Notifier
}

// Scheduler owns the asynq client, worker and cron scheduler for workflows.
type Scheduler struct {
	cfg    SchedulerConfig
	client *asynq.Client
	server *asynq.Server
	sched  *asynq.Scheduler
}

func NewScheduler(cfg SchedulerConfig) (*Scheduler, error) {
	if cfg.Store == nil || cfg.Service == nil {
		return nil, errors.New("workflow scheduler is not configured")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	opt := asynq.RedisClientOpt{Addr: cfg.RedisAddr, Password: cfg.RedisPassword}
	return &Scheduler{
		cfg:    cfg,
		client: asynq.NewClient(opt),
		server: asynq.NewServer(opt, asynq.Config{
			Concurrency: cfg.Concurrency,
			Queues:      map[string]int{QueueWorkflow: 1},
		}),
		sched: asynq.NewScheduler(opt, nil),
	}, nil
}

// Start registers the periodic scan, starts the worker and fires one scan
// immediately so overdue workflows catch up after a restart.
func (s *Scheduler) Start() error {
	mux := asynq.NewServeMux()
	mux.HandleFunc(TaskTypeScan, s.handleScan)
	mux.HandleFunc(TaskTypeRun, s.handleRun)

	if _, err := s.sched.Register(scanInterval, asynq.NewTask(TaskTypeScan, nil), asynq.Queue(QueueWorkflow), asynq.MaxRetry(0)); err != nil {
		return fmt.Errorf("register scan: %w", err)
	}
	if err := s.server.Start(mux); err != nil {
		return fmt.Errorf("start asynq server: %w", err)
	}
	if err := s.sched.Start(); err != nil {
		s.server.Shutdown()
		return fmt.Errorf("start asynq scheduler: %w", err)
	}
	if err := s.EnqueueScan(); err != nil {
		slog.Warn("initial workflow scan", "error", err)
	}
	slog.Info("workflow scheduler started", "queue", QueueWorkflow, "concurrency", s.cfg.Concurrency)
	return nil
}

// Shutdown stops the scheduler, worker and client.
func (s *Scheduler) Shutdown() {
	if s.sched != nil {
		s.sched.Shutdown()
	}
	if s.server != nil {
		s.server.Shutdown()
	}
	if s.client != nil {
		_ = s.client.Close()
	}
}

// EnqueueScan queues an immediate scan of due workflows.
func (s *Scheduler) EnqueueScan() error {
	if _, err := s.client.Enqueue(asynq.NewTask(TaskTypeScan, nil), asynq.Queue(QueueWorkflow), asynq.MaxRetry(0)); err != nil {
		return fmt.Errorf("enqueue scan: %w", err)
	}
	return nil
}

// EnqueueRun queues a manual run, used by the REST API.
func (s *Scheduler) EnqueueRun(_ context.Context, workflowID int64, trigger, actor string) error {
	return s.enqueueRun(workflowID, trigger, actor, "")
}

func (s *Scheduler) enqueueRun(workflowID int64, trigger, actor, dedupeID string) error {
	payload, err := json.Marshal(runPayload{WorkflowID: workflowID, Trigger: trigger, Actor: actor})
	if err != nil {
		return err
	}
	opts := []asynq.Option{asynq.Queue(QueueWorkflow), asynq.MaxRetry(0)}
	if dedupeID != "" {
		opts = append(opts, asynq.TaskID(dedupeID))
	}
	if _, err := s.client.Enqueue(asynq.NewTask(TaskTypeRun, payload), opts...); err != nil {
		return fmt.Errorf("enqueue run for workflow %d: %w", workflowID, err)
	}
	return nil
}

func (s *Scheduler) handleScan(ctx context.Context, _ *asynq.Task) error {
	workflows, err := s.cfg.Store.ListDueWorkflows(ctx, scanLimit)
	if err != nil {
		return fmt.Errorf("list due workflows: %w", err)
	}
	for _, wf := range workflows {
		dedupe := fmt.Sprintf("wf-run:%d:%d", wf.ID, wf.NextRunAt.UnixNano())
		if err := s.enqueueRun(wf.ID, store.WorkflowTriggerSchedule, "", dedupe); err != nil {
			slog.Warn("enqueue scheduled workflow", "workflow_id", wf.ID, "error", err)
		}
	}
	return nil
}

func (s *Scheduler) handleRun(ctx context.Context, t *asynq.Task) error {
	var payload runPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("decode run payload: %w", err)
	}
	wf, err := s.cfg.Store.GetWorkflowByID(ctx, payload.WorkflowID)
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Info("workflow no longer exists", "workflow_id", payload.WorkflowID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("load workflow %d: %w", payload.WorkflowID, err)
	}
	if payload.Trigger == store.WorkflowTriggerSchedule && !wf.Enabled {
		return nil
	}

	_, runErr := s.cfg.Service.Run(ctx, wf, payload.Trigger, payload.Actor)
	if s.cfg.Notifier != nil {
		if updated, err := s.cfg.Store.GetWorkflowByID(ctx, wf.ID); err == nil {
			s.cfg.Notifier.BroadcastWorkflow(ws.EventWorkflowUpdated, &updated)
		} else {
			slog.Warn("reload workflow", "workflow_id", wf.ID, "error", err)
		}
	}
	if runErr != nil {
		slog.Error("workflow run failed", "workflow_id", wf.ID, "error", runErr)
	}
	return nil
}
