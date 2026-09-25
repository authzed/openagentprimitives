package v1alpha1

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LabelDelegationRoot names the root of the delegation tree a session belongs
// to. It is absent on a root and present on every descendant, set once at
// creation by the SubagentRequest controller and never updated — the value is
// a pure function of the parent, so a re-apply is byte-identical.
//
// It exists so the delegation closure is one indexed List for the whole tree
// rather than a walk per member -- true of ListClosure itself; DescendantsOf
// narrows that List down to one member's subtree with a containment walk per
// member, since a "whole tree" List carries no per-member ancestry on its
// own. The pooled total-agent ceiling, the forensic-hold cascade and the
// closure-aware denial streak all need "every session in this tree", and all
// three would otherwise re-derive it from parent chains on every reconcile.
const LabelDelegationRoot = "agentprimitives.authzed.com/delegation-root"

// LabelAttendedParentNamespace and LabelAttendedParentName together name the
// session watching an `attended` delegated child — sr.Spec.Parent, the
// request's OWN parent, not necessarily the tree's root. Set once at
// creation, alongside LabelDelegationRoot, and never updated: the pair is a
// pure function of the request that created the child, so a re-apply is
// byte-identical.
//
// Split into TWO labels rather than one "<namespace>/<name>" value for the
// same reason LabelWorkshopSessionNamespace/LabelWorkshopSessionName are
// (workshop_types.go): a Kubernetes label VALUE may not contain "/" —
// validation.IsValidLabelValue rejects it — so a combined value would be
// rejected by a real apiserver on every attended child's Create, a failure a
// fake-client unit test would never catch. Each half, a bare namespace or
// session name, is independently a valid label value on its own.
//
// Two things read the pair. The one-attended-child-at-a-time check (spec
// §lifecycle) lists a tree's whole closure and keeps only members carrying
// LabelAttendedParentName with a non-terminal AgentSession phase — its mere
// presence is the "this is an attended child" marker, independent of
// whatever the SubagentRequest's own spec.mode says today. Tasks 6-7's
// routing (append the child's turns to the watching session's inbox, wake it
// on mention or on the child's error/completion) resolve the watching
// session FROM this pair rather than re-deriving it from the child's
// OwningSubagentRequest at delivery time — a live read on every turn the
// SubagentRequest controller already paid for once, at creation. Both are
// still ordinary label VALUES (a namespace or session name), so a caller who
// needs to find every child watched by one session can select on them
// directly (client.MatchingLabels{LabelAttendedParentNamespace: ns,
// LabelAttendedParentName: name}) with no hash or List-then-filter.
const (
	LabelAttendedParentNamespace = "agentprimitives.authzed.com/attended-parent-namespace"
	LabelAttendedParentName      = "agentprimitives.authzed.com/attended-parent-name"
)

// RootNameFor returns the name of the delegation tree sess belongs to: sess's
// own LabelDelegationRoot value if it carries one, and sess's own name
// otherwise — a session with no label IS a root, by construction.
//
// This is the induction step: buildChild calls RootNameFor(parent) to compute
// a new child's root with no API call and no walk, by reading the parent's
// label (or, when the parent is itself a root, the parent's name) once, at
// creation time. The same read applies to any session, not only a parent —
// DescendantsOf calls RootNameFor(sess) to resolve sess's own tree root the
// same way before listing.
func RootNameFor(sess *AgentSession) string {
	if root, ok := sess.Labels[LabelDelegationRoot]; ok && root != "" {
		return root
	}
	return sess.Name
}

// ListClosure returns every session in namespace ns labelled as a member of
// the delegation tree rooted at rootName.
//
// It returns DESCENDANTS ONLY. The root itself is never labelled with its own
// name — RootNameFor's whole point is that a root needs no label — so the
// root is never a member of this list. A caller counting the whole tree (the
// pooled agent ceiling in particular) must add one for the root itself.
func ListClosure(ctx context.Context, r client.Reader, ns, rootName string) ([]AgentSession, error) {
	var list AgentSessionList
	if err := r.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{LabelDelegationRoot: rootName}); err != nil {
		return nil, fmt.Errorf("list delegation closure for root %q in namespace %q: %w", rootName, ns, err)
	}
	return list.Items, nil
}

