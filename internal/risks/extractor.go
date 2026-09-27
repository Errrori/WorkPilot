package risks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/Errrori/workpilot/internal/store"
)

const (
	DefaultMaxRisks   = 10
	DefaultCharBudget = 12000
	DefaultTimeout    = 120 * time.Second

	taskFetchLimit  = 200
	chunkFetchLimit = 500

	maxSourcesPerRisk   = 5
	maxTaskRefsPerRisk  = 5
	maxTitleRunes       = 200
	maxDescriptionRunes = 2000
	maxOwnerRunes       = 64
	snippetRunes        = 200
)

// ErrNoMaterial means the group has neither unfinished tasks nor indexed chunks.
var ErrNoMaterial = errors.New("no tasks or indexed material to identify risks from")

// ChatModel is the non-streaming subset of the Eino chat model.
type ChatModel interface {
	Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error)
}

// Store reads risk material and persists suggested risks.
type Store interface {
	ListUnfinishedTasks(ctx context.Context, groupID string, limit int) ([]store.Task, error)
	ListExtractionChunks(ctx context.Context, groupID string, fileID int64, limit int) ([]store.RetrievedChunk, error)
	ExistingRiskTitles(ctx context.Context, groupID string) ([]string, error)
	InsertRisk(ctx context.Context, in store.RiskInsert) (store.Risk, error)
}

type Config struct {
	ChatModel  ChatModel
	Store      Store
	MaxRisks   int
	CharBudget int
	Timeout    time.Duration
}

type Service struct {
	cfg Config
}

