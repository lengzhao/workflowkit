// Package agent registers the "agent" node kind: delegate to a child agent.
//
// The delegation is synchronous — the pipeline waits for the child's summary,
// because a follow-up turn arriving later is not pipeline semantics. The task
// must be self-contained: the child starts with an empty session, so any fact it
// needs has to be in the task. For large data, pass a path and let the child
// read it in its own context; only the summary crosses back.
package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/lengzhao/workflowkit"
)

func init() {
	workflowkit.Register("agent", New,
		workflowkit.WithDescription("Delegate a self-contained task to a child agent and wait for its summary."),
		workflowkit.WithConfig(Config{}),
		workflowkit.WithExample(map[string]any{
			"id":    "audit",
			"type":  "agent",
			"agent": "auditor",
			"task":  "Audit {{ .workflow.runDir }}/out.json and list the problems you find.",
		}),
	)
}

// Config is the agent step's own fields.
type Config struct {
	// Agent names the child agent definition.
	Agent string `json:"agent" desc:"child agent name"`
	// Task is the self-contained instruction. It may contain templates.
	Task string `json:"task" desc:"self-contained task for the child; may contain {{ }} templates"`
}

// New builds an agent node.
func New(cfg Config) (workflowkit.Node, error) {
	if strings.TrimSpace(cfg.Agent) == "" {
		return nil, fmt.Errorf("agent: agent is required")
	}
	if strings.TrimSpace(cfg.Task) == "" {
		return nil, fmt.Errorf("agent: task is required")
	}
	return &node{cfg: cfg}, nil
}

type node struct{ cfg Config }

func (n *node) Run(ctx context.Context, env workflowkit.Env, c *workflowkit.Ctx) (workflowkit.Output, error) {
	if env.Agents == nil {
		return workflowkit.Output{}, fmt.Errorf("agent step %q requires an Agents capability", c.StepID)
	}
	task, err := c.Render(n.cfg.Task)
	if err != nil {
		return workflowkit.Output{}, err
	}
	summary, err := env.Agents.Run(ctx, n.cfg.Agent, task, nil)
	if err != nil {
		return workflowkit.Output{}, fmt.Errorf("agent step %q: %w", c.StepID, err)
	}
	// The delegation's metadata goes into Data so a downstream step can read it
	// with dig, for example: {{ dig .steps.audit.json "session" }}
	return workflowkit.Output{Text: summary.Text, Data: map[string]any{
		"session": summary.Session,
		"status":  summary.Status,
		"steps":   summary.Steps,
	}}, nil
}
