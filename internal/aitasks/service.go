package aitasks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/Errrori/workpilot/internal/llmtrack"
	"github.com/Errrori/workpilot/internal/store"
)

const (
	// DefaultCharBudget bounds the material sent to the LLM.
	DefaultCharBudget = 12000
	// DefaultTimeout bounds one report generation.
	DefaultTimeout = 120 * time.Second
	// DefaultLookbackDays is used when the first run has no previous run.
	DefaultLookbackDays = 7

	maxErrorRunes = 500

	noMaterialContent = "本周期暂无可汇总的群内动态。"
	defaultReportName = "AI 报告"
)

// ChatModel is the non-streaming subset of the Eino chat model.
type ChatModel interface {
	Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error)
}

// Store reads report material and persists reports and chat announcements.
type Store interface {
	ListMessagesSince(ctx context.Context, groupID string, since, until time.Time, excludeSender string, limit int) ([]store.Message, error)
	ListTasksUpdatedSince(ctx context.Context, groupID string, since time.Time, limit int) ([]store.Task, error)
	ListUnfinishedTasks(ctx context.Context, groupID string, limit int) ([]store.Task, error)
	ListReportRisks(ctx context.Context, groupID string, since time.Time, limit int) ([]store.Risk, error)
	ListFilesSince(ctx context.Context, groupID string, since time.Time, limit int) ([]store.File, error)
	ListRepoActivitySince(ctx context.Context, groupID string, since time.Time, limit int) ([]store.RepoItem, error)
	InsertReport(ctx context.Context, in store.ReportInsert) (store.Report, error)
	MarkAiTaskRun(ctx context.Context, taskID int64, status, errMessage string, ranAt, nextRun time.Time) error
	InsertMessage(ctx context.Context, groupID, senderName, content string, citations []store.Citation) (store.Message, error)
}

type Config struct {
	ChatModel  ChatModel
	Store      Store
	CharBudget int
	Timeout    time.Duration
}

type Service struct {
	cfg Config
}

