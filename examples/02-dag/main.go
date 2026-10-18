// Command dag shows the fan-out/fan-in shape: two branches run in parallel
// (dependsOn: []), a join step waits for both, and a conditional step decides
// whether to submit or to alert.
//
//	go run ./examples/02-dag
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
	dir, err := os.MkdirTemp("", "workflowkit-dag")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// Two independent branches (dependsOn: []) fan out; the check step joins
	// them by listing both as dependencies. There is no merge node: a join is
	// just a step that depends on several steps, which is why the library needs
	// no fan-in primitive.
	wf, err := workflowkit.Parse([]byte(`{
  "description": "并行取两份数据 → 汇合校验 → 条件提交",
  "concurrency": 2,
  "steps": [
    { "id": "ledger", "type": "script", "dependsOn": [],
      "run": "sleep 0.3; printf '{\"total\":100}'" },
    { "id": "ref", "type": "script", "dependsOn": [],
      "run": "sleep 0.3; printf '{\"total\":100}'" },

    { "id": "check", "type": "model", "dependsOn": ["ledger", "ref"],
      "prompt": "两份统计是否一致？{{ .steps.ledger.text }} vs {{ .steps.ref.text }}",
      "outputSchema": {
        "type": "object",
        "properties": { "ok": { "type": "boolean" }, "reason": { "type": "string" } },
        "required": ["ok", "reason"]
      } },

    { "id": "submit", "type": "tool", "tool": "ledger__import",
      "when": "{{ eq (dig .steps.check.json \"ok\") true }}",
      "args": { "total": "{{ dig .steps.ledger.json \"total\" }}" } }
  ],
  "result": "check={{ dig .steps.check.json \"reason\" }} submit={{ .steps.submit.status }}"
}`))
	if err != nil {
		log.Fatal(err)
	}

	start := time.Now()
	res, err := workflowkit.Run(context.Background(), wf, nil, demo.Env(dir))
	if err != nil {
		log.Fatal(err)
	}
	elapsed := time.Since(start)

	fmt.Println("result:", res.Text)
	for _, id := range []string{"ledger", "ref", "check", "submit"} {
		s := res.Steps[id]
		fmt.Printf("  %-7s status=%-8s %.0fms\n", id, s.Status, s.Duration*1000)
	}
	// Two 300ms branches overlap, so the wall clock stays near one branch plus
	// the join. A serial pipeline would take ~600ms.
	fmt.Printf("wall clock: %s (parallel branches overlapped)\n", elapsed.Round(time.Millisecond))
}
