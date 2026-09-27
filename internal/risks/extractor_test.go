package risks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/Errrori/workpilot/internal/store"
)

type fakeChatModel struct {
	reply  string
	err    error
	inputs []*schema.Message
}

func (m *fakeChatModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.inputs = input
	if m.err != nil {
		return nil, m.err
	}
	return schema.AssistantMessage(m.reply, nil), nil
}

type fakeStore struct {
	tasks    []store.Task
	chunks   []store.RetrievedChunk
	titles   []string
	inserted []store.RiskInsert
	listErr  error
	err      error
}

func (s *fakeStore) ListUnfinishedTasks(_ context.Context, _ string, _ int) ([]store.Task, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.tasks, nil
}

func (s *fakeStore) ListExtractionChunks(_ context.Context, _ string, _ int64, _ int) ([]store.RetrievedChunk, error) {
	return s.chunks, nil
}

func (s *fakeStore) ExistingRiskTitles(_ context.Context, _ string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.titles, nil
}

func (s *fakeStore) InsertRisk(_ context.Context, in store.RiskInsert) (store.Risk, error) {
	if s.err != nil {
		return store.Risk{}, s.err
	}
	s.inserted = append(s.inserted, in)
	return store.Risk{
		ID:             int64(len(s.inserted)),
		GroupID:        in.GroupID,
		Title:          in.Title,
		Description:    in.Description,
		Severity:       in.Severity,
		Status:         in.Status,
		Owner:          in.Owner,
		Source:         in.Source,
		CreatedBy:      in.CreatedBy,
		Citations:      in.Citations,
		RelatedTaskIDs: in.RelatedTaskIDs,
	}, nil
}

func testTasks() []store.Task {
	return []store.Task{
		{ID: 11, GroupID: "group-1", Title: "补齐测试环境", Status: store.TaskStatusTodo, Priority: store.TaskPriorityHigh, UpdatedAt: time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)},
		{ID: 12, GroupID: "group-1", Title: "接入监控告警", Status: store.TaskStatusDoing, Assignee: "bob", Priority: store.TaskPriorityMedium, UpdatedAt: time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)},
	}
}

func testChunks() []store.RetrievedChunk {
	return []store.RetrievedChunk{
		{ChunkID: 1, FileID: 7, FileName: "meeting.md", ChunkIndex: 0, Content: strings.Repeat("联", 300)},
		{ChunkID: 2, FileID: 7, FileName: "meeting.md", ChunkIndex: 1, Content: "联调依赖第三方接口，对方可能延期两周。"},
	}
}

func TestExtractStoresSuggestions(t *testing.T) {
	chat := &fakeChatModel{reply: "```json\n" +
		`[{"title":" 第三方接口延期阻塞联调 ","description":"对方可能延期两周，影响十月上线","severity":"HIGH","owner":"alice","task_ids":[1,"2",2,99],"sources":[1,99]},` +
		`{"title":"测试环境未就绪","description":"","severity":"urgent","task_ids":[],"sources":[]}]` +
		"\n```"}
	st := &fakeStore{tasks: testTasks(), chunks: testChunks(), titles: []string{"旧风险"}}
	svc := NewService(Config{ChatModel: chat, Store: st, MaxRisks: 5, CharBudget: 1000})

	created, err := svc.Extract(context.Background(), "group-1", "bob", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(created) != 2 || len(st.inserted) != 2 {
		t.Fatalf("created = %d, inserted = %d, want 2", len(created), len(st.inserted))
	}

	first := st.inserted[0]
	if first.Title != "第三方接口延期阻塞联调" || first.Severity != store.RiskSeverityHigh || first.Owner != "alice" {
		t.Fatalf("first insert = %#v", first)
	}
	if first.Status != store.RiskStatusSuggested || first.Source != store.RiskSourceExtracted || first.CreatedBy != "bob" {
		t.Fatalf("first insert = %#v", first)
	}
	if len(first.Citations) != 1 || first.Citations[0].FileID != 7 || first.Citations[0].FileName != "meeting.md" {
		t.Fatalf("first citations = %#v", first.Citations)
	}
	if len(first.RelatedTaskIDs) != 2 || first.RelatedTaskIDs[0] != 11 || first.RelatedTaskIDs[1] != 12 {
		t.Fatalf("related task ids = %#v", first.RelatedTaskIDs)
	}
	if second := st.inserted[1]; second.Severity != store.RiskSeverityMedium || len(second.Citations) != 0 || len(second.RelatedTaskIDs) != 0 {
		t.Fatalf("second insert = %#v", second)
	}

	if len(chat.inputs) != 2 || chat.inputs[0].Role != schema.System {
		t.Fatalf("chat inputs = %#v", chat.inputs)
	}
	prompt := chat.inputs[1].Content
	for _, want := range []string{"【任务 1】补齐测试环境", "【任务 2】接入监控告警", "【资料 1】文件：meeting.md（第 0 块）"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %q", want, prompt)
		}
	}
}

