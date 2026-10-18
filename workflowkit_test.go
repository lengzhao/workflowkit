package workflowkit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lengzhao/workflowkit"
	// The blank import registers the built-in node kinds, exactly as a host does.
	_ "github.com/lengzhao/workflowkit/nodes"
)

// ---------------------------------------------------------------------------
// Mock host capabilities
// ---------------------------------------------------------------------------

type mockShell struct {
	mu       sync.Mutex
	commands []string
	out      map[string]string
	failOn   string
	delay    time.Duration
}

func (m *mockShell) Run(ctx context.Context, command string, timeout *float64) (string, string, int, error) {
	m.mu.Lock()
	m.commands = append(m.commands, command)
	out, fail := m.out[command], m.failOn == command
	delay := m.delay
	m.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", "", -1, ctx.Err()
		}
	}
	if fail {
		return "", "boom", 1, nil
	}
	if out == "" {
		out = "ok"
	}
	return out, "", 0, nil
}

type mockTools struct {
	mu    sync.Mutex
	calls []workflowkit.ToolCall
	fn    func(call workflowkit.ToolCall) (workflowkit.ToolResult, error)
}

func (m *mockTools) Execute(ctx context.Context, call workflowkit.ToolCall) (workflowkit.ToolResult, error) {
	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.mu.Unlock()
	if m.fn != nil {
		return m.fn(call)
	}
	return workflowkit.ToolResult{Content: `{"status":"ok"}`}, nil
}

type mockLLM struct{ reply string }

func (m *mockLLM) Complete(ctx context.Context, model, prompt string, schema map[string]any) (string, error) {
	return m.reply, nil
}

type mockAgents struct{ summary string }

func (m *mockAgents) Run(ctx context.Context, agent, task string, timeout *int) (workflowkit.Summary, error) {
	return workflowkit.Summary{Text: m.summary, Session: "sess-1", Status: "completed", Steps: 3}, nil
}

// writingShell writes its output to a file in the run directory as well as
// returning it, so tests can exercise readJSON.
type writingShell struct {
	inner *mockShell
	step  string
	body  string
	dir   string
}

func (w *writingShell) Run(ctx context.Context, command string, timeout *float64) (string, string, int, error) {
	out, errOut, code, err := w.inner.Run(ctx, command, timeout)
	if command == w.step && w.dir != "" {
		_ = os.WriteFile(filepath.Join(w.dir, "a.json"), []byte(w.body), 0o644)
	}
	return out, errOut, code, err
}

type tempFiles struct{ root string }

func (f tempFiles) RunDir(runID string) (string, error) {
	dir := filepath.Join(f.root, runID)
	return dir, os.MkdirAll(dir, 0o755)
}

// recordingFiles remembers the run directory so a test's shell double can write
// into it, and optionally runs a callback with that directory.
type recordingFiles struct {
	inner tempFiles
	on    func(dir string)
}

func (f recordingFiles) RunDir(runID string) (string, error) {
	dir, err := f.inner.RunDir(runID)
	if err == nil && f.on != nil {
		f.on(dir)
	}
	return dir, err
}

func testEnv(t *testing.T) (workflowkit.Env, *mockShell, *mockTools) {
	t.Helper()
	shell := &mockShell{out: map[string]string{}, failOn: ""}
	tools := &mockTools{}
	env := workflowkit.Env{
		Shell:  shell,
		Tools:  tools,
		LLM:    &mockLLM{reply: `{"ok":true}`},
		Agents: &mockAgents{summary: "looks fine"},
		Files:  tempFiles{root: t.TempDir()},
	}
	return env, shell, tools
}

// mustParse parses a definition and fails the test on error.
func mustParse(t *testing.T, def string) *workflowkit.Workflow {
	t.Helper()
	wf, err := workflowkit.Parse([]byte(def))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	return wf
}

// ---------------------------------------------------------------------------
// Load-time validation
// ---------------------------------------------------------------------------

