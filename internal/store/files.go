package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

const fileColumns = `id, group_id::text, uploader_name, file_name, content_type, size_bytes, storage_path, parse_status, parse_error, parsed_at, index_status, index_error, indexed_at, chunk_count, created_at`

func scanFile(row interface {
	Scan(dest ...any) error
}) (File, error) {
	var f File
	err := row.Scan(&f.ID, &f.GroupID, &f.UploaderName, &f.FileName, &f.ContentType, &f.SizeBytes, &f.StoragePath, &f.ParseStatus, &f.ParseError, &f.ParsedAt, &f.IndexStatus, &f.IndexError, &f.IndexedAt, &f.ChunkCount, &f.CreatedAt)
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

func ListFiles(ctx context.Context, pool *pgxpool.Pool, groupID string) ([]File, error) {
	rows, err := pool.Query(ctx,
		`select `+fileColumns+`
		 from files
		 where group_id = $1::uuid
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
