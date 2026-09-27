package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	RiskStatusSuggested  = "suggested"
	RiskStatusOpen       = "open"
	RiskStatusMitigating = "mitigating"
	RiskStatusResolved   = "resolved"
	RiskStatusDismissed  = "dismissed"
)

const (
	RiskSeverityLow    = "low"
	RiskSeverityMedium = "medium"
	RiskSeverityHigh   = "high"
)

const (
	RiskSourceManual    = "manual"
	RiskSourceExtracted = "extracted"
)

func IsValidRiskStatus(status string) bool {
	switch status {
	case RiskStatusSuggested, RiskStatusOpen, RiskStatusMitigating, RiskStatusResolved, RiskStatusDismissed:
		return true
	default:
		return false
	}
}

func IsValidRiskSeverity(severity string) bool {
	switch severity {
	case RiskSeverityLow, RiskSeverityMedium, RiskSeverityHigh:
		return true
	default:
		return false
	}
}

// RiskInsert is one new risk record.
type RiskInsert struct {
	GroupID        string
	Title          string
	Description    string
	Severity       string
	Status         string
	Owner          string
	Source         string
	CreatedBy      string
	ConfirmedBy    string
	Citations      []Citation
	RelatedTaskIDs []int64
}

// RiskUpdate patches risk fields; nil fields stay unchanged.
type RiskUpdate struct {
	Title       *string
	Description *string
	Severity    *string
	Status      *string
	Owner       *string
	Actor       string
}

const riskColumns = `id, group_id::text, title, description, severity, status, owner, source, citations, related_task_ids, created_by, confirmed_by, confirmed_at, created_at, updated_at`

func scanRisk(row interface {
	Scan(dest ...any) error
}) (Risk, error) {
	var r Risk
	var raw []byte
	err := row.Scan(&r.ID, &r.GroupID, &r.Title, &r.Description, &r.Severity, &r.Status, &r.Owner, &r.Source, &raw, &r.RelatedTaskIDs, &r.CreatedBy, &r.ConfirmedBy, &r.ConfirmedAt, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return Risk{}, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &r.Citations); err != nil {
			return Risk{}, err
		}
	}
	return r, nil
}

func InsertRisk(ctx context.Context, pool *pgxpool.Pool, in RiskInsert) (Risk, error) {
	raw, err := encodeCitations(in.Citations)
	if err != nil {
		return Risk{}, err
	}
	status := in.Status
	if status == "" {
		status = RiskStatusSuggested
	}
	severity := in.Severity
	if severity == "" {
		severity = RiskSeverityMedium
	}
	source := in.Source
	if source == "" {
		source = RiskSourceManual
	}
	taskIDs := in.RelatedTaskIDs
	if taskIDs == nil {
		taskIDs = []int64{}
	}
	var confirmedAt *time.Time
	if in.ConfirmedBy != "" {
		now := time.Now()
		confirmedAt = &now
	}
	return scanRisk(pool.QueryRow(ctx,
		`insert into risks (group_id, title, description, severity, status, owner, source, citations, related_task_ids, created_by, confirmed_by, confirmed_at)
		 values ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		 returning `+riskColumns,
		in.GroupID, in.Title, in.Description, severity, status, in.Owner, source, raw, taskIDs, in.CreatedBy, in.ConfirmedBy, confirmedAt,
	))
}

func ListRisks(ctx context.Context, pool *pgxpool.Pool, groupID, status string, limit int) ([]Risk, error) {
	if limit < 1 {
		limit = 100
	}
	query := `select ` + riskColumns + `
	          from risks
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

func GetRisk(ctx context.Context, pool *pgxpool.Pool, groupID string, riskID int64) (Risk, error) {
	return scanRisk(pool.QueryRow(ctx,
		`select `+riskColumns+`
		 from risks
		 where group_id = $1::uuid and id = $2`,
		groupID, riskID,
	))
}

// UpdateRisk patches a risk; leaving suggested sets who confirmed it and when.
func UpdateRisk(ctx context.Context, pool *pgxpool.Pool, groupID string, riskID int64, in RiskUpdate) (Risk, error) {
	return scanRisk(pool.QueryRow(ctx,
		`update risks set
		     title = coalesce($3::text, title),
		     description = coalesce($4::text, description),
		     severity = coalesce($5::text, severity),
		     status = coalesce($6::text, status),
		     owner = coalesce($7::text, owner),
		     confirmed_by = case
		         when $6::text is not null and $6::text <> 'suggested' and confirmed_at is null then $8::text
		         else confirmed_by end,
		     confirmed_at = case
		         when $6::text is not null and $6::text <> 'suggested' and confirmed_at is null then now()
		         else confirmed_at end,
		     updated_at = now()
		 where group_id = $1::uuid and id = $2
		 returning `+riskColumns,
		groupID, riskID, in.Title, in.Description, in.Severity, in.Status, in.Owner, in.Actor,
	))
}

func DeleteRisk(ctx context.Context, pool *pgxpool.Pool, groupID string, riskID int64) (Risk, error) {
	return scanRisk(pool.QueryRow(ctx,
		`delete from risks
		 where group_id = $1::uuid and id = $2
		 returning `+riskColumns,
		groupID, riskID,
	))
}

// ExistingRiskTitles returns titles that already exist and were not dismissed.
func ExistingRiskTitles(ctx context.Context, pool *pgxpool.Pool, groupID string) ([]string, error) {
	rows, err := pool.Query(ctx,
		`select title from risks where group_id = $1::uuid and status <> 'dismissed'`,
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

// ListUnfinishedTasks returns tasks still needing attention, oldest update first.
func ListUnfinishedTasks(ctx context.Context, pool *pgxpool.Pool, groupID string, limit int) ([]Task, error) {
	if limit < 1 {
		limit = 1
	}
	rows, err := pool.Query(ctx,
		`select `+taskColumns+`
		 from tasks
		 where group_id = $1::uuid and status in ('suggested', 'todo', 'doing')
		 order by updated_at asc, id asc
		 limit $2`,
		groupID, limit)
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

// RiskStore adapts risk persistence to the extraction service.
type RiskStore struct {
	Pool *pgxpool.Pool
}

func (s RiskStore) ListUnfinishedTasks(ctx context.Context, groupID string, limit int) ([]Task, error) {
	return ListUnfinishedTasks(ctx, s.Pool, groupID, limit)
}

func (s RiskStore) ListExtractionChunks(ctx context.Context, groupID string, fileID int64, limit int) ([]RetrievedChunk, error) {
	return ListExtractionChunks(ctx, s.Pool, groupID, fileID, limit)
}

func (s RiskStore) ExistingRiskTitles(ctx context.Context, groupID string) ([]string, error) {
	return ExistingRiskTitles(ctx, s.Pool, groupID)
}

func (s RiskStore) InsertRisk(ctx context.Context, in RiskInsert) (Risk, error) {
	return InsertRisk(ctx, s.Pool, in)
}
