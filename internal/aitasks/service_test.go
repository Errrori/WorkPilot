package aitasks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/Errrori/workpilot/internal/store"
)

type fakeChatModel struct {
	reply  string
	err    error
	calls  int
	inputs []*schema.Message
}

func (m *fakeChatModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.calls++
	m.inputs = input
	if m.err != nil {
		return nil, m.err
	}
	return schema.AssistantMessage(m.reply, nil), nil
}

type markCall struct {
	status  string
	errMsg  string
	ranAt   time.Time
	nextRun time.Time
}

type fakeStore struct {
	messages []store.Message
	tasks    []store.Task
	risks    []store.Risk
	files    []store.File

	insertedReports []store.ReportInsert
	posted          []string
	marks           []markCall
	insertErr       error
	markErr         error

	sinceMessages time.Time
	untilMessages time.Time
	sinceTasks    time.Time
	sinceRisks    time.Time
	sinceFiles    time.Time
	messagesCall  bool
	tasksCall     bool
	risksCall     bool
	filesCall     bool
}

func (s *fakeStore) ListMessagesSince(_ context.Context, _ string, since, until time.Time, _ string, _ int) ([]store.Message, error) {
	s.messagesCall, s.sinceMessages, s.untilMessages = true, since, until
	return s.messages, nil
}

func (s *fakeStore) ListTasksUpdatedSince(_ context.Context, _ string, since time.Time, _ int) ([]store.Task, error) {
	s.tasksCall, s.sinceTasks = true, since
	return s.tasks, nil
}

func (s *fakeStore) ListUnfinishedTasks(_ context.Context, _ string, _ int) ([]store.Task, error) {
	return s.tasks, nil
}

func (s *fakeStore) ListReportRisks(_ context.Context, _ string, since time.Time, _ int) ([]store.Risk, error) {
	s.risksCall, s.sinceRisks = true, since
	return s.risks, nil
}

func (s *fakeStore) ListFilesSince(_ context.Context, _ string, since time.Time, _ int) ([]store.File, error) {
	s.filesCall, s.sinceFiles = true, since
	return s.files, nil
}

func (s *fakeStore) InsertReport(_ context.Context, in store.ReportInsert) (store.Report, error) {
	if s.insertErr != nil {
		return store.Report{}, s.insertErr
	}
	s.insertedReports = append(s.insertedReports, in)
	report := store.Report{
		ID:             int64(len(s.insertedReports)),
		GroupID:        in.GroupID,
		AiTaskID:       in.AiTaskID,
		Title:          in.Title,
		Content:        in.Content,
		Status:         in.Status,
		Error:          in.Error,
		Trigger:        in.Trigger,
		PeriodStart:    in.PeriodStart,
		PeriodEnd:      in.PeriodEnd,
		Metrics:        in.Metrics,
		RelatedTaskIDs: in.RelatedTaskIDs,
		RelatedRiskIDs: in.RelatedRiskIDs,
		CreatedBy:      in.CreatedBy,
		CreatedAt:      time.Now(),
	}
	return report, nil
}

func (s *fakeStore) MarkAiTaskRun(_ context.Context, _ int64, status, errMessage string, ranAt, nextRun time.Time) error {
	s.marks = append(s.marks, markCall{status: status, errMsg: errMessage, ranAt: ranAt, nextRun: nextRun})
	return s.markErr
}

func (s *fakeStore) InsertMessage(_ context.Context, _ string, senderName, content string, _ []store.Citation) (store.Message, error) {
	s.posted = append(s.posted, senderName+": "+content)
	return store.Message{ID: int64(len(s.posted)), GroupID: "group-1", SenderName: senderName, Content: content, CreatedAt: time.Now()}, nil
}

func testAiTask() store.AiTask {
	return store.AiTask{
		ID:           5,
		GroupID:      "group-1",
		Name:         "每周进展周报",
		Prompt:       "重点写风险和下周计划",
		Schedule:     "0 18 * * 5",
		Timezone:     "Asia/Shanghai",
		Sources:      []string{store.AiTaskSourceMessages, store.AiTaskSourceTasks, store.AiTaskSourceRisks, store.AiTaskSourceFiles},
		LookbackDays: 7,
		Enabled:      true,
		CreatedBy:    "alice",
	}
}

