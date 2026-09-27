package httpapi

import (
	"testing"

	"github.com/Errrori/workpilot/internal/store"
)

func TestNormalizeAiTaskSourcesDefaultsToAll(t *testing.T) {
	sources, ok := normalizeAiTaskSources(nil)
	if !ok {
		t.Fatal("expected ok")
	}
	want := []string{store.AiTaskSourceMessages, store.AiTaskSourceTasks, store.AiTaskSourceRisks, store.AiTaskSourceFiles}
	if len(sources) != len(want) {
		t.Fatalf("sources = %#v", sources)
	}
	for i, source := range want {
		if sources[i] != source {
			t.Fatalf("sources[%d] = %q, want %q", i, sources[i], source)
		}
	}
}

func TestNormalizeAiTaskSourcesDedupes(t *testing.T) {
	sources, ok := normalizeAiTaskSources([]string{"Files", " messages ", "files"})
	if !ok {
		t.Fatal("expected ok")
	}
	if len(sources) != 2 || sources[0] != store.AiTaskSourceFiles || sources[1] != store.AiTaskSourceMessages {
		t.Fatalf("sources = %#v", sources)
	}
}

func TestNormalizeAiTaskSourcesRejectsUnknown(t *testing.T) {
	for _, input := range [][]string{{"chunks"}, {""}, {"messages", "unknown"}} {
		if _, ok := normalizeAiTaskSources(input); ok {
			t.Fatalf("input %#v: expected rejection", input)
		}
	}
}
