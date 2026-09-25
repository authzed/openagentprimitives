package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
)

// revocationFailureCategory is the notice row a failed invalidation rides.
//
// It has a row of its own because no existing one means "a withdrawal of access
// did not take effect here": internal_error is ToneDegraded and documents itself
// as a failure a retry fixes, which this is not — re-sending a message withdraws
// nothing. revocation_not_applied is TonePrivacy, non-terminal: privacy sits
// deliberately off the severity ramp (channelinteractions/tone.go) and is the
// tone for "who can reach whose data", and the reader genuinely can act — end
// the session, or withdraw the access where it was granted.
const revocationFailureCategory = categories.RevocationNotApplied

// revocationFailureNotifier tells a session's participants that a withdrawal of
// access could not be applied here.
//
// It exists because the subscriber's success hook is how a revocation becomes a
// durable claim (loop.EmitRevoked appends a signed Revoked record), so a failed
// invalidation deliberately runs no hook at all — see pkg/authz/revocation's
// RegisterSubscriber. That leaves the failure in a log line and nowhere a person
// can see it, and the person who withdrew the access is waiting on exactly that
// outcome (AGENTS.md §Never silently drop errors, clause 3).
//
// It NEVER records the revocation as applied: it publishes one read-only notice
// and returns. Writing any durable "revoked" marker here would resurrect the bug
// the hook-skipping fix removed.
type revocationFailureNotifier struct {
	publish channelevents.PublishFunc
	// signer signs the notice's envelope with the session's identity key.
	// Nil-safe: a nil signer publishes unsigned (test fixtures without a
	// signer still work).
	signer *channelevents.EnvelopeSigner
	sess   channelevents.SessionRef

	// mu guards notified. report runs on the NATS subscription goroutine, and
	// claimAndRecover dispatches through the same registry on the Run
	// goroutine, so two callers can race for the same key.
	mu sync.Mutex
	// notified is the dedup set, keyed by kind+key. Core NATS Subscribe has no
	// ack, so one failed revocation can be delivered to this process more than
	// once; without this the reader gets one message per delivery for a
	// condition that has not changed.
	//
	// Deliberately in-memory and deliberately never persisted. It is bounded by
	// the number of distinct revocations addressed to one session, and losing it
	// on restart is CORRECT rather than a durability gap: after a restart the
	// broker cache is empty and the MCPTools have re-frozen the credential, so
	// the capability really is live again in a new process and the failure is a
	// new fact about it. A persisted marker would also be one write away from
	// reading as the revocation record itself.
	notified map[string]struct{}
}

// newRevocationFailureNotifier builds the notifier for one session. publish may
// be nil (a session with no channel surface); report then logs and posts
// nothing, which is the only thing it can honestly do.
func newRevocationFailureNotifier(publish channelevents.PublishFunc, signer *channelevents.EnvelopeSigner, sess channelevents.SessionRef) *revocationFailureNotifier {
	return &revocationFailureNotifier{
		publish:  publish,
		signer:   signer,
		sess:     sess,
		notified: map[string]struct{}{},
	}
}

// report surfaces one failed invalidation, at most once per (kind, key).
//
// Best-effort by construction: it is called from the invalidator decorator on a
// path whose error is already being returned to the subscriber, so a publish
// failure is logged and never propagated — the invalidation outcome must not
// depend on whether the chat surface accepted a message.
func (n *revocationFailureNotifier) report(_ context.Context, kind, noun, key string, cause error) {
	if n == nil {
		return
	}
	sessRef := n.sess.Namespace + "/" + n.sess.Name
	dedupKey := kind + "\x00" + key

	n.mu.Lock()
	_, seen := n.notified[dedupKey]
	if !seen {
		n.notified[dedupKey] = struct{}{}
	}
	n.mu.Unlock()

	if seen {
		// Not silence: the operator still gets a line per delivery, so a storm of
		// redeliveries is visible in logs even though the reader sees one message.
		slog.Default().Info("revocation not applied; already surfaced to this session",
			"session", sessRef, "kind", kind, "key", key, "err", cause.Error())
		return
	}
	if n.publish == nil {
		slog.Default().Info("revocation not applied and no channel surface to say so",
			"session", sessRef, "kind", kind, "key", key, "err", cause.Error())
		return
	}
	// Logged as well as surfaced: the notice says what the reader can act on,
	// the log line keeps the cause an operator needs.
	slog.Default().Info("revocation not applied; surfacing to session", "session", sessRef,
		"kind", kind, "key", key, "err", cause.Error())
	// The ref names the failure it reports, so two failed revocations in one
	// session are two distinct requests on the wire rather than one repeated ref.
	// Built from kind+key directly: dedupKey's NUL separator has no business on a
	// wire identifier.
	err := revocationFailureNotice(noun, kind, key).PublishSigned(n.signer, n.publish, n.sess,
		"notice-revocation-not-applied-"+n.sess.Name+"-"+kind+"-"+key)
	if err != nil {
		// Give the key back, so a redelivery of this same revoke gets another
		// chance to reach the reader. The dedup set means "already told them",
		// and a publish that failed told nobody. Not a retry loop: nothing is
		// re-driven here, and if every attempt fails the reader sees nothing
		// either way while the operator still gets one line per delivery.
		n.mu.Lock()
		delete(n.notified, dedupKey)
		n.mu.Unlock()
		slog.Default().Info("publish revocation-not-applied notice", "session", sessRef,
			"kind", kind, "key", key, "err", err.Error())
	}
}

