// Command minimal runs the smallest useful pipeline: a definition file, a run,
// and a printed result.
//
//	go run ./examples/01-minimal
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/lengzhao/workflowkit"
	"github.com/lengzhao/workflowkit/examples/demo"
	// The blank import registers the built-in node kinds. Importing it is what
	// makes script/tool/model/agent/foreach available; a host that wants only
	// some of them imports those packages instead.
	_ "github.com/lengzhao/workflowkit/nodes"
)

func main() {
	dir, err := os.MkdirTemp("", "workflowkit-minimal")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// A definition file, exactly as it would live in a workflows directory.
	definition := `{
  "description": "生成一份小报告并写库",
  "inputs": {
    "type": "object",
    "properties": { "date": { "type": "string" } },
    "required": ["date"]
  },
  "steps": [
    {
      "id": "gen",
      "type": "script",
      "run": "printf '{\"rows\":42,\"date\":\"%s\"}' {{ .inputs.date | quote }}"
    },
    {
      "id": "submit",
      "type": "tool",
      "tool": "ledger__import",
      "args": { "rows": "{{ dig .steps.gen.json \"rows\" }}" }
    }
  ],
  "result": "导入 {{ dig .steps.submit.json \"rows\" }} 行（{{ .inputs.date }}）"
}`
	path := filepath.Join(dir, "daily.json")
	if err := os.WriteFile(path, []byte(definition), 0o644); err != nil {
		log.Fatal(err)
	}

	// Load: this validates the definition and resolves the file location.
	wf, err := workflowkit.Load(path)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("loaded %q (%d steps, estimated %ds)\n", wf.ID, len(wf.Steps), wf.EstimateDuration())

	// Run: capabilities come in per call, so one process can serve many hosts.
	res, err := workflowkit.Run(context.Background(), wf, map[string]any{"date": "2026-10-18"}, demo.Env(dir))
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("result:", res.Text)
	for _, id := range []string{"gen", "submit"} {
		s := res.Steps[id]
		fmt.Printf("  %-7s status=%-6s text=%s\n", id, s.Status, firstLine(s.Text))
	}
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}