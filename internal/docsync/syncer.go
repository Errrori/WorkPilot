// Package docsync pulls documentation directories from external sources
// (currently GitHub repositories) into the file workspace and RAG index.
package docsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"

	"github.com/Errrori/workpilot/internal/gitsync"
	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

const (
	// TaskTypeScan looks for document sources due for a sync.
	TaskTypeScan = "doc:scan"
	// TaskTypeSync syncs one document source.
	TaskTypeSync = "doc:sync"
	// QueueDocs is the asynq queue used for document syncs.
	QueueDocs = "docsync"

	scanInterval    = "@every 1m"
	scanLimit       = 100
	maxErrorRunes   = 500
	maxErrorDetails = 3
)

type syncPayload struct {
	SourceID int64 `json:"source_id"`
}

// GitHub is the subset of the GitHub client used by document sync.
type GitHub interface {
	Repo(ctx context.Context, owner, repo string) (gitsync.RepoInfo, error)
	Tree(ctx context.Context, owner, repo, ref, etag string) (gitsync.TreeResult, error)
	Blob(ctx context.Context, owner, repo, sha string, maxBytes int64) ([]byte, error)
}

// SyncStore persists document source progress and file versions.
type SyncStore interface {
	ListDueDocSources(ctx context.Context, limit int) ([]store.DocSource, error)
	GetDocSourceByID(ctx context.Context, sourceID int64) (store.DocSource, error)
	ListDocSourceItemRecords(ctx context.Context, sourceID int64) ([]store.DocSourceItem, error)
	UpsertDocSourceItem(ctx context.Context, sourceID int64, in store.DocSourceItemInput) (store.DocSourceItem, error)
	ArchiveDocSourceItem(ctx context.Context, sourceID int64, externalID string) error
	MarkDocSourceSync(ctx context.Context, sourceID int64, status, errMessage string, checkedAt time.Time, syncedAt *time.Time, nextSync time.Time, etag string) (store.DocSource, error)
	InsertSyncedFile(ctx context.Context, in store.SyncedFileInsert) (store.File, error)
	ActivateDocFile(ctx context.Context, fileID int64) (store.DocSource, bool, error)
}

// FileWriter stores downloaded bytes on local disk.
type FileWriter interface {
	Save(groupID, originalName string, r io.Reader, maxBytes int64) (string, int64, error)
	Remove(rel string) error
}

// ParseEnqueuer schedules files for parsing.
type ParseEnqueuer interface {
	Enqueue(f store.File)
}

// Notifier broadcasts document source events to the group room.
type Notifier interface {
	BroadcastDocSource(event string, s *store.DocSource)
}

type SchedulerConfig struct {
	RedisAddr     string
	RedisPassword string
	Concurrency   int
	Interval      time.Duration
	Client        GitHub
	Store         SyncStore
	Files         FileWriter
	Parser        ParseEnqueuer
	Notifier      Notifier
}

// Scheduler owns the asynq client, worker and periodic scan for document syncs.
type Scheduler struct {
	cfg    SchedulerConfig
	client *asynq.Client
	server *asynq.Server
	sched  *asynq.Scheduler
	locks  sync.Map
}

func NewScheduler(cfg SchedulerConfig) (*Scheduler, error) {
	if cfg.Client == nil || cfg.Store == nil || cfg.Files == nil || cfg.Parser == nil {
		return nil, errors.New("document sync scheduler is not configured")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Minute
	}
	opt := asynq.RedisClientOpt{Addr: cfg.RedisAddr, Password: cfg.RedisPassword}
	return &Scheduler{
		cfg:    cfg,
		client: asynq.NewClient(opt),
		server: asynq.NewServer(opt, asynq.Config{
			Concurrency: cfg.Concurrency,
			Queues:      map[string]int{QueueDocs: 1},
		}),
		sched: asynq.NewScheduler(opt, nil),
	}, nil
}

// Start registers the periodic scan, starts the worker and fires one scan
// immediately so overdue sources catch up after a restart.
func (s *Scheduler) Start() error {
	mux := asynq.NewServeMux()
	mux.HandleFunc(TaskTypeScan, s.handleScan)
	mux.HandleFunc(TaskTypeSync, s.handleSync)

	if _, err := s.sched.Register(scanInterval, asynq.NewTask(TaskTypeScan, nil), asynq.Queue(QueueDocs), asynq.MaxRetry(0)); err != nil {
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
		slog.Warn("initial document sync scan", "error", err)
	}
	slog.Info("document sync scheduler started", "queue", QueueDocs, "concurrency", s.cfg.Concurrency, "interval", s.cfg.Interval.String())
	return nil
}

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

