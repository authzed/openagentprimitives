package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

const (
	curTestNS   = testNS
	curTestName = "demo-cur"
	curCredName = "github-pat"
)

// fixtureCUR returns an Open CredentialUpdateRequest naming fixtureSession's
// session, with a resolved credential + platform-authored reason -- the
// shape channelsd's watcher expects to find on every poll.
func fixtureCUR(mutate ...func(*spiceboxv1alpha1.CredentialUpdateRequest)) *spiceboxv1alpha1.CredentialUpdateRequest {
	cur := &spiceboxv1alpha1.CredentialUpdateRequest{
		ObjectMeta: metav1.ObjectMeta{Name: curTestName, Namespace: curTestNS, UID: types.UID("demo-cur-uid")},
		Spec: spiceboxv1alpha1.CredentialUpdateRequestSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: testNS, Name: testSession},
			ToolName:   "github__create_issue",
			Why:        "my push kept failing so I assumed the token expired",
		},
		Status: spiceboxv1alpha1.CredentialUpdateRequestStatus{
			Phase:         spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
			Determination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified,
			Reason:        "GitHub rejected this token: 401 Unauthorized — Bad credentials",
			ResolvedCredential: &spiceboxv1alpha1.ResolvedCredentialRef{
				IdentityKind: "SessionUserIdentity",
				Namespace:    testNS,
				Name:         testSession,
				Credential:   curCredName,
			},
		},
	}
	for _, m := range mutate {
		m(cur)
	}
	return cur
}

// newCredentialUpdateWatcher builds a fake-client-backed watcher wired with a
// working signer + external URL + capturingPublisher, so a test only needs to
// mutate the specific thing under test.
func newCredentialUpdateWatcher(t *testing.T, objs ...client.Object) (client.Client, *CredentialUpdateWatcher, *capturingPublisher) {
	t.Helper()
	scheme := newTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.CredentialUpdateRequest{}).
		Build()
	pub := &capturingPublisher{}
	w := &CredentialUpdateWatcher{
		K8s:             c,
		LinkSigner:      passthroughlink.New([]byte("test-signing-key-not-for-production")),
		ExternalBaseURL: func() string { return "https://agent.example.invalid" },
		NATSPublish:     pub.publish,
	}
	return c, w, pub
}

func getCURFrom(t *testing.T, c client.Client) *spiceboxv1alpha1.CredentialUpdateRequest {
	t.Helper()
	var got spiceboxv1alpha1.CredentialUpdateRequest
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: curTestNS, Name: curTestName}, &got))
	return &got
}

// --- The recipient must carry a deliverable Kind ---------------------------

// TestReconcileOne_RecipientCarriesChannelKind: pkg/channels/channelkinds/slack/
// interaction.go's sendRequest skips every recipient whose Kind != "slack"
// before ever calling Principal(), so a Requester built with Subject set but
// Kind empty is silently dropped and the whole card vanishes (0 ephemeral,
// 0 DM, Send returns nil). Dropping the Kind:
// identity.Kind(sess.Spec.InputChannel.Kind) assignment in ReconcileOne makes
// this fail.
func TestReconcileOne_RecipientCarriesChannelKind(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureCUR()
	_, w, pub := newCredentialUpdateWatcher(t, sess, cur)

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	got := pub.findInteractionRequest(t)
	require.NotNil(t, got.Audience.Requester)
	assert.Equal(t, sess.Spec.InputChannel.Kind, string(got.Audience.Requester.Kind),
		"Requester.Kind must be the session's channel kind -- omitting it makes the Slack sender drop the card silently")
	assert.True(t, got.Audience.Requester.HasIdentity(), "the recipient must be resolvable at all")
}

// TestReconcileOne_RecipientPrefersExternalIDAndEmail proves the recipient is
// built the SAME way credential_request.go's starterIdentity is: Kind +
// ExternalID + Email from the started-by annotations, Subject used only as a
// fallback when no verified email is on record.
func TestReconcileOne_RecipientPrefersExternalIDAndEmail(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureCUR()
	_, w, pub := newCredentialUpdateWatcher(t, sess, cur)

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	got := pub.findInteractionRequest(t)
	r := got.Audience.Requester
	require.NotNil(t, r)
	assert.Equal(t, testStarterExternalID, string(r.ExternalID))
	assert.Equal(t, testStarterEmail, string(r.Email))
	assert.Empty(t, string(r.Subject), "Subject is a fallback only -- must not be set when Email is known")
}

