package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Errrori/workpilot/internal/store"
)

// Node type names supported by the MVP engine.
const (
	NodeTypeRetrieve     = "retrieve"
	NodeTypeAsk          = "ask"
	NodeTypeExtractTasks = "extract_tasks"
	NodeTypeReport       = "report"
)

const (
	// MaxNodes bounds one workflow definition.
	MaxNodes = 20

	maxNodeIDRunes  = 32
	maxQueryRunes   = 2000
	maxPromptRunes  = 4000
	maxTitleRunes   = 200
	maxTopK         = 20
	maxExtractMax   = 50
	minLookbackDays = 1
	maxLookbackDays = 90
)

var nodeIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// IsValidNodeType reports whether the type is executable by the engine.
func IsValidNodeType(nodeType string) bool {
	switch nodeType {
	case NodeTypeRetrieve, NodeTypeAsk, NodeTypeExtractTasks, NodeTypeReport:
		return true
	default:
		return false
	}
}

// Definition is one stored workflow graph.
type Definition struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// Node is one step of a workflow.
type Node struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config,omitempty"`
}

// Edge points from one node to its direct successor.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// RetrieveConfig configures a vector retrieval step.
type RetrieveConfig struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k,omitempty"`
}

// AskConfig configures one custom-prompt LLM step.
type AskConfig struct {
	Prompt string `json:"prompt"`
}

// ExtractTasksConfig configures task extraction. When the step follows a
// retrieve step its chunks are used as material; otherwise the whole group is
// used, optionally limited to one file.
type ExtractTasksConfig struct {
	FileID int64 `json:"file_id,omitempty"`
	Max    int   `json:"max,omitempty"`
}

// ReportConfig configures one report step. Sources default to the standard
// report material set.
type ReportConfig struct {
	Title        string   `json:"title,omitempty"`
	Prompt       string   `json:"prompt,omitempty"`
	Sources      []string `json:"sources,omitempty"`
	LookbackDays int      `json:"lookback_days,omitempty"`
}

// ParseDefinition decodes and validates a stored definition.
func ParseDefinition(raw []byte) (Definition, error) {
	var d Definition
	if len(raw) == 0 {
		return d, errors.New("definition is required")
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return Definition{}, fmt.Errorf("decode definition: %w", err)
	}
	if err := d.Validate(); err != nil {
		return Definition{}, err
	}
	return d, nil
}

// Validate checks node ids and types, edges, the linear pipeline shape and
// every node config, including template references to previous steps.
func (d Definition) Validate() error {
	if len(d.Nodes) == 0 {
		return errors.New("definition must contain at least one node")
	}
	if len(d.Nodes) > MaxNodes {
		return fmt.Errorf("definition supports at most %d nodes", MaxNodes)
	}

	byID := make(map[string]Node, len(d.Nodes))
	for _, n := range d.Nodes {
		if n.ID == "" {
			return errors.New("node id is required")
		}
		if utf8.RuneCountInString(n.ID) > maxNodeIDRunes || !nodeIDPattern.MatchString(n.ID) {
			return fmt.Errorf("invalid node id %q", n.ID)
		}
		if _, ok := byID[n.ID]; ok {
			return fmt.Errorf("duplicate node id %q", n.ID)
		}
		if !IsValidNodeType(n.Type) {
			return fmt.Errorf("node %q has unsupported type %q", n.ID, n.Type)
		}
		byID[n.ID] = n
	}

	in := make(map[string]int, len(d.Nodes))
	out := make(map[string]int, len(d.Nodes))
	for _, e := range d.Edges {
		if _, ok := byID[e.From]; !ok {
			return fmt.Errorf("edge from unknown node %q", e.From)
		}
		if _, ok := byID[e.To]; !ok {
			return fmt.Errorf("edge to unknown node %q", e.To)
		}
		if e.From == e.To {
			return fmt.Errorf("node %q cannot link to itself", e.From)
		}
		in[e.To]++
		out[e.From]++
		if in[e.To] > 1 {
			return fmt.Errorf("node %q has multiple predecessors; only linear pipelines are supported", e.To)
		}
		if out[e.From] > 1 {
			return fmt.Errorf("node %q has multiple successors; only linear pipelines are supported", e.From)
		}
	}

	start := ""
	for _, n := range d.Nodes {
		if in[n.ID] == 0 {
			if start != "" {
				return errors.New("only one start node is supported")
			}
			start = n.ID
		}
	}
	if start == "" {
		return errors.New("workflow has no start node")
	}

	order, err := d.chain(byID, start)
	if err != nil {
		return err
	}
	if len(order) != len(d.Nodes) {
		return errors.New("workflow nodes must form one connected pipeline")
	}

	ancestors := make(map[string]bool, len(order))
	for _, n := range order {
		if err := validateNodeConfig(n, ancestors); err != nil {
			return err
		}
		ancestors[n.ID] = true
	}
	return nil
}

// Chain returns the linear execution order. It must only be called on a
// definition that passed Validate.
func (d Definition) Chain() []Node {
	byID := make(map[string]Node, len(d.Nodes))
	in := make(map[string]int, len(d.Nodes))
	for _, n := range d.Nodes {
		byID[n.ID] = n
	}
	for _, e := range d.Edges {
		in[e.To]++
	}
	start := ""
	for _, n := range d.Nodes {
		if in[n.ID] == 0 {
			start = n.ID
			break
		}
	}
	order, err := d.chain(byID, start)
	if err != nil {
		return nil
	}
	return order
}

