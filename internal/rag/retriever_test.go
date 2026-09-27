package rag

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/components/retriever"

	"github.com/Errrori/workpilot/internal/store"
)

type fakeChunkSearcher struct {
	groupID string
	limit   int
	vector  []float64
	chunks  []store.RetrievedChunk
	err     error
}

func (s *fakeChunkSearcher) SearchChunks(_ context.Context, groupID string, vector []float64, limit int) ([]store.RetrievedChunk, error) {
	s.groupID = groupID
	s.vector = vector
	s.limit = limit
	return s.chunks, s.err
}

func TestRetrieveReturnsScoredDocuments(t *testing.T) {
	searcher := &fakeChunkSearcher{chunks: []store.RetrievedChunk{
		{ChunkID: 1, FileID: 7, FileName: "PRD.md", ChunkIndex: 0, Content: "第一块", Score: 0.91},
		{ChunkID: 2, FileID: 7, FileName: "PRD.md", ChunkIndex: 1, Content: "第二块", Score: 0.82},
	}}
	r := NewPgVectorRetriever(&fakeEmbedder{dim: 3}, searcher, 5)
	ctx := ContextWithGroup(context.Background(), "group-1")

	docs, err := r.Retrieve(ctx, "项目进度如何")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if searcher.groupID != "group-1" {
		t.Fatalf("group = %q, want group-1", searcher.groupID)
	}
	if searcher.limit != 5 {
		t.Fatalf("limit = %d, want 5", searcher.limit)
	}
	if len(searcher.vector) != 3 {
		t.Fatalf("query vector dim = %d, want 3", len(searcher.vector))
	}
	if len(docs) != 2 {
		t.Fatalf("docs = %d, want 2", len(docs))
	}
	if docs[0].ID != "7:0" || docs[1].ID != "7:1" {
		t.Fatalf("doc IDs = %q, %q", docs[0].ID, docs[1].ID)
	}
	if got := docs[0].MetaData[MetaFileName]; got != "PRD.md" {
		t.Fatalf("file name meta = %v", got)
	}
	if got := docs[0].MetaData[MetaChunkIndex]; got != 0 {
		t.Fatalf("chunk index meta = %v", got)
	}
	if docs[0].Score() != 0.91 || docs[1].Score() != 0.82 {
		t.Fatalf("scores = %v, %v", docs[0].Score(), docs[1].Score())
	}
}

func TestRetrieveDefaultsTopK(t *testing.T) {
	searcher := &fakeChunkSearcher{}
	r := NewPgVectorRetriever(&fakeEmbedder{dim: 2}, searcher, 0)

	if _, err := r.Retrieve(ContextWithGroup(context.Background(), "group-1"), "q"); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if searcher.limit != DefaultTopK {
		t.Fatalf("limit = %d, want %d", searcher.limit, DefaultTopK)
	}
}

func TestRetrieveHonorsTopKOption(t *testing.T) {
	searcher := &fakeChunkSearcher{}
	r := NewPgVectorRetriever(&fakeEmbedder{dim: 2}, searcher, 5)

	if _, err := r.Retrieve(ContextWithGroup(context.Background(), "group-1"), "q", retriever.WithTopK(2)); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if searcher.limit != 2 {
		t.Fatalf("limit = %d, want 2", searcher.limit)
	}
}

func TestRetrieveFiltersByScoreThreshold(t *testing.T) {
	searcher := &fakeChunkSearcher{chunks: []store.RetrievedChunk{
		{FileID: 7, ChunkIndex: 0, Content: "high", Score: 0.9},
		{FileID: 7, ChunkIndex: 1, Content: "low", Score: 0.2},
	}}
	r := NewPgVectorRetriever(&fakeEmbedder{dim: 2}, searcher, 5)

	docs, err := r.Retrieve(ContextWithGroup(context.Background(), "group-1"), "q", retriever.WithScoreThreshold(0.5))
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(docs) != 1 || docs[0].Content != "high" {
		t.Fatalf("docs = %#v", docs)
	}
}

func TestRetrieveRequiresGroupScope(t *testing.T) {
	r := NewPgVectorRetriever(&fakeEmbedder{dim: 2}, &fakeChunkSearcher{}, 3)

	if _, err := r.Retrieve(context.Background(), "q"); err == nil {
		t.Fatal("expected error without group scope")
	}
}

func TestRetrieveReportsEmbeddingFailure(t *testing.T) {
	r := NewPgVectorRetriever(&fakeEmbedder{err: errors.New("ollama down")}, &fakeChunkSearcher{}, 3)

	if _, err := r.Retrieve(ContextWithGroup(context.Background(), "group-1"), "q"); err == nil {
		t.Fatal("expected embedding error")
	}
}

func TestRetrieveReportsSearchFailure(t *testing.T) {
	r := NewPgVectorRetriever(&fakeEmbedder{dim: 2}, &fakeChunkSearcher{err: errors.New("db down")}, 3)

	if _, err := r.Retrieve(ContextWithGroup(context.Background(), "group-1"), "q"); err == nil {
		t.Fatal("expected search error")
	}
}
