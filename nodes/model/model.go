// Package model registers the "model" node kind: one-shot model completion.
//
// A model step is a function call to a model, not an agent turn: no tools, no
// session, no side effects. Use it for judging, classifying and reformatting.
// With outputSchema the model is instructed to answer with JSON that the core
// then validates, so a bad answer fails the step instead of silently degrading
// to text. A prompt that exceeds maxInputBytes fails rather than being
// truncated: routing a large payload back through a model is what workflows
// exist to avoid.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lengzhao/workflowkit"
)

// DefaultMaxInputBytes bounds a rendered model prompt.
const DefaultMaxInputBytes = 16384

const jsonInstruction = "Respond with a single JSON value that validates against this JSON Schema. " +
	"No markdown fence, no commentary, JSON only.\nSchema:\n"

func init() {
	workflowkit.Register("model", New,
		workflowkit.WithDescription("One-shot model completion (no tools, no session). Optionally constrained by outputSchema."),
		workflowkit.WithConfig(Config{}),
		workflowkit.WithExample(map[string]any{
			"id":     "check",
			"type":   "model",
			"prompt": "Judge whether this can be submitted: {{ .steps.gen.text }}",
			"outputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
				"required":   []any{"ok"},
			},
		}),
	)
}

// Config is the model step's own fields.
type Config struct {
	// Model names the model; empty uses the host's default.
	Model string `json:"model,omitempty" desc:"model name; empty uses the host's default"`
	// Prompt is the user message. It may contain templates.
	Prompt string `json:"prompt" desc:"user message; may contain {{ }} templates"`
	// OutputSchema constrains the answer to JSON matching this schema.
	OutputSchema map[string]any `json:"outputSchema,omitempty" desc:"JSON Schema the answer must match"`
	// MaxInputBytes bounds the rendered prompt. Defaults to 16384.
	MaxInputBytes int `json:"maxInputBytes,omitempty" desc:"max rendered prompt size in bytes (default 16384)"`
	// System is an optional system message, useful for keeping the JSON
	// instruction stable across prompts.
	System string `json:"system,omitempty" desc:"optional system message"`
}

// New builds a model node.
func New(cfg Config) (workflowkit.Node, error) {
	if strings.TrimSpace(cfg.Prompt) == "" {
		return nil, fmt.Errorf("model: prompt is required")
	}
	if cfg.MaxInputBytes < 0 {
		return nil, fmt.Errorf("model: maxInputBytes must not be negative")
	}
	if len(cfg.OutputSchema) > 0 {
		// Compile now so a malformed outputSchema is a load-time error.
		if err := workflowkit.CompileSchema(cfg.OutputSchema); err != nil {
			return nil, fmt.Errorf("model: outputSchema: %w", err)
		}
	}
	return &node{cfg: cfg}, nil
}

type node struct{ cfg Config }

func (n *node) Run(ctx context.Context, env workflowkit.Env, c *workflowkit.Ctx) (workflowkit.Output, error) {
	if env.LLM == nil {
		return workflowkit.Output{}, fmt.Errorf("model step %q requires an LLM capability", c.StepID)
	}
	prompt, err := c.Render(n.cfg.Prompt)
	if err != nil {
		return workflowkit.Output{}, err
	}
	limit := n.cfg.MaxInputBytes
	if limit <= 0 {
		limit = DefaultMaxInputBytes
	}
	if len(prompt) > limit {
		return workflowkit.Output{}, fmt.Errorf(
			"model step %q: rendered prompt is %d bytes, over maxInputBytes %d; distill the data in an earlier step instead of raising the limit",
			c.StepID, len(prompt), limit)
	}
	if strings.TrimSpace(n.cfg.System) != "" {
		system, err := c.Render(n.cfg.System)
		if err != nil {
			return workflowkit.Output{}, err
		}
		prompt = system + "\n\n" + prompt
	}
	text, err := env.LLM.Complete(ctx, n.cfg.Model, prompt, n.cfg.OutputSchema)
	if err != nil {
		return workflowkit.Output{}, fmt.Errorf("model step %q: %w", c.StepID, err)
	}
	out := workflowkit.Output{Text: text}
	if len(n.cfg.OutputSchema) > 0 {
		parsed, err := parseJSON(text)
		if err != nil {
			return workflowkit.Output{Text: text}, fmt.Errorf("model step %q: %w", c.StepID, err)
		}
		if err := workflowkit.ValidateSchema(n.cfg.OutputSchema, parsed); err != nil {
			return workflowkit.Output{Text: text}, fmt.Errorf("model step %q: output does not match outputSchema: %w", c.StepID, err)
		}
		out.Data = parsed
	}
	return out, nil
}

// parseJSON extracts the first JSON value from model text, tolerating a
// markdown fence around it.
func parseJSON(text string) (any, error) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "```") {
		if i := strings.Index(trimmed, "\n"); i >= 0 {
			trimmed = trimmed[i+1:]
		}
		trimmed = strings.TrimSuffix(strings.TrimSpace(trimmed), "```")
		trimmed = strings.TrimSpace(trimmed)
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return nil, fmt.Errorf("model did not return valid JSON: %w", err)
	}
	return value, nil
}

// JSONInstruction is the system instruction a model step prepends when an
// outputSchema is set. Hosts implementing Completer can reuse it.
func JSONInstruction() string { return jsonInstruction }
