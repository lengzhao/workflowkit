package workflowkit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SpillThreshold is the size beyond which a step's output is written to a file
// and referenced by path instead of being kept inline. It is the mechanism
// behind the library's one invariant: large payloads never travel through the
// caller's context.
const SpillThreshold = 50 * 1024

// DefaultConcurrency caps how many steps of one dependency level run at once.
const DefaultConcurrency = 4

// DefaultStepTimeout is not enforced by the core (a step's own timeout is
// honored, and the outer deadline belongs to the caller) but names the value
// the estimate uses for steps without one.
const DefaultStepTimeout = estimateStepTimeout

// Result is what Run returns. Text is the rendered result template (empty when
// the definition declares none). Steps holds every step's outcome, so a caller
// that is not a model can read structured data instead of parsing text.
type Result struct {
	Status string                `json:"status"` // ok / error
	Text   string                `json:"text,omitempty"`
	Steps  map[string]StepResult `json:"steps"`
	Dir    string                `json:"dir,omitempty"`
	Err    error                 `json:"-"`
}

// Run executes a workflow with the given inputs and host capabilities.
//
// Inputs are validated against the definition's inputs schema. Steps run level
// by level (dependency DAG; a serial pipeline is a single chain), with steps of
// one level running concurrently up to the workflow's concurrency. A failure
// stops the run before the next level unless the step sets continueOnError;
// steps of the failing level always finish. Cancelling ctx aborts the current
// step and starts no new level, keeping the results of everything that
// completed.
func Run(ctx context.Context, wf *Workflow, inputs map[string]any, env Env) (*Result, error) {
	if wf == nil {
		return nil, fmt.Errorf("workflowkit: nil workflow")
	}
	if err := wf.validate(); err != nil {
		return nil, err
	}
	if inputs == nil {
		inputs = map[string]any{}
	}
	if err := validateInputs(wf, inputs); err != nil {
		return nil, err
	}

	runID := newRunID()
	runDir, err := resolveRunDir(env, runID)
	if err != nil {
		return nil, err
	}

	if env.Traces != nil {
		var end func(error)
		ctx, end = env.Traces.StartRun(ctx, runID, wf.ID, inputs)
		if end != nil {
			defer func() { end(nil) }()
		}
	}

	ex := &executor{
		wf:      wf,
		env:     env,
		runDir:  runDir,
		inputs:  inputs,
		results: map[string]StepResult{},
		visible: map[string]bool{},
	}
	res := ex.run(ctx)
	res.Dir = runDir
	if res.Err != nil {
		return res, res.Err
	}
	return res, nil
}

// validateInputs checks Run's arguments against the workflow's inputs schema.
// A workflow without inputs accepts anything.
func validateInputs(wf *Workflow, inputs map[string]any) error {
	if len(wf.Inputs) == 0 {
		return nil
	}
	if err := validateAgainstSchema(wf.Inputs, inputs); err != nil {
		return fmt.Errorf("workflow %q: inputs do not match the inputs schema: %w", wf.ID, err)
	}
	return nil
}

func resolveRunDir(env Env, runID string) (string, error) {
	if env.Files != nil {
		return env.Files.RunDir(runID)
	}
	// Without a Files capability the artifacts still need a home; a temporary
	// directory keeps the run working and is the caller's to clean up.
	dir := filepath.Join(os.TempDir(), "workflowkit", runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("workflowkit: create run directory: %w", err)
	}
	return dir, nil
}

func newRunID() string {
	return time.Now().UTC().Format("20060102T150405") + "-" + fmt.Sprintf("%06d", time.Now().UnixNano()%1_000_000)
}

// executor holds one run's state. It is confined to a single Run call, so
// concurrent runs do not share anything.
type executor struct {
	wf     *Workflow
	env    Env
	runDir string
	inputs map[string]any

	mu      sync.Mutex
	results map[string]StepResult
	visible map[string]bool
}

// run executes the levels and renders the result template.
func (ex *executor) run(ctx context.Context) *Result {
	res := &Result{Status: "ok", Steps: map[string]StepResult{}}

	plan := levels(ex.wf.Steps)
	if plan == nil {
		res.Status = "error"
		res.Err = fmt.Errorf("workflow %q: dependency cycle detected", ex.wf.ID)
		return res
	}

	concurrency := ex.wf.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}

	var failed error
	for _, level := range plan {
		if ctx.Err() != nil {
			if failed == nil {
				failed = ctx.Err()
			}
			break
		}
		levelResults := ex.runLevel(ctx, level, concurrency)
		// Results are published only after the level completes: steps within a
		// level read strictly earlier levels, so the maps are written with no
		// concurrent readers.
		ex.mu.Lock()
		for id, r := range levelResults {
			ex.results[id] = r
			ex.visible[id] = true
		}
		ex.mu.Unlock()

		for idx := range ex.wf.Steps {
			if !contains(level, idx) {
				continue
			}
			step := &ex.wf.Steps[idx]
			r := levelResults[step.ID]
			if r.Status == "error" && !step.ContinueOnError && failed == nil {
				failed = fmt.Errorf("workflow %q step %q: %s", ex.wf.ID, step.ID, r.Error)
			}
		}
		// A failing step aborts the next level; the current level always runs
		// to completion (the serial "break" generalized).
		if failed != nil {
			break
		}
	}

	// Publish everything, including steps that never ran because the run
	// aborted, so the caller sees the whole pipeline.
	ex.mu.Lock()
	res.Steps = make(map[string]StepResult, len(ex.wf.Steps))
	for _, step := range ex.wf.Steps {
		if r, ok := ex.results[step.ID]; ok {
			res.Steps[step.ID] = r
			continue
		}
		res.Steps[step.ID] = StepResult{ID: step.ID, Status: "skipped"}
	}
	ex.mu.Unlock()

	if failed != nil {
		res.Status = "error"
		res.Err = failed
		return res
	}
	if strings.TrimSpace(ex.wf.Result) != "" {
		text, err := renderTemplate(ex.wf.ID+".result", ex.wf.Result, ex.templateData())
		if err != nil {
			res.Status = "error"
			res.Err = err
			return res
		}
		res.Text = text
	}
	return res
}