func (s *Scheduler) EnqueueScan() error {
	if _, err := s.client.Enqueue(asynq.NewTask(TaskTypeScan, nil), asynq.Queue(QueueDocs), asynq.MaxRetry(0)); err != nil {
		return fmt.Errorf("enqueue scan: %w", err)
	}
	return nil
}

// EnqueueSync queues a manual sync, used by the REST API.
func (s *Scheduler) EnqueueSync(_ context.Context, sourceID int64) error {
	payload, err := json.Marshal(syncPayload{SourceID: sourceID})
	if err != nil {
		return err
	}
	if _, err := s.client.Enqueue(asynq.NewTask(TaskTypeSync, payload), asynq.Queue(QueueDocs), asynq.MaxRetry(0)); err != nil {
		return fmt.Errorf("enqueue document sync for source %d: %w", sourceID, err)
	}
	return nil
}

func (s *Scheduler) handleScan(ctx context.Context, _ *asynq.Task) error {
	sources, err := s.cfg.Store.ListDueDocSources(ctx, scanLimit)
	if err != nil {
		return fmt.Errorf("list due document sources: %w", err)
	}
	for _, source := range sources {
		payload, err := json.Marshal(syncPayload{SourceID: source.ID})
		if err != nil {
			continue
		}
		dedupe := fmt.Sprintf("doc-sync:%d:%d", source.ID, source.NextSyncAt.UnixNano())
		if _, err := s.client.Enqueue(asynq.NewTask(TaskTypeSync, payload), asynq.Queue(QueueDocs), asynq.MaxRetry(0), asynq.TaskID(dedupe)); err != nil {
			slog.Warn("enqueue document sync", "source_id", source.ID, "error", err)
		}
	}
	return nil
}

func (s *Scheduler) handleSync(ctx context.Context, t *asynq.Task) error {
	var payload syncPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("decode document sync payload: %w", err)
	}
	source, err := s.cfg.Store.GetDocSourceByID(ctx, payload.SourceID)
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Info("document source no longer exists", "source_id", payload.SourceID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("load document source %d: %w", payload.SourceID, err)
	}
	defer s.lockSource(source.ID)()

	if err := s.SyncSource(ctx, source); err != nil {
		slog.Error("document sync failed", "source_id", source.ID, "source", source.Name, "error", err)
	}
	return nil
}

