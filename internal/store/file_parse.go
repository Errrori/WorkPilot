package store

import (
	"context"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	ParseStatusPending     = "pending"
	ParseStatusParsing     = "parsing"
	ParseStatusParsed      = "parsed"
	ParseStatusFailed      = "failed"
	ParseStatusUnsupported = "unsupported"
)

// MarkFileParsing moves a file into parsing unless it is already being parsed.
func MarkFileParsing(ctx context.Context, pool *pgxpool.Pool, fileID int64) (File, error) {
	return scanFile(pool.QueryRow(ctx,
		`update files
		 set parse_status = 'parsing', parse_error = ''
		 where id = $1 and parse_status <> 'parsing'
		 returning `+fileColumns,
		fileID,
	))
}

// MarkFileParsePending resets a file so it can be parsed again.
func MarkFileParsePending(ctx context.Context, pool *pgxpool.Pool, groupID string, fileID int64) (File, error) {
	return scanFile(pool.QueryRow(ctx,
		`update files
		 set parse_status = 'pending', parse_error = ''
		 where group_id = $1::uuid and id = $2 and parse_status <> 'parsing'
		 returning `+fileColumns,
		groupID, fileID,
	))
}

// MarkFileParsed stores the extracted markdown and marks the file as parsed.
func MarkFileParsed(ctx context.Context, pool *pgxpool.Pool, fileID int64, content string) (File, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return File{}, err
	}
	defer tx.Rollback(ctx)

	f, err := scanFile(tx.QueryRow(ctx,
		`update files
		 set parse_status = 'parsed', parse_error = '', parsed_at = now()
		 where id = $1
		 returning `+fileColumns,
		fileID,
	))
	if err != nil {
		return File{}, err
	}

	if _, err := tx.Exec(ctx,
		`insert into file_contents (file_id, group_id, content, char_count)
		 values ($1, $2::uuid, $3, $4)
		 on conflict (file_id) do update
		 set content = excluded.content, char_count = excluded.char_count, created_at = now()`,
		f.ID, f.GroupID, content, utf8.RuneCountInString(content),
	); err != nil {
		return File{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return File{}, err
	}
	return f, nil
}

func MarkFileFailed(ctx context.Context, pool *pgxpool.Pool, fileID int64, reason string) (File, error) {
	return markFileParseError(ctx, pool, fileID, ParseStatusFailed, reason)
}

func MarkFileUnsupported(ctx context.Context, pool *pgxpool.Pool, fileID int64, reason string) (File, error) {
	return markFileParseError(ctx, pool, fileID, ParseStatusUnsupported, reason)
}

func markFileParseError(ctx context.Context, pool *pgxpool.Pool, fileID int64, status, reason string) (File, error) {
	return scanFile(pool.QueryRow(ctx,
		`update files
		 set parse_status = $2, parse_error = $3
		 where id = $1
		 returning `+fileColumns,
		fileID, status, reason,
	))
}

// ResetParsingFiles returns parses interrupted by a restart to pending.
func ResetParsingFiles(ctx context.Context, pool *pgxpool.Pool) ([]File, error) {
	rows, err := pool.Query(ctx,
		`update files
		 set parse_status = 'pending'
		 where parse_status = 'parsing'
		 returning `+fileColumns)
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

func GetFileContent(ctx context.Context, pool *pgxpool.Pool, groupID string, fileID int64) (FileContent, error) {
	var c FileContent
	err := pool.QueryRow(ctx,
		`select file_id, group_id::text, content, char_count, created_at
		 from file_contents
		 where group_id = $1::uuid and file_id = $2`,
		groupID, fileID,
	).Scan(&c.FileID, &c.GroupID, &c.Content, &c.CharCount, &c.CreatedAt)
	return c, err
}

// FileParseStore adapts parse persistence to the parser worker.
type FileParseStore struct {
	Pool *pgxpool.Pool
}

func (s FileParseStore) MarkFileParsing(ctx context.Context, fileID int64) (File, error) {
	return MarkFileParsing(ctx, s.Pool, fileID)
}

func (s FileParseStore) MarkFileParsed(ctx context.Context, fileID int64, content string) (File, error) {
	return MarkFileParsed(ctx, s.Pool, fileID, content)
}

func (s FileParseStore) MarkFileFailed(ctx context.Context, fileID int64, reason string) (File, error) {
	return MarkFileFailed(ctx, s.Pool, fileID, reason)
}

func (s FileParseStore) MarkFileUnsupported(ctx context.Context, fileID int64, reason string) (File, error) {
	return MarkFileUnsupported(ctx, s.Pool, fileID, reason)
}