// TestReconcileOne_RecipientFallsBackToSubjectWhenNoEmail covers the dangerous
// shape: a Subject-only requester (no Email on record) satisfies
// InteractionRequestPayload.Validate on Subject alone (hasSubject), so a
// missing Kind does NOT fail loudly here the way it does when Email/ExternalID
// are present (see TestReconcileOne_RecipientCarriesChannelKind). The Kind
// assertion below is therefore load-bearing on its own: without it this test
// would pass even with Kind stripped, and the card would silently vanish.
func TestReconcileOne_RecipientFallsBackToSubjectWhenNoEmail(t *testing.T) {
	sess := fixtureSession(t, func(s *spiceboxv1alpha1.AgentSession) {
		delete(s.Annotations, spiceboxv1alpha1.AnnotationStartedByEmail)
	})
	cur := fixtureCUR()
	_, w, pub := newCredentialUpdateWatcher(t, sess, cur)

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	got := pub.findInteractionRequest(t)
	r := got.Audience.Requester
	require.NotNil(t, r)
	assert.Empty(t, string(r.Email))
	assert.Equal(t, testStarter, string(r.Subject), "no verified email -> fall back to the precomputed Subject canonical")
	assert.Equal(t, sess.Spec.InputChannel.Kind, string(r.Kind),
		"Kind must be set even on the Subject-fallback path: Validate() passes on Subject alone, "+
			"so a missing Kind here would NOT be caught by payload validation -- only by the Slack "+
			"sender's own Kind!=\"slack\" check, silently, in production")
}

// --- Card content rules ------------------------------------------------

func TestReconcileOne_LeadIsSanitizedReason(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureCUR()
	_, w, pub := newCredentialUpdateWatcher(t, sess, cur)

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	got := pub.findInteractionRequest(t)
	assert.Equal(t, cur.Status.Reason, got.Lead, "Lead must be the platform verdict line (already clean, so sanitization is a no-op)")
}

// TestReconcileOne_LeadSanitizesDirtyProviderText: for VerifyRejected, Reason
// is ProbeDetail -- the provider's raw HTTP response body, up to 200 bytes,
// which can carry newlines, markup and control characters straight onto a
// user-facing card. Dropping the sanitizeCardText(cur.Status.Reason) call
// makes this fail.
func TestReconcileOne_LeadSanitizesDirtyProviderText(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureCUR(func(c *spiceboxv1alpha1.CredentialUpdateRequest) {
		c.Status.Reason = "401 Unauthorized\n\n\x1b[1mFORGED HEADING\x1b[0m\r\nplease trust this"
	})
	_, w, pub := newCredentialUpdateWatcher(t, sess, cur)

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	got := pub.findInteractionRequest(t)
	assert.NotContains(t, got.Lead, "\n", "sanitized Lead must never contain a literal newline")
	assert.NotContains(t, got.Lead, "\r")
	assert.NotContains(t, got.Lead, "\x1b", "sanitized Lead must never contain a raw control/escape byte")
	assert.Contains(t, got.Lead, "401 Unauthorized")
}

// TestSanitizeCardTextDropsFormatCharacters covers the half that line
// structure and HTML escaping do NOT: Unicode FORMAT characters (category Cf)
// survive both and reorder or hide text visually inside the rendered line, so
// an attributed block can be made to read as something the author never wrote.
// Dropping only the C0/C1 controls leaves that open.
func TestSanitizeCardTextDropsFormatCharacters(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{name: "right-to-left override reorders the rest of the line", in: "revoked‮dnekot dilav"},
		{name: "right-to-left embedding", in: "revoked‫hidden‬ tail"},
		{name: "first strong isolate", in: "revoked⁨spoofed⁩ tail"},
		{name: "zero-width space splits a word invisibly", in: "rev​oked"},
		{name: "zero-width joiner", in: "rev‍oked"},
		{name: "soft hyphen", in: "rev­oked"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeCardText(tc.in)
			for _, r := range got {
				assert.False(t, unicode.Is(unicode.Cf, r),
					"sanitized text must carry no Unicode format character (found U+%04X in %q)", r, got)
			}
			assert.NotEmpty(t, got, "the visible characters must survive; only the format ones are dropped")
		})
	}
}

