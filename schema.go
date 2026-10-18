package workflowkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/template"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// validateAgainstSchema checks a value against a JSON Schema. Both node configs
// at load time and model-step outputs at run time go through here.
func validateAgainstSchema(schema map[string]any, value any) error {
	doc, err := toJSONDocument(schema)
	if err != nil {
		return fmt.Errorf("parse schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("schema.json", doc); err != nil {
		return fmt.Errorf("load schema: %w", err)
	}
	compiled, err := compiler.Compile("schema.json")
	if err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	doc2, err := toJSONDocument(value)
	if err != nil {
		return fmt.Errorf("parse value: %w", err)
	}
	if err := compiled.Validate(doc2); err != nil {
		return err
	}
	return nil
}

// toJSONDocument round-trips a Go value through encoding/json so the schema
// library sees the same shapes a definition file produces.
func toJSONDocument(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(raw))
}

// Schema returns the JSON Schema of a workflow definition. It is what lets a
// caller — including a model — generate a definition and check its shape before
// Parse validates the semantics.
func Schema() map[string]any {
	return map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type":    "object",
		"required": []any{
			"description", "steps",
		},
		"properties": map[string]any{
			"id":          map[string]any{"type": "string", "description": "workflow name; defaults to the file name"},
			"description": map[string]any{"type": "string", "description": "one line; callers route by it"},
			"inputs":      map[string]any{"type": "object", "description": "JSON Schema of the arguments Run accepts"},
			"concurrency": map[string]any{"type": "integer", "minimum": 0, "description": "max steps of one dependency level running at once (default 4)"},
			"steps":       StepsSchema(),
			"result":      map[string]any{"type": "string", "description": "template rendered into Result.Text; optional"},
		},
		"additionalProperties": false,
	}
}

// Graph helpers: dependency resolution, cycle detection, level grouping and
// the critical-path timeout estimate.

// dependencies returns the ids step i waits for. A step without an explicit
// dependsOn waits for the previous step in declaration order (the serial
// default, which keeps the simplest definitions unchanged); an explicit list —
// even an empty one — replaces that implicit edge.
func dependencies(steps []Step, i int) []string {
	step := steps[i]
	if step.DependsOn != nil {
		return step.DependsOn
	}
	if i == 0 {
		return nil
	}
	return []string{steps[i-1].ID}
}

func stepIndex(steps []Step) map[string]int {
	index := make(map[string]int, len(steps))
	for i := range steps {
		index[steps[i].ID] = i
	}
	return index
}

// validateGraph checks dependsOn references and acyclicity over the resolved
// edges, implicit serial edges included.
func (wf *Workflow) validateGraph() error {
	if err := validateGraph(wf.ID, wf.Steps); err != nil {
		return err
	}
	return nil
}

func validateGraph(scope string, steps []Step) error {
	index := stepIndex(steps)
	for i := range steps {
		step := &steps[i]
		seen := map[string]bool{}
		for _, dep := range step.DependsOn {
			switch {
			case dep == step.ID:
				return fmt.Errorf("workflow %q step %q: dependsOn must not reference itself", scope, step.ID)
			case seen[dep]:
				return fmt.Errorf("workflow %q step %q: duplicate dependsOn %q", scope, step.ID, dep)
			}
			seen[dep] = true
			if _, ok := index[dep]; !ok {
				return fmt.Errorf("workflow %q step %q: dependsOn %q matches no step id", scope, step.ID, dep)
			}
		}
	}
	// Depth-first cycle check over resolved edges.
	const (
		white = iota
		gray
		black
	)
	color := make([]int, len(steps))
	var visit func(i int) error
	visit = func(i int) error {
		color[i] = gray
		for _, dep := range dependencies(steps, i) {
			j, ok := index[dep]
			if !ok {
				continue
			}
			switch color[j] {
			case gray:
				return fmt.Errorf("workflow %q: dependency cycle between steps %q and %q", scope, steps[i].ID, dep)
			case white:
				if err := visit(j); err != nil {
					return err
				}
			}
		}
		color[i] = black
		return nil
	}
	for i := range steps {
		if color[i] == white {
			if err := visit(i); err != nil {
				return err
			}
		}
	}
	return nil
}