func TestParseRejectsInvalidDefinitions(t *testing.T) {
	cases := []struct {
		name string
		def  string
		want string
	}{
		{"no description", `{"steps":[{"id":"a","type":"script","run":"x"}]}`, "description is required"},
		{"no steps", `{"description":"d","steps":[]}`, "at least one step is required"},
		{"trailing data", `{"description":"d","steps":[{"id":"a","type":"script","run":"x"}]} {}`, "trailing data"},
		{"step without id", `{"description":"d","steps":[{"type":"script","run":"x"}]}`, "has no id"},
		{"duplicate id", `{"description":"d","steps":[
			{"id":"a","type":"script","run":"x"},
			{"id":"a","type":"script","run":"y"}]}`, "duplicate step id"},
		{"unknown type", `{"description":"d","steps":[{"id":"a","type":"nope"}]}`, "unknown type"},
		{"missing required node field", `{"description":"d","steps":[{"id":"a","type":"script"}]}`, "missing property 'run'"},
		{"typo in node field", `{"description":"d","steps":[{"id":"a","type":"script","runn":"x"}]}`, "additional properties 'runn' not allowed"},
		{"bad when template", `{"description":"d","steps":[{"id":"a","type":"script","run":"x","when":"{{ .a "}]}`, "invalid when template"},
		{"dependsOn unknown id", `{"description":"d","steps":[
			{"id":"a","type":"script","run":"x","dependsOn":["zz"]}]}`, "matches no step id"},
		{"dependsOn self", `{"description":"d","steps":[
			{"id":"a","type":"script","run":"x","dependsOn":["a"]}]}`, "must not reference itself"},
		{"negative timeout", `{"description":"d","steps":[{"id":"a","type":"script","run":"x","timeoutSeconds":-1}]}`, "must not be negative"},
		{"bad outputSchema", `{"description":"d","steps":[{"id":"a","type":"model","prompt":"p","outputSchema":{"type":"nonsense"}}]}`, "outputSchema"},
		{"bad retry", `{"description":"d","steps":[{"id":"a","type":"script","run":"x","retry":{"maxTries":-1}}]}`, "must not be negative"},
		{"bad concurrency", `{"description":"d","concurrency":-1,"steps":[{"id":"a","type":"script","run":"x"}]}`, "must not be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := workflowkit.Parse([]byte(tc.def))
			if err == nil {
				t.Fatalf("expected an error, got none")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestParseRejectsCycleAcrossImplicitEdges(t *testing.T) {
	// b depends on a implicitly (serial default); a explicitly depends on b.
	def := `{"description":"d","steps":[
		{"id":"a","type":"script","run":"x","dependsOn":["b"]},
		{"id":"b","type":"script","run":"y"}]}`
	_, err := workflowkit.Parse([]byte(def))
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected a cycle error, got %v", err)
	}
}

func TestUnknownTypeListsRegisteredKinds(t *testing.T) {
	_, err := workflowkit.Parse([]byte(`{"description":"d","steps":[{"id":"a","type":"nope"}]}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, kind := range []string{"script", "tool", "model", "agent", "foreach"} {
		if !strings.Contains(err.Error(), kind) {
			t.Fatalf("error %q should list the registered kind %q", err, kind)
		}
	}
}

func TestParseValidatesSubSteps(t *testing.T) {
	def := `{"description":"d","steps":[{"id":"each","type":"foreach",
		"items":"[1,2]","body":[{"id":"inner","type":"nope"}]}]}`
	_, err := workflowkit.Parse([]byte(def))
	if err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Fatalf("expected sub-step validation to fail, got %v", err)
	}
}

func TestParseValidatesInputsSchema(t *testing.T) {
	wf := mustParse(t, `{"description":"d",
		"inputs":{"type":"object","required":["date"],"properties":{"date":{"type":"string"}}},
		"steps":[{"id":"a","type":"script","run":"x"}]}`)
	env, _, _ := testEnv(t)
	if _, err := workflowkit.Run(context.Background(), wf, map[string]any{}, env); err == nil {
		t.Fatal("expected inputs validation to fail")
	}
	if _, err := workflowkit.Run(context.Background(), wf, map[string]any{"date": "2026-01-01"}, env); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Execution semantics
// ---------------------------------------------------------------------------

func TestSerialDefaultOrder(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"a","type":"script","run":"cmd-a"},
		{"id":"b","type":"script","run":"cmd-b"},
		{"id":"c","type":"script","run":"cmd-c"}]}`)
	env, shell, _ := testEnv(t)
	if _, err := workflowkit.Run(context.Background(), wf, nil, env); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	got := strings.Join(shell.commands, "|")
	if !strings.Contains(got, "cmd-a") || strings.Index(got, "cmd-a") > strings.Index(got, "cmd-b") {
		t.Fatalf("steps did not run in declaration order: %v", shell.commands)
	}
}

func TestDependsOnEmptyRunsInParallel(t *testing.T) {
	// Two branches, each 100ms; with dependsOn: [] they must overlap.
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"a","type":"script","run":"cmd-a","dependsOn":[]},
		{"id":"b","type":"script","run":"cmd-b","dependsOn":[]}]}`)
	env, shell, _ := testEnv(t)
	shell.delay = 100 * time.Millisecond
	start := time.Now()
	if _, err := workflowkit.Run(context.Background(), wf, nil, env); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 180*time.Millisecond {
		t.Fatalf("steps did not run in parallel (took %s)", elapsed)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	wf := mustParse(t, `{"description":"d","concurrency":2,"steps":[
		{"id":"a","type":"script","run":"cmd-a","dependsOn":[]},
		{"id":"b","type":"script","run":"cmd-b","dependsOn":[]},
		{"id":"c","type":"script","run":"cmd-c","dependsOn":[]},
		{"id":"d","type":"script","run":"cmd-d","dependsOn":[]}]}`)
	env, shell, _ := testEnv(t)
	shell.delay = 60 * time.Millisecond
	start := time.Now()
	if _, err := workflowkit.Run(context.Background(), wf, nil, env); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	// 4 steps at concurrency 2 → two rounds of ~60ms. Allow scheduling slack.
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("concurrency limit not applied (took %s)", elapsed)
	}
}

func TestWhenSkipsStep(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"a","type":"script","run":"cmd-a"},
		{"id":"b","type":"script","run":"cmd-b","when":"{{ eq .steps.a.text \"never\" }}"}]}`)
	env, _, _ := testEnv(t)
	res, err := workflowkit.Run(context.Background(), wf, nil, env)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Steps["b"].Status != "skipped" {
		t.Fatalf("expected b to be skipped, got %q", res.Steps["b"].Status)
	}
}

