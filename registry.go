package workflowkit

import (
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// Registry holds node types by kind.
//
// Registration is process-wide and happens in init(), mirroring
// github.com/lengzhao/pluginkit: importing a nodes/ package is what makes its
// kind available. The map is written during init and only read afterwards, so
// read access needs no lock; the mutex only guards registration and the test
// helper resetRegistry.
var (
	registryMu sync.RWMutex
	registry   = map[string]*registration{}
)

// registration is one registered node type.
type registration struct {
	kind  string
	fn    any
	info  NodeInfo
	ctor  constructor
	order int // registration order, for stable ListTypes output
}

// constructor is the normalized form of a registered constructor. It returns a
// fresh Node for one step. Config decoding is the caller's job because the
// config arrives per step, not per registration.
type constructor func(cfg map[string]any) (Node, error)

// Register registers a node type under kind.
//
// The constructor must be one of:
//
//	func() (N, error)
//	func(cfg Q) (N, error)   // Q is a struct; cfg fields are decoded into it
//
// where N implements Node. Any other shape panics, and registering the same
// kind twice panics — both mirror pluginkit, where a bad registration must fail
// during init() rather than at the first Run.
//
// Options (WithConfig, WithSchema, WithDescription, WithExample, WithSubSteps)
// supply the self-description used by Describe and reported to callers.
func Register(kind string, ctor any, opts ...Option) {
	if err := register(kind, ctor, opts); err != nil {
		panic(err)
	}
}

func register(kind string, ctor any, opts []Option) error {
	if kind == "" {
		return fmt.Errorf("workflowkit: node kind must not be empty")
	}
	fn, err := parseConstructor(ctor)
	if err != nil {
		return fmt.Errorf("workflowkit: node %q: %w", kind, err)
	}

	info := NodeInfo{Kind: kind}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(&info); err != nil {
			return fmt.Errorf("workflowkit: node %q: %w", kind, err)
		}
	}

	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[kind]; exists {
		return fmt.Errorf("workflowkit: node kind %q already registered", kind)
	}
	registry[kind] = &registration{
		kind:  kind,
		fn:    ctor,
		info:  info,
		ctor:  fn,
		order: len(registry),
	}
	return nil
}

// parseConstructor validates the constructor shape and normalizes it.
func parseConstructor(ctor any) (constructor, error) {
	if ctor == nil {
		return nil, fmt.Errorf("constructor is nil")
	}
	t := reflect.TypeOf(ctor)
	if t.Kind() != reflect.Func {
		return nil, fmt.Errorf("constructor must be a function")
	}
	if t.IsVariadic() {
		return nil, fmt.Errorf("constructor must not be variadic")
	}
	if t.NumOut() != 2 {
		return nil, fmt.Errorf("constructor must return (T, error)")
	}
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	if !t.Out(1).Implements(errorType) {
		return nil, fmt.Errorf("constructor second return value must be error")
	}
	nodeType := t.Out(0)
	if nodeType.Kind() != reflect.Interface && !reflect.PointerTo(nodeType).Implements(reflect.TypeOf((*Node)(nil)).Elem()) {
		// Accept both T and *T as long as *T implements Node; the reflect.Call
		// below returns whatever the constructor returned, and we type-assert.
	}
	nodeIfc := reflect.TypeOf((*Node)(nil)).Elem()
	if !nodeType.Implements(nodeIfc) {
		return nil, fmt.Errorf("constructor must return a value implementing Node, got %s", nodeType)
	}

	switch t.NumIn() {
	case 0:
		out := reflect.ValueOf(ctor)
		return func(map[string]any) (Node, error) {
			return callConstructor(out, nil)
		}, nil
	case 1:
		cfgType := t.In(0)
		if cfgType.Kind() == reflect.Pointer {
			cfgType = cfgType.Elem()
		}
		if cfgType.Kind() != reflect.Struct {
			return nil, fmt.Errorf("constructor config parameter must be a struct, got %s", t.In(0))
		}
		cfgPtr := t.In(0)
		out := reflect.ValueOf(ctor)
		return func(cfg map[string]any) (Node, error) {
			decoded, err := decodeConfig(cfgPtr, cfg)
			if err != nil {
				return nil, err
			}
			return callConstructor(out, []reflect.Value{decoded})
		}, nil
	default:
		return nil, fmt.Errorf("constructor must take no arguments or (cfg), got %d arguments", t.NumIn())
	}
}

func callConstructor(fn reflect.Value, args []reflect.Value) (Node, error) {
	out := fn.Call(args)
	if err := out[1].Interface(); err != nil {
		return nil, err.(error)
	}
	node, ok := out[0].Interface().(Node)
	if !ok {
		return nil, fmt.Errorf("constructor returned %T, which does not implement Node", out[0].Interface())
	}
	return node, nil
}

// ListTypes returns the registered node kinds, sorted for stable output.
func ListTypes() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	kinds := make([]string, 0, len(registry))
	for kind := range registry {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

// Describe returns the self-description of a node kind. The bool reports
// whether the kind is registered; an unknown kind is not an error.
//
// Describe is what makes the library usable by a model: it enumerates the
// parameters (Config), a minimal Example, and the SubSteps field names a
// composite node executes itself.
func Describe(kind string) (NodeInfo, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	reg, ok := registry[kind]
	if !ok {
		return NodeInfo{}, false
	}
	return reg.info, true
}

// lookup returns the registration for kind.
func lookup(kind string) (*registration, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	reg, ok := registry[kind]
	return reg, ok
}

// resetRegistry clears the registry. Test-only helper.
func resetRegistry() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]*registration{}
}
