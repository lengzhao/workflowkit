// Package foreach registers the "foreach" node kind: run a body of steps once
// per element of an array.
//
// It is both the library's loop and the reference composite node: its body is
// validated by Parse like any other steps, and it executes them through
// Ctx.RunSteps, so each body step keeps its own when, retry, timeout and
// continueOnError. A body runs against a context whose .item and .index are the
// current element and position.
package foreach

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/lengzhao/workflowkit"
)

func init() {
	workflowkit.Register("foreach", New,
		workflowkit.WithDescription("Run a body of steps once per element of an array, with .item and .index set for each element."),
		workflowkit.WithConfig(Config{}),
		workflowkit.WithSubSteps("body"),
		workflowkit.WithExample(map[string]any{
			"id":          "each",
			"type":        "foreach",
			"items":       "{{ dig (readJSON (printf \"%s/rows.json\" .workflow.runDir)) \"rows\" }}",
			"concurrency": 3,
			"body": []any{
				map[string]any{
					"id":   "save",
					"type": "tool",
					"tool": "db__insert",
					"args": map[string]any{"row": "{{ .item }}"},
				},
			},
		}),
	)
}

// Config is the foreach step's own fields.
type Config struct {
	// Items is the array to iterate: a template or a literal array.
	Items any `json:"items" desc:"array to iterate; a template rendering to an array, or a literal array"`
	// Concurrency caps how many items run at once. Omitted uses the default (4);
	// 0 means no limit, which is the usual choice for a rate-limited API.
	Concurrency *int `json:"concurrency,omitempty" desc:"max items running at once (default 4, 0 = no limit)"`
	// Body is the list of steps to run per element.
	Body []workflowkit.Step `json:"body" desc:"steps to run for each element"`
	// OnItemError is "fail" (default) or "continue". With "continue", an item
	// that fails does not stop the others; the failing items are reported in
	// the node's output.
	OnItemError string `json:"onItemError,omitempty" desc:"\"fail\" (default) or \"continue\""`
}

// New builds a foreach node.
func New(cfg Config) (workflowkit.Node, error) {
	if cfg.Items == nil {
		return nil, fmt.Errorf("foreach: items is required")
	}
	if len(cfg.Body) == 0 {
		return nil, fmt.Errorf("foreach: body must contain at least one step")
	}
	switch cfg.OnItemError {
	case "", "fail", "continue":
	default:
		return nil, fmt.Errorf("foreach: onItemError must be \"fail\" or \"continue\", got %q", cfg.OnItemError)
	}
	return &node{cfg: cfg}, nil
}

type node struct{ cfg Config }

// itemResult is one element's outcome, reported in the node's output.
type itemResult struct {
	index  int
	status string
	err    string
}

// slotCount sizes the semaphore, where 0 means no limit. A buffered channel
// needs a positive capacity, so "no limit" becomes the item count.
func slotCount(limit, n int) int {
	if limit <= 0 {
		return n
	}
	return limit
}

func (n *node) Run(ctx context.Context, env workflowkit.Env, c *workflowkit.Ctx) (workflowkit.Output, error) {
	items, err := n.items(c)
	if err != nil {
		return workflowkit.Output{}, err
	}
	if len(items) == 0 {
		return workflowkit.Output{Text: "foreach: no items", Data: map[string]any{"count": 0, "failed": 0}}, nil
	}
	if err := workflowkit.ValidateConcurrency(n.cfg.Concurrency); err != nil {
		return workflowkit.Output{}, err
	}
	results := make([]itemResult, len(items))
	sem := make(chan struct{}, slotCount(workflowkit.ResolveConcurrency(n.cfg.Concurrency, len(items)), len(items)))
	var wg sync.WaitGroup
	for i, item := range items {
		i, item := i, item
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = n.runItem(ctx, c, i, item)
		}()
	}
	wg.Wait()

	failures := make([]itemResult, 0)
	for _, r := range results {
		if r.status == "error" {
			failures = append(failures, r)
		}
	}
	// The summary goes into Data, so a downstream step reads it with dig, for
	// example: {{ dig .steps.each.json "failed" }}
	out := workflowkit.Output{
		Text: summarize(len(items), len(failures)),
		Data: map[string]any{
			"count":    len(items),
			"failed":   len(failures),
			"statuses": statuses(results),
		},
	}
	if len(failures) > 0 && n.cfg.OnItemError != "continue" {
		first := failures[0]
		return out, fmt.Errorf("foreach step %q: item %d failed: %s", c.StepID, first.index, first.err)
	}
	return out, nil
}

// items renders the items field into a slice. A single rendered value that is
// not an array is treated as a one-element list only when it is non-nil
// scalar; an object is an error, because iterating an object is ambiguous.
func (n *node) items(c *workflowkit.Ctx) ([]any, error) {
	rendered, err := c.Values(n.cfg.Items)
	if err != nil {
		return nil, err
	}
	switch v := rendered.(type) {
	case nil:
		return nil, nil
	case []any:
		return v, nil
	case string:
		// A template that rendered to a JSON array is already parsed by Values;
		// an empty string means nothing to iterate.
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		return []any{v}, nil
	case map[string]any:
		return nil, fmt.Errorf("foreach step %q: items rendered to an object; render an array instead", c.StepID)
	default:
		return []any{v}, nil
	}
}

func (n *node) runItem(ctx context.Context, c *workflowkit.Ctx, index int, item any) itemResult {
	if ctx.Err() != nil {
		return itemResult{index: index, status: "error", err: ctx.Err().Error()}
	}
	// The core scopes body step ids per item, so the node just supplies the item.
	inner := c.WithItem(item, index)
	_, err := inner.RunSteps(ctx, n.cfg.Body)
	if err != nil {
		return itemResult{index: index, status: "error", err: err.Error()}
	}
	return itemResult{index: index, status: "ok"}
}

func summarize(total, failed int) string {
	if failed == 0 {
		return fmt.Sprintf("foreach: %d item(s) ok", total)
	}
	return fmt.Sprintf("foreach: %d item(s), %d failed", total, failed)
}

func statuses(results []itemResult) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = r.status
	}
	return out
}
