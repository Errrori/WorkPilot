package parser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

const (
	DefaultWorkers = 2
	queueSize      = 128
	maxReasonRunes = 500
)

// FileSource reads stored files back from disk.
type FileSource interface {
	Open(rel string) (io.ReadCloser, error)
}

// ResultStore persists parse progress.
type ResultStore interface {
	MarkFileParsing(ctx context.Context, fileID int64) (store.File, error)
	MarkFileParsed(ctx context.Context, fileID int64, content string) (store.File, error)
	MarkFileFailed(ctx context.Context, fileID int64, reason string) (store.File, error)
	MarkFileUnsupported(ctx context.Context, fileID int64, reason string) (store.File, error)
}

// Notifier broadcasts parse results to group members.
type Notifier interface {
	BroadcastFile(event string, f *store.File)
}

// Worker parses queued files with a bounded pool of goroutines.
type Worker struct {
	client   *Client
	store    ResultStore
	files    FileSource
	notifier Notifier
	ctx      context.Context
	queue    chan store.File
	wg       sync.WaitGroup
	onParsed func(store.File)
}

func NewWorker(ctx context.Context, client *Client, st ResultStore, files FileSource, notifier Notifier, workers int) *Worker {
	if workers < 1 {
		workers = DefaultWorkers
	}
	w := &Worker{
		client:   client,
		store:    st,
		files:    files,
		notifier: notifier,
		ctx:      ctx,
		queue:    make(chan store.File, queueSize),
	}
	w.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go w.loop()
	}
	return w
}

// SetOnParsed registers a callback invoked after a file was parsed successfully.
func (w *Worker) SetOnParsed(fn func(store.File)) {
	w.onParsed = fn
}

// Enqueue schedules a file for parsing without blocking the caller.
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
	current, err := w.store.MarkFileParsing(w.ctx, f.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		log.Printf("parse file %d: mark parsing: %v", f.ID, err)
		return
	}

	content, err := w.extract(current)
	if err != nil {
		w.finishWithError(current, err)
		return
	}

	updated, err := w.store.MarkFileParsed(w.ctx, current.ID, content)
	if err != nil {
		log.Printf("parse file %d: mark parsed: %v", current.ID, err)
		return
	}
	w.notifier.BroadcastFile(ws.EventFileParsed, &updated)
	if w.onParsed != nil {
		w.onParsed(updated)
	}
}

func (w *Worker) extract(f store.File) (string, error) {
	rc, err := w.files.Open(f.StoragePath)
	if err != nil {
		return "", fmt.Errorf("open stored file: %w", err)
	}
	defer rc.Close()
	return w.client.Parse(w.ctx, f.FileName, rc)
}

func (w *Worker) finishWithError(f store.File, parseErr error) {
	reason := truncate(parseErr.Error(), maxReasonRunes)

	var updated store.File
	var err error
	if errors.Is(parseErr, ErrUnsupported) {
		updated, err = w.store.MarkFileUnsupported(w.ctx, f.ID, reason)
	} else {
		updated, err = w.store.MarkFileFailed(w.ctx, f.ID, reason)
	}
	if err != nil {
		log.Printf("parse file %d: mark failed: %v", f.ID, err)
		return
	}
	w.notifier.BroadcastFile(ws.EventFileParseFailed, &updated)
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
