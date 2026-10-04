package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Errrori/workpilot/internal/store"
)

func chainDefinition() Definition {
	return Definition{
		Nodes: []Node{
			{ID: "r1", Type: NodeTypeRetrieve, Config: json.RawMessage(`{"query":"当前风险","top_k":3}`)},
			{ID: "a1", Type: NodeTypeAsk, Config: json.RawMessage(`{"prompt":"基于资料总结：{{r1}}"}`)},
			{ID: "t1", Type: NodeTypeExtractTasks, Config: json.RawMessage(`{"max":5}`)},
			{ID: "p1", Type: NodeTypeReport, Config: json.RawMessage(`{"title":"周报","sources":["tasks","risks"]}`)},
		},
		Edges: []Edge{
			{From: "r1", To: "a1"},
			{From: "a1", To: "t1"},
			{From: "t1", To: "p1"},
		},
	}
}

func TestValidateLinearChain(t *testing.T) {
	d := chainDefinition()
	if err := d.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	order := d.Chain()
	ids := make([]string, 0, len(order))
	for _, n := range order {
		ids = append(ids, n.ID)
	}
	if strings.Join(ids, ",") != "r1,a1,t1,p1" {
		t.Fatalf("chain = %v", ids)
	}
}

func TestParseDefinition(t *testing.T) {
	raw, err := json.Marshal(chainDefinition())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	d, err := ParseDefinition(raw)
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	if len(d.Nodes) != 4 {
		t.Fatalf("nodes = %d", len(d.Nodes))
	}
	if _, err := ParseDefinition(nil); err == nil {
		t.Fatal("expected error for empty definition")
	}
	if _, err := ParseDefinition([]byte(`{`)); err == nil {
		t.Fatal("expected error for invalid json")
	}
}

func TestValidateRejectsBadShapes(t *testing.T) {
	cases := map[string]Definition{
		"duplicate id": {
			Nodes: []Node{
				{ID: "n1", Type: NodeTypeRetrieve, Config: json.RawMessage(`{"query":"q"}`)},
				{ID: "n1", Type: NodeTypeAsk, Config: json.RawMessage(`{"prompt":"p"}`)},
			},
		},
		"unknown type": {
			Nodes: []Node{{ID: "n1", Type: "summarize"}},
		},
		"bad node id": {
			Nodes: []Node{{ID: "n 1", Type: NodeTypeReport}},
		},
		"unknown edge": {
			Nodes: []Node{{ID: "n1", Type: NodeTypeReport}},
			Edges: []Edge{{From: "n1", To: "nope"}},
		},
		"self loop": {
			Nodes: []Node{{ID: "n1", Type: NodeTypeReport}},
			Edges: []Edge{{From: "n1", To: "n1"}},
		},
		"fan out": {
			Nodes: []Node{
				{ID: "r1", Type: NodeTypeRetrieve, Config: json.RawMessage(`{"query":"q"}`)},
				{ID: "a1", Type: NodeTypeAsk, Config: json.RawMessage(`{"prompt":"p"}`)},
				{ID: "a2", Type: NodeTypeAsk, Config: json.RawMessage(`{"prompt":"p"}`)},
			},
			Edges: []Edge{{From: "r1", To: "a1"}, {From: "r1", To: "a2"}},
		},
		"fan in": {
			Nodes: []Node{
				{ID: "r1", Type: NodeTypeRetrieve, Config: json.RawMessage(`{"query":"q"}`)},
				{ID: "r2", Type: NodeTypeRetrieve, Config: json.RawMessage(`{"query":"q"}`)},
				{ID: "a1", Type: NodeTypeAsk, Config: json.RawMessage(`{"prompt":"p"}`)},
			},
			Edges: []Edge{{From: "r1", To: "a1"}, {From: "r2", To: "a1"}},
		},
		"two starts": {
			Nodes: []Node{
				{ID: "r1", Type: NodeTypeRetrieve, Config: json.RawMessage(`{"query":"q"}`)},
				{ID: "r2", Type: NodeTypeRetrieve, Config: json.RawMessage(`{"query":"q"}`)},
			},
		},
		"pure cycle": {
			Nodes: []Node{
				{ID: "a1", Type: NodeTypeAsk, Config: json.RawMessage(`{"prompt":"p"}`)},
				{ID: "a2", Type: NodeTypeAsk, Config: json.RawMessage(`{"prompt":"p"}`)},
			},
			Edges: []Edge{{From: "a1", To: "a2"}, {From: "a2", To: "a1"}},
		},
		"cycle plus path": {
			Nodes: []Node{
				{ID: "s1", Type: NodeTypeReport},
				{ID: "t1", Type: NodeTypeReport},
				{ID: "c1", Type: NodeTypeAsk, Config: json.RawMessage(`{"prompt":"p"}`)},
				{ID: "c2", Type: NodeTypeAsk, Config: json.RawMessage(`{"prompt":"p"}`)},
			},
			Edges: []Edge{{From: "s1", To: "t1"}, {From: "c1", To: "c2"}, {From: "c2", To: "c1"}},
		},
	}
	for name, d := range cases {
		if err := d.Validate(); err == nil {
			t.Fatalf("%s: expected validation error", name)
		}
	}
}

