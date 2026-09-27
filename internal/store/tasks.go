package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	TaskStatusSuggested = "suggested"
	TaskStatusTodo      = "todo"
	TaskStatusDoing     = "doing"
	TaskStatusDone      = "done"
	TaskStatusRejected  = "rejected"
)

const (
	TaskPriorityLow    = "low"
	TaskPriorityMedium = "medium"
	TaskPriorityHigh   = "high"
)

const (
	TaskSourceManual    = "manual"
	TaskSourceExtracted = "extracted"
)

func IsValidTaskStatus(status string) bool {
	switch status {
	case TaskStatusSuggested, TaskStatusTodo, TaskStatusDoing, TaskStatusDone, TaskStatusRejected:
		return true
	default:
		return false
	}
}

func IsValidTaskPriority(priority string) bool {
	switch priority {
	case TaskPriorityLow, TaskPriorityMedium, TaskPriorityHigh:
		return true
	default:
		return false
	}
}

// TaskInsert is one new task record.
type TaskInsert struct {
	GroupID     string
	Title       string
	Description string
	Assignee    string
	Status      string
	Priority    string
	Source      string
	CreatedBy   string
	ConfirmedBy string
	Citations   []Citation
}

// TaskUpdate patches task fields; nil fields stay unchanged.
type TaskUpdate struct {
	Title       *string
	Description *string
	Assignee    *string
	Status      *string
	Priority    *string
	Actor       string
}

const taskColumns = `id, group_id::text, title, description, assignee, status, priority, source, citations, created_by, confirmed_by, confirmed_at, created_at, updated_at`

func scanTask(row interface {
	Scan(dest ...any) error
}) (Task, error) {
	var t Task
	var raw []byte
	err := row.Scan(&t.ID, &t.GroupID, &t.Title, &t.Description, &t.Assignee, &t.Status, &t.Priority, &t.Source, &raw, &t.CreatedBy, &t.ConfirmedBy, &t.ConfirmedAt, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Task{}, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &t.Citations); err != nil {
			return Task{}, err
		}
	}
	return t, nil
}

func InsertTask(ctx context.Context, pool *pgxpool.Pool, in TaskInsert) (Task, error) {
	raw, err := encodeCitations(in.Citations)
	if err != nil {
		return Task{}, err
	}
	status := in.Status
	if status == "" {
		status = TaskStatusSuggested
	}
	priority := in.Priority
	if priority == "" {
		priority = TaskPriorityMedium
	}
	source := in.Source
	if source == "" {
		source = TaskSourceManual
	}
	var confirmedAt *time.Time
	if in.ConfirmedBy != "" {
		now := time.Now()
		confirmedAt = &now
	}
	return scanTask(pool.QueryRow(ctx,
		`insert into tasks (group_id, title, description, assignee, status, priority, source, citations, created_by, confirmed_by, confirmed_at)
		 values ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 returning `+taskColumns,
		in.GroupID, in.Title, in.Description, in.Assignee, status, priority, source, raw, in.CreatedBy, in.ConfirmedBy, confirmedAt,
	))
}

func ListTasks(ctx context.Context, pool *pgxpool.Pool, groupID, status string, limit int) ([]Task, error) {
	if limit < 1 {
		limit = 100
	}
	query := `select ` + taskColumns + `
	          from tasks
	          where group_id = $1::uuid`
	args := []any{groupID}
	if status != "" {
		query += ` and status = $2`
		args = append(args, status)
	}
	query += fmt.Sprintf(` order by updated_at desc, id desc limit $%d`, len(args)+1)
	args = append(args, limit)

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func GetTask(ctx context.Context, pool *pgxpool.Pool, groupID string, taskID int64) (Task, error) {
	return scanTask(pool.QueryRow(ctx,
		`select `+taskColumns+`
		 from tasks
		 where group_id = $1::uuid and id = $2`,
		groupID, taskID,
	))
}

// UpdateTask patches a task; leaving suggested sets who confirmed it and when.
func UpdateTask(ctx context.Context, pool *pgxpool.Pool, groupID string, taskID int64, in TaskUpdate) (Task, error) {
	return scanTask(pool.QueryRow(ctx,
		`update tasks set
		     title = coalesce($3::text, title),
		     description = coalesce($4::text, description),
		     assignee = coalesce($5::text, assignee),
		     status = coalesce($6::text, status),
		     priority = coalesce($7::text, priority),
		     confirmed_by = case
		         when $6::text is not null and $6::text <> 'suggested' and confirmed_at is null then $8::text
		         else confirmed_by end,
		     confirmed_at = case
		         when $6::text is not null and $6::text <> 'suggested' and confirmed_at is null then now()
		         else confirmed_at end,
		     updated_at = now()
		 where group_id = $1::uuid and id = $2
		 returning `+taskColumns,
		groupID, taskID, in.Title, in.Description, in.Assignee, in.Status, in.Priority, in.Actor,
	))
}

func DeleteTask(ctx context.Context, pool *pgxpool.Pool, groupID string, taskID int64) (Task, error) {
	return scanTask(pool.QueryRow(ctx,
		`delete from tasks
		 where group_id = $1::uuid and id = $2
		 returning `+taskColumns,
		groupID, taskID,
	))
}

// ExistingTaskTitles returns titles that already exist and were not rejected.
func ExistingTaskTitles(ctx context.Context, pool *pgxpool.Pool, groupID string) ([]string, error) {
	rows, err := pool.Query(ctx,
		`select title from tasks where group_id = $1::uuid and status <> 'rejected'`,
		groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var titles []string
	for rows.Next() {
		var title string
		if err := rows.Scan(&title); err != nil {
			return nil, err
		}
		titles = append(titles, title)
	}
	return titles, rows.Err()
}

// ListExtractionChunks returns indexed chunks with file names for task
// extraction, newest files first.
func ListExtractionChunks(ctx context.Context, pool *pgxpool.Pool, groupID string, fileID int64, limit int) ([]RetrievedChunk, error) {
	if limit < 1 {
		limit = 1
	}
	query := `select c.id, c.file_id, f.file_name, c.chunk_index, c.content
	          from doc_chunks c
	          join files f on f.id = c.file_id
	          where c.group_id = $1::uuid`
	args := []any{groupID}
	if fileID > 0 {
		query += ` and c.file_id = $2`
		args = append(args, fileID)
	}
	query += fmt.Sprintf(` order by c.file_id desc, c.chunk_index asc limit $%d`, len(args)+1)
	args = append(args, limit)

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []RetrievedChunk
	for rows.Next() {
		var ch RetrievedChunk
		if err := rows.Scan(&ch.ChunkID, &ch.FileID, &ch.FileName, &ch.ChunkIndex, &ch.Content); err != nil {
			return nil, err
		}
		chunks = append(chunks, ch)
	}
	return chunks, rows.Err()
}

// TaskStore adapts task persistence to the extraction service.
type TaskStore struct {
	Pool *pgxpool.Pool
}

func (s TaskStore) ListExtractionChunks(ctx context.Context, groupID string, fileID int64, limit int) ([]RetrievedChunk, error) {
	return ListExtractionChunks(ctx, s.Pool, groupID, fileID, limit)
}

func (s TaskStore) ExistingTaskTitles(ctx context.Context, groupID string) ([]string, error) {
	return ExistingTaskTitles(ctx, s.Pool, groupID)
}

func (s TaskStore) InsertTask(ctx context.Context, in TaskInsert) (Task, error) {
	return InsertTask(ctx, s.Pool, in)
}
