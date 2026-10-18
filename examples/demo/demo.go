// Package demo is a minimal host implementation used by the examples. It is
// deliberately small: it shows what a host has to provide, and nothing more.
//
// A real host replaces each piece with its own: a sandboxed shell, a tool
// runtime that enforces policy, an LLM client, a subagent spawner, and a
// workspace-backed artifact directory.
package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lengzhao/workflowkit"
)

// Shell runs commands with /bin/sh. It is the shell step's capability.
type Shell struct{}

// Run implements workflowkit.Shell: raw stdout/stderr/exit code, no text
// protocol to parse.
func (Shell) Run(ctx context.Context, command string, timeoutSeconds *float64) (string, string, int, error) {
	runCtx := ctx
	if timeoutSeconds != nil && *timeoutSeconds > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(*timeoutSeconds)*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(runCtx, "/bin/sh", "-c", command)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if runCtx.Err() != nil {
		return stdout.String(), stderr.String(), -1, runCtx.Err()
	}
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return stdout.String(), stderr.String(), exit.ExitCode(), nil
		}
		return stdout.String(), stderr.String(), -1, err
	}
	return stdout.String(), stderr.String(), 0, nil
}

// Tool is one in-memory tool.
type Tool struct {
	Name   string
	Output string
	Fn     func(input map[string]any) (string, error)
}

// Tools is a tiny tool registry that records the calls it receives, so an
// example can print what the pipeline passed to it.
type Tools struct {
	Registry map[string]Tool

	mu    sync.Mutex
	calls []workflowkit.ToolCall
}

// Execute implements workflowkit.ToolInvoker. A real host would run its
// policy, approval and hook pipeline here — which is exactly why a tool step
// should be used instead of curling from inside a script.
func (t *Tools) Execute(ctx context.Context, call workflowkit.ToolCall) (workflowkit.ToolResult, error) {
	t.mu.Lock()
	t.calls = append(t.calls, call)
	t.mu.Unlock()
	tool, ok := t.Registry[call.Name]
	if !ok {
		return workflowkit.ToolResult{}, fmt.Errorf("no such tool: %s", call.Name)
	}
	if tool.Fn != nil {
		out, err := tool.Fn(call.Input)
		if err != nil {
			return workflowkit.ToolResult{}, err
		}
		return workflowkit.ToolResult{Content: out}, nil
	}
	return workflowkit.ToolResult{Content: tool.Output}, nil
}

// Calls returns the recorded calls.
func (t *Tools) Calls() []workflowkit.ToolCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]workflowkit.ToolCall, len(t.calls))
	copy(out, t.calls)
	return out
}

// Catalog lists the tools for a model choosing tools.
func (t *Tools) ListTools(ctx context.Context) ([]workflowkit.ToolInfo, error) {
	var out []workflowkit.ToolInfo
	for name := range t.Registry {
		out = append(out, workflowkit.ToolInfo{
			Name:        name,
			Description: "demo tool " + name,
			InputSchema: map[string]any{"type": "object"},
		})
	}
	return out, nil
}

// LLM answers with a fixed reply. A real Completer calls a model provider.
type LLM struct{ Reply string }

// Complete implements workflowkit.Completer.
func (l LLM) Complete(ctx context.Context, model, prompt string, schema map[string]any) (string, error) {
	if l.Reply != "" {
		return l.Reply, nil
	}
	return `{"ok":true,"reason":"demo reply"}`, nil
}

// Agents returns a fixed summary. A real AgentRunner spawns a child agent.
type Agents struct{ Summary string }

// Run implements workflowkit.AgentRunner.
func (a Agents) Run(ctx context.Context, agent, task string, timeoutSeconds *int) (workflowkit.Summary, error) {
	if a.Summary == "" {
		return workflowkit.Summary{Text: "nothing to report", Session: "demo-session", Status: "completed"}, nil
	}
	return workflowkit.Summary{Text: a.Summary, Session: "demo-session", Status: "completed", Steps: 2}, nil
}

// Files puts run artifacts under a root directory. A real host would resolve
// its workspace (for AgentKit: work/workflow/<runID>).
type Files struct{ Root string }

// RunDir implements workflowkit.Files.
func (f Files) RunDir(runID string) (string, error) {
	if f.Root == "" {
		f.Root = filepath.Join(os.TempDir(), "workflowkit-examples")
	}
	dir := filepath.Join(f.Root, runID)
	return dir, os.MkdirAll(dir, 0o755)
}

// Env assembles a complete environment. Every field is optional — an
// environment that does not use a capability can leave it nil.
func Env(root string) workflowkit.Env {
	tools := &Tools{Registry: map[string]Tool{
		"ledger__import": {Name: "ledger__import", Fn: func(input map[string]any) (string, error) {
			return `{"status":"imported","rows":128}`, nil
		}},
		"store": {Name: "store", Fn: func(input map[string]any) (string, error) {
			return fmt.Sprintf(`{"stored":%s}`, mustJSON(input)), nil
		}},
	}}
	return workflowkit.Env{
		Shell:   Shell{},
		Tools:   tools,
		LLM:     LLM{},
		Agents:  Agents{},
		Files:   Files{Root: root},
		Catalog: tools,
	}
}

// mustJSON renders a tool input as compact JSON, keeping the demo free of an
// encoding/json import for one call site.
func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