func TestFailureAbortsNextLevel(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"a","type":"script","run":"cmd-a"},
		{"id":"b","type":"script","run":"cmd-b","dependsOn":["a"]}]}`)
	env, shell, _ := testEnv(t)
	shell.failOn = "cmd-a"
	res, err := workflowkit.Run(context.Background(), wf, nil, env)
	if err == nil {
		t.Fatal("expected the run to fail")
	}
	if res.Status != "error" {
		t.Fatalf("expected status error, got %q", res.Status)
	}
	if res.Steps["a"].Status != "error" {
		t.Fatalf("expected a to be recorded as error, got %q", res.Steps["a"].Status)
	}
	if res.Steps["b"].Status != "skipped" {
		t.Fatalf("expected b not to run, got %q", res.Steps["b"].Status)
	}
}

func TestContinueOnErrorKeepsGoing(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"a","type":"script","run":"cmd-a","continueOnError":true},
		{"id":"b","type":"script","run":"cmd-b"}]}`)
	env, shell, _ := testEnv(t)
	shell.failOn = "cmd-a"
	res, err := workflowkit.Run(context.Background(), wf, nil, env)
	if err != nil {
		t.Fatalf("run should not fail when continueOnError is set: %v", err)
	}
	if res.Steps["a"].Status != "error" || res.Steps["b"].Status != "ok" {
		t.Fatalf("unexpected statuses: a=%q b=%q", res.Steps["a"].Status, res.Steps["b"].Status)
	}
}

func TestRetry(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"a","type":"script","run":"cmd-a","retry":{"maxTries":3,"waitSeconds":0}}]}`)
	env, shell, _ := testEnv(t)
	shell.failOn = "cmd-a" // always fails → 3 attempts
	res, err := workflowkit.Run(context.Background(), wf, nil, env)
	if err == nil {
		t.Fatal("expected failure")
	}
	if len(shell.commands) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(shell.commands))
	}
	if res.Steps["a"].Status != "error" {
		t.Fatalf("expected error status")
	}
}

func TestStepTimeout(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"a","type":"script","run":"cmd-a","timeoutSeconds":1}]}`)
	env, shell, _ := testEnv(t)
	shell.delay = 3 * time.Second
	start := time.Now()
	res, err := workflowkit.Run(context.Background(), wf, nil, env)
	if err == nil {
		t.Fatal("expected a timeout failure")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout was not applied (took %s)", elapsed)
	}
	if !strings.Contains(res.Steps["a"].Error, "timed out") {
		t.Fatalf("expected a timeout error, got %q", res.Steps["a"].Error)
	}
}

