package store

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	IndexStatusPending  = "pending"
	IndexStatusIndexing = "indexing"
	IndexStatusIndexed  = "indexed"
	IndexStatusFailed   = "failed"
	IndexStatusSkipped  = "skipped"
)

// FileChunk is one embedded chunk to persist for a file.
type FileChunk struct {
	Index     int
	Content   string
	CharCount int
	Vector    []float64
}

// MarkFileIndexing moves a file into indexing unless it is already being indexed.
func MarkFileIndexing(ctx context.Context, pool *pgxpool.Pool, fileID int64) (File, error) {
	return scanFile(pool.QueryRow(ctx,
		`update files
		 set index_status = 'indexing', index_error = ''
		 where id = $1 and index_status <> 'indexing'
		 returning `+fileColumns,
		fileID,
	))
}

// MarkFileIndexPending resets a file so its chunks can be rebuilt.
func MarkFileIndexPending(ctx context.Context, pool *pgxpool.Pool, groupID string, fileID int64) (File, error) {
	return scanFile(pool.QueryRow(ctx,
		`update files
		 set index_status = 'pending', index_error = ''
		 where group_id = $1::uuid and id = $2 and index_status <> 'indexing'
		 returning `+fileColumns,
		groupID, fileID,
	))
}

func MarkFileIndexed(ctx context.Context, pool *pgxpool.Pool, fileID int64, chunkCount int) (File, error) {
	return scanFile(pool.QueryRow(ctx,
		`update files
		 set index_status = 'indexed', index_error = '', indexed_at = now(), chunk_count = $2
		 where id = $1
		 returning `+fileColumns,
		fileID, chunkCount,
	))
}

func MarkFileIndexFailed(ctx context.Context, pool *pgxpool.Pool, fileID int64, reason string) (File, error) {
	return scanFile(pool.QueryRow(ctx,
		`update files
		 set index_status = 'failed', index_error = $2
		 where id = $1
		 returning `+fileColumns,
		fileID, reason,
	))
}

// ResetIndexingFiles returns indexes interrupted by a restart to pending.
func ResetIndexingFiles(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx,
		`update files set index_status = 'pending' where index_status = 'indexing'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ListPendingIndexFiles returns parsed files whose chunks still need to be built.
func ListPendingIndexFiles(ctx context.Context, pool *pgxpool.Pool) ([]File, error) {
	rows, err := pool.Query(ctx,
		`select `+fileColumns+`
		 from files
		 where parse_status = 'parsed' and index_status = 'pending'
		 order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

// ReplaceFileChunks swaps all stored chunks of a file inside one transaction.
func ReplaceFileChunks(ctx context.Context, pool *pgxpool.Pool, groupID string, fileID int64, chunks []FileChunk) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `delete from doc_chunks where file_id = $1`, fileID); err != nil {
		return err
	}
	for _, ch := range chunks {
		if _, err := tx.Exec(ctx,
			`insert into doc_chunks (group_id, file_id, chunk_index, content, char_count, embedding)
			 values ($1::uuid, $2, $3, $4, $5, $6)`,
			groupID, fileID, ch.Index, ch.Content, ch.CharCount, formatVector(ch.Vector),
		); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// SearchChunks returns the chunks of a group closest to the query vector.
func SearchChunks(ctx context.Context, pool *pgxpool.Pool, groupID string, vector []float64, limit int) ([]RetrievedChunk, error) {
	if limit < 1 {
		limit = 1
	}
	rows, err := pool.Query(ctx,
		`select c.id, c.file_id, f.file_name, c.chunk_index, c.content,
		        1 - (c.embedding <=> $2::vector) as score
		 from doc_chunks c
		 join files f on f.id = c.file_id
		 where c.group_id = $1::uuid
		 order by c.embedding <=> $2::vector
		 limit $3`,
		groupID, formatVector(vector), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []RetrievedChunk
	for rows.Next() {
		var ch RetrievedChunk
		if err := rows.Scan(&ch.ChunkID, &ch.FileID, &ch.FileName, &ch.ChunkIndex, &ch.Content, &ch.Score); err != nil {
			return nil, err
		}
		chunks = append(chunks, ch)
	}
	return chunks, rows.Err()
}

func ListFileChunks(ctx context.Context, pool *pgxpool.Pool, groupID string, fileID int64) ([]DocChunk, error) {
	rows, err := pool.Query(ctx,
		`select id, file_id, group_id::text, chunk_index, content, char_count, created_at
		 from doc_chunks
		 where group_id = $1::uuid and file_id = $2
		 order by chunk_index`,
		groupID, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []DocChunk
	for rows.Next() {
		var ch DocChunk
		if err := rows.Scan(&ch.ID, &ch.FileID, &ch.GroupID, &ch.ChunkIndex, &ch.Content, &ch.CharCount, &ch.CreatedAt); err != nil {
			return nil, err
		}
		chunks = append(chunks, ch)
	}
	return chunks, rows.Err()
}

func formatVector(v []float64) string {
	var b strings.Builder
	b.Grow(len(v) * 8)
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(x, 'f', -1, 64))
	}
	b.WriteByte(']')
	return b.String()
}

// RagStore adapts index persistence to the rag worker.
type RagStore struct {
	Pool *pgxpool.Pool
}

func (s RagStore) MarkFileIndexing(ctx context.Context, fileID int64) (File, error) {
	return MarkFileIndexing(ctx, s.Pool, fileID)
}

func (s RagStore) MarkFileIndexed(ctx context.Context, fileID int64, chunkCount int) (File, error) {
	return MarkFileIndexed(ctx, s.Pool, fileID, chunkCount)
}

func (s RagStore) MarkFileIndexFailed(ctx context.Context, fileID int64, reason string) (File, error) {
	return MarkFileIndexFailed(ctx, s.Pool, fileID, reason)
}

func (s RagStore) GetFileContent(ctx context.Context, groupID string, fileID int64) (FileContent, error) {
	return GetFileContent(ctx, s.Pool, groupID, fileID)
}

func (s RagStore) SearchChunks(ctx context.Context, groupID string, vector []float64, limit int) ([]RetrievedChunk, error) {
	return SearchChunks(ctx, s.Pool, groupID, vector, limit)
}
