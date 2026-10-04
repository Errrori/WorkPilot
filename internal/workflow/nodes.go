package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"

	"github.com/Errrori/workpilot/internal/aitasks"
	"github.com/Errrori/workpilot/internal/llmtrack"
	"github.com/Errrori/workpilot/internal/rag"
	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

const (
	extractionChunkLimit = 500
	citationSnippetRunes = 200
	noRetrievalText      = "（检索无结果）"
)

// Retriever matches the Eino retriever component.
type Retriever interface {
	Retrieve(ctx context.Context, query string, opts ...retriever.Option) ([]*schema.Document, error)
}

// ChatModel matches the non-streaming subset of the Eino chat model.
type ChatModel interface {
	Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error)
}

// Extractor extracts task suggestions from explicit material.
type Extractor interface {
	ExtractFromChunks(ctx context.Context, groupID, user string, chunks []store.RetrievedChunk, max int) ([]store.Task, error)
}

// MaterialStore loads fallback extraction material.
type MaterialStore interface {
	ListExtractionChunks(ctx context.Context, groupID string, fileID int64, limit int) ([]store.RetrievedChunk, error)
}

// ReportGenerator produces one report row.
type ReportGenerator interface {
	GenerateReport(ctx context.Context, req aitasks.ReportRequest) (aitasks.ReportResult, error)
}

// NodeNotifier broadcasts node side effects (suggested tasks, reports and chat
// announcements) to the group room.
type NodeNotifier interface {
	BroadcastTask(event string, t *store.Task)
	BroadcastReport(event string, r *store.Report)
	BroadcastMessage(m *store.Message)
}

// Deps wires concrete services into the built-in node handlers.
type Deps struct {
	Retriever Retriever
	ChatModel ChatModel
	Extractor Extractor
	Material  MaterialStore
	Reports   ReportGenerator
	Notifier  NodeNotifier
	MinScore  float64
}

// NewHandlers builds the node handler registry.
func NewHandlers(d Deps) map[string]Handler {
	return map[string]Handler{
		NodeTypeRetrieve:     &retrieveHandler{retriever: d.Retriever, minScore: d.MinScore},
		NodeTypeAsk:          &askHandler{model: d.ChatModel},
		NodeTypeExtractTasks: &extractTasksHandler{extractor: d.Extractor, material: d.Material, notifier: d.Notifier},
		NodeTypeReport:       &reportHandler{reports: d.Reports, notifier: d.Notifier},
	}
}

type retrieveHandler struct {
	retriever Retriever
	minScore  float64
}

func (h *retrieveHandler) Run(ctx context.Context, req NodeRequest) (NodeOutput, error) {
	if h.retriever == nil {
		return NodeOutput{}, errors.New("retriever is not configured")
	}
	cfg, err := req.Node.RetrieveConfig()
	if err != nil {
		return NodeOutput{}, err
	}
	query := strings.TrimSpace(RenderTemplate(cfg.Query, req.Vars))
	if query == "" {
		return NodeOutput{}, errors.New("retrieve query rendered empty")
	}

	opts := make([]retriever.Option, 0, 2)
	if cfg.TopK > 0 {
		opts = append(opts, retriever.WithTopK(cfg.TopK))
	}
	if h.minScore > 0 {
		opts = append(opts, retriever.WithScoreThreshold(h.minScore))
	}
	docs, err := h.retriever.Retrieve(rag.ContextWithGroup(ctx, req.Workflow.GroupID), query, opts...)
	if err != nil {
		return NodeOutput{}, fmt.Errorf("retrieve: %w", err)
	}
	if len(docs) == 0 {
		return NodeOutput{Text: noRetrievalText}, nil
	}

	out := NodeOutput{
		Material:  make([]store.RetrievedChunk, 0, len(docs)),
		Citations: make([]store.Citation, 0, len(docs)),
	}
	var b strings.Builder
	for i, doc := range docs {
		fileID, _ := doc.MetaData[rag.MetaFileID].(int64)
		fileName, _ := doc.MetaData[rag.MetaFileName].(string)
		chunkIndex, _ := doc.MetaData[rag.MetaChunkIndex].(int)
		fmt.Fprintf(&b, "【资料 %d】文件：%s（第 %d 块）\n%s\n\n", i+1, fileName, chunkIndex, doc.Content)
		out.Material = append(out.Material, store.RetrievedChunk{
			FileID:     fileID,
			FileName:   fileName,
			ChunkIndex: chunkIndex,
			Content:    doc.Content,
			Score:      doc.Score(),
		})
		out.Citations = append(out.Citations, store.Citation{
			Index:      i + 1,
			FileID:     fileID,
			FileName:   fileName,
			ChunkIndex: chunkIndex,
			Snippet:    truncateRunes(doc.Content, citationSnippetRunes),
			Score:      doc.Score(),
		})
	}
	out.Text = strings.TrimSpace(b.String())
	return out, nil
}

