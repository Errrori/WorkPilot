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

// Generate runs one report for the task and records the outcome on both the
// report row and the task (last run, next run). A failed generation still
// produces a failed report so the failure is visible in the UI.
func (s *Service) Generate(ctx context.Context, task store.AiTask, trigger, actor string) (Result, error) {
	if s.cfg.ChatModel == nil || s.cfg.Store == nil {
		return Result{}, errors.New("ai task service is not configured")
	}
	loc, err := LoadLocation(task.Timezone)
	if err != nil {
		return Result{}, err
	}

	now := time.Now()
	periodStart, periodEnd := reportPeriod(task, now, loc)

	runCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	mat, gatherErr := s.gather(runCtx, task, periodStart, periodEnd)

	var content string
	var genErr error
	if gatherErr != nil {
		genErr = gatherErr
	} else if mat.empty() {
		content = noMaterialContent
	} else {
		runCtx = llmtrack.WithCall(runCtx, llmtrack.SourceAiTask, task.GroupID)
		out, err := s.cfg.ChatModel.Generate(runCtx, buildPromptMessages(mat, task.Prompt, s.cfg.CharBudget, loc))
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

	taskID := task.ID
	report, err := s.cfg.Store.InsertReport(runCtx, store.ReportInsert{
		GroupID:        task.GroupID,
		AiTaskID:       &taskID,
		Title:          reportTitle(task, periodStart, periodEnd, loc),
		Content:        content,
		Status:         status,
		Error:          errMessage,
		Trigger:        trigger,
		PeriodStart:    &periodStart,
		PeriodEnd:      &periodEnd,
		Metrics:        &mat.metrics,
		RelatedTaskIDs: mat.relatedTaskIDs(),
		RelatedRiskIDs: mat.relatedRiskIDs(),
		CreatedBy:      actor,
	})

	nextRun := nextRunOrFallback(task, now)
	if markErr := s.cfg.Store.MarkAiTaskRun(ctx, task.ID, status, errMessage, now, nextRun); markErr != nil {
		slog.Warn("mark ai task run", "task_id", task.ID, "error", markErr)
	}
	if err != nil {
		return Result{}, fmt.Errorf("insert report: %w", err)
	}

	result := Result{Report: report}
	if genErr == nil {
		message, err := s.cfg.Store.InsertMessage(ctx, task.GroupID, store.SenderAI, formatAnnouncement(report), nil)
		if err != nil {
			slog.Warn("post report to group", "report_id", report.ID, "group_id", task.GroupID, "error", err)
		} else {
			result.Message = &message
		}
	}
	if genErr != nil {
		return result, genErr
	}
	return result, nil
}

// reportPeriod is [last run or now-lookback_days, now) in the task timezone.
func reportPeriod(task store.AiTask, now time.Time, loc *time.Location) (time.Time, time.Time) {
	lookback := task.LookbackDays
	if lookback <= 0 {
		lookback = DefaultLookbackDays
	}
	start := now.In(loc).AddDate(0, 0, -lookback)
	if task.LastRunAt != nil {
		if last := task.LastRunAt.In(loc); last.After(start) {
			start = last
		}
	}
	return start, now.In(loc)
}

func reportTitle(task store.AiTask, start, end time.Time, loc *time.Location) string {
	if start.In(loc).Format("2006-01-02") == end.In(loc).Format("2006-01-02") {
		return fmt.Sprintf("%s（%s）", task.Name, end.In(loc).Format("2006-01-02"))
	}
	return fmt.Sprintf("%s（%s ~ %s）", task.Name, start.In(loc).Format("2006-01-02"), end.In(loc).Format("2006-01-02"))
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
