package useridentity

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	platformuseridentity "github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
)

// The canonical subjects of two different people who both link a credential for
// the SAME GitHub account. Opaque base64-ish strings, matching how the platform
// actually spells a canonical user id.
const (
	aliceSubject = "user:YWxpY2VAZXhhbXBsZS5jb20="
	bobSubject   = "user:Ym9iQGV4YW1wbGUuY29t"
	attestedID   = "583231"

	// carolSubject and otherAttestedID name a THIRD person claiming a
	// DIFFERENT account — used where a test needs an unrelated identity to
	// prove a match check actually discriminates, rather than merely having
	// nothing else to compare against.
	carolSubject    = "user:Y2Fyb2xAZXhhbXBsZS5jb20="
	otherAttestedID = "999999"
)

type recordedEdge struct {
	objType   string
	subjectID string
	canonical string
}

// fakeAttestedWriter stands in for *spicedb.Client. It answers lookups out of
// what it has been told to hold PLUS everything written through it, so a second
// catalog claiming one account sees the first catalog's edge exactly as it
// would against a real datastore.
//
// sole tracks the #sole_user relation the same way bound tracks #user: keyed
// by "<objType>:<subjectID>", present only while some canonical id currently
// holds it. Unlike #user, at most one entry can ever be live per key — a
// second TouchSoleIdentity is never issued without an intervening
// DeleteSoleIdentity — so a single string value (not a slice) is enough.
type fakeAttestedWriter struct {
	writes      []recordedEdge
	bound       map[string][]string // "<objType>:<subjectID>" -> canonical user ids (#user)
	soleWrites  []recordedEdge      // every TouchSoleIdentity call, in order
	soleDeletes []string            // every DeleteSoleIdentity call's "<objType>:<subjectID>", in order
	sole        map[string]string   // "<objType>:<subjectID>" -> the canonical id currently holding #sole_user
	lookupErr   error
	// soleLookupErr gates ONLY LookupSoleIdentitySubjects, independent of
	// lookupErr (which gates LookupAttestedIdentitySubjects). A real SpiceDB
	// read-path degradation would fail both relations' reads alike, but a
	// test proving the SOLE lookup's own fail-closed behavior needs to fail
	// it without also tripping the #user lookup that runs earlier in the
	// same reconcile and would otherwise return before ever reaching it.
	soleLookupErr error
	writeErr      error
}

func newFakeWriter() *fakeAttestedWriter {
	return &fakeAttestedWriter{bound: map[string][]string{}, sole: map[string]string{}}
}

func (f *fakeAttestedWriter) LookupAttestedIdentitySubjects(_ context.Context, objType, subjectID string) ([]string, error) {
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	return f.bound[objType+":"+subjectID], nil
}

func (f *fakeAttestedWriter) TouchAttestedIdentity(_ context.Context, objType, subjectID string, canonicalID identity.CanonicalUserID) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes = append(f.writes, recordedEdge{objType: objType, subjectID: subjectID, canonical: canonicalID.String()})
	key := objType + ":" + subjectID
	f.bound[key] = append(f.bound[key], canonicalID.String())
	return nil
}

func (f *fakeAttestedWriter) TouchSoleIdentity(_ context.Context, objType, subjectID string, canonicalID identity.CanonicalUserID) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.soleWrites = append(f.soleWrites, recordedEdge{objType: objType, subjectID: subjectID, canonical: canonicalID.String()})
	f.sole[objType+":"+subjectID] = canonicalID.String()
	return nil
}

// LookupSoleIdentitySubjects answers out of the SAME f.sole map
// TouchSoleIdentity/DeleteSoleIdentity maintain — a real client's lookup
// reads back exactly what its own touch/delete wrote, and the fake mirrors
// that rather than keeping a separate, driftable record.
func (f *fakeAttestedWriter) LookupSoleIdentitySubjects(_ context.Context, objType, subjectID string) ([]string, error) {
	if f.soleLookupErr != nil {
		return nil, f.soleLookupErr
	}
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	if cid, ok := f.sole[objType+":"+subjectID]; ok {
		return []string{cid}, nil
	}
	return nil, nil
}

// LookupSoleIdentityAccounts answers out of the same f.sole map, from the
// resource side: which accounts of objType does this canonical id hold
// #sole_user on. Same fidelity argument as LookupSoleIdentitySubjects — a
// real client reads back its own writes.
func (f *fakeAttestedWriter) LookupSoleIdentityAccounts(_ context.Context, objType string, canonicalID identity.CanonicalUserID) ([]string, error) {
	if f.soleLookupErr != nil {
		return nil, f.soleLookupErr
	}
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	var out []string
	for key, holder := range f.sole {
		if holder != canonicalID.String() {
			continue
		}
		ot, id, found := strings.Cut(key, ":")
		if !found || ot != objType {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out) // map order is not a contract; a test asserting call order needs one
	return out, nil
}

func (f *fakeAttestedWriter) DeleteSoleIdentity(_ context.Context, objType, subjectID string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	key := objType + ":" + subjectID
	f.soleDeletes = append(f.soleDeletes, key)
	delete(f.sole, key)
	return nil
}

// capturePublish returns a PublishFunc that decodes every monitoring event into
// the slice it also returns. err, when non-nil, is returned to the caller after
// the event is recorded, so a test can assert both that the notice was
// attempted and that its failure changed nothing.
func capturePublish(t *testing.T, err error) (channelevents.PublishFunc, *[]channelevents.MonitoringEvent) {
	t.Helper()
	var got []channelevents.MonitoringEvent
	return func(_ string, data []byte) error {
		var ev channelevents.MonitoringEvent
		require.NoError(t, json.Unmarshal(data, &ev), "monitoring event must decode")
		got = append(got, ev)
		return err
	}, &got
}

// attestedSecret builds a credential Secret carrying the link-time attestation
// Task 2 records, plus any extra annotations the case needs.
func attestedSecret(t *testing.T, name, providerID, subjectID string, extra map[string]string) *corev1.Secret {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		Data:       map[string][]byte{"token": []byte("ghp_abc")},
	}
	adoptguard.WithAdoptedLabel(sec)
	platformuseridentity.SetAttestation(sec, providerID, subjectID)
	for k, v := range extra {
		sec.Annotations[k] = v
	}
	return sec
}

