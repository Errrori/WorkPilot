package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Workflow run statuses.
const (
	WorkflowRunStatusRunning   = "running"
	WorkflowRunStatusSucceeded = "succeeded"
	WorkflowRunStatusFailed    = "failed"
)

// Workflow node run statuses.
const (
	WorkflowNodeStatusPending   = "pending"
	WorkflowNodeStatusRunning   = "running"
	WorkflowNodeStatusSucceeded = "succeeded"
	WorkflowNodeStatusFailed    = "failed"
	WorkflowNodeStatusSkipped   = "skipped"
)

// Workflow triggers.
const (
	WorkflowTriggerManual   = "manual"
	WorkflowTriggerSchedule = "schedule"
)

const workflowColumns = `id, group_id::text, name, description, definition, enabled, schedule, timezone, created_by, last_run_at, last_status, last_error, next_run_at, created_at, updated_at`

func scanWorkflow(row interface {
	Scan(dest ...any) error
}) (Workflow, error) {
	var w Workflow
	var definition []byte
	var lastStatus *string
	err := row.Scan(&w.ID, &w.GroupID, &w.Name, &w.Description, &definition, &w.Enabled, &w.Schedule, &w.Timezone, &w.CreatedBy, &w.LastRunAt, &lastStatus, &w.LastError, &w.NextRunAt, &w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		return Workflow{}, err
	}
	if lastStatus != nil {
		w.LastStatus = *lastStatus
	}
	if len(definition) > 0 {
		w.Definition = json.RawMessage(definition)
	}
	return w, nil
}

// WorkflowInsert is one new workflow.
type WorkflowInsert struct {
	GroupID     string
	Name        string
	Description string
	Definition  []byte
	Enabled     bool
	Schedule    string
	Timezone    string
	CreatedBy   string
	NextRunAt   time.Time
}

// WorkflowUpdate patches a workflow; nil fields stay unchanged.
type WorkflowUpdate struct {
	Name        *string
	Description *string
	Definition  *[]byte
	Enabled     *bool
	Schedule    *string
	Timezone    *string
	NextRunAt   *time.Time
}

func InsertWorkflow(ctx context.Context, pool *pgxpool.Pool, in WorkflowInsert) (Workflow, error) {
	timezone := in.Timezone
	if timezone == "" {
		timezone = "Asia/Shanghai"
	}
	next := in.NextRunAt
	if next.IsZero() {
		next = time.Now()
	}
	return scanWorkflow(pool.QueryRow(ctx,
		`insert into workflows (group_id, name, description, definition, enabled, schedule, timezone, created_by, next_run_at)
		 values ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9)
		 returning `+workflowColumns,
		in.GroupID, in.Name, in.Description, in.Definition, in.Enabled, in.Schedule, timezone, in.CreatedBy, next,
	))
}

func ListWorkflows(ctx context.Context, pool *pgxpool.Pool, groupID string, limit int) ([]Workflow, error) {
	if limit < 1 {
		limit = 100
	}
	rows, err := pool.Query(ctx,
		`select `+workflowColumns+`
		 from workflows
		 where group_id = $1::uuid
		 order by id desc
		 limit $2`,
		groupID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workflows []Workflow
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		workflows = append(workflows, w)
	}
	return workflows, rows.Err()
}

func GetWorkflow(ctx context.Context, pool *pgxpool.Pool, groupID string, workflowID int64) (Workflow, error) {
	return scanWorkflow(pool.QueryRow(ctx,
		`select `+workflowColumns+`
		 from workflows
		 where group_id = $1::uuid and id = $2`,
		groupID, workflowID,
	))
}

// GetWorkflowByID loads a workflow without the group filter, for scheduled runs.
func GetWorkflowByID(ctx context.Context, pool *pgxpool.Pool, workflowID int64) (Workflow, error) {
	return scanWorkflow(pool.QueryRow(ctx,
		`select `+workflowColumns+`
		 from workflows
		 where id = $1`,
		workflowID,
	))
}

func UpdateWorkflow(ctx context.Context, pool *pgxpool.Pool, groupID string, workflowID int64, in WorkflowUpdate) (Workflow, error) {
	return scanWorkflow(pool.QueryRow(ctx,
		`update workflows set
		     name = coalesce($3::text, name),
		     description = coalesce($4::text, description),
		     definition = coalesce($5::jsonb, definition),
		     enabled = coalesce($6::bool, enabled),
		     schedule = coalesce($7::text, schedule),
		     timezone = coalesce($8::text, timezone),
		     next_run_at = coalesce($9::timestamptz, next_run_at),
		     updated_at = now()
		 where group_id = $1::uuid and id = $2
		 returning `+workflowColumns,
		groupID, workflowID, in.Name, in.Description, in.Definition, in.Enabled, in.Schedule, in.Timezone, in.NextRunAt,
	))
}