func NewService(cfg Config) *Service {
	if cfg.CharBudget <= 0 {
		cfg.CharBudget = DefaultCharBudget
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	return &Service{cfg: cfg}
}

// Result is one generated report plus the chat announcement, if posted.
type Result struct {
	Report  store.Report
	Message *store.Message
}

// ReportRequest describes one report generation. Scheduled AI tasks and
// workflow report nodes share this request; AiTaskID is nil for workflow runs.
type ReportRequest struct {
	GroupID      string
	AiTaskID     *int64
	Name         string
	Prompt       string
	Sources      []string
	LookbackDays int
	Timezone     string
	Trigger      string
	Actor        string
	LastRunAt    *time.Time
	CallSource   string
}

// ReportResult is the outcome of one report generation. GenErr carries a
// generation failure whose failed report row was still stored; the error
// returned next to it reports infrastructure failures instead.
type ReportResult struct {
	Report  store.Report
	Message *store.Message
	GenErr  error
}

// Generate runs one report for the task and records the outcome on both the
// report row and the task (last run, next run). A failed generation still
// produces a failed report so the failure is visible in the UI.
func (s *Service) Generate(ctx context.Context, task store.AiTask, trigger, actor string) (Result, error) {
	taskID := task.ID
	now := time.Now()
	res, err := s.GenerateReport(ctx, ReportRequest{
		GroupID:      task.GroupID,
		AiTaskID:     &taskID,
		Name:         task.Name,
		Prompt:       task.Prompt,
		Sources:      task.Sources,
		LookbackDays: task.LookbackDays,
		Timezone:     task.Timezone,
		Trigger:      trigger,
		Actor:        actor,
		LastRunAt:    task.LastRunAt,
		CallSource:   llmtrack.SourceAiTask,
	})

	status := store.ReportStatusSucceeded
	errMessage := ""
	if res.GenErr != nil {
		status = store.ReportStatusFailed
		errMessage = truncateRunes(res.GenErr.Error(), maxErrorRunes)
	}
	nextRun := nextRunOrFallback(task, now)
	if markErr := s.cfg.Store.MarkAiTaskRun(ctx, task.ID, status, errMessage, now, nextRun); markErr != nil {
		slog.Warn("mark ai task run", "task_id", task.ID, "error", markErr)
	}
	if err != nil {
		return Result{}, err
	}
	return Result{Report: res.Report, Message: res.Message}, res.GenErr
}

// GenerateReport gathers material, asks the model and stores one report row,
// then announces the report in the group on success. AiTaskID is nil for
// workflow runs; CallSource tags the llm_usage rows.
func (s *Service) GenerateReport(ctx context.Context, req ReportRequest) (ReportResult, error) {
	if s.cfg.ChatModel == nil || s.cfg.Store == nil {
		return ReportResult{}, errors.New("ai task service is not configured")
	}
	loc, err := LoadLocation(req.Timezone)
	if err != nil {
		return ReportResult{}, err
	}

	now := time.Now()
	periodStart, periodEnd := reportPeriod(req.LookbackDays, req.LastRunAt, now, loc)

	runCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	mat, gatherErr := s.gather(runCtx, req.GroupID, req.Sources, periodStart, periodEnd)

	var content string
	var genErr error
	if gatherErr != nil {
		genErr = gatherErr
	} else if mat.empty() {
		content = noMaterialContent
	} else {
		source := req.CallSource
		if source == "" {
			source = llmtrack.SourceAiTask
		}
		runCtx = llmtrack.WithCall(runCtx, source, req.GroupID)
		out, err := s.cfg.ChatModel.Generate(runCtx, buildPromptMessages(mat, req.Prompt, s.cfg.CharBudget, loc))
		if err != nil {
			genErr = fmt.Errorf("llm generate: %w", err)
		} else if out == nil || strings.TrimSpace(out.Content) == "" {
			genErr = errors.New("llm returned an empty response")
		} else {
			content = strings.TrimSpace(out.Content)
		}
	}

	status := store.ReportStatusSucceeded
	errMessage := ""
	if genErr != nil {
		status = store.ReportStatusFailed
		errMessage = truncateRunes(genErr.Error(), maxErrorRunes)
		content = ""
	}

	report, err := s.cfg.Store.InsertReport(runCtx, store.ReportInsert{
		GroupID:        req.GroupID,
		AiTaskID:       req.AiTaskID,
		Title:          reportTitle(req.Name, periodStart, periodEnd, loc),
		Content:        content,
		Status:         status,
		Error:          errMessage,
		Trigger:        req.Trigger,
		PeriodStart:    &periodStart,
		PeriodEnd:      &periodEnd,
		Metrics:        &mat.metrics,
		RelatedTaskIDs: mat.relatedTaskIDs(),
		RelatedRiskIDs: mat.relatedRiskIDs(),
		CreatedBy:      req.Actor,
	})
	if err != nil {
		return ReportResult{}, fmt.Errorf("insert report: %w", err)
	}

	result := ReportResult{Report: report, GenErr: genErr}
	if genErr == nil {
		message, err := s.cfg.Store.InsertMessage(ctx, req.GroupID, store.SenderAI, formatAnnouncement(report), nil)
		if err != nil {
			slog.Warn("post report to group", "report_id", report.ID, "group_id", req.GroupID, "error", err)
		} else {
			result.Message = &message
		}
	}
	return result, nil
}

// reportPeriod is [last run or now-lookback_days, now) in the given location.
func reportPeriod(lookbackDays int, lastRunAt *time.Time, now time.Time, loc *time.Location) (time.Time, time.Time) {
	if lookbackDays <= 0 {
		lookbackDays = DefaultLookbackDays
	}
	start := now.In(loc).AddDate(0, 0, -lookbackDays)
	if lastRunAt != nil {
		if last := lastRunAt.In(loc); last.After(start) {
			start = last
		}
	}
	return start, now.In(loc)
}

func reportTitle(name string, start, end time.Time, loc *time.Location) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = defaultReportName
	}
	if start.In(loc).Format("2006-01-02") == end.In(loc).Format("2006-01-02") {
		return fmt.Sprintf("%s（%s）", name, end.In(loc).Format("2006-01-02"))
	}
	return fmt.Sprintf("%s（%s ~ %s）", name, start.In(loc).Format("2006-01-02"), end.In(loc).Format("2006-01-02"))
}

func nextRunOrFallback(task store.AiTask, after time.Time) time.Time {
	next, err := NextRun(task.Schedule, task.Timezone, after)
	if err != nil {
		slog.Warn("ai task schedule invalid", "task_id", task.ID, "schedule", task.Schedule, "error", err, "retry_in", "1h")
		return after.Add(time.Hour)
	}
	return next
}

func formatAnnouncement(r store.Report) string {
	return fmt.Sprintf("【AI 报告】%s\n\n%s", r.Title, strings.TrimSpace(r.Content))
}