func TestCancelStopsNewLevels(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"a","type":"script","run":"cmd-a"},
		{"id":"b","type":"script","run":"cmd-b"}]}`)
	env, shell, _ := testEnv(t)
	shell.delay = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	res, err := workflowkit.Run(ctx, wf, nil, env)
	if err == nil {
		t.Fatal("expected a cancellation error")
	}
	if res == nil {
		t.Fatal("expected a result even on cancellation")
	}
}

// ---------------------------------------------------------------------------
// Data passing
// ---------------------------------------------------------------------------

func TestLargeOutputSpillsToFile(t *testing.T) {
	big := strings.Repeat("x", workflowkit.SpillThreshold+100)
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"a","type":"script","run":"cmd-a"},
		{"id":"b","type":"script","run":"cmd-b"}]}`)
	env, shell, _ := testEnv(t)
	shell.out["cmd-a"] = big
	res, err := workflowkit.Run(context.Background(), wf, nil, env)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	a := res.Steps["a"]
	if a.Path == "" {
		t.Fatal("expected the large output to be spilled to a path")
	}
	if len(a.Text) > workflowkit.SpillThreshold+200 {
		t.Fatalf("inline text should be truncated, got %d bytes", len(a.Text))
	}
	data, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatalf("spilled file unreadable: %v", err)
	}
	if string(data) != big {
		t.Fatal("spilled file does not hold the full output")
	}
	if strings.Contains(res.Text, big) {
		t.Fatal("the result must not carry the large payload")
	}
}

func TestBigIntegerSurvivesThreeHops(t *testing.T) {
	// A 19-digit integer above 2^53 travels a JSON file → readJSON → dig →
	// tool arg without ever becoming a float64.
	const big = "9007199254740993"
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"a","type":"script","run":"cmd-a"},
		{"id":"b","type":"tool","tool":"store",
		 "args":{"version":"{{ dig (readJSON (printf \"%s/a.json\" .workflow.runDir)) \"version\" }}"}}]}`)
	env, shell, tools := testEnv(t)
	ws := &writingShell{inner: shell, step: "cmd-a", body: `{"version":` + big + `}`}
	env.Shell = ws
	env.Files = recordingFiles{inner: tempFiles{root: t.TempDir()}, on: func(dir string) { ws.dir = dir }}
	if _, err := workflowkit.Run(context.Background(), wf, nil, env); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if len(tools.calls) != 1 {
		t.Fatalf("expected one tool call, got %d", len(tools.calls))
	}
	got := fmt.Sprintf("%v", tools.calls[0].Input["version"])
	if got != big {
		t.Fatalf("integer lost precision: got %s, want %s", got, big)
	}
}

// TestBigIntegerSurvivesStepToStep checks the other path: a step's structured
// output read by the next step through dig, with no file involved.
func TestBigIntegerSurvivesStepToStep(t *testing.T) {
	const big = "9007199254740993"
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"a","type":"tool","tool":"emit","args":{"n":1}},
		{"id":"b","type":"tool","tool":"store",
		 "args":{"version":"{{ dig .steps.a.json \"version\" }}"}}]}`)
	env, _, tools := testEnv(t)
	env.Tools = &mockTools{fn: func(call workflowkit.ToolCall) (workflowkit.ToolResult, error) {
		if call.Name == "emit" {
			return workflowkit.ToolResult{Content: `{"version":` + big + `}`}, nil
		}
		return workflowkit.ToolResult{Content: "ok"}, nil
	}}
	if _, err := workflowkit.Run(context.Background(), wf, nil, env); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	_ = tools
}