// revocationFailureNotice is the user-facing half of a failed invalidation.
//
// noun names what was withdrawn and comes from the Invalidator that refused —
// revocation.Invalidator.Noun, a fixed phrase owned by the kind's own package.
// That is what lets the copy say "a connected account" without this package
// switching on kind, which would be the `if kind == "x"` AGENTS.md forbids
// outside the kind's own package. It is safe in Body (which surfaces render as
// trusted markup) precisely because it is a repo-owned constant and never
// anything derived from the revoke key off the bus.
//
// The revoke key IS bus-supplied, and is operator vocabulary
// (<namespace>/<secretName>), so it travels in the Excerpt, which every surface
// renders inert — never interpolated into Lead/Body/NextStep.
func revocationFailureNotice(noun, kind, key string) *notice.Notice {
	return notice.New(revocationFailureCategory, notice.Args{
		Lead: "Access that was withdrawn may still be usable here",
		Body: "Someone withdrew " + withdrawnThing(noun) + " this conversation is using, " +
			"and this conversation could not apply that change. " +
			"The agent may still be able to use it until this conversation ends.",
		NextStep: "Treat the access as still live: end this conversation, and withdraw " +
			"the access wherever it was originally granted.",
		Excerpt: &channelevents.InteractionExcerpt{
			Label:   "Reference",
			Content: fmt.Sprintf("%s %s", kind, key),
		},
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	})
}

// withdrawnThing is the noun phrase the body reads "Someone withdrew ___ this
// conversation is using".
//
// It falls back to the bare "access" so the sentence survives an Invalidator
// that has no useful noun to offer. A future kind returning "" must degrade to
// vaguer copy, never to "Someone withdrew  this conversation is using" — the
// interface documents the empty case as legal, so the only place that can be
// enforced is here, at the one substitution site.
func withdrawnThing(noun string) string {
	if strings.TrimSpace(noun) == "" {
		return "access"
	}
	return noun
}

// surfacingInvalidator reports a failed Invalidate to a session's participants
// and returns the error unchanged.
//
// It wraps at the REGISTRY rather than at the subscriber so both dispatch paths
// are covered by one seam: the live ap.revocation subscriber, and
// claimAndRecover's restart re-application, which looks the invalidator up in
// the same registry and today also only logs on failure.
//
// The error is returned untouched, so the subscriber still skips its hook and
// the session's signed log still records nothing. This type only makes a failure
// visible; it never claims a revocation succeeded.
type surfacingInvalidator struct {
	inner  revocation.Invalidator
	report func(ctx context.Context, kind, noun, key string, cause error)
}

func (s *surfacingInvalidator) Kind() string { return s.inner.Kind() }

// Noun forwards the wrapped kind's own noun, unchanged. A decorator that
// invented one here would put a per-kind word back in this package, which is
// the switch wrapping at the registry exists to avoid.
func (s *surfacingInvalidator) Noun() string { return s.inner.Noun() }

func (s *surfacingInvalidator) Invalidate(key string) error {
	err := s.inner.Invalidate(key)
	if err != nil && s.report != nil {
		// Invalidate carries no context (the interface is called from the NATS
		// callback and from restart recovery alike); the notifier only needs one
		// for symmetry with the other publish sites.
		s.report(context.Background(), s.inner.Kind(), s.inner.Noun(), key, err)
	}
	return err
}

var _ revocation.Invalidator = (*surfacingInvalidator)(nil)