// userIdentityFor builds a catalog naming subject, with one static credential
// backed by secretName.
func userIdentityFor(name, subject, secretName string) *spiceboxv1alpha1.UserIdentity {
	return &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject: subject,
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "gh", Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secretName, Key: "token"},
				},
			}},
		},
	}
}

func newEdgeReconciler(c client.Client, w AttestedIdentityWriter, publish channelevents.PublishFunc) *Reconciler {
	r := newReconciler(c)
	r.SpiceDB = w
	r.MonitoringPublish = publish
	return r
}

func canonicalOf(t *testing.T, subject string) string {
	t.Helper()
	canon, err := identity.Subject(subject).CanonicalUserID()
	require.NoError(t, err)
	return canon.String()
}

// The edge binds the catalog's DECLARED owner, never whoever pasted the token.
func TestReconcile_WritesTheAttestedEdgeForAUserIdentity(t *testing.T) {
	sec := attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil)
	c := buildClient(t, sec, userIdentityFor("u-alice", aliceSubject, "u-alice-gh"))
	w := newFakeWriter()
	publish, published := capturePublish(t, nil)

	r := newEdgeReconciler(c, w, publish)
	reconcileOnce(t, r, "u-alice")

	require.Len(t, w.writes, 1, "exactly one edge for one attested credential")
	assert.Equal(t, "github_user", w.writes[0].objType, "the numeric-id-keyed GitHub identity type")
	assert.Equal(t, attestedID, w.writes[0].subjectID, "keyed on the provider's stable numeric id")
	assert.Equal(t, canonicalOf(t, aliceSubject), w.writes[0].canonical,
		"bound to spec.subject — the record that declares whose credential this is")
	assert.Empty(t, *published, "no conflict, so nothing to report")
}

// An AgentIdentity names no human, so a shared bot token must mint nothing.
// Asserted by absence: zero relationship writes.
func TestReconcile_WritesNoEdgeForAnAgentIdentityCredential(t *testing.T) {
	sec := attestedSecret(t, "shared-bot-gh", "github-pat", attestedID, map[string]string{
		adoptkit.OwnerAnnotationKey("AgentIdentity", types.NamespacedName{Namespace: "agents", Name: "release-bot"}): "agents/release-bot",
	})
	c := buildClient(t, sec, userIdentityFor("u-alice", aliceSubject, "shared-bot-gh"))
	w := newFakeWriter()
	publish, published := capturePublish(t, nil)

	r := newEdgeReconciler(c, w, publish)
	reconcileOnce(t, r, "u-alice")

	assert.Empty(t, w.writes, "a credential an AgentIdentity also holds names no single human; it mints nothing")
	assert.Empty(t, *published, "nothing was claimed, so there is nothing to report")
}

// A second claim on one GitHub account WRITES ITS EDGE and reports. Refusing
// would block the case where both people are present while doing nothing about
// the case where one is not — theft leaves no collision to detect.
func TestReconcile_ConflictWritesTheEdgeAndReports(t *testing.T) {
	c := buildClient(t,
		attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil),
		attestedSecret(t, "u-bob-gh", "github-pat", attestedID, nil),
		userIdentityFor("u-alice", aliceSubject, "u-alice-gh"),
		userIdentityFor("u-bob", bobSubject, "u-bob-gh"),
	)
	w := newFakeWriter()
	publish, published := capturePublish(t, nil)

	r := newEdgeReconciler(c, w, publish)
	reconcileOnce(t, r, "u-alice")
	reconcileOnce(t, r, "u-bob")

	// require, not assert: the next line indexes writes[1], so a refused second
	// edge must abort here rather than panic over the assertion that named it.
	require.Len(t, w.writes, 2, "both edges are written; a conflict is reported, never refused")
	assert.Equal(t, canonicalOf(t, bobSubject), w.writes[1].canonical, "the second claim is the one that landed")

	require.Len(t, *published, 1, "reported once, by the claim that collided")
	ev := (*published)[0]
	assert.Equal(t, "credential", ev.Category)
	assert.Equal(t, channelevents.MonitoringLevelWarning, ev.Level)
	assert.Equal(t, channelevents.MonitoringTransitionFailed, ev.Transition)
	assert.Equal(t, "UserIdentity", ev.Source.Kind)
	assert.Equal(t, "u-bob", ev.Source.Name)
	assert.Contains(t, ev.Summary, attestedID, "the notice names the contested account")
	assert.Contains(t, ev.Summary, aliceSubject, "the notice names the subject already bound")
	assert.Contains(t, ev.Summary, bobSubject, "the notice names the subject now claiming it")
}

// The notice is best effort; the edge is the durable half.
func TestReconcile_MonitoringPublishFailureDoesNotLoseTheEdge(t *testing.T) {
	c := buildClient(t,
		attestedSecret(t, "u-bob-gh", "github-pat", attestedID, nil),
		userIdentityFor("u-bob", bobSubject, "u-bob-gh"),
	)
	w := newFakeWriter()
	// Alice's edge already exists in the datastore, so Bob's claim collides.
	w.bound["github_user:"+attestedID] = []string{canonicalOf(t, aliceSubject)}
	publish, published := capturePublish(t, errors.New("nats down"))

	r := newEdgeReconciler(c, w, publish)
	_, err := r.Reconcile(context.Background(), reconcileRequestFor("u-bob"))

	require.NoError(t, err, "a publish failure does not fail the reconcile")
	assert.Len(t, w.writes, 1, "the edge is written even when the notice fails")
	assert.Len(t, *published, 1, "the notice was attempted")
}

