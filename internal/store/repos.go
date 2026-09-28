package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	RepoProviderGitHub = "github"

	RepoLastStatusSucceeded = "succeeded"
	RepoLastStatusFailed    = "failed"

	RepoKindPullRequest = "pull_request"
	RepoKindIssue       = "issue"
	RepoKindCommit      = "commit"
)

const repoColumns = `id, group_id::text, provider, owner, name, enabled, last_synced_at, last_status, last_error, next_sync_at, created_by, created_at, updated_at`

func scanRepo(row interface {
	Scan(dest ...any) error
}) (Repo, error) {
	var r Repo
	var lastStatus *string
	err := row.Scan(&r.ID, &r.GroupID, &r.Provider, &r.Owner, &r.Name, &r.Enabled, &r.LastSyncedAt, &lastStatus, &r.LastError, &r.NextSyncAt, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return Repo{}, err
	}
	if lastStatus != nil {
		r.LastStatus = *lastStatus
	}
	return r, nil
}

// RepoInsert is one new repository binding.
type RepoInsert struct {
	GroupID    string
	Provider   string
	Owner      string
	Name       string
	CreatedBy  string
	NextSyncAt time.Time
}

// RepoUpdate patches a repository binding; nil fields stay unchanged.
type RepoUpdate struct {
	Enabled    *bool
	NextSyncAt *time.Time
}

// IsUniqueViolation reports whether the error is a Postgres unique conflict.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func InsertRepo(ctx context.Context, pool *pgxpool.Pool, in RepoInsert) (Repo, error) {
	provider := in.Provider
	if provider == "" {
		provider = RepoProviderGitHub
	}
	nextSync := in.NextSyncAt
	if nextSync.IsZero() {
		nextSync = time.Now()
	}
	return scanRepo(pool.QueryRow(ctx,
		`insert into repos (group_id, provider, owner, name, created_by, next_sync_at)
		 values ($1::uuid, $2, $3, $4, $5, $6)
		 returning `+repoColumns,
		in.GroupID, provider, in.Owner, in.Name, in.CreatedBy, nextSync,
	))
}

func ListRepos(ctx context.Context, pool *pgxpool.Pool, groupID string, limit int) ([]Repo, error) {
	if limit < 1 {
		limit = 100
	}
	rows, err := pool.Query(ctx,
		`select `+repoColumns+`
		 from repos
		 where group_id = $1::uuid
		 order by id asc
		 limit $2`,
		groupID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var repos []Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		repos = append(repos, r)
	}
	return repos, rows.Err()
}

func GetRepo(ctx context.Context, pool *pgxpool.Pool, groupID string, repoID int64) (Repo, error) {
	return scanRepo(pool.QueryRow(ctx,
		`select `+repoColumns+`
		 from repos
		 where group_id = $1::uuid and id = $2`,
		groupID, repoID,
	))
}

// GetRepoByID loads a repository without the group filter, for scheduled syncs.
func GetRepoByID(ctx context.Context, pool *pgxpool.Pool, repoID int64) (Repo, error) {
	return scanRepo(pool.QueryRow(ctx,
		`select `+repoColumns+`
		 from repos
		 where id = $1`,
		repoID,
	))
}

func UpdateRepo(ctx context.Context, pool *pgxpool.Pool, groupID string, repoID int64, in RepoUpdate) (Repo, error) {
	return scanRepo(pool.QueryRow(ctx,
		`update repos set
		     enabled = coalesce($3::bool, enabled),
		     next_sync_at = coalesce($4::timestamptz, next_sync_at),
		     updated_at = now()
		 where group_id = $1::uuid and id = $2
		 returning `+repoColumns,
		groupID, repoID, in.Enabled, in.NextSyncAt,
	))
}

func DeleteRepo(ctx context.Context, pool *pgxpool.Pool, groupID string, repoID int64) (Repo, error) {
	return scanRepo(pool.QueryRow(ctx,
		`delete from repos
		 where group_id = $1::uuid and id = $2
		 returning `+repoColumns,
		groupID, repoID,
	))
}

// ListDueRepos returns enabled repositories whose next sync time has passed.
func ListDueRepos(ctx context.Context, pool *pgxpool.Pool, limit int) ([]Repo, error) {
	if limit < 1 {
		limit = 100
	}
	rows, err := pool.Query(ctx,
		`select `+repoColumns+`
		 from repos
		 where enabled and next_sync_at <= now()
		 order by next_sync_at asc
		 limit $1`,
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var repos []Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		repos = append(repos, r)
	}
	return repos, rows.Err()
}

// MarkRepoSync records the outcome of a finished sync and moves the next one.
func MarkRepoSync(ctx context.Context, pool *pgxpool.Pool, repoID int64, status, errMessage string, syncedAt, nextSync time.Time) (Repo, error) {
	return scanRepo(pool.QueryRow(ctx,
		`update repos
		 set last_synced_at = $2, last_status = $3, last_error = $4, next_sync_at = $5, updated_at = now()
		 where id = $1
		 returning `+repoColumns,
		repoID, syncedAt, status, errMessage, nextSync,
	))
}

