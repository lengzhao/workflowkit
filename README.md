# workflowkit

An embeddable Go engine for declarative pipelines. Import it, write a pipeline
as a definition file (or a Go literal), and run it in-process.

```go
import (
    "github.com/lengzhao/workflowkit"
    _ "github.com/lengzhao/workflowkit/nodes"          // built-in node kinds
)

wf, err := workflowkit.Load("workflows/daily.json")     // parse + validate
res, err := workflowkit.Run(ctx, wf,
    map[string]any{"date": "2026-10-18"},
    workflowkit.Env{Shell: myShell, Tools: myTools},    // capabilities per call
)
fmt.Println(res.Text)
```

There is no scheduler, no trigger, no persistence and no background goroutine.
The library does `register → parse → run` and nothing else.

## Why it looks the way it does

**Node types are pluggable, and the built-ins are ordinary plugins.**
`script`, `tool`, `model`, `agent` and `foreach` each live in their own package
and register themselves in `init()`, exactly the way a host adds its own kind.
Dropping any of them leaves the library fully functional.

```go
package mynode

func init() {
    workflowkit.Register("mykind", func(cfg Config) (workflowkit.Node, error) {
        return &node{cfg: cfg}, nil
    },
        workflowkit.WithDescription("what this kind does"),
        workflowkit.WithConfig(Config{}),          // schema from the struct tags
        workflowkit.WithExample(map[string]any{...}),
    )
}

type node struct{ cfg Config }

func (n *node) Run(ctx context.Context, env workflowkit.Env, c *workflowkit.Ctx) (workflowkit.Output, error) {
    cmd, err := c.Render(n.cfg.Command)            // templates
    if err != nil {
        return workflowkit.Output{}, err
    }
    // ... do the work
    return workflowkit.Output{Text: out}, nil
}
```

A custom kind gets conditionals (`when`), dependencies (`dependsOn`), retries,
timeouts, parallel execution and output spilling **for free** — those belong to
the core, not to the node. For a composite kind (one that runs sub-steps), call
`c.RunSteps(ctx, steps)`: the sub-steps keep all of the above, which is how
`foreach` stays short.

**Everything checkable is checked at load time.** `Parse` validates field
requirements, template syntax, dependency references, cycles, unknown kinds,
node configs and sub-steps — and it constructs each node once, so a missing
required field or a bad enum is a load-time error rather than a first-run
surprise.

**The library describes itself, so a model can build pipelines with it.**
`ListTypes`, `Describe`, `Schema` and `Env.Catalog` give a caller everything
needed to generate a definition, and `Parse` is the same validator the executor
uses — there is no second checker to keep in sync.

```
ListTypes() + Describe(kind) + Env.Catalog.ListTools(ctx)   →  a model writes a definition
Parse(generated)                                            →  validated, errors are actionable
Run(ctx, wf, inputs, env)
```

## Definition format

```jsonc
{
  "description": "生成回购账本并写库",            // required; callers route by it
  "inputs": { "type": "object", "required": ["date"], "properties": { "date": { "type": "string" } } },
  "concurrency": 4,                              // parallel steps per dependency level
  "steps": [
    { "id": "gen", "type": "script",
      "run": "python3 gen.py {{ .inputs.date | quote }} --out {{ .workflow.runDir }}/l.json" },

    { "id": "check", "type": "model",
      "prompt": "判定这份统计能否入库：{{ .steps.gen.text }}",
      "outputSchema": { "type": "object", "properties": { "ok": { "type": "boolean" } }, "required": ["ok"] } },

    { "id": "submit", "type": "tool", "tool": "ledger__import",
      "when": "{{ eq (dig .steps.check.json \"ok\") true }}",
      "args": { "path": "{{ .workflow.runDir }}/l.json" },
      "retry": { "maxTries": 3, "waitSeconds": 2 } }
  ],
  "result": "已写库，状态 {{ dig .steps.submit.json \"status\" }}"
}
```

### Common step fields

| Field | Meaning |
|---|---|
| `id` | unique in the workflow; how later steps reference this one |
| `type` | a registered node kind |
| `when` | template; `""` / `"0"` / `"false"` skips the step (status `skipped`) |
| `dependsOn` | omitted = wait for the previous step; `[]` = start immediately; a list = wait for those ids |
| `continueOnError` | record the failure and keep going (downstream is not skipped automatically) |
| `retry` | `{ "maxTries": n, "waitSeconds": n }` |
| `timeoutSeconds` | per-step wall clock |

### Execution