func testMaterialStore() *fakeStore {
	return &fakeStore{
		messages: []store.Message{
			{ID: 1, GroupID: "group-1", SenderName: "alice", Content: "登录模块开发完成", CreatedAt: time.Now().Add(-2 * time.Hour)},
			{ID: 2, GroupID: "group-1", SenderName: "bob", Content: "测试环境还没就绪", CreatedAt: time.Now().Add(-1 * time.Hour)},
		},
		tasks: []store.Task{
			{ID: 11, GroupID: "group-1", Title: "补齐测试环境", Status: store.TaskStatusTodo, Priority: store.TaskPriorityHigh, UpdatedAt: time.Now().Add(-3 * time.Hour)},
			{ID: 12, GroupID: "group-1", Title: "登录模块联调", Status: store.TaskStatusDoing, Assignee: "bob", UpdatedAt: time.Now().Add(-30 * time.Minute)},
		},
		risks: []store.Risk{
			{ID: 21, GroupID: "group-1", Title: "测试环境就绪时间未确认", Severity: store.RiskSeverityHigh, Status: store.RiskStatusOpen, UpdatedAt: time.Now().Add(-2 * time.Hour)},
		},
		files: []store.File{
			{ID: 31, GroupID: "group-1", FileName: "周会纪要.md", UploaderName: "alice", CreatedAt: time.Now().Add(-24 * time.Hour)},
		},
	}
}

func TestGenerateBuildsReport(t *testing.T) {
	chat := &fakeChatModel{reply: "# 本周进展\n\n- 登录模块开发完成\n\n## 风险\n\n- 测试环境未就绪"}
	st := testMaterialStore()
	svc := NewService(Config{ChatModel: chat, Store: st, CharBudget: 4000})

	result, err := svc.Generate(context.Background(), testAiTask(), store.ReportTriggerManual, "bob")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(st.insertedReports) != 1 {
		t.Fatalf("inserted reports = %d, want 1", len(st.insertedReports))
	}
	inserted := st.insertedReports[0]
	if inserted.Status != store.ReportStatusSucceeded || inserted.Trigger != store.ReportTriggerManual || inserted.CreatedBy != "bob" {
		t.Fatalf("inserted = %#v", inserted)
	}
	if !strings.Contains(inserted.Title, "每周进展周报") {
		t.Fatalf("title = %q", inserted.Title)
	}
	if inserted.Metrics == nil || inserted.Metrics.Messages != 2 || inserted.Metrics.Files != 1 {
		t.Fatalf("metrics = %#v", inserted.Metrics)
	}
	if inserted.Metrics.Tasks[store.TaskStatusTodo] != 1 || inserted.Metrics.Tasks[store.TaskStatusDoing] != 1 {
		t.Fatalf("task metrics = %#v", inserted.Metrics.Tasks)
	}
	if inserted.Metrics.Risks[store.RiskStatusOpen] != 1 {
		t.Fatalf("risk metrics = %#v", inserted.Metrics.Risks)
	}
	if len(inserted.RelatedTaskIDs) != 2 || len(inserted.RelatedRiskIDs) != 1 {
		t.Fatalf("related ids = %#v %#v", inserted.RelatedTaskIDs, inserted.RelatedRiskIDs)
	}
	if result.Report.Content != chat.reply {
		t.Fatalf("content = %q", result.Report.Content)
	}
	if len(st.posted) != 1 || !strings.Contains(st.posted[0], store.SenderAI) || !strings.Contains(st.posted[0], "登录模块开发完成") {
		t.Fatalf("posted = %#v", st.posted)
	}
	if len(st.marks) != 1 || st.marks[0].status != store.ReportStatusSucceeded || !st.marks[0].nextRun.After(st.marks[0].ranAt) {
		t.Fatalf("marks = %#v", st.marks)
	}

	if chat.calls != 1 || len(chat.inputs) != 2 {
		t.Fatalf("chat calls = %d inputs = %d", chat.calls, len(chat.inputs))
	}
	prompt := chat.inputs[1].Content
	for _, want := range []string{
		"登录模块开发完成",
		"- 【待办】补齐测试环境",
		"【高风险/待处理】测试环境就绪时间未确认",
		"- 周会纪要.md",
		"用户要求：重点写风险和下周计划",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %q", want, prompt)
		}
	}
}

