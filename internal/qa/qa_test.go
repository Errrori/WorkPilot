package qa

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"

	"github.com/Errrori/workpilot/internal/rag"
	"github.com/Errrori/workpilot/internal/store"
)

type fakeRetriever struct {
	groupID string
	query   string
	docs    []*schema.Document
	err     error
}

func (r *fakeRetriever) Retrieve(ctx context.Context, query string, _ ...retriever.Option) ([]*schema.Document, error) {
	r.query = query
	r.groupID, _ = rag.GroupFromContext(ctx)
	if r.err != nil {
		return nil, r.err
	}
	return r.docs, nil
}

type fakeChatModel struct {
	msgs   []*schema.Message
	chunks []string
	err    error
}

func (m *fakeChatModel) Stream(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.msgs = input
	if m.err != nil {
		return nil, m.err
	}
	if len(m.chunks) == 0 {
		return schema.StreamReaderFromArray([]*schema.Message{}), nil
	}
	msgs := make([]*schema.Message, 0, len(m.chunks))
	for _, chunk := range m.chunks {
		msgs = append(msgs, schema.AssistantMessage(chunk, nil))
	}
	return schema.StreamReaderFromArray(msgs), nil
}

type insertedMessage struct {
	groupID   string
	sender    string
	content   string
	citations []store.Citation
}

type fakeMessageStore struct {
	inserts []insertedMessage
	err     error
}

func (s *fakeMessageStore) InsertMessage(_ context.Context, groupID, senderName, content string, citations []store.Citation) (store.Message, error) {
	if s.err != nil {
		return store.Message{}, s.err
	}
	s.inserts = append(s.inserts, insertedMessage{groupID, senderName, content, citations})
	return store.Message{
		ID:         int64(len(s.inserts)),
		GroupID:    groupID,
		SenderName: senderName,
		Content:    content,
		Citations:  citations,
	}, nil
}

func testDocs() []*schema.Document {
	makeDoc := func(chunk int, content string, score float64) *schema.Document {
		return (&schema.Document{
			ID:      rag.ChunkID(7, chunk),
			Content: content,
			MetaData: map[string]any{
				rag.MetaFileID:     int64(7),
				rag.MetaGroupID:    "group-1",
				rag.MetaChunkIndex: chunk,
				rag.MetaFileName:   "PRD.md",
			},
		}).WithScore(score)
	}
	return []*schema.Document{
		makeDoc(0, "项目计划在十月上线。", 0.91),
		makeDoc(1, "风险：测试环境尚未就绪。", 0.83),
	}
}

func newTestService(docs []*schema.Document, chunks []string) (*Service, *fakeRetriever, *fakeChatModel, *fakeMessageStore) {
	ret := &fakeRetriever{docs: docs}
	chat := &fakeChatModel{chunks: chunks}
	ms := &fakeMessageStore{}
	return NewService(Config{Retriever: ret, ChatModel: chat, Store: ms}), ret, chat, ms
}

func TestStreamAnswersWithCitations(t *testing.T) {
	svc, ret, chat, ms := newTestService(testDocs(), []string{"根据 [1] ", "项目计划在十月上线 [1]，测试环境有风险 [2]。"})

	var sources []store.Citation
	var deltas []string
	msg, err := svc.Stream(context.Background(), "group-1", "alice", "项目进度如何？",
		func(c []store.Citation) error { sources = c; return nil },
		func(d string) error { deltas = append(deltas, d); return nil })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if ret.groupID != "group-1" {
		t.Fatalf("retrieved group = %q", ret.groupID)
	}
	if ret.query != "项目进度如何？" {
		t.Fatalf("retrieved query = %q", ret.query)
	}
	if len(sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(sources))
	}
	if sources[0].Index != 1 || sources[1].Index != 2 {
		t.Fatalf("source indexes = %d, %d", sources[0].Index, sources[1].Index)
	}
	if sources[0].FileID != 7 || sources[0].FileName != "PRD.md" || sources[0].ChunkIndex != 0 {
		t.Fatalf("source = %#v", sources[0])
	}
	if sources[0].Score != 0.91 {
		t.Fatalf("source score = %v", sources[0].Score)
	}
	if len(chat.msgs) != 2 || chat.msgs[0].Role != schema.System {
		t.Fatalf("chat messages = %#v", chat.msgs)
	}
	if !strings.Contains(chat.msgs[1].Content, "【资料 1】") || !strings.Contains(chat.msgs[1].Content, "问题：项目进度如何？") {
		t.Fatalf("user prompt = %q", chat.msgs[1].Content)
	}
	if want := "根据 [1] 项目计划在十月上线 [1]，测试环境有风险 [2]。"; strings.Join(deltas, "") != want {
		t.Fatalf("deltas = %q, want %q", strings.Join(deltas, ""), want)
	}
	if msg.Content != strings.Join(deltas, "") {
		t.Fatalf("answer message = %q", msg.Content)
	}
	if len(ms.inserts) != 2 {
		t.Fatalf("inserts = %d, want 2", len(ms.inserts))
	}
	if ms.inserts[0].sender != "alice" || ms.inserts[0].content != "项目进度如何？" || ms.inserts[0].citations != nil {
		t.Fatalf("question insert = %#v", ms.inserts[0])
	}
	if ms.inserts[1].sender != SenderName || len(ms.inserts[1].citations) != 2 {
		t.Fatalf("answer insert = %#v", ms.inserts[1])
	}
}

