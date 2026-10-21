package rag

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Errrori/workpilot/internal/store"
)

type fakeSparseSource struct {
	mu         sync.Mutex
	version    store.ChunkVersion
	chunks     []store.GroupChunk
	versionErr error
	listErr    error
	listCalls  int
}

func (s *fakeSparseSource) GroupChunkVersion(_ context.Context, _ string) (store.ChunkVersion, error) {
	if s.versionErr != nil {
		return store.ChunkVersion{}, s.versionErr
	}
	return s.version, nil
}

func (s *fakeSparseSource) ListGroupChunks(_ context.Context, _ string, limit int) ([]store.GroupChunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	if limit > 0 && len(s.chunks) > limit {
		return s.chunks[:limit], nil
	}
	return s.chunks, nil
}

func TestTokenizeMatchesCharacterBigramBaseline(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{input: "", want: nil},
		{input: "a", want: []string{"a"}},
		{input: "R-03 风险", want: []string{"r-", "-0", "03", "3风", "风险"}},
		{input: "风险", want: []string{"风险"}},
	}
	for _, tt := range tests {
		got := tokenize(tt.input)
		if len(got) != len(tt.want) {
			t.Fatalf("tokenize(%q) = %v, want %v", tt.input, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("tokenize(%q) = %v, want %v", tt.input, got, tt.want)
			}
		}
	}
}

func TestSparseIndexRanksExactIdentifier(t *testing.T) {
	index := buildSparseIndex([]store.GroupChunk{
		{ID: 1, Content: "R-03 风险负责人是 alice"},
		{ID: 2, Content: "星河项目每周五发布"},
	})

	hits := index.search("R-03 负责人", 10)
	if len(hits) == 0 {
		t.Fatal("expected a hit")
	}
	if hits[0].ID != 1 {
		t.Fatalf("top hit = %d, want 1", hits[0].ID)
	}
	if hits[0].Score <= 0 {
		t.Fatalf("score = %v, want positive", hits[0].Score)
	}
}

func TestBm25SearcherCachesUntilVersionChanges(t *testing.T) {
	source := &fakeSparseSource{
		version: store.ChunkVersion{Count: 1, MaxID: 1, MaxCreatedAt: time.Unix(1, 0)},
		chunks:  []store.GroupChunk{{ID: 1, Content: "旧版本发布计划"}},
	}
	searcher := NewBm25Searcher(source, SparseConfig{})

	first, err := searcher.Search(context.Background(), "group-1", "旧版本", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(first) != 1 || first[0].ID != 1 {
		t.Fatalf("first hits = %#v", first)
	}
	if _, err := searcher.Search(context.Background(), "group-1", "旧版本", 5); err != nil {
		t.Fatalf("second Search: %v", err)
	}
	if source.listCalls != 1 {
		t.Fatalf("list calls = %d, want 1", source.listCalls)
	}

	source.version = store.ChunkVersion{Count: 1, MaxID: 2, MaxCreatedAt: time.Unix(2, 0)}
	source.chunks = []store.GroupChunk{{ID: 2, Content: "新版本发布计划"}}
	hits, err := searcher.Search(context.Background(), "group-1", "新版本", 5)
	if err != nil {
		t.Fatalf("Search after change: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != 2 {
		t.Fatalf("hits after change = %#v", hits)
	}
	if source.listCalls != 2 {
		t.Fatalf("list calls = %d, want 2", source.listCalls)
	}
}

func TestBm25SearcherRejectsOversizedGroup(t *testing.T) {
	source := &fakeSparseSource{
		chunks: []store.GroupChunk{
			{ID: 1, Content: "a"},
			{ID: 2, Content: "b"},
		},
	}
	searcher := NewBm25Searcher(source, SparseConfig{MaxChunksPerGroup: 1})

	_, err := searcher.Search(context.Background(), "group-1", "a", 5)
	if !errors.Is(err, ErrSparseIndexTooLarge) {
		t.Fatalf("err = %v, want ErrSparseIndexTooLarge", err)
	}
}

func TestBm25SearcherReturnsNoHitsForEmptyGroup(t *testing.T) {
	searcher := NewBm25Searcher(&fakeSparseSource{}, SparseConfig{})

	hits, err := searcher.Search(context.Background(), "group-1", "anything", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits = %#v, want none", hits)
	}
}

func TestBm25SearcherPropagatesSourceErrors(t *testing.T) {
	source := &fakeSparseSource{versionErr: errors.New("db down")}
	searcher := NewBm25Searcher(source, SparseConfig{})
	if _, err := searcher.Search(context.Background(), "group-1", "q", 5); err == nil {
		t.Fatal("expected version error")
	}

	source = &fakeSparseSource{listErr: errors.New("db down")}
	searcher = NewBm25Searcher(source, SparseConfig{})
	if _, err := searcher.Search(context.Background(), "group-1", "q", 5); err == nil {
		t.Fatal("expected list error")
	}
}