const askSystemPrompt = `你是 WorkPilot，一个面向小型研发团队的项目进度助手。
请只依据用户消息中的资料内容回答，不要编造资料中没有的信息；资料不足时明确说明“资料中未包含”。
只输出回答正文，不要输出额外解释。`

type askHandler struct {
	model ChatModel
}

func (h *askHandler) Run(ctx context.Context, req NodeRequest) (NodeOutput, error) {
	if h.model == nil {
		return NodeOutput{}, errors.New("chat model is not configured")
	}
	cfg, err := req.Node.AskConfig()
	if err != nil {
		return NodeOutput{}, err
	}
	prompt := strings.TrimSpace(RenderTemplate(cfg.Prompt, req.Vars))
	if prompt == "" {
		return NodeOutput{}, errors.New("ask prompt rendered empty")
	}

	ctx = llmtrack.WithCall(ctx, llmtrack.SourceWorkflow, req.Workflow.GroupID)
	out, err := h.model.Generate(ctx, []*schema.Message{
		schema.SystemMessage(askSystemPrompt),
		schema.UserMessage(prompt),
	})
	if err != nil {
		return NodeOutput{}, fmt.Errorf("llm generate: %w", err)
	}
	if out == nil || strings.TrimSpace(out.Content) == "" {
		return NodeOutput{}, errors.New("llm returned an empty response")
	}

	result := NodeOutput{Text: strings.TrimSpace(out.Content)}
	if len(req.Upstream) > 0 {
		result.Citations = req.Upstream[len(req.Upstream)-1].Citations
	}
	return result, nil
}

type extractTasksHandler struct {
	extractor Extractor
	material  MaterialStore
	notifier  NodeNotifier
}

func (h *extractTasksHandler) Run(ctx context.Context, req NodeRequest) (NodeOutput, error) {
	if h.extractor == nil {
		return NodeOutput{}, errors.New("task extractor is not configured")
	}
	cfg, err := req.Node.ExtractTasksConfig()
	if err != nil {
		return NodeOutput{}, err
	}

	chunks := upstreamMaterial(req.Upstream)
	if len(chunks) == 0 && h.material != nil {
		chunks, err = h.material.ListExtractionChunks(ctx, req.Workflow.GroupID, cfg.FileID, extractionChunkLimit)
		if err != nil {
			return NodeOutput{}, fmt.Errorf("list chunks: %w", err)
		}
	}
	if len(chunks) == 0 {
		return NodeOutput{}, errors.New("no indexed material to extract tasks from")
	}

	created, err := h.extractor.ExtractFromChunks(ctx, req.Workflow.GroupID, req.Run.Actor, chunks, cfg.Max)
	if err != nil {
		return NodeOutput{}, err
	}
	ids := make([]int64, 0, len(created))
	for i := range created {
		ids = append(ids, created[i].ID)
		if h.notifier != nil {
			h.notifier.BroadcastTask(ws.EventTaskSuggested, &created[i])
		}
	}
	return NodeOutput{Text: fmt.Sprintf("新增 %d 条任务建议", len(created)), TaskIDs: ids}, nil
}

// upstreamMaterial returns the material carried by the nearest previous step,
// so a step between a retrieve step and the extraction step does not hide it.
func upstreamMaterial(upstream []NodeOutput) []store.RetrievedChunk {
	for i := len(upstream) - 1; i >= 0; i-- {
		if len(upstream[i].Material) > 0 {
			return upstream[i].Material
		}
	}
	return nil
}

type reportHandler struct {
	reports  ReportGenerator
	notifier NodeNotifier
}

func (h *reportHandler) Run(ctx context.Context, req NodeRequest) (NodeOutput, error) {
	if h.reports == nil {
		return NodeOutput{}, errors.New("report service is not configured")
	}
	cfg, err := req.Node.ReportConfig()
	if err != nil {
		return NodeOutput{}, err
	}
	name := strings.TrimSpace(RenderTemplate(cfg.Title, req.Vars))
	if name == "" {
		name = req.Workflow.Name
	}

	res, err := h.reports.GenerateReport(ctx, aitasks.ReportRequest{
		GroupID:      req.Workflow.GroupID,
		Name:         name,
		Prompt:       strings.TrimSpace(RenderTemplate(cfg.Prompt, req.Vars)),
		Sources:      cfg.Sources,
		LookbackDays: cfg.LookbackDays,
		Timezone:     req.Workflow.Timezone,
		Trigger:      req.Run.Trigger,
		Actor:        req.Run.Actor,
		LastRunAt:    req.Run.LastRunAt,
		CallSource:   llmtrack.SourceWorkflow,
	})
	if err != nil {
		return NodeOutput{}, err
	}
	if h.notifier != nil && res.Report.ID != 0 {
		h.notifier.BroadcastReport(ws.EventReportCreated, &res.Report)
		if res.Message != nil {
			h.notifier.BroadcastMessage(res.Message)
		}
	}
	out := NodeOutput{ReportID: res.Report.ID}
	if res.GenErr != nil {
		return out, res.GenErr
	}
	out.Text = res.Report.Content
	return out, nil
}