// TestSanitizeCardTextKeepsOrdinaryText is the other half of the rule above:
// dropping format characters must not touch anything a human actually reads,
// including non-Latin scripts and emoji that carry no joiner.
func TestSanitizeCardTextKeepsOrdinaryText(t *testing.T) {
	for _, in := range []string{
		"401 Unauthorized - Bad credentials",
		"la clé n'est plus valide",
		"トークンが失効しました",
		"token expired 🔑",
	} {
		assert.Equal(t, in, sanitizeCardText(in), "ordinary text must pass through untouched")
	}
}

// TestReconcileOne_ToolFieldIsSanitized pins C7: the Tool row and the
// monitoring event render the SAME cur.Spec.ToolName, and the two must agree
// about whether that value is trusted. The CRD field carries no pattern, so
// the card sanitizes it rather than relying on the runner's normalization
// holding forever.
func TestReconcileOne_ToolFieldIsSanitized(t *testing.T) {
	const dirty = "github__create_issue\n\x1b[1mFORGED\x1b[0m‮dnekot"
	sess := fixtureSession(t)
	cur := fixtureCUR(func(c *spiceboxv1alpha1.CredentialUpdateRequest) { c.Spec.ToolName = dirty })
	_, w, pub := newCredentialUpdateWatcher(t, sess, cur)

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	got := pub.findInteractionRequest(t)
	var toolValue string
	for _, f := range got.Fields {
		if f.Label == "Tool" {
			toolValue = f.Value
		}
	}
	require.NotEmpty(t, toolValue, "the card must carry a Tool row")
	assert.Equal(t, sanitizeCardText(dirty), toolValue,
		"the Tool row must be sanitized the same way publishAgentOwnedMonitoring sanitizes the same value")
	assert.NotContains(t, toolValue, "\n")
	assert.NotContains(t, toolValue, "\x1b")
	assert.NotContains(t, toolValue, "‮")
	assert.Contains(t, toolValue, "github__create_issue", "the readable name must survive")
}

func TestReconcileOne_BodyAttributesWhyVisibly(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureCUR()
	_, w, pub := newCredentialUpdateWatcher(t, sess, cur)

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	got := pub.findInteractionRequest(t)
	assert.Contains(t, got.Body, "The agent said:")
	assert.Contains(t, got.Body, "my push kept failing so I assumed the token expired")
}

func TestReconcileOne_EmptyWhyProducesNoAttributionBlock(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureCUR(func(c *spiceboxv1alpha1.CredentialUpdateRequest) { c.Spec.Why = "" })
	_, w, pub := newCredentialUpdateWatcher(t, sess, cur)

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	got := pub.findInteractionRequest(t)
	assert.Empty(t, got.Body, "an empty why must not emit a dangling \"The agent said:\" block")
}

func TestReconcileOne_FieldsNameIdentityCredentialAndTool(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureCUR()
	_, w, pub := newCredentialUpdateWatcher(t, sess, cur)

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	got := pub.findInteractionRequest(t)
	var joined strings.Builder
	for _, f := range got.Fields {
		joined.WriteString(f.Label + ": " + f.Value + "\n")
	}
	assert.Contains(t, joined.String(), "SessionUserIdentity")
	assert.Contains(t, joined.String(), curCredName)
	assert.Contains(t, joined.String(), "github__create_issue")
}

func TestReconcileOne_ExactlyOneActionWithGivenLink(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureCUR()
	_, w, pub := newCredentialUpdateWatcher(t, sess, cur)

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	got := pub.findInteractionRequest(t)
	require.Len(t, got.Actions, 1)
	assert.NotEmpty(t, got.Actions[0].Label)
	assert.Equal(t, channelevents.ActionKindLink, got.Actions[0].Kind)
	assert.Contains(t, got.Actions[0].URL, "https://agent.example.invalid/link")
}

// --- Idempotency (C2, now living in channelsd) --------------------------

