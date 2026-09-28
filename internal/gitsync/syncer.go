package gitsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"

	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

const (
	// TaskTypeScan looks for repositories due for a sync and enqueues them.
	TaskTypeScan = "git:scan"
	// TaskTypeSync syncs one repository.
	TaskTypeSync = "git:sync"
	// QueueGit is the asynq queue used for repository syncs.
	QueueGit = "git"

	scanInterval    = "@every 1m"
	scanLimit       = 100
	maxItemsPerSync = 200
	maxErrorRunes   = 500
)

type syncPayload struct {
	RepoID int64 `json:"repo_id"`
}

// SyncStore lists due repositories and persists sync results.
type SyncStore interface {
	ListDueRepos(ctx context.Context, limit int) ([]store.Repo, error)
	GetRepoByID(ctx context.Context, repoID int64) (store.Repo, error)
	UpsertRepoItems(ctx context.Context, repoID int64, items []store.RepoItemInput) (int, error)
	MarkRepoSync(ctx context.Context, repoID int64, status, errMessage string, syncedAt, nextSync time.Time) (store.Repo, error)
}

// Notifier broadcasts repository events to the group room.
type Notifier interface {
	BroadcastRepo(event string, r *store.Repo)
}

type SchedulerConfig struct {
	RedisAddr     string
	RedisPassword string
	Concurrency   int
	Interval      time.Duration
	LookbackDays  int
	Client        *Client
	Store         SyncStore
	Notifier      Notifier
}

// Scheduler owns the asynq client, worker and periodic scan for repository syncs.
type Scheduler struct {
	cfg    SchedulerConfig
	client *asynq.Client
	server *asynq.Server
	sched  *asynq.Scheduler
}

func NewScheduler(cfg SchedulerConfig) (*Scheduler, error) {
	if cfg.Client == nil || cfg.Store == nil {
		return nil, errors.New("git sync scheduler is not configured")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Minute
	}
	if cfg.LookbackDays <= 0 {
		cfg.LookbackDays = 7
	}
	opt := asynq.RedisClientOpt{Addr: cfg.RedisAddr, Password: cfg.RedisPassword}
	return &Scheduler{
		cfg:    cfg,
		client: asynq.NewClient(opt),
		server: asynq.NewServer(opt, asynq.Config{
			Concurrency: cfg.Concurrency,
			Queues:      map[string]int{QueueGit: 1},
		}),
		sched: asynq.NewScheduler(opt, nil),
	}, nil
}

// Start registers the periodic scan, starts the worker and fires one scan
// immediately so overdue repositories catch up after a restart.
func (s *Scheduler) Start() error {
	mux := asynq.NewServeMux()
	mux.HandleFunc(TaskTypeScan, s.handleScan)
	mux.HandleFunc(TaskTypeSync, s.handleSync)

	if _, err := s.sched.Register(scanInterval, asynq.NewTask(TaskTypeScan, nil), asynq.Queue(QueueGit), asynq.MaxRetry(0)); err != nil {
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
		slog.Warn("initial git sync scan", "error", err)
	}
	slog.Info("git sync scheduler started", "queue", QueueGit, "concurrency", s.cfg.Concurrency, "interval", s.cfg.Interval.String())
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

// EnqueueScan queues an immediate scan of due repositories.
func (s *Scheduler) EnqueueScan() error {
	if _, err := s.client.Enqueue(asynq.NewTask(TaskTypeScan, nil), asynq.Queue(QueueGit), asynq.MaxRetry(0)); err != nil {
		return fmt.Errorf("enqueue scan: %w", err)
	}
	return nil
}

// EnqueueSync queues a manual sync, used by the REST API.
func (s *Scheduler) EnqueueSync(_ context.Context, repoID int64) error {
	return s.enqueueSync(repoID, "")
}

func (s *Scheduler) enqueueSync(repoID int64, dedupeID string) error {
	payload, err := json.Marshal(syncPayload{RepoID: repoID})
	if err != nil {
		return err
	}
	opts := []asynq.Option{asynq.Queue(QueueGit), asynq.MaxRetry(0)}
	if dedupeID != "" {
		opts = append(opts, asynq.TaskID(dedupeID))
	}
	if _, err := s.client.Enqueue(asynq.NewTask(TaskTypeSync, payload), opts...); err != nil {
		return fmt.Errorf("enqueue sync for repo %d: %w", repoID, err)
	}
	return nil
}

func (s *Scheduler) handleScan(ctx context.Context, _ *asynq.Task) error {
	repos, err := s.cfg.Store.ListDueRepos(ctx, scanLimit)
	if err != nil {
		return fmt.Errorf("list due repos: %w", err)
	}
	for _, repo := range repos {
		dedupe := fmt.Sprintf("git-sync:%d:%d", repo.ID, repo.NextSyncAt.UnixNano())
		if err := s.enqueueSync(repo.ID, dedupe); err != nil {
			slog.Warn("enqueue git sync", "repo_id", repo.ID, "error", err)
		}
	}
	return nil
}

func (s *Scheduler) handleSync(ctx context.Context, t *asynq.Task) error {
	var payload syncPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("decode sync payload: %w", err)
	}
	repo, err := s.cfg.Store.GetRepoByID(ctx, payload.RepoID)
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Info("repo no longer exists", "repo_id", payload.RepoID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("load repo %d: %w", payload.RepoID, err)
	}
	if err := s.SyncRepo(ctx, repo); err != nil {
		slog.Error("git sync failed", "repo_id", repo.ID, "repo", repo.Owner+"/"+repo.Name, "error", err)
	}
	return nil
}

