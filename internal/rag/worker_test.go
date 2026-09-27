package rag

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/components/indexer"
	"github.com/cloudwego/eino/schema"
	"github.com/jackc/pgx/v5"

	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

type fakeStore struct {
	mu          sync.Mutex
	content     store.FileContent
	contentErr  error
	indexingErr error
	indexed     map[int64]int
	failed      map[int64]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		indexed: map[int64]int{},
		failed:  map[int64]string{},
	}
}

func (s *fakeStore) MarkFileIndexing(_ context.Context, fileID int64) (store.File, error) {
	if s.indexingErr != nil {
		return store.File{}, s.indexingErr
	}
	return store.File{ID: fileID, GroupID: "group-1", FileName: "PRD.md", IndexStatus: store.IndexStatusIndexing}, nil
}

func (s *fakeStore) MarkFileIndexed(_ context.Context, fileID int64, chunkCount int) (store.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.indexed[fileID] = chunkCount
	return store.File{ID: fileID, GroupID: "group-1", IndexStatus: store.IndexStatusIndexed, ChunkCount: chunkCount}, nil
}

func (s *fakeStore) MarkFileIndexFailed(_ context.Context, fileID int64, reason string) (store.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed[fileID] = reason
	return store.File{ID: fileID, GroupID: "group-1", IndexStatus: store.IndexStatusFailed}, nil
}

func (s *fakeStore) GetFileContent(_ context.Context, _ string, _ int64) (store.FileContent, error) {
	if s.contentErr != nil {
		return store.FileContent{}, s.contentErr
	}
	return s.content, nil
}

type fakeEmbedder struct {
	mu    sync.Mutex
	dim   int
	err   error
	calls int
}

func (e *fakeEmbedder) EmbedStrings(_ context.Context, texts []string, _ ...embedding.Option) ([][]float64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float64, len(texts))
	for i := range texts {
		out[i] = make([]float64, e.dim)
		if e.dim > 0 {
			out[i][0] = float64(i)
		}
	}
	return out, nil
}

type fakeIndexer struct {
	mu   sync.Mutex
	docs []*schema.Document
	err  error
}

func (i *fakeIndexer) Store(_ context.Context, docs []*schema.Document, _ ...indexer.Option) ([]string, error) {
	if i.err != nil {
		return nil, i.err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.docs = append(i.docs, docs...)
	ids := make([]string, len(docs))
	for j, doc := range docs {
		ids[j] = doc.ID
	}
	return ids, nil
}

type fakeNotifier struct {
	mu     sync.Mutex
	events []string
	files  []store.File
}

func (n *fakeNotifier) BroadcastFile(event string, f *store.File) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, event)
	n.files = append(n.files, *f)
}

func newTestWorker(st IndexStore, emb embedding.Embedder, idx indexer.Indexer, nt Notifier, batch, dim int) *Worker {
	return &Worker{
		cfg: WorkerConfig{
			Chunker:   NewChunker(10, 0),
			Embedder:  emb,
			Dim:       dim,
			BatchSize: batch,
			Indexer:   idx,
			Store:     st,
			Notifier:  nt,
			Workers:   1,
		},
		ctx:   context.Background(),
		queue: make(chan store.File, 8),
	}
}

func TestWorkerIndexesFile(t *testing.T) {
	st := newFakeStore()
	st.content = store.FileContent{FileID: 7, GroupID: "group-1", Content: strings.Repeat("a", 10) + "\n\n" + strings.Repeat("b", 10)}
	emb := &fakeEmbedder{dim: 3}
	idx := &fakeIndexer{}
	nt := &fakeNotifier{}
	w := newTestWorker(st, emb, idx, nt, 16, 3)

	w.process(store.File{ID: 7, GroupID: "group-1"})

	if got := st.indexed[7]; got != 2 {
		t.Fatalf("chunk_count = %d, want 2", got)
	}
	if len(idx.docs) != 2 {
		t.Fatalf("stored docs = %d, want 2", len(idx.docs))
	}
	if idx.docs[0].ID != "7:0" || idx.docs[1].ID != "7:1" {
		t.Fatalf("doc IDs = %q, %q", idx.docs[0].ID, idx.docs[1].ID)
	}
	if len(idx.docs[0].DenseVector()) != 3 {
		t.Fatalf("doc embedding missing: %#v", idx.docs[0].MetaData)
	}
	if event := nt.events[len(nt.events)-1]; event != ws.EventFileIndexed {
		t.Fatalf("event = %q, want %q", event, ws.EventFileIndexed)
	}
	if nt.files[0].IndexStatus != store.IndexStatusIndexed {
		t.Fatalf("event status = %q", nt.files[0].IndexStatus)
	}
}