// A credential with no attestation is skipped silently — most credentials are
// not forge credentials.
func TestReconcile_NoAttestationWritesNothing(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "u-alice-gh", Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		Data:       map[string][]byte{"token": []byte("ghp_abc")},
	}
	adoptguard.WithAdoptedLabel(sec)
	c := buildClient(t, sec, userIdentityFor("u-alice", aliceSubject, "u-alice-gh"))
	w := newFakeWriter()
	publish, published := capturePublish(t, nil)

	r := newEdgeReconciler(c, w, publish)
	reconcileOnce(t, r, "u-alice")

	assert.Empty(t, w.writes)
	assert.Empty(t, *published)
}

// A half-written attestation is not a claim about an unknown provider, and a
// provider with no registered SpiceDB object type is not guessed at.
func TestReconcile_PartialOrUnmappedAttestationWritesNothing(t *testing.T) {
	cases := []struct {
		name string
		sec  func(t *testing.T) *corev1.Secret
	}{
		{
			name: "provider annotation only: absent, not a claim about an unknown subject",
			sec: func(t *testing.T) *corev1.Secret {
				s := attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil)
				delete(s.Annotations, platformuseridentity.AttestedSubjectAnnotation)
				return s
			},
		},
		{
			name: "subject annotation only: absent, not a claim about an unknown provider",
			sec: func(t *testing.T) *corev1.Secret {
				s := attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil)
				delete(s.Annotations, platformuseridentity.AttestedProviderAnnotation)
				return s
			},
		},
		{
			name: "provider with no registered object type: nothing written rather than a guessed type",
			sec: func(t *testing.T) *corev1.Secret {
				return attestedSecret(t, "u-alice-gh", "linear-oauth", attestedID, nil)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := buildClient(t, tc.sec(t), userIdentityFor("u-alice", aliceSubject, "u-alice-gh"))
			w := newFakeWriter()
			publish, published := capturePublish(t, nil)

			r := newEdgeReconciler(c, w, publish)
			reconcileOnce(t, r, "u-alice")

			assert.Empty(t, w.writes)
			assert.Empty(t, *published)
		})
	}
}

// The edge is identity, and the subject it binds must be a user. A catalog
// whose spec.subject is not a user subject mints nothing rather than writing a
// tuple whose subject type the schema does not permit.
func TestReconcile_NonUserSubjectWritesNothing(t *testing.T) {
	sec := attestedSecret(t, "u-grp-gh", "github-pat", attestedID, nil)
	c := buildClient(t, sec, userIdentityFor("u-grp", "group:engineering", "u-grp-gh"))
	w := newFakeWriter()
	publish, published := capturePublish(t, nil)

	r := newEdgeReconciler(c, w, publish)
	reconcileOnce(t, r, "u-grp")

	assert.Empty(t, w.writes)
	assert.Empty(t, *published)
}

// A SpiceDB write failure is surfaced to controller-runtime so the edge is
// retried, rather than being lost with a green reconcile.
func TestReconcile_EdgeWriteFailureRequeues(t *testing.T) {
	c := buildClient(t,
		attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil),
		userIdentityFor("u-alice", aliceSubject, "u-alice-gh"),
	)
	w := newFakeWriter()
	w.writeErr = errors.New("spicedb unavailable")
	publish, _ := capturePublish(t, nil)

	r := newEdgeReconciler(c, w, publish)
	_, err := r.Reconcile(context.Background(), reconcileRequestFor("u-alice"))

	require.Error(t, err, "a lost edge must requeue, not pass silently")
	assert.Contains(t, err.Error(), "spicedb unavailable")

	// The status still converged: a SpiceDB outage does not hold the CR invalid.
	var got spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "u-alice"}, &got))
	assert.Equal(t, []string{"gh"}, got.Status.AvailableCredentials)
}

// Nil SpiceDB is the disconnected/test wiring: a logged no-op, never a panic
// and never a failed reconcile.
func TestReconcile_NilSpiceDBDegradesToNoOp(t *testing.T) {
	c := buildClient(t,
		attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil),
		userIdentityFor("u-alice", aliceSubject, "u-alice-gh"),
	)
	r := newReconciler(c) // no SpiceDB, no MonitoringPublish
	reconcileOnce(t, r, "u-alice")
}

// Nil MonitoringPublish silences the NOTICE, not the edge.
func TestReconcile_NilMonitoringPublishStillWritesTheEdge(t *testing.T) {
	c := buildClient(t,
		attestedSecret(t, "u-bob-gh", "github-pat", attestedID, nil),
		userIdentityFor("u-bob", bobSubject, "u-bob-gh"),
	)
	w := newFakeWriter()
	w.bound["github_user:"+attestedID] = []string{canonicalOf(t, aliceSubject)}

	r := newEdgeReconciler(c, w, nil)
	reconcileOnce(t, r, "u-bob")

	assert.Len(t, w.writes, 1, "the edge is the durable half; an unconfigured monitoring channel cannot suppress it")
}

// Re-reconciling an already-bound account is a no-op: no duplicate write, and
// no repeat of a conflict notice a human has already been handed.
func TestReconcile_AlreadyBoundIsQuiet(t *testing.T) {
	c := buildClient(t,
		attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil),
		userIdentityFor("u-alice", aliceSubject, "u-alice-gh"),
	)
	w := newFakeWriter()
	publish, published := capturePublish(t, nil)

	r := newEdgeReconciler(c, w, publish)
	reconcileOnce(t, r, "u-alice")
	reconcileOnce(t, r, "u-alice")

	assert.Len(t, w.writes, 1, "the second reconcile re-observes its own edge and writes nothing")
	assert.Empty(t, *published)
}

// reconcileRequestFor is the cluster-scoped request shape for a UserIdentity.
// Cases that must inspect the returned error call Reconcile directly rather
// than through reconcileOnce, which requires success.
func reconcileRequestFor(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: client.ObjectKey{Name: name}}
}

