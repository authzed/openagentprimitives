package pinnededit

import (
	"sort"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// IdleStatusGate is a registerable hard gate on a session's VISIBLE status.
//
// It answers one question, and only for a session the badge derivation would
// otherwise render "in progress": given an AgentSession whose runner has YIELDED
// (phase=Idle — nothing is actively running), what terminal badge, if any, must
// its pinned opening message show instead? A gate returns (badge, true) to force
// that badge, or ("", false) to abstain.
//
// This is deliberately the STATUS axis, not the phase axis. An Idle session must
// stay Idle so a later inbound (a second push to a PR, a redelivery) re-joins it;
// forcing its phase terminal would break that re-join. So the gate corrects only
// what a person SEES, never the lifecycle.
//
// A new gate is a package with a Register() in its init(), not a case in a
// switch here — mirroring completion.Requirement and the channel-kind registry.
// Capabilities and channel kinds can each contribute a gate this way.
type IdleStatusGate interface {
	// Key is the stable registry identifier (also the tie-break sort key).
	Key() string
	// TerminalBadgeForIdle reports the terminal badge to show for an Idle sess,
	// or ok=false to abstain. It must be a pure read of sess.
	TerminalBadgeForIdle(sess *v1alpha1.AgentSession) (v1alpha1.OpeningBadge, bool)
}

// idleStatusGates is the registry, keyed by Key(). Populated by gate packages'
// init() via Register; read by terminalBadgeForIdle during badge derivation.
var idleStatusGates = map[string]IdleStatusGate{}

// Register adds g to the idle-status-gate registry. It panics on a duplicate
// key — a collision is a wiring bug (two gates fighting over the same name),
// caught at process start, exactly like the completion and channel-kind
// registries.
func Register(g IdleStatusGate) {
	k := g.Key()
	if _, dup := idleStatusGates[k]; dup {
		panic("pinnededit: duplicate IdleStatusGate key " + k)
	}
	idleStatusGates[k] = g
}

// terminalBadgeForIdle consults every registered gate in Key order and returns
// the first terminal badge one supplies (first-fires-wins, deterministic). It
// returns ok=false when no gate fires — the caller then keeps the default
// in_progress badge. Pure: safe to call on every derivation.
func terminalBadgeForIdle(sess *v1alpha1.AgentSession) (v1alpha1.OpeningBadge, bool) {
	keys := make([]string, 0, len(idleStatusGates))
	for k := range idleStatusGates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if b, ok := idleStatusGates[k].TerminalBadgeForIdle(sess); ok {
			return b, true
		}
	}
	return "", false
}
