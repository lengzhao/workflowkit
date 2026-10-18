package workflowkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
)

// The template function set is deliberately tiny: dig, quote, readJSON. A
// larger set would break load-time validation and make a definition harder to
// audit. Conditionals use text/template's built-in eq/ne/lt/gt/and/or/not.

// templateFuncs returns the FuncMap for a render against data.
func templateFuncs(data map[string]any) template.FuncMap {
	return template.FuncMap{
		"dig":      tmplDig,
		"quote":    tmplQuote,
		"readJSON": func(path string) (any, error) { return tmplReadJSON(data, path) },
	}
}

// parseTemplate checks a template's syntax without rendering it. data may be
// nil: the function map is needed for parsing named functions to resolve, but
// readJSON's closure tolerates a nil data map as long as it is never called.
func parseTemplate(name, text string, data map[string]any) (*template.Template, error) {
	tmpl, err := template.New(name).Funcs(templateFuncs(data)).Parse(text)
	if err != nil {
		return nil, fmt.Errorf("template %s: %w", name, err)
	}
	return tmpl, nil
}

func renderTemplate(name, text string, data map[string]any) (string, error) {
	tmpl, err := parseTemplate(name, text, data)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("template %s: %w", name, err)
	}
	return out.String(), nil
}

// jsonValue wraps a decoded JSON value so a direct action such as
// {{ .steps.x.json }} renders as compact JSON while dig can still walk it.
type jsonValue struct{ V any }

func (j jsonValue) String() string {
	raw, err := json.Marshal(j.V)
	if err != nil {
		return "null"
	}
	return string(raw)
}

// tmplDig walks nested maps and slices by a dotted path:
// dig .steps.submit.json "status" or dig .steps.rows.json "items.0.id".
func tmplDig(value any, path string) (any, error) {
	cur := value
	if path == "" {
		return normalizeComposite(cur), nil
	}
	for _, key := range strings.Split(path, ".") {
		if wrapped, ok := cur.(jsonValue); ok {
			cur = wrapped.V
		}
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[key]
			if !ok {
				return nil, fmt.Errorf("dig %q: no key %q", path, key)
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(node) {
				return nil, fmt.Errorf("dig %q: bad index %q", path, key)
			}
			cur = node[i]
		default:
			return nil, fmt.Errorf("dig %q: %q is not an object or array", path, key)
		}
	}
	return normalizeComposite(cur), nil
}

// normalizeComposite wraps objects and arrays so direct actions render JSON.
func normalizeComposite(v any) any {
	switch v.(type) {
	case map[string]any, []any:
		return jsonValue{V: v}
	default:
		return v
	}
}

// tmplQuote shell-quotes a value for safe interpolation into a command.
func tmplQuote(value any) (string, error) {
	s, err := stringify(value)
	if err != nil {
		return "", err
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'", nil
}

// stringify renders scalars as-is and composite values as compact JSON.
func stringify(value any) (string, error) {
	switch v := value.(type) {
	case nil:
		return "", nil
	case jsonValue:
		return stringify(v.V)
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case json.Number:
		return v.String(), nil
	case float64:
		if n, ok := floatToNumber(v); ok {
			return n.String(), nil
		}
		return strconv.FormatFloat(v, 'g', -1, 64), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("render value: %w", err)
		}
		return string(raw), nil
	}
}

// decodeJSON decodes one JSON value, keeping numbers as json.Number. Without
// UseNumber every number becomes float64 and integers above 2^53 lose precision.
func decodeJSON(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("extra JSON value")
		}
		return nil, err
	}
	return value, nil
}

// jsonComposite reports whether s looks like a JSON object or array.
func jsonComposite(s string) bool {
	if len(s) < 2 {
		return false
	}
	return (s[0] == '{' && s[len(s)-1] == '}') || (s[0] == '[' && s[len(s)-1] == ']')
}

// promoteJSONComposite parses a rendered arg when the whole value is a JSON
// object or array, so a tool whose parameter is an object receives an object
// rather than a JSON string. Other text, including numeric text, stays a string.
func promoteJSONComposite(rendered string) (any, bool, error) {
	trimmed := strings.TrimSpace(rendered)
	if !jsonComposite(trimmed) {
		return nil, false, nil
	}
	value, err := decodeJSON(trimmed)
	if err != nil {
		return nil, false, fmt.Errorf("JSON object or array: %w", err)
	}
	switch value.(type) {
	case map[string]any, []any:
		return value, true, nil
	default:
		return nil, false, nil
	}
}

