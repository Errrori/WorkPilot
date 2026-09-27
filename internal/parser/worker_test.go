package parser

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

type fakeStore struct {
	mu          sync.Mutex
	parsingErr  error
	parsed      map[int64]string
	failed      map[int64]string
	unsupported map[int64]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		parsed:      map[int64]string{},
		failed:      map[int64]string{},
		unsupported: map[int64]string{},
	}
}

func (s *fakeStore) MarkFileParsing(_ context.Context, fileID int64) (store.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.parsingErr != nil {
		return store.File{}, s.parsingErr
	}
	return store.File{
		ID:          fileID,
		GroupID:     "group-1",
		FileName:    "PRD.md",
		StoragePath: "group-1/abc.md",
		ParseStatus: store.ParseStatusParsing,
	}, nil
}

func (s *fakeStore) MarkFileParsed(_ context.Context, fileID int64, content string) (store.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.parsed[fileID] = content
	return store.File{ID: fileID, GroupID: "group-1", ParseStatus: store.ParseStatusParsed}, nil
}

func (s *fakeStore) MarkFileFailed(_ context.Context, fileID int64, reason string) (store.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed[fileID] = reason
	return store.File{ID: fileID, GroupID: "group-1", ParseStatus: store.ParseStatusFailed}, nil
}

func (s *fakeStore) MarkFileUnsupported(_ context.Context, fileID int64, reason string) (store.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unsupported[fileID] = reason
	return store.File{ID: fileID, GroupID: "group-1", ParseStatus: store.ParseStatusUnsupported}, nil
}

type fakeSource struct {
	content map[string]string
	err     error
}

func (s fakeSource) Open(rel string) (io.ReadCloser, error) {
	if s.err != nil {
		return nil, s.err
	}
	data, ok := s.content[rel]
	if !ok {
		return nil, errors.New("missing stored file")
	}
	return io.NopCloser(strings.NewReader(data)), nil
}

type fakeEvent struct {
	event string
	file  store.File
}

type fakeNotifier struct {
	mu     sync.Mutex
	events []fakeEvent
	done   chan struct{}
}

func (n *fakeNotifier) BroadcastFile(event string, f *store.File) {
	n.mu.Lock()
	n.events = append(n.events, fakeEvent{event: event, file: *f})
	n.mu.Unlock()
	if n.done != nil {
		n.done <- struct{}{}
	}
}

func (n *fakeNotifier) last() fakeEvent {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.events) == 0 {
		return fakeEvent{}
	}
	return n.events[len(n.events)-1]
}

func newTestWorker(client *Client, st ResultStore, src FileSource, nt Notifier) *Worker {
	return &Worker{
		client:   client,
		store:    st,
		files:    src,
		notifier: nt,
		ctx:      context.Background(),
		queue:    make(chan store.File, 8),
	}
}

func markdownServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestWorkerProcessSuccess(t *testing.T) {
	srv := markdownServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"filename":"PRD.md","markdown":"# Parsed"}`))
	})
	st := newFakeStore()
	nt := &fakeNotifier{}
	w := newTestWorker(NewClient(srv.URL, 5*time.Second), st, fakeSource{content: map[string]string{"group-1/abc.md": "raw"}}, nt)

	w.process(store.File{ID: 7, GroupID: "group-1"})

	if got := st.parsed[7]; got != "# Parsed" {
		t.Fatalf("stored content = %q, want %q", got, "# Parsed")
	}
	ev := nt.last()
	if ev.event != ws.EventFileParsed {
		t.Fatalf("event = %q, want %q", ev.event, ws.EventFileParsed)
	}
	if ev.file.ParseStatus != store.ParseStatusParsed {
		t.Fatalf("event status = %q, want parsed", ev.file.ParseStatus)
	}
}

func TestWorkerProcessUnsupported(t *testing.T) {
	srv := markdownServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		_, _ = w.Write([]byte(`{"detail":"unsupported extension: .png"}`))
	})
	st := newFakeStore()
	nt := &fakeNotifier{}
	w := newTestWorker(NewClient(srv.URL, 5*time.Second), st, fakeSource{content: map[string]string{"group-1/abc.md": "raw"}}, nt)

	w.process(store.File{ID: 8, GroupID: "group-1"})

	if _, ok := st.unsupported[8]; !ok {
		t.Fatal("expected unsupported status")
	}
	if len(st.failed) != 0 {
		t.Fatalf("unexpected failed statuses: %v", st.failed)
	}
	if ev := nt.last(); ev.event != ws.EventFileParseFailed || ev.file.ParseStatus != store.ParseStatusUnsupported {
		t.Fatalf("event = %+v", ev)
	}
}

func TestWorkerProcessFailure(t *testing.T) {
	srv := markdownServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})
	st := newFakeStore()
	nt := &fakeNotifier{}
	w := newTestWorker(NewClient(srv.URL, 5*time.Second), st, fakeSource{content: map[string]string{"group-1/abc.md": "raw"}}, nt)

	w.process(store.File{ID: 9, GroupID: "group-1"})

	if _, ok := st.failed[9]; !ok {
		t.Fatal("expected failed status")
	}
	if ev := nt.last(); ev.event != ws.EventFileParseFailed || ev.file.ParseStatus != store.ParseStatusFailed {
		t.Fatalf("event = %+v", ev)
	}
}

func TestWorkerProcessMissingStoredFile(t *testing.T) {
	st := newFakeStore()
	nt := &fakeNotifier{}
	w := newTestWorker(NewClient("http://127.0.0.1:1", time.Second), st, fakeSource{err: errors.New("gone")}, nt)

	w.process(store.File{ID: 10, GroupID: "group-1"})

	if reason := st.failed[10]; !strings.Contains(reason, "open stored file") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestWorkerSkipsDeletedFile(t *testing.T) {
	st := newFakeStore()
	st.parsingErr = pgx.ErrNoRows
	nt := &fakeNotifier{}
	w := newTestWorker(NewClient("http://127.0.0.1:1", time.Second), st, fakeSource{}, nt)

	w.process(store.File{ID: 11, GroupID: "group-1"})

	if len(st.parsed) != 0 || len(st.failed) != 0 || len(nt.events) != 0 {
		t.Fatalf("expected no action, got parsed=%v failed=%v events=%v", st.parsed, st.failed, nt.events)
	}
}

func TestWorkerEnqueueProcessesAsynchronously(t *testing.T) {
	srv := markdownServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"markdown":"ok"}`))
	})
	st := newFakeStore()
	nt := &fakeNotifier{done: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewWorker(ctx, NewClient(srv.URL, 5*time.Second), st, fakeSource{content: map[string]string{"group-1/abc.md": "raw"}}, nt, 1)

	w.Enqueue(store.File{ID: 12, GroupID: "group-1", StoragePath: "group-1/abc.md"})

	select {
	case <-nt.done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for parse event")
	}
	if got := st.parsed[12]; got != "ok" {
		t.Fatalf("stored content = %q", got)
	}
}
