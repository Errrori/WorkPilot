package tasks

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	chunks   []store.RetrievedChunk
	titles   []string
	inserted []store.TaskInsert
	listErr  error
	err      error
}

func (s *fakeStore) ListExtractionChunks(_ context.Context, _ string, _ int64, _ int) ([]store.RetrievedChunk, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.chunks, nil
}

func (s *fakeStore) ExistingTaskTitles(_ context.Context, _ string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.titles, nil
}

func (s *fakeStore) InsertTask(_ context.Context, in store.TaskInsert) (store.Task, error) {
	if s.err != nil {
		return store.Task{}, s.err
	}
	s.inserted = append(s.inserted, in)
	return store.Task{
		ID:          int64(len(s.inserted)),
		GroupID:     in.GroupID,
		Title:       in.Title,
		Description: in.Description,
		Assignee:    in.Assignee,
		Status:      in.Status,
		Priority:    in.Priority,
		Source:      in.Source,
		CreatedBy:   in.CreatedBy,
		Citations:   in.Citations,
	}, nil
}

func testChunks() []store.RetrievedChunk {
	return []store.RetrievedChunk{
		{ChunkID: 1, FileID: 7, FileName: "PRD.md", ChunkIndex: 0, Content: strings.Repeat("需", 300)},
		{ChunkID: 2, FileID: 7, FileName: "PRD.md", ChunkIndex: 1, Content: "计划十月上线，待补齐测试环境。"},
	}
}

func TestExtractStoresSuggestions(t *testing.T) {
	chat := &fakeChatModel{reply: "```json\n" +
		`[{"title":" 补齐测试环境 ","description":"","assignee":"bob","priority":"HIGH","sources":[2,2,99]},` +
		`{"title":"补一下监控","description":"给服务加监控","priority":"urgent","sources":[]}]` +
		"\n```"}
	st := &fakeStore{chunks: testChunks(), titles: []string{"另一个任务"}}
	svc := NewService(Config{ChatModel: chat, Store: st, MaxSuggestions: 5, CharBudget: 1000})

	created, err := svc.Extract(context.Background(), "group-1", "alice", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(created) != 2 || len(st.inserted) != 2 {
		t.Fatalf("created = %d, inserted = %d, want 2", len(created), len(st.inserted))
	}

	first := st.inserted[0]
	if first.Title != "补齐测试环境" || first.Assignee != "bob" || first.Priority != store.TaskPriorityHigh {
		t.Fatalf("first insert = %#v", first)
	}
	if first.Status != store.TaskStatusSuggested || first.Source != store.TaskSourceExtracted || first.CreatedBy != "alice" {
		t.Fatalf("first insert = %#v", first)
	}
	if len(first.Citations) != 1 {
		t.Fatalf("first citations = %#v", first.Citations)
	}
	c := first.Citations[0]
	if c.Index != 1 || c.FileID != 7 || c.FileName != "PRD.md" || c.ChunkIndex != 1 {
		t.Fatalf("citation = %#v", c)
	}
	if c.Snippet != "计划十月上线，待补齐测试环境。" {
		t.Fatalf("citation snippet = %q", c.Snippet)
	}
	if second := st.inserted[1]; second.Priority != store.TaskPriorityMedium || len(second.Citations) != 0 {
		t.Fatalf("second insert = %#v", second)
	}

	if len(chat.inputs) != 2 || chat.inputs[0].Role != schema.System {
		t.Fatalf("chat inputs = %#v", chat.inputs)
	}
	prompt := chat.inputs[1].Content
	for _, want := range []string{"【资料 1】文件：PRD.md（第 0 块）", "【资料 2】文件：PRD.md（第 1 块）"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %q", want, prompt)
		}
	}
}

