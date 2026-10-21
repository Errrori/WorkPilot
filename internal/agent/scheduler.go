package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hibiken/asynq"

	"github.com/Errrori/workpilot/internal/store"
)

const (
	// TaskTypeRun executes one agent run for a trigger message.
	TaskTypeRun = "agent:run"
	// QueueAgent is the asynq queue used for agent runs.
	QueueAgent = "agent"
)

type runPayload struct {
	MessageID int64 `json:"message_id"`
}

type SchedulerConfig struct {
	RedisAddr     string
	RedisPassword string
	Concurrency   int
	Service       *Service
}

// Scheduler owns the asynq client and worker for agent runs.
type Scheduler struct {
	cfg    SchedulerConfig
	client *asynq.Client
	server *asynq.Server
}

func NewScheduler(cfg SchedulerConfig) (*Scheduler, error) {
	if cfg.Service == nil {
		return nil, errors.New("agent scheduler is not configured")
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
			Queues:      map[string]int{QueueAgent: 1},
		}),
	}, nil
}

// Start registers the run handler and starts the worker.
func (s *Scheduler) Start() error {
	mux := asynq.NewServeMux()
	mux.HandleFunc(TaskTypeRun, s.handleRun)
	if err := s.server.Start(mux); err != nil {
		return fmt.Errorf("start asynq server: %w", err)
	}
	slog.Info("agent scheduler started", "queue", QueueAgent, "concurrency", s.cfg.Concurrency)
	return nil
}

// Shutdown stops the worker and client.
func (s *Scheduler) Shutdown() {
	if s.server != nil {
		s.server.Shutdown()
	}
	if s.client != nil {
		_ = s.client.Close()
	}
}

// Trigger implements the WS hook: only real mentions are enqueued.
func (s *Scheduler) Trigger(_ context.Context, m store.Message) error {
	if m.SenderName == store.SenderAI || !IsMention(m.Content) {
		return nil
	}
	return s.EnqueueRun(m.ID)
}

// EnqueueRun queues one agent run, deduplicated by trigger message id.
func (s *Scheduler) EnqueueRun(messageID int64) error {
	payload, err := json.Marshal(runPayload{MessageID: messageID})
	if err != nil {
		return err
	}
	_, err = s.client.Enqueue(
		asynq.NewTask(TaskTypeRun, payload),
		asynq.Queue(QueueAgent),
		asynq.MaxRetry(0),
		asynq.TaskID(fmt.Sprintf("agent-run:%d", messageID)),
	)
	if err != nil {
		return fmt.Errorf("enqueue agent run for message %d: %w", messageID, err)
	}
	return nil
}

func (s *Scheduler) handleRun(ctx context.Context, t *asynq.Task) error {
	var payload runPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("decode run payload: %w", err)
	}
	if err := s.cfg.Service.RunMessage(ctx, payload.MessageID); err != nil {
		// The run is already recorded; do not retry.
		slog.Warn("agent run failed", "message_id", payload.MessageID, "error", err)
	}
	return nil
}
