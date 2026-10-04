package httpapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Errrori/workpilot/internal/workflow"
)

func TestNextWorkflowRun(t *testing.T) {
	now := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)

	next, err := nextWorkflowRun("", "UTC", now)
	if err != nil || !next.Equal(now) {
		t.Fatalf("manual next = %v, %v", next, err)
	}

	next, err = nextWorkflowRun("*/5 * * * *", "UTC", now)
	if err != nil {
		t.Fatalf("cron next: %v", err)
	}
	if !next.After(now) {
		t.Fatalf("cron next = %v, want after %v", next, now)
	}

	if _, err := nextWorkflowRun("bad cron", "UTC", now); err == nil {
		t.Fatal("expected error for invalid cron")
	}
	if _, err := nextWorkflowRun("*/5 * * * *", "Not/AZone", now); err == nil {
		t.Fatal("expected error for invalid timezone")
	}
	if _, err := nextWorkflowRun("", "Not/AZone", now); err == nil {
		t.Fatal("expected error for invalid timezone without schedule")
	}
}

func TestNormalizeWorkflowDefinition(t *testing.T) {
	raw := json.RawMessage(`{"nodes":[{"id":"n1","type":"report","config":{}}],"edges":[]}`)
	out, err := normalizeWorkflowDefinition(raw)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var def workflow.Definition
	if err := json.Unmarshal(out, &def); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(def.Nodes) != 1 || def.Nodes[0].ID != "n1" {
		t.Fatalf("definition = %#v", def)
	}

	for name, input := range map[string]json.RawMessage{
		"missing": nil,
		"null":    json.RawMessage(`null`),
		"empty":   json.RawMessage(`{"nodes":[]}`),
		"cycle":   json.RawMessage(`{"nodes":[{"id":"a","type":"report"},{"id":"b","type":"report"}],"edges":[{"from":"a","to":"b"},{"from":"b","to":"a"}]}`),
	} {
		if _, err := normalizeWorkflowDefinition(input); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}
