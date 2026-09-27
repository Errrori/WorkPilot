package aitasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"

	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

const (
	// TaskTypeScan looks for due AI tasks and enqueues their runs.
	TaskTypeScan = "ai:scan"
	// TaskTypeRun generates one report.
	TaskTypeRun = "ai:run"
	// QueueAI is the asynq queue used for AI tasks.
	QueueAI = "ai"

	scanInterval = "@every 1m"
	scanLimit    = 100
)

type runPayload struct {
	TaskID  int64  `json:"task_id"`
	Trigger string `json:"trigger"`
	Actor   string `json:"actor,omitempty"`
}

// TaskStore lists due tasks and loads one task for a run.
type TaskStore interface {
	ListDueAiTasks(ctx context.Context, limit int) ([]store.AiTask, error)
	GetAiTaskByID(ctx context.Context, taskID int64) (store.AiTask, error)
}

// Notifier broadcasts report, AI task and chat events to the group room.
type Notifier interface {
	BroadcastReport(event string, r *store.Report)
	BroadcastAiTask(event string, t *store.AiTask)
	BroadcastMessage(m *store.Message)
}

type SchedulerConfig struct {
	RedisAddr     string
	RedisPassword string
	Concurrency   int
	Store         TaskStore
	Service       *Service
	Notifier      Notifier
}

// Scheduler owns the asynq client, worker and cron scheduler for AI tasks.
type Scheduler struct {
	cfg    SchedulerConfig
	client *asynq.Client
	server *asynq.Server
	sched  *asynq.Scheduler
}

func NewScheduler(cfg SchedulerConfig) (*Scheduler, error) {
	if cfg.Store == nil || cfg.Service == nil {
		return nil, errors.New("ai task scheduler is not configured")
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
			Queues:      map[string]int{QueueAI: 1},
		}),
		sched: asynq.NewScheduler(opt, nil),
	}, nil
}

// Start registers the periodic scan, starts the worker and fires one scan
// immediately so overdue tasks catch up after a restart.
func (s *Scheduler) Start() error {
	mux := asynq.NewServeMux()
	mux.HandleFunc(TaskTypeScan, s.handleScan)
	mux.HandleFunc(TaskTypeRun, s.handleRun)

	if _, err := s.sched.Register(scanInterval, asynq.NewTask(TaskTypeScan, nil), asynq.Queue(QueueAI), asynq.MaxRetry(0)); err != nil {
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
		log.Printf("initial ai task scan: %v", err)
	}
	log.Printf("ai task scheduler started (queue %s, concurrency %d)", QueueAI, s.cfg.Concurrency)
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

// EnqueueScan queues an immediate scan of due tasks.
func (s *Scheduler) EnqueueScan() error {
	if _, err := s.client.Enqueue(asynq.NewTask(TaskTypeScan, nil), asynq.Queue(QueueAI), asynq.MaxRetry(0)); err != nil {
		return fmt.Errorf("enqueue scan: %w", err)
	}
	return nil
}

// EnqueueRun queues a manual run, used by the REST API.
func (s *Scheduler) EnqueueRun(_ context.Context, taskID int64, trigger, actor string) error {
	return s.enqueueRun(taskID, trigger, actor, "")
}

func (s *Scheduler) enqueueRun(taskID int64, trigger, actor, dedupeID string) error {
	payload, err := json.Marshal(runPayload{TaskID: taskID, Trigger: trigger, Actor: actor})
	if err != nil {
		return err
	}
	opts := []asynq.Option{asynq.Queue(QueueAI), asynq.MaxRetry(0)}
	if dedupeID != "" {
		opts = append(opts, asynq.TaskID(dedupeID))
	}
	if _, err := s.client.Enqueue(asynq.NewTask(TaskTypeRun, payload), opts...); err != nil {
		return fmt.Errorf("enqueue run for ai task %d: %w", taskID, err)
	}
	return nil
}

func (s *Scheduler) handleScan(ctx context.Context, _ *asynq.Task) error {
	tasks, err := s.cfg.Store.ListDueAiTasks(ctx, scanLimit)
	if err != nil {
		return fmt.Errorf("list due ai tasks: %w", err)
	}
	for _, task := range tasks {
		dedupe := fmt.Sprintf("ai-run:%d:%d", task.ID, task.NextRunAt.UnixNano())
		if err := s.enqueueRun(task.ID, store.ReportTriggerSchedule, "", dedupe); err != nil {
			log.Printf("%v", err)
		}
	}
	return nil
}

func (s *Scheduler) handleRun(ctx context.Context, t *asynq.Task) error {
	var payload runPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("decode run payload: %w", err)
	}
	task, err := s.cfg.Store.GetAiTaskByID(ctx, payload.TaskID)
	if errors.Is(err, pgx.ErrNoRows) {
		log.Printf("ai task %d no longer exists", payload.TaskID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("load ai task %d: %w", payload.TaskID, err)
	}
	if payload.Trigger == store.ReportTriggerSchedule && !task.Enabled {
		return nil
	}

	result, genErr := s.cfg.Service.Generate(ctx, task, payload.Trigger, payload.Actor)
	if s.cfg.Notifier != nil {
		if result.Report.ID != 0 {
			s.cfg.Notifier.BroadcastReport(ws.EventReportCreated, &result.Report)
			if result.Message != nil {
				s.cfg.Notifier.BroadcastMessage(result.Message)
			}
		}
		if updated, err := s.cfg.Store.GetAiTaskByID(ctx, task.ID); err == nil {
			s.cfg.Notifier.BroadcastAiTask(ws.EventAiTaskUpdated, &updated)
		} else {
			log.Printf("reload ai task %d: %v", task.ID, err)
		}
	}
	if genErr != nil {
		log.Printf("ai task %d run: %v", task.ID, genErr)
	}
	return nil
}
