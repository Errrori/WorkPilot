package workflow

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Errrori/workpilot/internal/aitasks"
	"github.com/Errrori/workpilot/internal/store"
)

func TestUpstreamMaterialPrefersNearest(t *testing.T) {
	upstream := []NodeOutput{
		{Material: []store.RetrievedChunk{{ChunkIndex: 1}}},
		{Text: "answer"},
		{Material: []store.RetrievedChunk{{ChunkIndex: 2}}},
		{Text: "answer2"},
	}
	got := upstreamMaterial(upstream)
	if len(got) != 1 || got[0].ChunkIndex != 2 {
		t.Fatalf("material = %#v", got)
	}
	if got := upstreamMaterial([]NodeOutput{{Text: "x"}}); got != nil {
		t.Fatalf("material = %#v, want nil", got)
	}
}

type fakeTaskExtractor struct {
	tasks []store.Task
	err   error
}

func (f *fakeTaskExtractor) ExtractFromChunks(_ context.Context, _, _ string, _ []store.RetrievedChunk, _ int) ([]store.Task, error) {
	return f.tasks, f.err
}

type fakeReportGenerator struct {
	res  aitasks.ReportResult
	err  error
	last aitasks.ReportRequest
}

func (f *fakeReportGenerator) GenerateReport(_ context.Context, req aitasks.ReportRequest) (aitasks.ReportResult, error) {
	f.last = req
	return f.res, f.err
}

type fakeNodeNotifier struct {
	tasks    []store.Task
	reports  []store.Report
	messages []store.Message
}

func (n *fakeNodeNotifier) BroadcastTask(_ string, t *store.Task) {
	n.tasks = append(n.tasks, *t)
}

func (n *fakeNodeNotifier) BroadcastReport(_ string, r *store.Report) {
	n.reports = append(n.reports, *r)
}

func (n *fakeNodeNotifier) BroadcastMessage(m *store.Message) {
	n.messages = append(n.messages, *m)
}

func TestExtractTasksNodeBroadcastsSuggestions(t *testing.T) {
	notifier := &fakeNodeNotifier{}
	handler := &extractTasksHandler{
		extractor: &fakeTaskExtractor{tasks: []store.Task{{ID: 11, Title: "a"}, {ID: 12, Title: "b"}}},
		notifier:  notifier,
	}
	req := NodeRequest{
		Workflow: testWorkflow(),
		Node:     Node{ID: "t1", Type: NodeTypeExtractTasks, Config: json.RawMessage(`{"max":5}`)},
		Upstream: []NodeOutput{{Material: []store.RetrievedChunk{{Content: "m"}}}},
	}

	out, err := handler.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out.TaskIDs) != 2 || out.TaskIDs[0] != 11 || out.TaskIDs[1] != 12 {
		t.Fatalf("task ids = %v", out.TaskIDs)
	}
	if len(notifier.tasks) != 2 {
		t.Fatalf("broadcast tasks = %#v", notifier.tasks)
	}
}

func TestReportNodeBroadcastsReport(t *testing.T) {
	notifier := &fakeNodeNotifier{}
	message := store.Message{ID: 5, GroupID: "group-1", SenderName: store.SenderAI, Content: "announce"}
	generator := &fakeReportGenerator{res: aitasks.ReportResult{
		Report:  store.Report{ID: 7, GroupID: "group-1", Content: "body", Status: store.ReportStatusSucceeded},
		Message: &message,
	}}
	handler := &reportHandler{reports: generator, notifier: notifier}

	wf := testWorkflow()
	wf.Timezone = "UTC"
	out, err := handler.Run(context.Background(), NodeRequest{
		Workflow: wf,
		Run:      RunInfo{RunID: 3, Trigger: store.WorkflowTriggerManual, Actor: "alice"},
		Node:     Node{ID: "p1", Type: NodeTypeReport, Config: json.RawMessage(`{}`)},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.ReportID != 7 || out.Text != "body" {
		t.Fatalf("output = %+v", out)
	}
	if generator.last.Timezone != "UTC" || generator.last.Trigger != store.WorkflowTriggerManual || generator.last.Actor != "alice" {
		t.Fatalf("report request = %+v", generator.last)
	}
	if len(notifier.reports) != 1 || notifier.reports[0].ID != 7 {
		t.Fatalf("broadcast reports = %#v", notifier.reports)
	}
	if len(notifier.messages) != 1 || notifier.messages[0].Content != "announce" {
		t.Fatalf("broadcast messages = %#v", notifier.messages)
	}
}
