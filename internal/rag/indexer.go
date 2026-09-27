package rag

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/indexer"
	"github.com/cloudwego/eino/schema"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Errrori/workpilot/internal/store"
)

// Metadata keys carried on indexed documents.
const (
	MetaFileID     = "file_id"
	MetaGroupID    = "group_id"
	MetaChunkIndex = "chunk_index"
)

// PgVectorIndexer stores pre-embedded documents in the doc_chunks table.
type PgVectorIndexer struct {
	pool *pgxpool.Pool
}

func NewPgVectorIndexer(pool *pgxpool.Pool) *PgVectorIndexer {
	return &PgVectorIndexer{pool: pool}
}

// ChunkID builds the stable document ID for a file chunk.
func ChunkID(fileID int64, index int) string {
	return strconv.FormatInt(fileID, 10) + ":" + strconv.Itoa(index)
}

// Store replaces the chunks of every file referenced by docs.
func (i *PgVectorIndexer) Store(ctx context.Context, docs []*schema.Document, _ ...indexer.Option) ([]string, error) {
	type fileGroup struct {
		groupID string
		chunks  []store.FileChunk
	}

	groups := make(map[int64]*fileGroup)
	var order []int64
	ids := make([]string, 0, len(docs))

	for _, doc := range docs {
		if doc == nil {
			continue
		}
		fileID, err := metaInt64(doc.MetaData, MetaFileID)
		if err != nil {
			return nil, fmt.Errorf("document %q: %w", doc.ID, err)
		}
		groupID, err := metaString(doc.MetaData, MetaGroupID)
		if err != nil {
			return nil, fmt.Errorf("document %q: %w", doc.ID, err)
		}
		chunkIndex, err := metaInt(doc.MetaData, MetaChunkIndex)
		if err != nil {
			return nil, fmt.Errorf("document %q: %w", doc.ID, err)
		}
		vector := doc.DenseVector()
		if len(vector) == 0 {
			return nil, fmt.Errorf("document %q has no embedding", doc.ID)
		}

		group := groups[fileID]
		if group == nil {
			group = &fileGroup{groupID: groupID}
			groups[fileID] = group
			order = append(order, fileID)
		}
		group.chunks = append(group.chunks, store.FileChunk{
			Index:     chunkIndex,
			Content:   doc.Content,
			CharCount: utf8.RuneCountInString(doc.Content),
			Vector:    vector,
		})
		ids = append(ids, doc.ID)
	}

	for _, fileID := range order {
		group := groups[fileID]
		sort.Slice(group.chunks, func(a, b int) bool {
			return group.chunks[a].Index < group.chunks[b].Index
		})
		if err := store.ReplaceFileChunks(ctx, i.pool, group.groupID, fileID, group.chunks); err != nil {
			return nil, fmt.Errorf("replace chunks for file %d: %w", fileID, err)
		}
	}
	return ids, nil
}

func metaInt64(meta map[string]any, key string) (int64, error) {
	switch v := meta[key].(type) {
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	default:
		return 0, fmt.Errorf("missing metadata %q", key)
	}
}

func metaInt(meta map[string]any, key string) (int, error) {
	switch v := meta[key].(type) {
	case int:
		return v, nil
	case int64:
		return int(v), nil
	default:
		return 0, fmt.Errorf("missing metadata %q", key)
	}
}

func metaString(meta map[string]any, key string) (string, error) {
	v, ok := meta[key].(string)
	if !ok || v == "" {
		return "", fmt.Errorf("missing metadata %q", key)
	}
	return v, nil
}