// TestReconcileOne_IdempotentOnRetryBeforePatchObserved pins C2: a retry
// against the SAME (still Status.InteractionRef=="") CR object -- exactly
// what reconcileAll would pass on the NEXT tick if the first tick's
// InteractionRef patch had not yet been observed -- must not publish a
// second card. Reverting the in-memory alreadyPublished guard makes this
// fail.
func TestReconcileOne_IdempotentOnRetryBeforePatchObserved(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureCUR()
	_, w, pub := newCredentialUpdateWatcher(t, sess, cur)

	require.NoError(t, w.ReconcileOne(context.Background(), cur))
	require.Len(t, pub.interactionRequests(t), 1)

	// Same in-memory cur (InteractionRef still "" in this copy, simulating a
	// caller that re-lists before observing its own successful patch).
	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	assert.Len(t, pub.interactionRequests(t), 1, "a retry must not publish a second card")
}

// TestReconcileOne_PersistedInteractionRefShortCircuits proves the OTHER half
// of the dedup: once InteractionRef is actually persisted (the normal case on
// the NEXT tick's fresh List), ReconcileOne must no-op immediately without
// even reaching the publish path.
func TestReconcileOne_PersistedInteractionRefShortCircuits(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureCUR()
	c, w, pub := newCredentialUpdateWatcher(t, sess, cur)

	require.NoError(t, w.ReconcileOne(context.Background(), cur))
	require.Len(t, pub.interactionRequests(t), 1)

	persisted := getCURFrom(t, c)
	require.NotEmpty(t, persisted.Status.InteractionRef, "sanity: the first call must have persisted InteractionRef")

	require.NoError(t, w.ReconcileOne(context.Background(), persisted))
	assert.Len(t, pub.interactionRequests(t), 1, "a CR with InteractionRef already set must never be re-published")
}

// TestReconcileOne_EvictsDedupEntryAfterSuccessfulPatch: the in-memory dedup
// map must not grow for the process's lifetime, so after a successful
// publish+patch the UID's entry must be gone.
func TestReconcileOne_EvictsDedupEntryAfterSuccessfulPatch(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureCUR()
	_, w, _ := newCredentialUpdateWatcher(t, sess, cur)

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	_, stillTracked := w.alreadyPublished(cur.UID)
	assert.False(t, stillTracked, "the dedup map entry must be evicted once InteractionRef is durably persisted")
}

// TestReconcileAll_ForgetsDurablyFailingEntryOnceCRIsDeleted: forgetPublished
// only evicts on a SUCCESSFUL InteractionRef patch, so a durably-failing patch
// leaves its dedup entry in place on purpose — evicting it would reopen the
// double-publish race the map exists to close. But that entry must not outlive
// the CR: once owner-ref GC deletes the CredentialUpdateRequest, forgetUnseen
// (called at the end of every reconcileAll) must prune it, or the map grows
// without bound for every request that ever hit this failure mode.
func TestReconcileAll_ForgetsDurablyFailingEntryOnceCRIsDeleted(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureCUR()
	c, w, _, _ := newFailablePatchWatcher(t, sess, cur)

	w.reconcileAll(context.Background(), discardLogger(t))
	_, tracked := w.alreadyPublished(cur.UID)
	require.True(t, tracked, "sanity: a durably-failing patch must leave the dedup entry in place")

	// The CR is deleted out from under the watcher (owner-ref GC with its
	// session) -- the next reconcileAll's List no longer sees it at all.
	require.NoError(t, c.Delete(context.Background(), cur))

	w.reconcileAll(context.Background(), discardLogger(t))
	_, stillTracked := w.alreadyPublished(cur.UID)
	assert.False(t, stillTracked, "forgetUnseen must prune a UID whose CR no longer appears in any List, or the map leaks forever")
}

