package workflowkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
)

// NodeInfo is the self-description of a registered node kind. It is what
// Describe returns and what a model reads to decide whether a kind fits.
type NodeInfo struct {
	// Kind is the value of a step's "type" field.
	Kind string `json:"kind"`
	// Description is one line explaining what the kind does (used for routing).
	Description string `json:"description,omitempty"`
	// Config is the JSON Schema of the kind's own step fields (run, prompt,
	// url, …), excluding the common fields every step has. Empty when the kind
	// declares no config.
	Config map[string]any `json:"config,omitempty"`
	// SubSteps names config fields holding []Step that the node executes
	// itself via Ctx.RunSteps (e.g. "body" for foreach). Parse recursively
	// validates the steps found under these fields.
	SubSteps []string `json:"subSteps,omitempty"`
	// Example is a minimal, working step body (its "type" plus own fields) that
	// must validate against Config. A test enforces that.
	Example map[string]any `json:"example,omitempty"`
}

// Option configures a node kind's self-description at registration time.
type Option func(*NodeInfo) error

// WithDescription sets the one-line description used for routing.
func WithDescription(desc string) Option {
	return func(info *NodeInfo) error {
		info.Description = desc
		return nil
	}
}

// WithSchema sets the kind's config JSON Schema directly. It fails when a
// schema was already supplied by WithConfig.
func WithSchema(schema map[string]any) Option {
	return func(info *NodeInfo) error {
		if info.Config != nil {
			return fmt.Errorf("config schema already set (WithConfig and WithSchema are mutually exclusive)")
		}
		info.Config = schema
		return nil
	}
}

// WithConfig derives the config JSON Schema from a config struct's json tags
// and descriptions parsed from the same struct's Go comments is NOT possible at
// runtime, so WithConfig derives the schema from the struct shape and requires
// descriptions to be attached with the `desc` struct tag:
//
//	type HTTPConfig struct {
//		URL    string `json:"url" desc:"request URL, may contain templates"`
//		Method string `json:"method" desc:"HTTP method (default GET)"`
//	}
//
// Types supported in the derivation: string, bool, int/float kinds, slices and
// maps of those, and any (left unconstrained). Use WithSchema when a kind needs
// conditionals (oneOf/if-then) beyond this mapping.
func WithConfig(cfgStruct any) Option {
	return func(info *NodeInfo) error {
		if info.Config != nil {
			return fmt.Errorf("config schema already set (WithConfig and WithSchema are mutually exclusive)")
		}
		schema, err := schemaFromStruct(cfgStruct)
		if err != nil {
			return err
		}
		info.Config = schema
		return nil
	}
}

// WithExample sets a minimal, working example of the kind's own step fields.
func WithExample(example map[string]any) Option {
	return func(info *NodeInfo) error {
		info.Example = example
		return nil
	}
}

// WithSubSteps names config fields that hold []Step and are executed by the
// node itself. Parse validates those steps recursively.
func WithSubSteps(fields ...string) Option {
	return func(info *NodeInfo) error {
		info.SubSteps = append(info.SubSteps, fields...)
		return nil
	}
}

// StepsSchema returns the JSON Schema for an array of steps, for node config
// schemas that accept sub-steps (with WithSubSteps). It only requires the shape
// every step has; the core validates ids and node-specific fields recursively.
func StepsSchema() map[string]any {
	return map[string]any{
		"type": "array",
		"items": map[string]any{
			"type":     "object",
			"required": []any{"id", "type"},
			"properties": map[string]any{
				"id":   map[string]any{"type": "string"},
				"type": map[string]any{"type": "string"},
			},
		},
	}
}

var stepType = reflect.TypeOf(Step{})

// schemaFromStruct builds a JSON Schema from a config struct using the `json`
// and `desc` struct tags. Required is every field without `omitempty`.
func schemaFromStruct(cfgStruct any) (map[string]any, error) {
	if cfgStruct == nil {
		return nil, nil
	}
	t := reflect.TypeOf(cfgStruct)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("config must be a struct, got %s", t)
	}
	props := map[string]any{}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		name, optional, skip := jsonFieldName(field)
		if skip {
			continue
		}
		sch, err := schemaFromType(field.Type)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", field.Name, err)
		}
		if desc := field.Tag.Get("desc"); desc != "" {
			sch["description"] = desc
		}
		props[name] = sch
		if !optional {
			required = append(required, name)
		}
	}
	out := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		out["required"] = required
	}
	out["additionalProperties"] = false
	return out, nil
}

func jsonFieldName(field reflect.StructField) (name string, optional, skip bool) {
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	name = field.Name
	if tag == "" {
		return name, false, false
	}
	parts := bytes.Split([]byte(tag), []byte(","))
	if len(parts) > 0 && len(parts[0]) > 0 {
		name = string(parts[0])
	}
	for _, part := range parts[1:] {
		if string(part) == "omitempty" {
			optional = true
		}
	}
	return name, optional, false
}

// schemaFromType maps a Go type to a JSON Schema fragment.
func schemaFromType(t reflect.Type) (map[string]any, error) {
	// A sub-step list is described by the generic step schema, not by Step's own
	// fields: the steps inside belong to whichever node kinds the workflow uses.
	if t == stepType {
		return map[string]any{"type": "object"}, nil
	}
	if (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) && t.Elem() == stepType {
		return StepsSchema(), nil
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil
	case reflect.Slice, reflect.Array:
		item, err := schemaFromType(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": item}, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map key must be a string, got %s", t.Key())
		}
		value, err := schemaFromType(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": value}, nil
	case reflect.Interface:
		// any: accept anything.
		return map[string]any{}, nil
	case reflect.Pointer:
		return schemaFromType(t.Elem())
	case reflect.Struct:
		return schemaFromStruct(reflect.New(t).Elem().Interface())
	default:
		return nil, fmt.Errorf("unsupported type %s", t)
	}
}

// CheckConfig builds a node from a step's fields to surface node-level errors —
// a missing required field or a misspelled one — at load time rather than at
// the first Run. Parse calls it; a definition that passes validation is known
// to construct.
func CheckConfig(kind string, fields map[string]any) error {
	reg, ok := lookup(kind)
	if !ok {
		return fmt.Errorf("unknown type %q", kind)
	}
	_, err := reg.ctor(fields)
	return err
}

// decodeConfig decodes a step's config map into the constructor's config struct
// type, rejecting unknown fields. This is where pluginkit's strictness lives:
// the core collects every non-common field and hands it to the node, and the
// node's own config struct is what makes a typo an error.
func decodeConfig(cfgType reflect.Type, cfg map[string]any) (reflect.Value, error) {
	if cfg == nil {
		cfg = map[string]any{}
	}
	wantPtr := cfgType.Kind() == reflect.Pointer
	st := cfgType
	if wantPtr {
		st = cfgType.Elem()
	}
	dest := reflect.New(st)
	if len(cfg) > 0 {
		raw, err := json.Marshal(cfg)
		if err != nil {
			return reflect.Value{}, fmt.Errorf("encode config: %w", err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(dest.Interface()); err != nil {
			return reflect.Value{}, fmt.Errorf("invalid config: %w", err)
		}
	}
	if wantPtr {
		return dest, nil
	}
	return dest.Elem(), nil
}