func DeleteWorkflow(ctx context.Context, pool *pgxpool.Pool, groupID string, workflowID int64) (Workflow, error) {
	return scanWorkflow(pool.QueryRow(ctx,
		`delete from workflows
		 where group_id = $1::uuid and id = $2
		 returning `+workflowColumns,
		groupID, workflowID,
	))
}

// ListDueWorkflows returns enabled scheduled workflows whose next run has passed.
func ListDueWorkflows(ctx context.Context, pool *pgxpool.Pool, limit int) ([]Workflow, error) {
	if limit < 1 {
		limit = 100
	}
	rows, err := pool.Query(ctx,
		`select `+workflowColumns+`
		 from workflows
		 where enabled and schedule <> '' and next_run_at <= now()
		 order by next_run_at asc
		 limit $1`,
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workflows []Workflow
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		workflows = append(workflows, w)
	}
	return workflows, rows.Err()
}

// MarkWorkflowRun records the outcome of a finished run on the workflow itself.
func MarkWorkflowRun(ctx context.Context, pool *pgxpool.Pool, workflowID int64, status, errMessage string, ranAt, nextRun time.Time) error {
	_, err := pool.Exec(ctx,
		`update workflows
		 set last_run_at = $2, last_status = $3, last_error = $4, next_run_at = $5, updated_at = now()
		 where id = $1`,
		workflowID, ranAt, status, errMessage, nextRun)
	return err
}

const workflowRunColumns = `id, workflow_id, group_id::text, status, trigger, error, started_at, finished_at`

func scanWorkflowRun(row interface {
	Scan(dest ...any) error
}) (WorkflowRun, error) {
	var r WorkflowRun
	err := row.Scan(&r.ID, &r.WorkflowID, &r.GroupID, &r.Status, &r.Trigger, &r.Error, &r.StartedAt, &r.FinishedAt)
	if err != nil {
		return WorkflowRun{}, err
	}
	return r, nil
}

// WorkflowRunInsert is one new workflow execution.
type WorkflowRunInsert struct {
	WorkflowID int64
	GroupID    string
	Trigger    string
}

func StartWorkflowRun(ctx context.Context, pool *pgxpool.Pool, in WorkflowRunInsert) (WorkflowRun, error) {
	trigger := in.Trigger
	if trigger == "" {
		trigger = WorkflowTriggerManual
	}
	return scanWorkflowRun(pool.QueryRow(ctx,
		`insert into workflow_runs (workflow_id, group_id, status, trigger)
		 values ($1, $2::uuid, $3, $4)
		 returning `+workflowRunColumns,
		in.WorkflowID, in.GroupID, WorkflowRunStatusRunning, trigger,
	))
}

func FinishWorkflowRun(ctx context.Context, pool *pgxpool.Pool, runID int64, status, errMessage string) error {
	_, err := pool.Exec(ctx,
		`update workflow_runs
		 set status = $2, error = $3, finished_at = now()
		 where id = $1`,
		runID, status, errMessage)
	return err
}