// newFailablePatchWatcher builds a watcher whose status-subresource patches
// fail while *failPatch is true, so a test can drive "the card published but
// its delivery record did not land" and then let the write recover. Returns
// the client, the watcher, the publisher, and the flag to flip.
func newFailablePatchWatcher(t *testing.T, objs ...client.Object) (client.Client, *CredentialUpdateWatcher, *capturingPublisher, *bool) {
	t.Helper()
	failPatch := true
	c := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.CredentialUpdateRequest{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cli client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if failPatch {
					return fmt.Errorf("simulated status patch failure")
				}
				return cli.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	pub := &capturingPublisher{}
	w := &CredentialUpdateWatcher{
		K8s:             c,
		LinkSigner:      passthroughlink.New([]byte("test-signing-key-not-for-production")),
		ExternalBaseURL: func() string { return "https://agent.example.invalid" },
		NATSPublish:     pub.publish,
	}
	return c, w, pub, &failPatch
}

// TestReconcileAll_RetriesFailedDeliveryRecordWithoutRepublishing: publishing
// and RECORDING the publish have opposite retry semantics, and conflating them
// inverts the very falsehood CardDelivered exists to prevent.
//
// The failure this locks out: the card publishes, the status write that stamps
// InteractionRef/CardDelivered fails durably, and the in-memory dedup guard --
// whose only job is to stop a SECOND card -- also short-circuits every later
// pass, so nothing ever retries the write. InteractionRef stays empty forever,
// and at expiry the operator tells the user (and the tool tells the agent)
// that no card was ever shown -- about a card a human is looking at.
//
// Both halves are asserted together on purpose: a "fix" that retried by
// re-running the whole publish path would satisfy the record assertion while
// double-carding the human, so the exactly-one-publish assertion is what keeps
// the retry honest.
func TestReconcileAll_RetriesFailedDeliveryRecordWithoutRepublishing(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureCUR()
	c, w, pub, failPatch := newFailablePatchWatcher(t, sess, cur)

	// Pass 1: the card goes out, the delivery record does not.
	w.reconcileAll(context.Background(), discardLogger(t))
	published := pub.interactionRequests(t)
	require.Len(t, published, 1, "sanity: the card must have been published on the first pass")
	unrecorded := getCURFrom(t, c)
	require.Empty(t, unrecorded.Status.InteractionRef, "sanity: the failing write must have left delivery unrecorded")
	require.Nil(t, findCondition(unrecorded.Status.Conditions, spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered),
		"sanity: CardDelivered must be unstamped while the write is failing")

	// Pass 2, still failing: no second card, and still nothing recorded.
	w.reconcileAll(context.Background(), discardLogger(t))
	require.Len(t, pub.interactionRequests(t), 1, "a still-failing record must not trigger a second card")

	// The write recovers (a transient apiserver failure clears).
	*failPatch = false
	w.reconcileAll(context.Background(), discardLogger(t))

	got := getCURFrom(t, c)
	assert.Equal(t, published[0].RequestRef, got.Status.InteractionRef,
		"a later pass must retry the failed record -- and record the ref of the card that ACTUALLY went out, not a fresh one")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered)
	require.NotNil(t, cond, "CardDelivered must be stamped by the retry, in the same write as InteractionRef")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonCredentialUpdateCardDelivered, cond.Reason)

	assert.Len(t, pub.interactionRequests(t), 1,
		"exactly ONE card across all three passes: the retry re-drives the status write alone, never the publish")

	_, stillTracked := w.alreadyPublished(cur.UID)
	assert.False(t, stillTracked, "the dedup entry is evicted once the record finally lands")
}

// TestReconcileAll_RetriesDeliveryRecordEvenAfterRequestWentTerminal covers the
// same retry one step later: the record kept failing until the operator
// expired the request. The CR is no longer Open, so a phase-gated dispatch
// would drop it -- and the CR would stay permanently, wrongly, marked as
// never-delivered. The record must still land, because "was a human ever
// shown this?" stays a true-or-false fact about the past regardless of what
// phase the request has since reached.
func TestReconcileAll_RetriesDeliveryRecordEvenAfterRequestWentTerminal(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureCUR()
	c, w, pub, failPatch := newFailablePatchWatcher(t, sess, cur)

	w.reconcileAll(context.Background(), discardLogger(t))
	require.Len(t, pub.interactionRequests(t), 1, "sanity: the card must have been published")

	// The operator gives up on the un-clicked card and expires it.
	*failPatch = false
	expired := getCURFrom(t, c)
	expired.Status.Phase = spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired
	require.NoError(t, c.Status().Update(context.Background(), expired))

	w.reconcileAll(context.Background(), discardLogger(t))

	got := getCURFrom(t, c)
	assert.NotEmpty(t, got.Status.InteractionRef,
		"delivery must be recorded even on a terminal request -- a card that was shown was shown")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Len(t, pub.interactionRequests(t), 1, "a terminal request must never get a second card")
}