// RepoItemInput is one synced remote item, upserted by (repo, kind, external id).
type RepoItemInput struct {
	Kind            string
	ExternalID      string
	Number          *int
	Title           string
	State           string
	Author          string
	URL             string
	RemoteUpdatedAt *time.Time
}

const repoItemColumns = `i.id, i.repo_id, i.kind, i.external_id, i.number, i.title, i.state, i.author, i.url, i.remote_updated_at, i.created_at, i.updated_at`

func scanRepoItem(row interface {
	Scan(dest ...any) error
}) (RepoItem, error) {
	var it RepoItem
	err := row.Scan(&it.ID, &it.RepoID, &it.Kind, &it.ExternalID, &it.Number, &it.Title, &it.State, &it.Author, &it.URL, &it.RemoteUpdatedAt, &it.CreatedAt, &it.UpdatedAt)
	if err != nil {
		return RepoItem{}, err
	}
	return it, nil
}

// UpsertRepoItems stores a sync batch, refreshing changed items. It returns the
// number of rows written.
func UpsertRepoItems(ctx context.Context, pool *pgxpool.Pool, repoID int64, items []RepoItemInput) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, it := range items {
		if _, err := tx.Exec(ctx,
			`insert into repo_items (repo_id, kind, external_id, number, title, state, author, url, remote_updated_at)
			 values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			 on conflict (repo_id, kind, external_id) do update set
			     number = excluded.number,
			     title = excluded.title,
			     state = excluded.state,
			     author = excluded.author,
			     url = excluded.url,
			     remote_updated_at = excluded.remote_updated_at,
			     updated_at = now()`,
			repoID, it.Kind, it.ExternalID, it.Number, it.Title, it.State, it.Author, it.URL, it.RemoteUpdatedAt,
		); err != nil {
			return 0, fmt.Errorf("upsert repo item %s %s: %w", it.Kind, it.ExternalID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(items), nil
}

// ListRepoItems returns synced items of one repository, newest first.
func ListRepoItems(ctx context.Context, pool *pgxpool.Pool, repoID int64, kind string, limit int) ([]RepoItem, error) {
	if limit < 1 {
		limit = 100
	}
	query := `select ` + repoItemColumns + `
	          from repo_items i
	          where i.repo_id = $1`
	args := []any{repoID}
	if kind != "" {
		query += ` and i.kind = $2`
		args = append(args, kind)
	}
	query += fmt.Sprintf(` order by i.remote_updated_at desc nulls last, i.id desc limit $%d`, len(args)+1)
	args = append(args, limit)

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []RepoItem
	for rows.Next() {
		it, err := scanRepoItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// ListRepoActivitySince returns group repository items updated in the window,
// oldest first, together with their repository owner/name.
func ListRepoActivitySince(ctx context.Context, pool *pgxpool.Pool, groupID string, since time.Time, limit int) ([]RepoItem, error) {
	if limit < 1 {
		limit = 200
	}
	rows, err := pool.Query(ctx,
		`select `+repoItemColumns+`, r.owner, r.name
		 from repo_items i
		 join repos r on r.id = i.repo_id
		 where r.group_id = $1::uuid and i.remote_updated_at >= $2
		 order by i.remote_updated_at asc, i.id asc
		 limit $3`,
		groupID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []RepoItem
	for rows.Next() {
		var it RepoItem
		if err := rows.Scan(&it.ID, &it.RepoID, &it.Kind, &it.ExternalID, &it.Number, &it.Title, &it.State, &it.Author, &it.URL, &it.RemoteUpdatedAt, &it.CreatedAt, &it.UpdatedAt, &it.RepoOwner, &it.RepoName); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// RepoStore adapts repository persistence to the sync scheduler.
type RepoStore struct {
	Pool *pgxpool.Pool
}

func (s RepoStore) ListDueRepos(ctx context.Context, limit int) ([]Repo, error) {
	return ListDueRepos(ctx, s.Pool, limit)
}

func (s RepoStore) GetRepoByID(ctx context.Context, repoID int64) (Repo, error) {
	return GetRepoByID(ctx, s.Pool, repoID)
}

func (s RepoStore) UpsertRepoItems(ctx context.Context, repoID int64, items []RepoItemInput) (int, error) {
	return UpsertRepoItems(ctx, s.Pool, repoID, items)
}

func (s RepoStore) MarkRepoSync(ctx context.Context, repoID int64, status, errMessage string, syncedAt, nextSync time.Time) (Repo, error) {
	return MarkRepoSync(ctx, s.Pool, repoID, status, errMessage, syncedAt, nextSync)
}
