// pkg/controllers/credentialupdaterequest/export_test.go
//
// Test-only exports for the external credentialupdaterequest_test package.
package credentialupdaterequest

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// MapCanonicalToFollowersForTest exposes the unexported canonical->followers
// map func. It is exported for tests SPECIFICALLY because that map func is the
// one half of propagation no end-to-end assertion can isolate: a follower that
// settles proves the Collapsed arm ran, but says nothing about whether anything
// would ever have ENQUEUED it in production. Calling it directly is the only
// way to make "the enqueue exists and selects the right requests" a fact a
// mutation can redden on its own.
func (r *Reconciler) MapCanonicalToFollowersForTest(ctx context.Context, o client.Object) []reconcile.Request {
	return r.mapCanonicalToFollowers(ctx, o)
}

// MapSecretToRequestsForTest exposes the unexported Secret->requests map func.
// It is exported for the same reason MapCanonicalToFollowersForTest is: the
// enqueue is the ONE half of unparking no end-to-end assertion can isolate. A
// reconcile driven directly proves reconcileOpen notices a changed Secret, but
// says nothing about whether a Secret write in production would ever have
// WOKEN that request -- and a selector that silently matches nothing is exactly
// how a request sits Open until its wait window elapses after a human has
// already pasted the replacement.
func (r *Reconciler) MapSecretToRequestsForTest(ctx context.Context, o client.Object) []reconcile.Request {
	return r.mapSecretToRequests(ctx, o)
}

// BudgetLimitForTest exposes the ask budget's limit. The budget tests spell
// out sequences of two-and-then-a-third asks, so their whole arithmetic assumes
// this value; asserting it makes a future change to the limit fail with "the
// limit is no longer two" rather than with a confusing "this ask should have
// been refused".
const BudgetLimitForTest = budgetLimit

// FollowerReasonForTest exposes the follower-facing reason renderer, so the
// honesty invariant ("a follower may never claim a human ignored a card it was
// never shown") can be asserted against every terminal phase directly, rather
// than only through whichever phases an end-to-end fixture happens to reach.
var FollowerReasonForTest = followerReason

// CanonicalOpenRequestForTest exposes the canonical election, because its
// lowest-name TIE-BREAK -- property (4), the line that stops followers
// splitting across two canonicals -- is unobservable through the reconciler.
// Every end-to-end route into it goes through a List, and the fake client
// returns List results NAME-SORTED, so first-match-wins and lowest-name-wins
// produce identical answers there and only a REVERSED comparison ever reddens.
// Production's cache-backed List carries no such ordering guarantee. Calling
// the election directly with peers in a deliberately unsorted order is the only
// way to make the tie-break a fact a deletion can redden.
var CanonicalOpenRequestForTest = canonicalOpenRequestFor

// CredentialHandleForTest builds the collapse key the election matches on, so a
// test calling CanonicalOpenRequestForTest directly constructs it the same way
// the reconciler does rather than restating its field layout.
var CredentialHandleForTest = handleForSource
