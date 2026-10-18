package workflowkit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/lengzhao/workflowkit"
)

// The benchmarks in this file guard against super-linear run cost. Two such
// regressions have already existed: each step copying the whole visible step map
// (quadratic in width), and every Parse/Run recompiling each node's JSON Schema.
func benchWorkflow(b *testing.B, n int) {
	steps := make([]map[string]any, n)
	for i := 0; i < n; i++ {
		steps[i] = map[string]any{"id": fmt.Sprintf("s%d", i), "type": "bench", "dependsOn": []any{}}
	}
	def, err := json.Marshal(map[string]any{"description": "bench", "concurrency": 8, "steps": steps})
	if err != nil {
		b.Fatal(err)
	}
	wf, err := workflowkit.Parse(def)
	if err != nil {
		b.Fatal(err)
	}
	env := workflowkit.Env{Shell: nil}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := workflowkit.Run(context.Background(), wf, nil, env); err != nil {
			b.Fatal(err)
		}
	}
}

func init() {
	workflowkit.Register("bench", func() (workflowkit.Node, error) { return benchNode{}, nil },
		workflowkit.WithDescription("bench node"), workflowkit.WithConfig(struct{}{}),
		workflowkit.WithExample(map[string]any{"id": "b", "type": "bench"}))
}

type benchNode struct{}

func (benchNode) Run(ctx context.Context, env workflowkit.Env, c *workflowkit.Ctx) (workflowkit.Output, error) {
	// Touch the step view, the way a real node reading a template would.
	_ = c.Steps.IDs()
	return workflowkit.Output{Text: "x"}, nil
}

func BenchmarkWideLevel10(b *testing.B)   { benchWorkflow(b, 10) }
func BenchmarkWideLevel100(b *testing.B)  { benchWorkflow(b, 100) }
func BenchmarkWideLevel1000(b *testing.B) { benchWorkflow(b, 1000) }
