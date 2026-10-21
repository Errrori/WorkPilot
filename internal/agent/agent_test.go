package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/Errrori/workpilot/internal/store"
)

type fakeModel struct {
	turns  []*schema.Message
	idx    int
	inputs [][]*schema.Message
}

func (m *fakeModel) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.inputs = append(m.inputs, in)
	if m.idx >= len(m.turns) {
		return schema.AssistantMessage("", nil), nil
	}
	out := m.turns[m.idx]
	m.idx++
	return out, nil
}

func (m *fakeModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("not implemented")
}

func (m *fakeModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

type finishedRun struct {
	status string
	reply  string
	steps  string
}

type fakeStore struct {
	trigger  store.Message
	byID     map[int64]store.Message
	recent   []store.Message
	files    []store.File
	replies  []store.Message
	runs     []store.AgentRunInsert
	finished []finishedRun
}

func (s *fakeStore) GetMessageByID(_ context.Context, id int64) (store.Message, error) {
	if m, ok := s.byID[id]; ok {
		return m, nil
	}
	return s.trigger, nil
}

func (s *fakeStore) ListMessages(_ context.Context, _ string, _ int) ([]store.Message, error) {
	return s.recent, nil
}

func (s *fakeStore) InsertMessage(_ context.Context, groupID, senderName, content string, citations []store.Citation) (store.Message, error) {
	m := store.Message{ID: int64(100 + len(s.replies)), GroupID: groupID, SenderName: senderName, Content: content, Citations: citations}
	s.replies = append(s.replies, m)
	return m, nil
}

func (s *fakeStore) ListTasks(_ context.Context, _ string, _ string, _ int) ([]store.Task, error) {
	return nil, nil
}

func (s *fakeStore) ListFiles(_ context.Context, _ string) ([]store.File, error) {
	return s.files, nil
}

func (s *fakeStore) InsertAgentRun(_ context.Context, in store.AgentRunInsert) (store.AgentRun, error) {
	s.runs = append(s.runs, in)
	return store.AgentRun{ID: int64(len(s.runs)), GroupID: in.GroupID}, nil
}

func (s *fakeStore) FinishAgentRun(_ context.Context, _ int64, status, reply, steps string, _, _ int, _ string) error {
	s.finished = append(s.finished, finishedRun{status: status, reply: reply, steps: steps})
	return nil
}

type fakeExtractor struct {
	calls     int
	msgs      []store.Message
	fileCalls int
	fileID    int64
}

func (e *fakeExtractor) ExtractFromMessages(_ context.Context, _, _ string, msgs []store.Message, _ int) ([]store.Task, error) {
	e.calls++
	e.msgs = msgs
	return []store.Task{{ID: 5, Title: "修复登录超时", Status: store.TaskStatusSuggested}}, nil
}

func (e *fakeExtractor) Extract(_ context.Context, _, _ string, fileID int64) ([]store.Task, error) {
	e.fileCalls++
	e.fileID = fileID
	return []store.Task{{ID: 6, Title: "来自文件的任务", Status: store.TaskStatusSuggested, Origin: store.TaskOriginDocument}}, nil
}

type fakeFileSink struct {
	saved       int
	lastContent string
}

func (f *fakeFileSink) Save(_ context.Context, groupID, _, fileName, content string) (store.File, error) {
	f.saved++
	f.lastContent = content
	return store.File{ID: 3, GroupID: groupID, FileName: fileName}, nil
}

type fakeNotifier struct {
	tasks    []store.Task
	messages []store.Message
	files    []store.File
}

func (n *fakeNotifier) BroadcastMessage(m *store.Message) { n.messages = append(n.messages, *m) }
func (n *fakeNotifier) BroadcastTask(_ string, t *store.Task) {
	n.tasks = append(n.tasks, *t)
}
func (n *fakeNotifier) BroadcastFile(_ string, f *store.File) { n.files = append(n.files, *f) }

func toolCall(id, name, args string) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID:   id,
		Type: "function",
		Function: schema.FunctionCall{
			Name:      name,
			Arguments: args,
		},
	}})
}

