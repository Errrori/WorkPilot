package qa

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
)

type ModelConfig struct {
	Provider string
	BaseURL  string
	Model    string
	APIKey   string
	Timeout  time.Duration
}

// NewChatModel builds the configured OpenAI-compatible chat model.
func NewChatModel(ctx context.Context, cfg ModelConfig) (model.BaseChatModel, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "", "openai":
		if cfg.Model == "" {
			return nil, fmt.Errorf("llm model is required")
		}
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("llm api key is required")
		}
		return openai.NewChatModel(ctx, &openai.ChatModelConfig{
			BaseURL: cfg.BaseURL,
			APIKey:  cfg.APIKey,
			Model:   cfg.Model,
			Timeout: cfg.Timeout,
		})
	default:
		return nil, fmt.Errorf("unsupported llm provider %q", cfg.Provider)
	}
}
