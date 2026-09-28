package llmtrack

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type streamItem struct {
	msg *schema.Message
	err error
}

type fakeModel struct {
	genOut  *schema.Message
	genErr  error
	stream  []streamItem
	strmErr error
}

func (f *fakeModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return f.genOut, f.genErr
}

func (f *fakeModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if f.strmErr != nil {
		return nil, f.strmErr
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		for _, item := range f.stream {
			if closed := sw.Send(item.msg, item.err); closed {
				return
			}
		}
	}()
	return sr, nil
}

type fakeRecorder struct {
	mu      sync.Mutex
	records []Record
	calls   chan Record
}

func (r *fakeRecorder) InsertLLMUsage(_ context.Context, rec Record) error {
	r.mu.Lock()
	r.records = append(r.records, rec)
	r.mu.Unlock()
	if r.calls != nil {
		r.calls <- rec
	}
	return nil
}

func (r *fakeRecorder) last(t *testing.T) Record {
	t.Helper()
	select {
	case rec := <-r.calls:
		return rec
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for usage record")
		return Record{}
	}
}

func usageMessage(prompt, completion int) *schema.Message {
	return &schema.Message{
		Role: schema.Assistant,
		ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{
				PromptTokens:     prompt,
				CompletionTokens: completion,
				TotalTokens:      prompt + completion,
			},
		},
	}
}

func TestGenerateRecordsUsage(t *testing.T) {
	rec := &fakeRecorder{calls: make(chan Record, 1)}
	client := Wrap(&fakeModel{genOut: usageMessage(10, 5)}, rec, "openai", "test-model")

	ctx := WithCall(context.Background(), SourceQA, "group-1")
	out, err := client.Generate(ctx, []*schema.Message{schema.UserMessage("q")})
	if err != nil || out == nil {
		t.Fatalf("generate: out=%v err=%v", out, err)
	}

	got := rec.last(t)
	if got.Source != SourceQA || got.GroupID != "group-1" || got.Provider != "openai" || got.Model != "test-model" {
		t.Fatalf("record attrs = %+v", got)
	}
	if got.PromptTokens != 10 || got.CompletionTokens != 5 || got.TotalTokens != 15 {
		t.Fatalf("record tokens = %+v", got)
	}
	if got.Status != StatusSucceeded || got.Error != "" {
		t.Fatalf("record status = %+v", got)
	}
}

func TestGenerateRecordsFailure(t *testing.T) {
	rec := &fakeRecorder{calls: make(chan Record, 1)}
	client := Wrap(&fakeModel{genErr: errors.New("boom")}, rec, "openai", "test-model")

	_, err := client.Generate(WithCall(context.Background(), SourceTaskExtract, "g"), nil)
	if err == nil {
		t.Fatal("expected error")
	}
	got := rec.last(t)
	if got.Status != StatusFailed || got.Error != "boom" {
		t.Fatalf("record = %+v", got)
	}
}

func TestStreamRecordsUsageFromFinalChunk(t *testing.T) {
	rec := &fakeRecorder{calls: make(chan Record, 1)}
	client := Wrap(&fakeModel{stream: []streamItem{
		{msg: &schema.Message{Content: "a"}},
		{msg: &schema.Message{Content: "b"}},
		{msg: usageMessage(20, 7)},
	}}, rec, "openai", "test-model")

	ctx := WithCall(context.Background(), SourceAiTask, "group-2")
	stream, err := client.Stream(ctx, nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	var text string
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		text += msg.Content
	}
	if text != "ab" {
		t.Fatalf("text = %q", text)
	}

	got := rec.last(t)
	if got.Source != SourceAiTask || got.GroupID != "group-2" || got.Status != StatusSucceeded {
		t.Fatalf("record = %+v", got)
	}
	if got.PromptTokens != 20 || got.CompletionTokens != 7 || got.TotalTokens != 27 {
		t.Fatalf("record tokens = %+v", got)
	}
}

func TestStreamRecordsError(t *testing.T) {
	rec := &fakeRecorder{calls: make(chan Record, 1)}
	client := Wrap(&fakeModel{stream: []streamItem{
		{msg: &schema.Message{Content: "a"}},
		{err: errors.New("stream broke")},
	}}, rec, "openai", "test-model")

	stream, err := client.Stream(WithCall(context.Background(), SourceRiskExtract, "g"), nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	for {
		_, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				t.Fatal("expected stream error, got EOF")
			}
			break
		}
	}

	got := rec.last(t)
	if got.Status != StatusFailed || got.Error != "stream broke" {
		t.Fatalf("record = %+v", got)
	}
}

func TestStreamInitialError(t *testing.T) {
	rec := &fakeRecorder{calls: make(chan Record, 1)}
	client := Wrap(&fakeModel{strmErr: errors.New("dial failed")}, rec, "openai", "test-model")

	_, err := client.Stream(WithCall(context.Background(), SourceQA, "g"), nil)
	if err == nil {
		t.Fatal("expected error")
	}
	got := rec.last(t)
	if got.Status != StatusFailed || got.Error != "dial failed" {
		t.Fatalf("record = %+v", got)
	}
}

func TestCallDefaultsToUnknownSource(t *testing.T) {
	rec := &fakeRecorder{calls: make(chan Record, 1)}
	client := Wrap(&fakeModel{genOut: &schema.Message{Content: "x"}}, rec, "openai", "m")

	if _, err := client.Generate(context.Background(), nil); err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := rec.last(t)
	if got.Source != "unknown" {
		t.Fatalf("source = %q", got.Source)
	}
}