// SyncSource fetches the remote tree, applies only changed files and records
// the outcome on the source. Visible files switch only after indexing.
func (s *Scheduler) SyncSource(ctx context.Context, source store.DocSource) error {
	cfg, err := NormalizeConfig(source.Config)
	if err != nil {
		return s.failSync(ctx, source, err, "")
	}

	now := time.Now()
	nextSync := now.Add(s.cfg.Interval)
	ref := cfg.Ref
	if ref == "" {
		info, err := s.cfg.Client.Repo(ctx, cfg.Owner, cfg.Name)
		if err != nil {
			return s.failSync(ctx, source, err, source.LastETag)
		}
		ref = strings.TrimSpace(info.DefaultBranch)
		if ref == "" {
			ref = "main"
		}
	}

	tree, err := s.cfg.Client.Tree(ctx, cfg.Owner, cfg.Name, ref, source.LastETag)
	if err != nil {
		return s.failSync(ctx, source, err, source.LastETag)
	}
	if tree.NotModified {
		updated, err := s.cfg.Store.MarkDocSourceSync(ctx, source.ID, store.DocSourceStatusSucceeded, "", now, nil, nextSync, source.LastETag)
		if err != nil {
			return err
		}
		s.notify(ws.EventDocSourceSynced, &updated)
		slog.Info("document sync unchanged", "source_id", source.ID, "source", source.Name)
		return nil
	}
	if tree.Truncated {
		return s.failSync(ctx, source, errors.New("GitHub tree is truncated; narrow include globs or reduce repository size"), tree.ETag)
	}

	records, err := s.cfg.Store.ListDocSourceItemRecords(ctx, source.ID)
	if err != nil {
		return s.failSync(ctx, source, fmt.Errorf("load source items: %w", err), tree.ETag)
	}
	existing := make(map[string]store.DocSourceItem, len(records))
	for _, item := range records {
		existing[item.ExternalID] = item
	}

	maxBytes := int64(cfg.MaxFileMB) << 20
	type candidate struct {
		entry    gitsync.TreeEntry
		excluded string
	}
	matched := make([]gitsync.TreeEntry, 0)
	for _, entry := range tree.Entries {
		if entry.Type == "blob" && MatchesPath(entry.Path, cfg) {
			matched = append(matched, entry)
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].Path < matched[j].Path
	})
	candidates := make([]candidate, 0, len(matched))
	included := 0
	for _, entry := range matched {
		reason := ""
		if entry.Size > maxBytes {
			reason = fmt.Sprintf("file exceeds %d MB limit", cfg.MaxFileMB)
		} else if included >= cfg.MaxFiles {
			reason = fmt.Sprintf("source exceeds %d file limit", cfg.MaxFiles)
		} else {
			included++
		}
		candidates = append(candidates, candidate{entry: entry, excluded: reason})
	}

	seen := make(map[string]bool, len(candidates))
	changed := false
	failures := make([]string, 0)
	etag := tree.ETag
	if etag == "" {
		etag = source.LastETag
	}

	for _, cand := range candidates {
		entry := cand.entry
		seen[entry.Path] = true
		prev, ok := existing[entry.Path]

		if cand.excluded != "" {
			if ok && (prev.Active || prev.FileID != nil || prev.PendingFileID != nil) {
				if err := s.cfg.Store.ArchiveDocSourceItem(ctx, source.ID, entry.Path); err != nil {
					failures = append(failures, fmt.Sprintf("%s: %v", entry.Path, err))
					continue
				}
				changed = true
			} else if !ok || prev.State != store.DocSourceItemExcluded {
				if _, err := s.cfg.Store.UpsertDocSourceItem(ctx, source.ID, store.DocSourceItemInput{
					ExternalID:  entry.Path,
					ContentHash: entry.SHA,
					State:       store.DocSourceItemExcluded,
					Active:      false,
				}); err != nil {
					failures = append(failures, fmt.Sprintf("%s: %v", entry.Path, err))
				}
			}
			continue
		}

		if ok && prev.State == store.DocSourceItemIncluded && prev.ContentHash == entry.SHA &&
			(prev.FileID != nil || prev.PendingFileID != nil) {
			if !prev.Active {
				if _, err := s.cfg.Store.UpsertDocSourceItem(ctx, source.ID, store.DocSourceItemInput{
					ExternalID:      entry.Path,
					ContentHash:     prev.ContentHash,
					RemoteUpdatedAt: prev.RemoteUpdatedAt,
					FileID:          prev.FileID,
					PendingFileID:   prev.PendingFileID,
					State:           store.DocSourceItemIncluded,
					Active:          true,
				}); err != nil {
					failures = append(failures, fmt.Sprintf("%s: %v", entry.Path, err))
				} else {
					changed = true
				}
			}
			continue
		}

		content, err := s.cfg.Client.Blob(ctx, cfg.Owner, cfg.Name, entry.SHA, maxBytes)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", entry.Path, err))
			s.markItemFailure(ctx, source.ID, entry.Path, prev)
			continue
		}
		if int64(len(content)) > maxBytes {
			failures = append(failures, fmt.Sprintf("%s: file exceeds %d MB limit", entry.Path, cfg.MaxFileMB))
			s.markItemFailure(ctx, source.ID, entry.Path, prev)
			continue
		}

		fileName := truncateRunes(path.Base(entry.Path), 200)
		rel, size, err := s.cfg.Files.Save(source.GroupID, fileName, bytes.NewReader(content), maxBytes)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: save file: %v", entry.Path, err))
			s.markItemFailure(ctx, source.ID, entry.Path, prev)
			continue
		}
		file, err := s.cfg.Store.InsertSyncedFile(ctx, store.SyncedFileInsert{
			GroupID:      source.GroupID,
			UploaderName: source.Name,
			FileName:     fileName,
			ContentType:  contentTypeFor(entry.Path),
			StoragePath:  rel,
			SizeBytes:    size,
			Origin:       store.FileOriginGitHub,
			SourceID:     source.ID,
			SourceURL:    sourceURL(cfg, ref, entry.Path),
			ContentHash:  entry.SHA,
			Active:       false,
		})
		if err != nil {
			_ = s.cfg.Files.Remove(rel)
			failures = append(failures, fmt.Sprintf("%s: store file: %v", entry.Path, err))
			s.markItemFailure(ctx, source.ID, entry.Path, prev)
			continue
		}

		var visibleID *int64
		if ok {
			visibleID = prev.FileID
		}
		pendingID := file.ID
		updatedAt := now
		if _, err := s.cfg.Store.UpsertDocSourceItem(ctx, source.ID, store.DocSourceItemInput{
			ExternalID:      entry.Path,
			ContentHash:     entry.SHA,
			RemoteUpdatedAt: &updatedAt,
			FileID:          visibleID,
			PendingFileID:   &pendingID,
			State:           store.DocSourceItemIncluded,
			Active:          true,
		}); err != nil {
			failures = append(failures, fmt.Sprintf("%s: record item: %v", entry.Path, err))
			_ = s.cfg.Files.Remove(rel)
			continue
		}
		s.cfg.Parser.Enqueue(file)
		changed = true
	}

	for externalID, item := range existing {
		if seen[externalID] || !item.Active {
			continue
		}
		if err := s.cfg.Store.ArchiveDocSourceItem(ctx, source.ID, externalID); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", externalID, err))
			continue
		}
		changed = true
	}

	status := store.DocSourceStatusSucceeded
	statusMessage := ""
	if len(failures) > 0 {
		status = store.DocSourceStatusFailed
		statusMessage = summarizeFailures(failures)
	}
	var syncedAt *time.Time
	if changed {
		syncedAt = &now
	}
	updated, err := s.cfg.Store.MarkDocSourceSync(ctx, source.ID, status, statusMessage, now, syncedAt, nextSync, etag)
	if err != nil {
		return err
	}
	s.notify(ws.EventDocSourceSynced, &updated)
	slog.Info("document sync finished",
		"source_id", source.ID,
		"source", source.Name,
		"candidates", len(candidates),
		"failures", len(failures),
		"changed", changed,
	)
	return nil
}

