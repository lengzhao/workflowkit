package workflowkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Workflow is one parsed pipeline. It mirrors the definition file, so Go
// callers can build the same structure with a literal.
type Workflow struct {
	// ID is the workflow's name. It comes from the file name when loading from
	// disk, or from the id field when present.
	ID string `json:"id,omitempty"`
	// Description is required: it is what a caller (or a model) routes by.
	Description string `json:"description"`
	// Inputs is the JSON Schema of the arguments Run accepts.
	Inputs map[string]any `json:"inputs,omitempty"`
	// Concurrency caps how many steps of one dependency level run at once.
	// Omitted uses DefaultConcurrency; 0 means no limit.
	Concurrency *int `json:"concurrency,omitempty"`
	// Steps are the pipeline stages, in declaration order.
	Steps []Step `json:"steps"`
	// Result is a template rendered into Result.Text. Optional: without it the
	// caller reads Result.Steps instead.
	Result string `json:"result,omitempty"`
	// Dir is the directory holding the definition file, set by the loaders.
	// Relative paths in a definition (scripts, readJSON) resolve against it.
	Dir string `json:"-"`
	// Path is the definition file itself, for error messages.
	Path string `json:"-"`
}

// Step is one pipeline stage: common fields plus the node's own fields in
// Fields.
type Step struct {
	// ID is unique within the workflow and is how later steps reference this
	// one in templates and dependsOn.
	ID string `json:"id"`
	// Type names a registered node kind.
	Type string `json:"type"`
	// When is an optional template. When it renders to "" / "0" / "false" the
	// step is skipped and recorded with status "skipped".
	When string `json:"when,omitempty"`
	// DependsOn lists the steps this one waits for. A nil slice (field absent)
	// keeps the serial default: wait for the previous step in declaration
	// order. An explicit list — including an empty one — replaces that edge, so
	// dependsOn: [] starts immediately and can run in parallel.
	DependsOn []string `json:"dependsOn,omitempty"`
	// ContinueOnError records a failure and keeps going instead of aborting the
	// run. Downstream steps are not skipped automatically; use When to branch.
	ContinueOnError bool `json:"continueOnError,omitempty"`
	// Retry retries a failing step.
	Retry *Retry `json:"retry,omitempty"`
	// TimeoutSeconds bounds this step's wall clock.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
	// Fields holds the node's own fields (run, prompt, args, url, …), exactly
	// as written. The core does not interpret them; the node decodes them into
	// its own config struct, so adding a node type never changes this struct.
	Fields map[string]any `json:"-"`
}

// Retry is a step's retry policy.
type Retry struct {
	// MaxTries is the total number of attempts (1 means no retry).
	MaxTries int `json:"maxTries"`
	// WaitSeconds is the delay between attempts. 0 means retry immediately.
	WaitSeconds int `json:"waitSeconds,omitempty"`
}

// commonFields are the step keys the core consumes; everything else a step
// declares is passed to the node in Fields.
var commonFields = map[string]bool{
	"id":              true,
	"type":            true,
	"when":            true,
	"dependsOn":       true,
	"continueOnError": true,
	"retry":           true,
	"timeoutSeconds":  true,
}

// UnmarshalJSON splits a step into common fields and node fields.
func (s *Step) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	s.Fields = map[string]any{}
	for key, value := range raw {
		if commonFields[key] {
			continue
		}
		s.Fields[key] = value
	}
	// The common fields are decoded with unknown fields allowed, because the
	// remaining keys are the node's own. A typo in a common field name (such as
	// "timeoutsecond") therefore lands in Fields and is rejected by the node's
	// own config decode, which is strict.
	encoded, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var c struct {
		ID              string   `json:"id"`
		Type            string   `json:"type"`
		When            string   `json:"when"`
		DependsOn       []string `json:"dependsOn"`
		ContinueOnError bool     `json:"continueOnError"`
		Retry           *Retry   `json:"retry"`
		TimeoutSeconds  int      `json:"timeoutSeconds"`
	}
	if err := json.Unmarshal(encoded, &c); err != nil {
		return err
	}
	s.ID, s.Type, s.When = c.ID, c.Type, c.When
	s.DependsOn, s.ContinueOnError = c.DependsOn, c.ContinueOnError
	s.Retry, s.TimeoutSeconds = c.Retry, c.TimeoutSeconds
	return nil
}

