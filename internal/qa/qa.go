package qa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"

	"github.com/Errrori/workpilot/internal/llmtrack"
	"github.com/Errrori/workpilot/internal/rag"
	"github.com/Errrori/workpilot/internal/store"
)

const (
	DefaultTimeout = 120 * time.Second
	snippetRunes   = 200
	titleRunes     = 30
)

const systemPrompt = `你是 WorkPilot，一个面向小型研发团队的项目进度助手。
请只依据下面提供的“群内资料”回答问题，不要编造资料中不存在的信息。
回答中的每个结论都必须用 [编号] 标注来源，编号与资料的编号一致，例如 [1]。
如果资料不足以回答，请直接说明缺少什么，不要猜测。
用简体中文回答，简洁、分点。`

const noContextAnswer = "群内暂时没有可检索的已索引资料，无法回答这个问题。请先上传相关文档并等待索引完成。"

// Retriever fetches relevant documents for a group-scoped query.
type Retriever interface {
	Retrieve(ctx context.Context, query string, opts ...retriever.Option) ([]*schema.Document, error)
}

// ChatModel is the streaming subset of the Eino chat model.
type ChatModel interface {
	Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error)
}

// ConversationStore persists private QA threads and their turns.
type ConversationStore interface {
	CreateConversation(ctx context.Context, groupID, userName, title string) (store.QAConversation, error)
	GetConversation(ctx context.Context, groupID, userName string, id int64) (store.QAConversation, error)
	AppendMessage(ctx context.Context, conversationID int64, role, content string, citations []store.Citation) (store.QAMessage, error)
}

type Config struct {
	Retriever Retriever
	ChatModel ChatModel
	Store     ConversationStore
	Timeout   time.Duration
	// MinScore drops retrieved chunks below this cosine similarity so that
	// unrelated questions yield no context and the model refuses instead of
	// answering from noise. Zero disables the filter.
	MinScore float64
}

type Service struct {
	cfg Config
}

func NewService(cfg Config) *Service {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	return &Service{cfg: cfg}
}

// retrievalOpts returns the shared retrieval options derived from config.
func (s *Service) retrievalOpts() []retriever.Option {
	if s.cfg.MinScore > 0 {
		return []retriever.Option{retriever.WithScoreThreshold(s.cfg.MinScore)}
	}
	return nil
}

// Stream persists the question in a private conversation, retrieves group
// context and streams a cited answer. Sources are delivered before the first
// delta; the persisted answer message is returned.
func (s *Service) Stream(ctx context.Context, groupID, user string, conversationID int64, question string, onConversation func(store.QAConversation) error, onSources func([]store.Citation) error, onDelta func(string) error) (store.QAMessage, error) {
	if s.cfg.Retriever == nil || s.cfg.ChatModel == nil || s.cfg.Store == nil {
		return store.QAMessage{}, errors.New("qa service is not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	conversation, err := s.ensureConversation(ctx, groupID, user, conversationID, question)
	if err != nil {
		return store.QAMessage{}, err
	}
	if onConversation != nil {
		if err := onConversation(conversation); err != nil {
			return store.QAMessage{}, err
		}
	}

	if _, err := s.cfg.Store.AppendMessage(ctx, conversation.ID, store.QARoleUser, question, nil); err != nil {
		return store.QAMessage{}, fmt.Errorf("save question: %w", err)
	}

	docs, err := s.cfg.Retriever.Retrieve(rag.ContextWithGroup(ctx, groupID), question, s.retrievalOpts()...)
	if err != nil {
		return store.QAMessage{}, fmt.Errorf("retrieve: %w", err)
	}

	citations := buildCitations(docs)
	if onSources != nil {
		if err := onSources(citations); err != nil {
			return store.QAMessage{}, err
		}
	}

	if len(docs) == 0 {
		if onDelta != nil {
			if err := onDelta(noContextAnswer); err != nil {
				return store.QAMessage{}, err
			}
		}
		return s.save(ctx, conversation.ID, noContextAnswer, nil)
	}

	ctx = llmtrack.WithCall(ctx, llmtrack.SourceQA, groupID)
	stream, err := s.cfg.ChatModel.Stream(ctx, buildMessages(question, docs))
	if err != nil {
		return store.QAMessage{}, fmt.Errorf("llm stream: %w", err)
	}
	defer stream.Close()

	var answer strings.Builder
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return store.QAMessage{}, fmt.Errorf("llm stream: %w", err)
		}
		if msg == nil || msg.Content == "" {
			continue
		}
		answer.WriteString(msg.Content)
		if onDelta != nil {
			if err := onDelta(msg.Content); err != nil {
				return store.QAMessage{}, err
			}
		}
	}

	text := strings.TrimSpace(answer.String())
	if text == "" {
		return store.QAMessage{}, errors.New("llm returned an empty answer")
	}
	return s.save(ctx, conversation.ID, text, citations)
}