func TestExtractWorksWithTasksOnly(t *testing.T) {
	chat := &fakeChatModel{reply: `[{"title":"高优先级任务停滞","task_ids":[1],"sources":[]}]`}
	st := &fakeStore{tasks: testTasks()}
	svc := NewService(Config{ChatModel: chat, Store: st})

	created, err := svc.Extract(context.Background(), "group-1", "alice", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("created = %#v", created)
	}
	prompt := chat.inputs[1].Content
	if !strings.Contains(prompt, "任务看板") || strings.Contains(prompt, "群内资料") {
		t.Fatalf("prompt = %q", prompt)
	}
}

func TestExtractSkipsExistingTitles(t *testing.T) {
	chat := &fakeChatModel{reply: `[{"title":" 测试环境未就绪 ","sources":[]}]`}
	st := &fakeStore{tasks: testTasks(), titles: []string{"测试环境未就绪"}}
	svc := NewService(Config{ChatModel: chat, Store: st})

	created, err := svc.Extract(context.Background(), "group-1", "alice", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(created) != 0 || len(st.inserted) != 0 {
		t.Fatalf("created = %#v, inserted = %#v", created, st.inserted)
	}
}

func TestExtractWithoutMaterial(t *testing.T) {
	chat := &fakeChatModel{reply: "[]"}
	st := &fakeStore{}
	svc := NewService(Config{ChatModel: chat, Store: st})

	_, err := svc.Extract(context.Background(), "group-1", "alice", 0)
	if !errors.Is(err, ErrNoMaterial) {
		t.Fatalf("err = %v, want ErrNoMaterial", err)
	}
	if chat.inputs != nil {
		t.Fatal("chat model should not be called")
	}
}

func TestExtractInvalidResponse(t *testing.T) {
	chat := &fakeChatModel{reply: "抱歉，我看不出来。"}
	st := &fakeStore{tasks: testTasks()}
	svc := NewService(Config{ChatModel: chat, Store: st})

	_, err := svc.Extract(context.Background(), "group-1", "alice", 0)
	if err == nil || !strings.Contains(err.Error(), "parse llm response") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseRisksTolerantRefs(t *testing.T) {
	content := `[{"title":"风险","severity":"high","task_ids":[1,"2","x",0,99],"sources":[1,"2",0,99]}]`

	suggestions, err := parseRisks(content, testTasks(), testChunks(), 10)
	if err != nil {
		t.Fatalf("parseRisks: %v", err)
	}
	if len(suggestions) != 1 {
		t.Fatalf("suggestions = %#v", suggestions)
	}
	if len(suggestions[0].RelatedTaskIDs) != 2 {
		t.Fatalf("related task ids = %#v", suggestions[0].RelatedTaskIDs)
	}
	if len(suggestions[0].Citations) != 2 || suggestions[0].Citations[0].ChunkIndex != 0 || suggestions[0].Citations[1].ChunkIndex != 1 {
		t.Fatalf("citations = %#v", suggestions[0].Citations)
	}
}

func TestParseRisksTruncatesFields(t *testing.T) {
	content := `[{"title":"","sources":[]},{"title":"` + strings.Repeat("险", 250) +
		`","description":"` + strings.Repeat("说", 2100) +
		`","owner":"` + strings.Repeat("人", 80) +
		`","severity":"high","sources":[1]}]`

	suggestions, err := parseRisks(content, testTasks(), testChunks(), 10)
	if err != nil {
		t.Fatalf("parseRisks: %v", err)
	}
	if len(suggestions) != 1 {
		t.Fatalf("suggestions = %#v", suggestions)
	}
	sg := suggestions[0]
	if got := len([]rune(sg.Title)); got != maxTitleRunes {
		t.Fatalf("title runes = %d, want %d", got, maxTitleRunes)
	}
	if got := len([]rune(sg.Description)); got != maxDescriptionRunes {
		t.Fatalf("description runes = %d, want %d", got, maxDescriptionRunes)
	}
	if got := len([]rune(sg.Owner)); got != maxOwnerRunes {
		t.Fatalf("owner runes = %d, want %d", got, maxOwnerRunes)
	}
	if got := len([]rune(sg.Citations[0].Snippet)); got != snippetRunes {
		t.Fatalf("snippet runes = %d, want %d", got, snippetRunes)
	}
}

func TestBuildMaterialSharesBudget(t *testing.T) {
	tasks := []store.Task{
		{ID: 1, Title: strings.Repeat("任", 60)},
		{ID: 2, Title: strings.Repeat("务", 60)},
		{ID: 3, Title: strings.Repeat("三", 60)},
	}
	chunks := []store.RetrievedChunk{
		{Content: strings.Repeat("a", 50)},
		{Content: strings.Repeat("b", 50)},
		{Content: strings.Repeat("c", 50)},
	}

	keptTasks, keptChunks := buildMaterial(tasks, chunks, 200)
	if len(keptTasks) != 1 {
		t.Fatalf("kept tasks = %d, want 1", len(keptTasks))
	}
	if len(keptChunks) != 2 {
		t.Fatalf("kept chunks = %d, want 2", len(keptChunks))
	}

	keptTasks, keptChunks = buildMaterial(nil, chunks, 60)
	if len(keptTasks) != 0 || len(keptChunks) != 2 {
		t.Fatalf("kept tasks = %d, kept chunks = %d", len(keptTasks), len(keptChunks))
	}
}
