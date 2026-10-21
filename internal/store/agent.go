package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Agent run statuses recorded in agent_runs.status.
const (
	AgentRunStatusRunning   = "running"
	AgentRunStatusSucceeded = "succeeded"
	AgentRunStatusFailed    = "failed"
)

// AgentRunInsert is one new agent run.
type AgentRunInsert struct {
	GroupID          string
	TriggerMessageID int64
	TriggerText      string
	CreatedBy        string
}

const agentRunColumns = `id, group_id::text, trigger_message_id, status, trigger_text, reply, steps, tokens, latency_ms, error, created_by, created_at, finished_at`

func scanAgentRun(row interface {
	Scan(dest ...any) error
}) (AgentRun, error) {
	var r AgentRun
	var steps []byte
	err := row.Scan(&r.ID, &r.GroupID, &r.TriggerMessageID, &r.Status, &r.TriggerText, &r.Reply, &steps, &r.Tokens, &r.LatencyMS, &r.Error, &r.CreatedBy, &r.CreatedAt, &r.FinishedAt)
	if err != nil {
		return AgentRun{}, err
	}
	if len(steps) > 0 {
		r.Steps = steps
	}
	return r, nil
}

func InsertAgentRun(ctx context.Context, pool *pgxpool.Pool, in AgentRunInsert) (AgentRun, error) {
	return scanAgentRun(pool.QueryRow(ctx,
		`insert into agent_runs (group_id, trigger_message_id, status, trigger_text, created_by)
		 values ($1::uuid, $2, 'running', $3, $4)
		 returning `+agentRunColumns,
		in.GroupID, in.TriggerMessageID, in.TriggerText, in.CreatedBy,
	))
}

// FinishAgentRun closes a run with its final reply and trace. steps is raw
// JSON; an empty string stores null.
func FinishAgentRun(ctx context.Context, pool *pgxpool.Pool, runID int64, status, reply, steps string, tokens, latencyMS int, errMessage string) error {
	var raw any
	if steps != "" {
		raw = steps
	}
	_, err := pool.Exec(ctx,
		`update agent_runs
		 set status = $2, reply = $3, steps = $4, tokens = $5, latency_ms = $6, error = $7, finished_at = now()
		 where id = $1`,
		runID, status, reply, raw, tokens, latencyMS, errMessage,
	)
	return err
}

// AgentStore adapts message, task and run persistence to the agent service.
type AgentStore struct {
	Pool *pgxpool.Pool
}

func (s AgentStore) GetMessageByID(ctx context.Context, messageID int64) (Message, error) {
	return GetMessageByID(ctx, s.Pool, messageID)
}

func (s AgentStore) ListMessages(ctx context.Context, groupID string, limit int) ([]Message, error) {
	return ListMessages(ctx, s.Pool, groupID, limit)
}

func (s AgentStore) InsertMessage(ctx context.Context, groupID, senderName, content string, citations []Citation) (Message, error) {
	return InsertMessage(ctx, s.Pool, groupID, senderName, content, citations)
}

func (s AgentStore) ListTasks(ctx context.Context, groupID, status string, limit int) ([]Task, error) {
	return ListTasks(ctx, s.Pool, groupID, status, limit)
}

func (s AgentStore) ListFiles(ctx context.Context, groupID string) ([]File, error) {
	return ListFiles(ctx, s.Pool, groupID)
}

func (s AgentStore) InsertAgentRun(ctx context.Context, in AgentRunInsert) (AgentRun, error) {
	return InsertAgentRun(ctx, s.Pool, in)
}

func (s AgentStore) FinishAgentRun(ctx context.Context, runID int64, status, reply, steps string, tokens, latencyMS int, errMessage string) error {
	return FinishAgentRun(ctx, s.Pool, runID, status, reply, steps, tokens, latencyMS, errMessage)
}
