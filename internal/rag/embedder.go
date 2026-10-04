package rag

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/embedding/ollama"
	"github.com/cloudwego/eino/components/embedding"
)

type EmbedderConfig struct {
	Provider string
	BaseURL  string
	Model    string
	Timeout  time.Duration
	// KeepAlive keeps the model resident in Ollama between calls, avoiding the
	// reload cost that otherwise shows up as tail latency.
	KeepAlive time.Duration
}

// NewEmbedder builds the configured embedding component.
func NewEmbedder(ctx context.Context, cfg EmbedderConfig) (embedding.Embedder, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "", "ollama":
		if cfg.Model == "" {
			return nil, fmt.Errorf("embedding model is required")
		}
		conf := &ollama.EmbeddingConfig{
			BaseURL: cfg.BaseURL,
			Model:   cfg.Model,
			Timeout: cfg.Timeout,
		}
		if cfg.KeepAlive > 0 {
			conf.KeepAlive = &cfg.KeepAlive
		}
		return ollama.NewEmbedder(ctx, conf)
	default:
		return nil, fmt.Errorf("unsupported embedding provider %q", cfg.Provider)
	}
}

// Warmup forces the embedding model to load into memory so the first real
// request does not pay the cold-start cost. It is best-effort: callers should
// log failures instead of aborting startup, since indexing will retry anyway.
func Warmup(ctx context.Context, embedder embedding.Embedder, dim int) (time.Duration, error) {
	if embedder == nil {
		return 0, fmt.Errorf("embedder is required")
	}
	start := time.Now()
	vectors, err := embedder.EmbedStrings(ctx, []string{"warmup"})
	duration := time.Since(start)
	if err != nil {
		return duration, fmt.Errorf("warm up embedder: %w", err)
	}
	if len(vectors) != 1 {
		return duration, fmt.Errorf("warmup returned %d vectors, want 1", len(vectors))
	}
	if dim > 0 && len(vectors[0]) != dim {
		return duration, fmt.Errorf("warmup embedding dimension %d, want %d", len(vectors[0]), dim)
	}
	return duration, nil
}
