package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"

	"github.com/Errrori/workpilot/internal/rag"
	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/tasks"
	"github.com/Errrori/workpilot/internal/ws"
)

const (
	toolSearchDocs = "search_docs"
	toolListTasks  = "list_tasks"
	toolListFiles  = "list_files"
	toolRecent     = "get_recent_messages"
	toolExtract    = "extract_tasks"
	toolArchive    = "archive_text"

	citationSnippetRunes = 200
	maxArchiveNameRunes  = 80
	maxRecentMessages    = 50
	maxInstructionRunes  = 30
)

// step is one recorded tool call of an agent run.
type step struct {
	Tool   string `json:"tool"`
	Args   string `json:"args,omitempty"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// runState carries the material and side effects of one run.
type runState struct {
	groupID      string
	actor        string
	material     []store.Message
	citations    []store.Citation
	tasksCreated int
	archivedFile string
}

func toolInfos() []*schema.ToolInfo {
	return []*schema.ToolInfo{
		{
			Name: toolSearchDocs,
			Desc: "在群内已上传并索引的文档里做语义检索，返回带编号的原文片段。回答文档相关问题时使用。",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"query": {Type: schema.String, Desc: "检索问题", Required: true},
				"top_k": {Type: schema.Integer, Desc: "返回片段数，默认 6"},
			}),
		},
		{
			Name: toolListTasks,
			Desc: "查看群内任务看板，用于了解现状或判断是否已存在同名任务。",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"status": {Type: schema.String, Desc: "按状态过滤，可选", Enum: []string{
					store.TaskStatusSuggested, store.TaskStatusTodo, store.TaskStatusDoing, store.TaskStatusDone, store.TaskStatusRejected,
				}},
			}),
		},
		{
			Name: toolListFiles,
			Desc: "列出群内已上传的文件（编号、文件名、解析/索引状态），用于定位用户所说的某个文件。",
		},
		{
			Name: toolRecent,
			Desc: "读取最近的群消息作为素材，用于理解“上面的讨论/之前的结论”等指代。",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"limit": {Type: schema.Integer, Desc: "读取条数，默认配置值（约 20，最大 50）"},
			}),
		},
		{
			Name: toolExtract,
			Desc: "把群消息素材或某个已上传文件抽取成任务建议并写入看板（状态为 suggested，自动去重）。用户要求把讨论、结论或纪要记成任务时调用；指定文件时先 list_files 找到 file_id 再传入。",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"file_id": {Type: schema.Integer, Desc: "指定从某个文件抽取时传其编号，否则省略（用当前群消息素材）"},
			}),
		},
		{
			Name: toolArchive,
			Desc: "把内容存成群文件并加入知识库，之后可以被检索到。仅当用户明确要求存档、新建文档、上传或希望以后能搜到时调用。content 必须只放要写入文件的正文，不要包含用户的指令或 @WorkPilot 字样。",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"title":   {Type: schema.String, Desc: "文件名（不含扩展名），可选"},
				"content": {Type: schema.String, Desc: "要写入文件的正文；省略时才回退用当前群消息素材"},
			}),
		},
	}
}

func (s *Service) runTool(ctx context.Context, state *runState, name, rawArgs string) (string, error) {
	switch name {
	case toolSearchDocs:
		var args struct {
			Query string `json:"query"`
			TopK  int    `json:"top_k"`
		}
		if err := decodeArgs(rawArgs, &args); err != nil {
			return "", err
		}
		query := strings.TrimSpace(args.Query)
		if query == "" {
			return "", errors.New("query is required")
		}
		return s.searchDocs(ctx, state, query, args.TopK)
	case toolListTasks:
		var args struct {
			Status string `json:"status"`
		}
		if err := decodeArgs(rawArgs, &args); err != nil {
			return "", err
		}
		return s.listTasks(ctx, state.groupID, strings.TrimSpace(args.Status))
	case toolListFiles:
		return s.listFiles(ctx, state.groupID)
	case toolRecent:
		var args struct {
			Limit int `json:"limit"`
		}
		if err := decodeArgs(rawArgs, &args); err != nil {
			return "", err
		}
		return s.loadRecentMessages(ctx, state, args.Limit)
	case toolExtract:
		var args struct {
			FileID int64 `json:"file_id"`
		}
		if err := decodeArgs(rawArgs, &args); err != nil {
			return "", err
		}
		return s.extractTasks(ctx, state, args.FileID)
	case toolArchive:
		var args struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		}
		if err := decodeArgs(rawArgs, &args); err != nil {
			return "", err
		}
		return s.archive(ctx, state, strings.TrimSpace(args.Title), args.Content)
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func decodeArgs(raw string, dest any) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), dest); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	return nil
}

func (s *Service) searchDocs(ctx context.Context, state *runState, query string, topK int) (string, error) {
	if s.cfg.Retriever == nil {
		return "", errors.New("retriever is not configured")
	}
	opts := make([]retriever.Option, 0, 2)
	if topK > 0 {
		opts = append(opts, retriever.WithTopK(topK))
	}
	if s.cfg.MinScore > 0 {
		opts = append(opts, retriever.WithScoreThreshold(s.cfg.MinScore))
	}
	docs, err := s.cfg.Retriever.Retrieve(rag.ContextWithGroup(ctx, state.groupID), query, opts...)
	if err != nil {
		return "", fmt.Errorf("retrieve: %w", err)
	}
	if len(docs) == 0 {
		return "（检索无结果）", nil
	}

	var b strings.Builder
	for i, doc := range docs {
		fileID, _ := doc.MetaData[rag.MetaFileID].(int64)
		fileName, _ := doc.MetaData[rag.MetaFileName].(string)
		chunkIndex, _ := doc.MetaData[rag.MetaChunkIndex].(int)
		fmt.Fprintf(&b, "【资料 %d】文件：%s（第 %d 块）\n%s\n\n", i+1, fileName, chunkIndex, doc.Content)
		state.citations = append(state.citations, store.Citation{
			Index:      len(state.citations) + 1,
			FileID:     fileID,
			FileName:   fileName,
			ChunkIndex: chunkIndex,
			Snippet:    truncateRunes(doc.Content, citationSnippetRunes),
			Score:      doc.Score(),
		})
	}
	return strings.TrimSpace(b.String()), nil
}

func (s *Service) listTasks(ctx context.Context, groupID, status string) (string, error) {
	if status != "" && !store.IsValidTaskStatus(status) {
		return "", fmt.Errorf("invalid status %q", status)
	}
	list, err := s.cfg.Store.ListTasks(ctx, groupID, status, 100)
	if err != nil {
		return "", fmt.Errorf("list tasks: %w", err)
	}
	if len(list) == 0 {
		return "当前没有符合条件的任务。", nil
	}
	var b strings.Builder
	for _, t := range list {
		fmt.Fprintf(&b, "#%d [%s] %s", t.ID, t.Status, t.Title)
		if t.Assignee != "" {
			fmt.Fprintf(&b, "（负责人：%s）", t.Assignee)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String()), nil
}

func (s *Service) listFiles(ctx context.Context, groupID string) (string, error) {
	files, err := s.cfg.Store.ListFiles(ctx, groupID)
	if err != nil {
		return "", fmt.Errorf("list files: %w", err)
	}
	if len(files) == 0 {
		return "群内还没有文件。", nil
	}
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "#%d %s（解析：%s，索引：%s）\n", f.ID, f.FileName, f.ParseStatus, f.IndexStatus)
	}
	return strings.TrimSpace(b.String()), nil
}

// loadRecentMessages replaces the run material with recent group messages so
// later extract_tasks/archive_text calls operate on the intended discussion.
func (s *Service) loadRecentMessages(ctx context.Context, state *runState, limit int) (string, error) {
	if limit <= 0 {
		limit = s.cfg.ContextMessages
	}
	if limit <= 0 || limit > maxRecentMessages {
		limit = maxRecentMessages
	}
	recent, err := s.cfg.Store.ListMessages(ctx, state.groupID, limit)
	if err != nil {
		return "", fmt.Errorf("list recent messages: %w", err)
	}
	material := make([]store.Message, 0, len(recent))
	for _, m := range recent {
		if m.SenderName == store.SenderAI {
			continue
		}
		material = append(material, m)
	}
	if len(material) == 0 {
		return "最近没有可用的群消息。", nil
	}
	state.material = material

	var b strings.Builder
	for i, m := range material {
		fmt.Fprintf(&b, "【消息 %d】%s（消息 #%d）\n%s\n\n", i+1, m.SenderName, m.ID, m.Content)
	}
	return strings.TrimSpace(b.String()), nil
}

func (s *Service) extractTasks(ctx context.Context, state *runState, fileID int64) (string, error) {
	var created []store.Task
	var err error
	if fileID > 0 {
		created, err = s.cfg.Extractor.Extract(ctx, state.groupID, state.actor, fileID)
	} else {
		created, err = s.cfg.Extractor.ExtractFromMessages(ctx, state.groupID, state.actor, state.material, 0)
	}
	if errors.Is(err, tasks.ErrNoMaterial) {
		return "没有可抽取的素材。", nil
	}
	if err != nil {
		return "", err
	}
	if len(created) == 0 {
		return "没有抽取到新的任务（可能都已存在）。", nil
	}
	for i := range created {
		if s.cfg.Notifier != nil {
			s.cfg.Notifier.BroadcastTask(ws.EventTaskSuggested, &created[i])
		}
	}
	state.tasksCreated += len(created)
	var b strings.Builder
	fmt.Fprintf(&b, "已创建 %d 条任务建议（待看板确认）：\n", len(created))
	for _, t := range created {
		fmt.Fprintf(&b, "#%d %s", t.ID, t.Title)
		if t.Assignee != "" {
			fmt.Fprintf(&b, "（负责人：%s）", t.Assignee)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String()), nil
}

func (s *Service) archive(ctx context.Context, state *runState, title, body string) (string, error) {
	if s.cfg.FileSink == nil {
		return "", errors.New("file archiving is not configured")
	}
	// Prefer the body the model selected; only fall back to the material.
	content := strings.TrimSpace(body)
	if content != "" {
		content = strings.TrimSpace(StripMention(content))
	} else {
		content = renderArchive(state.material)
	}
	if content == "" {
		return "没有可存档的内容。", nil
	}
	if title == "" {
		title = "群聊存档-" + time.Now().Format("20060102-150405")
	}
	name := sanitizeArchiveName(title) + ".md"
	file, err := s.cfg.FileSink.Save(ctx, state.groupID, state.actor, name, content)
	if err != nil {
		return "", fmt.Errorf("archive text: %w", err)
	}
	state.archivedFile = file.FileName
	if s.cfg.Notifier != nil {
		s.cfg.Notifier.BroadcastFile(ws.EventFileUploaded, &file)
	}
	return fmt.Sprintf("已存档为文件 %s（#%d），稍后会解析并加入知识库。", file.FileName, file.ID), nil
}

func renderArchive(material []store.Message) string {
	single := len(material) == 1
	parts := make([]string, 0, len(material))
	for _, m := range material {
		content := cleanArchiveContent(m.Content, single)
		if content == "" {
			continue
		}
		if !single {
			content = strings.TrimSpace(m.SenderName) + "：\n" + content
		}
		parts = append(parts, content)
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// cleanArchiveContent drops the wake word and, when the archived message was an
// instruction to the agent, a short leading instruction clause such as
// "帮我把这个存入知识库，然后抽取任务：" so the file holds only the content.
func cleanArchiveContent(content string, single bool) string {
	instructed := IsMention(content)
	content = StripMention(content)
	if single && instructed {
		content = stripLeadingInstruction(content)
	}
	return strings.TrimSpace(content)
}

var archiveInstructionKeywords = []string{"存档", "存入", "存进", "知识库", "归档", "入库", "抽取", "整理", "记录", "新建", "创建", "生成"}

func stripLeadingInstruction(content string) string {
	idx := strings.IndexAny(content, ":：")
	if idx < 0 {
		return content
	}
	prefix := content[:idx]
	if len([]rune(prefix)) > maxInstructionRunes {
		return content
	}
	keyword := false
	for _, kw := range archiveInstructionKeywords {
		if strings.Contains(prefix, kw) {
			keyword = true
			break
		}
	}
	if !keyword {
		return content
	}
	rest := strings.TrimPrefix(content[idx:], ":")
	rest = strings.TrimPrefix(rest, "：")
	return strings.TrimSpace(rest)
}

func sanitizeArchiveName(title string) string {
	title = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			return '_'
		}
		return r
	}, title)
	title = strings.TrimSpace(title)
	if title == "" {
		return "群聊存档"
	}
	return truncateRunes(title, maxArchiveNameRunes)
}
