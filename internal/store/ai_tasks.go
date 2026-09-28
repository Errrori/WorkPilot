package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	ReportStatusSucceeded = "succeeded"
	ReportStatusFailed    = "failed"
)

const (
	ReportTriggerSchedule = "schedule"
	ReportTriggerManual   = "manual"
)

const (
	AiTaskSourceMessages = "messages"
	AiTaskSourceTasks    = "tasks"
	AiTaskSourceRisks    = "risks"
	AiTaskSourceFiles    = "files"
	AiTaskSourceGit      = "git"
)

// IsValidAiTaskSource reports whether the source name is selectable material.
func IsValidAiTaskSource(source string) bool {
	switch source {
	case AiTaskSourceMessages, AiTaskSourceTasks, AiTaskSourceRisks, AiTaskSourceFiles, AiTaskSourceGit:
		return true
	default:
		return false
	}
}

const aiTaskColumns = `id, group_id::text, name, prompt, schedule, timezone, sources, lookback_days, enabled, created_by, last_run_at, last_status, last_error, next_run_at, created_at, updated_at`

func scanAiTask(row interface {
	Scan(dest ...any) error
}) (AiTask, error) {
	var t AiTask
	var raw []byte
	var lastStatus *string
	err := row.Scan(&t.ID, &t.GroupID, &t.Name, &t.Prompt, &t.Schedule, &t.Timezone, &raw, &t.LookbackDays, &t.Enabled, &t.CreatedBy, &t.LastRunAt, &lastStatus, &t.LastError, &t.NextRunAt, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return AiTask{}, err
	}
	if lastStatus != nil {
		t.LastStatus = *lastStatus
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &t.Sources); err != nil {
			return AiTask{}, err
		}
	}
	if t.Sources == nil {
		t.Sources = []string{}
	}
	return t, nil
}

// AiTaskInsert is one new custom AI task.
type AiTaskInsert struct {
	GroupID      string
	Name         string
	Prompt       string
	Schedule     string
	Timezone     string
	Sources      []string
	LookbackDays int
	Enabled      bool
	CreatedBy    string
	NextRunAt    time.Time
}

// AiTaskUpdate patches an AI task; nil fields stay unchanged.
type AiTaskUpdate struct {
	Name         *string
	Prompt       *string
	Schedule     *string
	Timezone     *string
	Sources      *[]string
	LookbackDays *int
	Enabled      *bool
	NextRunAt    *time.Time
}

func encodeSources(sources []string) ([]byte, error) {
	if sources == nil {
		sources = []string{}
	}
	return json.Marshal(sources)
}

func InsertAiTask(ctx context.Context, pool *pgxpool.Pool, in AiTaskInsert) (AiTask, error) {
	raw, err := encodeSources(in.Sources)
	if err != nil {
		return AiTask{}, err
	}
	timezone := in.Timezone
	if timezone == "" {
		timezone = "Asia/Shanghai"
	}
	lookback := in.LookbackDays
	if lookback <= 0 {
		lookback = 7
	}
	return scanAiTask(pool.QueryRow(ctx,
		`insert into ai_tasks (group_id, name, prompt, schedule, timezone, sources, lookback_days, enabled, created_by, next_run_at)
		 values ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 returning `+aiTaskColumns,
		in.GroupID, in.Name, in.Prompt, in.Schedule, timezone, raw, lookback, in.Enabled, in.CreatedBy, in.NextRunAt,
	))
}

