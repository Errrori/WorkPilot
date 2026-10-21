package rag

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"

	"github.com/Errrori/workpilot/internal/store"
)

type fakeSparseSearcher struct {
	groupID string
	query   string
	limit   int
	hits    []store.SparseHit
	err     error
}

func (s *fakeSparseSearcher) Search(_ context.Context, groupID, query string, limit int) ([]store.SparseHit, error) {
	s.groupID = groupID
	s.query = query
	s.limit = limit
	return s.hits, s.err
}

type fakeDenseScores struct {
	groupID string
	ids     []int64
	scores  map[int64]float64
	err     error
}

func (s *fakeDenseScores) ChunkDenseScores(_ context.Context, groupID string, _ []float64, ids []int64) (map[int64]float64, error) {
	s.groupID = groupID
	s.ids = append([]int64(nil), ids...)
	return s.scores, s.err
}

func denseChunk(id, fileID int64, index int, content string, score float64) store.RetrievedChunk {
	return store.RetrievedChunk{
		ChunkID:    id,
		FileID:     fileID,
		FileName:   "dense.md",
		ChunkIndex: index,
		Content:    content,
		Score:      score,
	}
}

func sparseHit(id, fileID int64, index int, content string, score float64) store.SparseHit {
	return store.SparseHit{
		GroupChunk: store.GroupChunk{
			ID:         id,
			FileID:     fileID,
			FileName:   "sparse.md",
			ChunkIndex: index,
			Content:    content,
		},
		Score: score,
	}
}

func TestHybridRetrieverFusesDenseAndSparse(t *testing.T) {
	dense := &fakeChunkSearcher{chunks: []store.RetrievedChunk{
		denseChunk(1, 10, 0, "dense one", 0.9),
		denseChunk(2, 10, 1, "dense two", 0.8),
		denseChunk(3, 10, 2, "dense three", 0.7),
	}}
	sparse := &fakeSparseSearcher{hits: []store.SparseHit{
		sparseHit(4, 11, 0, "sparse four", 5),
		sparseHit(2, 10, 1, "dense two", 4),
	}}
	scores := &fakeDenseScores{scores: map[int64]float64{4: 0.6}}
	r := NewHybridRetriever(HybridConfig{
		Embedder:    &fakeEmbedder{dim: 2},
		Dense:       dense,
		DenseScores: scores,
		Sparse:      sparse,
		TopK:        3,
		Candidates:  10,
	})

	docs, err := r.Retrieve(ContextWithGroup(context.Background(), "group-1"), "R-03 负责人")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if dense.groupID != "group-1" || sparse.groupID != "group-1" || scores.groupID != "group-1" {
		t.Fatalf("group scope not propagated: %q %q %q", dense.groupID, sparse.groupID, scores.groupID)
	}
	if dense.limit != 10 || sparse.limit != 10 {
		t.Fatalf("candidate limits = %d/%d, want 10/10", dense.limit, sparse.limit)
	}
	if len(scores.ids) != 1 || scores.ids[0] != 4 {
		t.Fatalf("dense score lookup ids = %v, want [4]", scores.ids)
	}
	if len(docs) != 3 {
		t.Fatalf("docs = %d, want 3", len(docs))
	}
	want := []string{"10:1", "10:0", "11:0"}
	for i, id := range want {
		if docs[i].ID != id {
			t.Fatalf("docs[%d].ID = %q, want %q (order %v)", i, docs[i].ID, id, docIDs(docs))
		}
	}
	if docs[0].Score() != 0.8 || docs[1].Score() != 0.9 || docs[2].Score() != 0.6 {
		t.Fatalf("scores = %v/%v/%v", docs[0].Score(), docs[1].Score(), docs[2].Score())
	}
}

