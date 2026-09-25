package hold

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/trifecta"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// TrifectaTripperDeps wires the containment half of the trifecta.
//
// EVERY input is operator-visible, and that is the whole design rather than a
// convenience. This package's doc says the runner is the party under
// suspicion, so a tripper that read runner-produced trifecta audit records
// would be defeated by the simplest possible attack: a compromised runner
// writes none, and the tripper concludes the closure is clean.
//
// So the legs are re-derived here from what the OPERATOR can see for itself —
// the bound data slots the SubagentRequest controller wrote to status, the
// tags' own SpiceDB facts, and the child's declared tools. None of it requires
// the runner to have cooperated, or even to still be alive.
type TrifectaTripperDeps struct {
	// Enabled gates the whole tripper. FALSE by default: containment that
	// freezes sessions must be switched on deliberately.
	Enabled bool

	Client client.Client

	// BoundTagsOf returns the tag ids bound into a session's data slots, read
	// from SubagentRequest status — written by the CONTROLLER, never by the
	// runner.
	BoundTagsOf func(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) ([]string, error)

	// TagCarriesUntrusted and TagReaders answer the tags' own SpiceDB
	// permissions; ChildAudience answers agentsession#read_transcript.
	TagCarriesUntrusted func(ctx context.Context, tagID string) (bool, error)
	TagReaders          func(ctx context.Context, tagID string) ([]string, error)
	ChildAudience       func(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) ([]string, error)

	// CanAct reports whether the session's CLASS can act — leg C, derived from
	// the class's declared tools rather than from anything the runner reports
	// about its own live tool set.
	CanAct func(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (bool, error)

	Logger *slog.Logger
}

// TrifectaTripper freezes a session whose closure has gone trifecta.
//
// The SIBLING of the dispatch gate, not a duplicate of it. They ask different
// questions in different processes: the gate asks "may this call proceed?" and
// denies one call; this asks "has this delegation gone wrong?" and freezes the
// session for review. A closure can reach the second state without the first
// ever firing — the gate may be in logging mode, or the runner may never have
// reported anything at all.
type TrifectaTripper struct{ deps TrifectaTripperDeps }

// NewTrifectaTripper builds the tripper.
func NewTrifectaTripper(deps TrifectaTripperDeps) *TrifectaTripper {
	return &TrifectaTripper{deps: deps}
}

func (t *TrifectaTripper) Name() string { return "trifecta-closure" }

func (t *TrifectaTripper) logger() *slog.Logger {
	if t.deps.Logger != nil {
		return t.deps.Logger
	}
	return slog.Default()
}

// OnSignal re-derives the trifecta from operator-visible state and trips.
//
// Read-only with respect to memory, per this package's contract: it receives
// SignalEntryAppended, which fires after every append-only Put, so a hook that
// reacted by Putting would fire the signal again and recurse without bound. On
// trip it creates a SessionHold CR through the Kubernetes client instead.
func (t *TrifectaTripper) OnSignal(ctx context.Context, sig memory.Signal) error {
	if !t.deps.Enabled || sig.Kind != memory.SignalEntryAppended {
		return nil
	}
	ns, name, ok := strings.Cut(sig.Scope.ID, "/")
	if !ok || ns == "" || name == "" {
		return fmt.Errorf("trifecta-closure: session scope id %q is not <namespace>/<name>", sig.Scope.ID)
	}

	var sess spiceboxv1alpha1.AgentSession
	if err := t.deps.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		if apierrors.IsNotFound(err) {
			// Gone before we looked. Nothing to freeze, and nothing wrong.
			return nil
		}
		return fmt.Errorf("trifecta-closure: get AgentSession %s: %w", sig.Scope.ID, err)
	}

	legs, err := t.deriveLegs(ctx, &sess)
	if err != nil {
		// Surfaced, never swallowed. A closure whose legs cannot be
		// established is not thereby clean, and the signal dispatcher logs and
		// retries rather than silently concluding nothing happened.
		return err
	}
	v := trifecta.Evaluate(legs)
	if !v.Refused {
		return nil
	}
	return t.trip(ctx, &sess, v.Reason)
}

// deriveLegs rebuilds the three legs from operator-visible state alone.
func (t *TrifectaTripper) deriveLegs(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (trifecta.Legs, error) {
	if t.deps.BoundTagsOf == nil || t.deps.CanAct == nil {
		return trifecta.Legs{}, fmt.Errorf("trifecta-closure: tripper is not fully wired")
	}
	canAct, err := t.deps.CanAct(ctx, sess)
	if err != nil {
		return trifecta.Legs{}, fmt.Errorf("trifecta-closure: whether %s/%s can act: %w", sess.Namespace, sess.Name, err)
	}
	tags, err := t.deps.BoundTagsOf(ctx, sess)
	if err != nil {
		return trifecta.Legs{}, fmt.Errorf("trifecta-closure: bound tags of %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	if len(tags) == 0 {
		return trifecta.Legs{Consequential: canAct}, nil
	}

	audience, err := t.deps.ChildAudience(ctx, sess)
	if err != nil {
		return trifecta.Legs{}, fmt.Errorf("trifecta-closure: audience of %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	// Derive through the SAME code the dispatch gate uses, from a Handoff built
	// out of operator-visible facts. One derivation, two processes — so the two
	// halves cannot drift into disagreeing about what a leg means.
	legs, err := trifecta.Derive(ctx, trifecta.Deps{
		TagCarriesUntrusted: t.deps.TagCarriesUntrusted,
		TagReaders:          t.deps.TagReaders,
		ChildAudience: func(context.Context, authz.SessionRef) ([]string, error) {
			return audience, nil
		},
	}, trifecta.Handoff{BoundInputs: tags})
	if err != nil {
		return trifecta.Legs{}, err
	}
	legs.Consequential = canAct
	return legs, nil
}

func (t *TrifectaTripper) trip(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, reason string) error {
	h := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{
			// Deterministic, so a repeated signal on an already-held session is
			// an AlreadyExists no-op rather than a second hold per append.
			Name:            "trifecta-closure-" + sess.Name,
			Namespace:       sess.Namespace,
			OwnerReferences: cosidecar.OwnerRef(sess),
		},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: sess.Namespace, Name: sess.Name},
			Reason:     reason,
			Source:     "tripper/" + t.Name(),
		},
	}
	if err := t.deps.Client.Create(ctx, h); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("trifecta-closure: create SessionHold %s/%s: %w", sess.Namespace, h.Name, err)
	}
	t.logger().Info("trifecta closure tripped a forensic hold",
		"session", sess.Namespace+"/"+sess.Name, "hold", h.Name, "reason", reason)
	return nil
}