func TestRunMessageLoadsRecentMessagesOnDemand(t *testing.T) {
	recent := []store.Message{
		{ID: 5, GroupID: "g1", SenderName: "李四", Content: "周五前修复登录超时，补压测用例"},
		{ID: 7, GroupID: "g1", SenderName: "张三", Content: "@WorkPilot 把上面的结论记成任务"},
	}
	model := &fakeModel{turns: []*schema.Message{
		toolCall("call-1", toolRecent, "{}"),
		toolCall("call-2", toolExtract, "{}"),
		schema.AssistantMessage("已创建 1 条任务建议，请在看板确认。", nil),
	}}
	st := &fakeStore{
		trigger: recent[1],
		byID:    map[int64]store.Message{5: recent[0], 7: recent[1]},
		recent:  recent,
	}
	extractor := &fakeExtractor{}
	notifier := &fakeNotifier{}
	svc, err := NewService(Config{ChatModel: model, Extractor: extractor, Store: st, Notifier: notifier})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	if err := svc.RunMessage(context.Background(), 7); err != nil {
		t.Fatalf("RunMessage: %v", err)
	}
	if extractor.calls != 1 || len(extractor.msgs) != 2 || extractor.msgs[0].ID != 5 {
		t.Fatalf("extractor calls = %d, msgs = %#v", extractor.calls, extractor.msgs)
	}
	if len(notifier.tasks) != 1 || notifier.tasks[0].Title != "修复登录超时" {
		t.Fatalf("broadcast tasks = %#v", notifier.tasks)
	}
	if len(st.replies) != 1 || st.replies[0].SenderName != store.SenderAI {
		t.Fatalf("replies = %#v", st.replies)
	}
	if len(st.finished) != 1 || st.finished[0].status != store.AgentRunStatusSucceeded {
		t.Fatalf("finished = %#v", st.finished)
	}
	for _, tool := range []string{toolRecent, toolExtract} {
		if !strings.Contains(st.finished[0].steps, tool) {
			t.Fatalf("steps = %q, missing %s", st.finished[0].steps, tool)
		}
	}
}

func TestRunMessageUsesReplyTargetOnly(t *testing.T) {
	target := store.Message{ID: 5, GroupID: "g1", SenderName: "李四", Content: "周五前修复登录超时，补压测用例"}
	model := &fakeModel{turns: []*schema.Message{
		toolCall("call-1", toolExtract, "{}"),
		schema.AssistantMessage("已处理。", nil),
	}}
	st := &fakeStore{
		trigger: store.Message{ID: 9, GroupID: "g1", SenderName: "张三", Content: "@WorkPilot 把上面结论记成任务", ReplyTo: ptrInt64(5)},
		byID:    map[int64]store.Message{5: target},
		recent:  []store.Message{{ID: 3, GroupID: "g1", SenderName: "王五", Content: "无关闲聊"}},
	}
	extractor := &fakeExtractor{}
	svc, err := NewService(Config{ChatModel: model, Extractor: extractor, Store: st})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.RunMessage(context.Background(), 9); err != nil {
		t.Fatalf("RunMessage: %v", err)
	}
	if len(extractor.msgs) != 1 || extractor.msgs[0].ID != 5 {
		t.Fatalf("msgs = %#v", extractor.msgs)
	}
}

func TestRunMessageDefaultsToTriggerOnly(t *testing.T) {
	model := &fakeModel{turns: []*schema.Message{
		toolCall("call-1", toolExtract, "{}"),
		schema.AssistantMessage("已处理。", nil),
	}}
	st := &fakeStore{
		trigger: store.Message{ID: 9, GroupID: "g1", SenderName: "张三", Content: "@WorkPilot 记成任务：修复登录超时"},
		recent:  []store.Message{{ID: 3, GroupID: "g1", SenderName: "王五", Content: "无关闲聊"}},
	}
	extractor := &fakeExtractor{}
	svc, err := NewService(Config{ChatModel: model, Extractor: extractor, Store: st})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.RunMessage(context.Background(), 9); err != nil {
		t.Fatalf("RunMessage: %v", err)
	}
	if extractor.calls != 1 || len(extractor.msgs) != 1 || extractor.msgs[0].ID != 9 {
		t.Fatalf("msgs = %#v", extractor.msgs)
	}
}

