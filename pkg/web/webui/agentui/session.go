package agentui

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Branch identifies which rung of the session-resolution ladder fired.
// Per the design spec (§Component 3, "Session resolution"): "the mechanism
// is seamless; the fact is disclosed in chrome" — chrome renders this
// ("resumed your session" vs "your session is asleep"), so it has to survive
// as data on Resolution, not just as ResolveSession's internal control flow.
//
// Each value names an observed CONDITION of the named session, never an action
// the platform took: resolving a session for the view is a read, and a branch
// whose name asserted a mutation would be read straight into chrome copy that
// claims one.
type Branch string

const (
	// Attached: the named session is still live. No wake, no new session —
	// the browser attaches directly.
	Attached Branch = "attached"
	// Asleep: the named session is idle/slept and its pod is gone
	// (v1alpha1.WakeEligible). Rendering the view does NOT rehydrate it —
	// nothing on this read path stamps the wake annotation, because waking
	// from a read would make a GET mutating. Chrome discloses the condition
	// plus the fact that sending a message resumes it (channelsd's inbound
	// Deliver path stamps the annotation).
	Asleep Branch = "asleep"
	// Ended: the named session is terminal (Succeeded/Failed, including
	// wall-clock-expired, which transitions straight to Failed).
	//
	// The three API routes answer 410 rather than a view with an empty
	// session: a UI is always session-scoped, and there is no rung that hands
	// the browser a session-less UI to bind against. The session shell answers
	// earlier and differently — a read-only transcript, with a notice when the
	// agent's view was what was asked for — so a viewer keeps the one thing an
	// ended session can still show. Starting a REPLACEMENT is an explicit POST
	// either way, never a side effect of a read.
	Ended Branch = "ended"
)

// AllBranches is every value ResolveSession can return, in ladder order.
// Exported so a consumer enumerates the set rather than copying it: the
// browser turns each value into disclosure copy, and a branch with no copy
// written for it renders the generic default — a silent downgrade of
// disclosure the design calls non-negotiable.
//
// CAVEAT: pkg/web/webui/sessions' origins golden is generated from this slice
// and TestOriginsGolden_PinsAllBranches asserts they match, but no TypeScript
// file reads that golden yet. A new Branch added here, with the golden
// regenerated, passes every suite silently today.
var AllBranches = []Branch{Attached, Asleep, Ended}

// Resolution is ResolveSession's pure decision: which ladder rung a named,
// already-interact-authorized AgentSession lands on. SessionName is set for
// Attached/Asleep and empty for Ended.
type Resolution struct {
	// Branch is the rung that fired; chrome renders disclosure copy from it.
	Branch Branch
	// SessionName is set for Attached/Asleep and EMPTY for Ended.
	SessionName string
}

// ResolveSession decides which ladder rung applies to sess — a specific,
// named AgentSession the caller has ALREADY confirmed the viewer may
// interact with. It is pure decision logic over sess's own fields: no I/O,
// no re-checking authorization, and — this is the load-bearing property — no
// searching for some OTHER, more-recently-created session for the same
// AgentClass.
//
// Keying the ladder to "the most recent session for this class" instead
// silently breaks navigation: a viewer opening a link to their own live
// session lands on a DIFFERENT, newer one they can also interact with, or on
// Ended with an empty session when they cannot. A UI is always session-scoped
// (agent-scoped UIs are a listed Non-goal), so the ladder is keyed to the ONE
// session the URL names, never to "the agent" in the abstract.
//
//	still live (not WakeEligible, not terminal)  -> ATTACH
//	idle / slept (WakeEligible)                   -> the named session is asleep
//	terminal (Succeeded/Failed, incl. expired)    -> the named session ended
//
// ResolveSession only picks the branch; it never stamps a wake annotation
// and never creates an AgentSession. Nothing else on the read path that
// resolves a session for the view does either, which is why the Asleep branch
// is named for the condition it observed rather than for a rehydration that
// does not happen here.
func ResolveSession(sess *spiceboxv1alpha1.AgentSession) Resolution {
	if spiceboxv1alpha1.WakeEligible(sess) {
		return Resolution{Branch: Asleep, SessionName: sess.Name}
	}
	switch sess.Status.Phase {
	case spiceboxv1alpha1.AgentSessionPhaseSucceeded, spiceboxv1alpha1.AgentSessionPhaseFailed:
		// Genuinely terminal — the WakeEligible check above already caught the
		// archive-swept "parked, not finished" case.
		return Resolution{Branch: Ended}
	default:
		// Running, Pending, "", and every Awaiting* phase: neither slept nor
		// terminal, so live enough to attach directly. The Awaiting* phases
		// are waiting on THIS viewer's next action, not asleep.
		return Resolution{Branch: Attached, SessionName: sess.Name}
	}
}
