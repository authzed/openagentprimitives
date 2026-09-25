// Package sessionhold is the revocation kind that halts a running session's
// turn loop when a forensic hold is tripped.
//
// LATENCY ONLY. Delivery is core NATS with no ack/nak and no retry, so a
// dropped envelope must cost nothing but time: the AgentSession reconciler
// (pkg/controllers/agentsession/hold.go) parks the session and reaps its pods
// on its own next pass regardless of whether this ever fires. Nothing here is
// the sole enforcement of a hold.
//
// Holders reached (per the bus README's "fan-out, not cache-drop"): the
// runner's turn loop (pkg/agent/runner.Loop.Run runs off the same context the
// injected stop func cancels), any in-flight tool call blocked underneath it
// (loop_dispatch.go derives each call's context from that same one), and the
// lifecycle sequencer (sequencer.go's applyEvent/claimAndRecover take that
// context too) — cancelling it unwinds all three together.
//
// Precision, not just reach: the invalidator is constructed with the one
// session key its own runner process is running, and Invalidate is a no-op
// for any other key. The ap.revocation subject is a single flat, namespace-
// filtered stream — every runner sharing a namespace receives every
// session-hold revoke published there, sibling sessions included — so
// without this check, holding one session would stop every other
// concurrently-running session in the same scope.
package sessionhold

import "github.com/authzed/openagentprimitives/pkg/authz/revocation"

// Kind is the session-hold revocation Invalidator.
type Kind struct {
	sessionKey string
	stop       func()
}

// New builds the invalidator for the one session this runner process is
// running. sessionKey is "<namespace>/<name>", matching how the AgentSession
// reconciler builds the revoke key in hold.go. stop cancels the runner's root
// context; nil is safe and makes Invalidate a permanent no-op, for binaries
// that register no loop.
func New(sessionKey string, stop func()) *Kind {
	return &Kind{sessionKey: sessionKey, stop: stop}
}

// Kind implements revocation.Invalidator.
func (k *Kind) Kind() string { return "session-hold" }

// Noun implements revocation.Invalidator.
//
// "A held session" names, from the reader's side, exactly what stopped: their
// session was frozen for review. It covers the one thing this kind ever
// withdraws, so it needs no further qualification.
func (k *Kind) Noun() string { return "a held session" }

// Invalidate halts the session named by key, if and only if key names the
// session this process is running — see the package doc for why that check
// exists. Safe to call repeatedly: at-most-once delivery means a redelivery
// is possible, and cancelling an already-cancelled loop is a no-op.
func (k *Kind) Invalidate(key string) error {
	if key != k.sessionKey {
		return nil
	}
	if k.stop == nil {
		return nil
	}
	k.stop()
	return nil
}

var _ revocation.Invalidator = (*Kind)(nil)
