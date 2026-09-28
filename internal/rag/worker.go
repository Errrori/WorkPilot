package rag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/components/indexer"
	"github.com/cloudwego/eino/schema"
	"github.com/jackc/pgx/v5"

	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

const (
	DefaultWorkers        = 1
	DefaultEmbedBatchSize = 16
	queueSize             = 128
	maxReasonRunes        = 500
)

// IndexStore persists index progress.
type IndexStore interface {
	MarkFileIndexing(ctx context.Context, fileID int64) (store.File, error)
	MarkFileIndexed(ctx context.Context, fileID int64, chunkCount int) (store.File, error)
	MarkFileIndexFailed(ctx context.Context, fileID int64, reason string) (store.File, error)
	GetFileContent(ctx context.Context, groupID string, fileID int64) (store.FileContent, error)
}

// Notifier broadcasts index results to group members.
type Notifier interface {
	BroadcastFile(event string, f *store.File)
}

type WorkerConfig struct {
	Chunker   *Chunker
	Embedder  embedding.Embedder
	Dim       int
	BatchSize int
	Indexer   indexer.Indexer
	Store     IndexStore
	Notifier  Notifier
	Workers   int
}

// Worker indexes queued files with a bounded pool of goroutines.
type Worker struct {
	cfg   WorkerConfig
	ctx   context.Context
	queue chan store.File
	wg    sync.WaitGroup
}

func NewWorker(ctx context.Context, cfg WorkerConfig) *Worker {
	if cfg.Workers < 1 {
		cfg.Workers = DefaultWorkers
	}
	if cfg.BatchSize < 1 {
		cfg.BatchSize = DefaultEmbedBatchSize
	}
	if cfg.Dim < 1 {
		cfg.Dim = 1024
	}
	if cfg.Chunker == nil {
		cfg.Chunker = NewChunker(DefaultChunkSize, DefaultChunkOverlap)
	}
	w := &Worker{
		cfg:   cfg,
		ctx:   ctx,
		queue: make(chan store.File, queueSize),
	}
	w.wg.Add(cfg.Workers)
	for i := 0; i < cfg.Workers; i++ {
		go w.loop()
	}
	return w
}

// Enqueue schedules a file for indexing without blocking the caller.
func (w *Worker) Enqueue(f store.File) {
	if f.ID == 0 {
		return
	}
	select {
	case w.queue <- f:
	default:
		go func() {
			select {
			case w.queue <- f:
			case <-w.ctx.Done():
			}
		}()
	}
}

func (w *Worker) loop() {
	defer w.wg.Done()
	for {
		select {
		case <-w.ctx.Done():
			return
		case f := <-w.queue:
			w.process(f)
		}
	}
}

func (w *Worker) process(f store.File) {
	current, err := w.cfg.Store.MarkFileIndexing(w.ctx, f.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		slog.Error("index file: mark indexing", "file_id", f.ID, "error", err)
		return
	}

	count, err := w.index(current)
	if err != nil {
		w.finishWithError(current, err)
		return
	}

	updated, err := w.cfg.Store.MarkFileIndexed(w.ctx, current.ID, count)
	if err != nil {
		slog.Error("index file: mark indexed", "file_id", current.ID, "error", err)
		return
	}
	w.cfg.Notifier.BroadcastFile(ws.EventFileIndexed, &updated)
}

func (w *Worker) index(f store.File) (int, error) {
	content, err := w.cfg.Store.GetFileContent(w.ctx, f.GroupID, f.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("parsed content not found")
	}
	if err != nil {
		return 0, fmt.Errorf("load parsed content: %w", err)
	}

	chunks := w.cfg.Chunker.Split(content.Content)
	if len(chunks) == 0 {
		return 0, nil
	}

	docs := make([]*schema.Document, len(chunks))
	for i, chunk := range chunks {
		docs[i] = &schema.Document{
			ID:      ChunkID(f.ID, i),
			Content: chunk,
			MetaData: map[string]any{
				MetaFileID:     f.ID,
				MetaGroupID:    f.GroupID,
				MetaChunkIndex: i,
			},
		}
	}

	for start := 0; start < len(docs); start += w.cfg.BatchSize {
		end := min(start+w.cfg.BatchSize, len(docs))
		texts := make([]string, 0, end-start)
		for _, doc := range docs[start:end] {
			texts = append(texts, doc.Content)
		}
		vectors, err := w.cfg.Embedder.EmbedStrings(w.ctx, texts)
		if err != nil {
			return 0, fmt.Errorf("embed chunks: %w", err)
		}
		if len(vectors) != len(texts) {
			return 0, fmt.Errorf("embedder returned %d vectors for %d chunks", len(vectors), len(texts))
		}
		for i, vector := range vectors {
			if len(vector) != w.cfg.Dim {
				return 0, fmt.Errorf("embedding dimension %d, want %d", len(vector), w.cfg.Dim)
			}
			docs[start+i] = docs[start+i].WithDenseVector(vector)
		}
	}

	if _, err := w.cfg.Indexer.Store(w.ctx, docs); err != nil {
		return 0, fmt.Errorf("store chunks: %w", err)
	}
	return len(chunks), nil
}

func (w *Worker) finishWithError(f store.File, indexErr error) {
	reason := truncate(indexErr.Error(), maxReasonRunes)
	updated, err := w.cfg.Store.MarkFileIndexFailed(w.ctx, f.ID, reason)
	if err != nil {
		slog.Error("index file: mark failed", "file_id", f.ID, "error", err)
		return
	}
	w.cfg.Notifier.BroadcastFile(ws.EventFileIndexFailed, &updated)
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