func TestObjectArgIsPassedAsObject(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"a","type":"tool","tool":"emit"},
		{"id":"b","type":"tool","tool":"store",
		 "args":{"payload":"{{ .steps.a.json }}"}}]}`)
	env, _, _ := testEnv(t)
	seen := map[string]any{}
	env.Tools = &mockTools{fn: func(call workflowkit.ToolCall) (workflowkit.ToolResult, error) {
		if call.Name == "emit" {
			return workflowkit.ToolResult{Content: `{"rows":[1,2,3]}`}, nil
		}
		seen = call.Input
		return workflowkit.ToolResult{Content: "ok"}, nil
	}}
	if _, err := workflowkit.Run(context.Background(), wf, nil, env); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	payload, ok := seen["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload should be an object, got %T", seen["payload"])
	}
	rows, ok := payload["rows"].([]any)
	if !ok || len(rows) != 3 {
		t.Fatalf("payload shape wrong: %#v", payload)
	}
}

func TestResultTemplateRenders(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"a","type":"script","run":"cmd-a"}],
		"result":"status={{ .steps.a.status }}"}`)
	env, _, _ := testEnv(t)
	res, err := workflowkit.Run(context.Background(), wf, nil, env)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Text != "status=ok" {
		t.Fatalf("unexpected result text %q", res.Text)
	}
}

// ---------------------------------------------------------------------------
// Node kinds
// ---------------------------------------------------------------------------

func TestModelStepValidatesOutputSchema(t *testing.T) {
	def := `{"description":"d","steps":[{"id":"m","type":"model","prompt":"p",
		"outputSchema":{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}}]}`
	wf := mustParse(t, def)

	env, _, _ := testEnv(t)
	env.LLM = &mockLLM{reply: `{"ok":true}`}
	if _, err := workflowkit.Run(context.Background(), wf, nil, env); err != nil {
		t.Fatalf("valid model output should pass: %v", err)
	}

	env.LLM = &mockLLM{reply: `{"ok":"yes"}`}
	if _, err := workflowkit.Run(context.Background(), wf, nil, env); err == nil {
		t.Fatal("model output not matching outputSchema must fail")
	}

	env.LLM = &mockLLM{reply: "not json"}
	if _, err := workflowkit.Run(context.Background(), wf, nil, env); err == nil {
		t.Fatal("non-JSON model output must fail")
	}
}

func TestModelStepPromptLimit(t *testing.T) {
	wf := mustParse(t, `{"description":"d","inputs":{"type":"object","properties":{"x":{"type":"string"}}},
		"steps":[{"id":"m","type":"model","prompt":"{{ .inputs.x }}"}]}`)
	env, _, _ := testEnv(t)
	_, err := workflowkit.Run(context.Background(), wf, map[string]any{"x": strings.Repeat("y", 20000)}, env)
	if err == nil || !strings.Contains(err.Error(), "maxInputBytes") {
		t.Fatalf("expected a maxInputBytes failure, got %v", err)
	}
}

func TestAgentStep(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[{"id":"a","type":"agent","agent":"auditor","task":"check it"}]}`)
	env, _, _ := testEnv(t)
	res, err := workflowkit.Run(context.Background(), wf, nil, env)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Steps["a"].Text != "looks fine" {
		t.Fatalf("unexpected summary %q", res.Steps["a"].Text)
	}
	if res.Steps["a"].Meta["session"] != "sess-1" {
		t.Fatalf("expected the child session in meta, got %#v", res.Steps["a"].Meta)
	}
}

func TestMissingCapabilityNamesStepAndCapability(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[{"id":"m","type":"model","prompt":"p"}]}`)
	_, err := workflowkit.Run(context.Background(), wf, nil, workflowkit.Env{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "m") || !strings.Contains(err.Error(), "LLM") {
		t.Fatalf("error should name the step and the capability, got %q", err)
	}
}

func TestForeach(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"each","type":"foreach","items":"[1,2,3]","concurrency":2,
		 "body":[{"id":"save","type":"tool","tool":"store","args":{"n":"{{ .item }}"}}]}]}`)
	env, _, tools := testEnv(t)
	res, err := workflowkit.Run(context.Background(), wf, nil, env)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if len(tools.calls) != 3 {
		t.Fatalf("expected 3 tool calls, got %d", len(tools.calls))
	}
	if res.Steps["each"].Meta["count"] != 3 {
		t.Fatalf("expected count 3, got %#v", res.Steps["each"].Meta)
	}
}

func TestForeachEmptyItemsSkips(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"each","type":"foreach","items":"[]",
		 "body":[{"id":"save","type":"tool","tool":"store"}]}]}`)
	env, _, tools := testEnv(t)
	if _, err := workflowkit.Run(context.Background(), wf, nil, env); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if len(tools.calls) != 0 {
		t.Fatalf("expected no calls, got %d", len(tools.calls))
	}
}