// promoteJSONNumber keeps an interpolated JSON number a json.Number so integer
// tool fields are not sent as strings. It only applies when the template is a
// single action, so a static "42" stays a string.
func promoteJSONNumber(templateText, rendered string) (json.Number, bool) {
	if !singleTemplateAction(templateText) {
		return "", false
	}
	value, err := decodeJSON(strings.TrimSpace(rendered))
	if err != nil {
		return "", false
	}
	n, ok := value.(json.Number)
	return n, ok
}

func singleTemplateAction(text string) bool {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "{{") || !strings.HasSuffix(trimmed, "}}") {
		return false
	}
	inner := trimmed[2 : len(trimmed)-2]
	return !strings.Contains(inner, "{{") && !strings.Contains(inner, "}}")
}

// floatToNumber rewrites a float64 from encoding/json back to an integer
// literal when the value is a whole number in int64 range.
func floatToNumber(n float64) (json.Number, bool) {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return "", false
	}
	if n == math.Trunc(n) && n >= math.MinInt64 && n <= math.MaxInt64 {
		return json.Number(strconv.FormatInt(int64(n), 10)), true
	}
	return json.Number(strconv.FormatFloat(n, 'f', -1, 64)), true
}

// renderValue deep-renders template strings in a tool's args.
func renderValue(value any, data map[string]any) (any, error) {
	switch v := value.(type) {
	case string:
		rendered, err := renderTemplate("args", v, data)
		if err != nil {
			return nil, err
		}
		if promoted, ok, err := promoteJSONComposite(rendered); ok || err != nil {
			return promoted, err
		}
		if n, ok := promoteJSONNumber(v, rendered); ok {
			return n, nil
		}
		return rendered, nil
	case json.Number, bool, nil:
		return v, nil
	case float64:
		if n, ok := floatToNumber(v); ok {
			return n, nil
		}
		return v, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			rendered, err := renderValue(item, data)
			if err != nil {
				return nil, fmt.Errorf("args.%s: %w", key, err)
			}
			out[key] = rendered
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			rendered, err := renderValue(item, data)
			if err != nil {
				return nil, fmt.Errorf("args[%d]: %w", i, err)
			}
			out[i] = rendered
		}
		return out, nil
	default:
		return value, nil
	}
}

// tmplReadJSON reads a JSON object or array from the run directory or the
// definition's directory. Both limits matter: a path influenced by a model must
// not read arbitrary files.
func tmplReadJSON(data map[string]any, path string) (any, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("readJSON: empty path")
	}
	roots := workflowRoots(data)
	if len(roots) == 0 {
		return nil, fmt.Errorf("readJSON: no allowed directory in this context")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("readJSON: %w", err)
	}
	// EvalSymlinks both canonicalizes the target and resolves a symlink escape
	// (a link inside an allowed root pointing outside it).
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("readJSON %q: %w", path, err)
	}
	resolved = filepath.Clean(resolved)
	if !withinRoots(roots, resolved) {
		return nil, fmt.Errorf("readJSON: %q is outside the run directory", path)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("readJSON %q: %w", path, err)
	}
	value, err := decodeJSON(string(raw))
	if err != nil {
		return nil, fmt.Errorf("readJSON %q: %w", path, err)
	}
	switch value.(type) {
	case map[string]any, []any:
		return jsonValue{V: value}, nil
	default:
		return nil, fmt.Errorf("readJSON %q: JSON value must be an object or array", path)
	}
}

// workflowRoots lists the directories readJSON may read from: this run's
// artifact directory and the definition file's directory.
func workflowRoots(data map[string]any) []string {
	wf, _ := data["workflow"].(map[string]any)
	if wf == nil {
		return nil
	}
	var roots []string
	for _, key := range []string{"runDir", "dir"} {
		s, _ := wf[key].(string)
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		abs, err := filepath.Abs(s)
		if err != nil {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		roots = append(roots, filepath.Clean(abs))
	}
	return roots
}

func withinRoots(roots []string, path string) bool {
	for _, root := range roots {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// parseJSONContent returns the decoded value when content is a JSON object or
// array, else nil. Numbers stay json.Number.
func parseJSONContent(content string) any {
	trimmed := strings.TrimSpace(content)
	if !jsonComposite(trimmed) {
		return nil
	}
	value, err := decodeJSON(trimmed)
	if err != nil {
		return nil
	}
	switch value.(type) {
	case map[string]any, []any:
		return value
	default:
		return nil
	}
}