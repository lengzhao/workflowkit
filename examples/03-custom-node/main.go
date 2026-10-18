// Command customnode shows how a host adds a node kind. The mechanism is the
// same one the built-in kinds use, so the built-ins double as the template.
//
//	go run ./examples/03-custom-node
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/lengzhao/workflowkit"
	"github.com/lengzhao/workflowkit/examples/demo"
	_ "github.com/lengzhao/workflowkit/nodes"
)

// ---------------------------------------------------------------------------
// A host-defined node kind: "http" fetches a URL and reports what it got.
// ---------------------------------------------------------------------------

// HTTPConfig is the kind's own step fields. The struct is the documentation: the
// json and desc tags become the schema Describe reports, and the core decodes a
// step's fields into it strictly, so a misspelled field is a load-time error.
type HTTPConfig struct {
	URL     string `json:"url" desc:"request URL; may contain {{ }} templates"`
	Method  string `json:"method,omitempty" desc:"HTTP method (default GET)"`
	Timeout int    `json:"timeoutSeconds,omitempty" desc:"request timeout in seconds"`
}

type httpNode struct{ cfg HTTPConfig }

// Run does the work. Everything else — conditionals, dependencies, retries,
// timeouts, parallel execution, output spilling — comes from the core.
func (n *httpNode) Run(ctx context.Context, env workflowkit.Env, c *workflowkit.Ctx) (workflowkit.Output, error) {
	url, err := c.Render(n.cfg.URL)
	if err != nil {
		return workflowkit.Output{}, err
	}
	method := strings.ToUpper(strings.TrimSpace(n.cfg.Method))
	if method == "" {
		method = "GET"
	}
	// A real implementation would use an HTTP client from Env.Extra. Here we
	// only report what the step asked for, so the example needs no network.
	return workflowkit.Output{
		Text: fmt.Sprintf("%s %s -> 200", method, url),
		Data: map[string]any{"status": 200, "method": method, "url": url},
	}, nil
}

func init() {
	// Register in init(), exactly like a built-in kind. Registering the same
	// kind twice panics, so a conflict is caught at startup.
	workflowkit.Register("http", func(cfg HTTPConfig) (workflowkit.Node, error) {
		if strings.TrimSpace(cfg.URL) == "" {
			return nil, fmt.Errorf("http: url is required")
		}
		return &httpNode{cfg: cfg}, nil
	},
		workflowkit.WithDescription("Fetch a URL and report the response. Own fields: url, method, timeoutSeconds."),
		workflowkit.WithConfig(HTTPConfig{}),
		workflowkit.WithExample(map[string]any{
			"id":     "fetch",
			"type":   "http",
			"url":    "https://example.com/api?d={{ .inputs.date }}",
			"method": "GET",
		}),
	)
}

// ---------------------------------------------------------------------------

func main() {
	dir, err := os.MkdirTemp("", "workflowkit-custom")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// What a model would read before writing a definition: the kind list, and
	// this kind's parameters and example.
	fmt.Println("registered kinds:", strings.Join(workflowkit.ListTypes(), ", "))
	info, _ := workflowkit.Describe("http")
	fmt.Printf("http: %s\n", info.Description)
	fmt.Printf("http example: %v\n\n", info.Example)

	// The custom kind takes a template, a retry and a conditional — none of
	// which its implementation mentions. The core provides them.
	wf, err := workflowkit.Parse([]byte(`{
  "description": "自定义 http 节点 + 核心提供的 retry / when / dependsOn",
  "inputs": { "type": "object", "properties": { "date": { "type": "string" } }, "required": ["date"] },
  "steps": [
    { "id": "fetch", "type": "http",
      "url": "https://example.com/ledger?date={{ .inputs.date }}",
      "method": "GET",
      "dependsOn": [] },

    { "id": "slow", "type": "http",
      "url": "https://example.com/ref?date={{ .inputs.date }}",
      "dependsOn": [],
      "retry": { "maxTries": 2, "waitSeconds": 0 } },

    { "id": "store", "type": "tool", "tool": "store",
      "dependsOn": ["fetch", "slow"],
      "when": "{{ eq (dig .steps.fetch.json \"status\") 200 }}",
      "args": { "url": "{{ dig .steps.fetch.json \"url\" }}",
                "ref": "{{ dig .steps.slow.json \"url\" }}" } }
  ],
  "result": "fetch={{ dig .steps.fetch.json \"status\" }} store={{ .steps.store.status }}"
}`))
	if err != nil {
		log.Fatal(err)
	}

	res, err := workflowkit.Run(context.Background(), wf, map[string]any{"date": "2026-10-18"}, demo.Env(dir))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("result:", res.Text)
	for _, id := range []string{"fetch", "slow", "store"} {
		s := res.Steps[id]
		fmt.Printf("  %-6s status=%-8s text=%s\n", id, s.Status, s.Text)
	}
}