// ListWorkflowRuns returns newest runs first, without node runs.
func ListWorkflowRuns(ctx context.Context, pool *pgxpool.Pool, groupID string, workflowID int64, limit int) ([]WorkflowRun, error) {
	if limit < 1 {
		limit = 20
	}
	rows, err := pool.Query(ctx,
		`select `+workflowRunColumns+`
		 from workflow_runs
		 where group_id = $1::uuid and workflow_id = $2
		 order by id desc
		 limit $3`,
		groupID, workflowID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []WorkflowRun
	for rows.Next() {
		r, err := scanWorkflowRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

func GetWorkflowRun(ctx context.Context, pool *pgxpool.Pool, groupID string, runID int64) (WorkflowRun, error) {
	return scanWorkflowRun(pool.QueryRow(ctx,
		`select `+workflowRunColumns+`
		 from workflow_runs
		 where group_id = $1::uuid and id = $2`,
		groupID, runID,
	))
}

const workflowNodeRunColumns = `id, run_id, node_id, type, status, input, output, error, tokens, latency_ms, started_at, finished_at`

func scanWorkflowNodeRun(row interface {
	Scan(dest ...any) error
}) (WorkflowNodeRun, error) {
	var n WorkflowNodeRun
	var input, output []byte
	err := row.Scan(&n.ID, &n.RunID, &n.NodeID, &n.Type, &n.Status, &input, &output, &n.Error, &n.Tokens, &n.LatencyMS, &n.StartedAt, &n.FinishedAt)
	if err != nil {
		return WorkflowNodeRun{}, err
	}
	if len(input) > 0 {
		n.Input = json.RawMessage(input)
	}
	if len(output) > 0 {
		n.Output = json.RawMessage(output)
	}
	return n, nil
}

// WorkflowNodeRunInsert is one pending step of a run.
type WorkflowNodeRunInsert struct {
	RunID  int64
	NodeID string
	Type   string
}

func InsertWorkflowNodeRun(ctx context.Context, pool *pgxpool.Pool, in WorkflowNodeRunInsert) (WorkflowNodeRun, error) {
	return scanWorkflowNodeRun(pool.QueryRow(ctx,
		`insert into workflow_node_runs (run_id, node_id, type, status)
		 values ($1, $2, $3, $4)
		 returning `+workflowNodeRunColumns,
		in.RunID, in.NodeID, in.Type, WorkflowNodeStatusPending,
	))
}

func StartWorkflowNodeRun(ctx context.Context, pool *pgxpool.Pool, nodeRunID int64, input []byte) error {
	_, err := pool.Exec(ctx,
		`update workflow_node_runs
		 set status = $2, input = $3, started_at = now()
		 where id = $1`,
		nodeRunID, WorkflowNodeStatusRunning, input)
	return err
}

func FinishWorkflowNodeRun(ctx context.Context, pool *pgxpool.Pool, nodeRunID int64, status string, output []byte, errMessage string, tokens, latencyMS int) error {
	_, err := pool.Exec(ctx,
		`update workflow_node_runs
		 set status = $2, output = $3, error = $4, tokens = $5, latency_ms = $6, finished_at = now()
		 where id = $1`,
		nodeRunID, status, output, errMessage, tokens, latencyMS)
	return err
}

func SkipWorkflowNodeRun(ctx context.Context, pool *pgxpool.Pool, nodeRunID int64) error {
	_, err := pool.Exec(ctx,
		`update workflow_node_runs
		 set status = $2, finished_at = now()
		 where id = $1`,
		nodeRunID, WorkflowNodeStatusSkipped)
	return err
}

// ListWorkflowNodeRuns returns the steps of one run in execution order.
func ListWorkflowNodeRuns(ctx context.Context, pool *pgxpool.Pool, runID int64) ([]WorkflowNodeRun, error) {
	rows, err := pool.Query(ctx,
		`select `+workflowNodeRunColumns+`
		 from workflow_node_runs
		 where run_id = $1
		 order by id asc`,
		runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []WorkflowNodeRun
	for rows.Next() {
		n, err := scanWorkflowNodeRun(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// WorkflowStore adapts workflow persistence to the service and scheduler.
type WorkflowStore struct {
	Pool *pgxpool.Pool
}

func (s WorkflowStore) GetWorkflowByID(ctx context.Context, workflowID int64) (Workflow, error) {
	return GetWorkflowByID(ctx, s.Pool, workflowID)
}

func (s WorkflowStore) ListDueWorkflows(ctx context.Context, limit int) ([]Workflow, error) {
	return ListDueWorkflows(ctx, s.Pool, limit)
}

func (s WorkflowStore) StartWorkflowRun(ctx context.Context, in WorkflowRunInsert) (WorkflowRun, error) {
	return StartWorkflowRun(ctx, s.Pool, in)
}

func (s WorkflowStore) FinishWorkflowRun(ctx context.Context, runID int64, status, errMessage string) error {
	return FinishWorkflowRun(ctx, s.Pool, runID, status, errMessage)
}

func (s WorkflowStore) MarkWorkflowRun(ctx context.Context, workflowID int64, status, errMessage string, ranAt, nextRun time.Time) error {
	return MarkWorkflowRun(ctx, s.Pool, workflowID, status, errMessage, ranAt, nextRun)
}

func (s WorkflowStore) InsertWorkflowNodeRun(ctx context.Context, in WorkflowNodeRunInsert) (WorkflowNodeRun, error) {
	return InsertWorkflowNodeRun(ctx, s.Pool, in)
}

func (s WorkflowStore) StartWorkflowNodeRun(ctx context.Context, nodeRunID int64, input []byte) error {
	return StartWorkflowNodeRun(ctx, s.Pool, nodeRunID, input)
}

func (s WorkflowStore) FinishWorkflowNodeRun(ctx context.Context, nodeRunID int64, status string, output []byte, errMessage string, tokens, latencyMS int) error {
	return FinishWorkflowNodeRun(ctx, s.Pool, nodeRunID, status, output, errMessage, tokens, latencyMS)
}

func (s WorkflowStore) SkipWorkflowNodeRun(ctx context.Context, nodeRunID int64) error {
	return SkipWorkflowNodeRun(ctx, s.Pool, nodeRunID)
}
