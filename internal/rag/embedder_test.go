package rag

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestWarmupEmbedsOnce(t *testing.T) {
	emb := &fakeEmbedder{dim: 3}

	if _, err := Warmup(context.Background(), emb, 3); err != nil {
		t.Fatalf("Warmup error = %v", err)
	}
	if emb.calls != 1 {
		t.Fatalf("embed calls = %d, want 1", emb.calls)
	}
}

func TestWarmupDimensionMismatch(t *testing.T) {
	emb := &fakeEmbedder{dim: 4}

	_, err := Warmup(context.Background(), emb, 3)
	if err == nil || !strings.Contains(err.Error(), "dimension") {
		t.Fatalf("err = %v, want dimension mismatch", err)
	}
}

func TestWarmupPropagatesEmbedderError(t *testing.T) {
	emb := &fakeEmbedder{dim: 3, err: errors.New("connection refused")}

	if _, err := Warmup(context.Background(), emb, 3); err == nil {
		t.Fatal("expected error")
	}
}

func TestWarmupRejectsNilEmbedder(t *testing.T) {
	if _, err := Warmup(context.Background(), nil, 3); err == nil {
		t.Fatal("expected error for nil embedder")
	}
}

func TestNewEmbedderRequiresModel(t *testing.T) {
	if _, err := NewEmbedder(context.Background(), EmbedderConfig{Provider: "ollama"}); err == nil {
		t.Fatal("expected error for missing model")
	}
}

func TestNewEmbedderRejectsUnknownProvider(t *testing.T) {
	if _, err := NewEmbedder(context.Background(), EmbedderConfig{Provider: "banana", Model: "x"}); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}