// runLevel executes one dependency level, up to concurrency steps at a time.
func (ex *executor) runLevel(ctx context.Context, level []int, concurrency int) map[string]StepResult {
	out := make(map[string]StepResult, len(level))
	if len(level) == 1 {
		step := &ex.wf.Steps[level[0]]
		out[step.ID] = ex.runStep(ctx, step, nil, -1)
		return out
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, idx := range level {
		idx := idx
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			step := &ex.wf.Steps[idx]
			r := ex.runStep(ctx, step, nil, -1)
			mu.Lock()
			out[step.ID] = r
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// runStep executes one step: evalWhen, retries, timeout and output capture.
// item/index carry the foreach context (nil/-1 outside a body).
func (ex *executor) runStep(ctx context.Context, step *Step, item any, index int) StepResult {
	start := time.Now()
	res := StepResult{ID: step.ID, Status: "ok"}

	c := ex.newCtx(step, item, index)
	data := c.data()

	if strings.TrimSpace(step.When) != "" {
		ok, err := evalWhen(ex.wf.ID+"."+step.ID, step.When, data)
		switch {
		case err != nil:
			res.Status = "error"
			res.Error = fmt.Sprintf("when condition: %v", err)
			res.Duration = time.Since(start).Seconds()
			return res
		case !ok:
			res.Status = "skipped"
			res.Duration = time.Since(start).Seconds()
			return res
		}
	}

	stepCtx := ctx
	cancel := func() {}
	if step.TimeoutSeconds > 0 {
		stepCtx, cancel = context.WithTimeout(ctx, time.Duration(step.TimeoutSeconds)*time.Second)
	}
	defer cancel()

	if ex.env.Traces != nil {
		var end func(Output, error)
		stepCtx, end = ex.env.Traces.StartStep(stepCtx, step.ID, step.Type)
		if end != nil {
			defer func() { end(Output{Text: res.Text}, errFromString(res.Error)) }()
		}
	}

	attempts := 1
	if step.Retry != nil && step.Retry.MaxTries > 1 {
		attempts = step.Retry.MaxTries
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		out, err := ex.invoke(stepCtx, step, c)
		// A step whose own deadline expired fails with a timeout error, even
		// when the node reported no error (a host may turn a timeout into a
		// benign-looking result) and regardless of what the node's own error
		// said, so the message is the actionable one.
		if errorsIsDeadline(stepCtx, ctx) {
			err = stepTimeoutError(step)
		}
		if err == nil {
			ex.captureOutput(&res, step, out)
			res.Duration = time.Since(start).Seconds()
			return res
		}
		lastErr = err
		if attempt < attempts && !isFinalError(stepCtx, ctx, err) {
			if step.Retry != nil && step.Retry.WaitSeconds > 0 {
				select {
				case <-time.After(time.Duration(step.Retry.WaitSeconds) * time.Second):
				case <-ctx.Done():
					lastErr = ctx.Err()
					attempt = attempts
				}
			}
			continue
		}
		break
	}

	res.Status = "error"
	res.Error = lastErr.Error()
	res.Duration = time.Since(start).Seconds()
	return res
}

// invoke constructs the node and runs it, emitting a call/result event pair so
// a host can surface pipeline internals without touching its own history.
func (ex *executor) invoke(ctx context.Context, step *Step, c *Ctx) (Output, error) {
	reg, ok := lookup(step.Type)
	if !ok {
		return Output{}, fmt.Errorf("unknown type %q", step.Type)
	}
	node, err := reg.ctor(step.Fields)
	if err != nil {
		return Output{}, err
	}
	ex.emit(ctx, Event{Kind: "call", StepID: step.ID, Node: step.Type, Input: step.Fields})
	out, err := node.Run(ctx, ex.env, c)
	ex.emit(ctx, Event{Kind: "result", StepID: step.ID, Node: step.Type, Output: out.Text, Err: err})
	return out, err
}

// newCtx builds the execution context for one step invocation.
func (ex *executor) newCtx(step *Step, item any, index int) *Ctx {
	return &Ctx{
		StepID: step.ID,
		Cfg:    step.Fields,
		Inputs: ex.inputs,
		Steps:  ex.snapshot(),
		Item:   item,
		Index:  index,
		RunDir: ex.runDir,
		exec:   ex,
	}
}

// captureOutput records a successful step's output, spilling large text.
func (ex *executor) captureOutput(res *StepResult, step *Step, out Output) {
	res.Text, res.Path = ex.spill(step.ID, out.Text)
	res.Data = out.Data
	if res.Data == nil {
		res.Data = parseJSONContent(out.Text)
	}
	res.Meta = out.Meta
}

// spill caps an inline view, writing oversized text to an artifact file and
// returning its path.
func (ex *executor) spill(stepID, content string) (view, path string) {
	if len(content) <= SpillThreshold {
		return content, ""
	}
	path = filepath.Join(ex.runDir, sanitizeFileName(stepID)+".out")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return truncateRunes(content, SpillThreshold), ""
	}
	return truncateRunes(content, SpillThreshold) + "\n... [full output: " + path + "]", path
}

// snapshot returns a read-only view of the currently visible steps.
func (ex *executor) snapshot() StepView {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	view := map[string]StepResult{}
	for id, r := range ex.results {
		if ex.visible[id] {
			view[id] = r
		}
	}
	return mapStepView(view)
}

// templateData builds the template context for the workflow result.
func (ex *executor) templateData() map[string]any {
	ex.mu.Lock()
	steps := map[string]any{}
	for id, r := range ex.results {
		steps[id] = stepTemplateData(r)
	}
	ex.mu.Unlock()
	return map[string]any{
		"inputs": ex.inputs,
		"steps":  steps,
		"workflow": map[string]any{
			"id":     ex.wf.ID,
			"runDir": ex.runDir,
		},
		"item":  nil,
		"index": -1,
	}
}

// stepTemplateData is one step's template-visible shape.
func stepTemplateData(r StepResult) map[string]any {
	var jsonData any = ""
	if r.Data != nil {
		jsonData = jsonValue{V: r.Data}
	}
	return map[string]any{
		"status": r.Status,
		"text":   r.Text,
		"path":   r.Path,
		"json":   jsonData,
		"meta":   r.Meta,
		"error":  r.Error,
	}
}

// mapStepView is a StepView over a map.
type mapStepView map[string]StepResult

func (m mapStepView) Get(id string) (StepResult, bool) {
	r, ok := m[id]
	return r, ok
}

func (m mapStepView) IDs() []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	return ids
}

// runStepNested executes a step that belongs to a composite node's body, in the
// body's item context. Because the core runs it, the body step keeps its own
// when, retry, timeout and continueOnError.
func (ex *executor) runStepNested(ctx context.Context, step Step, item any, index int) StepResult {
	res := ex.runStep(ctx, &step, item, index)
	ex.mu.Lock()
	ex.results[step.ID] = res
	ex.mu.Unlock()
	return res
}

// runStepsNested executes body steps in declaration order, stopping at the
// first failure unless that step sets continueOnError.
func (ex *executor) runStepsNested(ctx context.Context, steps []Step, item any, index int) ([]StepResult, error) {
	out := make([]StepResult, 0, len(steps))
	for i := range steps {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		r := ex.runStepNested(ctx, steps[i], item, index)
		out = append(out, r)
		if r.Status == "error" && !steps[i].ContinueOnError {
			return out, fmt.Errorf("step %q: %s", steps[i].ID, r.Error)
		}
	}
	return out, nil
}

// emit sends a step-level event when a sink is configured.
func (ex *executor) emit(ctx context.Context, ev Event) {
	if ex.env.Events == nil {
		return
	}
	ex.env.Events.Emit(ctx, ev)
}

func contains(list []int, v int) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func sanitizeFileName(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, name)
}

func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return string([]rune(s)[:max/2]) + "..."
}

func stepTimeoutError(step *Step) error {
	if step.TimeoutSeconds > 0 {
		return fmt.Errorf("timed out after %ds", step.TimeoutSeconds)
	}
	return fmt.Errorf("timed out: enclosing deadline exceeded")
}

// errorsIsDeadline reports whether the step's own deadline (not the caller's)
// has expired.
func errorsIsDeadline(stepCtx, parent context.Context) bool {
	if stepCtx.Err() == nil {
		return false
	}
	return parent.Err() == nil
}

// isFinalError reports whether a retry should be skipped: timeouts and
// cancellation are not worth retrying.
func isFinalError(stepCtx, parent context.Context, err error) bool {
	if errorsIsDeadline(stepCtx, parent) {
		return true
	}
	if parent.Err() != nil {
		return true
	}
	return false
}

func errFromString(s string) error {
	if s == "" {
		return nil
	}
	return fmt.Errorf("%s", s)
}

// marshalCompact renders a value as compact JSON, for event payloads.
func marshalCompact(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}