func TestForeachItemErrorContinues(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"each","type":"foreach","items":"[1,2,3]","onItemError":"continue",
		 "body":[{"id":"save","type":"tool","tool":"store","args":{"n":"{{ .item }}"}}]}]}`)
	env, _, _ := testEnv(t)
	env.Tools = &mockTools{fn: func(call workflowkit.ToolCall) (workflowkit.ToolResult, error) {
		if fmt.Sprintf("%v", call.Input["n"]) == "2" {
			return workflowkit.ToolResult{}, fmt.Errorf("no")
		}
		return workflowkit.ToolResult{Content: "ok"}, nil
	}}
	res, err := workflowkit.Run(context.Background(), wf, nil, env)
	if err != nil {
		t.Fatalf("onItemError=continue should not fail the run: %v", err)
	}
	if res.Steps["each"].Meta["failed"] != 1 {
		t.Fatalf("expected 1 failed item, got %#v", res.Steps["each"].Meta)
	}
}

func TestForeachItemErrorFailsByDefault(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"each","type":"foreach","items":"[1]",
		 "body":[{"id":"save","type":"tool","tool":"store"}]}]}`)
	env, _, _ := testEnv(t)
	env.Tools = &mockTools{fn: func(call workflowkit.ToolCall) (workflowkit.ToolResult, error) {
		return workflowkit.ToolResult{}, fmt.Errorf("no")
	}}
	if _, err := workflowkit.Run(context.Background(), wf, nil, env); err == nil {
		t.Fatal("expected the run to fail")
	}
}

func TestForeachBodyRetryApplies(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"each","type":"foreach","items":"[1]",
		 "body":[{"id":"save","type":"tool","tool":"store","retry":{"maxTries":3}}]}]}`)
	env, _, _ := testEnv(t)
	attempts := 0
	env.Tools = &mockTools{fn: func(call workflowkit.ToolCall) (workflowkit.ToolResult, error) {
		attempts++
		return workflowkit.ToolResult{}, fmt.Errorf("no")
	}}
	_, _ = workflowkit.Run(context.Background(), wf, nil, env)
	if attempts != 3 {
		t.Fatalf("expected 3 attempts inside the body, got %d", attempts)
	}
}

// ---------------------------------------------------------------------------
// Custom node types
// ---------------------------------------------------------------------------

// evenNode is a host-defined node kind: it fails on odd input, showing that a
// custom type gets conditionals, retries and timeouts from the core for free.
type evenConfig struct {
	Value int `json:"value"`
}

type evenNode struct{ cfg evenConfig }

func (n *evenNode) Run(ctx context.Context, env workflowkit.Env, c *workflowkit.Ctx) (workflowkit.Output, error) {
	if n.cfg.Value%2 != 0 {
		return workflowkit.Output{}, fmt.Errorf("odd")
	}
	return workflowkit.Output{Text: fmt.Sprintf("even:%d", n.cfg.Value), Data: map[string]any{"value": n.cfg.Value}}, nil
}

func init() {
	workflowkit.Register("test-even", func(cfg evenConfig) (workflowkit.Node, error) {
		return &evenNode{cfg: cfg}, nil
	},
		workflowkit.WithDescription("test node: succeeds on even values"),
		workflowkit.WithConfig(evenConfig{}),
		workflowkit.WithExample(map[string]any{"id": "e", "type": "test-even", "value": 2}),
	)
}

func TestCustomNodeType(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"e","type":"test-even","value":4},
		{"id":"t","type":"tool","tool":"store","args":{"v":"{{ dig .steps.e.json \"value\" }}"}}]}`)
	env, _, tools := testEnv(t)
	res, err := workflowkit.Run(context.Background(), wf, nil, env)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Steps["e"].Text != "even:4" {
		t.Fatalf("unexpected text %q", res.Steps["e"].Text)
	}
	if fmt.Sprintf("%v", tools.calls[0].Input["v"]) != "4" {
		t.Fatalf("custom node output did not reach the tool: %#v", tools.calls[0].Input)
	}
}