func ListAiTasks(ctx context.Context, pool *pgxpool.Pool, groupID string, limit int) ([]AiTask, error) {
	if limit < 1 {
		limit = 100
	}
	rows, err := pool.Query(ctx,
		`select `+aiTaskColumns+`
		 from ai_tasks
		 where group_id = $1::uuid
		 order by id desc
		 limit $2`,
		groupID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []AiTask
	for rows.Next() {
		t, err := scanAiTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func GetAiTask(ctx context.Context, pool *pgxpool.Pool, groupID string, taskID int64) (AiTask, error) {
	return scanAiTask(pool.QueryRow(ctx,
		`select `+aiTaskColumns+`
		 from ai_tasks
		 where group_id = $1::uuid and id = $2`,
		groupID, taskID,
	))
}

// GetAiTaskByID loads a task without the group filter, for scheduled runs.
func GetAiTaskByID(ctx context.Context, pool *pgxpool.Pool, taskID int64) (AiTask, error) {
	return scanAiTask(pool.QueryRow(ctx,
		`select `+aiTaskColumns+`
		 from ai_tasks
		 where id = $1`,
		taskID,
	))
}

func UpdateAiTask(ctx context.Context, pool *pgxpool.Pool, groupID string, taskID int64, in AiTaskUpdate) (AiTask, error) {
	var raw []byte
	if in.Sources != nil {
		var err error
		if raw, err = encodeSources(*in.Sources); err != nil {
			return AiTask{}, err
		}
	}
	return scanAiTask(pool.QueryRow(ctx,
		`update ai_tasks set
		     name = coalesce($3::text, name),
		     prompt = coalesce($4::text, prompt),
		     schedule = coalesce($5::text, schedule),
		     timezone = coalesce($6::text, timezone),
		     sources = coalesce($7::jsonb, sources),
		     lookback_days = coalesce($8::int, lookback_days),
		     enabled = coalesce($9::bool, enabled),
		     next_run_at = coalesce($10::timestamptz, next_run_at),
		     updated_at = now()
		 where group_id = $1::uuid and id = $2
		 returning `+aiTaskColumns,
		groupID, taskID, in.Name, in.Prompt, in.Schedule, in.Timezone, raw, in.LookbackDays, in.Enabled, in.NextRunAt,
	))
}

func DeleteAiTask(ctx context.Context, pool *pgxpool.Pool, groupID string, taskID int64) (AiTask, error) {
	return scanAiTask(pool.QueryRow(ctx,
		`delete from ai_tasks
		 where group_id = $1::uuid and id = $2
		 returning `+aiTaskColumns,
		groupID, taskID,
	))
}

// ListDueAiTasks returns enabled tasks whose next run time has passed.
func ListDueAiTasks(ctx context.Context, pool *pgxpool.Pool, limit int) ([]AiTask, error) {
	if limit < 1 {
		limit = 100
	}
	rows, err := pool.Query(ctx,
		`select `+aiTaskColumns+`
		 from ai_tasks
		 where enabled and next_run_at <= now()
		 order by next_run_at asc
		 limit $1`,
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []AiTask
	for rows.Next() {
		t, err := scanAiTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// SetAiTaskNextRun moves the next run time, used when a scan enqueues a run.
func SetAiTaskNextRun(ctx context.Context, pool *pgxpool.Pool, taskID int64, next time.Time) error {
	_, err := pool.Exec(ctx,
		`update ai_tasks set next_run_at = $2, updated_at = now() where id = $1`,
		taskID, next)
	return err
}

// MarkAiTaskRun records the outcome of a finished run.
func MarkAiTaskRun(ctx context.Context, pool *pgxpool.Pool, taskID int64, status, errMessage string, ranAt, nextRun time.Time) error {
	_, err := pool.Exec(ctx,
		`update ai_tasks
		 set last_run_at = $2, last_status = $3, last_error = $4, next_run_at = $5, updated_at = now()
		 where id = $1`,
		taskID, ranAt, status, errMessage, nextRun)
	return err
}

// ReportInsert is one report record with a final status.
type ReportInsert struct {
	GroupID        string
	AiTaskID       *int64
	Title          string
	Content        string
	Status         string
	Error          string
	Trigger        string
	PeriodStart    *time.Time
	PeriodEnd      *time.Time
	Metrics        *ReportMetrics
	RelatedTaskIDs []int64
	RelatedRiskIDs []int64
	CreatedBy      string
}

const reportColumns = `id, group_id::text, ai_task_id, title, content, status, error, trigger, period_start, period_end, metrics, related_task_ids, related_risk_ids, created_by, created_at, finished_at`

func scanReport(row interface {
	Scan(dest ...any) error
}) (Report, error) {
	var r Report
	var raw []byte
	err := row.Scan(&r.ID, &r.GroupID, &r.AiTaskID, &r.Title, &r.Content, &r.Status, &r.Error, &r.Trigger, &r.PeriodStart, &r.PeriodEnd, &raw, &r.RelatedTaskIDs, &r.RelatedRiskIDs, &r.CreatedBy, &r.CreatedAt, &r.FinishedAt)
	if err != nil {
		return Report{}, err
	}
	if len(raw) > 0 {
		var metrics ReportMetrics
		if err := json.Unmarshal(raw, &metrics); err != nil {
			return Report{}, err
		}
		r.Metrics = &metrics
	}
	return r, nil
}

func InsertReport(ctx context.Context, pool *pgxpool.Pool, in ReportInsert) (Report, error) {
	var raw []byte
	if in.Metrics != nil {
		var err error
		if raw, err = json.Marshal(in.Metrics); err != nil {
			return Report{}, err
		}
	}
	taskIDs := in.RelatedTaskIDs
	if taskIDs == nil {
		taskIDs = []int64{}
	}
	riskIDs := in.RelatedRiskIDs
	if riskIDs == nil {
		riskIDs = []int64{}
	}
	return scanReport(pool.QueryRow(ctx,
		`insert into reports (group_id, ai_task_id, title, content, status, error, trigger, period_start, period_end, metrics, related_task_ids, related_risk_ids, created_by, finished_at)
		 values ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, now())
		 returning `+reportColumns,
		in.GroupID, in.AiTaskID, in.Title, in.Content, in.Status, in.Error, in.Trigger, in.PeriodStart, in.PeriodEnd, raw, taskIDs, riskIDs, in.CreatedBy,
	))
}

// ListReports returns newest reports first; aiTaskID > 0 filters by task.
func ListReports(ctx context.Context, pool *pgxpool.Pool, groupID string, aiTaskID int64, limit int) ([]Report, error) {
	if limit < 1 {
		limit = 20
	}
	query := `select ` + reportColumns + `
	          from reports
	          where group_id = $1::uuid`
	args := []any{groupID}
	if aiTaskID > 0 {
		query += ` and ai_task_id = $2`
		args = append(args, aiTaskID)
	}
	query += fmt.Sprintf(` order by id desc limit $%d`, len(args)+1)
	args = append(args, limit)

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var reports []Report
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		reports = append(reports, r)
	}
	return reports, rows.Err()
}

func GetReport(ctx context.Context, pool *pgxpool.Pool, groupID string, reportID int64) (Report, error) {
	return scanReport(pool.QueryRow(ctx,
		`select `+reportColumns+`
		 from reports
		 where group_id = $1::uuid and id = $2`,
		groupID, reportID,
	))
}

func DeleteReport(ctx context.Context, pool *pgxpool.Pool, groupID string, reportID int64) (Report, error) {
	return scanReport(pool.QueryRow(ctx,
		`delete from reports
		 where group_id = $1::uuid and id = $2
		 returning `+reportColumns,
		groupID, reportID,
	))
}

// ListMessagesSince returns messages in [since, until) except from one sender
// (used to keep AI-published reports out of report material), oldest first.
func ListMessagesSince(ctx context.Context, pool *pgxpool.Pool, groupID string, since, until time.Time, excludeSender string, limit int) ([]Message, error) {
	if limit < 1 {
		limit = 500
	}
	rows, err := pool.Query(ctx,
		`select `+messageColumns+`
		 from messages
		 where group_id = $1::uuid and created_at >= $2 and created_at < $3 and ($4 = '' or sender_name <> $4)
		 order by id asc
		 limit $5`,
		groupID, since, until, excludeSender, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// ListTasksUpdatedSince returns tasks touched in the window, oldest update first.
func ListTasksUpdatedSince(ctx context.Context, pool *pgxpool.Pool, groupID string, since time.Time, limit int) ([]Task, error) {
	if limit < 1 {
		limit = 500
	}
	rows, err := pool.Query(ctx,
		`select `+taskColumns+`
		 from tasks
		 where group_id = $1::uuid and updated_at >= $2
		 order by updated_at asc, id asc
		 limit $3`,
		groupID, since, limit)
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

// ListReportRisks returns risks that are active (open/mitigating) or touched
// in the window, oldest update first.
func ListReportRisks(ctx context.Context, pool *pgxpool.Pool, groupID string, since time.Time, limit int) ([]Risk, error) {
	if limit < 1 {
		limit = 500
	}
	rows, err := pool.Query(ctx,
		`select `+riskColumns+`
		 from risks
		 where group_id = $1::uuid and (status in ('open', 'mitigating') or updated_at >= $2)
		 order by updated_at asc, id asc
		 limit $3`,
		groupID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var risks []Risk
	for rows.Next() {
		r, err := scanRisk(rows)
		if err != nil {
			return nil, err
		}
		risks = append(risks, r)
	}
	return risks, rows.Err()
}

// ListFilesSince returns files uploaded in the window, oldest first.
func ListFilesSince(ctx context.Context, pool *pgxpool.Pool, groupID string, since time.Time, limit int) ([]File, error) {
	if limit < 1 {
		limit = 200
	}
	rows, err := pool.Query(ctx,
		`select `+fileColumns+`
		 from files
		 where group_id = $1::uuid and created_at >= $2
		 order by id asc
		 limit $3`,
		groupID, since, limit)
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

// AiTaskStore adapts AI task persistence to the reports service.
type AiTaskStore struct {
	Pool *pgxpool.Pool
}

func (s AiTaskStore) GetAiTaskByID(ctx context.Context, taskID int64) (AiTask, error) {
	return GetAiTaskByID(ctx, s.Pool, taskID)
}

func (s AiTaskStore) ListDueAiTasks(ctx context.Context, limit int) ([]AiTask, error) {
	return ListDueAiTasks(ctx, s.Pool, limit)
}

func (s AiTaskStore) ListMessagesSince(ctx context.Context, groupID string, since, until time.Time, excludeSender string, limit int) ([]Message, error) {
	return ListMessagesSince(ctx, s.Pool, groupID, since, until, excludeSender, limit)
}

func (s AiTaskStore) ListTasksUpdatedSince(ctx context.Context, groupID string, since time.Time, limit int) ([]Task, error) {
	return ListTasksUpdatedSince(ctx, s.Pool, groupID, since, limit)
}

func (s AiTaskStore) ListUnfinishedTasks(ctx context.Context, groupID string, limit int) ([]Task, error) {
	return ListUnfinishedTasks(ctx, s.Pool, groupID, limit)
}

func (s AiTaskStore) ListReportRisks(ctx context.Context, groupID string, since time.Time, limit int) ([]Risk, error) {
	return ListReportRisks(ctx, s.Pool, groupID, since, limit)
}

func (s AiTaskStore) ListFilesSince(ctx context.Context, groupID string, since time.Time, limit int) ([]File, error) {
	return ListFilesSince(ctx, s.Pool, groupID, since, limit)
}

func (s AiTaskStore) ListRepoActivitySince(ctx context.Context, groupID string, since time.Time, limit int) ([]RepoItem, error) {
	return ListRepoActivitySince(ctx, s.Pool, groupID, since, limit)
}

func (s AiTaskStore) InsertReport(ctx context.Context, in ReportInsert) (Report, error) {
	return InsertReport(ctx, s.Pool, in)
}

func (s AiTaskStore) MarkAiTaskRun(ctx context.Context, taskID int64, status, errMessage string, ranAt, nextRun time.Time) error {
	return MarkAiTaskRun(ctx, s.Pool, taskID, status, errMessage, ranAt, nextRun)
}

func (s AiTaskStore) InsertMessage(ctx context.Context, groupID, senderName, content string, citations []Citation) (Message, error) {
	return InsertMessage(ctx, s.Pool, groupID, senderName, content, citations)
}
