package tasks

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

	"github.com/Errrori/workpilot/internal/llmtrack"
	"github.com/Errrori/workpilot/internal/store"
)

const (
	DefaultMaxSuggestions = 20
	DefaultCharBudget     = 12000
	DefaultTimeout        = 120 * time.Second

	chunkFetchLimit     = 500
	maxSourcesPerTask   = 5
	maxTitleRunes       = 200
	maxDescriptionRunes = 2000
	maxAssigneeRunes    = 64
	snippetRunes        = 200
	maxMessageRunes     = 6000
)

// ErrNoMaterial means the group has no indexed chunks to extract from.
var ErrNoMaterial = errors.New("no indexed material to extract tasks from")

// ChatModel is the non-streaming subset of the Eino chat model.
type ChatModel interface {
	Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error)
}

// Store reads extraction material and persists suggested tasks.
type Store interface {
	ListExtractionChunks(ctx context.Context, groupID string, fileID int64, limit int) ([]store.RetrievedChunk, error)
	ExistingTaskTitles(ctx context.Context, groupID string) ([]string, error)
	InsertTask(ctx context.Context, in store.TaskInsert) (store.Task, error)
}

type Config struct {
	ChatModel      ChatModel
	Store          Store
	MaxSuggestions int
	CharBudget     int
	Timeout        time.Duration
}

type Service struct {
	cfg Config
}

