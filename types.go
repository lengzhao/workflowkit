package workflowkit

import (
	"context"
	"fmt"
	"sync"
)

// Node is one executable step. A node type's constructor returns a value
// implementing this interface.
//
// env carries the host capabilities (shell, tools, llm, …) for this run. It is
// passed per call rather than injected at construction because workflowkit is
// an embedded library: the same registered type may be used by different hosts,
// or by the same host with different capabilities per run (multi-tenant, tests
// with mocks). See doc.go.
//
// A node is responsible only for its own work. Scheduling, conditionals,
// dependencies, retries, timeouts, templating, output spilling and result
// aggregation are the core's job and are inherited by every node type for free.
type Node interface {
	Run(ctx context.Context, env Env, c *Ctx) (Output, error)
}

// Output is what a node returns and what templates read as .steps.<id>.
//
// Text becomes .text (spilled to a file beyond the spill threshold, in which
// case .path holds the location). Data becomes .json when it is an object or
// array, so dig can walk it. Meta is extra template-readable information (for
// example an agent's child session id).
type Output struct {
	Text string
	Data any
	Meta map[string]any
}

// Ctx is the per-step execution context handed to a node. A node reads its own
// config from Cfg, renders templates through Render/Value, and reaches the
// host through the Env passed to Run.
type Ctx struct {
	// StepID is the step's id, for error messages.
	StepID string
	// Cfg holds the step's own fields (everything that is not a common field),
	// unrendered, exactly as written in the definition.
	Cfg map[string]any
	// Inputs are the arguments the caller passed to Run.
	Inputs map[string]any
	// Steps reads the results of steps from earlier dependency levels. Call it
	// from inside Run, not at construction time: the view is built when the
	// node is invoked.
	Steps StepView
	// Item and Index are set inside a foreach body: the current element and its
	// position. They are nil/-1 outside.
	Item  any
	Index int
	// RunDir is this run's artifact directory (created by the core).
	RunDir string

	exec  *executor
	local *localSteps
}

// localSteps holds one composite body's results under their declared ids, so a
// body step can refer to a sibling as .steps.<declared-id> instead of the
// namespaced id the run publishes globally (each[0].check). Each item gets its
// own, which is what makes the alias unambiguous.
type localSteps struct {
	mu sync.Mutex
	m  map[string]StepResult
}

func newLocalSteps() *localSteps { return &localSteps{m: map[string]StepResult{}} }

func (l *localSteps) put(id string, r StepResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m[id] = r
}

func (l *localSteps) snapshot() map[string]StepResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]StepResult, len(l.m))
	for id, r := range l.m {
		out[id] = r
	}
	return out
}

// StepView reads the results of finished steps. It is only populated with
// steps the current step may see: results of earlier dependency levels. Steps
// of the same level are not visible to each other, which is what makes
// concurrent execution of a level safe.
type StepView interface {
	// Get returns the result of a finished step and whether it exists.
	Get(id string) (StepResult, bool)
	// IDs lists the step ids currently visible.
	IDs() []string
}

// StepResult is one step's outcome, both for templates (.steps.<id>) and for
// the caller (Result.Steps).
type StepResult struct {
	ID       string         `json:"id"`
	Status   string         `json:"status"` // ok / error / skipped
	Text     string         `json:"text,omitempty"`
	Path     string         `json:"path,omitempty"`
	Data     any            `json:"data,omitempty"`
	Meta     map[string]any `json:"meta,omitempty"`
	Error    string         `json:"error,omitempty"`
	Duration float64        `json:"durationSeconds,omitempty"`
}

// Env carries host capabilities into Run. Every field is optional: a
// definition that does not use a capability does not need it, and a node that
// needs a missing capability fails with an error naming it.
type Env struct {
	Shell  Shell       // script step
	Tools  ToolInvoker // tool step
	LLM    Completer   // model step
	Agents AgentRunner // agent step
	Files  Files       // run artifact directory
	// Catalog lists callable tools with their schemas. Optional: without it,
	// tool names and args are only checked at execution time.
	Catalog ToolCatalog
	// Events receives step-level call/result events. Nil is silent.
	Events EventSink
	// Traces receives spans for the run and each step. Nil is silent.
	Traces Tracer
	// Extra carries capabilities specific to a host's own node types, so a
	// custom node can reach a service the core knows nothing about.
	Extra map[string]any
}

// Shell runs a command and returns raw output. It exists so script steps get
// stdout/stderr/exit code without parsing a model-facing text protocol.
type Shell interface {
	Run(ctx context.Context, command string, timeoutSeconds *float64) (stdout, stderr string, exitCode int, err error)
}

// ToolInvoker executes a tool call. Implementations must keep the host's
// policy/approval/hooks in the path: a tool step is not a way around them.
type ToolInvoker interface {
	Execute(ctx context.Context, call ToolCall) (ToolResult, error)
}

// ToolCall is one tool invocation.
type ToolCall struct {
	ID    string
	Name  string
	Input map[string]any
}

// ToolResult is a tool's output.
type ToolResult struct {
	Content string
}