// TestReconcile_ALinkedCredentialMintsTheEdge closes the join the rest of this
// file's fixtures cannot: it lets the LINK PATH create the credential and then
// reconciles what that path actually produced.
//
// Every other case here hand-stamps the attestation onto its fixture Secret,
// which proves the reconciler reads an annotation but proves nothing about
// anything writing one. A test that manufactures its own precondition cannot
// notice that production never produces it — which is exactly how a pipeline
// that mints zero edges on a real cluster passed a full suite.
//
// platformuseridentity.PutToken is the single function every human-facing link
// route lands in (the `oap user-identity put-token` CLI, identityd's
// /link/submit, the account portal), so driving it here covers all three.
func TestReconcile_ALinkedCredentialMintsTheEdge(t *testing.T) {
	ctx := context.Background()
	c := buildClient(t)

	require.NoError(t, platformuseridentity.PutToken(ctx, c, platformuseridentity.PutTokenRequest{
		Subject:        identity.Subject(aliceSubject),
		CredentialName: "gh",
		Token:          "ghp_abc",
		// What the link's live verification observed, threaded through
		// unchanged — no test-only annotation stamping anywhere.
		ProviderID: "github-pat",
		SubjectID:  attestedID,
	}), "the link path must store the credential")

	uiName := platformuseridentity.NameForSubject(identity.Subject(aliceSubject))
	w := newFakeWriter()
	r := newEdgeReconciler(c, w, nil)
	reconcileOnce(t, r, uiName)

	require.Len(t, w.writes, 1, "linking a verified forge credential must mint exactly one edge")
	assert.Equal(t, recordedEdge{
		objType:   "github_user",
		subjectID: attestedID,
		canonical: canonicalOf(t, aliceSubject),
	}, w.writes[0], "the edge binds the provider account to the catalog's declared owner")
}

// TestReconcile_AnUnverifiedLinkMintsNothing is the other half of the join: a
// credential stored WITHOUT a verification verdict — the visitor pressed
// "store anyway", or --skip-verify was passed — establishes no provider
// account, so there is no identity to bind and nothing may be invented.
func TestReconcile_AnUnverifiedLinkMintsNothing(t *testing.T) {
	ctx := context.Background()
	c := buildClient(t)

	require.NoError(t, platformuseridentity.PutToken(ctx, c, platformuseridentity.PutTokenRequest{
		Subject:        identity.Subject(aliceSubject),
		CredentialName: "gh",
		Token:          "ghp_abc",
	}), "the link path must store the credential even with nothing attested")

	uiName := platformuseridentity.NameForSubject(identity.Subject(aliceSubject))
	w := newFakeWriter()
	r := newEdgeReconciler(c, w, nil)
	reconcileOnce(t, r, uiName)

	assert.Empty(t, w.writes, "an unattested credential names no account")
}

// agentIdentityHolding builds an AgentIdentity in the identities namespace
// whose named credential is backed by secretName — an agent that REFERENCES
// the Secret without having adopted it yet.
func agentIdentityHolding(name, secretName string) *spiceboxv1alpha1.AgentIdentity {
	return &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "gh", Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secretName, Key: "token"},
				},
			}},
		},
	}
}

// TestReconcile_WritesNoEdgeForACredentialAnAgentOnlyReferences covers the
// ordering the adoption annotation alone cannot: the AgentIdentity exists and
// names this Secret, but its own reconciler has not run, so nothing has
// stamped an owner annotation yet.
//
// The window matters because the edge is irreversible. Reconciling a shared
// bot credential once, before adoption lands, would permanently bind that
// account to whoever linked it.
func TestReconcile_WritesNoEdgeForACredentialAnAgentOnlyReferences(t *testing.T) {
	// No AgentIdentity owner annotation anywhere — only the reference.
	sec := attestedSecret(t, "shared-bot-gh", "github-pat", attestedID, nil)
	c := buildClient(t, sec,
		userIdentityFor("u-alice", aliceSubject, "shared-bot-gh"),
		agentIdentityHolding("release-bot", "shared-bot-gh"),
	)
	w := newFakeWriter()
	publish, published := capturePublish(t, nil)

	r := newEdgeReconciler(c, w, publish)
	reconcileOnce(t, r, "u-alice")

	assert.Empty(t, w.writes, "an agent already names this Secret; adoption having not landed yet does not make it unshared")
	assert.Empty(t, *published, "nothing was claimed, so there is nothing to report")
}

// An AgentIdentity that names a DIFFERENT Secret must not suppress the edge:
// the guard has to be a reference check, not the mere presence of an agent in
// the namespace.
func TestReconcile_AnUnrelatedAgentIdentityDoesNotSuppressTheEdge(t *testing.T) {
	sec := attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil)
	c := buildClient(t, sec,
		userIdentityFor("u-alice", aliceSubject, "u-alice-gh"),
		agentIdentityHolding("release-bot", "some-other-secret"),
	)
	w := newFakeWriter()

	r := newEdgeReconciler(c, w, nil)
	reconcileOnce(t, r, "u-alice")

	require.Len(t, w.writes, 1, "an agent holding an unrelated credential says nothing about this one")
	assert.Equal(t, attestedID, w.writes[0].subjectID)
}

// A list that cannot be answered must NOT mint. The alternative is writing an
// irreversible identity edge on the strength of an unanswered question, and
// the question is exactly "is this a shared bot credential?".
func TestReconcile_AnUnreadableAgentIdentityListFailsClosed(t *testing.T) {
	sec := attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil)
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(sec, userIdentityFor("u-alice", aliceSubject, "u-alice-gh")).
		WithStatusSubresource(&spiceboxv1alpha1.UserIdentity{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*spiceboxv1alpha1.AgentIdentityList); ok {
					return errors.New("apiserver unavailable")
				}
				return cl.List(ctx, list, opts...)
			},
		}).
		Build()
	w := newFakeWriter()

	r := newEdgeReconciler(c, w, nil)
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "u-alice"}})
	require.Error(t, err, "an unanswerable sharing check must requeue, not mint")
	assert.Contains(t, err.Error(), "list AgentIdentities")
	assert.Empty(t, w.writes, "nothing may be written while the question is open")
}