// SyncRepo fetches remote activity and records the outcome on the repository.
func (s *Scheduler) SyncRepo(ctx context.Context, repo store.Repo) error {
	now := time.Now()
	nextSync := now.Add(s.cfg.Interval)
	since := now.AddDate(0, 0, -s.cfg.LookbackDays)

	items, err := s.fetch(ctx, repo, since)
	if err != nil {
		_, markErr := s.cfg.Store.MarkRepoSync(ctx, repo.ID, store.RepoLastStatusFailed, truncateRunes(err.Error(), maxErrorRunes), now, nextSync)
		if markErr != nil {
			slog.Warn("mark repo sync failure", "repo_id", repo.ID, "error", markErr)
		}
		s.notify(ctx, ws.EventRepoSynced, repo.ID)
		return err
	}

	written, err := s.cfg.Store.UpsertRepoItems(ctx, repo.ID, items)
	if err != nil {
		_, markErr := s.cfg.Store.MarkRepoSync(ctx, repo.ID, store.RepoLastStatusFailed, truncateRunes(err.Error(), maxErrorRunes), now, nextSync)
		if markErr != nil {
			slog.Warn("mark repo sync failure", "repo_id", repo.ID, "error", markErr)
		}
		s.notify(ctx, ws.EventRepoSynced, repo.ID)
		return err
	}

	updated, err := s.cfg.Store.MarkRepoSync(ctx, repo.ID, store.RepoLastStatusSucceeded, "", now, nextSync)
	if err != nil {
		return err
	}
	s.notifyRepo(ws.EventRepoSynced, &updated)
	slog.Info("git sync finished", "repo_id", repo.ID, "repo", repo.Owner+"/"+repo.Name, "items", written)
	return nil
}

// fetch pulls pull requests, issues and commits in the lookback window.
func (s *Scheduler) fetch(ctx context.Context, repo store.Repo, since time.Time) ([]store.RepoItemInput, error) {
	pulls, err := s.cfg.Client.PullRequests(ctx, repo.Owner, repo.Name, since, maxItemsPerSync)
	if err != nil {
		return nil, err
	}
	issues, err := s.cfg.Client.Issues(ctx, repo.Owner, repo.Name, since, maxItemsPerSync)
	if err != nil {
		return nil, err
	}
	commits, err := s.cfg.Client.Commits(ctx, repo.Owner, repo.Name, since, maxItemsPerSync)
	if err != nil {
		return nil, err
	}
	items := make([]store.RepoItemInput, 0, len(pulls)+len(issues)+len(commits))
	for _, it := range pulls {
		items = append(items, toInput(it))
	}
	for _, it := range issues {
		items = append(items, toInput(it))
	}
	for _, it := range commits {
		items = append(items, toInput(it))
	}
	return items, nil
}

func toInput(it Item) store.RepoItemInput {
	return store.RepoItemInput{
		Kind:            it.Kind,
		ExternalID:      it.ExternalID,
		Number:          it.Number,
		Title:           it.Title,
		State:           it.State,
		Author:          it.Author,
		URL:             it.URL,
		RemoteUpdatedAt: it.UpdatedAt,
	}
}

// notify reloads the repository after an outcome it did not return.
func (s *Scheduler) notify(ctx context.Context, event string, repoID int64) {
	if s.cfg.Notifier == nil {
		return
	}
	repo, err := s.cfg.Store.GetRepoByID(ctx, repoID)
	if err != nil {
		slog.Warn("reload repo for broadcast", "repo_id", repoID, "error", err)
		return
	}
	s.notifyRepo(event, &repo)
}

func (s *Scheduler) notifyRepo(event string, repo *store.Repo) {
	if s.cfg.Notifier != nil && repo != nil {
		s.cfg.Notifier.BroadcastRepo(event, repo)
	}
}
