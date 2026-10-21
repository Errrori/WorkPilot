package qa

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"
	"github.com/jackc/pgx/v5"

	"github.com/Errrori/workpilot/internal/rag"
	"github.com/Errrori/workpilot/internal/store"
)

type fakeRetriever struct {
	groupID string
	query   string
	docs    []*schema.Document
	err     error
}

func (r *fakeRetriever) Retrieve(ctx context.Context, query string, opts ...retriever.Option) ([]*schema.Document, error) {
	r.query = query
	r.groupID, _ = rag.GroupFromContext(ctx)
	if r.err != nil {
		return nil, r.err
	}
	common := retriever.GetCommonOptions(&retriever.Options{}, opts...)
	if common.ScoreThreshold == nil {
		return r.docs, nil
	}
	var kept []*schema.Document
	for _, d := range r.docs {
		if d.Score() >= *common.ScoreThreshold {
			kept = append(kept, d)
		}
	}
	return kept, nil
}

type fakeChatModel struct {
	msgs          []*schema.Message
	chunks        []string
	err           error
	generateCalls int
	generateInput []*schema.Message
	generateOut   string
	generateErr   error
}

func (m *fakeChatModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.generateCalls++
	m.generateInput = input
	if m.generateErr != nil {
		return nil, m.generateErr
	}
	return schema.AssistantMessage(m.generateOut, nil), nil
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

type appendedMessage struct {
	conversationID int64
	role           string
	content        string
	citations      []store.Citation
	rewrittenQuery *string
}

type fakeConversationStore struct {
	conversations []store.QAConversation
	messages      []store.QAMessage
	appends       []appendedMessage
	createErr     error
	getErr        error
	appendErr     error
	listErr       error
	nextID        int64
}

func (s *fakeConversationStore) CreateConversation(_ context.Context, groupID, userName, title string) (store.QAConversation, error) {
	if s.createErr != nil {
		return store.QAConversation{}, s.createErr
	}
	s.nextID++
	c := store.QAConversation{ID: s.nextID, GroupID: groupID, UserName: userName, Title: title}
	s.conversations = append(s.conversations, c)
	return c, nil
}

func (s *fakeConversationStore) GetConversation(_ context.Context, groupID, userName string, id int64) (store.QAConversation, error) {
	if s.getErr != nil {
		return store.QAConversation{}, s.getErr
	}
	for _, c := range s.conversations {
		if c.ID == id && c.GroupID == groupID && c.UserName == userName {
			return c, nil
		}
	}
	return store.QAConversation{}, pgx.ErrNoRows
}

func (s *fakeConversationStore) AppendMessage(_ context.Context, conversationID int64, role, content string, citations []store.Citation, rewrittenQuery *string) (store.QAMessage, error) {
	if s.appendErr != nil {
		return store.QAMessage{}, s.appendErr
	}
	s.appends = append(s.appends, appendedMessage{conversationID, role, content, citations, rewrittenQuery})
	msg := store.QAMessage{
		ID:             int64(len(s.appends)),
		ConversationID: conversationID,
		Role:           role,
		Content:        content,
		Citations:      citations,
		RewrittenQuery: rewrittenQuery,
	}
	s.messages = append(s.messages, msg)
	return msg, nil
}

func (s *fakeConversationStore) ListMessages(_ context.Context, conversationID int64, limit int) ([]store.QAMessage, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var messages []store.QAMessage
	for _, m := range s.messages {
		if m.ConversationID == conversationID {
			messages = append(messages, m)
		}
	}
	if limit > 0 && len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}
	return messages, nil
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

func newTestService(docs []*schema.Document, chunks []string) (*Service, *fakeRetriever, *fakeChatModel, *fakeConversationStore) {
	ret := &fakeRetriever{docs: docs}
	chat := &fakeChatModel{chunks: chunks}
	cs := &fakeConversationStore{}
	return NewService(Config{Retriever: ret, ChatModel: chat, Store: cs}), ret, chat, cs
}

func TestSearchReturnsCitationsWithoutLLM(t *testing.T) {
	svc := NewService(Config{Retriever: &fakeRetriever{docs: testDocs()}})

	sources, err := svc.Search(context.Background(), "group-1", "项目进度如何？", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("sources = %#v", sources)
	}
	if sources[0].FileID != 7 || sources[0].FileName != "PRD.md" || sources[0].ChunkIndex != 0 || sources[0].Score != 0.91 {
		t.Fatalf("source = %#v", sources[0])
	}
}

func TestStreamAnswersWithCitations(t *testing.T) {
	svc, ret, chat, cs := newTestService(testDocs(), []string{"根据 [1] ", "项目计划在十月上线 [1]，测试环境有风险 [2]。"})

	var conversation store.QAConversation
	var sources []store.Citation
	var deltas []string
	msg, err := svc.Stream(context.Background(), "group-1", "alice", 0, "项目进度如何？",
		func(c store.QAConversation) error { conversation = c; return nil },
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
	if conversation.ID == 0 || conversation.GroupID != "group-1" || conversation.UserName != "alice" || conversation.Title != "项目进度如何？" {
		t.Fatalf("conversation = %#v", conversation)
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
	if msg.Content != strings.Join(deltas, "") || msg.ConversationID != conversation.ID || msg.Role != store.QARoleAssistant {
		t.Fatalf("answer message = %#v", msg)
	}
	if len(cs.appends) != 2 {
		t.Fatalf("appends = %d, want 2", len(cs.appends))
	}
	if cs.appends[0].conversationID != conversation.ID || cs.appends[0].role != store.QARoleUser || cs.appends[0].content != "项目进度如何？" || cs.appends[0].citations != nil {
		t.Fatalf("question append = %#v", cs.appends[0])
	}
	if cs.appends[1].role != store.QARoleAssistant || len(cs.appends[1].citations) != 2 {
		t.Fatalf("answer append = %#v", cs.appends[1])
	}
}

func TestStreamReusesExistingConversation(t *testing.T) {
	svc, _, _, cs := newTestService(testDocs(), []string{"answer"})
	existing := store.QAConversation{ID: 42, GroupID: "group-1", UserName: "alice", Title: "旧会话"}
	cs.conversations = append(cs.conversations, existing)

	var got store.QAConversation
	if _, err := svc.Stream(context.Background(), "group-1", "alice", 42, "继续",
		func(c store.QAConversation) error { got = c; return nil }, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got.ID != 42 {
		t.Fatalf("conversation = %#v", got)
	}
	if len(cs.conversations) != 1 {
		t.Fatalf("conversations = %#v", cs.conversations)
	}
	if len(cs.appends) != 2 || cs.appends[0].conversationID != 42 || cs.appends[1].conversationID != 42 {
		t.Fatalf("appends = %#v", cs.appends)
	}
}

func TestStreamRejectsForeignConversation(t *testing.T) {
	svc, _, chat, cs := newTestService(testDocs(), []string{"answer"})
	cs.conversations = append(cs.conversations, store.QAConversation{ID: 7, GroupID: "group-1", UserName: "bob"})

	called := false
	_, err := svc.Stream(context.Background(), "group-1", "alice", 7, "q",
		func(store.QAConversation) error { called = true; return nil }, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "load conversation") {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("conversation callback should not run")
	}
	if len(cs.appends) != 0 || chat.msgs != nil {
		t.Fatalf("unexpected writes: appends=%#v chat=%#v", cs.appends, chat.msgs)
	}
}

func TestStreamTruncatesConversationTitle(t *testing.T) {
	svc, _, _, cs := newTestService(testDocs(), []string{"answer"})
	question := strings.Repeat("问", titleRunes+10)

	if _, err := svc.Stream(context.Background(), "group-1", "alice", 0, question, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(cs.conversations) != 1 {
		t.Fatalf("conversations = %#v", cs.conversations)
	}
	if got := len([]rune(cs.conversations[0].Title)); got != titleRunes {
		t.Fatalf("title runes = %d, want %d", got, titleRunes)
	}
}

func TestStreamWithoutContextSkipsModel(t *testing.T) {
	svc, _, chat, cs := newTestService(nil, []string{"unused"})

	var sources []store.Citation
	var deltas []string
	msg, err := svc.Stream(context.Background(), "group-1", "alice", 0, "有资料吗？",
		nil,
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
	if msg.Content != noContextAnswer || msg.Citations != nil || msg.Role != store.QARoleAssistant {
		t.Fatalf("message = %#v", msg)
	}
	if chat.msgs != nil {
		t.Fatalf("chat model should not be called")
	}
	if len(cs.appends) != 2 || cs.appends[1].citations != nil {
		t.Fatalf("appends = %#v", cs.appends)
	}
}

func TestStreamFiltersContextBelowMinScore(t *testing.T) {
	ret := &fakeRetriever{docs: testDocs()}
	chat := &fakeChatModel{chunks: []string{"answer"}}
	cs := &fakeConversationStore{}
	svc := NewService(Config{Retriever: ret, ChatModel: chat, Store: cs, MinScore: 0.9})

	var sources []store.Citation
	if _, err := svc.Stream(context.Background(), "group-1", "alice", 0, "q",
		nil,
		func(c []store.Citation) error { sources = c; return nil },
		func(string) error { return nil }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(sources) != 1 || sources[0].Score != 0.91 {
		t.Fatalf("sources = %#v, want only the 0.91 chunk", sources)
	}
}

func TestStreamRefusesWhenAllContextBelowMinScore(t *testing.T) {
	ret := &fakeRetriever{docs: testDocs()}
	chat := &fakeChatModel{chunks: []string{"unused"}}
	cs := &fakeConversationStore{}
	svc := NewService(Config{Retriever: ret, ChatModel: chat, Store: cs, MinScore: 0.95})

	var sources []store.Citation
	var deltas []string
	msg, err := svc.Stream(context.Background(), "group-1", "alice", 0, "q",
		nil,
		func(c []store.Citation) error { sources = c; return nil },
		func(d string) error { deltas = append(deltas, d); return nil })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("sources = %#v, want none", sources)
	}
	if strings.Join(deltas, "") != noContextAnswer || msg.Content != noContextAnswer {
		t.Fatalf("answer = %q, want refusal", msg.Content)
	}
	if chat.msgs != nil {
		t.Fatal("chat model should not be called")
	}
}

func TestStreamTruncatesSourceSnippet(t *testing.T) {
	docs := testDocs()
	docs[0].Content = strings.Repeat("测", 300)
	svc, _, _, _ := newTestService(docs, []string{"answer"})

	var sources []store.Citation
	if _, err := svc.Stream(context.Background(), "group-1", "alice", 0, "q",
		nil,
		func(c []store.Citation) error { sources = c; return nil },
		func(string) error { return nil }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := len([]rune(sources[0].Snippet)); got != snippetRunes {
		t.Fatalf("snippet runes = %d, want %d", got, snippetRunes)
	}
}

func TestStreamRetrieverFailureSavesOnlyQuestion(t *testing.T) {
	svc, ret, chat, cs := newTestService(nil, nil)
	ret.err = errors.New("db down")

	_, err := svc.Stream(context.Background(), "group-1", "alice", 0, "q", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "retrieve") {
		t.Fatalf("err = %v", err)
	}
	if len(cs.appends) != 1 || cs.appends[0].role != store.QARoleUser {
		t.Fatalf("appends = %#v", cs.appends)
	}
	if chat.msgs != nil {
		t.Fatal("chat model should not be called")
	}
}

func TestStreamModelFailureSavesOnlyQuestion(t *testing.T) {
	svc, _, chat, cs := newTestService(testDocs(), nil)
	chat.err = errors.New("connection refused")

	_, err := svc.Stream(context.Background(), "group-1", "alice", 0, "q", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "llm stream") {
		t.Fatalf("err = %v", err)
	}
	if len(cs.appends) != 1 {
		t.Fatalf("appends = %#v", cs.appends)
	}
}

func TestStreamEmptyAnswerFails(t *testing.T) {
	svc, _, _, cs := newTestService(testDocs(), []string{"", ""})

	_, err := svc.Stream(context.Background(), "group-1", "alice", 0, "q", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "empty answer") {
		t.Fatalf("err = %v", err)
	}
	if len(cs.appends) != 1 {
		t.Fatalf("appends = %#v", cs.appends)
	}
}

func TestStreamPropagatesDeltaCallbackError(t *testing.T) {
	svc, _, _, cs := newTestService(testDocs(), []string{"a", "b"})

	sent := 0
	_, err := svc.Stream(context.Background(), "group-1", "alice", 0, "q", nil, nil, func(string) error {
		sent++
		if sent == 2 {
			return errors.New("client gone")
		}
		return nil
	})
	if err == nil || err.Error() != "client gone" {
		t.Fatalf("err = %v", err)
	}
	if len(cs.appends) != 1 {
		t.Fatalf("appends = %#v", cs.appends)
	}
}

func seedHistory(cs *fakeConversationStore, conversationID int64) {
	cs.messages = append(cs.messages,
		store.QAMessage{ConversationID: conversationID, Role: store.QARoleUser, Content: "项目有哪些风险？"},
		store.QAMessage{ConversationID: conversationID, Role: store.QARoleAssistant, Content: "测试环境尚未就绪。"},
	)
}

func TestStreamRewritesFollowUpQuery(t *testing.T) {
	svc, ret, chat, cs := newTestService(testDocs(), []string{"answer"})
	cs.conversations = append(cs.conversations, store.QAConversation{ID: 42, GroupID: "group-1", UserName: "alice"})
	seedHistory(cs, 42)
	chat.generateOut = "项目风险的负责人是谁？"

	var sources []store.Citation
	if _, err := svc.Stream(context.Background(), "group-1", "alice", 42, "那负责人呢？", nil,
		func(c []store.Citation) error { sources = c; return nil }, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if chat.generateCalls != 1 {
		t.Fatalf("generate calls = %d, want 1", chat.generateCalls)
	}
	if got := ret.query; got != chat.generateOut {
		t.Fatalf("retrieval query = %q, want %q", got, chat.generateOut)
	}
	if len(cs.appends) != 2 || cs.appends[0].content != "那负责人呢？" {
		t.Fatalf("appends = %#v", cs.appends)
	}
	if got := cs.appends[0].rewrittenQuery; got == nil || *got != chat.generateOut {
		t.Fatalf("rewritten query = %v", got)
	}
	if len(sources) != 2 {
		t.Fatalf("sources = %#v", sources)
	}
	if len(chat.msgs) != 4 {
		t.Fatalf("answer messages = %#v", chat.msgs)
	}
	if chat.msgs[1].Role != schema.User || chat.msgs[1].Content != "项目有哪些风险？" {
		t.Fatalf("history user message = %#v", chat.msgs[1])
	}
	if chat.msgs[2].Role != schema.Assistant || chat.msgs[2].Content != "测试环境尚未就绪。" {
		t.Fatalf("history assistant message = %#v", chat.msgs[2])
	}
	if !strings.Contains(chat.msgs[3].Content, "问题：那负责人呢？") {
		t.Fatalf("final user message = %q", chat.msgs[3].Content)
	}
}

func TestStreamSkipsRewriteForStandaloneQuestion(t *testing.T) {
	svc, ret, chat, cs := newTestService(testDocs(), []string{"answer"})
	cs.conversations = append(cs.conversations, store.QAConversation{ID: 42, GroupID: "group-1", UserName: "alice"})
	seedHistory(cs, 42)
	chat.generateOut = "不该被使用"
	question := strings.Repeat("验收标准与发布安排", 3)

	if _, err := svc.Stream(context.Background(), "group-1", "alice", 42, question, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if chat.generateCalls != 0 {
		t.Fatalf("generate calls = %d, want 0", chat.generateCalls)
	}
	if ret.query != question {
		t.Fatalf("retrieval query = %q, want original", ret.query)
	}
	if cs.appends[0].rewrittenQuery != nil {
		t.Fatalf("rewritten query = %v, want nil", cs.appends[0].rewrittenQuery)
	}
}

func TestStreamRewriteFailureFallsBack(t *testing.T) {
	svc, ret, chat, cs := newTestService(testDocs(), []string{"answer"})
	cs.conversations = append(cs.conversations, store.QAConversation{ID: 42, GroupID: "group-1", UserName: "alice"})
	seedHistory(cs, 42)
	chat.generateErr = errors.New("rewrite down")

	if _, err := svc.Stream(context.Background(), "group-1", "alice", 42, "那负责人呢？", nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if chat.generateCalls != 1 {
		t.Fatalf("generate calls = %d, want 1", chat.generateCalls)
	}
	if ret.query != "那负责人呢？" {
		t.Fatalf("retrieval query = %q, want original", ret.query)
	}
	if cs.appends[0].rewrittenQuery != nil {
		t.Fatalf("rewritten query = %v, want nil", cs.appends[0].rewrittenQuery)
	}
}

func TestStreamRewriteEmptyFallsBack(t *testing.T) {
	svc, ret, chat, cs := newTestService(testDocs(), []string{"answer"})
	cs.conversations = append(cs.conversations, store.QAConversation{ID: 42, GroupID: "group-1", UserName: "alice"})
	seedHistory(cs, 42)
	chat.generateOut = "  "

	if _, err := svc.Stream(context.Background(), "group-1", "alice", 42, "那负责人呢？", nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if ret.query != "那负责人呢？" || cs.appends[0].rewrittenQuery != nil {
		t.Fatalf("query = %q, rewritten = %v", ret.query, cs.appends[0].rewrittenQuery)
	}
}

func TestStreamRewriteModeOff(t *testing.T) {
	ret := &fakeRetriever{docs: testDocs()}
	chat := &fakeChatModel{chunks: []string{"answer"}, generateOut: "改写"}
	cs := &fakeConversationStore{conversations: []store.QAConversation{{ID: 42, GroupID: "group-1", UserName: "alice"}}}
	seedHistory(cs, 42)
	svc := NewService(Config{Retriever: ret, ChatModel: chat, Store: cs, RewriteMode: RewriteModeOff})

	if _, err := svc.Stream(context.Background(), "group-1", "alice", 42, "那负责人呢？", nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if chat.generateCalls != 0 {
		t.Fatalf("generate calls = %d, want 0", chat.generateCalls)
	}
}

func TestStreamRewriteModeAlways(t *testing.T) {
	ret := &fakeRetriever{docs: testDocs()}
	chat := &fakeChatModel{chunks: []string{"answer"}, generateOut: "独立改写"}
	cs := &fakeConversationStore{conversations: []store.QAConversation{{ID: 42, GroupID: "group-1", UserName: "alice"}}}
	seedHistory(cs, 42)
	svc := NewService(Config{Retriever: ret, ChatModel: chat, Store: cs, RewriteMode: RewriteModeAlways})

	if _, err := svc.Stream(context.Background(), "group-1", "alice", 42, strings.Repeat("验收标准与发布安排", 3), nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if chat.generateCalls != 1 || ret.query != "独立改写" {
		t.Fatalf("generate calls = %d, query = %q", chat.generateCalls, ret.query)
	}
}

func TestStreamHistoryReadFailureDegradesToSingleTurn(t *testing.T) {
	svc, ret, chat, cs := newTestService(testDocs(), []string{"answer"})
	cs.conversations = append(cs.conversations, store.QAConversation{ID: 42, GroupID: "group-1", UserName: "alice"})
	cs.listErr = errors.New("db down")

	if _, err := svc.Stream(context.Background(), "group-1", "alice", 42, "那负责人呢？", nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if chat.generateCalls != 0 || ret.query != "那负责人呢？" {
		t.Fatalf("generate calls = %d, query = %q", chat.generateCalls, ret.query)
	}
}

func TestStreamWithoutContextRefusesEvenWithHistory(t *testing.T) {
	svc, _, chat, cs := newTestService(nil, []string{"unused"})
	cs.conversations = append(cs.conversations, store.QAConversation{ID: 42, GroupID: "group-1", UserName: "alice"})
	seedHistory(cs, 42)

	var deltas []string
	msg, err := svc.Stream(context.Background(), "group-1", "alice", 42, "那负责人呢？", nil, nil,
		func(d string) error { deltas = append(deltas, d); return nil })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if strings.Join(deltas, "") != noContextAnswer || msg.Content != noContextAnswer {
		t.Fatalf("answer = %q", msg.Content)
	}
	if chat.msgs != nil {
		t.Fatal("answer model should not be called without context")
	}
}

func TestTrimHistoryBudgetAndTurns(t *testing.T) {
	messages := []store.QAMessage{
		{Role: store.QARoleUser, Content: strings.Repeat("一", 10)},
		{Role: store.QARoleAssistant, Content: strings.Repeat("二", 10)},
		{Role: store.QARoleUser, Content: strings.Repeat("三", 10)},
		{Role: store.QARoleAssistant, Content: strings.Repeat("四", 10)},
	}
	trimmed := trimHistory(messages, 6, 25)
	if len(trimmed) != 2 || trimmed[0].Role != store.QARoleUser || trimmed[1].Role != store.QARoleAssistant {
		t.Fatalf("trimmed = %#v", trimmed)
	}
	if trimmed[0].Content != strings.Repeat("三", 10) {
		t.Fatalf("trimmed first = %q", trimmed[0].Content)
	}

	leadingAssistant := []store.QAMessage{
		{Role: store.QARoleAssistant, Content: "孤立回答"},
		{Role: store.QARoleUser, Content: "问题"},
	}
	trimmed = trimHistory(leadingAssistant, 6, 100)
	if len(trimmed) != 1 || trimmed[0].Role != store.QARoleUser {
		t.Fatalf("trimmed leading assistant = %#v", trimmed)
	}

	trimmed = trimHistory([]store.QAMessage{{Role: store.QARoleAssistant, Content: noContextAnswer}}, 6, 100)
	if len(trimmed) != 0 {
		t.Fatalf("trimmed refusal = %#v", trimmed)
	}
}

func TestParseRewrite(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{"plain", "项目风险的负责人是谁？", "项目风险的负责人是谁？", true},
		{"quoted", "“项目风险的负责人是谁？”", "项目风险的负责人是谁？", true},
		{"prefix", "改写后的问题：项目风险的负责人是谁？", "项目风险的负责人是谁？", true},
		{"multiline", "项目风险的负责人是谁？\n解释", "项目风险的负责人是谁？", true},
		{"empty", "  \n ", "", false},
		{"too long", strings.Repeat("问", rewriteQueryRunes+1), "", false},
	}
	for _, tc := range cases {
		got, ok := parseRewrite(tc.raw, "那负责人呢？")
		if got != tc.want || ok != tc.ok {
			t.Fatalf("%s: parseRewrite = (%q, %v), want (%q, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
