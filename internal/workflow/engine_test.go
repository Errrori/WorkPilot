package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/Errrori/workpilot/internal/llmtrack"
	"github.com/Errrori/workpilot/internal/store"
)

type finishCall struct {
	id        int64
	status    string
	output    []byte
	errMsg    string
	tokens    int
	latencyMS int
}

type fakeRunStore struct {
	inserted []store.WorkflowNodeRunInsert
	starts   map[int64][]byte
	finished []finishCall
	skipped  []int64
	nextID   int64
}

func (s *fakeRunStore) InsertWorkflowNodeRun(_ context.Context, in store.WorkflowNodeRunInsert) (store.WorkflowNodeRun, error) {
	s.inserted = append(s.inserted, in)
	s.nextID++
	if s.starts == nil {
		s.starts = make(map[int64][]byte)
	}
	return store.WorkflowNodeRun{ID: s.nextID, RunID: in.RunID, NodeID: in.NodeID, Type: in.Type, Status: store.WorkflowNodeStatusPending}, nil
}

func (s *fakeRunStore) StartWorkflowNodeRun(_ context.Context, nodeRunID int64, input []byte) error {
	s.starts[nodeRunID] = input
	return nil
}

func (s *fakeRunStore) FinishWorkflowNodeRun(_ context.Context, nodeRunID int64, status string, output []byte, errMessage string, tokens, latencyMS int) error {
	s.finished = append(s.finished, finishCall{id: nodeRunID, status: status, output: output, errMsg: errMessage, tokens: tokens, latencyMS: latencyMS})
	return nil
}

func (s *fakeRunStore) SkipWorkflowNodeRun(_ context.Context, nodeRunID int64) error {
	s.skipped = append(s.skipped, nodeRunID)
	return nil
}

type fakeHandler struct {
	fn func(ctx context.Context, req NodeRequest) (NodeOutput, error)
}

func (h fakeHandler) Run(ctx context.Context, req NodeRequest) (NodeOutput, error) {
	return h.fn(ctx, req)
}

func askChainDefinition() Definition {
	return Definition{
		Nodes: []Node{
			{ID: "n1", Type: NodeTypeAsk, Config: json.RawMessage(`{"prompt":"p1"}`)},
			{ID: "n2", Type: NodeTypeAsk, Config: json.RawMessage(`{"prompt":"{{n1}}"}`)},
			{ID: "n3", Type: NodeTypeAsk, Config: json.RawMessage(`{"prompt":"{{n2}}"}`)},
		},
		Edges: []Edge{{From: "n1", To: "n2"}, {From: "n2", To: "n3"}},
	}
}

func testWorkflow() store.Workflow {
	return store.Workflow{ID: 1, GroupID: "group-1", Name: "迭代梳理"}
}