func NewService(cfg Config) *Service {
	if cfg.MaxRisks <= 0 {
		cfg.MaxRisks = DefaultMaxRisks
	}
	if cfg.CharBudget <= 0 {
		cfg.CharBudget = DefaultCharBudget
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	return &Service{cfg: cfg}
}

// Extract asks the LLM for project risks grounded in the group's unfinished
// tasks and indexed material, and stores the new ones as suggested risks.
// fileID > 0 limits the document material to a single file.
func (s *Service) Extract(ctx context.Context, groupID, user string, fileID int64) ([]store.Risk, error) {
	if s.cfg.ChatModel == nil || s.cfg.Store == nil {
		return nil, errors.New("risk service is not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	tasks, err := s.cfg.Store.ListUnfinishedTasks(ctx, groupID, taskFetchLimit)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	chunks, err := s.cfg.Store.ListExtractionChunks(ctx, groupID, fileID, chunkFetchLimit)
	if err != nil {
		return nil, fmt.Errorf("list chunks: %w", err)
	}
	tasks, chunks = buildMaterial(tasks, chunks, s.cfg.CharBudget)
	if len(tasks) == 0 && len(chunks) == 0 {
		return nil, ErrNoMaterial
	}

	out, err := s.cfg.ChatModel.Generate(ctx, buildMessages(tasks, chunks))
	if err != nil {
		return nil, fmt.Errorf("llm generate: %w", err)
	}
	if out == nil || strings.TrimSpace(out.Content) == "" {
		return nil, errors.New("llm returned an empty response")
	}

	suggestions, err := parseRisks(out.Content, tasks, chunks, s.cfg.MaxRisks)
	if err != nil {
		return nil, fmt.Errorf("parse llm response: %w", err)
	}
	if len(suggestions) == 0 {
		return []store.Risk{}, nil
	}

	seen, err := s.existingTitles(ctx, groupID)
	if err != nil {
		return nil, fmt.Errorf("load existing risks: %w", err)
	}

	created := make([]store.Risk, 0, len(suggestions))
	for _, sg := range suggestions {
		key := normalizeTitle(sg.Title)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		risk, err := s.cfg.Store.InsertRisk(ctx, store.RiskInsert{
			GroupID:        groupID,
			Title:          sg.Title,
			Description:    sg.Description,
			Severity:       sg.Severity,
			Status:         store.RiskStatusSuggested,
			Owner:          sg.Owner,
			Source:         store.RiskSourceExtracted,
			CreatedBy:      user,
			Citations:      sg.Citations,
			RelatedTaskIDs: sg.RelatedTaskIDs,
		})
		if err != nil {
			return created, fmt.Errorf("insert risk: %w", err)
		}
		created = append(created, risk)
	}
	return created, nil
}

func (s *Service) existingTitles(ctx context.Context, groupID string) (map[string]struct{}, error) {
	titles, err := s.cfg.Store.ExistingRiskTitles(ctx, groupID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(titles))
	for _, title := range titles {
		seen[normalizeTitle(title)] = struct{}{}
	}
	return seen, nil
}

const systemPrompt = `你是 WorkPilot，一个面向小型研发团队的项目进度助手。
请只依据下面提供的「任务看板」和「群内资料」识别项目风险（如进度延期、阻塞、依赖未就绪、需求不明确、资源不足、范围蔓延等），不要编造资料中不存在的信息。
没有明显风险时输出 []。
只输出严格的 JSON 数组，不要输出任何解释、前后缀或 Markdown 代码块。
数组元素格式：
{"title": "风险摘要（简短明确）", "description": "风险原因与影响，没有则为空字符串", "severity": "low|medium|high", "owner": "跟进人，资料中没有则为空字符串", "task_ids": [相关任务编号], "sources": [相关资料编号]}
task_ids 是相关任务的编号（对应【任务 n】），sources 是支撑该风险的资料编号（对应【资料 n】），没有依据时输出空数组。`

var taskStatusLabels = map[string]string{
	store.TaskStatusSuggested: "待确认",
	store.TaskStatusTodo:      "待办",
	store.TaskStatusDoing:     "进行中",
}

var taskPriorityLabels = map[string]string{
	store.TaskPriorityLow:    "低",
	store.TaskPriorityMedium: "中",
	store.TaskPriorityHigh:   "高",
}

func buildMessages(tasks []store.Task, chunks []store.RetrievedChunk) []*schema.Message {
	var b strings.Builder
	if len(tasks) > 0 {
		b.WriteString("任务看板（未完成）：\n")
		for i, t := range tasks {
			status := taskStatusLabels[t.Status]
			if status == "" {
				status = t.Status
			}
			priority := taskPriorityLabels[t.Priority]
			if priority == "" {
				priority = t.Priority
			}
			assignee := t.Assignee
			if assignee == "" {
				assignee = "未指派"
			}
			fmt.Fprintf(&b, "【任务 %d】%s（%s，负责人 %s，%s优先级，更新于 %s）\n",
				i+1, t.Title, status, assignee, priority, t.UpdatedAt.Format("2006-01-02 15:04"))
		}
		b.WriteString("\n")
	}
	if len(chunks) > 0 {
		b.WriteString("群内资料：\n")
		for i, ch := range chunks {
			fmt.Fprintf(&b, "【资料 %d】文件：%s（第 %d 块）\n%s\n\n", i+1, ch.FileName, ch.ChunkIndex, ch.Content)
		}
	}
	return []*schema.Message{
		schema.SystemMessage(systemPrompt),
		schema.UserMessage(strings.TrimSpace(b.String())),
	}
}

// buildMaterial trims tasks and chunks to the character budget. Tasks get at
// most half of it so the document material is always represented when present.
func buildMaterial(tasks []store.Task, chunks []store.RetrievedChunk, budget int) ([]store.Task, []store.RetrievedChunk) {
	if budget <= 0 {
		budget = DefaultCharBudget
	}
	taskBudget := budget / 2

	var keptTasks []store.Task
	taskChars := 0
	for _, t := range tasks {
		if len(keptTasks) > 0 && taskChars >= taskBudget {
			break
		}
		keptTasks = append(keptTasks, t)
		taskChars += utf8.RuneCountInString(t.Title) + utf8.RuneCountInString(t.Assignee) + 40
	}

	chunkBudget := budget - taskChars
	var keptChunks []store.RetrievedChunk
	chunkChars := 0
	for _, ch := range chunks {
		if len(keptChunks) > 0 && chunkChars >= chunkBudget {
			break
		}
		keptChunks = append(keptChunks, ch)
		chunkChars += utf8.RuneCountInString(ch.Content)
	}
	if keptTasks == nil {
		keptTasks = []store.Task{}
	}
	if keptChunks == nil {
		keptChunks = []store.RetrievedChunk{}
	}
	return keptTasks, keptChunks
}

type suggestion struct {
	Title          string
	Description    string
	Severity       string
	Owner          string
	Citations      []store.Citation
	RelatedTaskIDs []int64
}

type rawRisk struct {
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Severity    string     `json:"severity"`
	Owner       string     `json:"owner"`
	TaskIDs     numberList `json:"task_ids"`
	Sources     numberList `json:"sources"`
}

// numberList accepts references as numbers or numeric strings, since models
// sometimes quote them.
type numberList []int

func (s *numberList) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	for _, item := range raw {
		var n int
		if err := json.Unmarshal(item, &n); err == nil {
			*s = append(*s, n)
			continue
		}
		var str string
		if err := json.Unmarshal(item, &str); err != nil {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(str)); err == nil {
			*s = append(*s, n)
		}
	}
	return nil
}

func parseRisks(content string, tasks []store.Task, chunks []store.RetrievedChunk, max int) ([]suggestion, error) {
	raw, err := extractJSONArray(content)
	if err != nil {
		return nil, err
	}
	var items []rawRisk
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("decode json array: %w", err)
	}

	if max <= 0 {
		max = DefaultMaxRisks
	}
	suggestions := make([]suggestion, 0, len(items))
	for _, item := range items {
		title := truncateRunes(strings.TrimSpace(item.Title), maxTitleRunes)
		if title == "" {
			continue
		}
		suggestions = append(suggestions, suggestion{
			Title:          title,
			Description:    truncateRunes(strings.TrimSpace(item.Description), maxDescriptionRunes),
			Severity:       normalizeSeverity(item.Severity),
			Owner:          truncateRunes(strings.TrimSpace(item.Owner), maxOwnerRunes),
			Citations:      buildCitations(item.Sources, chunks),
			RelatedTaskIDs: buildTaskRefs(item.TaskIDs, tasks),
		})
		if len(suggestions) >= max {
			break
		}
	}
	return suggestions, nil
}

func extractJSONArray(content string) (string, error) {
	s := strings.TrimSpace(content)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	start := strings.IndexByte(s, '[')
	end := strings.LastIndexByte(s, ']')
	if start < 0 || end < start {
		return "", errors.New("response does not contain a json array")
	}
	return s[start : end+1], nil
}

func buildCitations(sources []int, material []store.RetrievedChunk) []store.Citation {
	var citations []store.Citation
	seen := make(map[int]struct{}, len(sources))
	for _, n := range sources {
		if len(citations) >= maxSourcesPerRisk {
			break
		}
		if n < 1 || n > len(material) {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		ch := material[n-1]
		citations = append(citations, store.Citation{
			Index:      len(citations) + 1,
			FileID:     ch.FileID,
			FileName:   ch.FileName,
			ChunkIndex: ch.ChunkIndex,
			Snippet:    truncateRunes(ch.Content, snippetRunes),
		})
	}
	return citations
}

func buildTaskRefs(refs []int, tasks []store.Task) []int64 {
	var ids []int64
	seen := make(map[int]struct{}, len(refs))
	for _, n := range refs {
		if len(ids) >= maxTaskRefsPerRisk {
			break
		}
		if n < 1 || n > len(tasks) {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		ids = append(ids, tasks[n-1].ID)
	}
	return ids
}

func normalizeSeverity(severity string) string {
	s := strings.ToLower(strings.TrimSpace(severity))
	if store.IsValidRiskSeverity(s) {
		return s
	}
	return store.RiskSeverityMedium
}

func normalizeTitle(title string) string {
	return strings.ToLower(strings.Join(strings.Fields(title), " "))
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
