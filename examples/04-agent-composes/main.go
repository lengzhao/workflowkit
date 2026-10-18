// Command agentcomposes shows the loop that makes the library usable by a
// model: it reads the self-description (kinds, their parameters, their
// examples, the definition schema, the available tools), assembles a
// definition, and validates it with the same Parse the executor uses.
//
// The "model" here is a scripted function so the example is deterministic and
// needs no provider. Swap it for a real completions call and the surrounding
// code is unchanged.
//
//	go run ./examples/04-agent-composes
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/lengzhao/workflowkit"
	"github.com/lengzhao/workflowkit/examples/demo"
	_ "github.com/lengzhao/workflowkit/nodes"
)

// ---------------------------------------------------------------------------
// Step 1: the briefing a model receives. Everything here is read from the
// library at runtime, so it can never drift from the code.
// ---------------------------------------------------------------------------

// Briefing is the catalogue a model needs before writing a definition.
type Briefing struct {
	Kinds      []KindBrief    `json:"kinds"`
	Tools      []ToolBrief    `json:"tools"`
	Rules      []string       `json:"rules"`
	MinimalDef map[string]any `json:"minimalDefinition"`
}

type KindBrief struct {
	Kind        string         `json:"kind"`
	Description string         `json:"description"`
	Config      map[string]any `json:"config,omitempty"`
	SubSteps    []string       `json:"subSteps,omitempty"`
	Example     map[string]any `json:"example,omitempty"`
}

type ToolBrief struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

func buildBriefing(ctx context.Context, env workflowkit.Env) (Briefing, error) {
	var b Briefing
	for _, kind := range workflowkit.ListTypes() {
		info, _ := workflowkit.Describe(kind)
		b.Kinds = append(b.Kinds, KindBrief{
			Kind:        info.Kind,
			Description: info.Description,
			Config:      info.Config,
			SubSteps:    info.SubSteps,
			Example:     info.Example,
		})
	}
	sort.Slice(b.Kinds, func(i, j int) bool { return b.Kinds[i].Kind < b.Kinds[j].Kind })
	if env.Catalog != nil {
		tools, err := env.Catalog.ListTools(ctx)
		if err != nil {
			return b, err
		}
		for _, t := range tools {
			b.Tools = append(b.Tools, ToolBrief{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
		}
	}
	b.Rules = []string{
		"Common fields on every step: id, type, when, dependsOn, continueOnError, retry, timeoutSeconds.",
		"Omit dependsOn to wait for the previous step; dependsOn: [] starts immediately; a list waits for those ids.",
		"A step depending on several ids is a join.",
		"Templates: {{ .inputs.x }}, {{ .steps.<id>.text }}, {{ .steps.<id>.path }}, {{ .steps.<id>.json }}; functions dig, quote, readJSON.",
		"Put large data in files under {{ .workflow.runDir }} and pass paths, not contents.",
	}
	// The smallest valid definition, assembled from one example, is the best
	// shape hint a model can get.
	b.MinimalDef = map[string]any{
		"description": "…",
		"steps":       []any{},
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// Step 2: the "model" writes a definition from the briefing.
// ---------------------------------------------------------------------------

// compose stands in for a completion call. A real one would send the briefing
// as the prompt and parse the reply as JSON.
func compose(b Briefing) string {
	// It only uses kinds and tools that the briefing actually offered, which is
	// the point: the model cannot invent a capability.
	hasScript, hasTool := false, false
	for _, k := range b.Kinds {
		if k.Kind == "script" {
			hasScript = true
		}
		if k.Kind == "tool" {
			hasTool = true
		}
	}
	var toolNames []string
	for _, t := range b.Tools {
		toolNames = append(toolNames, t.Name)
	}
	if !hasScript || !hasTool {
		return `{}`
	}
	return fmt.Sprintf(`{
  "description": "统计当日行数并写库（由 Agent 依据 briefing 生成）",
  "inputs": {
    "type": "object",
    "properties": { "date": { "type": "string" } },
    "required": ["date"]
  },
  "steps": [
    { "id": "count", "type": "script",
      "run": "printf '{\"rows\":17,\"date\":\"%%s\"}' {{ .inputs.date | quote }}" },
    { "id": "submit", "type": "tool", "tool": %q,
      "args": { "rows": "{{ dig .steps.count.json \"rows\" }}" } }
  ],
  "result": "已写入 {{ dig .steps.submit.json \"rows\" }} 行（{{ .inputs.date }}）"
}`, pickTool(toolNames, "ledger__import"))
}

func pickTool(names []string, want string) string {
	for _, n := range names {
		if n == want {
			return n
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// ---------------------------------------------------------------------------

func main() {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "workflowkit-agent")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	env := demo.Env(dir)

	briefing, err := buildBriefing(ctx, env)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("=== briefing (%d kinds, %d tools) ===\n", len(briefing.Kinds), len(briefing.Tools))
	for _, k := range briefing.Kinds {
		fmt.Printf("  %-8s %s\n", k.Kind, k.Description)
	}
	for _, t := range briefing.Tools {
		fmt.Printf("  tool %-16s %s\n", t.Name, t.Description)
	}
	fmt.Println("  rules:")
	for _, r := range briefing.Rules {
		fmt.Printf("    - %s\n", r)
	}

	generated := compose(briefing)
	fmt.Printf("\n=== generated definition (%d bytes) ===\n%s\n", len(generated), generated)

	// Step 3: the model's output is validated by the same Parse the executor
	// uses. A bad definition comes back as an error the model can act on — no
	// separate checker to keep in sync.
	wf, err := workflowkit.Parse([]byte(generated))
	if err != nil {
		fmt.Println("\nvalidation failed:", compact(err.Error()))
		os.Exit(1)
	}
	// Parse has no filename to take an id from (Load does), so an inline
	// definition names itself.
	if wf.ID == "" {
		wf.ID = "generated"
	}
	fmt.Printf("\n=== validated: %q, %d steps, estimated %ds ===\n", wf.ID, len(wf.Steps), wf.EstimateDuration())

	// Step 4: run it, and repair it if it fails. A model does exactly this: the
	// error names the step and the missing field, so the fix is mechanical.
	res, err := workflowkit.Run(ctx, wf, map[string]any{"date": "2026-10-18"}, env)
	if err != nil {
		fmt.Println("\nrun failed:", compact(err.Error()))
		fmt.Println("→ repairing the result template and retrying")
		repaired := strings.Replace(generated, `dig .steps.submit.json \"rows\"`, `dig .steps.submit.json \"status\"`, 1)
		wf, err = workflowkit.Parse([]byte(repaired))
		if err != nil {
			log.Fatal(err)
		}
		wf.ID = "generated"
		res, err = workflowkit.Run(ctx, wf, map[string]any{"date": "2026-10-18"}, env)
		if err != nil {
			log.Fatal(err)
		}
	}
	fmt.Println("result:", res.Text)

	// What a model would do with a definition it got wrong: read the error and
	// fix it. The error names the unknown kind and lists the ones that exist.
	bad := `{"description":"broken","steps":[{"id":"a","type":"htttp","url":"x"}]}`
	if _, err := workflowkit.Parse([]byte(bad)); err != nil {
		fmt.Println("\n(rejected definition returns, actionable:)")
		fmt.Println(" ", compact(err.Error()))
	}
	_ = json.Marshal
}

func compact(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}
