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
}

// NewEmbedder builds the configured embedding component.
func NewEmbedder(ctx context.Context, cfg EmbedderConfig) (embedding.Embedder, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "", "ollama":
		if cfg.Model == "" {
			return nil, fmt.Errorf("embedding model is required")
		}
		return ollama.NewEmbedder(ctx, &ollama.EmbeddingConfig{
			BaseURL: cfg.BaseURL,
			Model:   cfg.Model,
			Timeout: cfg.Timeout,
		})
	default:
		return nil, fmt.Errorf("unsupported embedding provider %q", cfg.Provider)
	}
}
