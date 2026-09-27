package rag

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"

	"github.com/Errrori/workpilot/internal/store"
)

const DefaultTopK = 6

// MetaFileName carries the source file name on retrieved documents.
const MetaFileName = "file_name"

type groupContextKey struct{}

// ContextWithGroup scopes a retrieval to one group.
func ContextWithGroup(ctx context.Context, groupID string) context.Context {
	return context.WithValue(ctx, groupContextKey{}, groupID)
}

// GroupFromContext returns the group scope set by ContextWithGroup.
func GroupFromContext(ctx context.Context) (string, bool) {
	groupID, ok := ctx.Value(groupContextKey{}).(string)
	return groupID, ok && groupID != ""
}

// ChunkSearcher runs the vector query behind the retriever.
type ChunkSearcher interface {
	SearchChunks(ctx context.Context, groupID string, vector []float64, limit int) ([]store.RetrievedChunk, error)
}

// PgVectorRetriever implements the Eino retriever component over doc_chunks.
type PgVectorRetriever struct {
	embedder embedding.Embedder
	searcher ChunkSearcher
	topK     int
}

var _ retriever.Retriever = (*PgVectorRetriever)(nil)

func NewPgVectorRetriever(embedder embedding.Embedder, searcher ChunkSearcher, topK int) *PgVectorRetriever {
	if topK < 1 {
		topK = DefaultTopK
	}
	return &PgVectorRetriever{embedder: embedder, searcher: searcher, topK: topK}
}

// Retrieve embeds the query and returns the closest chunks of the group set on
// the context, ordered by descending cosine similarity.
func (r *PgVectorRetriever) Retrieve(ctx context.Context, query string, opts ...retriever.Option) ([]*schema.Document, error) {
	if r.embedder == nil || r.searcher == nil {
		return nil, fmt.Errorf("retriever is not configured")
	}
	groupID, ok := GroupFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("group id is required for retrieval")
	}
	options := retriever.GetCommonOptions(&retriever.Options{TopK: &r.topK}, opts...)

	vectors, err := r.embedder.EmbedStrings(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("embedder returned %d vectors, want 1", len(vectors))
	}

	limit := r.topK
	if options.TopK != nil {
		limit = *options.TopK
	}
	chunks, err := r.searcher.SearchChunks(ctx, groupID, vectors[0], limit)
	if err != nil {
		return nil, fmt.Errorf("search chunks: %w", err)
	}

	docs := make([]*schema.Document, 0, len(chunks))
	for _, ch := range chunks {
		if options.ScoreThreshold != nil && ch.Score < *options.ScoreThreshold {
			continue
		}
		doc := &schema.Document{
			ID:      ChunkID(ch.FileID, ch.ChunkIndex),
			Content: ch.Content,
			MetaData: map[string]any{
				MetaFileID:     ch.FileID,
				MetaGroupID:    groupID,
				MetaChunkIndex: ch.ChunkIndex,
				MetaFileName:   ch.FileName,
			},
		}
		docs = append(docs, doc.WithScore(ch.Score))
	}
	return docs, nil
}
