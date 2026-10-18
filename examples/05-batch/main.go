// Command batch shows foreach: run a body of steps once per element, with the
// body's own retry and conditionals applying because the core executes it.
//
//	go run ./examples/05-batch
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/lengzhao/workflowkit"
	"github.com/lengzhao/workflowkit/examples/demo"
	_ "github.com/lengzhao/workflowkit/nodes"
)

func main() {
	dir, err := os.MkdirTemp("", "workflowkit-batch")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// The body step carries its own retry and continueOnError, and the
	// foreach step decides whether one bad item stops the rest.
	wf, err := workflowkit.Parse([]byte(`{
  "description": "对每个账户跑一遍校验与上报",
  "concurrency": 2,
  "steps": [
    { "id": "fetch", "type": "script",
      "run": "printf '{\"accounts\":[{\"id\":\"A1\"},{\"id\":\"A2\"},{\"id\":\"A3\"}]}'" },

    { "id": "each", "type": "foreach", "dependsOn": ["fetch"],
      "items": "{{ dig .steps.fetch.json \"accounts\" }}",
      "as": "account",
      "concurrency": 2,
      "onItemError": "continue",
      "body": [
        { "id": "check", "type": "script",
          "run": "printf 'checked %s' {{ dig .item \"id\" | quote }}",
          "retry": { "maxTries": 2, "waitSeconds": 0 } },
        { "id": "report", "type": "tool", "tool": "store",
          "when": "{{ eq .steps.check.status \"ok\" }}",
          "args": { "account": "{{ dig .item \"id\" }}" } }
      ] }
  ],
  "result": "共 {{ dig .steps.each.meta \"count\" }} 个账户，失败 {{ dig .steps.each.meta \"failed\" }} 个"
}`))
	if err != nil {
		log.Fatal(err)
	}

	start := time.Now()
	res, err := workflowkit.Run(context.Background(), wf, nil, demo.Env(dir))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("result:", res.Text)
	fmt.Printf("each: %s (took %s)\n", res.Steps["each"].Text, time.Since(start).Round(time.Millisecond))
	// The tool received one call per account, each carrying that account's id:
	// the body ran in the item context.
	fmt.Println("tool calls (one per account, in the item context):")
	for id, s := range res.Steps {
		if id == "each" || id == "fetch" {
			continue
		}
		fmt.Printf("  %-16s %s\n", id, s.Status)
	}
}