func (d Definition) chain(byID map[string]Node, start string) ([]Node, error) {
	next := make(map[string]string, len(d.Edges))
	for _, e := range d.Edges {
		next[e.From] = e.To
	}
	seen := make(map[string]bool, len(d.Nodes))
	order := make([]Node, 0, len(d.Nodes))
	for id := start; id != ""; id = next[id] {
		if seen[id] {
			return nil, errors.New("workflow contains a cycle")
		}
		seen[id] = true
		order = append(order, byID[id])
	}
	return order, nil
}

// RetrieveConfig decodes the node config with defaults.
func (n Node) RetrieveConfig() (RetrieveConfig, error) {
	var cfg RetrieveConfig
	if err := decodeConfig(n, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// AskConfig decodes the node config.
func (n Node) AskConfig() (AskConfig, error) {
	var cfg AskConfig
	if err := decodeConfig(n, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// ExtractTasksConfig decodes the node config with defaults.
func (n Node) ExtractTasksConfig() (ExtractTasksConfig, error) {
	var cfg ExtractTasksConfig
	if err := decodeConfig(n, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// ReportConfig decodes the node config with defaults.
func (n Node) ReportConfig() (ReportConfig, error) {
	var cfg ReportConfig
	if err := decodeConfig(n, &cfg); err != nil {
		return cfg, err
	}
	if len(cfg.Sources) == 0 {
		cfg.Sources = store.DefaultAiTaskSources()
	}
	return cfg, nil
}

func decodeConfig(n Node, dest any) error {
	if len(n.Config) == 0 {
		return nil
	}
	if err := json.Unmarshal(n.Config, dest); err != nil {
		return fmt.Errorf("node %q config: %w", n.ID, err)
	}
	return nil
}

func validateNodeConfig(n Node, ancestors map[string]bool) error {
	switch n.Type {
	case NodeTypeRetrieve:
		cfg, err := n.RetrieveConfig()
		if err != nil {
			return err
		}
		if strings.TrimSpace(cfg.Query) == "" {
			return fmt.Errorf("retrieve node %q requires a query", n.ID)
		}
		if utf8.RuneCountInString(cfg.Query) > maxQueryRunes {
			return fmt.Errorf("retrieve node %q query exceeds %d characters", n.ID, maxQueryRunes)
		}
		if cfg.TopK < 0 || cfg.TopK > maxTopK {
			return fmt.Errorf("retrieve node %q top_k must be between 0 and %d", n.ID, maxTopK)
		}
		return validateTemplateRefs(n.ID, cfg.Query, ancestors)
	case NodeTypeAsk:
		cfg, err := n.AskConfig()
		if err != nil {
			return err
		}
		if strings.TrimSpace(cfg.Prompt) == "" {
			return fmt.Errorf("ask node %q requires a prompt", n.ID)
		}
		if utf8.RuneCountInString(cfg.Prompt) > maxPromptRunes {
			return fmt.Errorf("ask node %q prompt exceeds %d characters", n.ID, maxPromptRunes)
		}
		return validateTemplateRefs(n.ID, cfg.Prompt, ancestors)
	case NodeTypeExtractTasks:
		cfg, err := n.ExtractTasksConfig()
		if err != nil {
			return err
		}
		if cfg.FileID < 0 {
			return fmt.Errorf("extract_tasks node %q file_id must be positive", n.ID)
		}
		if cfg.Max < 0 || cfg.Max > maxExtractMax {
			return fmt.Errorf("extract_tasks node %q max must be between 1 and %d", n.ID, maxExtractMax)
		}
		return nil
	case NodeTypeReport:
		cfg, err := n.ReportConfig()
		if err != nil {
			return err
		}
		if utf8.RuneCountInString(cfg.Title) > maxTitleRunes {
			return fmt.Errorf("report node %q title exceeds %d characters", n.ID, maxTitleRunes)
		}
		if utf8.RuneCountInString(cfg.Prompt) > maxPromptRunes {
			return fmt.Errorf("report node %q prompt exceeds %d characters", n.ID, maxPromptRunes)
		}
		for _, source := range cfg.Sources {
			if !store.IsValidAiTaskSource(source) {
				return fmt.Errorf("report node %q has invalid source %q", n.ID, source)
			}
		}
		if cfg.LookbackDays != 0 && (cfg.LookbackDays < minLookbackDays || cfg.LookbackDays > maxLookbackDays) {
			return fmt.Errorf("report node %q lookback_days must be between %d and %d", n.ID, minLookbackDays, maxLookbackDays)
		}
		return validateTemplateRefs(n.ID, cfg.Prompt, ancestors)
	}
	return nil
}

func validateTemplateRefs(nodeID, tpl string, ancestors map[string]bool) error {
	for _, ref := range TemplateRefs(tpl) {
		if !ancestors[ref] {
			return fmt.Errorf("node %q references %q, which is not a previous step", nodeID, ref)
		}
	}
	return nil
}
