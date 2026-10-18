// Package tool registers the "tool" node kind: call a host tool.
//
// The call goes through the host's ToolInvoker, so the host's policy, approval
// and hook pipeline applies. This is the difference between a tool step and
// running curl inside a script step: the former stays on the enforcement plane.
package tool

import (
	"context"
	"fmt"
	"strings"

	"github.com/lengzhao/workflowkit"
)

func init() {
	workflowkit.Register("tool", New,
		workflowkit.WithDescription("Call a host tool. Arguments may contain templates; a leaf rendering to a JSON object or array is passed as that value."),
		workflowkit.WithConfig(Config{}),
		workflowkit.WithExample(map[string]any{
			"id":   "submit",
			"type": "tool",
			"tool": "ledger__import",
			"args": map[string]any{"path": "{{ .workflow.runDir }}/out.json"},
		}),
	)
}

// Config is the tool step's own fields.
type Config struct {
	// Tool is the tool's name as the host exposes it.
	Tool string `json:"tool" desc:"tool name, as the host exposes it"`
	// Args are the tool arguments. String leaves are rendered as templates; a
	// leaf that renders to a whole JSON object or array is passed as that value.
	Args map[string]any `json:"args,omitempty" desc:"tool arguments; string leaves may contain templates"`
}

// New builds a tool node.
func New(cfg Config) (workflowkit.Node, error) {
	if strings.TrimSpace(cfg.Tool) == "" {
		return nil, fmt.Errorf("tool: tool is required")
	}
	return &node{cfg: cfg}, nil
}

type node struct{ cfg Config }

func (n *node) Run(ctx context.Context, env workflowkit.Env, c *workflowkit.Ctx) (workflowkit.Output, error) {
	if env.Tools == nil {
		return workflowkit.Output{}, fmt.Errorf("tool step %q requires a Tools capability", c.StepID)
	}
	args := map[string]any{}
	if len(n.cfg.Args) > 0 {
		rendered, err := c.Values(n.cfg.Args)
		if err != nil {
			return workflowkit.Output{}, err
		}
		var ok bool
		args, ok = rendered.(map[string]any)
		if !ok {
			return workflowkit.Output{}, fmt.Errorf("tool step %q: args did not render to an object", c.StepID)
		}
	}
	result, err := env.Tools.Execute(ctx, workflowkit.ToolCall{
		ID:    c.StepID,
		Name:  n.cfg.Tool,
		Input: args,
	})
	if err != nil {
		return workflowkit.Output{Text: result.Content}, fmt.Errorf("tool step %q: %w", c.StepID, err)
	}
	return workflowkit.Output{Text: result.Content}, nil
}