// TestReconcile_AFailedRevokeAndAFailedEdgeAreBothReported — a pass where both
// the revoke publish and the edge write fail must surface BOTH.
//
// The edge write became a second error source on a return that previously
// carried only the status failure, and returning it alone dropped the revoke
// error with nothing logged: the revoke would still retry, but an operator
// reading logs would have no record that it had failed at all.
func TestReconcile_AFailedRevokeAndAFailedEdgeAreBothReported(t *testing.T) {
	ctx := context.Background()
	extraSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "u-alice-extra", Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		Data:       map[string][]byte{"token": []byte("tok")},
	}
	adoptguard.WithAdoptedLabel(extraSecret)
	ui := userIdentityFor("u-alice", aliceSubject, "u-alice-gh")
	ui.Spec.Credentials = append(ui.Spec.Credentials, spiceboxv1alpha1.AgentCredential{
		Name: "extra", Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "u-alice-extra", Key: "token"},
		},
	})
	c := buildClient(t, attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil), extraSecret, ui)

	rp, bus := newCapturingPublisher(t)
	r := newEdgeReconciler(c, newFakeWriter(), nil)
	r.RevokePublisher = rp
	// First pass primes the revoke observation (a first sighting never emits).
	reconcileOnce(t, r, "u-alice")

	// Drop a credential so the next pass owes a revoke, then break both the
	// bus and SpiceDB. A fresh writer is used so the edge is attempted again
	// rather than short-circuiting on the binding the first pass made.
	var live spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: "u-alice"}, &live))
	live.Spec.Credentials = live.Spec.Credentials[:1]
	require.NoError(t, c.Update(ctx, &live))
	bus.setFailure(errors.New("nats down"))
	w := newFakeWriter()
	w.writeErr = errors.New("spicedb unavailable")
	r.SpiceDB = w

	_, err := r.Reconcile(ctx, reconcileRequestFor("u-alice"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spicedb unavailable", "the edge failure must be reported")
	assert.Contains(t, err.Error(), "nats down", "the revoke failure must not be dropped by the edge failure")
}

// TestReconcile_ConflictIsNotRepublishedWhileTheWriteKeepsFailing — the notice
// is published once the edge lands, so a write that keeps failing produces a
// retry, not a stream of duplicate warnings onto an already-degraded cluster.
func TestReconcile_ConflictIsNotRepublishedWhileTheWriteKeepsFailing(t *testing.T) {
	ctx := context.Background()
	c := buildClient(t,
		attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil),
		attestedSecret(t, "u-bob-gh", "github-pat", attestedID, nil),
		userIdentityFor("u-alice", aliceSubject, "u-alice-gh"),
		userIdentityFor("u-bob", bobSubject, "u-bob-gh"),
	)
	w := newFakeWriter()
	publish, published := capturePublish(t, nil)
	r := newEdgeReconciler(c, w, publish)

	// Alice binds the account first.
	reconcileOnce(t, r, "u-alice")
	require.Empty(t, *published, "one claim is not a conflict")

	// Bob's claim collides, but his edge write keeps failing.
	w.writeErr = errors.New("spicedb unavailable")
	for i := 0; i < 3; i++ {
		_, err := r.Reconcile(ctx, reconcileRequestFor("u-bob"))
		require.Error(t, err, "a lost edge must requeue")
	}
	assert.Empty(t, *published,
		"a conflict whose edge never landed must not re-notify on every requeue")

	// Once the write lands, the human hears about it — exactly once, because
	// the next pass finds Bob among the bound subjects.
	w.writeErr = nil
	reconcileOnce(t, r, "u-bob")
	reconcileOnce(t, r, "u-bob")
	require.Len(t, *published, 1, "one notice for one conflict, reported when the edge exists")
	assert.Contains(t, (*published)[0].Summary, attestedID)
}

// The first and only claimant gets #sole_user, which is what repo roles
// traverse — #user alone confers no durable authority.
func TestAttestedEdge_SoleClaimantGetsSoleUser(t *testing.T) {
	sec := attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil)
	c := buildClient(t, sec, userIdentityFor("u-alice", aliceSubject, "u-alice-gh"))
	w := newFakeWriter()

	r := newEdgeReconciler(c, w, nil)
	reconcileOnce(t, r, "u-alice")

	require.Len(t, w.writes, 1, "the #user edge is written, as always")
	require.Len(t, w.soleWrites, 1, "the lone claimant also gets #sole_user")
	assert.Equal(t, "github_user", w.soleWrites[0].objType)
	assert.Equal(t, attestedID, w.soleWrites[0].subjectID)
	assert.Equal(t, canonicalOf(t, aliceSubject), w.soleWrites[0].canonical)
	assert.Equal(t, canonicalOf(t, aliceSubject), w.sole["github_user:"+attestedID],
		"the relation itself now names alice")
	assert.Empty(t, w.soleDeletes, "nothing to withdraw when there is no conflict")
}

// A second claimant withdraws #sole_user from BOTH — fail closed, because
// nothing here can tell which claim is legitimate. #user is unchanged: both
// bindings still land (notice, not veto).
//
// Bob's catalog is created only AFTER alice's first reconcile (rather than
// alongside it) so this actually exercises "alice held it, then a second
// claimant withdraws it" as a causal sequence: #sole_user is derived from
// LIVE UserIdentity CRs (see liveAttestedClaimants), so if both existed from
// the start alice would never have held it in the first place — a real, but
// different, invariant this test does not need to prove.
func TestAttestedEdge_SecondClaimantWithdrawsSoleUser(t *testing.T) {
	ctx := context.Background()
	c := buildClient(t,
		attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil),
		userIdentityFor("u-alice", aliceSubject, "u-alice-gh"),
	)
	w := newFakeWriter()
	r := newEdgeReconciler(c, w, nil)

	reconcileOnce(t, r, "u-alice")
	key := "github_user:" + attestedID
	require.Contains(t, w.sole, key, "alice held it before bob showed up")

	// Bob links the same account afterward.
	require.NoError(t, c.Create(ctx, attestedSecret(t, "u-bob-gh", "github-pat", attestedID, nil)))
	require.NoError(t, c.Create(ctx, userIdentityFor("u-bob", bobSubject, "u-bob-gh")))
	reconcileOnce(t, r, "u-bob")

	require.Len(t, w.writes, 2, "both #user edges land — notice, not veto, unchanged")
	assert.NotContains(t, w.sole, key, "neither claimant keeps #sole_user once the account is contested")
	assert.NotEmpty(t, w.soleDeletes, "bob's own pass explicitly withdraws it")
}

