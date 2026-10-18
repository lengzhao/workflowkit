// Package script registers the "script" node kind: run a shell command.
//
// The command is rendered from the step's run field and executed through the
// host's Shell capability, so the node sees raw stdout/stderr/exit code rather
// than parsing a model-facing text protocol. A non-zero exit code, a timeout, or
// a cancellation fails the step.
package script

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/lengzhao/workflowkit"
)

func init() {
	workflowkit.Register("script", New,
		workflowkit.WithDescription("Run a shell command and capture its output. Relative paths resolve against the definition file's directory."),
		workflowkit.WithConfig(Config{}),
		workflowkit.WithExample(map[string]any{
			"id":   "gen",
			"type": "script",
			"run":  "python3 scripts/gen.py {{ .inputs.date | quote }} --out {{ .workflow.runDir }}/out.json",
		}),
	)
}

// Config is the script step's own fields.
type Config struct {
	// Run is the shell command. It may contain templates and is executed in Dir.
	Run string `json:"run" desc:"shell command; may contain {{ }} templates"`
	// Dir is the working directory. Defaults to the definition file's directory.
	Dir string `json:"dir,omitempty" desc:"working directory; defaults to the definition file's directory"`
	// TimeoutSeconds is the command timeout, when the step does not set one.
	TimeoutSeconds *float64 `json:"timeoutSeconds,omitempty" desc:"command timeout in seconds"`
}

// New builds a script node. This is the constructor shape Register accepts:
// func(cfg) (Node, error).
func New(cfg Config) (workflowkit.Node, error) {
	if strings.TrimSpace(cfg.Run) == "" {
		return nil, fmt.Errorf("script: run is required")
	}
	return &node{cfg: cfg}, nil
}

type node struct{ cfg Config }

func (n *node) Run(ctx context.Context, env workflowkit.Env, c *workflowkit.Ctx) (workflowkit.Output, error) {
	if env.Shell == nil {
		return workflowkit.Output{}, fmt.Errorf("script step %q requires a Shell capability", c.StepID)
	}
	command, err := c.Render(n.cfg.Run)
	if err != nil {
		return workflowkit.Output{}, err
	}
	dir := n.cfg.Dir
	if strings.TrimSpace(dir) != "" {
		rendered, err := c.Render(dir)
		if err != nil {
			return workflowkit.Output{}, err
		}
		if !filepath.IsAbs(rendered) {
			rendered = filepath.Join(c.DefinitionDir(), rendered)
		}
		dir = rendered
	} else {
		dir = c.DefinitionDir()
	}
	shellCmd := command
	if dir != "" {
		shellCmd = fmt.Sprintf("cd %q && %s", dir, command)
	}
	timeout := n.cfg.TimeoutSeconds
	stdout, stderr, exitCode, err := env.Shell.Run(ctx, shellCmd, timeout)
	combined := stdout
	if strings.TrimSpace(stderr) != "" {
		combined = strings.TrimRight(combined, "\n") + "\n[stderr]\n" + stderr
	}
	if err != nil {
		return workflowkit.Output{Text: combined}, fmt.Errorf("script step %q: %w", c.StepID, err)
	}
	if exitCode != 0 {
		return workflowkit.Output{Text: combined},
			fmt.Errorf("script step %q: exit code %d: %s", c.StepID, exitCode, truncate(stderr+stdout))
	}
	return workflowkit.Output{Text: combined, Data: nil}, nil
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 400 {
		return s
	}
	return string([]rune(s)[:200]) + "..."
}