// --- reconcileAll filtering ----------------------------------------------

func TestReconcileAll_SkipsNonOpenAndAlreadyPublished(t *testing.T) {
	sess := fixtureSession(t)
	openNoRef := fixtureCUR(func(c *spiceboxv1alpha1.CredentialUpdateRequest) {
		c.Name = "cur-open-no-ref"
	})
	refused := fixtureCUR(func(c *spiceboxv1alpha1.CredentialUpdateRequest) {
		c.Name = "cur-refused"
		c.Status.Phase = spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused
	})
	alreadyPublished := fixtureCUR(func(c *spiceboxv1alpha1.CredentialUpdateRequest) {
		c.Name = "cur-already-published"
		c.Status.InteractionRef = "existing-ref"
	})
	_, w, pub := newCredentialUpdateWatcher(t, sess, openNoRef, refused, alreadyPublished)

	w.reconcileAll(context.Background(), discardLogger(t))

	reqs := pub.interactionRequests(t)
	require.Len(t, reqs, 1, "only the Open, undelivered request may publish")
}

// --- Fail-closed on unconfigured external URL ----------------------------

func TestReconcileOne_ExternalURLUnconfiguredFailsClosed(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureCUR()
	_, w, pub := newCredentialUpdateWatcher(t, sess, cur)
	w.ExternalBaseURL = func() string { return "" }

	err := w.ReconcileOne(context.Background(), cur)
	require.Error(t, err, "an unconfigured external URL must fail closed (retried next tick), never emit a broken link")
	assert.Empty(t, pub.interactionRequests(t), "no card may be published without a real link")
	assert.True(t, pub.hasNonMonitoring(), "a best-effort notification should still reach the session")
}

// TestReconcileOne_ExternalURLUnconfiguredNotifiesOnlyOnFirstTransition:
// reconcileAll re-dispatches this SAME CR every tick for the whole park while
// it stays Open/undelivered, so without a dedup handleExternalURLUnconfigured
// would publish a fresh KindNotification on EVERY call -- roughly 360 times
// over a 30-minute park. The
// CardDelivered=False condition (stamped on the first call) must suppress
// every subsequent notification while the CR keeps failing the SAME way, and
// the CR's own condition must reflect the failure for an operator/dashboard
// to read even though this watcher has no monitoring-channel broadcast.
func TestReconcileOne_ExternalURLUnconfiguredNotifiesOnlyOnFirstTransition(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureCUR()
	c, w, pub := newCredentialUpdateWatcher(t, sess, cur)
	w.ExternalBaseURL = func() string { return "" }

	err := w.ReconcileOne(context.Background(), cur)
	require.Error(t, err, "first call must still fail closed")
	firstCount := len(pub.envelopesOfKind(channelevents.KindNotification))
	assert.Equal(t, 1, firstCount, "exactly one notification on the first transition")

	got := getCURFrom(t, c)
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered)
	require.NotNil(t, cond, "CardDelivered condition must be stamped on the fail-closed path")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.NotEmpty(t, cond.Reason)

	// Second call, same failure: no additional notification.
	err = w.ReconcileOne(context.Background(), got)
	require.Error(t, err, "still fails closed on the second tick")
	assert.Equal(t, firstCount, len(pub.envelopesOfKind(channelevents.KindNotification)),
		"a repeated identical failure must not re-notify (New-Important-2)")
}

func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

