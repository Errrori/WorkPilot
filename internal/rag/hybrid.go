package rag

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"

	"github.com/Errrori/workpilot/internal/store"
)

const (
	DefaultCandidates   = 30
	DefaultRRFK         = 60
	DefaultHybridWeight = 1.0
	maxHybridCandidates = 200
)

// DenseScoreSearcher fills in vector scores for chunks that only matched the
// lexical side, so the semantic score floor applies to every candidate.
type DenseScoreSearcher interface {
	ChunkDenseScores(ctx context.Context, groupID string, vector []float64, ids []int64) (map[int64]float64, error)
}

type HybridConfig struct {
	Embedder    embedding.Embedder
	Dense       ChunkSearcher
	DenseScores DenseScoreSearcher
	Sparse      SparseSearcher

	TopK         int
	Candidates   int
	RRFK         float64
	DenseWeight  float64
	SparseWeight float64
}

// HybridRetriever fuses dense and BM25 candidates with reciprocal rank fusion.
type HybridRetriever struct {
	embedder     embedding.Embedder
	dense        ChunkSearcher
	denseScores  DenseScoreSearcher
	sparse       SparseSearcher
	topK         int
	candidates   int
	rrfK         float64
	denseWeight  float64
	sparseWeight float64
}

var _ retriever.Retriever = (*HybridRetriever)(nil)

func NewHybridRetriever(cfg HybridConfig) *HybridRetriever {
	topK := cfg.TopK
	if topK < 1 {
		topK = DefaultTopK
	}
	candidates := cfg.Candidates
	if candidates < 1 {
		candidates = DefaultCandidates
	}
	if candidates < topK {
		candidates = topK
	}
	if candidates > maxHybridCandidates {
		candidates = maxHybridCandidates
	}
	rrfK := cfg.RRFK
	if rrfK <= 0 {
		rrfK = DefaultRRFK
	}
	denseWeight := cfg.DenseWeight
	if denseWeight <= 0 {
		denseWeight = DefaultHybridWeight
	}
	sparseWeight := cfg.SparseWeight
	if sparseWeight <= 0 {
		sparseWeight = DefaultHybridWeight
	}
	return &HybridRetriever{
		embedder:     cfg.Embedder,
		dense:        cfg.Dense,
		denseScores:  cfg.DenseScores,
		sparse:       cfg.Sparse,
		topK:         topK,
		candidates:   candidates,
		rrfK:         rrfK,
		denseWeight:  denseWeight,
		sparseWeight: sparseWeight,
	}
}

func (r *HybridRetriever) Retrieve(ctx context.Context, query string, opts ...retriever.Option) ([]*schema.Document, error) {
	if r.embedder == nil || r.dense == nil {
		return nil, fmt.Errorf("hybrid retriever is not configured")
	}
	groupID, ok := GroupFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("group id is required for retrieval")
	}

	defaultTopK := r.topK
	options := retriever.GetCommonOptions(&retriever.Options{TopK: &defaultTopK}, opts...)
	limit := r.topK
	if options.TopK != nil {
		limit = *options.TopK
	}
	candidates := r.candidates
	if candidates < limit {
		candidates = limit
	}

	vectors, err := r.embedder.EmbedStrings(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("embedder returned %d vectors, want 1", len(vectors))
	}

	denseChunks, err := r.dense.SearchChunks(ctx, groupID, vectors[0], candidates)
	if err != nil {
		return nil, fmt.Errorf("search chunks: %w", err)
	}
	if r.sparse == nil {
		return chunkDocuments(denseChunks, groupID, options.ScoreThreshold, limit), nil
	}

	sparseHits, err := r.sparse.Search(ctx, groupID, query, candidates)
	if err != nil {
		slog.Warn("hybrid retrieval: sparse search failed; using dense only",
			"group_id", groupID, "error", err)
		return chunkDocuments(denseChunks, groupID, options.ScoreThreshold, limit), nil
	}

	docs, err := r.fuse(ctx, groupID, vectors[0], denseChunks, sparseHits, options.ScoreThreshold, limit)
	if err != nil {
		slog.Warn("hybrid retrieval: fusion failed; using dense only",
			"group_id", groupID, "error", err)
		return chunkDocuments(denseChunks, groupID, options.ScoreThreshold, limit), nil
	}
	slog.Debug("hybrid retrieval",
		"group_id", groupID,
		"dense_candidates", len(denseChunks),
		"sparse_candidates", len(sparseHits),
		"returned", len(docs),
	)
	return docs, nil
}

