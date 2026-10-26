package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	DocSourceProviderGitHub = "github"

	DocSourceStatusSucceeded = "succeeded"
	DocSourceStatusFailed    = "failed"

	DocSourceItemIncluded = "included"
	DocSourceItemExcluded = "excluded"
	DocSourceItemFailed   = "failed"

	FileOriginUpload = "upload"
	FileOriginGitHub = "github"
)

const docSourceColumns = `id, group_id::text, provider, name, config, enabled, last_checked_at, last_synced_at, last_status, last_error, last_etag, next_sync_at, created_by, created_at, updated_at`

func scanDocSource(row interface {
	Scan(dest ...any) error
}) (DocSource, error) {
	var s DocSource
	var lastStatus *string
	err := row.Scan(&s.ID, &s.GroupID, &s.Provider, &s.Name, &s.Config, &s.Enabled, &s.LastCheckedAt, &s.LastSyncedAt, &lastStatus, &s.LastError, &s.LastETag, &s.NextSyncAt, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return DocSource{}, err
	}
	if lastStatus != nil {
		s.LastStatus = *lastStatus
	}
	return s, nil
}

type DocSourceInsert struct {
	GroupID    string
	Provider   string
	Name       string
	Config     DocSourceConfig
	CreatedBy  string
	NextSyncAt time.Time
}

func InsertDocSource(ctx context.Context, pool *pgxpool.Pool, in DocSourceInsert) (DocSource, error) {
	provider := in.Provider
	if provider == "" {
		provider = DocSourceProviderGitHub
	}
	nextSync := in.NextSyncAt
	if nextSync.IsZero() {
		nextSync = time.Now()
	}
	raw, err := json.Marshal(in.Config)
	if err != nil {
		return DocSource{}, err
	}
	return scanDocSource(pool.QueryRow(ctx,
		`insert into doc_sources (group_id, provider, name, config, created_by, next_sync_at)
		 values ($1::uuid, $2, $3, $4::jsonb, $5, $6)
		 returning `+docSourceColumns,
		in.GroupID, provider, in.Name, raw, in.CreatedBy, nextSync,
	))
}

