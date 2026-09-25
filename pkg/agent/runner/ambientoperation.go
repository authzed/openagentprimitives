package runner

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// resolveAmbientOperation answers "which operation does this call belong to?"
// for EVERY tool call, meta tools included, so the audit graph is total.
//
// Requiring the model to thread operation_id through respond_to_user and
// query_memory would burn tokens and invite errors for no benefit: those calls
// have exactly one correct operation and the runtime already knows it. So the
// answer is resolved, never authored — no exemption list, no new required
// argument, no prompt change, and no bootstrap problem for new_operation
// (which produces operations) or update_plan (which produces them when a plan
// item goes in_progress).
//
// Three levels, and the third is what makes it total:
//
//  1. the explicit operation_id on the {operation_id,_reason,args} envelope —
//     MCP and sandbox calls, unchanged;
//  2. otherwise the operation the in-progress plan item opened, which is the
//     work the agent has actually declared it is doing right now;
//  3. otherwise the session root.
//
// Returns "" only when no registry is wired, which is a test/degraded shape
// rather than a live one.
func resolveAmbientOperation(sess *tool.SessionContext, args json.RawMessage) string {
	if sess == nil || sess.Operations == nil {
		return ""
	}

	// 1. Explicit.
	if id := explicitOperationID(args); id != "" {
		return id
	}

	// 2. The in-progress plan item's operation. Scanned rather than read off
	// UpdateResult.InProgress because that ref exists only on the result of the
	// update that created it; by the time an unrelated tool call needs
	// attributing, the store is the only source left.
	if store, ok := plans.TryFrom(sess); ok {
		for _, p := range store.InProgress() {
			for _, it := range p.Items {
				if it.Status == plans.StatusInProgress && it.OperationID != "" {
					return it.OperationID
				}
			}
		}
	}

	// 3. The root. Minted here on first use, which is why a session that
	// attributes everything explicitly never grows one.
	return sess.Operations.Root().ID
}

// explicitOperationID returns the operation the call names on its
// {operation_id,_reason,args} envelope, or "" when it names none.
//
// Split out from resolveAmbientOperation because the recording site needs the
// distinction, not just the answer: a call that names its own operation is
// recorded by its dispatcher, and recording it centrally as well would
// double-count every external call.
//
// An unparseable body returns "" rather than an error. The dispatcher rejects
// malformed envelopes on its own terms, and pretending one named an operation
// would attribute a call to an id nothing registered.
func explicitOperationID(args json.RawMessage) string {
	var env struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(args, &env); err != nil {
		return ""
	}
	return env.OperationID
}