func TestRunMessageExtractsFromFile(t *testing.T) {
	model := &fakeModel{turns: []*schema.Message{
		toolCall("call-1", toolExtract, `{"file_id":42}`),
		schema.AssistantMessage("已从文件创建任务建议。", nil),
	}}
	st := &fakeStore{
		trigger: store.Message{ID: 9, GroupID: "g1", SenderName: "张三", Content: "@WorkPilot 从文件里抽任务"},
		files:   []store.File{{ID: 42, GroupID: "g1", FileName: "周报.md", ParseStatus: "parsed", IndexStatus: "indexed"}},
	}
	extractor := &fakeExtractor{}
	notifier := &fakeNotifier{}
	svc, err := NewService(Config{ChatModel: model, Extractor: extractor, Store: st, Notifier: notifier})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.RunMessage(context.Background(), 9); err != nil {
		t.Fatalf("RunMessage: %v", err)
	}
	if extractor.fileCalls != 1 || extractor.fileID != 42 || extractor.calls != 0 {
		t.Fatalf("fileCalls = %d, fileID = %d, calls = %d", extractor.fileCalls, extractor.fileID, extractor.calls)
	}
	if len(notifier.tasks) != 1 || notifier.tasks[0].Title != "来自文件的任务" {
		t.Fatalf("tasks = %#v", notifier.tasks)
	}
}

func TestRunMessageAnswersWithoutTools(t *testing.T) {
	model := &fakeModel{turns: []*schema.Message{
		schema.AssistantMessage("当前有 2 条 doing 任务。", nil),
	}}
	st := &fakeStore{
		trigger: store.Message{ID: 9, GroupID: "g1", SenderName: "张三", Content: "@WorkPilot 现在有哪些 tasks"},
	}
	svc, err := NewService(Config{ChatModel: model, Store: st})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.RunMessage(context.Background(), 9); err != nil {
		t.Fatalf("RunMessage: %v", err)
	}
	if len(st.replies) != 1 || !strings.Contains(st.replies[0].Content, "doing") {
		t.Fatalf("replies = %#v", st.replies)
	}
	if len(st.finished) != 1 || st.finished[0].status != store.AgentRunStatusSucceeded {
		t.Fatalf("finished = %#v", st.finished)
	}
}

func TestRunMessageArchivesOnDemand(t *testing.T) {
	target := store.Message{ID: 2, GroupID: "g1", SenderName: "王五", Content: "评审结论：本周先修登录超时"}
	model := &fakeModel{turns: []*schema.Message{
		toolCall("call-1", toolArchive, `{"title":"0912 评审纪要"}`),
		schema.AssistantMessage("已存档，稍后可在文件区检索。", nil),
	}}
	st := &fakeStore{
		trigger: store.Message{ID: 3, GroupID: "g1", SenderName: "王五", Content: "@WorkPilot 这份纪要存档一下", ReplyTo: ptrInt64(2)},
		byID:    map[int64]store.Message{2: target},
	}
	sink := &fakeFileSink{}
	notifier := &fakeNotifier{}
	svc, err := NewService(Config{ChatModel: model, Store: st, FileSink: sink, Notifier: notifier})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.RunMessage(context.Background(), 3); err != nil {
		t.Fatalf("RunMessage: %v", err)
	}
	if sink.saved != 1 || !strings.Contains(sink.lastContent, "评审结论") {
		t.Fatalf("saved = %d, content = %q", sink.saved, sink.lastContent)
	}
	if len(notifier.files) != 1 || notifier.files[0].FileName != "0912 评审纪要.md" {
		t.Fatalf("files = %#v", notifier.files)
	}
}