func TestWorkerBatchesEmbedding(t *testing.T) {
	var content []string
	for i := 0; i < 5; i++ {
		content = append(content, strings.Repeat(string(rune('a'+i)), 10))
	}
	st := newFakeStore()
	st.content = store.FileContent{FileID: 7, GroupID: "group-1", Content: strings.Join(content, "\n\n")}
	emb := &fakeEmbedder{dim: 2}
	w := newTestWorker(st, emb, &fakeIndexer{}, &fakeNotifier{}, 2, 2)

	w.process(store.File{ID: 7, GroupID: "group-1"})

	if emb.calls != 3 {
		t.Fatalf("embed calls = %d, want 3", emb.calls)
	}
}

func TestWorkerReportsEmbeddingFailure(t *testing.T) {
	st := newFakeStore()
	st.content = store.FileContent{FileID: 7, GroupID: "group-1", Content: "some content"}
	nt := &fakeNotifier{}
	w := newTestWorker(st, &fakeEmbedder{dim: 2, err: errors.New("connection refused")}, &fakeIndexer{}, nt, 16, 2)

	w.process(store.File{ID: 7, GroupID: "group-1"})

	if reason := st.failed[7]; !strings.Contains(reason, "embed chunks") {
		t.Fatalf("reason = %q", reason)
	}
	if event := nt.events[len(nt.events)-1]; event != ws.EventFileIndexFailed {
		t.Fatalf("event = %q, want %q", event, ws.EventFileIndexFailed)
	}
}

func TestWorkerReportsStoreFailure(t *testing.T) {
	st := newFakeStore()
	st.content = store.FileContent{FileID: 7, GroupID: "group-1", Content: "some content"}
	w := newTestWorker(st, &fakeEmbedder{dim: 2}, &fakeIndexer{err: errors.New("db down")}, &fakeNotifier{}, 16, 2)

	w.process(store.File{ID: 7, GroupID: "group-1"})

	if reason := st.failed[7]; !strings.Contains(reason, "store chunks") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestWorkerReportsDimensionMismatch(t *testing.T) {
	st := newFakeStore()
	st.content = store.FileContent{FileID: 7, GroupID: "group-1", Content: "some content"}
	w := newTestWorker(st, &fakeEmbedder{dim: 4}, &fakeIndexer{}, &fakeNotifier{}, 16, 2)

	w.process(store.File{ID: 7, GroupID: "group-1"})

	if reason := st.failed[7]; !strings.Contains(reason, "dimension") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestWorkerReportsMissingContent(t *testing.T) {
	st := newFakeStore()
	st.contentErr = pgx.ErrNoRows
	w := newTestWorker(st, &fakeEmbedder{dim: 2}, &fakeIndexer{}, &fakeNotifier{}, 16, 2)

	w.process(store.File{ID: 7, GroupID: "group-1"})

	if reason := st.failed[7]; !strings.Contains(reason, "parsed content not found") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestWorkerEmptyContentIndexesZeroChunks(t *testing.T) {
	st := newFakeStore()
	st.content = store.FileContent{FileID: 7, GroupID: "group-1", Content: "  \n\n "}
	idx := &fakeIndexer{}
	w := newTestWorker(st, &fakeEmbedder{dim: 2}, idx, &fakeNotifier{}, 16, 2)

	w.process(store.File{ID: 7, GroupID: "group-1"})

	if got := st.indexed[7]; got != 0 {
		t.Fatalf("chunk_count = %d, want 0", got)
	}
	if len(idx.docs) != 0 {
		t.Fatalf("stored docs = %d, want 0", len(idx.docs))
	}
}

func TestWorkerSkipsDeletedFile(t *testing.T) {
	st := newFakeStore()
	st.indexingErr = pgx.ErrNoRows
	nt := &fakeNotifier{}
	w := newTestWorker(st, &fakeEmbedder{dim: 2}, &fakeIndexer{}, nt, 16, 2)

	w.process(store.File{ID: 7, GroupID: "group-1"})

	if len(st.indexed) != 0 || len(st.failed) != 0 || len(nt.events) != 0 {
		t.Fatalf("expected no action, got indexed=%v failed=%v events=%v", st.indexed, st.failed, nt.events)
	}
}