// OnFileIndexed promotes a synced file after its chunks are ready.
func (s *Scheduler) OnFileIndexed(ctx context.Context, fileID int64) {
	source, ok, err := s.cfg.Store.ActivateDocFile(ctx, fileID)
	if err != nil {
		slog.Warn("activate synced document", "file_id", fileID, "error", err)
		return
	}
	if !ok {
		return
	}
	s.notify(ws.EventDocSourceUpdated, &source)
	slog.Info("synced document activated", "source_id", source.ID, "file_id", fileID)
}

func (s *Scheduler) markItemFailure(ctx context.Context, sourceID int64, externalID string, prev store.DocSourceItem) {
	in := store.DocSourceItemInput{
		ExternalID: externalID,
		State:      store.DocSourceItemFailed,
		Active:     prev.Active,
		FileID:     prev.FileID,
	}
	if _, err := s.cfg.Store.UpsertDocSourceItem(ctx, sourceID, in); err != nil {
		slog.Warn("mark document item failed", "source_id", sourceID, "path", externalID, "error", err)
	}
}

func (s *Scheduler) failSync(ctx context.Context, source store.DocSource, cause error, etag string) error {
	now := time.Now()
	nextSync := now.Add(s.cfg.Interval)
	updated, err := s.cfg.Store.MarkDocSourceSync(ctx, source.ID, store.DocSourceStatusFailed, truncateRunes(cause.Error(), maxErrorRunes), now, nil, nextSync, etag)
	if err != nil {
		return err
	}
	s.notify(ws.EventDocSourceSynced, &updated)
	return cause
}

func (s *Scheduler) notify(event string, source *store.DocSource) {
	if s.cfg.Notifier != nil && source != nil {
		s.cfg.Notifier.BroadcastDocSource(event, source)
	}
}

func (s *Scheduler) lockSource(sourceID int64) func() {
	value, _ := s.locks.LoadOrStore(sourceID, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func summarizeFailures(failures []string) string {
	shown := failures
	if len(shown) > maxErrorDetails {
		shown = shown[:maxErrorDetails]
	}
	message := fmt.Sprintf("%d file(s) failed: %s", len(failures), strings.Join(shown, "; "))
	if len(failures) > len(shown) {
		message += "; ..."
	}
	return truncateRunes(message, maxErrorRunes)
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