// TestReconcileOne_UserOwnedNoRecipientIsLoud: a no-recipient condition that
// only logs at Info and returns nil publishes nothing, records nothing, and
// retries nothing -- the agent burns its full ask timeout (>=10m) before
// anyone learns nobody was ever asked.
//
// Every row must produce the SAME three surfaces the agent-owned route does:
// CardDelivered=False/NoRecipient (so the operator's Expired branch keeps
// saying "never delivered"), a session notification, and a returned error so
// the next tick retries. InteractionRef must stay empty throughout.
func TestReconcileOne_UserOwnedNoRecipientIsLoud(t *testing.T) {
	cases := []struct {
		name       string
		sess       func(t *testing.T) *spiceboxv1alpha1.AgentSession
		cur        func() *spiceboxv1alpha1.CredentialUpdateRequest
		wantDetail string
	}{
		{
			name: "no resolved credential: CardDelivered=False, notified, error returned",
			sess: func(t *testing.T) *spiceboxv1alpha1.AgentSession { return fixtureSession(t) },
			cur: func() *spiceboxv1alpha1.CredentialUpdateRequest {
				return fixtureCUR(func(c *spiceboxv1alpha1.CredentialUpdateRequest) { c.Status.ResolvedCredential = nil })
			},
			wantDetail: "no credential was resolved",
		},
		{
			name: "session not channel-attached: CardDelivered=False, notified, error returned",
			sess: func(t *testing.T) *spiceboxv1alpha1.AgentSession {
				return fixtureSession(t, func(s *spiceboxv1alpha1.AgentSession) { s.Spec.InputChannel = nil })
			},
			cur:        func() *spiceboxv1alpha1.CredentialUpdateRequest { return fixtureCUR() },
			wantDetail: "not attached to a channel",
		},
		{
			name: "session records no started-by identity: CardDelivered=False, notified, error returned",
			sess: func(t *testing.T) *spiceboxv1alpha1.AgentSession {
				return fixtureSession(t, func(s *spiceboxv1alpha1.AgentSession) {
					delete(s.Annotations, spiceboxv1alpha1.AnnotationStartedByCanonicalID)
				})
			},
			cur:        func() *spiceboxv1alpha1.CredentialUpdateRequest { return fixtureCUR() },
			wantDetail: "no started-by identity",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cur := tc.cur()
			c, w, pub := newCredentialUpdateWatcher(t, tc.sess(t), cur)

			err := w.ReconcileOne(context.Background(), cur)
			require.Error(t, err, "a card nobody can be shown must fail loudly, not skip silently")
			assert.Contains(t, err.Error(), tc.wantDetail,
				"the error must name THIS cause, not some other refusal (got %q)", err)

			assert.Empty(t, pub.interactionRequests(t), "nothing may be published when there is no recipient")
			assert.Len(t, pub.envelopesOfKind(channelevents.KindNotification), 1,
				"the humans in the thread must learn the agent is blocked")

			got := getCURFrom(t, c)
			assert.Empty(t, got.Status.InteractionRef,
				"nothing was delivered, so InteractionRef must stay empty or the operator's Expired Reason would lie")
			cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered)
			require.NotNil(t, cond, "the no-recipient state must be visible on the CR")
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, spiceboxv1alpha1.ReasonCredentialUpdateNoRecipient, cond.Reason)
			assert.Contains(t, cond.Message, tc.wantDetail, "the condition must name the actual cause")

			// Same dedup rule the agent-owned route uses: the CR is
			// re-dispatched every 5s for the whole park, so a second identical
			// failure must not re-notify.
			require.Error(t, w.ReconcileOne(context.Background(), got), "still fails closed on the second tick")
			assert.Len(t, pub.envelopesOfKind(channelevents.KindNotification), 1,
				"a repeated identical failure must not re-notify")
		})
	}
}

// TestUserOwnedNoRecipientNotificationStaysUserFacing pins the copy rule on
// the notification the rows above assert the existence of: it goes to whoever
// is in the thread, so it must read as an explanation, never as an operator
// runbook. The cause detail belongs on the CR condition, which the rows above
// check separately.
func TestUserOwnedNoRecipientNotificationStaysUserFacing(t *testing.T) {
	sess := fixtureSession(t, func(s *spiceboxv1alpha1.AgentSession) { s.Spec.InputChannel = nil })
	cur := fixtureCUR()
	_, w, pub := newCredentialUpdateWatcher(t, sess, cur)

	require.Error(t, w.ReconcileOne(context.Background(), cur))

	notes := pub.envelopesOfKind(channelevents.KindNotification)
	require.Len(t, notes, 1)
	var got channelevents.NotificationPayload
	require.NoError(t, json.Unmarshal(notes[0].Payload, &got))

	assert.NotEmpty(t, got.Text)
	assert.NotEmpty(t, got.Short)
	for _, banned := range []string{"kubectl", "CredentialUpdateRequest", "AgentSession", "InputChannel", "status.", "annotation"} {
		assert.NotContains(t, got.Text, banned,
			"the thread-facing notice must not leak platform internals")
	}
}
