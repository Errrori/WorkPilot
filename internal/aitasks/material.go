package aitasks

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"

	"github.com/Errrori/workpilot/internal/store"
)

const (
	messageFetchLimit = 500
	taskFetchLimit    = 500
	riskFetchLimit    = 500
	fileFetchLimit    = 200

	budgetMessagesPct = 50
	budgetTasksPct    = 25
	budgetRisksPct    = 20
	budgetFilesPct    = 5

	maxMessageRunes = 500
	maxTaskRunes    = 400
	maxRiskRunes    = 400
	maxRelatedIDs   = 200
)

const defaultUserRequest = "汇总本周期项目进展、任务与风险，并给出下一步建议。"

const systemPrompt = `你是 WorkPilot，一个面向小型研发团队的项目进度助手。
请依据下面提供的「本周期群聊消息」「任务看板」「风险清单」「新增文件」和「用户要求」，撰写一份简洁的 Markdown 报告（通常是项目周报）。
要求：
- 只依据素材内容，不要编造；素材中没有的信息不要输出
- 开头一个一级标题，正文用 ## 小节，例如：本周期进展、任务看板、风险与阻塞、下一步建议（可按用户要求调整）
- 引用任务或风险时使用其标题原文，不要输出编号
- 突出变化、阻塞与待跟进事项，内容精炼
只输出 Markdown 正文，不要输出额外解释或代码块围栏。`

// material is the gathered report source material within the period.
type material struct {
	messages []store.Message
	tasks    []store.Task
	risks    []store.Risk
	files    []store.File
	metrics  store.ReportMetrics
}

func (m material) empty() bool {
	return len(m.messages) == 0 && len(m.tasks) == 0 && len(m.risks) == 0 && len(m.files) == 0
}

// gather loads the selected sources for [since, until). Period bounds are
// passed in the task's location; comparisons against the database are exact
// instants, so callers should pass the same instants used for the report.
func (s *Service) gather(ctx context.Context, task store.AiTask, since, until time.Time) (material, error) {
	var m material
	m.metrics = store.ReportMetrics{
		Tasks: map[string]int{},
		Risks: map[string]int{},
	}
	for _, source := range task.Sources {
		switch source {
		case store.AiTaskSourceMessages:
			messages, err := s.cfg.Store.ListMessagesSince(ctx, task.GroupID, since, until, store.SenderAI, messageFetchLimit)
			if err != nil {
				return m, fmt.Errorf("list messages: %w", err)
			}
			m.messages = messages
			m.metrics.Messages = len(messages)
		case store.AiTaskSourceTasks:
			changed, err := s.cfg.Store.ListTasksUpdatedSince(ctx, task.GroupID, since, taskFetchLimit)
			if err != nil {
				return m, fmt.Errorf("list changed tasks: %w", err)
			}
			unfinished, err := s.cfg.Store.ListUnfinishedTasks(ctx, task.GroupID, taskFetchLimit)
			if err != nil {
				return m, fmt.Errorf("list unfinished tasks: %w", err)
			}
			m.tasks = mergeTasks(changed, unfinished)
			for _, t := range m.tasks {
				m.metrics.Tasks[t.Status]++
			}
		case store.AiTaskSourceRisks:
			risks, err := s.cfg.Store.ListReportRisks(ctx, task.GroupID, since, riskFetchLimit)
			if err != nil {
				return m, fmt.Errorf("list risks: %w", err)
			}
			m.risks = risks
			for _, r := range m.risks {
				m.metrics.Risks[r.Status]++
			}
		case store.AiTaskSourceFiles:
			files, err := s.cfg.Store.ListFilesSince(ctx, task.GroupID, since, fileFetchLimit)
			if err != nil {
				return m, fmt.Errorf("list files: %w", err)
			}
			m.files = files
			m.metrics.Files = len(files)
		}
	}
	return m, nil
}

// mergeTasks merges window changes and the unfinished snapshot by task ID,
// oldest update first.
func mergeTasks(changed, unfinished []store.Task) []store.Task {
	byID := make(map[int64]store.Task, len(changed)+len(unfinished))
	for _, t := range changed {
		byID[t.ID] = t
	}
	for _, t := range unfinished {
		byID[t.ID] = t
	}
	merged := make([]store.Task, 0, len(byID))
	for _, t := range byID {
		merged = append(merged, t)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].UpdatedAt.Equal(merged[j].UpdatedAt) {
			return merged[i].ID < merged[j].ID
		}
		return merged[i].UpdatedAt.Before(merged[j].UpdatedAt)
	})
	return merged
}

func (m material) relatedTaskIDs() []int64 {
	ids := make([]int64, 0, len(m.tasks))
	for _, t := range m.tasks {
		if len(ids) >= maxRelatedIDs {
			break
		}
		ids = append(ids, t.ID)
	}
	return ids
}

func (m material) relatedRiskIDs() []int64 {
	ids := make([]int64, 0, len(m.risks))
	for _, r := range m.risks {
		if len(ids) >= maxRelatedIDs {
			break
		}
		ids = append(ids, r.ID)
	}
	return ids
}

