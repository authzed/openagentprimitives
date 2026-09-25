package hold

import (
	"context"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// ClosureDenialStamperDeps wires the §2.8 closure denial set.
type ClosureDenialStamperDeps struct {
	Client client.Client
	// Denied reports whether the scope that signalled has a denied decision.
	// Injected rather than reading the kind directly so this package does not
	// take a dependency on the decision kind's storage shape.
	Denied func(ctx context.Context, scope memory.Scope) (bool, error)
	Logger *slog.Logger
}

// ClosureDenialStamper records, on EVERY member of a delegation closure, that
// someone in it was denied.
//
// WHAT IT IS FOR. A denial the parent cannot get past is exactly what a parent
// routes around: it cannot make the call, so it delegates to a child that can,
// or re-splits, or rephrases. Keying the refusal to one session makes that
// work. The trifecta dispatch gate consults this flag and refuses in EVERY
// mode, because a denied closure is a structural fact — this delegation has
// already been judged to have gone wrong — rather than a policy judgement about
// the call in front of it.
//
// WHY IT IMPLEMENTS Tripper. Not because it trips a hold — it does not — but
// because Tripper IS the signal seam: {Name, OnSignal} on the plangate_hold
// Kind's ScopeHooks, delivered when a decision is appended. Reusing it means
// the stamp happens on the event that makes it true, once, instead of being
// recomputed on every reconcile of every session in the cluster. Trippers
// composes it beside the judgements that do freeze things.
//
// WHY A STAMP AND NOT A LOOKUP. The party that needs the answer is the RUNNER,
// and the runner cannot compute it: answering needs a List over the delegation
// tree, which its Role does not grant — it can Get its own object and nothing
// else. So the operator writes the fact where the runner is allowed to read it.
type ClosureDenialStamper struct{ deps ClosureDenialStamperDeps }

func NewClosureDenialStamper(deps ClosureDenialStamperDeps) *ClosureDenialStamper {
	return &ClosureDenialStamper{deps: deps}
}

func (s *ClosureDenialStamper) Name() string { return "closure-denial-stamp" }

func (s *ClosureDenialStamper) logger() *slog.Logger {
	if s.deps.Logger != nil {
		return s.deps.Logger
	}
	return slog.Default()
}

func (s *ClosureDenialStamper) OnSignal(ctx context.Context, sig memory.Signal) error {
	if s.deps.Client == nil || s.deps.Denied == nil {
		return nil
	}
	denied, err := s.deps.Denied(ctx, sig.Scope)
	if err != nil {
		return fmt.Errorf("closure denial stamp: reading decisions for %s: %w", sig.Scope.ID, err)
	}
	if !denied {
		// Only ever set. A later approval does not clear the fact that a
		// refusal happened in this closure, and clearing on the next clean
		// signal would let a parent launder a denial by making one approved
		// call afterwards.
		return nil
	}

	ns, name, ok := splitScopeID(sig.Scope.ID)
	if !ok {
		return fmt.Errorf("closure denial stamp: unparseable scope id %q", sig.Scope.ID)
	}
	var sess spiceboxv1alpha1.AgentSession
	if err := s.deps.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sess); err != nil {
		return fmt.Errorf("closure denial stamp: session %s: %w", sig.Scope.ID, err)
	}

	rootName := spiceboxv1alpha1.RootNameFor(&sess)
	members, err := spiceboxv1alpha1.ListClosure(ctx, s.deps.Client, ns, rootName)
	if err != nil {
		return fmt.Errorf("closure denial stamp: closure of %s/%s: %w", ns, rootName, err)
	}

	// The root plus every labelled descendant. ListClosure returns descendants
	// only — a root is never labelled with its own name.
	targets := make([]types.NamespacedName, 0, len(members)+1)
	targets = append(targets, types.NamespacedName{Namespace: ns, Name: rootName})
	for i := range members {
		targets = append(targets, types.NamespacedName{Namespace: members[i].Namespace, Name: members[i].Name})
	}

	// Every member is attempted even if one fails. A closure where half the
	// members learned of the denial is strictly better than one where the first
	// error stopped the fan-out, and the aggregate error still surfaces.
	var firstErr error
	for _, t := range targets {
		if err := s.stampOne(ctx, t); err != nil {
			s.logger().Info("closure denial stamp failed for one member; the rest still get it",
				"member", t.String(), "err", err.Error())
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// stampOne sets closureDenied on one member, and only when it is not already
// set — a no-op write would churn status and re-wake every watcher on a field
// that never changes back.
func (s *ClosureDenialStamper) stampOne(ctx context.Context, key types.NamespacedName) error {
	var sess spiceboxv1alpha1.AgentSession
	if err := s.deps.Client.Get(ctx, key, &sess); err != nil {
		return err
	}
	if sess.Status.ClosureDenied != nil && *sess.Status.ClosureDenied {
		return nil
	}
	base := sess.DeepCopy()
	denied := true
	sess.Status.ClosureDenied = &denied
	return s.deps.Client.Status().Patch(ctx, &sess, client.MergeFrom(base))
}

// splitScopeID splits a "<ns>/<name>" session scope id.
func splitScopeID(id string) (ns, name string, ok bool) {
	for i := 0; i < len(id); i++ {
		if id[i] == '/' {
			return id[:i], id[i+1:], i > 0 && i+1 < len(id)
		}
	}
	return "", "", false
}
