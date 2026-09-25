package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

func init() {
	Register(&newOperation{})
}

type newOperation struct{}

func (*newOperation) Name() string    { return "new_operation" }
func (*newOperation) Kind() tool.Kind { return tool.KindMeta }
func (*newOperation) Permission() authz.Permission {
	// new_operation allocates an in-process audit token: no external
	// resource read or mutation occurs.
	return authz.Permission{StateImpact: authz.Stateless}
}

// PermissionVariants returns nil — meta tools have no conditional
// variants today (only MCP-tooled AgentClasses use them).
func (*newOperation) PermissionVariants() []authz.PermissionVariant { return nil }
func (*newOperation) Description() string {
	return "Begin a new logical operation and receive an operation_id. " +
		"Every subsequent sandbox tool call must reference this operation_id " +
		"and supply a _reason explaining why the call is needed for the " +
		"operation. Open one operation per logical task; do not reuse an " +
		"operation_id across unrelated work. The description you supply is " +
		"recorded in the audit log alongside every tool call made under it."
}

func (*newOperation) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"description": {
				"type": "string",
				"description": "Concise description of the logical operation about to be performed. Captured in the audit trail."
			},
			"parent": {
				"description": "Optional anchor for the new operation in the audit graph. Either an existing operation_id (sibling/child operation under that scope) or a plan/item pair. Mutually exclusive — supply exactly one shape.",
				"oneOf": [
					{
						"type": "object",
						"additionalProperties": false,
						"properties": {"operation_id": {"type": "string"}},
						"required": ["operation_id"]
					},
					{
						"type": "object",
						"additionalProperties": false,
						"properties": {"plan": {"type": "string"}, "item": {"type": "string"}},
						"required": ["plan", "item"]
					}
				]
			}
		},
		"required": ["description"]
	}`)
}

type newOperationArgs struct {
	// Description is the agent's stated purpose for the operation.
	Description string `json:"description"`
	// Parent anchors the operation in the audit graph; nil means top-level.
	Parent *newOperationParentArgs `json:"parent,omitempty"`
}

// newOperationParentArgs names ONE parent: either a sibling operation, or a
// (Plan, Item) pair — never both.
type newOperationParentArgs struct {
	OperationID string `json:"operation_id,omitempty"`
	Plan        string `json:"plan,omitempty"`
	Item        string `json:"item,omitempty"`
}

func (*newOperation) Execute(_ context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	var a newOperationArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return tool.Result{Content: "new_operation: invalid arguments: " + err.Error(), IsError: true, Trusted: true}, nil
	}
	if a.Description == "" {
		return tool.Result{Content: "new_operation: description is required", IsError: true, Trusted: true}, nil
	}
	if sess == nil || sess.Operations == nil {
		return tool.Result{Trusted: true}, errors.New("new_operation: SessionContext.Operations is nil")
	}

	// Validate parent XOR before opening the operation, so an invalid
	// parent shape doesn't leave behind a parentless operation.
	var parentRef *tool.OperationParent
	if a.Parent != nil {
		hasOp := a.Parent.OperationID != ""
		hasItem := a.Parent.Plan != "" || a.Parent.Item != ""
		if hasOp == hasItem {
			return tool.Result{Content: "new_operation: parent must specify exactly one of operation_id OR (plan, item); got both or neither", IsError: true, Trusted: true}, nil
		}
		if hasItem && (a.Parent.Plan == "" || a.Parent.Item == "") {
			return tool.Result{Content: "new_operation: parent.plan and parent.item are both required when anchoring to a plan item", IsError: true, Trusted: true}, nil
		}
		if hasOp {
			parentRef = &tool.OperationParent{OperationID: a.Parent.OperationID}
		} else {
			parentRef = &tool.OperationParent{PlanItem: &tool.PlanItemRef{Plan: a.Parent.Plan, Item: a.Parent.Item}}
		}
	}

	op := sess.Operations.Begin(a.Description)
	if parentRef != nil {
		if !sess.Operations.SetParent(op.ID, parentRef) {
			return tool.Result{Trusted: true}, errors.New("new_operation: SetParent returned false on freshly-Begun operation (registry inconsistency)")
		}
	}
	body, err := json.Marshal(map[string]string{"operation_id": op.ID})
	if err != nil {
		return tool.Result{Trusted: true}, fmt.Errorf("new_operation: marshal: %w", err)
	}
	return tool.Result{Content: string(body), Trusted: true}, nil
}