func TestValidateRejectsBadConfigs(t *testing.T) {
	cases := map[string]Definition{
		"retrieve without query": {
			Nodes: []Node{{ID: "r1", Type: NodeTypeRetrieve}},
		},
		"retrieve top_k too large": {
			Nodes: []Node{{ID: "r1", Type: NodeTypeRetrieve, Config: json.RawMessage(`{"query":"q","top_k":21}`)}},
		},
		"ask without prompt": {
			Nodes: []Node{{ID: "a1", Type: NodeTypeAsk}},
		},
		"extract max too large": {
			Nodes: []Node{{ID: "t1", Type: NodeTypeExtractTasks, Config: json.RawMessage(`{"max":51}`)}},
		},
		"report invalid source": {
			Nodes: []Node{{ID: "p1", Type: NodeTypeReport, Config: json.RawMessage(`{"sources":["emails"]}`)}},
		},
		"report bad lookback": {
			Nodes: []Node{{ID: "p1", Type: NodeTypeReport, Config: json.RawMessage(`{"lookback_days":91}`)}},
		},
		"broken config json": {
			Nodes: []Node{{ID: "r1", Type: NodeTypeRetrieve, Config: json.RawMessage(`{"query":`)}},
		},
	}
	for name, d := range cases {
		if err := d.Validate(); err == nil {
			t.Fatalf("%s: expected validation error", name)
		}
	}
}

func TestValidateTemplateRefs(t *testing.T) {
	d := chainDefinition()
	d.Nodes[1].Config = json.RawMessage(`{"prompt":"引用不存在：{{nope}}"}`)
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "not a previous step") {
		t.Fatalf("err = %v", err)
	}

	d = chainDefinition()
	d.Nodes[0].Config = json.RawMessage(`{"query":"前向引用：{{a1}}"}`)
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "not a previous step") {
		t.Fatalf("err = %v", err)
	}

	d = chainDefinition()
	d.Nodes[3].Config = json.RawMessage(`{"prompt":"报告参考：{{a1}}"}`)
	if err := d.Validate(); err != nil {
		t.Fatalf("report prompt refs should be allowed: %v", err)
	}
}

func TestReportConfigDefaultsSources(t *testing.T) {
	node := Node{ID: "p1", Type: NodeTypeReport}
	cfg, err := node.ReportConfig()
	if err != nil {
		t.Fatalf("ReportConfig: %v", err)
	}
	if len(cfg.Sources) != len(store.DefaultAiTaskSources()) {
		t.Fatalf("sources = %v", cfg.Sources)
	}
}