// Parse decodes and validates a workflow definition.
//
// Everything checkable at load time is reported here rather than at Run: field
// requirements, template syntax, dependency references, cycles, unknown node
// kinds, node configs, and tool arguments that contain no templates. A model
// generating a definition can therefore validate it with the same code path the
// executor uses, with no separate checker to keep in sync.
func Parse(data []byte) (*Workflow, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	// UseNumber keeps integer literals exact; decoding into any would turn every
	// number into float64 and round integers above 2^53.
	dec.UseNumber()
	var wf Workflow
	if err := dec.Decode(&wf); err != nil {
		return nil, fmt.Errorf("workflow: invalid JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("workflow: invalid JSON: trailing data")
	}
	if err := wf.validate(); err != nil {
		return nil, err
	}
	return &wf, nil
}

// Validate checks an already-built Workflow. Parse calls it.
func (wf *Workflow) Validate() error { return wf.validate() }

// scopeName is the workflow name used in error messages. An inline definition
// (parsed from bytes, with no file behind it) has no id yet, so it reports as
// "(inline)" rather than as an empty string.
func (wf *Workflow) scopeName() string {
	if wf.ID != "" {
		return wf.ID
	}
	return "(inline)"
}

func (wf *Workflow) validate() error {
	scope := wf.scopeName()
	if strings.TrimSpace(wf.Description) == "" {
		return fmt.Errorf("workflow %q: description is required (callers route by it)", scope)
	}
	if len(wf.Steps) == 0 {
		return fmt.Errorf("workflow %q: at least one step is required", scope)
	}
	if err := ValidateConcurrency(wf.Concurrency); err != nil {
		return fmt.Errorf("workflow %q: %w", scope, err)
	}
	if err := validateStepList(scope, wf.Steps); err != nil {
		return err
	}
	if err := wf.validateGraph(); err != nil {
		return err
	}
	if strings.TrimSpace(wf.Result) != "" {
		if _, err := parseTemplate(scope+".result", wf.Result, nil); err != nil {
			return err
		}
	}
	return nil
}

// validateStepList validates ids, kinds, node configs and sub-steps for one
// list of steps. Composite nodes call it for their own bodies through Parse's
// recursion.
func validateStepList(scope string, steps []Step) error {
	seen := map[string]bool{}
	for i, step := range steps {
		if strings.TrimSpace(step.ID) == "" {
			return fmt.Errorf("workflow %q: step %d has no id", scope, i)
		}
		if seen[step.ID] {
			return fmt.Errorf("workflow %q: duplicate step id %q", scope, step.ID)
		}
		seen[step.ID] = true
		if err := validateStep(scope, step); err != nil {
			return err
		}
	}
	return nil
}

func validateStep(scope string, step Step) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("workflow %q step %q: %s", scope, step.ID, fmt.Sprintf(format, args...))
	}
	if strings.TrimSpace(step.Type) == "" {
		return fail("type is required")
	}
	reg, ok := lookup(step.Type)
	if !ok {
		return fail("unknown type %q (registered: %s)", step.Type, strings.Join(ListTypes(), ", "))
	}
	if step.TimeoutSeconds < 0 {
		return fail("timeoutSeconds must not be negative")
	}
	if step.Retry != nil {
		if step.Retry.MaxTries < 0 {
			return fail("retry.maxTries must not be negative")
		}
		if step.Retry.WaitSeconds < 0 {
			return fail("retry.waitSeconds must not be negative")
		}
	}
	if step.When != "" {
		if _, err := parseTemplate(scope+"."+step.ID+".when", step.When, nil); err != nil {
			return fail("invalid when template: %v", err)
		}
	}
	if err := reg.cfgSchema.Validate(step.Fields); err != nil {
		return fail("config does not match the %s schema: %v", step.Type, err)
	}
	// Construct once so node-level requirements (a missing field, a bad enum
	// value) are reported at load time too.
	if err := checkConfig(step.Type, step.Fields); err != nil {
		return fail("%v", err)
	}
	// Sub-steps declared by composite nodes are validated recursively.
	for _, field := range reg.info.SubSteps {
		raw, ok := step.Fields[field]
		if !ok {
			continue
		}
		sub, err := decodeSteps(raw)
		if err != nil {
			return fail("%s: %v", field, err)
		}
		if err := validateStepList(scope+"/"+step.ID, sub); err != nil {
			return err
		}
	}
	return nil
}

// decodeSteps reads a sub-step list from a decoded JSON value.
func decodeSteps(raw any) ([]Step, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid steps: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.UseNumber()
	var steps []Step
	if err := dec.Decode(&steps); err != nil {
		return nil, fmt.Errorf("invalid steps: %w", err)
	}
	return steps, nil
}

// withScopedID returns a copy of the step whose id is scoped, used when a
// composite body runs under a per-item prefix.
func (s Step) withScopedID(id string) *Step {
	next := s
	next.ID = id
	return &next
}

// SubSteps reads a sub-step list from a node's config, for composite nodes.
// It is the runtime counterpart of the validation Parse performs.
func SubSteps(cfg map[string]any, field string) ([]Step, error) {
	raw, ok := cfg[field]
	if !ok || raw == nil {
		return nil, nil
	}
	return decodeSteps(raw)
}