func (s *Service) ensureConversation(ctx context.Context, groupID, user string, conversationID int64, question string) (store.QAConversation, error) {
	if conversationID > 0 {
		conversation, err := s.cfg.Store.GetConversation(ctx, groupID, user, conversationID)
		if err != nil {
			return store.QAConversation{}, fmt.Errorf("load conversation: %w", err)
		}
		return conversation, nil
	}
	title := truncateRunes(strings.TrimSpace(question), titleRunes)
	conversation, err := s.cfg.Store.CreateConversation(ctx, groupID, user, title)
	if err != nil {
		return store.QAConversation{}, fmt.Errorf("create conversation: %w", err)
	}
	return conversation, nil
}

// Search returns the group's retrieval hits for query without calling the LLM.
func (s *Service) Search(ctx context.Context, groupID, query string, topK int) ([]store.Citation, error) {
	if s.cfg.Retriever == nil {
		return nil, errors.New("qa service is not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	opts := s.retrievalOpts()
	if topK > 0 {
		opts = append(opts, retriever.WithTopK(topK))
	}
	docs, err := s.cfg.Retriever.Retrieve(rag.ContextWithGroup(ctx, groupID), query, opts...)
	if err != nil {
		return nil, fmt.Errorf("retrieve: %w", err)
	}
	return buildCitations(docs), nil
}

func (s *Service) save(ctx context.Context, conversationID int64, content string, citations []store.Citation) (store.QAMessage, error) {
	msg, err := s.cfg.Store.AppendMessage(ctx, conversationID, store.QARoleAssistant, content, citations)
	if err != nil {
		return store.QAMessage{}, fmt.Errorf("save answer: %w", err)
	}
	return msg, nil
}

func buildMessages(question string, docs []*schema.Document) []*schema.Message {
	var context strings.Builder
	for i, doc := range docs {
		fileName, _ := doc.MetaData[rag.MetaFileName].(string)
		chunkIndex, _ := doc.MetaData[rag.MetaChunkIndex].(int)
		fmt.Fprintf(&context, "【资料 %d】文件：%s（第 %d 块）\n%s\n\n", i+1, fileName, chunkIndex, doc.Content)
	}
	return []*schema.Message{
		schema.SystemMessage(systemPrompt),
		schema.UserMessage("群内资料：\n" + context.String() + "问题：" + question),
	}
}

func buildCitations(docs []*schema.Document) []store.Citation {
	citations := make([]store.Citation, 0, len(docs))
	for i, doc := range docs {
		if doc == nil {
			continue
		}
		fileID, _ := doc.MetaData[rag.MetaFileID].(int64)
		fileName, _ := doc.MetaData[rag.MetaFileName].(string)
		chunkIndex, _ := doc.MetaData[rag.MetaChunkIndex].(int)
		citations = append(citations, store.Citation{
			Index:      i + 1,
			FileID:     fileID,
			FileName:   fileName,
			ChunkIndex: chunkIndex,
			Snippet:    truncateRunes(doc.Content, snippetRunes),
			Score:      doc.Score(),
		})
	}
	return citations
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
