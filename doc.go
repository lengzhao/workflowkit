// Package workflowkit is an embeddable engine for declarative pipelines: it
// chains nodes (shell commands, tool calls, one-shot completions, subagent
// delegations, and whatever a host registers) into a single definition file
// executed in-process.
//
// Design goals:
//
//   - Embeddable: the library is imported, never deployed. There is no
//     scheduler, no trigger, no persistence, no background goroutine.
//   - Stateless API: there is no Engine object. Node types are registered
//     process-wide in init() (the AgentKit/pluginkit convention); host
//     capabilities are passed to Run on every call.
//   - Open node types: script/tool/model/agent/foreach are ordinary
//     registrations from the nodes/ packages. A host adds a capability by
//     registering a type, not by patching this package.
//   - Self-describing: ListTypes, Describe and Schema let a caller — including
//     a model — enumerate the available building blocks and their parameters,
//     then generate a definition that Parse validates.
//
// The one invariant inherited from AgentKit's tool/workflow: the main
// conversation never touches large payloads. Nodes exchange data by reference
// (artifact files under the run directory) and only the rendered result
// template crosses back to the caller.
package workflowkit