func TestCustomNodeGetsCoreFeatures(t *testing.T) {
	// An odd value fails, and retry from the core retries it.
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"e","type":"test-even","value":3,"retry":{"maxTries":2},"continueOnError":true},
		{"id":"next","type":"script","run":"cmd-next"}]}`)
	env, shell, _ := testEnv(t)
	res, err := workflowkit.Run(context.Background(), wf, nil, env)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Steps["e"].Status != "error" {
		t.Fatalf("expected error status, got %q", res.Steps["e"].Status)
	}
	if res.Steps["next"].Status != "ok" {
		t.Fatalf("expected the next step to run, got %q", res.Steps["next"].Status)
	}
	if len(shell.commands) != 1 {
		t.Fatalf("expected the following step to run once, got %d", len(shell.commands))
	}
}

func TestDuplicateRegistrationPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering a duplicate kind must panic")
		}
	}()
	workflowkit.Register("script", func() (workflowkit.Node, error) { return &evenNode{}, nil })
}

// ---------------------------------------------------------------------------
// Self-description (what a model reads)
// ---------------------------------------------------------------------------

func TestDescribeAndExampleAgree(t *testing.T) {
	// Every registered kind's example must validate against its own schema:
	// this is the guard against the documentation drifting from the code.
	for _, kind := range workflowkit.ListTypes() {
		info, ok := workflowkit.Describe(kind)
		if !ok {
			t.Fatalf("ListTypes reported %q but Describe does not know it", kind)
		}
		if strings.TrimSpace(info.Description) == "" {
			t.Errorf("%s: description is empty; it is what callers route by", kind)
		}
		if info.Example == nil {
			t.Errorf("%s: example is missing", kind)
			continue
		}
		// The example includes id/type, which are common fields; validate only
		// the kind's own fields against its config schema.
		own := map[string]any{}
		for key, value := range info.Example {
			if key == "id" || key == "type" {
				continue
			}
			own[key] = value
		}
		if info.Config != nil {
			if err := workflowkit.ValidateSchema(info.Config, own); err != nil {
				t.Errorf("%s: example does not match its own schema: %v", kind, err)
			}
		}
	}
}

func TestSchemaAcceptsBuiltinExamples(t *testing.T) {
	// An example from every kind, assembled into one workflow, must satisfy
	// the definition schema.
	steps := []any{}
	for _, kind := range []string{"script", "tool", "model", "agent", "foreach"} {
		info, ok := workflowkit.Describe(kind)
		if !ok {
			t.Fatalf("kind %q not registered", kind)
		}
		step := map[string]any{}
		for key, value := range info.Example {
			step[key] = value
		}
		step["id"] = kind + "-step"
		steps = append(steps, step)
	}
	def := map[string]any{"description": "example set", "steps": steps}
	if err := workflowkit.ValidateSchema(workflowkit.Schema(), def); err != nil {
		t.Fatalf("the assembled example workflow does not match Schema(): %v", err)
	}
	// And the same definition must parse.
	raw, err := jsonMarshal(def)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workflowkit.Parse(raw); err != nil {
		t.Fatalf("the assembled example workflow does not parse: %v", err)
	}
}

func TestSchemaRejectsBrokenDefinition(t *testing.T) {
	if err := workflowkit.ValidateSchema(workflowkit.Schema(), map[string]any{"steps": []any{}}); err == nil {
		t.Fatal("a definition without a description must not satisfy the schema")
	}
}

func TestToolCatalog(t *testing.T) {
	env, _, _ := testEnv(t)
	env.Catalog = catalog{items: []workflowkit.ToolInfo{
		{Name: "ledger__import", Description: "import a ledger", InputSchema: map[string]any{"type": "object"}},
	}}
	tools, err := env.Catalog.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "ledger__import" {
		t.Fatalf("unexpected catalog: %#v", tools)
	}
}

type catalog struct{ items []workflowkit.ToolInfo }

func (c catalog) ListTools(ctx context.Context) ([]workflowkit.ToolInfo, error) { return c.items, nil }

// ---------------------------------------------------------------------------
// Loading from disk
// ---------------------------------------------------------------------------

func TestLoadAndLoadDirs(t *testing.T) {
	root := t.TempDir()
	local, shared := filepath.Join(root, "local"), filepath.Join(root, "shared")
	for _, dir := range []string{local, shared} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(dir, name, def string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(def), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sharedDef := `{"description":"shared version","steps":[{"id":"a","type":"script","run":"x"}]}`
	localDef := `{"description":"local version","steps":[{"id":"a","type":"script","run":"y"}]}`
	write(shared, "q.json", sharedDef)
	write(local, "q.json", localDef)
	write(shared, "other.json", `{"description":"other","steps":[{"id":"a","type":"script","run":"z"}]}`)
	write(shared, "broken.json", `{"nope":true}`)

	found, errs := workflowkit.LoadDirs(local, shared)
	if len(errs) != 1 {
		t.Fatalf("expected exactly one load error (broken.json), got %v", errs)
	}
	if len(found) != 2 {
		t.Fatalf("expected 2 workflows, got %d", len(found))
	}
	// The local directory wins on the shared id, and names come from file names.
	if found[0].ID != "other" || found[1].ID != "q" {
		t.Fatalf("unexpected ids: %q %q", found[0].ID, found[1].ID)
	}
	if found[1].Description != "local version" {
		t.Fatalf("local definition should win, got %q", found[1].Description)
	}
	if found[1].Dir != local {
		t.Fatalf("Dir should be the definition's directory, got %q", found[1].Dir)
	}
}

func TestEstimateDurationUsesCriticalPath(t *testing.T) {
	// Serial chain: 10 + 20 = 30. A parallel branch of 100 does not add on.
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"a","type":"script","run":"x","timeoutSeconds":10},
		{"id":"b","type":"script","run":"y","timeoutSeconds":20},
		{"id":"p","type":"script","run":"z","timeoutSeconds":100,"dependsOn":[]}]}`)
	if got := wf.EstimateDuration(); got != 100 {
		t.Fatalf("critical path should be 100, got %d", got)
	}
}