func TestHybridRetrieverAppliesThresholdToSparseOnlyHits(t *testing.T) {
	dense := &fakeChunkSearcher{chunks: []store.RetrievedChunk{
		denseChunk(1, 10, 0, "dense one", 0.9),
	}}
	sparse := &fakeSparseSearcher{hits: []store.SparseHit{
		sparseHit(4, 11, 0, "sparse four", 5),
	}}
	scores := &fakeDenseScores{scores: map[int64]float64{4: 0.2}}
	r := NewHybridRetriever(HybridConfig{
		Embedder:    &fakeEmbedder{dim: 2},
		Dense:       dense,
		DenseScores: scores,
		Sparse:      sparse,
		TopK:        5,
	})

	docs, err := r.Retrieve(
		ContextWithGroup(context.Background(), "group-1"),
		"q",
		retriever.WithScoreThreshold(0.5),
	)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != "10:0" {
		t.Fatalf("docs = %v", docIDs(docs))
	}
}

func TestHybridRetrieverFallsBackToDenseWhenSparseFails(t *testing.T) {
	dense := &fakeChunkSearcher{chunks: []store.RetrievedChunk{
		denseChunk(1, 10, 0, "dense one", 0.9),
		denseChunk(2, 10, 1, "dense two", 0.2),
	}}
	r := NewHybridRetriever(HybridConfig{
		Embedder: &fakeEmbedder{dim: 2},
		Dense:    dense,
		Sparse:   &fakeSparseSearcher{err: errors.New("sparse down")},
		TopK:     5,
	})

	docs, err := r.Retrieve(
		ContextWithGroup(context.Background(), "group-1"),
		"q",
		retriever.WithScoreThreshold(0.5),
	)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != "10:0" {
		t.Fatalf("docs = %v", docIDs(docs))
	}
}

func TestHybridRetrieverFallsBackWhenDenseScoresFail(t *testing.T) {
	dense := &fakeChunkSearcher{chunks: []store.RetrievedChunk{
		denseChunk(1, 10, 0, "dense one", 0.9),
	}}
	r := NewHybridRetriever(HybridConfig{
		Embedder:    &fakeEmbedder{dim: 2},
		Dense:       dense,
		DenseScores: &fakeDenseScores{err: errors.New("db down")},
		Sparse: &fakeSparseSearcher{hits: []store.SparseHit{
			sparseHit(4, 11, 0, "sparse four", 5),
		}},
		TopK: 5,
	})

	docs, err := r.Retrieve(ContextWithGroup(context.Background(), "group-1"), "q")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != "10:0" {
		t.Fatalf("docs = %v", docIDs(docs))
	}
}

func TestHybridRetrieverHonorsTopKOption(t *testing.T) {
	dense := &fakeChunkSearcher{chunks: []store.RetrievedChunk{
		denseChunk(1, 10, 0, "one", 0.9),
		denseChunk(2, 10, 1, "two", 0.8),
		denseChunk(3, 10, 2, "three", 0.7),
	}}
	r := NewHybridRetriever(HybridConfig{
		Embedder:   &fakeEmbedder{dim: 2},
		Dense:      dense,
		TopK:       3,
		Candidates: 5,
	})

	docs, err := r.Retrieve(ContextWithGroup(context.Background(), "group-1"), "q", retriever.WithTopK(2))
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("docs = %d, want 2", len(docs))
	}
	if dense.limit != 5 {
		t.Fatalf("dense limit = %d, want candidates 5", dense.limit)
	}
}

func TestHybridRetrieverRequiresGroupScope(t *testing.T) {
	r := NewHybridRetriever(HybridConfig{
		Embedder: &fakeEmbedder{dim: 2},
		Dense:    &fakeChunkSearcher{},
	})
	if _, err := r.Retrieve(context.Background(), "q"); err == nil {
		t.Fatal("expected error without group scope")
	}
}

func TestHybridRetrieverReportsDenseFailure(t *testing.T) {
	r := NewHybridRetriever(HybridConfig{
		Embedder: &fakeEmbedder{dim: 2},
		Dense:    &fakeChunkSearcher{err: errors.New("db down")},
	})
	if _, err := r.Retrieve(ContextWithGroup(context.Background(), "group-1"), "q"); err == nil {
		t.Fatal("expected dense search error")
	}
}

func docIDs(docs []*schema.Document) []string {
	ids := make([]string, len(docs))
	for i, doc := range docs {
		ids[i] = doc.ID
	}
	return ids
}