// levels groups step indices into dependency levels: every step's dependencies
// sit in strictly earlier levels, so steps within one level can run
// concurrently — they only read data produced by earlier levels. Declaration
// order is preserved inside a level. Returns nil on a cycle.
func levels(steps []Step) [][]int {
	index := stepIndex(steps)
	unmet := make([]int, len(steps))
	dependents := make([][]int, len(steps))
	for i := range steps {
		seen := map[int]bool{}
		for _, dep := range dependencies(steps, i) {
			j, ok := index[dep]
			if !ok || seen[j] {
				continue
			}
			seen[j] = true
			dependents[j] = append(dependents[j], i)
			unmet[i]++
		}
	}
	var frontier []int
	for i := range steps {
		if unmet[i] == 0 {
			frontier = append(frontier, i)
		}
	}
	var out [][]int
	done := 0
	for len(frontier) > 0 {
		out = append(out, frontier)
		done += len(frontier)
		var next []int
		for _, i := range frontier {
			for _, j := range dependents[i] {
				unmet[j]--
				if unmet[j] == 0 {
					next = append(next, j)
				}
			}
		}
		sort.Ints(next)
		frontier = next
	}
	if done < len(steps) {
		return nil
	}
	return out
}

// estimateTimeout is the default per-step timeout used by the duration
// estimate when a step declares none. Agent steps use the larger default.
const (
	estimateStepTimeout  = 120
	estimateAgentTimeout = 900
)

// EstimateDuration estimates the run's wall clock as the critical path (the
// longest dependency chain) of per-step timeouts, in seconds. Serial workflows
// form one chain, so the estimate equals the sum of all steps; parallel
// branches overlap instead of adding up. The library never enforces an outer
// deadline — that belongs to the caller, who can size it with this number.
func (wf *Workflow) EstimateDuration() int {
	steps := wf.Steps
	index := stepIndex(steps)
	memo := make([]int, len(steps))
	known := make([]bool, len(steps))
	var chain func(i int) int
	chain = func(i int) int {
		if known[i] {
			return memo[i]
		}
		best := 0
		for _, dep := range dependencies(steps, i) {
			j, ok := index[dep]
			if !ok {
				continue
			}
			if c := chain(j); c > best {
				best = c
			}
		}
		memo[i] = best + stepTimeoutSeconds(&steps[i])
		known[i] = true
		return memo[i]
	}
	total := 0
	for i := range steps {
		if c := chain(i); c > total {
			total = c
		}
	}
	return total
}

func stepTimeoutSeconds(step *Step) int {
	if step.TimeoutSeconds > 0 {
		return step.TimeoutSeconds
	}
	switch step.Type {
	case "agent":
		return estimateAgentTimeout
	default:
		return estimateStepTimeout
	}
}

// evalWhen renders a step's when template and maps the result to a boolean:
// empty, "0" and "false" (any case) are falsy, everything else truthy.
// Conditions compose with the built-in comparison functions.
func evalWhen(name, when string, data map[string]any) (bool, error) {
	rendered, err := renderTemplate(name+".when", when, data)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(rendered)) {
	case "", "0", "false":
		return false, nil
	}
	return true, nil
}

// ensure parseTemplate stays referenced from this file's package tests when
// template.go's helper is exercised only through workflow.go.
var _ = template.New
// ValidateSchema checks a value against a JSON Schema. It is exported for node
// implementations that need the same validation the core performs.
func ValidateSchema(schema map[string]any, value any) error {
	return validateAgainstSchema(schema, value)
}

// CompileSchema reports whether a JSON Schema is well-formed. Node
// constructors call it so a malformed schema fails at load time rather than at
// the first Run.
func CompileSchema(schema map[string]any) error {
	doc, err := toJSONDocument(schema)
	if err != nil {
		return fmt.Errorf("parse schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("schema.json", doc); err != nil {
		return fmt.Errorf("load schema: %w", err)
	}
	if _, err := compiler.Compile("schema.json"); err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	return nil
}