func TestGenerateWithoutMaterialSkipsLLM(t *testing.T) {
	chat := &fakeChatModel{reply: "unused"}
	st := &fakeStore{}
	svc := NewService(Config{ChatModel: chat, Store: st})

	result, err := svc.Generate(context.Background(), testAiTask(), store.ReportTriggerSchedule, "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if chat.calls != 0 {
		t.Fatalf("chat calls = %d, want 0", chat.calls)
	}
	if len(st.insertedReports) != 1 || st.insertedReports[0].Content != noMaterialContent {
		t.Fatalf("inserted = %#v", st.insertedReports)
	}
	if result.Report.Status != store.ReportStatusSucceeded {
		t.Fatalf("status = %q", result.Report.Status)
	}
	if len(st.posted) != 1 {
		t.Fatalf("posted = %d, want 1", len(st.posted))
	}
}

func TestGenerateFailureRecordsFailedReport(t *testing.T) {
	chat := &fakeChatModel{err: errors.New("llm down")}
	st := testMaterialStore()
	svc := NewService(Config{ChatModel: chat, Store: st})

	result, err := svc.Generate(context.Background(), testAiTask(), store.ReportTriggerSchedule, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if len(st.insertedReports) != 1 {
		t.Fatalf("inserted reports = %d, want 1", len(st.insertedReports))
	}
	inserted := st.insertedReports[0]
	if inserted.Status != store.ReportStatusFailed || !strings.Contains(inserted.Error, "llm down") {
		t.Fatalf("inserted = %#v", inserted)
	}
	if result.Report.Status != store.ReportStatusFailed {
		t.Fatalf("result status = %q", result.Report.Status)
	}
	if len(st.posted) != 0 {
		t.Fatalf("posted = %#v, want none", st.posted)
	}
	if len(st.marks) != 1 || st.marks[0].status != store.ReportStatusFailed {
		t.Fatalf("marks = %#v", st.marks)
	}
}

func TestGenerateUsesLastRunAsPeriodStart(t *testing.T) {
	chat := &fakeChatModel{reply: "ok"}
	st := testMaterialStore()
	svc := NewService(Config{ChatModel: chat, Store: st})

	task := testAiTask()
	lastRun := time.Now().Add(-2 * time.Hour)
	task.LastRunAt = &lastRun

	if _, err := svc.Generate(context.Background(), task, store.ReportTriggerSchedule, ""); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !st.sinceMessages.Equal(lastRun) {
		t.Fatalf("messages since = %s, want %s", st.sinceMessages, lastRun)
	}
	if !st.sinceTasks.Equal(lastRun) || !st.sinceRisks.Equal(lastRun) || !st.sinceFiles.Equal(lastRun) {
		t.Fatalf("since mismatch: %s %s %s", st.sinceTasks, st.sinceRisks, st.sinceFiles)
	}
}

func TestGenerateHonorsSelectedSources(t *testing.T) {
	chat := &fakeChatModel{reply: "ok"}
	st := testMaterialStore()
	svc := NewService(Config{ChatModel: chat, Store: st})

	task := testAiTask()
	task.Sources = []string{store.AiTaskSourceFiles}

	if _, err := svc.Generate(context.Background(), task, store.ReportTriggerSchedule, ""); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if st.messagesCall || st.tasksCall || st.risksCall {
		t.Fatalf("unselected sources were queried: %v %v %v", st.messagesCall, st.tasksCall, st.risksCall)
	}
	if !st.filesCall {
		t.Fatal("files source was not queried")
	}
	metrics := st.insertedReports[0].Metrics
	if metrics.Messages != 0 || len(metrics.Tasks) != 0 || len(metrics.Risks) != 0 || metrics.Files != 1 {
		t.Fatalf("metrics = %#v", metrics)
	}
}

func TestGenerateReportsInsertError(t *testing.T) {
	chat := &fakeChatModel{reply: "ok"}
	st := testMaterialStore()
	st.insertErr = errors.New("db down")
	svc := NewService(Config{ChatModel: chat, Store: st})

	result, err := svc.Generate(context.Background(), testAiTask(), store.ReportTriggerSchedule, "")
	if err == nil || !strings.Contains(err.Error(), "insert report") {
		t.Fatalf("err = %v", err)
	}
	if result.Report.ID != 0 {
		t.Fatalf("result report = %#v", result.Report)
	}
	if len(st.marks) != 1 || st.marks[0].status != store.ReportStatusSucceeded {
		t.Fatalf("marks = %#v", st.marks)
	}
}