// ToolInfo describes a callable tool for a model choosing tools.
type ToolInfo struct {
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	InputSchema  map[string]any `json:"inputSchema,omitempty"`
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
}

// ToolCatalog lists the tools available in this environment.
type ToolCatalog interface {
	ListTools(ctx context.Context) ([]ToolInfo, error)
}

// Completer performs a one-shot completion: no tools, no session, no side
// effects. modelName is empty when the step does not name a model.
type Completer interface {
	Complete(ctx context.Context, modelName string, prompt string, outputSchema map[string]any) (string, error)
}

// AgentRunner delegates to a child agent and waits for its summary.
type AgentRunner interface {
	Run(ctx context.Context, agent string, task string, timeoutSeconds *int) (Summary, error)
}

// Summary is what comes back from a child agent.
type Summary struct {
	Text    string
	Session string
	Status  string
	Steps   int
}

// Files resolves (and creates) the artifact directory for a run. The host
// decides where it lives.
type Files interface {
	RunDir(runID string) (string, error)
}

// EventSink receives step-level events. The core emits a call/result pair per
// step so a host can surface pipeline internals without writing them to the
// model's conversation history.
type EventSink interface {
	Emit(ctx context.Context, ev Event)
}

// Event is one step-level emission.
type Event struct {
	Kind   string // "call" or "result"
	StepID string
	Node   string // step type, or the tool name for tool steps
	Input  map[string]any
	Output string
	Err    error
}

// Tracer receives observations for a run and its steps. Nil disables tracing.
type Tracer interface {
	StartRun(ctx context.Context, runID, workflowID string, inputs map[string]any) (context.Context, func(err error))
	StartStep(ctx context.Context, stepID, nodeType string) (context.Context, func(Output, error))
}

// Render renders a template string against this step's context.
func (c *Ctx) Render(text string) (string, error) {
	return renderTemplate(c.StepID, text, c.data())
}

// Value renders a template string and preserves JSON types: a leaf that renders
// to an object or array becomes that value, and a single-action leaf that
// renders to a number stays a number.
func (c *Ctx) Value(text string) (any, error) {
	return renderValue(text, c.data())
}

// Values deep-renders templates inside a map, for node configs holding nested
// structures (tool args, HTTP bodies). Each string leaf is rendered with Value.
func (c *Ctx) Values(value any) (any, error) {
	return renderValue(value, c.data())
}

// Sub derives a context for a nested step, optionally inside a foreach body.
func (c *Ctx) Sub(stepID string) *Ctx {
	next := *c
	next.StepID = stepID
	return &next
}

// WithItem derives a context whose .item and .index are set, for a foreach body.
func (c *Ctx) WithItem(item any, index int) *Ctx {
	next := *c
	next.Item = item
	next.Index = index
	return &next
}

// RunStep executes one step in this run, inheriting conditionals, retries,
// timeouts and the current item context (.item/.index) from the core.
// Composite nodes use it to run their body.
func (c *Ctx) RunStep(ctx context.Context, step Step) (StepResult, error) {
	if c.exec == nil {
		return StepResult{}, fmt.Errorf("workflowkit: step %q: no executor (node built outside Run)", c.StepID)
	}
	return c.exec.runStepNested(ctx, step, c.Item, c.Index, c.local), nil
}

// RunSteps executes sub-steps in declaration order, stopping at the first
// failure unless that step sets continueOnError.
func (c *Ctx) RunSteps(ctx context.Context, steps []Step) ([]StepResult, error) {
	if c.exec == nil {
		return nil, fmt.Errorf("workflowkit: step %q: no executor (node built outside Run)", c.StepID)
	}
	// A fresh local view per call: this body's steps see each other by their
	// declared ids, independent of other items running concurrently.
	return c.exec.runStepsNested(ctx, steps, c.Item, c.Index)
}

// DefinitionDir is the directory holding the definition file, or "" for a
// programmatic workflow. Relative paths in a node config resolve against it.
func (c *Ctx) DefinitionDir() string {
	if c.exec == nil {
		return ""
	}
	return c.exec.wf.Dir
}

// data builds the template context for this step.
func (c *Ctx) data() map[string]any {
	steps := map[string]any{}
	if c.Steps != nil {
		for _, id := range c.Steps.IDs() {
			if r, ok := c.Steps.Get(id); ok {
				steps[id] = stepTemplateData(r)
			}
		}
	}
	// A composite body's own steps are also reachable by their declared ids, so
	// a body step writes .steps.check rather than .steps.each[0].check.
	if c.local != nil {
		for id, r := range c.local.snapshot() {
			steps[id] = stepTemplateData(r)
		}
	}
	data := map[string]any{
		"inputs": c.Inputs,
		"steps":  steps,
		"workflow": map[string]any{
			"runDir": c.RunDir,
		},
		"item":  c.Item,
		"index": c.Index,
	}
	if c.exec != nil && c.exec.wf.Dir != "" {
		data["workflow"].(map[string]any)["dir"] = c.exec.wf.Dir
		data["workflow"].(map[string]any)["id"] = c.exec.wf.ID
	}
	return data
}