type fusedCandidate struct {
	chunk      store.GroupChunk
	denseScore float64
	hasDense   bool
	denseRank  int
	sparseRank int
	fusedScore float64
}

func (r *HybridRetriever) fuse(
	ctx context.Context,
	groupID string,
	vector []float64,
	denseChunks []store.RetrievedChunk,
	sparseHits []store.SparseHit,
	threshold *float64,
	limit int,
) ([]*schema.Document, error) {
	fused := make(map[int64]*fusedCandidate, len(denseChunks)+len(sparseHits))
	for rank, chunk := range denseChunks {
		candidate := fused[chunk.ChunkID]
		if candidate == nil {
			candidate = &fusedCandidate{chunk: store.GroupChunk{
				ID:         chunk.ChunkID,
				FileID:     chunk.FileID,
				FileName:   chunk.FileName,
				ChunkIndex: chunk.ChunkIndex,
				Content:    chunk.Content,
			}}
			fused[chunk.ChunkID] = candidate
		}
		candidate.denseScore = chunk.Score
		candidate.hasDense = true
		candidate.denseRank = rank + 1
	}
	for rank, hit := range sparseHits {
		candidate := fused[hit.ID]
		if candidate == nil {
			candidate = &fusedCandidate{chunk: hit.GroupChunk}
			fused[hit.ID] = candidate
		}
		candidate.sparseRank = rank + 1
	}

	missingIDs := make([]int64, 0, len(fused))
	for id, candidate := range fused {
		if !candidate.hasDense {
			missingIDs = append(missingIDs, id)
		}
	}
	if len(missingIDs) > 0 {
		if r.denseScores == nil {
			return nil, fmt.Errorf("dense score lookup is not configured")
		}
		scores, err := r.denseScores.ChunkDenseScores(ctx, groupID, vector, missingIDs)
		if err != nil {
			return nil, fmt.Errorf("load dense scores: %w", err)
		}
		for id, score := range scores {
			if candidate := fused[id]; candidate != nil {
				candidate.denseScore = score
				candidate.hasDense = true
			}
		}
	}

	ordered := make([]*fusedCandidate, 0, len(fused))
	for _, candidate := range fused {
		if threshold != nil && candidate.denseScore < *threshold {
			continue
		}
		if candidate.denseRank > 0 {
			candidate.fusedScore += r.denseWeight / (r.rrfK + float64(candidate.denseRank))
		}
		if candidate.sparseRank > 0 {
			candidate.fusedScore += r.sparseWeight / (r.rrfK + float64(candidate.sparseRank))
		}
		ordered = append(ordered, candidate)
	}
	sort.SliceStable(ordered, func(a, b int) bool {
		if ordered[a].fusedScore != ordered[b].fusedScore {
			return ordered[a].fusedScore > ordered[b].fusedScore
		}
		if ordered[a].denseScore != ordered[b].denseScore {
			return ordered[a].denseScore > ordered[b].denseScore
		}
		return ordered[a].chunk.ID < ordered[b].chunk.ID
	})
	if limit > 0 && len(ordered) > limit {
		ordered = ordered[:limit]
	}

	docs := make([]*schema.Document, 0, len(ordered))
	for _, candidate := range ordered {
		doc := &schema.Document{
			ID:      ChunkID(candidate.chunk.FileID, candidate.chunk.ChunkIndex),
			Content: candidate.chunk.Content,
			MetaData: map[string]any{
				MetaFileID:     candidate.chunk.FileID,
				MetaGroupID:    groupID,
				MetaChunkIndex: candidate.chunk.ChunkIndex,
				MetaFileName:   candidate.chunk.FileName,
			},
		}
		docs = append(docs, doc.WithScore(candidate.denseScore))
	}
	return docs, nil
}