// Pins the actual reason the withdraw branch's `len(others) == 0` guard is
// load-bearing: without it, a SECOND claimant's own not-yet-bound pass would
// fire TWO notices in the SAME reconcile — the withdraw branch's own
// (othersLive-keyed) trigger AND the standard, further-down (others-keyed)
// trigger — because when the FIRST claimant was already sole-and-stored,
// `others` is non-empty for the second claimant's pass and `priorSole` is
// also non-empty (the first claimant genuinely held it). Both conditions the
// withdraw-branch trigger checks (othersLive>0, priorSole>0) would be true
// with or without the `others==0` guard; only the guard itself prevents the
// duplicate.
func TestAttestedEdge_SoleAndStoredFirstClaimantDoesNotDoubleNotifyWhenASecondArrives(t *testing.T) {
	ctx := context.Background()
	c := buildClient(t,
		attestedSecret(t, "u-bob-gh", "github-pat", attestedID, nil),
		userIdentityFor("u-bob", bobSubject, "u-bob-gh"),
	)
	w := newFakeWriter()
	publish, published := capturePublish(t, nil)
	r := newEdgeReconciler(c, w, publish)

	// Bob is first, alone, and both holds sole_user AND has his #user tuple
	// stored — the "sole-and-stored" precondition.
	reconcileOnce(t, r, "u-bob")
	key := "github_user:" + attestedID
	require.Contains(t, w.sole, key)
	require.Empty(t, *published)

	// Alice appears afterward, claiming the same account.
	require.NoError(t, c.Create(ctx, attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil)))
	require.NoError(t, c.Create(ctx, userIdentityFor("u-alice", aliceSubject, "u-alice-gh")))

	reconcileOnce(t, r, "u-alice")

	assert.NotContains(t, w.sole, key, "withdrawn now that a second claimant exists")
	require.Len(t, *published, 1,
		"exactly one notice for one conflict — the standard, stored-tuple trigger already covers this (bob is stored), so the withdraw branch's own trigger must stay silent")
}

// Derived, level-triggered — and RECOVERABLE, which is the point: #sole_user
// is derived from LIVE UserIdentity CRs (liveAttestedClaimants), not from the
// stored #user tuples LookupAttestedIdentitySubjects reads (those are never
// deleted; see attested_edge.go's derivation). Deleting bob's catalog is
// therefore enough on its own — no direct manipulation of the fake writer's
// state — to drop him out of the live claimant count and restore alice on the
// very next pass, with no event replayed.
func TestAttestedEdge_SoleUserIsRestoredWhenTheConflictClears(t *testing.T) {
	ctx := context.Background()
	bobUI := userIdentityFor("u-bob", bobSubject, "u-bob-gh")
	c := buildClient(t,
		attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil),
		attestedSecret(t, "u-bob-gh", "github-pat", attestedID, nil),
		userIdentityFor("u-alice", aliceSubject, "u-alice-gh"),
		bobUI,
	)
	w := newFakeWriter()
	r := newEdgeReconciler(c, w, nil)

	reconcileOnce(t, r, "u-alice")
	reconcileOnce(t, r, "u-bob")
	key := "github_user:" + attestedID
	require.NotContains(t, w.sole, key, "withdrawn while both claim the account")

	// Bob's catalog goes away — the real "unlink" action a human takes — and
	// nothing else is touched.
	require.NoError(t, c.Delete(ctx, bobUI))

	reconcileOnce(t, r, "u-alice")

	require.Contains(t, w.sole, key, "restored: alice is once again the sole LIVE claimant")
	assert.Equal(t, canonicalOf(t, aliceSubject), w.sole[key])
}

// A silent revocation is the worst version of this: the conflict notice must
// say that repository access was withdrawn from both claimants, not just that
// a second claim exists.
func TestAttestedEdge_ConflictNoticeSaysRepoAccessWasWithdrawn(t *testing.T) {
	c := buildClient(t,
		attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil),
		attestedSecret(t, "u-bob-gh", "github-pat", attestedID, nil),
		userIdentityFor("u-alice", aliceSubject, "u-alice-gh"),
		userIdentityFor("u-bob", bobSubject, "u-bob-gh"),
	)
	w := newFakeWriter()
	publish, published := capturePublish(t, nil)

	r := newEdgeReconciler(c, w, publish)
	reconcileOnce(t, r, "u-alice")
	reconcileOnce(t, r, "u-bob")

	require.Len(t, *published, 1)
	ev := (*published)[0]
	assert.Contains(t, ev.Summary, "repository access", "the notice must name what was withdrawn")
	assert.Contains(t, ev.Summary, "withdrawn", "must say access was taken away, not merely that a claim exists")
	assert.Contains(t, ev.Hint, "unlink", "must say how to resolve it: unlink the account from all but one UserIdentity")
}