func TestStreamWithoutContextSkipsModel(t *testing.T) {
	svc, _, chat, ms := newTestService(nil, []string{"unused"})

	var sources []store.Citation
	var deltas []string
	msg, err := svc.Stream(context.Background(), "group-1", "alice", "有资料吗？",
		func(c []store.Citation) error { sources = c; return nil },
		func(d string) error { deltas = append(deltas, d); return nil })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if len(sources) != 0 {
		t.Fatalf("sources = %#v", sources)
	}
	if strings.Join(deltas, "") != noContextAnswer {
		t.Fatalf("deltas = %q", strings.Join(deltas, ""))
	}
	if msg.Content != noContextAnswer || msg.Citations != nil {
		t.Fatalf("message = %#v", msg)
	}
	if chat.msgs != nil {
		t.Fatalf("chat model should not be called")
	}
	if len(ms.inserts) != 2 || ms.inserts[1].citations != nil {
		t.Fatalf("inserts = %#v", ms.inserts)
	}
}

func TestStreamTruncatesSourceSnippet(t *testing.T) {
	docs := testDocs()
	docs[0].Content = strings.Repeat("测", 300)
	svc, _, _, _ := newTestService(docs, []string{"answer"})

	var sources []store.Citation
	if _, err := svc.Stream(context.Background(), "group-1", "alice", "q",
		func(c []store.Citation) error { sources = c; return nil },
		func(string) error { return nil }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := len([]rune(sources[0].Snippet)); got != snippetRunes {
		t.Fatalf("snippet runes = %d, want %d", got, snippetRunes)
	}
}

func TestStreamRetrieverFailureSavesOnlyQuestion(t *testing.T) {
	svc, ret, chat, ms := newTestService(nil, nil)
	ret.err = errors.New("db down")

	_, err := svc.Stream(context.Background(), "group-1", "alice", "q", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "retrieve") {
		t.Fatalf("err = %v", err)
	}
	if len(ms.inserts) != 1 || ms.inserts[0].sender != "alice" {
		t.Fatalf("inserts = %#v", ms.inserts)
	}
	if chat.msgs != nil {
		t.Fatal("chat model should not be called")
	}
}

func TestStreamModelFailureSavesOnlyQuestion(t *testing.T) {
	svc, _, chat, ms := newTestService(testDocs(), nil)
	chat.err = errors.New("connection refused")

	_, err := svc.Stream(context.Background(), "group-1", "alice", "q", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "llm stream") {
		t.Fatalf("err = %v", err)
	}
	if len(ms.inserts) != 1 {
		t.Fatalf("inserts = %#v", ms.inserts)
	}
}

func TestStreamEmptyAnswerFails(t *testing.T) {
	svc, _, _, ms := newTestService(testDocs(), []string{"", ""})

	_, err := svc.Stream(context.Background(), "group-1", "alice", "q", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "empty answer") {
		t.Fatalf("err = %v", err)
	}
	if len(ms.inserts) != 1 {
		t.Fatalf("inserts = %#v", ms.inserts)
	}
}

func TestStreamPropagatesDeltaCallbackError(t *testing.T) {
	svc, _, _, ms := newTestService(testDocs(), []string{"a", "b"})

	sent := 0
	_, err := svc.Stream(context.Background(), "group-1", "alice", "q", nil, func(string) error {
		sent++
		if sent == 2 {
			return errors.New("client gone")
		}
		return nil
	})
	if err == nil || err.Error() != "client gone" {
		t.Fatalf("err = %v", err)
	}
	if len(ms.inserts) != 1 {
		t.Fatalf("inserts = %#v", ms.inserts)
	}
}
