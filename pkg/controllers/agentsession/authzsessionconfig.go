package agentsession

import (
	"context"
	"fmt"
	"reflect"

	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/memory"
	asc "github.com/authzed/openagentprimitives/pkg/memory/kinds/authz_session_config"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// The per-session authz config snapshot, written by the OPERATOR.
//
// authzd derives its cold-start authorization policy from this record —
// internal/cmd/authzd/cold_start_policy.go reads scopeEnabled + coldStart and fails the
// session closed when either is absent or unreadable. The writer therefore
// decides which gates run on the session, which is why the writer must not be
// the session itself — a compromised runner could otherwise hand authzd the
// policy it wanted applied to it.
//
// Writing it here makes it K8s-witnessed. Every field is derived from the two
// CRs this reconciler itself Got from the API server, so the record now says
// what the cluster says, not what the runner said. With the writer moved, the
// Kind is ComponentWritten (see its own doc) and the runner's session bearer is
// refused the write at the memory facade's per-kind door.

// authzSessionConfigContent derives the snapshot from the AgentSession and
// AgentClass the reconciler already resolved.
//
// PURE — same (sess, ac) in, same Content out. No wall clock, no randomness, no
// read of anything the runner can influence, so an unchanged reconcile produces
// byte-identical content and reconcileAuthzSessionConfig can skip the write
// entirely (see there for why that matters).
//
// PerToolPrompts is deliberately left unset; see the field's doc on asc.Content
// for what it was and why the operator cannot derive it.
func authzSessionConfigContent(
	sess *spiceboxv1alpha1.AgentSession, ac *spiceboxv1alpha1.AgentClass,
) asc.Content {
	return asc.Content{
		BoundEntities: ac.Spec.BoundEntities,
		Subject:       authzSessionSubject(sess, ac).String(),
		ScopeEnabled:  ac.Spec.GetScope().Enabled,
		ColdStart:     ac.Spec.GetScope().ColdStart,
	}
}

// authzSessionSubject resolves the ACTING principal for the session, mirroring
// the runner's Loop.ResolveAuthSubjects (pkg/agent/runner/loop.go) — which
// reads the same two places on the same AgentSession, so both land on the same
// answer for the same CR.
//
// The precedence is uniform across subject modes except "startedBy": the
// current requester (the canonical the channel listener stamps on each inbound)
// wins, falling back to the session initiator when no inbound has landed yet —
// which is the state a cold start is in. "startedBy" freezes on the initiator
// by definition.
//
// Reading a slack-specific annotation key here mirrors the runner's existing
// coupling rather than adding a new one: the "who spoke last" annotation has no
// transport-neutral accessor on channelkinds today. Lifting it to one is worth
// doing, and is separable from moving this writer.
func authzSessionSubject(
	sess *spiceboxv1alpha1.AgentSession, ac *spiceboxv1alpha1.AgentClass,
) identity.CanonicalUserID {
	startedBy := spiceboxv1alpha1.ResolveStartedByCanonical(sess)
	if ac.Spec.GetAuthz().GetToolCalls().Subject == "startedBy" {
		return startedBy
	}
	if cur := sess.Annotations[slack.LastInboundCanonicalIDAnnotationKey]; cur != "" {
		// Written by channelsd from the last inbound it resolved server-side.
		return identity.CanonicalFromTrusted(cur,
			"last-inbound canonical annotation written by channelsd")
	}
	return startedBy
}

// reconcileAuthzSessionConfig writes the snapshot for this session.
//
// ORDERING — this is the whole point of the move, so it is spelled out. It runs
// immediately after the AgentClass Valid gate, which is far above the runner
// pod: RunnerFactory.Start is at the very bottom of Reconcile. So on a FRESH
// session the record is durable before the process that reads it is created,
// and on a RESTART the same pass rewrites it before restarting the pod. There
// is no window in which a runner can read a missing record — unlike the
// arrangement this replaces, whose only guarantee was that the runner wrote it
// before calling loop.Run inside its own process.
//
// It returns an error rather than logging and continuing. That is a deliberate
// change from the runner's old log-and-continue: from here a failure is
// retryable for free (the reconcile requeues with backoff and the pod is not
// started yet), whereas booting a runner without the record just hands authzd
// an unresolvable policy and fails the session closed a moment later with a
// user-visible "I couldn't confirm this session's permission policy". Retrying
// the write beats surfacing a transient memory blip as a dead session.
func (r *Reconciler) reconcileAuthzSessionConfig(
	ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ac *spiceboxv1alpha1.AgentClass,
) error {
	if r.LifecycleMemory == nil {
		// Unit fixtures wire no memory. Production always does, so say something
		// an operator can grep rather than skipping in silence — a scope-enabled
		// class with no snapshot is a session authzd will refuse to start.
		if ac.Spec.GetScope().Enabled {
			log.FromContext(ctx).Info(
				"no memory wired; skipping the authz session config snapshot — this session's cold-start scope review cannot resolve and authzd will fail it closed",
				"session", sess.Namespace+"/"+sess.Name, "class", ac.Name)
		}
		return nil
	}

	scope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
	want := authzSessionConfigContent(sess, ac)

	// Read-before-write so an unchanged reconcile is a genuine no-op. The
	// accessor stamps Entry.CreatedAt from the wall clock on every Put, so
	// re-Putting identical content would still churn the stored entry on every
	// pass — the same idempotency bar an SSA-applied field is held to.
	if got, found, err := asc.Get(ctx, r.LifecycleMemory, scope); err == nil && found && reflect.DeepEqual(got, want) {
		return nil
	}

	if err := asc.Snapshot(ctx, r.LifecycleMemory, scope, want); err != nil {
		return fmt.Errorf("write authz session config snapshot: %w", err)
	}
	return nil
}