// The core of the silent-withdrawal bug: a second claimant whose OWN
// reconcile never runs (in production, a catalog Invalid for an unrelated
// reason never reaches reconcileAttestedEdges at all — see controller.go's
// setInvalid returns) still counts as a live claimant and still withdraws
// sole_user from the first. Without a dedicated trigger for this case, that
// withdrawal is completely silent: nobody's #user pass ever runs to report
// it via the stored-tuple notice, alice's own status stays Valid=True, and
// it is durable — bob never becomes valid, alice never gets it back.
//
// Bob's own Reconcile is deliberately never invoked in this test — that
// absence IS what stands in for "a catalog that never reaches
// reconcileAttestedEdges". His UserIdentity and Secret are still real,
// live cluster state, which is exactly what liveAttestedClaimants reads
// regardless of whether bob's own reconcile has ever run.
func TestAttestedEdge_InvisibleLiveConflictStillNotifies(t *testing.T) {
	ctx := context.Background()
	c := buildClient(t,
		attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil),
		userIdentityFor("u-alice", aliceSubject, "u-alice-gh"),
	)
	w := newFakeWriter()
	publish, published := capturePublish(t, nil)
	r := newEdgeReconciler(c, w, publish)

	// Alice is the sole claimant and holds sole_user.
	reconcileOnce(t, r, "u-alice")
	key := "github_user:" + attestedID
	require.Contains(t, w.sole, key)
	require.Empty(t, *published)

	// Bob's catalog appears, attesting to the SAME account. His own
	// Reconcile never runs here, standing in for a catalog Invalid for an
	// unrelated reason.
	require.NoError(t, c.Create(ctx, attestedSecret(t, "u-bob-gh", "github-pat", attestedID, nil)))
	require.NoError(t, c.Create(ctx, userIdentityFor("u-bob", bobSubject, "u-bob-gh")))

	// Alice's next pass is the only reconcile that ever runs for this
	// account from here on. It must withdraw sole_user AND report it — the
	// stored #user view never learns about bob (his own pass never writes
	// it), so the standard, stored-tuple notice never fires on its own.
	reconcileOnce(t, r, "u-alice")

	assert.NotContains(t, w.sole, key, "withdrawn even though bob's own reconcile never ran")
	require.Len(t, *published, 1, "a silent revocation is the one outcome this design forbids")
	ev := (*published)[0]
	assert.Contains(t, ev.Summary, bobSubject, "names the live claimant the stored view cannot see")

	// Level-triggered and reported exactly once: re-reconciling alice while
	// bob's invisible claim persists must not spam a fresh notice every pass.
	reconcileOnce(t, r, "u-alice")
	require.Len(t, *published, 1, "must not re-notify on every requeue while the conflict persists")
}

// Fail-closed on the WITHDRAWAL, not merely on the read: a degraded SpiceDB
// read path (LookupSoleIdentitySubjects failing while writes still succeed)
// must not let an already-granted sole_user survive for a now-contested
// account. The read failure costs the associated notice (an unknown prior
// state must not be reported as a real transition) and requeues the pass —
// it must never cost the withdrawal itself.
func TestAttestedEdge_SoleUserWithdrawalIsFailClosedWhenTheLookupFails(t *testing.T) {
	ctx := context.Background()
	c := buildClient(t,
		attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil),
		userIdentityFor("u-alice", aliceSubject, "u-alice-gh"),
	)
	w := newFakeWriter()
	publish, published := capturePublish(t, nil)
	r := newEdgeReconciler(c, w, publish)

	// Alice is the sole claimant and holds sole_user.
	reconcileOnce(t, r, "u-alice")
	key := "github_user:" + attestedID
	require.Contains(t, w.sole, key)

	// Bob appears — a second, live claimant — and the sole-identity LOOKUP
	// specifically is now degraded (the DELETE path still works; only the
	// read that would tell us whether this is a real transition fails).
	require.NoError(t, c.Create(ctx, attestedSecret(t, "u-bob-gh", "github-pat", attestedID, nil)))
	require.NoError(t, c.Create(ctx, userIdentityFor("u-bob", bobSubject, "u-bob-gh")))
	w.soleLookupErr = errors.New("spicedb read degraded")

	_, err := r.Reconcile(ctx, reconcileRequestFor("u-alice"))

	require.Error(t, err, "the lookup failure must requeue, not pass silently")
	assert.Contains(t, err.Error(), "spicedb read degraded")
	assert.NotContains(t, w.sole, key,
		"withdrawn even though the lookup that would have told us so failed — fail-closed on the WRITE, not fail-open because the READ degraded")
	assert.Contains(t, w.soleDeletes, key, "the delete must still be attempted despite the lookup failure")
	assert.Empty(t, *published, "an UNKNOWN prior state must not be read as something worth reporting")
}

// The core of the "recovery is level-triggered but nothing enqueues it" gap:
// this proves mapCoClaimants ITSELF enqueues the other co-claimant — not that
// a hand-driven Reconcile happens to converge, which hides the very absence
// of a trigger the gap was about. Without this mapper wired into
// SetupWithManager's Watches, bob's own event (a link, an unlink) never
// reaches alice at all, and her sole_user only catches up at the manager's
// (unconfigured, ~10h default) resync.
func TestMapCoClaimants_EnqueuesTheOtherLiveClaimant(t *testing.T) {
	ctx := context.Background()
	aliceUI := userIdentityFor("u-alice", aliceSubject, "u-alice-gh")
	bobUI := userIdentityFor("u-bob", bobSubject, "u-bob-gh")
	c := buildClient(t,
		attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil),
		attestedSecret(t, "u-bob-gh", "github-pat", attestedID, nil),
		aliceUI, bobUI,
	)
	r := newReconciler(c)

	// Bob's own event must enqueue alice — the OTHER live claimant of the
	// SAME attested account — not himself (the primary watch already
	// enqueues bob) and not nobody.
	reqs := r.mapCoClaimants(ctx, bobUI)

	require.Len(t, reqs, 1, "bob's event must enqueue exactly the other co-claimant")
	assert.Equal(t, "u-alice", reqs[0].Name)
}

// A UserIdentity that shares no attested account with anyone enqueues
// nothing — the mapper must not fan out to the whole fleet on every event
// just because SOME account somewhere is contested.
//
// carol MUST be present and MUST attest a DIFFERENT account: a fixture
// holding only alice never exercises the account-match check at all (there
// is nothing else to compare against, so the assertion would pass even if
// the mapper matched every candidate unconditionally). carol's presence is
// what makes this test able to fail against that mutant.
func TestMapCoClaimants_NoCoClaimantEnqueuesNothing(t *testing.T) {
	ctx := context.Background()
	aliceUI := userIdentityFor("u-alice", aliceSubject, "u-alice-gh")
	carolUI := userIdentityFor("u-carol", carolSubject, "u-carol-gh")
	c := buildClient(t,
		attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil),
		attestedSecret(t, "u-carol-gh", "github-pat", otherAttestedID, nil),
		aliceUI, carolUI,
	)
	r := newReconciler(c)

	assert.Empty(t, r.mapCoClaimants(ctx, aliceUI), "carol claims a DIFFERENT account and must not be enqueued")
}