// WorkshopNamespacesFor resolves the workshop namespace(s) root itself
// provisioned — never a descendant's — so a caller can extend a
// root-namespace-scoped closure List (ListClosure) across the workshop
// boundary Task 2 introduced without ListClosure itself ever becoming a
// cluster-wide List.
//
// A builder session gets AT MOST one Workshop CR, deterministically named
// WorkshopName(root.Name) in root.Namespace (workshop_types.go), so this is a
// single Get, never a List: the slice return is future-proofing for a root
// that could someday own more than one, not a sign this List's shape.
//
// The Workshop CR must be genuinely CONTROLLER-OWNED by root (an
// AgentSession-kind owner ref whose UID equals root.UID) — the same test
// workshopIdentityFor's SECURITY note applies to a sidecar identity, applied
// here to a resource count instead: an unowned or foreign-owned same-named CR (a
// leftover from a deleted-and-recreated root under the same name, which
// mints a fresh UID and so a fresh workshop per WorkshopNamespaceName's own
// doc) must not have its live children silently pooled into an unrelated
// root's ceiling.
//
// Returns (nil, nil) — not an error — when root has no Workshop, the found
// Workshop is not root's own, or the Workshop has not yet recorded a
// status.namespace (still Provisioning). Any of those simply means "root
// provisioned no workshop namespace to add", which is the ordinary case for
// every non-builder session and is not a failure.
func WorkshopNamespacesFor(ctx context.Context, r client.Reader, root *AgentSession) ([]string, error) {
	var ws Workshop
	key := client.ObjectKey{Namespace: root.Namespace, Name: WorkshopName(root.Name)}
	if err := r.Get(ctx, key, &ws); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get workshop %s/%s for delegation root %q: %w", key.Namespace, key.Name, root.Name, err)
	}
	owner := metav1.GetControllerOf(&ws)
	if owner == nil || owner.Kind != "AgentSession" || owner.UID != root.UID {
		return nil, nil
	}
	if ws.Status.Namespace == "" {
		return nil, nil
	}
	return []string{ws.Status.Namespace}, nil
}

// DescendantsOf returns every session in sess's own subtree: strict
// descendants of sess, excluding sess itself, its ancestors, and any other
// branch of the same tree. Unlike ListClosure, which returns an entire tree
// given its root's name, DescendantsOf narrows a tree-wide closure down to one
// member's own subtree — for a hold cascade or a ceiling scoped to a mid-tree
// delegation rather than the whole tree.
//
// It resolves sess's tree root via RootNameFor, lists that whole closure via
// ListClosure, then keeps only members whose ancestor chain passes through
// sess — using WalkAncestors for the containment test rather than a fourth
// hand-rolled walk. A member whose walk errors propagates the error
// immediately rather than being silently excluded: dropping a session from a
// hold cascade because its ancestor walk happened to fail would defeat the
// cascade's entire purpose.
func DescendantsOf(ctx context.Context, r client.Reader, sess *AgentSession) ([]AgentSession, error) {
	closure, err := ListClosure(ctx, r, sess.Namespace, RootNameFor(sess))
	if err != nil {
		return nil, err
	}

	var out []AgentSession
	for i := range closure {
		member := &closure[i]
		if member.Namespace == sess.Namespace && member.Name == sess.Name {
			continue // sess is never its own descendant
		}
		isDescendant := false
		err := WalkAncestors(ctx, r, member, func(cur *AgentSession) (bool, error) {
			if cur.Namespace == sess.Namespace && cur.Name == sess.Name {
				isDescendant = true
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk ancestors of %s/%s: %w", member.Namespace, member.Name, err)
		}
		if isDescendant {
			out = append(out, *member)
		}
	}
	return out, nil
}