func TestReadJSONStaysInsideAllowedDirs(t *testing.T) {
	wf := mustParse(t, `{"description":"d","inputs":{"type":"object","properties":{"p":{"type":"string"}}},
		"steps":[{"id":"t","type":"tool","tool":"store","args":{"payload":"{{ readJSON .inputs.p }}"}}]}`)
	env, _, _ := testEnv(t)
	secret := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(secret, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := workflowkit.Run(context.Background(), wf, map[string]any{"p": secret}, env); err == nil {
		t.Fatal("readJSON must refuse a path outside the workflow and run directories")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
// ---------------------------------------------------------------------------
// Composite nodes: item context, sibling references and auditability
// ---------------------------------------------------------------------------

// TestForeachBodySeesSiblingsByDeclaredID locks in the rule that a body step
// refers to a sibling as .steps.<declared-id>, not by the namespaced id the run
// publishes globally. Without it, a conditional inside a body would silently
// never see its sibling and skip.
func TestForeachBodySeesSiblingsByDeclaredID(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"each","type":"foreach","items":"[1,2,3]",
		 "body":[
			{"id":"check","type":"script","run":"cmd-check"},
			{"id":"report","type":"tool","tool":"store","when":"{{ eq .steps.check.status \"ok\" }}",
			 "args":{"n":"{{ .item }}"}}]}]}`)
	env, _, tools := testEnv(t)
	res, err := workflowkit.Run(context.Background(), wf, nil, env)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if len(tools.calls) != 3 {
		t.Fatalf("every item's report should have run; got %d tool calls", len(tools.calls))
	}
	// Body results are published under namespaced ids, so nested execution is
	// visible to the caller.
	for _, id := range []string{"each[0].check", "each[1].report", "each[2].report"} {
		if _, ok := res.Steps[id]; !ok {
			t.Errorf("expected %q in Result.Steps (nested execution must be auditable)", id)
		}
	}
}

// TestForeachItemIsolation checks that two items running concurrently do not
// see each other's body results: each item's .steps.check must be its own.
func TestForeachItemIsolation(t *testing.T) {
	wf := mustParse(t, `{"description":"d","steps":[
		{"id":"each","type":"foreach","items":"[1,2,3,4]","concurrency":4,
		 "body":[{"id":"echo","type":"script","run":"cmd-echo"},
		         {"id":"use","type":"tool","tool":"store",
		          "args":{"text":"{{ .steps.echo.text }}"}}]}]}`)
	env, shell, tools := testEnv(t)
	// Each command returns something generic; the point is that every item gets
	// its own sibling's result rather than a mix.
	shell.out["cmd-echo"] = "same"
	if _, err := workflowkit.Run(context.Background(), wf, nil, env); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if len(tools.calls) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(tools.calls))
	}
	for _, call := range tools.calls {
		if call.Input["text"] != "same" {
			t.Fatalf("item did not see its own sibling result: %#v", call.Input)
		}
	}
}