// Deleting bob's UserIdentity is itself an event this mapper must react to —
// this is what drives the RESTORING pass. controller-runtime hands a Delete
// handler the informer's last-known object, still carrying bob's
// credentials; this test hands the mapper that same pre-deletion object
// directly, which is what lets alice be nudged to re-check her sole_user
// without waiting for the resync.
func TestMapCoClaimants_EnqueuesOnTheDeletedObjectsLastKnownState(t *testing.T) {
	ctx := context.Background()
	aliceUI := userIdentityFor("u-alice", aliceSubject, "u-alice-gh")
	bobUI := userIdentityFor("u-bob", bobSubject, "u-bob-gh")
	c := buildClient(t,
		attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil),
		attestedSecret(t, "u-bob-gh", "github-pat", attestedID, nil),
		aliceUI, bobUI,
	)
	r := newReconciler(c)

	require.NoError(t, c.Delete(ctx, bobUI))

	reqs := r.mapCoClaimants(ctx, bobUI)
	require.Len(t, reqs, 1, "bob's last-known state still names the account alice also claims")
	assert.Equal(t, "u-alice", reqs[0].Name)
}

// BLOCKER (final whole-branch review): the derivation is driven ENTIRELY by
// the credentials a pass reads, so the one change it could never see is the
// last claim going away. Remove the credential and this account is never
// visited again; sole_user stands, and the repository roles a directory sync
// keeps rewriting stand with it, with no shipped path to remove the tuple —
// TypedWritesSource claims the relation, so no bootstrap or CLI can touch it.
//
// Every existing TestAttestedEdge_Sole* case exercises the two-claimant path,
// where the OTHER claimant's pass does the withdrawing. Here there is no other
// claimant, which is exactly why nothing ran.
func TestAttestedEdge_SoleUserIsWithdrawnWhenTheLastCredentialIsRemoved(t *testing.T) {
	ctx := context.Background()
	sec := attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil)
	c := buildClient(t, sec, userIdentityFor("u-alice", aliceSubject, "u-alice-gh"))
	w := newFakeWriter()
	r := newEdgeReconciler(c, w, nil)

	reconcileOnce(t, r, "u-alice")
	key := "github_user:" + attestedID
	require.Contains(t, w.sole, key, "alice holds it while she is the sole claimant")

	// What `oap user-identity delete-token` does: the credential leaves
	// spec.credentials, and its Secret is deleted just after.
	var live spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: "u-alice"}, &live))
	live.Spec.Credentials = nil
	require.NoError(t, c.Update(ctx, &live))
	require.NoError(t, c.Delete(ctx, sec))

	reconcileOnce(t, r, "u-alice")

	assert.Contains(t, w.soleDeletes, key,
		"the account lost its last claimant; sole_user must be withdrawn, not left standing")
	assert.NotContains(t, w.sole, key,
		"durable authority derived from an account nobody claims any more must not survive the unlink")
}

// The same gap through the other door, and the one no level-triggered
// recomputation can close on its own: a deleted UserIdentity is never
// reconciled again, and mapCoClaimants enqueues only OTHER claimants — of
// which, in the case that matters, there are none. The finalizer is what buys
// the one pass that withdraws.
//
// The release half is asserted too: a finalizer that withdraws but never lets
// go is a wedged object needing manual surgery, which is its own outage.
func TestAttestedEdge_SoleUserIsWithdrawnWhenTheUserIdentityIsDeleted(t *testing.T) {
	ctx := context.Background()
	aliceUI := userIdentityFor("u-alice", aliceSubject, "u-alice-gh")
	c := buildClient(t, attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil), aliceUI)
	w := newFakeWriter()
	r := newEdgeReconciler(c, w, nil)

	reconcileOnce(t, r, "u-alice")
	key := "github_user:" + attestedID
	require.Contains(t, w.sole, key, "alice holds it while she is the sole claimant")

	require.NoError(t, c.Delete(ctx, aliceUI))
	reconcileOnce(t, r, "u-alice") // the finalization pass

	assert.Contains(t, w.soleDeletes, key,
		"deleting the catalog removes the last claim; the edge it minted must go with it")
	assert.NotContains(t, w.sole, key,
		"otherwise the person keeps every repository role that account reaches, indefinitely")

	var gone spiceboxv1alpha1.UserIdentity
	err := c.Get(ctx, client.ObjectKey{Name: "u-alice"}, &gone)
	assert.True(t, apierrors.IsNotFound(err),
		"the finalizer must be released once the withdrawal lands, not hold the object in Terminating")
}

// A co-claimant's tuple is not collateral. Deleting one of two catalogs that
// claim the same account must leave the OTHER one's claim intact — the
// withdrawal is scoped to the subject that is leaving, and liveAttestedClaimants
// is what tells the two apart.
func TestAttestedEdge_DeletingOneOfTwoClaimantsLeavesTheOthersAccountAlone(t *testing.T) {
	ctx := context.Background()
	aliceUI := userIdentityFor("u-alice", aliceSubject, "u-alice-gh")
	c := buildClient(t,
		attestedSecret(t, "u-alice-gh", "github-pat", attestedID, nil),
		attestedSecret(t, "u-carol-gh", "github-pat", otherAttestedID, nil),
		aliceUI,
		userIdentityFor("u-carol", carolSubject, "u-carol-gh"),
	)
	w := newFakeWriter()
	r := newEdgeReconciler(c, w, nil)

	reconcileOnce(t, r, "u-alice")
	reconcileOnce(t, r, "u-carol")
	aliceKey := "github_user:" + attestedID
	carolKey := "github_user:" + otherAttestedID
	require.Contains(t, w.sole, aliceKey)
	require.Contains(t, w.sole, carolKey)

	require.NoError(t, c.Delete(ctx, aliceUI))
	reconcileOnce(t, r, "u-alice")

	assert.NotContains(t, w.sole, aliceKey, "alice's own account loses its last claimant")
	assert.Contains(t, w.sole, carolKey, "carol claims a different account and keeps it")
	assert.NotContains(t, w.soleDeletes, carolKey, "and it must never even be a delete target")
}