Levels are derived from the dependency graph (a serial pipeline is one chain).
Steps within a level run concurrently up to `concurrency` — omit it for the
default (4), set `0` for no limit. A join is simply a step that depends on
several steps — no fan-in node exists. A failing step lets
its level finish and then stops the run before the next level; `continueOnError`
overrides that. The library sets no outer deadline: size your own with
`wf.EstimateDuration()`, the critical path of step timeouts.

### Data passing

Large outputs are written to `<runDir>/<step>.out` and referenced by `.path`;
`.text` keeps a truncated view. This is the one invariant: large payloads never
travel through the caller's context.

| Template | Meaning |
|---|---|
| `{{ .inputs.x }}` | an argument passed to Run |
| `{{ .steps.<id>.text }}` | truncated inline view of a step's output |
| `{{ .steps.<id>.path }}` | file holding an oversized output |
| `{{ .steps.<id>.json }}` | a step's structured output (when it is an object or array) |
| `{{ .steps.<id>.status }}` | `ok` / `error` / `skipped` |
| `{{ .workflow.runDir }}` | this run's artifact directory |
| `{{ .item }}`, `{{ .index }}` | current element and position inside a `foreach` body |

Functions: `dig` (walk JSON by dotted path), `quote` (shell-quote), `readJSON`
(read a JSON object or array from the run or definition directory). Conditions
use text/template's built-in `eq`, `ne`, `lt`, `gt`, `and`, `or`, `not`.

JSON types survive the trip: a leaf rendering to an object or array is passed as
that value, a single-action leaf rendering to a number stays a number (so
integers above 2^53 are not rounded), and a static `"42"` stays a string.

## Built-in node kinds

| Kind | Own fields | Needs |
|---|---|---|
| `script` | `run`, `dir`, `timeoutSeconds` | `Env.Shell` |
| `tool` | `tool`, `args` | `Env.Tools` |
| `model` | `prompt`, `model`, `system`, `outputSchema`, `maxInputBytes` | `Env.LLM` |
| `agent` | `agent`, `task` | `Env.Agents` |
| `foreach` | `items`, `concurrency`, `body`, `onItemError` | — (uses the core) |

## Capabilities

Every field is optional; a definition that does not use a capability does not
need it, and a node that needs a missing one fails with an error naming the step
and the capability.

```go
type Env struct {
    Shell   Shell        // Run(ctx, cmd, timeout) (stdout, stderr string, exit int, err error)
    Tools   ToolInvoker  // Execute(ctx, call) (Result, error)
    LLM     Completer    // Complete(ctx, model, prompt, schema) (string, error)
    Agents  AgentRunner  // Run(ctx, agent, task, timeout) (Summary, error)
    Files   Files        // RunDir(runID) (string, error)
    Catalog ToolCatalog  // ListTools(ctx) ([]ToolInfo, error)   — optional
    Events  EventSink    // optional, silent when nil
    Traces  Tracer       // optional, silent when nil
    Extra   map[string]any // capabilities for a host's own node kinds
}
```

## Examples

```
go run ./examples/01-minimal         # a definition file, a run, a printed result
go run ./examples/02-dag             # parallel branches, a join, a conditional
go run ./examples/03-custom-node     # add a node kind; the core supplies the rest
go run ./examples/04-agent-composes  # self-description → generated definition → run
go run ./examples/05-batch           # foreach with per-item context
```

`examples/demo` is a small host implementation (real shell, in-memory tools,
fixed model replies) that the examples share.

## Tests

```
go test ./...        # unit tests, including the acceptance list from docs/
go test -race ./...
```

The core depends only on the standard library and a JSON Schema validator:

```
go list -deps . | grep -v '^internal\|^unicode\|^errors\|^strconv\|^sync\|^io\|^os\|^time\|^fmt\|^strings\|^sort\|^bytes\|^encoding\|^math\|^context\|^path\|^reflect\|^runtime\|^text\|^crypto\|^hash\|^regexp\|^bufio\|^container\|^iter\|^slices\|^maps\|^cmp\|^unique\|^weak\|^log\|^syscall\|^vendor\|^net\|^database\|^golang.org/x'
```

## Docs

| Doc | Contents |
|---|---|
| [docs/2026-10-18-requirements.zh.md](docs/2026-10-18-requirements.zh.md) | requirements, from the AgentKit `tool/workflow` feature |
| [docs/2026-10-18-design.zh.md](docs/2026-10-18-design.zh.md) | package layout and design decisions |
| [docs/2026-10-18-n8n-research.zh.md](docs/2026-10-18-n8n-research.zh.md) | what n8n / Argo Workflows do, and what was borrowed |