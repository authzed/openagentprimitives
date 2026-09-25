package agentsession

import (
	"context"
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagedecision"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ClosureDeniedFor reports whether ANY member of sess's delegation closure has
// a denied authorization decision recorded.
//
// WHY A CLOSURE AND NOT A SESSION. A denial the parent cannot get past is
// exactly what a parent routes around: it cannot make the call, so it spawns a
// child that can, or re-splits the task, or rephrases it. A per-session flag
// makes that work. Keying on the closure is what makes the refusal survive
// delegation, which is the whole point of §2.8 — the same reasoning that made
// the denial STREAK closure-aware rather than per-session.
//
// WHY THE OPERATOR. Answering it needs every session in the tree, and the
// runner's Role grants no `list` on agentsessions at all — it can Get its own
// object and nothing else. A runner-side version of this walk is Forbidden at
// the first hop in production while passing every test that drives it with an
// admin client. The operator computes; the runner reads the answer off its own
// session.
//
// The read is over each member's OWN memory scope, because a decision is
// recorded where it was made. Merging them is the closure part.
func ClosureDeniedFor(
	ctx context.Context,
	c client.Reader,
	mem memory.Memory,
	sess *spiceboxv1alpha1.AgentSession,
) (bool, error) {
	if sess == nil || mem == nil {
		return false, nil
	}

	scopes, err := closureScopes(ctx, c, sess)
	if err != nil {
		return false, err
	}
	for _, sc := range scopes {
		recs, err := infoleakagedecision.List(ctx, mem, sc)
		if err != nil {
			// An unreadable member is NOT a clean closure. Reporting false here
			// would clear a flag the dispatch gate refuses on in every mode,
			// which is the one direction this must never fail.
			return false, fmt.Errorf("closure denial scan at %s: %w", sc.ID, err)
		}
		for _, r := range recs {
			if r.Decision == infoleakagedecision.DecisionDenied {
				return true, nil
			}
		}
	}
	return false, nil
}

// closureScopes returns one memory scope per member of sess's delegation tree
// — the root and every labelled descendant.
//
// Built from LabelDelegationRoot via ListClosure rather than a parent walk per
// member: the label exists precisely so the whole tree is one indexed List, and
// the denial streak already reads it this way. A session with no lineage is its
// own closure, which costs exactly one scope and no List result to scan.
func closureScopes(ctx context.Context, c client.Reader, sess *spiceboxv1alpha1.AgentSession) ([]memory.Scope, error) {
	rootName := spiceboxv1alpha1.RootNameFor(sess)
	members, err := spiceboxv1alpha1.ListClosure(ctx, c, sess.Namespace, rootName)
	if err != nil {
		return nil, fmt.Errorf("closure of %s/%s: %w", sess.Namespace, rootName, err)
	}
	// The root itself is never labelled with its own name, so ListClosure
	// returns descendants only and the root is added here.
	scopes := make([]memory.Scope, 0, len(members)+1)
	scopes = append(scopes, memory.Scope{Kind: "session", ID: sess.Namespace + "/" + rootName})
	for i := range members {
		m := &members[i]
		scopes = append(scopes, memory.Scope{Kind: "session", ID: m.Namespace + "/" + m.Name})
	}
	return scopes, nil
}