func TestEngineRunsChainInOrder(t *testing.T) {
	st := &fakeRunStore{}
	var calls []string
	var secondVars string
	handlers := map[string]Handler{
		NodeTypeAsk: fakeHandler{fn: func(_ context.Context, req NodeRequest) (NodeOutput, error) {
			calls = append(calls, req.Node.ID)
			if req.Node.ID == "n2" {
				secondVars = req.Vars["n1"]
			}
			return NodeOutput{Text: "out-" + req.Node.ID}, nil
		}},
	}
	engine := NewEngine(EngineConfig{Handlers: handlers, Store: st})

	if err := engine.Run(context.Background(), testWorkflow(), RunInfo{RunID: 9, Trigger: store.WorkflowTriggerManual}, askChainDefinition()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Join(calls, ",") != "n1,n2,n3" {
		t.Fatalf("calls = %v", calls)
	}
	if secondVars != "out-n1" {
		t.Fatalf("second node vars = %q", secondVars)
	}
	if len(st.inserted) != 3 || len(st.finished) != 3 || len(st.skipped) != 0 {
		t.Fatalf("inserted=%d finished=%d skipped=%d", len(st.inserted), len(st.finished), len(st.skipped))
	}
	for _, f := range st.finished {
		if f.status != store.WorkflowNodeStatusSucceeded {
			t.Fatalf("finished = %+v", f)
		}
		if f.latencyMS < 0 {
			t.Fatalf("latency = %d", f.latencyMS)
		}
	}
	if st.inserted[0].RunID != 9 || st.inserted[0].Type != NodeTypeAsk {
		t.Fatalf("inserted = %+v", st.inserted[0])
	}
}

type fakeBaseModel struct {
	out *schema.Message
	err error
}

func (m *fakeBaseModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return m.out, m.err
}

func (m *fakeBaseModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("not supported")
}

func TestEngineRecordsNodeTokens(t *testing.T) {
	client := llmtrack.Wrap(&fakeBaseModel{out: &schema.Message{
		Content: "x",
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{
			PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
		}},
	}}, nil, "openai", "test-model")

	st := &fakeRunStore{}
	handlers := map[string]Handler{
		NodeTypeAsk: fakeHandler{fn: func(ctx context.Context, _ NodeRequest) (NodeOutput, error) {
			if _, err := client.Generate(ctx, nil); err != nil {
				return NodeOutput{}, err
			}
			return NodeOutput{Text: "ok"}, nil
		}},
	}
	engine := NewEngine(EngineConfig{Handlers: handlers, Store: st})

	def := Definition{
		Nodes: []Node{{ID: "n1", Type: NodeTypeAsk, Config: json.RawMessage(`{"prompt":"p"}`)}},
	}
	if err := engine.Run(context.Background(), testWorkflow(), RunInfo{RunID: 1}, def); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(st.finished) != 1 || st.finished[0].tokens != 15 {
		t.Fatalf("finished = %+v", st.finished)
	}
}

func TestEngineStopsAtFailedNode(t *testing.T) {
	st := &fakeRunStore{}
	handlers := map[string]Handler{
		NodeTypeAsk: fakeHandler{fn: func(_ context.Context, req NodeRequest) (NodeOutput, error) {
			if req.Node.ID == "n2" {
				return NodeOutput{}, errors.New("llm down")
			}
			return NodeOutput{Text: "out-" + req.Node.ID}, nil
		}},
	}
	engine := NewEngine(EngineConfig{Handlers: handlers, Store: st})

	err := engine.Run(context.Background(), testWorkflow(), RunInfo{RunID: 1}, askChainDefinition())
	if err == nil || !strings.Contains(err.Error(), "node n2") || !strings.Contains(err.Error(), "llm down") {
		t.Fatalf("err = %v", err)
	}
	if len(st.finished) != 2 {
		t.Fatalf("finished = %+v", st.finished)
	}
	if st.finished[0].status != store.WorkflowNodeStatusSucceeded || st.finished[1].status != store.WorkflowNodeStatusFailed {
		t.Fatalf("finished = %+v", st.finished)
	}
	if !strings.Contains(st.finished[1].errMsg, "llm down") {
		t.Fatalf("failed node err = %q", st.finished[1].errMsg)
	}
	if len(st.skipped) != 1 || st.skipped[0] != 3 {
		t.Fatalf("skipped = %v", st.skipped)
	}
}

func TestEngineNodeTimeout(t *testing.T) {
	st := &fakeRunStore{}
	handlers := map[string]Handler{
		NodeTypeAsk: fakeHandler{fn: func(ctx context.Context, _ NodeRequest) (NodeOutput, error) {
			<-ctx.Done()
			return NodeOutput{}, ctx.Err()
		}},
	}
	engine := NewEngine(EngineConfig{Handlers: handlers, Store: st, NodeTimeout: 20 * time.Millisecond})

	def := Definition{
		Nodes: []Node{{ID: "n1", Type: NodeTypeAsk, Config: json.RawMessage(`{"prompt":"p"}`)}},
	}
	err := engine.Run(context.Background(), testWorkflow(), RunInfo{RunID: 1}, def)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if len(st.finished) != 1 || st.finished[0].status != store.WorkflowNodeStatusFailed {
		t.Fatalf("finished = %+v", st.finished)
	}
}

func TestEngineEmptyDefinition(t *testing.T) {
	engine := NewEngine(EngineConfig{Handlers: map[string]Handler{}, Store: &fakeRunStore{}})
	if err := engine.Run(context.Background(), testWorkflow(), RunInfo{RunID: 1}, Definition{}); err == nil {
		t.Fatal("expected error for empty definition")
	}
}