func TestArchiveStripsInstructionAndWrappers(t *testing.T) {
	doc := "@WorkPilot 帮我把这个存入知识库，然后抽取任务：\n# 购物车模块\n实现添加商品接口"
	model := &fakeModel{turns: []*schema.Message{
		toolCall("call-1", toolArchive, `{"title":"购物车"}`),
		schema.AssistantMessage("已存档。", nil),
	}}
	st := &fakeStore{trigger: store.Message{ID: 3, GroupID: "g1", SenderName: "赵敏", Content: doc}}
	sink := &fakeFileSink{}
	svc, err := NewService(Config{ChatModel: model, Store: st, FileSink: sink})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.RunMessage(context.Background(), 3); err != nil {
		t.Fatalf("RunMessage: %v", err)
	}
	if strings.Contains(sink.lastContent, "@WorkPilot") || strings.Contains(sink.lastContent, "存入知识库") {
		t.Fatalf("archive content not cleaned: %q", sink.lastContent)
	}
	if !strings.Contains(sink.lastContent, "# 购物车模块") || strings.Contains(sink.lastContent, "消息 #") {
		t.Fatalf("archive content = %q", sink.lastContent)
	}
}

func TestArchiveUsesModelProvidedBody(t *testing.T) {
	body := "# 会议要求\n1. 完成任务A\n2. 完成任务B"
	args, err := json.Marshal(map[string]string{"title": "会议要求", "content": body})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	model := &fakeModel{turns: []*schema.Message{
		toolCall("call-1", toolArchive, string(args)),
		schema.AssistantMessage("已新建文档。", nil),
	}}
	st := &fakeStore{trigger: store.Message{
		ID: 3, GroupID: "g1", SenderName: "赵敏",
		Content: "@WorkPilot 帮我把会议讨论新建一个文档，然后抽取任务：根据文档，开发任务可拆分为以下条目：1. 完成任务A 2. 完成任务B",
	}}
	sink := &fakeFileSink{}
	svc, err := NewService(Config{ChatModel: model, Store: st, FileSink: sink})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.RunMessage(context.Background(), 3); err != nil {
		t.Fatalf("RunMessage: %v", err)
	}
	if sink.lastContent != body {
		t.Fatalf("archive body = %q, want %q", sink.lastContent, body)
	}
}

func TestRunMessageIgnoresNonMention(t *testing.T) {
	st := &fakeStore{trigger: store.Message{ID: 1, GroupID: "g1", SenderName: "张三", Content: "大家好"}}
	svc, err := NewService(Config{ChatModel: &fakeModel{}, Store: st})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.RunMessage(context.Background(), 1); err != nil {
		t.Fatalf("RunMessage: %v", err)
	}
	if len(st.runs) != 0 || len(st.replies) != 0 {
		t.Fatalf("runs = %#v, replies = %#v", st.runs, st.replies)
	}
}

func TestRunMessageFallsBackAfterMaxSteps(t *testing.T) {
	model := &fakeModel{turns: []*schema.Message{
		toolCall("call-1", toolExtract, "{}"),
	}}
	st := &fakeStore{
		trigger: store.Message{ID: 7, GroupID: "g1", SenderName: "张三", Content: "@WorkPilot 记成任务"},
	}
	svc, err := NewService(Config{ChatModel: model, Extractor: &fakeExtractor{}, Store: st, MaxSteps: 1})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.RunMessage(context.Background(), 7); err != nil {
		t.Fatalf("RunMessage: %v", err)
	}
	if len(st.replies) != 1 || !strings.Contains(st.replies[0].Content, "任务建议") {
		t.Fatalf("replies = %#v", st.replies)
	}
}

func TestMention(t *testing.T) {
	if !IsMention("你好 @WorkPilot 帮我看看") || !IsMention("@workpilot") || !IsMention("@WORKPILOT") {
		t.Fatal("IsMention should match case-insensitively")
	}
	if IsMention("WorkPilot 是什么") {
		t.Fatal("IsMention should require the @ prefix")
	}
	if got := StripMention("@WorkPilot 记成任务"); got != "记成任务" {
		t.Fatalf("StripMention = %q", got)
	}
	if got := StripMention("请 @WorkPilot 处理"); got != "请  处理" {
		t.Fatalf("StripMention = %q", got)
	}
}

func ptrInt64(v int64) *int64 { return &v }