func TestExtractSkipsExistingTitles(t *testing.T) {
	chat := &fakeChatModel{reply: `[{"title":" 已完成的旧任务 ","priority":"medium","sources":[1]}]`}
	st := &fakeStore{chunks: testChunks(), titles: []string{"已完成的旧任务"}}
	svc := NewService(Config{ChatModel: chat, Store: st})

	created, err := svc.Extract(context.Background(), "group-1", "alice", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(created) != 0 || len(st.inserted) != 0 {
		t.Fatalf("created = %#v, inserted = %#v", created, st.inserted)
	}
}

func TestExtractRespectsMaxSuggestions(t *testing.T) {
	chat := &fakeChatModel{reply: `[{"title":"任务一","sources":[]},{"title":"任务二","sources":[]}]`}
	st := &fakeStore{chunks: testChunks()}
	svc := NewService(Config{ChatModel: chat, Store: st, MaxSuggestions: 1})

	created, err := svc.Extract(context.Background(), "group-1", "alice", 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(created) != 1 || created[0].Title != "任务一" {
		t.Fatalf("created = %#v", created)
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

func TestExtractModelFailure(t *testing.T) {
	chat := &fakeChatModel{err: errors.New("connection refused")}
	st := &fakeStore{chunks: testChunks()}
	svc := NewService(Config{ChatModel: chat, Store: st})

	_, err := svc.Extract(context.Background(), "group-1", "alice", 0)
	if err == nil || !strings.Contains(err.Error(), "llm generate") {
		t.Fatalf("err = %v", err)
	}
	if len(st.inserted) != 0 {
		t.Fatalf("inserted = %#v", st.inserted)
	}
}

func TestExtractInvalidResponse(t *testing.T) {
	chat := &fakeChatModel{reply: "抱歉，我无法抽取任务。"}
	st := &fakeStore{chunks: testChunks()}
	svc := NewService(Config{ChatModel: chat, Store: st})

	_, err := svc.Extract(context.Background(), "group-1", "alice", 0)
	if err == nil || !strings.Contains(err.Error(), "parse llm response") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseSuggestionsTolerantSources(t *testing.T) {
	content := `[{"title":"任务","sources":[1,"2","x",0,99]},{"title":"无来源","sources":"1"}]`

	suggestions, err := parseSuggestions(content, testChunks(), 10)
	if err != nil {
		t.Fatalf("parseSuggestions: %v", err)
	}
	if len(suggestions) != 2 {
		t.Fatalf("suggestions = %#v", suggestions)
	}
	if len(suggestions[0].Citations) != 2 {
		t.Fatalf("citations = %#v", suggestions[0].Citations)
	}
	if suggestions[0].Citations[0].ChunkIndex != 0 || suggestions[0].Citations[1].ChunkIndex != 1 {
		t.Fatalf("citations = %#v", suggestions[0].Citations)
	}
	if len(suggestions[1].Citations) != 0 {
		t.Fatalf("citations = %#v", suggestions[1].Citations)
	}
}

func TestParseSuggestionsTruncatesFields(t *testing.T) {
	content := `[{"title":"","sources":[]},{"title":"` + strings.Repeat("标", 250) +
		`","description":"` + strings.Repeat("说", 2100) +
		`","assignee":"` + strings.Repeat("人", 80) +
		`","priority":"high","sources":[1]}]`

	suggestions, err := parseSuggestions(content, testChunks(), 10)
	if err != nil {
		t.Fatalf("parseSuggestions: %v", err)
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
	if got := len([]rune(sg.Assignee)); got != maxAssigneeRunes {
		t.Fatalf("assignee runes = %d, want %d", got, maxAssigneeRunes)
	}
	if got := len([]rune(sg.Citations[0].Snippet)); got != snippetRunes {
		t.Fatalf("snippet runes = %d, want %d", got, snippetRunes)
	}
}

func TestExtractJSONArrayForms(t *testing.T) {
	valid := map[string]string{
		`[{"title":"a"}]`:                     `[{"title":"a"}]`,
		"```json\n[{\"title\":\"a\"}]\n```":   `[{"title":"a"}]`,
		"以下是结果：\n[{\"title\":\"a\"}]\n完毕":     `[{"title":"a"}]`,
		"prefix [ {\"title\":\"a\"} ] suffix": `[ {"title":"a"} ]`,
	}
	for in, want := range valid {
		got, err := extractJSONArray(in)
		if err != nil || got != want {
			t.Fatalf("extractJSONArray(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := extractJSONArray("没有数组"); err == nil {
		t.Fatal("extractJSONArray should fail without an array")
	}
}

func TestBuildMaterialBudget(t *testing.T) {
	chunk := func(content string) store.RetrievedChunk {
		return store.RetrievedChunk{Content: content}
	}
	chunks := []store.RetrievedChunk{chunk(strings.Repeat("a", 10)), chunk(strings.Repeat("b", 10)), chunk(strings.Repeat("c", 10)), chunk(strings.Repeat("d", 10))}

	if got := len(buildMaterial(chunks, 25)); got != 3 {
		t.Fatalf("material chunks = %d, want 3", got)
	}
	if got := len(buildMaterial(nil, 100)); got != 0 {
		t.Fatalf("material chunks = %d, want 0", got)
	}
}