// buildPromptMessages renders the material and the user request.
func buildPromptMessages(m material, userRequest string, budget int, loc *time.Location) []*schema.Message {
	if budget <= 0 {
		budget = DefaultCharBudget
	}
	var b strings.Builder
	writeMessages(&b, m.messages, budget*budgetMessagesPct/100, loc)
	writeTasks(&b, m.tasks, budget*budgetTasksPct/100, loc)
	writeRisks(&b, m.risks, budget*budgetRisksPct/100, loc)
	writeFiles(&b, m.files, budget*budgetFilesPct/100, loc)

	request := strings.TrimSpace(userRequest)
	if request == "" {
		request = defaultUserRequest
	}
	fmt.Fprintf(&b, "用户要求：%s", request)
	return []*schema.Message{
		schema.SystemMessage(systemPrompt),
		schema.UserMessage(strings.TrimSpace(b.String())),
	}
}

func writeMessages(b *strings.Builder, messages []store.Message, budget int, loc *time.Location) {
	if len(messages) == 0 {
		return
	}
	b.WriteString("本周期群聊消息（按时间顺序）：\n")
	used := 0
	for _, m := range messages {
		line := truncateRunes(fmt.Sprintf("[%s] %s：%s\n",
			m.CreatedAt.In(loc).Format("01-02 15:04"), m.SenderName, oneLine(m.Content)), maxMessageRunes)
		if used > 0 && used+utf8.RuneCountInString(line) > budget {
			break
		}
		b.WriteString(line)
		used += utf8.RuneCountInString(line)
	}
	b.WriteString("\n")
}

func writeTasks(b *strings.Builder, tasks []store.Task, budget int, loc *time.Location) {
	if len(tasks) == 0 {
		return
	}
	b.WriteString("任务看板（未完成 + 本周期有更新）：\n")
	used := 0
	for _, t := range tasks {
		assignee := t.Assignee
		if assignee == "" {
			assignee = "未指派"
		}
		line := truncateRunes(fmt.Sprintf("- 【%s】%s（负责人 %s，%s优先级，更新于 %s）\n",
			taskStatusLabel(t.Status), t.Title, assignee, priorityLabel(t.Priority),
			t.UpdatedAt.In(loc).Format("01-02 15:04")), maxTaskRunes)
		if used > 0 && used+utf8.RuneCountInString(line) > budget {
			break
		}
		b.WriteString(line)
		used += utf8.RuneCountInString(line)
	}
	b.WriteString("\n")
}

func writeRisks(b *strings.Builder, risks []store.Risk, budget int, loc *time.Location) {
	if len(risks) == 0 {
		return
	}
	b.WriteString("风险清单（处理中/本周期有更新）：\n")
	used := 0
	for _, r := range risks {
		owner := r.Owner
		if owner == "" {
			owner = "未指派"
		}
		line := truncateRunes(fmt.Sprintf("- 【%s风险/%s】%s（跟进人 %s，更新于 %s）\n",
			severityLabel(r.Severity), riskStatusLabel(r.Status), r.Title, owner,
			r.UpdatedAt.In(loc).Format("01-02 15:04")), maxRiskRunes)
		if used > 0 && used+utf8.RuneCountInString(line) > budget {
			break
		}
		b.WriteString(line)
		used += utf8.RuneCountInString(line)
	}
	b.WriteString("\n")
}

func writeFiles(b *strings.Builder, files []store.File, budget int, loc *time.Location) {
	if len(files) == 0 {
		return
	}
	b.WriteString("本周期新增文件：\n")
	used := 0
	for _, f := range files {
		line := fmt.Sprintf("- %s（上传者 %s，%s）\n", f.FileName, f.UploaderName, f.CreatedAt.In(loc).Format("01-02 15:04"))
		if used > 0 && used+utf8.RuneCountInString(line) > budget {
			break
		}
		b.WriteString(line)
		used += utf8.RuneCountInString(line)
	}
	b.WriteString("\n")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

func taskStatusLabel(status string) string {
	labels := map[string]string{
		store.TaskStatusSuggested: "待确认",
		store.TaskStatusTodo:      "待办",
		store.TaskStatusDoing:     "进行中",
		store.TaskStatusDone:      "已完成",
		store.TaskStatusRejected:  "已忽略",
	}
	if label, ok := labels[status]; ok {
		return label
	}
	return status
}

func priorityLabel(priority string) string {
	labels := map[string]string{
		store.TaskPriorityLow:    "低",
		store.TaskPriorityMedium: "中",
		store.TaskPriorityHigh:   "高",
	}
	if label, ok := labels[priority]; ok {
		return label
	}
	return priority
}

func riskStatusLabel(status string) string {
	labels := map[string]string{
		store.RiskStatusSuggested:  "待确认",
		store.RiskStatusOpen:       "待处理",
		store.RiskStatusMitigating: "处理中",
		store.RiskStatusResolved:   "已解决",
		store.RiskStatusDismissed:  "已忽略",
	}
	if label, ok := labels[status]; ok {
		return label
	}
	return status
}

func severityLabel(severity string) string {
	labels := map[string]string{
		store.RiskSeverityLow:    "低",
		store.RiskSeverityMedium: "中",
		store.RiskSeverityHigh:   "高",
	}
	if label, ok := labels[severity]; ok {
		return label
	}
	return severity
}
