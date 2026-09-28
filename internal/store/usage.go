package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Errrori/workpilot/internal/llmtrack"
)

// LLMUsage is one tracked model call.
type LLMUsage struct {
	ID               int64     `json:"id"`
	GroupID          string    `json:"group_id,omitempty"`
	Source           string    `json:"source"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	LatencyMS        int       `json:"latency_ms"`
	Status           string    `json:"status"`
	Error            string    `json:"error,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// LLMUsageSummary aggregates tracked calls over a time window.
type LLMUsageSummary struct {
	Calls            int `json:"calls"`
	Failed           int `json:"failed"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	AvgLatencyMS     int `json:"avg_latency_ms"`
}

// LLMUsageSourceSummary is the per-source breakdown of the summary.
type LLMUsageSourceSummary struct {
	Source string `json:"source"`
	LLMUsageSummary
}

// UsageStore adapts llm_usage persistence to llmtrack.Recorder.
type UsageStore struct {
	Pool *pgxpool.Pool
}

func (s UsageStore) InsertLLMUsage(ctx context.Context, r llmtrack.Record) error {
	return InsertLLMUsage(ctx, s.Pool, r)
}

func InsertLLMUsage(ctx context.Context, pool *pgxpool.Pool, r llmtrack.Record) error {
	var groupID *string
	if r.GroupID != "" {
		groupID = &r.GroupID
	}
	_, err := pool.Exec(ctx,
		`insert into llm_usage
		     (group_id, source, provider, model, prompt_tokens, completion_tokens, total_tokens, latency_ms, status, error)
		 values ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		groupID, r.Source, r.Provider, r.Model,
		r.PromptTokens, r.CompletionTokens, r.TotalTokens, r.LatencyMS, r.Status, r.Error,
	)
	return err
}

const usageSummaryColumns = `count(*),
	 count(*) filter (where status = 'failed'),
	 coalesce(sum(prompt_tokens), 0),
	 coalesce(sum(completion_tokens), 0),
	 coalesce(sum(total_tokens), 0),
	 coalesce(avg(latency_ms), 0)::int`

func scanUsageSummary(row interface {
	Scan(dest ...any) error
}) (LLMUsageSummary, error) {
	var s LLMUsageSummary
	err := row.Scan(&s.Calls, &s.Failed, &s.PromptTokens, &s.CompletionTokens, &s.TotalTokens, &s.AvgLatencyMS)
	return s, err
}

func LLMUsageSummaryForGroup(ctx context.Context, pool *pgxpool.Pool, groupID string, since, until time.Time) (LLMUsageSummary, error) {
	return scanUsageSummary(pool.QueryRow(ctx,
		`select `+usageSummaryColumns+`
		 from llm_usage
		 where group_id = $1::uuid and created_at >= $2 and created_at < $3`,
		groupID, since, until,
	))
}

func LLMUsageBySource(ctx context.Context, pool *pgxpool.Pool, groupID string, since, until time.Time) ([]LLMUsageSourceSummary, error) {
	rows, err := pool.Query(ctx,
		`select source, `+usageSummaryColumns+`
		 from llm_usage
		 where group_id = $1::uuid and created_at >= $2 and created_at < $3
		 group by source
		 order by sum(total_tokens) desc, source`,
		groupID, since, until,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []LLMUsageSourceSummary
	for rows.Next() {
		var s LLMUsageSourceSummary
		if err := rows.Scan(&s.Source, &s.Calls, &s.Failed, &s.PromptTokens, &s.CompletionTokens, &s.TotalTokens, &s.AvgLatencyMS); err != nil {
			return nil, err
		}
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
}

func ListLLMUsage(ctx context.Context, pool *pgxpool.Pool, groupID string, since, until time.Time, limit int) ([]LLMUsage, error) {
	rows, err := pool.Query(ctx,
		`select id, coalesce(group_id::text, ''), source, provider, model,
		        prompt_tokens, completion_tokens, total_tokens, latency_ms, status, error, created_at
		 from llm_usage
		 where group_id = $1::uuid and created_at >= $2 and created_at < $3
		 order by id desc
		 limit $4`,
		groupID, since, until, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var usage []LLMUsage
	for rows.Next() {
		var u LLMUsage
		if err := rows.Scan(&u.ID, &u.GroupID, &u.Source, &u.Provider, &u.Model,
			&u.PromptTokens, &u.CompletionTokens, &u.TotalTokens, &u.LatencyMS, &u.Status, &u.Error, &u.CreatedAt); err != nil {
			return nil, err
		}
		usage = append(usage, u)
	}
	return usage, rows.Err()
}