func ListDocSources(ctx context.Context, pool *pgxpool.Pool, groupID string, limit int) ([]DocSource, error) {
	if limit < 1 {
		limit = 100
	}
	rows, err := pool.Query(ctx,
		`select `+docSourceColumns+`
		 from doc_sources
		 where group_id = $1::uuid
		 order by id asc
		 limit $2`,
		groupID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sources []DocSource
	for rows.Next() {
		s, err := scanDocSource(rows)
		if err != nil {
			return nil, err
		}
		sources = append(sources, s)
	}
	return sources, rows.Err()
}

func GetDocSource(ctx context.Context, pool *pgxpool.Pool, groupID string, sourceID int64) (DocSource, error) {
	return scanDocSource(pool.QueryRow(ctx,
		`select `+docSourceColumns+`
		 from doc_sources
		 where group_id = $1::uuid and id = $2`,
		groupID, sourceID,
	))
}

// GetDocSourceByID loads a source without the group filter, for scheduled syncs.
func GetDocSourceByID(ctx context.Context, pool *pgxpool.Pool, sourceID int64) (DocSource, error) {
	return scanDocSource(pool.QueryRow(ctx,
		`select `+docSourceColumns+`
		 from doc_sources
		 where id = $1`,
		sourceID,
	))
}

type DocSourceUpdate struct {
	Name       *string
	Config     *DocSourceConfig
	Enabled    *bool
	NextSyncAt *time.Time
}

func UpdateDocSource(ctx context.Context, pool *pgxpool.Pool, groupID string, sourceID int64, in DocSourceUpdate) (DocSource, error) {
	var raw []byte
	if in.Config != nil {
		encoded, err := json.Marshal(*in.Config)
		if err != nil {
			return DocSource{}, err
		}
		raw = encoded
	}
	return scanDocSource(pool.QueryRow(ctx,
		`update doc_sources set
		     name = coalesce($3, name),
		     config = coalesce($4::jsonb, config),
		     enabled = coalesce($5::bool, enabled),
		     next_sync_at = coalesce($6::timestamptz, next_sync_at),
		     updated_at = now()
		 where group_id = $1::uuid and id = $2
		 returning `+docSourceColumns,
		groupID, sourceID, in.Name, raw, in.Enabled, in.NextSyncAt,
	))
}

// DeleteDocSource removes the binding and archives every file it managed.
func DeleteDocSource(ctx context.Context, pool *pgxpool.Pool, groupID string, sourceID int64) (DocSource, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return DocSource{}, err
	}
	defer tx.Rollback(ctx)

	source, err := scanDocSource(tx.QueryRow(ctx,
		`select `+docSourceColumns+`
		 from doc_sources
		 where group_id = $1::uuid and id = $2
		 for update`,
		groupID, sourceID,
	))
	if err != nil {
		return DocSource{}, err
	}
	if _, err := tx.Exec(ctx, `update files set active = false where source_id = $1 and active`, sourceID); err != nil {
		return DocSource{}, err
	}
	if _, err := tx.Exec(ctx,
		`update doc_source_items
		 set active = false, state = 'excluded', file_id = null, pending_file_id = null, updated_at = now()
		 where source_id = $1`,
		sourceID,
	); err != nil {
		return DocSource{}, err
	}
	if _, err := tx.Exec(ctx, `delete from doc_sources where id = $1`, sourceID); err != nil {
		return DocSource{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DocSource{}, err
	}
	return source, nil
}

// ListDueDocSources returns enabled sources whose next sync time has passed.
func ListDueDocSources(ctx context.Context, pool *pgxpool.Pool, limit int) ([]DocSource, error) {
	if limit < 1 {
		limit = 100
	}
	rows, err := pool.Query(ctx,
		`select `+docSourceColumns+`
		 from doc_sources
		 where enabled and next_sync_at <= now()
		 order by next_sync_at asc
		 limit $1`,
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sources []DocSource
	for rows.Next() {
		s, err := scanDocSource(rows)
		if err != nil {
			return nil, err
		}
		sources = append(sources, s)
	}
	return sources, rows.Err()
}

// MarkDocSourceSync records a finished sync attempt and moves the next one.
func MarkDocSourceSync(ctx context.Context, pool *pgxpool.Pool, sourceID int64, status, errMessage string, checkedAt time.Time, syncedAt *time.Time, nextSync time.Time, etag string) (DocSource, error) {
	return scanDocSource(pool.QueryRow(ctx,
		`update doc_sources
		 set last_checked_at = $2,
		     last_synced_at = coalesce($3::timestamptz, last_synced_at),
		     last_status = $4,
		     last_error = $5,
		     next_sync_at = $6,
		     last_etag = $7,
		     updated_at = now()
		 where id = $1
		 returning `+docSourceColumns,
		sourceID, checkedAt, syncedAt, status, errMessage, nextSync, etag,
	))
}

const docSourceItemColumns = `id, source_id, external_id, content_hash, remote_updated_at, file_id, pending_file_id, state, active, created_at, updated_at`

func scanDocSourceItem(row interface {
	Scan(dest ...any) error
}) (DocSourceItem, error) {
	var it DocSourceItem
	err := row.Scan(&it.ID, &it.SourceID, &it.ExternalID, &it.ContentHash, &it.RemoteUpdatedAt, &it.FileID, &it.PendingFileID, &it.State, &it.Active, &it.CreatedAt, &it.UpdatedAt)
	return it, err
}

// ListDocSourceItemRecords returns the lightweight path records used by sync.
func ListDocSourceItemRecords(ctx context.Context, pool *pgxpool.Pool, sourceID int64) ([]DocSourceItem, error) {
	rows, err := pool.Query(ctx,
		`select `+docSourceItemColumns+`
		 from doc_source_items
		 where source_id = $1
		 order by external_id`,
		sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []DocSourceItem
	for rows.Next() {
		it, err := scanDocSourceItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// ListDocSourceItems returns items with their visible and pending file status.
func ListDocSourceItems(ctx context.Context, pool *pgxpool.Pool, sourceID int64, state string, limit int) ([]DocSourceItem, error) {
	if limit < 1 {
		limit = 200
	}
	query := `select i.id, i.source_id, i.external_id, i.content_hash, i.remote_updated_at,
	                 i.file_id, f.file_name, f.parse_status, f.index_status, f.chunk_count, f.active, f.indexed_at,
	                 i.pending_file_id, p.file_name, p.parse_status, p.index_status, p.chunk_count, p.active, p.indexed_at,
	                 i.state, i.active, i.created_at, i.updated_at
	          from doc_source_items i
	          left join files f on f.id = i.file_id
	          left join files p on p.id = i.pending_file_id
	          where i.source_id = $1`
	args := []any{sourceID}
	if state != "" {
		query += ` and i.state = $2`
		args = append(args, state)
	}
	query += ` order by i.external_id`
	query += ` limit $` + itoa(len(args)+1)
	args = append(args, limit)

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []DocSourceItem
	for rows.Next() {
		var it DocSourceItem
		var fileID, pendingID *int64
		var fileName, fileParse, fileIndex *string
		var fileChunks *int
		var fileActive *bool
		var fileIndexedAt *time.Time
		var pendingName, pendingParse, pendingIndex *string
		var pendingChunks *int
		var pendingActive *bool
		var pendingIndexedAt *time.Time
		if err := rows.Scan(
			&it.ID, &it.SourceID, &it.ExternalID, &it.ContentHash, &it.RemoteUpdatedAt,
			&fileID, &fileName, &fileParse, &fileIndex, &fileChunks, &fileActive, &fileIndexedAt,
			&pendingID, &pendingName, &pendingParse, &pendingIndex, &pendingChunks, &pendingActive, &pendingIndexedAt,
			&it.State, &it.Active, &it.CreatedAt, &it.UpdatedAt,
		); err != nil {
			return nil, err
		}
		it.FileID = fileID
		it.PendingFileID = pendingID
		if fileID != nil {
			it.File = &DocSourceFile{
				ID:          *fileID,
				FileName:    strOrEmpty(fileName),
				ParseStatus: strOrEmpty(fileParse),
				IndexStatus: strOrEmpty(fileIndex),
				ChunkCount:  intOrZero(fileChunks),
				Active:      fileActive != nil && *fileActive,
				IndexedAt:   fileIndexedAt,
			}
		}
		if pendingID != nil {
			it.PendingFile = &DocSourceFile{
				ID:          *pendingID,
				FileName:    strOrEmpty(pendingName),
				ParseStatus: strOrEmpty(pendingParse),
				IndexStatus: strOrEmpty(pendingIndex),
				ChunkCount:  intOrZero(pendingChunks),
				Active:      pendingActive != nil && *pendingActive,
				IndexedAt:   pendingIndexedAt,
			}
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

type DocSourceItemInput struct {
	ExternalID      string
	ContentHash     string
	RemoteUpdatedAt *time.Time
	FileID          *int64
	PendingFileID   *int64
	State           string
	Active          bool
}

func UpsertDocSourceItem(ctx context.Context, pool *pgxpool.Pool, sourceID int64, in DocSourceItemInput) (DocSourceItem, error) {
	state := in.State
	if state == "" {
		state = DocSourceItemIncluded
	}
	return scanDocSourceItem(pool.QueryRow(ctx,
		`insert into doc_source_items (source_id, external_id, content_hash, remote_updated_at, file_id, pending_file_id, state, active)
		 values ($1, $2, $3, $4, $5, $6, $7, $8)
		 on conflict (source_id, external_id) do update set
		     content_hash = excluded.content_hash,
		     remote_updated_at = coalesce(excluded.remote_updated_at, doc_source_items.remote_updated_at),
		     file_id = excluded.file_id,
		     pending_file_id = excluded.pending_file_id,
		     state = excluded.state,
		     active = excluded.active,
		     updated_at = now()
		 returning `+docSourceItemColumns,
		sourceID, in.ExternalID, in.ContentHash, in.RemoteUpdatedAt, in.FileID, in.PendingFileID, state, in.Active,
	))
}

// ArchiveDocSourceItem marks an upstream path as out of scope and hides both its
// visible and pending file versions.
func ArchiveDocSourceItem(ctx context.Context, pool *pgxpool.Pool, sourceID int64, externalID string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`update files set active = false
		 where id in (
		     select file_id from doc_source_items
		     where source_id = $1 and external_id = $2 and file_id is not null
		     union
		     select pending_file_id from doc_source_items
		     where source_id = $1 and external_id = $2 and pending_file_id is not null
		 )`,
		sourceID, externalID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`update doc_source_items
		 set active = false, state = 'excluded', file_id = null, pending_file_id = null, updated_at = now()
		 where source_id = $1 and external_id = $2`,
		sourceID, externalID,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ActivateDocFile promotes a freshly indexed pending version to visible and
// archives the previous version. It is a no-op for non-synced files.
func ActivateDocFile(ctx context.Context, pool *pgxpool.Pool, fileID int64) (DocSource, bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return DocSource{}, false, err
	}
	defer tx.Rollback(ctx)

	item, err := scanDocSourceItem(tx.QueryRow(ctx,
		`select `+docSourceItemColumns+`
		 from doc_source_items
		 where pending_file_id = $1
		 for update`,
		fileID,
	))
	if err == pgx.ErrNoRows {
		return DocSource{}, false, nil
	}
	if err != nil {
		return DocSource{}, false, err
	}

	if item.FileID != nil && *item.FileID != fileID {
		if _, err := tx.Exec(ctx, `update files set active = false where id = $1`, *item.FileID); err != nil {
			return DocSource{}, false, err
		}
	}
	if _, err := tx.Exec(ctx, `update files set active = true where id = $1`, fileID); err != nil {
		return DocSource{}, false, err
	}
	if _, err := tx.Exec(ctx,
		`update doc_source_items
		 set file_id = $2, pending_file_id = null, state = 'included', active = true,
		     remote_updated_at = coalesce(remote_updated_at, now()), updated_at = now()
		 where id = $1`,
		item.ID, fileID,
	); err != nil {
		return DocSource{}, false, err
	}
	source, err := scanDocSource(tx.QueryRow(ctx,
		`select `+docSourceColumns+` from doc_sources where id = $1`,
		item.SourceID,
	))
	if err != nil {
		return DocSource{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DocSource{}, false, err
	}
	return source, true, nil
}

// DocSourceStore adapts document-source persistence to the sync scheduler.
type DocSourceStore struct {
	Pool *pgxpool.Pool
}

func (s DocSourceStore) ListDueDocSources(ctx context.Context, limit int) ([]DocSource, error) {
	return ListDueDocSources(ctx, s.Pool, limit)
}

func (s DocSourceStore) GetDocSourceByID(ctx context.Context, sourceID int64) (DocSource, error) {
	return GetDocSourceByID(ctx, s.Pool, sourceID)
}

func (s DocSourceStore) ListDocSourceItemRecords(ctx context.Context, sourceID int64) ([]DocSourceItem, error) {
	return ListDocSourceItemRecords(ctx, s.Pool, sourceID)
}

func (s DocSourceStore) UpsertDocSourceItem(ctx context.Context, sourceID int64, in DocSourceItemInput) (DocSourceItem, error) {
	return UpsertDocSourceItem(ctx, s.Pool, sourceID, in)
}

func (s DocSourceStore) ArchiveDocSourceItem(ctx context.Context, sourceID int64, externalID string) error {
	return ArchiveDocSourceItem(ctx, s.Pool, sourceID, externalID)
}

func (s DocSourceStore) MarkDocSourceSync(ctx context.Context, sourceID int64, status, errMessage string, checkedAt time.Time, syncedAt *time.Time, nextSync time.Time, etag string) (DocSource, error) {
	return MarkDocSourceSync(ctx, s.Pool, sourceID, status, errMessage, checkedAt, syncedAt, nextSync, etag)
}

func (s DocSourceStore) InsertSyncedFile(ctx context.Context, in SyncedFileInsert) (File, error) {
	return InsertSyncedFile(ctx, s.Pool, in)
}

func (s DocSourceStore) ActivateDocFile(ctx context.Context, fileID int64) (DocSource, bool, error) {
	return ActivateDocFile(ctx, s.Pool, fileID)
}

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func intOrZero(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
