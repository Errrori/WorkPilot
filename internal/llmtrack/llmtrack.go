// Package llmtrack decorates the Eino chat model and records per-call token
// usage, latency and failures for cost observability.
package llmtrack

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Call sources recorded in llm_usage.source.
const (
	SourceQA          = "qa"
	SourceTaskExtract = "task_extract"
	SourceRiskExtract = "risk_extract"
	SourceAiTask      = "ai_task"
)

// Record statuses.
const (
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

const (
	maxErrorRunes = 500
	recordTimeout = 3 * time.Second
)

// Record is one tracked model call.
type Record struct {
	GroupID          string
	Source           string
	Provider         string
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	LatencyMS        int
	Status           string
	Error            string
}

// Recorder persists usage records.
type Recorder interface {
	InsertLLMUsage(ctx context.Context, r Record) error
}

// Client wraps a base chat model and records every call.
type Client struct {
	base     model.BaseChatModel
	recorder Recorder
	provider string
	model    string
}

var _ model.BaseChatModel = (*Client)(nil)

// Wrap decorates base with usage recording. provider and modelName are stored
// on every record.
func Wrap(base model.BaseChatModel, recorder Recorder, provider, modelName string) *Client {
	return &Client{base: base, recorder: recorder, provider: provider, model: modelName}
}

type callKey struct{}

type callInfo struct {
	source  string
	groupID string
}

// WithCall marks ctx with the source and group of the upcoming model call.
func WithCall(ctx context.Context, source, groupID string) context.Context {
	return context.WithValue(ctx, callKey{}, callInfo{source: source, groupID: groupID})
}

func callFrom(ctx context.Context) callInfo {
	info, _ := ctx.Value(callKey{}).(callInfo)
	if info.source == "" {
		info.source = "unknown"
	}
	return info
}

// Generate implements model.BaseChatModel.
func (c *Client) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	start := time.Now()
	out, err := c.base.Generate(ctx, input, opts...)

	rec := c.newRecord(ctx, start)
	if err != nil {
		rec.Status = StatusFailed
		rec.Error = truncateRunes(err.Error(), maxErrorRunes)
	} else if out != nil && out.ResponseMeta != nil {
		fillUsage(&rec, out.ResponseMeta.Usage)
	}
	c.record(ctx, rec)
	return out, err
}

// Stream implements model.BaseChatModel. Chunks are relayed as they arrive;
// the usage carried by the final chunk is recorded once the stream ends.
func (c *Client) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	start := time.Now()
	src, err := c.base.Stream(ctx, input, opts...)
	if err != nil {
		rec := c.newRecord(ctx, start)
		rec.Status = StatusFailed
		rec.Error = truncateRunes(err.Error(), maxErrorRunes)
		c.record(ctx, rec)
		return nil, err
	}

	out, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer src.Close()

		var usage *schema.TokenUsage
		finish := func(reason error) {
			rec := c.newRecord(ctx, start)
			if reason != nil {
				rec.Status = StatusFailed
				rec.Error = truncateRunes(reason.Error(), maxErrorRunes)
			}
			fillUsage(&rec, usage)
			c.record(ctx, rec)
		}

		for {
			msg, recvErr := src.Recv()
			if recvErr != nil {
				if !errors.Is(recvErr, io.EOF) {
					finish(recvErr)
					_ = sw.Send(nil, recvErr)
				} else {
					finish(nil)
				}
				sw.Close()
				return
			}
			if msg != nil && msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil {
				usage = msg.ResponseMeta.Usage
			}
			if closed := sw.Send(msg, nil); closed {
				finish(errors.New("stream closed by consumer"))
				return
			}
		}
	}()
	return out, nil
}

func (c *Client) newRecord(ctx context.Context, start time.Time) Record {
	info := callFrom(ctx)
	return Record{
		GroupID:   info.groupID,
		Source:    info.source,
		Provider:  c.provider,
		Model:     c.model,
		LatencyMS: int(time.Since(start).Milliseconds()),
		Status:    StatusSucceeded,
	}
}

func (c *Client) record(ctx context.Context, rec Record) {
	slog.Debug("llm call",
		"source", rec.Source,
		"group_id", rec.GroupID,
		"model", rec.Model,
		"status", rec.Status,
		"prompt_tokens", rec.PromptTokens,
		"completion_tokens", rec.CompletionTokens,
		"latency_ms", rec.LatencyMS,
	)
	if c.recorder == nil {
		return
	}
	// Usage must be recorded even when the request context was canceled.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := c.recorder.InsertLLMUsage(ctx, rec); err != nil {
		slog.Warn("record llm usage", "source", rec.Source, "error", err)
	}
}

func fillUsage(rec *Record, usage *schema.TokenUsage) {
	if usage == nil {
		return
	}
	rec.PromptTokens = usage.PromptTokens
	rec.CompletionTokens = usage.CompletionTokens
	rec.TotalTokens = usage.TotalTokens
	if rec.TotalTokens == 0 {
		rec.TotalTokens = rec.PromptTokens + rec.CompletionTokens
	}
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
