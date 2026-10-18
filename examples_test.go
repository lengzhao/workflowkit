package workflowkit_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lengzhao/workflowkit"
	"github.com/lengzhao/workflowkit/examples/demo"
	_ "github.com/lengzhao/workflowkit/nodes"
)

// TestExamplesRun compiles and runs every example command, so the examples stay
// working rather than rotting.
func TestExamplesRun(t *testing.T) {
	dirs, err := filepath.Glob("examples/0*")
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Fatal("no example commands found")
	}
	for _, dir := range dirs {
		dir := dir
		t.Run(filepath.Base(dir), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "go", "run", "./"+dir)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", dir, err, out)
			}
			if !strings.Contains(string(out), "result:") {
				t.Fatalf("%s printed no result:\n%s", dir, out)
			}
		})
	}
}

// TestAgentComposesLoop exercises the loop the 04 example demonstrates, in
// process: read the self-description, generate a definition, validate it with
// the same Parse the executor uses, and run it.
func TestAgentComposesLoop(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	env := demo.Env(dir)

	// 1) The briefing: what kinds exist and what each one's parameters are.
	for _, kind := range workflowkit.ListTypes() {
		info, ok := workflowkit.Describe(kind)
		if !ok {
			t.Fatalf("kind %q not describable", kind)
		}
		if info.Example == nil {
			t.Errorf("kind %q has no example for a model to copy", kind)
		}
	}
	tools, err := env.Catalog.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) == 0 {
		t.Fatal("the catalog should list tools")
	}

	// 2) A generated definition, built only from what the briefing offered.
	generated := `{
	  "description": "generated",
	  "steps": [
	    {"id":"count","type":"script","run":"printf '{\"rows\":17}'"},
	    {"id":"submit","type":"tool","tool":"` + tools[0].Name + `",
	     "args":{"rows":"{{ dig .steps.count.json \"rows\" }}"}}
	  ],
	  "result": "rows={{ dig .steps.submit.json \"rows\" }}"
	}`

	// 3) Validated by Parse — the same path the executor uses.
	wf, err := workflowkit.Parse([]byte(generated))
	if err != nil {
		t.Fatalf("generated definition did not validate: %v", err)
	}
	wf.ID = "generated"

	// 4) Run it.
	res, err := workflowkit.Run(ctx, wf, nil, env)
	if err != nil {
		t.Fatalf("generated definition did not run: %v", err)
	}
	if res.Text == "" {
		t.Fatal("expected a rendered result")
	}
}

// TestSchemaGuidesGeneration checks that a definition built from the schema
// passes, and a malformed one is rejected before any run.
func TestSchemaGuidesGeneration(t *testing.T) {
	ok := map[string]any{
		"description": "d",
		"steps": []any{
			map[string]any{"id": "a", "type": "script"},
		},
	}
	if err := workflowkit.ValidateSchema(workflowkit.Schema(), ok); err != nil {
		t.Fatalf("a well-shaped definition should satisfy the schema: %v", err)
	}
	bad := map[string]any{"steps": []any{map[string]any{"type": "script"}}}
	if err := workflowkit.ValidateSchema(workflowkit.Schema(), bad); err == nil {
		t.Fatal("a step without an id must not satisfy the schema")
	}
}

// TestDemoEnvIsUsable is a smoke test of the shared host implementation.
func TestDemoEnvIsUsable(t *testing.T) {
	env := demo.Env(t.TempDir())
	if env.Shell == nil || env.Tools == nil || env.LLM == nil || env.Agents == nil || env.Files == nil {
		t.Fatal("demo.Env should fill every capability")
	}
	out, _, code, err := env.Shell.Run(context.Background(), "printf hi", nil)
	if err != nil || code != 0 || out != "hi" {
		t.Fatalf("demo shell: out=%q code=%d err=%v", out, code, err)
	}
	dir, err := env.Files.RunDir("r1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("run dir was not created: %v", err)
	}
}
