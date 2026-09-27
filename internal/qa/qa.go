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

	"github.com/Errrori/workpilot/internal/rag"
	"github.com/Errrori/workpilot/internal/store"
)

const (
	// SenderName is the author shown on persisted AI answers.
	SenderName     = store.SenderAI
	DefaultTimeout = 120 * time.Second
	snippetRunes   = 200
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

// MessageStore persists questions and answers in the group history.
type MessageStore interface {
	InsertMessage(ctx context.Context, groupID, senderName, content string, citations []store.Citation) (store.Message, error)
}

type Config struct {
	Retriever Retriever
	ChatModel ChatModel
	Store     MessageStore
	Timeout   time.Duration
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

// Stream saves the question, retrieves group context and streams a cited
// answer. Sources are delivered before the first delta; the persisted answer
// message is returned.
func (s *Service) Stream(ctx context.Context, groupID, user, question string, onSources func([]store.Citation) error, onDelta func(string) error) (store.Message, error) {
	if s.cfg.Retriever == nil || s.cfg.ChatModel == nil || s.cfg.Store == nil {
		return store.Message{}, errors.New("qa service is not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	if _, err := s.cfg.Store.InsertMessage(ctx, groupID, user, question, nil); err != nil {
		return store.Message{}, fmt.Errorf("save question: %w", err)
	}

	docs, err := s.cfg.Retriever.Retrieve(rag.ContextWithGroup(ctx, groupID), question)
	if err != nil {
		return store.Message{}, fmt.Errorf("retrieve: %w", err)
	}

	citations := buildCitations(docs)
	if onSources != nil {
		if err := onSources(citations); err != nil {
			return store.Message{}, err
		}
	}

	if len(docs) == 0 {
		if onDelta != nil {
			if err := onDelta(noContextAnswer); err != nil {
				return store.Message{}, err
			}
		}
		return s.save(ctx, groupID, noContextAnswer, nil)
	}

	stream, err := s.cfg.ChatModel.Stream(ctx, buildMessages(question, docs))
	if err != nil {
		return store.Message{}, fmt.Errorf("llm stream: %w", err)
	}
	defer stream.Close()

	var answer strings.Builder
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return store.Message{}, fmt.Errorf("llm stream: %w", err)
		}
		if msg == nil || msg.Content == "" {
			continue
		}
		answer.WriteString(msg.Content)
		if onDelta != nil {
			if err := onDelta(msg.Content); err != nil {
				return store.Message{}, err
			}
		}
	}

	text := strings.TrimSpace(answer.String())
	if text == "" {
		return store.Message{}, errors.New("llm returned an empty answer")
	}
	return s.save(ctx, groupID, text, citations)
}

func (s *Service) save(ctx context.Context, groupID, content string, citations []store.Citation) (store.Message, error) {
	msg, err := s.cfg.Store.InsertMessage(ctx, groupID, SenderName, content, citations)
	if err != nil {
		return store.Message{}, fmt.Errorf("save answer: %w", err)
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