func NewService(cfg Config) *Service {
	if cfg.MaxSuggestions <= 0 {
		cfg.MaxSuggestions = DefaultMaxSuggestions
	}
	if cfg.CharBudget <= 0 {
		cfg.CharBudget = DefaultCharBudget
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	return &Service{cfg: cfg}
}

// Extract asks the LLM for actionable tasks grounded in the group's indexed
// material and stores the new ones as suggested tasks. fileID > 0 limits the
// material to a single file.
func (s *Service) Extract(ctx context.Context, groupID, user string, fileID int64) ([]store.Task, error) {
	if s.cfg.ChatModel == nil || s.cfg.Store == nil {
		return nil, errors.New("task service is not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	chunks, err := s.cfg.Store.ListExtractionChunks(ctx, groupID, fileID, chunkFetchLimit)
	if err != nil {
		return nil, fmt.Errorf("list chunks: %w", err)
	}
	return s.ExtractFromChunks(ctx, groupID, user, chunks, 0)
}

// ExtractFromChunks extracts tasks from explicit material; workflow nodes pass
// the chunks produced by their upstream retrieval step. max > 0 overrides the
// service suggestion limit.
func (s *Service) ExtractFromChunks(ctx context.Context, groupID, user string, chunks []store.RetrievedChunk, max int) ([]store.Task, error) {
	if s.cfg.ChatModel == nil || s.cfg.Store == nil {
		return nil, errors.New("task service is not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	material := buildMaterial(chunks, s.cfg.CharBudget)
	if len(material) == 0 {
		return nil, ErrNoMaterial
	}

	ctx = llmtrack.WithCall(ctx, llmtrack.SourceTaskExtract, groupID)
	out, err := s.cfg.ChatModel.Generate(ctx, buildMessages(material))
	if err != nil {
		return nil, fmt.Errorf("llm generate: %w", err)
	}
	if out == nil || strings.TrimSpace(out.Content) == "" {
		return nil, errors.New("llm returned an empty response")
	}

	suggestions, err := parseSuggestions(out.Content, material, s.resolveMax(max))
	if err != nil {
		return nil, fmt.Errorf("parse llm response: %w", err)
	}
	if len(suggestions) == 0 {
		return []store.Task{}, nil
	}

	seen, err := s.existingTitles(ctx, groupID)
	if err != nil {
		return nil, fmt.Errorf("load existing tasks: %w", err)
	}

	created := make([]store.Task, 0, len(suggestions))
	for _, sg := range suggestions {
		key := normalizeTitle(sg.Title)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		task, err := s.cfg.Store.InsertTask(ctx, store.TaskInsert{
			GroupID:     groupID,
			Title:       sg.Title,
			Description: sg.Description,
			Assignee:    sg.Assignee,
			Status:      store.TaskStatusSuggested,
			Priority:    sg.Priority,
			Source:      store.TaskSourceExtracted,
			Origin:      store.TaskOriginDocument,
			CreatedBy:   user,
			Citations:   sg.Citations,
		})
		if err != nil {
			return created, fmt.Errorf("insert task: %w", err)
		}
		created = append(created, task)
	}
	return created, nil
}

// ExtractFromMessages extracts tasks from chat messages and records which
// messages each task came from as source_message_ids.
func (s *Service) ExtractFromMessages(ctx context.Context, groupID, user string, msgs []store.Message, max int) ([]store.Task, error) {
	if s.cfg.ChatModel == nil || s.cfg.Store == nil {
		return nil, errors.New("task service is not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	material := buildMessageMaterial(msgs, s.cfg.CharBudget)
	if len(material) == 0 {
		return nil, ErrNoMaterial
	}

	ctx = llmtrack.WithCall(ctx, llmtrack.SourceTaskExtract, groupID)
	out, err := s.cfg.ChatModel.Generate(ctx, buildMessageMessages(material))
	if err != nil {
		return nil, fmt.Errorf("llm generate: %w", err)
	}
	if out == nil || strings.TrimSpace(out.Content) == "" {
		return nil, errors.New("llm returned an empty response")
	}

	suggestions, err := parseMessageSuggestions(out.Content, material, s.resolveMax(max))
	if err != nil {
		return nil, fmt.Errorf("parse llm response: %w", err)
	}
	if len(suggestions) == 0 {
		return []store.Task{}, nil
	}

	seen, err := s.existingTitles(ctx, groupID)
	if err != nil {
		return nil, fmt.Errorf("load existing tasks: %w", err)
	}

	created := make([]store.Task, 0, len(suggestions))
	for _, sg := range suggestions {
		key := normalizeTitle(sg.Title)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		task, err := s.cfg.Store.InsertTask(ctx, store.TaskInsert{
			GroupID:          groupID,
			Title:            sg.Title,
			Description:      sg.Description,
			Assignee:         sg.Assignee,
			Status:           store.TaskStatusSuggested,
			Priority:         sg.Priority,
			Source:           store.TaskSourceExtracted,
			Origin:           store.TaskOriginChat,
			CreatedBy:        user,
			SourceMessageIDs: sg.SourceMessageIDs,
		})
		if err != nil {
			return created, fmt.Errorf("insert task: %w", err)
		}
		created = append(created, task)
	}
	return created, nil
}

func (s *Service) existingTitles(ctx context.Context, groupID string) (map[string]struct{}, error) {
	titles, err := s.cfg.Store.ExistingTaskTitles(ctx, groupID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(titles))
	for _, title := range titles {
		seen[normalizeTitle(title)] = struct{}{}
	}
	return seen, nil
}

func (s *Service) resolveMax(max int) int {
	if max > 0 {
		return max
	}
	return s.cfg.MaxSuggestions
}

const systemPrompt = `你是 WorkPilot，一个面向小型研发团队的项目进度助手。
请只依据下面提供的“群内素材”抽取仍未完成、需要人跟进的待办任务，不要编造素材中不存在的信息。
已完成或纯信息性的内容不要输出。
只输出严格的 JSON 数组，不要输出任何解释、前后缀或 Markdown 代码块。
数组元素格式：
{"title": "任务标题（简短明确）", "description": "任务说明，没有则为空字符串", "assignee": "负责人，素材中没有则为空字符串", "priority": "low|medium|high", "sources": [素材编号]}
sources 是支撑该任务的素材编号（对应【资料 n】或【消息 n】）；每条任务都必须从素材中选出最相关的编号，确实没有依据才输出空数组。
没有可抽取的任务时输出 []。`

func buildMessages(material []store.RetrievedChunk) []*schema.Message {
	var context strings.Builder
	for i, ch := range material {
		fmt.Fprintf(&context, "【资料 %d】文件：%s（第 %d 块）\n%s\n\n", i+1, ch.FileName, ch.ChunkIndex, ch.Content)
	}
	return []*schema.Message{
		schema.SystemMessage(systemPrompt),
		schema.UserMessage("群内资料：\n" + context.String()),
	}
}

func buildMessageMessages(material []store.Message) []*schema.Message {
	var context strings.Builder
	for i, m := range material {
		fmt.Fprintf(&context, "【消息 %d】%s（消息 #%d）\n%s\n\n", i+1, m.SenderName, m.ID, m.Content)
	}
	return []*schema.Message{
		schema.SystemMessage(systemPrompt),
		schema.UserMessage("群内消息：\n" + context.String()),
	}
}

// buildMessageMaterial trims messages to the character budget, oldest first.
func buildMessageMaterial(msgs []store.Message, budget int) []store.Message {
	if budget <= 0 {
		budget = DefaultCharBudget
	}
	var material []store.Message
	total := 0
	for _, m := range msgs {
		content := truncateRunes(strings.TrimSpace(m.Content), maxMessageRunes)
		if content == "" {
			continue
		}
		if len(material) > 0 && total >= budget {
			break
		}
		material = append(material, store.Message{ID: m.ID, SenderName: m.SenderName, Content: content, ReplyTo: m.ReplyTo, CreatedAt: m.CreatedAt})
		total += utf8.RuneCountInString(content)
	}
	return material
}

// buildMaterial trims chunks to the character budget, newest file first.
func buildMaterial(chunks []store.RetrievedChunk, budget int) []store.RetrievedChunk {
	if budget <= 0 {
		budget = DefaultCharBudget
	}
	var material []store.RetrievedChunk
	total := 0
	for _, ch := range chunks {
		if len(material) > 0 && total >= budget {
			break
		}
		material = append(material, ch)
		total += utf8.RuneCountInString(ch.Content)
	}
	return material
}

type suggestion struct {
	Title            string
	Description      string
	Assignee         string
	Priority         string
	Citations        []store.Citation
	SourceMessageIDs []int64
}

type rawSuggestion struct {
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Assignee    string     `json:"assignee"`
	Priority    string     `json:"priority"`
	Sources     sourceList `json:"sources"`
}

// sourceList accepts source references as numbers or numeric strings, since
// models sometimes quote them.
type sourceList []int

func (s *sourceList) UnmarshalJSON(data []byte) error {
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

func parseSuggestions(content string, material []store.RetrievedChunk, max int) ([]suggestion, error) {
	return parseSuggestionsWith(content, max, func(sources []int) ([]store.Citation, []int64) {
		return buildCitations(sources, material), nil
	})
}

// parseMessageSuggestions maps each task's sources back to chat message ids.
func parseMessageSuggestions(content string, material []store.Message, max int) ([]suggestion, error) {
	return parseSuggestionsWith(content, max, func(sources []int) ([]store.Citation, []int64) {
		return nil, buildMessageIDs(sources, material)
	})
}

func parseSuggestionsWith(content string, max int, resolve func([]int) ([]store.Citation, []int64)) ([]suggestion, error) {
	raw, err := extractJSONArray(content)
	if err != nil {
		return nil, err
	}
	var items []rawSuggestion
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("decode json array: %w", err)
	}

	if max <= 0 {
		max = DefaultMaxSuggestions
	}
	suggestions := make([]suggestion, 0, len(items))
	for _, item := range items {
		title := truncateRunes(strings.TrimSpace(item.Title), maxTitleRunes)
		if title == "" {
			continue
		}
		citations, messageIDs := resolve(item.Sources)
		suggestions = append(suggestions, suggestion{
			Title:            title,
			Description:      truncateRunes(strings.TrimSpace(item.Description), maxDescriptionRunes),
			Assignee:         truncateRunes(strings.TrimSpace(item.Assignee), maxAssigneeRunes),
			Priority:         normalizePriority(item.Priority),
			Citations:        citations,
			SourceMessageIDs: messageIDs,
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
		if len(citations) >= maxSourcesPerTask {
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

// buildMessageIDs maps source positions to the referenced message ids.
func buildMessageIDs(sources []int, material []store.Message) []int64 {
	var ids []int64
	seen := make(map[int64]struct{}, len(sources))
	for _, n := range sources {
		if len(ids) >= maxSourcesPerTask {
			break
		}
		if n < 1 || n > len(material) {
			continue
		}
		id := material[n-1].ID
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

func normalizePriority(priority string) string {
	p := strings.ToLower(strings.TrimSpace(priority))
	if store.IsValidTaskPriority(p) {
		return p
	}
	return store.TaskPriorityMedium
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
