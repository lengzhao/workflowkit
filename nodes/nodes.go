// Package nodes registers the built-in node kinds by importing their packages:
//
//	import _ "github.com/lengzhao/workflowkit/nodes"
//
// The built-ins are ordinary registrations, not privileged code: each lives in
// its own package, registers itself in init(), and is reachable through the same
// Register/Describe/Parse path a host's own node type uses. Dropping any of them
// leaves the library fully functional.
package nodes

import (
	_ "github.com/lengzhao/workflowkit/nodes/agent"
	_ "github.com/lengzhao/workflowkit/nodes/foreach"
	_ "github.com/lengzhao/workflowkit/nodes/model"
	_ "github.com/lengzhao/workflowkit/nodes/script"
	_ "github.com/lengzhao/workflowkit/nodes/tool"
)
