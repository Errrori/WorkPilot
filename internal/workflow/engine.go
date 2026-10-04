package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Errrori/workpilot/internal/llmtrack"
	"github.com/Errrori/workpilot/internal/store"
)

const (
	// DefaultNodeTimeout bounds one node execution.
	DefaultNodeTimeout = 120 * time.Second
	// DefaultRunTimeout bounds one whole run.
	DefaultRunTimeout = 15 * time.Minute

	recordTimeout  = 5 * time.Second
	maxErrorRunes  = 500
	maxOutputRunes = 20000
)

// RunInfo carries the run-level metadata nodes may need.
type RunInfo struct {
	RunID     int64
	Trigger   string
	Actor     string
	LastRunAt *time.Time
}

// NodeRequest is one step execution request.
type NodeRequest struct {
	Workflow store.Workflow
	Run      RunInfo
	Node     Node
	Upstream []NodeOutput
	Vars     map[string]string
}

// NodeOutput is the result of one executed step.
type NodeOutput struct {
	Text      string
	Citations []store.Citation
	Material  []store.RetrievedChunk
	ReportID  int64
	TaskIDs   []int64
}

// Handler executes one node type.
type Handler interface {
	Run(ctx context.Context, req NodeRequest) (NodeOutput, error)
}

// RunStore persists workflow execution progress.
type RunStore interface {
	InsertWorkflowNodeRun(ctx context.Context, in store.WorkflowNodeRunInsert) (store.WorkflowNodeRun, error)
	StartWorkflowNodeRun(ctx context.Context, nodeRunID int64, input []byte) error
	FinishWorkflowNodeRun(ctx context.Context, nodeRunID int64, status string, output []byte, errMessage string, tokens, latencyMS int) error
	SkipWorkflowNodeRun(ctx context.Context, nodeRunID int64) error
}

// EngineConfig wires the node handlers and run records.
type EngineConfig struct {
	Handlers    map[string]Handler
	Store       RunStore
	NodeTimeout time.Duration
	RunTimeout  time.Duration
}

// Engine executes a validated definition step by step.
type Engine struct {
	cfg EngineConfig
}

func NewEngine(cfg EngineConfig) *Engine {
	if cfg.NodeTimeout <= 0 {
		cfg.NodeTimeout = DefaultNodeTimeout
	}
	if cfg.RunTimeout <= 0 {
		cfg.RunTimeout = DefaultRunTimeout
	}
	return &Engine{cfg: cfg}
}

type outputRecord struct {
	Text      string           `json:"text,omitempty"`
	Citations []store.Citation `json:"citations,omitempty"`
	ReportID  int64            `json:"report_id,omitempty"`
	TaskIDs   []int64          `json:"task_ids,omitempty"`
}

// Run executes the definition in chain order, recording one node run per step.
// The first failing node stops the run and marks the remaining nodes skipped.
func (e *Engine) Run(ctx context.Context, wf store.Workflow, info RunInfo, def Definition) error {
	order := def.Chain()
	if len(order) == 0 {
		return errors.New("workflow definition is empty")
	}
	if e.cfg.Store == nil {
		return errors.New("workflow run store is not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, e.cfg.RunTimeout)
	defer cancel()

	nodes := make(map[string]store.WorkflowNodeRun, len(order))
	for _, n := range order {
		row, err := e.cfg.Store.InsertWorkflowNodeRun(ctx, store.WorkflowNodeRunInsert{RunID: info.RunID, NodeID: n.ID, Type: n.Type})
		if err != nil {
			return fmt.Errorf("create node run %s: %w", n.ID, err)
		}
		nodes[n.ID] = row
	}

	var upstream []NodeOutput
	vars := make(map[string]string, len(order))
	for i, n := range order {
		handler, ok := e.cfg.Handlers[n.Type]
		if !ok {
			runErr := fmt.Errorf("unsupported node type %q", n.Type)
			e.stop(order, nodes, i, runErr, 0, 0, NodeOutput{})
			return fmt.Errorf("node %s: %w", n.ID, runErr)
		}

		req := NodeRequest{
			Workflow: wf,
			Run:      info,
			Node:     n,
			Upstream: upstream,
			Vars:     vars,
		}

		if err := e.cfg.Store.StartWorkflowNodeRun(ctx, nodes[n.ID].ID, encodeInput(upstream)); err != nil {
			slog.Warn("start workflow node run", "run_id", info.RunID, "node_id", n.ID, "error", err)
		}

		nodeCtx, cancelNode := context.WithTimeout(ctx, e.cfg.NodeTimeout)
		nodeCtx, collector := llmtrack.WithCollector(nodeCtx)
		start := time.Now()
		out, runErr := handler.Run(nodeCtx, req)
		latency := int(time.Since(start).Milliseconds())
		usage := collector.Usage()
		cancelNode()

		if runErr != nil {
			e.stop(order, nodes, i, runErr, usage.TotalTokens, latency, out)
			return fmt.Errorf("node %s: %w", n.ID, runErr)
		}

		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
		if err := e.cfg.Store.FinishWorkflowNodeRun(rctx, nodes[n.ID].ID, store.WorkflowNodeStatusSucceeded, encodeOutput(out), "", usage.TotalTokens, latency); err != nil {
			slog.Warn("finish workflow node run", "run_id", info.RunID, "node_id", n.ID, "error", err)
		}
		rcancel()

		vars[n.ID] = out.Text
		upstream = append(upstream, out)
	}
	return nil
}

// stop records the failing node and marks every later node skipped.
func (e *Engine) stop(order []Node, nodes map[string]store.WorkflowNodeRun, failed int, runErr error, tokens, latency int, out NodeOutput) {
	rctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()

	errMessage := truncateRunes(runErr.Error(), maxErrorRunes)
	if err := e.cfg.Store.FinishWorkflowNodeRun(rctx, nodes[order[failed].ID].ID, store.WorkflowNodeStatusFailed, encodeOutput(out), errMessage, tokens, latency); err != nil {
		slog.Warn("finish failed workflow node run", "node_id", order[failed].ID, "error", err)
	}
	for _, rest := range order[failed+1:] {
		if err := e.cfg.Store.SkipWorkflowNodeRun(rctx, nodes[rest.ID].ID); err != nil {
			slog.Warn("skip workflow node run", "node_id", rest.ID, "error", err)
		}
	}
}

func encodeInput(upstream []NodeOutput) []byte {
	if len(upstream) == 0 {
		return nil
	}
	raw, err := json.Marshal(map[string]string{
		"upstream_text": truncateRunes(upstream[len(upstream)-1].Text, maxOutputRunes),
	})
	if err != nil {
		return nil
	}
	return raw
}

func encodeOutput(out NodeOutput) []byte {
	raw, err := json.Marshal(outputRecord{
		Text:      truncateRunes(out.Text, maxOutputRunes),
		Citations: out.Citations,
		ReportID:  out.ReportID,
		TaskIDs:   out.TaskIDs,
	})
	if err != nil {
		return nil
	}
	return raw
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
