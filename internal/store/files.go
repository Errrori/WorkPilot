package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

const fileColumns = `id, group_id::text, uploader_name, file_name, content_type, size_bytes, storage_path, parse_status, parse_error, parsed_at, index_status, index_error, indexed_at, chunk_count, origin, source_id, source_url, content_hash, active, created_at`

func scanFile(row interface {
	Scan(dest ...any) error
}) (File, error) {
	var f File
	err := row.Scan(&f.ID, &f.GroupID, &f.UploaderName, &f.FileName, &f.ContentType, &f.SizeBytes, &f.StoragePath, &f.ParseStatus, &f.ParseError, &f.ParsedAt, &f.IndexStatus, &f.IndexError, &f.IndexedAt, &f.ChunkCount, &f.Origin, &f.SourceID, &f.SourceURL, &f.ContentHash, &f.Active, &f.CreatedAt)
	return f, err
}

func InsertFile(ctx context.Context, pool *pgxpool.Pool, groupID, uploaderName, fileName, contentType, storagePath string, sizeBytes int64) (File, error) {
	return scanFile(pool.QueryRow(ctx,
		`insert into files (group_id, uploader_name, file_name, content_type, size_bytes, storage_path)
		 values ($1::uuid, $2, $3, $4, $5, $6)
		 returning `+fileColumns,
		groupID, uploaderName, fileName, contentType, sizeBytes, storagePath,
	))
}

// SyncedFileInsert stores one file version fetched from a document source.
type SyncedFileInsert struct {
	GroupID      string
	UploaderName string
	FileName     string
	ContentType  string
	StoragePath  string
	SizeBytes    int64
	Origin       string
	SourceID     int64
	SourceURL    string
	ContentHash  string
	Active       bool
}

func InsertSyncedFile(ctx context.Context, pool *pgxpool.Pool, in SyncedFileInsert) (File, error) {
	return scanFile(pool.QueryRow(ctx,
		`insert into files (group_id, uploader_name, file_name, content_type, size_bytes, storage_path, origin, source_id, source_url, content_hash, active)
		 values ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 returning `+fileColumns,
		in.GroupID, in.UploaderName, in.FileName, in.ContentType, in.SizeBytes, in.StoragePath,
		in.Origin, in.SourceID, in.SourceURL, in.ContentHash, in.Active,
	))
}

// ListFiles returns the visible files of a group. Archived versions remain
// downloadable by id but are hidden from the workspace and retrieval.
func ListFiles(ctx context.Context, pool *pgxpool.Pool, groupID string) ([]File, error) {
	rows, err := pool.Query(ctx,
		`select `+fileColumns+`
		 from files
		 where group_id = $1::uuid and active
		 order by id`,
		groupID,
	)
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

func GetFile(ctx context.Context, pool *pgxpool.Pool, groupID string, fileID int64) (File, error) {
	return scanFile(pool.QueryRow(ctx,
		`select `+fileColumns+`
		 from files
		 where group_id = $1::uuid and id = $2`,
		groupID, fileID,
	))
}

func DeleteFile(ctx context.Context, pool *pgxpool.Pool, groupID string, fileID int64) (File, error) {
	return scanFile(pool.QueryRow(ctx,
		`delete from files
		 where group_id = $1::uuid and id = $2
		 returning `+fileColumns,
		groupID, fileID,
	))
}
