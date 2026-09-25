package runner

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// PreDispatchSnapshotForImpact returns the PreDispatchSnapshot to
// stamp onto a ToolCall whose tool has the given stateImpact, or nil
// if no snapshot is required. Snapshots are taken for stateImpact
// values readwrite and external — both can leave irreversible
// artifacts that future fork-from-this-turn must reproduce.
//
// Returns nil when sessionUID is empty (e.g. kubectl-driven sessions
// without UID propagation, or test fixtures); a snapshot handle
// without a sessionUID can't be later resolved by the restart
// reconciler.
//
// sessionUID is the parent AgentSession's UID; turnIndex must be the
// durable, monotonic memory turn index (memTurnIndex) — NOT the per-Run
// turnCount, which resets to 0 on resume — because the restart reconciler
// compares it against a transcript-space CutTurnIndex (see the call site in
// dispatchToolUses and AnalyzePostCut); sequence is the within-turn ordering
// (0 for the first stateful dispatch, 1 for the second, etc.).
func PreDispatchSnapshotForImpact(sessionUID string, turnIndex, sequence int32, impact authz.StateImpact) *spiceboxv1alpha1.PreDispatchSnapshot {
	if sessionUID == "" {
		return nil
	}
	switch impact {
	case authz.Readwrite, authz.External:
		return &spiceboxv1alpha1.PreDispatchSnapshot{
			SessionUID: sessionUID,
			TurnIndex:  turnIndex,
			Sequence:   sequence,
		}
	default:
		return nil
	}
}
