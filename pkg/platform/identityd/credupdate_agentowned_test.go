package identityd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientpkg "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
	"github.com/authzed/openagentprimitives/pkg/web/admind/agentcred"
)

// The one implementation identityd's click gate is meant to run against is
// spicedb.Client.CheckAgentIdentityUpdateCredential — the helper Task 3 made
// unconditionally FullyConsistent, with no `fullyConsistent bool` opt-in a
// caller could get wrong. Pinning the satisfaction here means a future edit
// that grows a consistency parameter (or introduces a second, MinimizeLatency
// helper for identityd to call) breaks the BUILD rather than silently
// reintroducing the stale-read refusal of a legitimate admin.
var _ AgentIdentityAuthz = (*spicedb.Client)(nil)

const (
	agentOwnedNS      = "tenant-a"
	agentOwnedSess    = "sess-agentcred"
	agentOwnedID      = "billing-bot-id"
	agentOwnedCred    = "billing-api-key"
	agentOwnedSecret  = "billing-bot-creds"
	agentOwnedKey     = "apiKey"
	agentOwnedStale   = "sk_dead_value"
	agentOwnedFresh   = "sk_fresh_value"
	adminSubject      = identity.Subject("user:admin@example.org")
	bystanderSubject  = identity.Subject("user:bystander@example.org")
	agentOwnedStarter = "user:requester@example.org"
)

// fakeAgentAuthz is a permission oracle under test control.
//
// ONE type serves both roles — identityd's UX gate and the operator's
// authoritative check — precisely so a test can hand a DIFFERENT instance to
// each and prove they are genuinely independent. It satisfies
// admind.PlatformChecker in full; the two platform-wide methods are unused by
// the credential route and answer with zero values.
//
// permitted is read on EVERY call rather than captured once, so a test can
// revoke between the render and the submit — which is the only way to prove the
// check happens at click time rather than at publish time.
type fakeAgentAuthz struct {
	permitted map[string]bool // "<ns>/<name>|<canonical>" → allowed
	err       error
	calls     int
}

func (f *fakeAgentAuthz) CheckAgentIdentityUpdateCredential(_ context.Context, ns, name string, canonical identity.CanonicalUserID) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	return f.permitted[ns+"/"+name+"|"+canonical.String()], nil
}

func (f *fakeAgentAuthz) CheckPlatformPermission(context.Context, string, identity.CanonicalUserID, bool) (bool, error) {
	return false, nil
}

func (f *fakeAgentAuthz) ListPlatformAdmins(context.Context) ([]string, error) { return nil, nil }

func (f *fakeAgentAuthz) grant(ns, name string, subject identity.Subject) {
	canonical, err := subject.CanonicalUserID()
	if err != nil {
		panic("fakeAgentAuthz.grant: " + err.Error())
	}
	if f.permitted == nil {
		f.permitted = map[string]bool{}
	}
	f.permitted[ns+"/"+name+"|"+canonical.String()] = true
}

func (f *fakeAgentAuthz) revoke(ns, name string, subject identity.Subject) {
	canonical, err := subject.CanonicalUserID()
	if err != nil {
		panic("fakeAgentAuthz.revoke: " + err.Error())
	}
	delete(f.permitted, ns+"/"+name+"|"+canonical.String())
}

// agentOwnedObjects is the full agent-owned shape: a session, the AgentIdentity
// + its backing Secret, and an Open CredentialUpdateRequest whose status names
// that identity as the credential's owner and records the Secret the reconciler
// watches.
func agentOwnedObjects(extra ...clientpkg.Object) []clientpkg.Object {
	return append([]clientpkg.Object{
		makeAgentSession(agentOwnedNS, agentOwnedSess, agentOwnedStarter),
		makeAgentIdentity(),
		makeAgentSecret(agentOwnedStale),
		makeCredUpdateRequest(spiceboxv1alpha1.IdentityKindAgentIdentity, agentOwnedNS, agentOwnedID, agentOwnedCred,
			&spiceboxv1alpha1.NamespacedRef{Namespace: agentOwnedNS, Name: agentOwnedSecret}),
	}, extra...)
}

// newAgentOwnedFixture builds the standard fixture: the SAME oracle answers
// identityd's UX gate and the operator's authoritative check, which is the
// production arrangement (both are the one SpiceDB).
func newAgentOwnedFixture(t *testing.T, authz *fakeAgentAuthz, extra ...clientpkg.Object) linkFixture {
	t.Helper()
	// A nil *fakeAgentAuthz must reach Deps as a genuine nil INTERFACE, not a
	// typed-nil pointer boxed into one — the fail-closed branch tests that.
	var webAuthz AgentIdentityAuthz
	var opAuthz admind.PlatformChecker
	if authz != nil {
		webAuthz, opAuthz = authz, authz
	}
	return newLinkFixtureWithAuthz(t, webAuthz, opAuthz, agentOwnedObjects(extra...)...)
}

// newLinkFixtureWithAuthz mirrors newLinkFixture but additionally wires:
//
//   - webAuthz: identityd's own UX gate;
//   - opAuthz: a REAL pkg/web/admind handler, over the SAME fake cluster, on an
//     httptest server that identityd's operator client points at.
//
// The second is what makes these tests worth having. The write is performed by
// the operator, so a test that stubbed it would assert only that identityd sent
// a request — not that the AgentIdentity's Secret moved, nor that the operator's
// independent check governs. Passing DIFFERENT oracles for webAuthz and opAuthz
// is how a test proves the operator does not defer to webd's verdict.
//
// A nil opAuthz leaves the operator client unwired (identityd then fails closed).
func newLinkFixtureWithAuthz(t *testing.T, webAuthz AgentIdentityAuthz, opAuthz admind.PlatformChecker, objs ...clientpkg.Object) linkFixture {
	t.Helper()
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	signer := passthroughlink.New(signerKey)

	var writer AgentCredentialWriter
	if opAuthz != nil {
		const operatorToken = "test-admind-token"
		adm, err := admind.New(admind.Config{
			Mem:     memory.NewLocal(inmem.NewBackend()),
			K8s:     c,
			Checker: opAuthz,
			Token:   operatorToken,
			Logger:  testr.New(t),
		})
		require.NoError(t, err, "build the operator-side admind handler")
		opSrv := httptest.NewServer(adm.Handler())
		t.Cleanup(opSrv.Close)
		writer = agentcred.New(opSrv.URL, operatorToken)
	}

	srv := NewServer(Deps{
		K8s:             c,
		LinkSigner:      signer,
		ExternalBaseURL: func() string { return "https://identityd.example.org" },
		Authenticators: map[string]channelkinds.WebAuthenticator{
			"fake": fakeAuthenticator{externalBaseURL: "https://identityd.example.org"},
		},
		AgentIdentityAuthz:    webAuthz,
		AgentCredentialWriter: writer,
	})
	fx := linkFixture{srv: srv, signer: signer, c: c}
	fx.web = buildWebHandler(t, fx)
	return fx
}

func makeAgentIdentity() *spiceboxv1alpha1.AgentIdentity {
	return &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: agentOwnedID, Namespace: agentOwnedNS},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: agentOwnedCred,
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: agentOwnedSecret, Key: agentOwnedKey},
				},
			}},
		},
	}
}

func makeAgentSecret(value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: agentOwnedSecret, Namespace: agentOwnedNS},
		Data:       map[string][]byte{agentOwnedKey: []byte(value)},
	}
}

// makeCredUpdateRequest builds the Open request identityd reads to learn WHOSE
// credential a credential_update link covers.
func makeCredUpdateRequest(kind, idNS, idName, cred string, secretRef *spiceboxv1alpha1.NamespacedRef) *spiceboxv1alpha1.CredentialUpdateRequest {
	return &spiceboxv1alpha1.CredentialUpdateRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "cur-" + cred, Namespace: agentOwnedNS},
		Spec: spiceboxv1alpha1.CredentialUpdateRequestSpec{
			SessionRef:  spiceboxv1alpha1.NamespacedRef{Namespace: agentOwnedNS, Name: agentOwnedSess},
			Origin:      "mcpserver/billing",
			ToolName:    "list_invoices",
			RequestedBy: agentOwnedStarter,
		},
		Status: spiceboxv1alpha1.CredentialUpdateRequestStatus{
			Phase: spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
			ResolvedCredential: &spiceboxv1alpha1.ResolvedCredentialRef{
				IdentityKind: kind, Namespace: idNS, Name: idName, Credential: cred,
			},
			CredentialSecretRef: secretRef,
		},
	}
}

// mintCredUpdateLink mints the credential_update deep-link channelsd publishes.
// subject == "" is the MONITORING-channel variant: it has no single addressee,
// so it carries no subject and its click must be authorized at identityd.
func mintCredUpdateLink(t *testing.T, fx linkFixture, subject identity.Subject, cred string) string {
	t.Helper()
	raw, err := fx.signer.Mint(passthroughlink.Payload{
		Issuer:              passthroughlink.IssuerChannelsd,
		Audience:            passthroughlink.AudienceIdentityd,
		SessionRef:          agentOwnedNS + "/" + agentOwnedSess,
		Subject:             subject,
		RequiredCredentials: []string{cred},
		Purpose:             passthroughlink.PurposeCredentialUpdate,
		ExpiresAt:           time.Now().Add(5 * time.Minute).Unix(),
	})
	require.NoError(t, err)
	return raw
}

// agentSecretValue reads the CURRENT value of the AgentIdentity's backing
// Secret — the one the CredentialUpdateRequest reconciler watches to decide the
// request was Fulfilled.
func agentSecretValue(t *testing.T, fx linkFixture) string {
	t.Helper()
	var sec corev1.Secret
	require.NoError(t, fx.c.Get(context.Background(),
		clientpkg.ObjectKey{Namespace: agentOwnedNS, Name: agentOwnedSecret}, &sec))
	return string(sec.Data[agentOwnedKey])
}

// requireNoUserIdentity asserts the submitting human's OWN UserIdentity was not
// touched. This is the failure the whole task exists to remove: before the
// agent-owned write path, an admin's click ran useridentity.PutToken and
// overwrote their personal credential while the shared one stayed dead.
func requireNoUserIdentity(t *testing.T, fx linkFixture, subject identity.Subject) {
	t.Helper()
	var ui spiceboxv1alpha1.UserIdentity
	err := fx.c.Get(context.Background(),
		clientpkg.ObjectKey{Name: useridentity.NameForSubject(subject)}, &ui)
	assert.True(t, apierrors.IsNotFound(err),
		"the submitter's own UserIdentity must not be written for an agent-owned credential; got err=%v", err)
}

// TestLinkSubmit_AgentOwned_WritesTheAgentIdentitySecret is the core of this
// task: an admin who completes the flow must move the AGENT's backing Secret —
// the one status.credentialSecretRef names and the reconciler watches — and must
// NOT touch their own UserIdentity.
func TestLinkSubmit_AgentOwned_WritesTheAgentIdentitySecret(t *testing.T) {
	authz := &fakeAgentAuthz{}
	authz.grant(agentOwnedNS, agentOwnedID, adminSubject)
	fx := newAgentOwnedFixture(t, authz)

	raw := mintCredUpdateLink(t, fx, "", agentOwnedCred) // monitoring-channel link: no subject
	cookie := mintCookie(t, fx.signer, adminSubject, time.Time{})

	form := url.Values{"link": {raw}, "credential": {agentOwnedCred}, "token": {agentOwnedFresh}}
	rec := doPOST(t, fx, form, cookie)
	require.Equal(t, http.StatusFound, rec.Code, "an authorized admin's submit must succeed; body=%s", rec.Body.String())

	assert.Equal(t, agentOwnedFresh, agentSecretValue(t, fx),
		"the AgentIdentity's backing Secret must carry the replacement value")
	requireNoUserIdentity(t, fx, adminSubject)
	assert.Positive(t, authz.calls, "the click must be authorized, not assumed")
}

// TestLinkSubmit_AgentOwned_NonAdminRefusedAndNothingWritten — the
// monitoring-channel link is not subject-bound, so ANYONE may hold it. A holder
// without agentidentity#update_credential must be refused, and no Secret and no
// UserIdentity may change.
func TestLinkSubmit_AgentOwned_NonAdminRefusedAndNothingWritten(t *testing.T) {
	authz := &fakeAgentAuthz{}
	authz.grant(agentOwnedNS, agentOwnedID, adminSubject) // someone else is the admin
	fx := newAgentOwnedFixture(t, authz)

	raw := mintCredUpdateLink(t, fx, "", agentOwnedCred)
	cookie := mintCookie(t, fx.signer, bystanderSubject, time.Time{})

	form := url.Values{"link": {raw}, "credential": {agentOwnedCred}, "token": {agentOwnedFresh}}
	rec := doPOST(t, fx, form, cookie)
	require.Equal(t, http.StatusForbidden, rec.Code, "a non-admin holder of the link must be refused")

	assert.Equal(t, agentOwnedStale, agentSecretValue(t, fx),
		"a refused submit must leave the agent's Secret exactly as it was")
	requireNoUserIdentity(t, fx, bystanderSubject)

	// Positive control: without it, every NotContains below would also pass on an
	// empty body or a page that never rendered.
	body := rec.Body.String()
	require.Contains(t, body, "only a platform administrator may do",
		"the refusal page itself must have rendered, or the assertions below prove nothing")

	// The remedy must name the ROLE a reader can actually ask for. `editor` ships
	// unpopulated, so platform administrator is the only standing that exists;
	// naming the permission would send the reader to request a grant nobody can
	// perform, and would put the authorization model's vocabulary on a page any
	// link-holder can reach.
	for _, internal := range []string{"update_credential", "agentidentity#", "spicedb", "SpiceDB"} {
		assert.NotContains(t, body, internal,
			"the refusal page must not name the internal identifier %q; say what the reader can ask for, not how it is modelled", internal)
	}
	assert.Contains(t, body, "ask a platform administrator",
		"the page must point at the only standing that can actually be granted")
}

// TestLinkSubmit_AgentOwned_PermissionIsCheckedAtClickTime — the in-thread
// variant of the card is minted only for an author who passed the check at
// PUBLISH time. Losing the permission before clicking must refuse: the check
// that matters is the one at the click.
func TestLinkSubmit_AgentOwned_PermissionIsCheckedAtClickTime(t *testing.T) {
	authz := &fakeAgentAuthz{}
	authz.grant(agentOwnedNS, agentOwnedID, adminSubject)
	fx := newAgentOwnedFixture(t, authz)

	// Subject-bound (in-thread) link, minted while the author held the permission.
	raw := mintCredUpdateLink(t, fx, adminSubject, agentOwnedCred)
	cookie := mintCookie(t, fx.signer, adminSubject, time.Time{})

	rec := doGET(t, fx, raw, "", cookie)
	require.Equal(t, http.StatusOK, rec.Code, "while permitted, the form must render")

	// …and now the grant is withdrawn, with the same link + cookie in hand.
	authz.revoke(agentOwnedNS, agentOwnedID, adminSubject)

	form := url.Values{"link": {raw}, "credential": {agentOwnedCred}, "token": {agentOwnedFresh}}
	rec = doPOST(t, fx, form, cookie)
	require.Equal(t, http.StatusForbidden, rec.Code, "a permission lost before the click must refuse the write")
	assert.Equal(t, agentOwnedStale, agentSecretValue(t, fx), "nothing may be written after the grant is withdrawn")

	// The re-render must refuse too — the page is not a capability once granted.
	rec = doGET(t, fx, raw, "", cookie)
	assert.Equal(t, http.StatusForbidden, rec.Code, "re-opening the page after revocation must refuse")
}

// TestLinkGet_AgentOwned_NoCookieRedirectsToLogin — a cookie-less visitor is
// sent into sign-in, exactly as the user-owned path does. A 500 (or a bare 403)
// would dead-end an admin who simply has not signed in yet.
func TestLinkGet_AgentOwned_NoCookieRedirectsToLogin(t *testing.T) {
	authz := &fakeAgentAuthz{}
	authz.grant(agentOwnedNS, agentOwnedID, adminSubject)
	fx := newAgentOwnedFixture(t, authz)

	raw := mintCredUpdateLink(t, fx, "", agentOwnedCred)
	rec := doGET(t, fx, raw, "", "") // no cookie
	require.Equal(t, http.StatusFound, rec.Code, "no cookie must redirect, not 500")
	assert.Contains(t, rec.Header().Get("Location"), "/oidc/login?d=",
		"the redirect must begin sign-in and carry the signed link back")
	assert.Zero(t, authz.calls, "no permission check is possible before a subject is proven")
}

// TestLinkSubmit_AgentOwned_NoAuthzWiredFailsClosed — identityd built without an
// authorization client cannot answer the one question that gates this write, so
// it refuses. Fail-closed, never fail-open.
func TestLinkSubmit_AgentOwned_NoAuthzWiredFailsClosed(t *testing.T) {
	fx := newAgentOwnedFixture(t, nil)

	raw := mintCredUpdateLink(t, fx, "", agentOwnedCred)
	cookie := mintCookie(t, fx.signer, adminSubject, time.Time{})

	form := url.Values{"link": {raw}, "credential": {agentOwnedCred}, "token": {agentOwnedFresh}}
	rec := doPOST(t, fx, form, cookie)
	require.Equal(t, http.StatusForbidden, rec.Code, "no authorization client → refuse")
	assert.Equal(t, agentOwnedStale, agentSecretValue(t, fx), "nothing may be written")
	requireNoUserIdentity(t, fx, adminSubject)
}

// TestLinkSubmit_AgentOwned_CheckFailureFailsClosed — an authorization service
// that ERRORS is not a grant. The write is refused and the fault is not
// mistaken for a denial in the logs (see mayUpdateAgentCredential).
func TestLinkSubmit_AgentOwned_CheckFailureFailsClosed(t *testing.T) {
	authz := &fakeAgentAuthz{err: assertErr("spicedb unavailable")}
	fx := newAgentOwnedFixture(t, authz)

	raw := mintCredUpdateLink(t, fx, "", agentOwnedCred)
	cookie := mintCookie(t, fx.signer, adminSubject, time.Time{})

	form := url.Values{"link": {raw}, "credential": {agentOwnedCred}, "token": {agentOwnedFresh}}
	rec := doPOST(t, fx, form, cookie)
	require.Equal(t, http.StatusForbidden, rec.Code, "a failed check must refuse, not admit")
	assert.Equal(t, agentOwnedStale, agentSecretValue(t, fx))
}

// TestLinkSubmit_AgentOwned_SameValueRefused — re-pasting the value that is
// already stored would leave the Secret's content hash unchanged, so the
// request would sit out its window and then report that NOBODY updated the
// credential. A human did act; say something true instead of writing nothing
// and claiming success.
func TestLinkSubmit_AgentOwned_SameValueRefused(t *testing.T) {
	authz := &fakeAgentAuthz{}
	authz.grant(agentOwnedNS, agentOwnedID, adminSubject)
	fx := newAgentOwnedFixture(t, authz)

	raw := mintCredUpdateLink(t, fx, "", agentOwnedCred)
	cookie := mintCookie(t, fx.signer, adminSubject, time.Time{})

	form := url.Values{"link": {raw}, "credential": {agentOwnedCred}, "token": {agentOwnedStale}}
	rec := doPOST(t, fx, form, cookie)
	require.Equal(t, http.StatusConflict, rec.Code, "an unchanged value must be refused, not silently accepted")
	assert.Contains(t, rec.Body.String(), "Paste the NEW credential",
		"the page must tell the admin what to do instead")
}

// TestLinkGet_AgentOwned_RendersAPasteFormForAnAdmin — the monitoring link
// carries NO subject, so the starter-recipient gate the user-owned path applies
// would refuse it outright. The agent-owned route must serve it (to an
// authorized admin) and render a PAT form, not an OAuth button that would link
// the VISITOR's own account.
func TestLinkGet_AgentOwned_RendersAPasteFormForAnAdmin(t *testing.T) {
	authz := &fakeAgentAuthz{}
	authz.grant(agentOwnedNS, agentOwnedID, adminSubject)
	fx := newAgentOwnedFixture(t, authz)

	raw := mintCredUpdateLink(t, fx, "", agentOwnedCred)
	cookie := mintCookie(t, fx.signer, adminSubject, time.Time{})

	rec := doGET(t, fx, raw, "", cookie)
	require.Equal(t, http.StatusOK, rec.Code, "an authorized admin must be served the subject-less monitoring link")
	body := rec.Body.String()
	assert.Contains(t, body, `data-app="identity-link"`)
	assert.Contains(t, body, `"credentialName":"`+agentOwnedCred+`"`)
	assert.Contains(t, body, `"kind":"pat"`, "the row must be a paste form, never an OAuth link to the visitor's own account")
	assert.Contains(t, body, `"status":"missing"`, "the row must render its form; a 'linked' badge would hide it")
}

// TestLinkSubmit_UserOwnedCredentialUpdateStillUsesPutToken is the REGRESSION
// guard. A credential_update link for a PERSON's own credential is the feature's
// only path that worked before this task; it must keep writing that person's
// UserIdentity master Secret and must not touch any AgentIdentity Secret.
func TestLinkSubmit_UserOwnedCredentialUpdateStillUsesPutToken(t *testing.T) {
	stubVerify(t, http.StatusOK, `{"login":"tester"}`)
	const userCred = "github-pat"
	authz := &fakeAgentAuthz{} // wired, but must never be consulted on this path

	// A cluster that ALSO holds the agent-owned shape, so a mis-route would be
	// visible rather than merely absent.
	fx := newLinkFixtureWithAuthz(t, authz, authz,
		makeAgentSession(agentOwnedNS, agentOwnedSess, agentOwnedStarter),
		makeAgentIdentity(),
		makeAgentSecret(agentOwnedStale),
		makeCredUpdateRequest(spiceboxv1alpha1.IdentityKindUserIdentity, "", useridentity.NameForSubject(agentOwnedStarter), userCred, nil),
	)

	raw := mintCredUpdateLink(t, fx, agentOwnedStarter, userCred)
	cookie := mintCookie(t, fx.signer, agentOwnedStarter, time.Time{})

	form := url.Values{"link": {raw}, "credential": {userCred}, "token": {"ghp_userTOKEN"}}
	rec := doPOST(t, fx, form, cookie)
	require.Equal(t, http.StatusFound, rec.Code, "the user-owned path must still succeed; body=%s", rec.Body.String())

	uiName := useridentity.NameForSubject(agentOwnedStarter)
	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui),
		"a user-owned credential update must still write the user's own UserIdentity")
	require.Len(t, ui.Spec.Credentials, 1)
	assert.Equal(t, userCred, ui.Spec.Credentials[0].Name)

	var sec corev1.Secret
	require.NoError(t, fx.c.Get(context.Background(), clientpkg.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      useridentity.MasterSecretName(uiName, userCred),
	}, &sec))
	assert.Equal(t, "ghp_userTOKEN", string(sec.Data["token"]))

	assert.Equal(t, agentOwnedStale, agentSecretValue(t, fx),
		"no AgentIdentity Secret may be touched by a user-owned credential update")
	assert.Zero(t, authz.calls, "the agent-owned permission check must not run on the user-owned path")
}

// TestLinkSubmit_AmbiguousOwnershipRefused — two live requests in one session
// naming DIFFERENT owners for the link's credential is a shape nothing mints.
// Guessing would send a shared bot token into a person's private identity (or
// the reverse), so the resolution refuses instead.
func TestLinkSubmit_AmbiguousOwnershipRefused(t *testing.T) {
	authz := &fakeAgentAuthz{}
	authz.grant(agentOwnedNS, agentOwnedID, adminSubject)

	second := makeCredUpdateRequest(spiceboxv1alpha1.IdentityKindAgentIdentity, agentOwnedNS, "other-bot-id", agentOwnedCred, nil)
	second.Name = "cur-second"
	fx := newAgentOwnedFixture(t, authz, second)

	raw := mintCredUpdateLink(t, fx, "", agentOwnedCred)
	cookie := mintCookie(t, fx.signer, adminSubject, time.Time{})

	form := url.Values{"link": {raw}, "credential": {agentOwnedCred}, "token": {agentOwnedFresh}}
	rec := doPOST(t, fx, form, cookie)
	require.Equal(t, http.StatusForbidden, rec.Code, "ambiguous ownership must refuse")
	assert.Equal(t, agentOwnedStale, agentSecretValue(t, fx))
}

// assertErr is a tiny error value for table-free error injection.
type assertErr string

func (e assertErr) Error() string { return string(e) }

// AgentIdentityAuthz completes fxWebDeps' implementation of the OPTIONAL
// identityd.WebAuthzDeps interface, so the webui-hosted Server the fixture's
// requests actually reach carries the same authorization client the fixture
// was built with. Declared here rather than in handlers_link_test.go because
// this is the only surface that reads it.
func (d fxWebDeps) AgentIdentityAuthz() AgentIdentityAuthz { return d.fx.srv.deps.AgentIdentityAuthz }

// AgentCredentialWriter is the other half of that interface: the operator client
// identityd submits the agent-owned write to. identityd holds no Secret access,
// so without this the fixture's requests could never move a Secret.
func (d fxWebDeps) AgentCredentialWriter() AgentCredentialWriter {
	return d.fx.srv.deps.AgentCredentialWriter
}

// TestLinkSubmit_AgentOwned_OperatorRefusesWhatWebdWouldAllow is the crux of the
// two-component split: the OPERATOR's check is authoritative, and it is not
// webd's.
//
// webd is given an oracle that says yes; the operator is given one that says no.
// If the operator ever deferred to the caller — a header, a flag, a "webd
// already checked" convention — this would write, because webd's gate passed and
// webd asserted the subject. It must refuse, and nothing may move.
func TestLinkSubmit_AgentOwned_OperatorRefusesWhatWebdWouldAllow(t *testing.T) {
	webPermissive := &fakeAgentAuthz{}
	webPermissive.grant(agentOwnedNS, agentOwnedID, adminSubject)
	operatorStrict := &fakeAgentAuthz{} // grants nobody

	fx := newLinkFixtureWithAuthz(t, webPermissive, operatorStrict, agentOwnedObjects()...)

	raw := mintCredUpdateLink(t, fx, "", agentOwnedCred)
	cookie := mintCookie(t, fx.signer, adminSubject, time.Time{})

	form := url.Values{"link": {raw}, "credential": {agentOwnedCred}, "token": {agentOwnedFresh}}
	rec := doPOST(t, fx, form, cookie)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"the operator's refusal must stand even though webd's own gate allowed it; body=%s", rec.Body.String())

	assert.Equal(t, agentOwnedStale, agentSecretValue(t, fx), "nothing may be written on the operator's refusal")
	requireNoUserIdentity(t, fx, adminSubject)
	assert.Positive(t, webPermissive.calls, "sanity: webd's gate really did run and really did allow")
	assert.Positive(t, operatorStrict.calls, "the operator must run its OWN check, not trust the caller")
}

// TestLinkSubmit_AgentOwned_NoOperatorClientFailsClosed — identityd cannot write
// a Secret itself, by design. With no operator client wired there is nothing
// that can perform the write, and the submit must say so rather than appear to
// succeed.
//
// It must also say the RIGHT thing. A missing operator client is a deployment
// wiring fault: no value the admin can paste and no number of retries will ever
// make the write succeed, so reporting it as a conflict over the value they
// typed — and telling them to try again — is advice that can never work. The
// admin retries, watches it fail identically, and never learns that the thing
// they need is an operator finishing the install.
func TestLinkSubmit_AgentOwned_NoOperatorClientFailsClosed(t *testing.T) {
	webAuthz := &fakeAgentAuthz{}
	webAuthz.grant(agentOwnedNS, agentOwnedID, adminSubject)
	fx := newLinkFixtureWithAuthz(t, webAuthz, nil, agentOwnedObjects()...) // nil → no writer

	raw := mintCredUpdateLink(t, fx, "", agentOwnedCred)
	cookie := mintCookie(t, fx.signer, adminSubject, time.Time{})

	form := url.Values{"link": {raw}, "credential": {agentOwnedCred}, "token": {agentOwnedFresh}}
	rec := doPOST(t, fx, form, cookie)
	// assert, not require: the status and the copy are three separate facts
	// about the same page, and a run should report all of them at once.
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"an unwired operator client is a server-side fault, not a conflict with the submitted value")

	body := rec.Body.String()
	assert.NotContains(t, body, "Please try again",
		"retrying cannot fix a wiring fault, so the page must not advise it")
	assert.Contains(t, body, "retrying won't change that",
		"the page must tell the admin plainly that this needs an operator, not another paste")

	assert.Equal(t, agentOwnedStale, agentSecretValue(t, fx))
	requireNoUserIdentity(t, fx, adminSubject)
}

// ---------------------------------------------------------------------------
// PurposeWorkshopCredential — a bot credential for an AgentIdentity living in
// a workshop namespace W, connected by the builder who started the workshop.
//
// This purpose shares every gate function above (resolveAgentOwnedTarget,
// gateAgentOwned/mayUpdateAgentCredential, submitAgentOwned,
// handleAgentOwnedLinkGet) with PurposeCredentialUpdate, but its AUTHORITY is
// different: not agentidentity#update_credential (platform-admin-only), but
// "started the workshop that owns this AgentIdentity's namespace" — the same
// fact pkg/web/admind's handleAgentIdentityRefCredentialUpdate re-checks on
// the write. These tests exist to prove identityd's UX gate asserts that same
// fact, not the admin permission the other purpose uses.
// ---------------------------------------------------------------------------

const (
	wcredBuilderNS   = "builder-b"
	wcredBuilderSess = "builder-x"
	wcredWorkshopNS  = "ws-abc123"
	wcredAgentID     = "weather-ai"
	wcredCred        = "weather-api-key"
	wcredSecret      = "weather-ai-creds"
	wcredKey         = "apiKey"
	wcredStale       = "sk_dead_weather"
	wcredFresh       = "sk_fresh_weather"
	// wcredStarterBare is the BARE canonical id (no "user:" prefix) — the
	// shape both identity.CanonicalUserID and WorkshopSpec.StarterCanonical
	// use. See workshop_hook.go (canonical.String() written verbatim) and
	// admind's handleAgentIdentityRefCredentialUpdate (compares bare-to-bare).
	wcredStarterBare = "builder-starter"
	wcredStarter     = identity.Subject("user:" + wcredStarterBare)
	wcredBystander   = identity.Subject("user:someone-else")
)

// makeWorkshopNamespace is the Namespace object admind's own re-derivation
// (handleAgentIdentityRefCredentialUpdate) reads to map the target namespace
// back to the builder session that owns it. identityd's own check does not
// consume these labels — it trusts payload.SessionRef directly, since it rode
// inside the HMAC-signed link — but the round-trip tests below exercise the
// REAL operator, which does.
func makeWorkshopNamespace() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: wcredWorkshopNS,
			Labels: map[string]string{
				spiceboxv1alpha1.LabelWorkshopSessionNamespace: wcredBuilderNS,
				spiceboxv1alpha1.LabelWorkshopSessionName:      wcredBuilderSess,
			},
		},
	}
}

// makeWorkshop returns the Workshop CR the click-time gate reads: starter +
// the provisioned namespace admind's re-derivation must land back on.
func makeWorkshop(starterCanonical string) *spiceboxv1alpha1.Workshop {
	return &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spiceboxv1alpha1.WorkshopName(wcredBuilderSess),
			Namespace: wcredBuilderNS,
		},
		Spec: spiceboxv1alpha1.WorkshopSpec{
			Session:          spiceboxv1alpha1.NamespacedRef{Namespace: wcredBuilderNS, Name: wcredBuilderSess},
			StarterCanonical: starterCanonical,
			SidecarToolbox:   "builder-sidecar",
			Limits: spiceboxv1alpha1.WorkshopLimits{
				MaxAge:              metav1.Duration{Duration: time.Hour},
				MaxObjectsPerKind:   10,
				MaxObjects:          50,
				MaxConcurrentProbes: 2,
			},
		},
		Status: spiceboxv1alpha1.WorkshopStatus{Namespace: wcredWorkshopNS, Phase: "Ready"},
	}
}

// makeWorkshopAgentIdentity is a "static" (pasteable) credential in the
// workshop namespace — the shape a builder's request_credential tool would
// have authored for a freshly-declared bot identity.
func makeWorkshopAgentIdentity() *spiceboxv1alpha1.AgentIdentity {
	return &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: wcredAgentID, Namespace: wcredWorkshopNS},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: wcredCred,
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: wcredSecret, Key: wcredKey},
				},
			}},
		},
	}
}

// makeWorkshopOAuthAgentIdentity is the SAME identity/credential name, but
// type=oauth: its Secret is a multi-key bundle only an OAuth ceremony can
// produce, so agentidentity.Resolve reports it not replaceable by pasting a
// value (ErrNotReplaceable).
func makeWorkshopOAuthAgentIdentity() *spiceboxv1alpha1.AgentIdentity {
	return &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: wcredAgentID, Namespace: wcredWorkshopNS},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: wcredCred,
				Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretRef{Name: wcredSecret},
				},
			}},
		},
	}
}

func makeWorkshopSecret(value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: wcredSecret, Namespace: wcredWorkshopNS},
		Data:       map[string][]byte{wcredKey: []byte(value)},
	}
}

// newWorkshopCredFixture builds the standard workshop-credential shape: the
// builder AgentSession (handleLinkGet's step-2 lookup needs it to exist,
// whatever the purpose), the labeled workshop Namespace, the Workshop CR
// naming starterCanonical, and the target AgentIdentity + its backing Secret.
func newWorkshopCredFixture(t *testing.T, starterCanonical string, writer AgentCredentialWriter, extraIdentity clientpkg.Object, extra ...clientpkg.Object) linkFixture {
	t.Helper()
	scheme := newScheme(t)
	objs := append([]clientpkg.Object{
		makeAgentSession(wcredBuilderNS, wcredBuilderSess, string(wcredStarter)),
		makeWorkshopNamespace(),
		makeWorkshop(starterCanonical),
		extraIdentity,
		makeWorkshopSecret(wcredStale),
	}, extra...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	signer := passthroughlink.New(signerKey)
	srv := NewServer(Deps{
		K8s:             c,
		LinkSigner:      signer,
		ExternalBaseURL: func() string { return "https://identityd.example.org" },
		Authenticators: map[string]channelkinds.WebAuthenticator{
			"fake": fakeAuthenticator{externalBaseURL: "https://identityd.example.org"},
		},
		AgentCredentialWriter: writer,
	})
	fx := linkFixture{srv: srv, signer: signer, c: c}
	fx.web = buildWebHandler(t, fx)
	return fx
}

// newWorkshopCredFixtureWithRealOperator wires a REAL admind handler over the
// same fake cluster, the way newLinkFixtureWithAuthz does for the
// credential_update purpose — so a submit test proves the AgentIdentity's
// Secret actually moves through the real write path (Task 6's
// handleAgentIdentityRefCredentialUpdate), not merely that identityd sent
// some request. The Checker is required by admind.New but never consulted on
// this branch: the workshop-starter check reads the cluster directly.
func newWorkshopCredFixtureWithRealOperator(t *testing.T, starterCanonical string) linkFixture {
	t.Helper()
	scheme := newScheme(t)
	objs := []clientpkg.Object{
		makeAgentSession(wcredBuilderNS, wcredBuilderSess, string(wcredStarter)),
		makeWorkshopNamespace(),
		makeWorkshop(starterCanonical),
		makeWorkshopAgentIdentity(),
		makeWorkshopSecret(wcredStale),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	signer := passthroughlink.New(signerKey)

	const operatorToken = "test-admind-token"
	adm, err := admind.New(admind.Config{
		Mem:     memory.NewLocal(inmem.NewBackend()),
		K8s:     c,
		Checker: &fakeAgentAuthz{}, // unused by the AgentIdentityRef branch; admind.New still requires non-nil
		Token:   operatorToken,
		Logger:  testr.New(t),
	})
	require.NoError(t, err, "build the operator-side admind handler")
	opSrv := httptest.NewServer(adm.Handler())
	t.Cleanup(opSrv.Close)
	writer := agentcred.New(opSrv.URL, operatorToken)

	srv := NewServer(Deps{
		K8s:             c,
		LinkSigner:      signer,
		ExternalBaseURL: func() string { return "https://identityd.example.org" },
		Authenticators: map[string]channelkinds.WebAuthenticator{
			"fake": fakeAuthenticator{externalBaseURL: "https://identityd.example.org"},
		},
		AgentCredentialWriter: writer,
	})
	fx := linkFixture{srv: srv, signer: signer, c: c}
	fx.web = buildWebHandler(t, fx)
	return fx
}

// mintWorkshopCredLink mints a PurposeWorkshopCredential deep-link: SessionRef
// names the BUILDER session ("<B>/<X>"), AgentIdentityRef names the target
// identity in the workshop namespace ("<W>/<name>").
func mintWorkshopCredLink(t *testing.T, fx linkFixture, agentIdentityRef string, creds []string) string {
	t.Helper()
	raw, err := fx.signer.Mint(passthroughlink.Payload{
		Issuer:              passthroughlink.IssuerChannelsd,
		Audience:            passthroughlink.AudienceIdentityd,
		SessionRef:          wcredBuilderNS + "/" + wcredBuilderSess,
		AgentIdentityRef:    agentIdentityRef,
		RequiredCredentials: creds,
		Purpose:             passthroughlink.PurposeWorkshopCredential,
		ExpiresAt:           time.Now().Add(5 * time.Minute).Unix(),
	})
	require.NoError(t, err)
	return raw
}

// workshopSecretValue reads the CURRENT value of the workshop AgentIdentity's
// backing Secret.
func workshopSecretValue(t *testing.T, fx linkFixture) string {
	t.Helper()
	var sec corev1.Secret
	require.NoError(t, fx.c.Get(context.Background(),
		clientpkg.ObjectKey{Namespace: wcredWorkshopNS, Name: wcredSecret}, &sec))
	return string(sec.Data[wcredKey])
}

// fakeCredentialWriter is a directly-inspectable AgentCredentialWriter: it
// records exactly the agentcred.Request it was handed, so a test can assert
// the SHAPE of the outgoing request (which of SessionRef/AgentIdentityRef got
// set) without standing up a real operator.
type fakeCredentialWriter struct {
	calls   int
	subject identity.Subject
	req     agentcred.Request
	resp    agentcred.Response
	err     error
}

func (f *fakeCredentialWriter) Replace(_ context.Context, subject identity.Subject, req agentcred.Request) (agentcred.Response, error) {
	f.calls++
	f.subject = subject
	f.req = req
	return f.resp, f.err
}

// TestLinkGet_WorkshopCredential_StarterRendersPasteForm is test (a): the
// clicker's cookie canonical equals the Workshop's spec.starterCanonical, so
// the paste form renders for the AgentIdentity named in W — the same PAT/
// static row shape credential_update renders, never an OAuth button that
// would link the visitor's own account.
func TestLinkGet_WorkshopCredential_StarterRendersPasteForm(t *testing.T) {
	fx := newWorkshopCredFixture(t, wcredStarterBare, nil, makeWorkshopAgentIdentity())

	raw := mintWorkshopCredLink(t, fx, wcredWorkshopNS+"/"+wcredAgentID, []string{wcredCred})
	cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})

	rec := doGET(t, fx, raw, "", cookie)
	require.Equal(t, http.StatusOK, rec.Code, "the workshop's starter must be served the paste form; body=%s", rec.Body.String())

	body := rec.Body.String()
	assert.Contains(t, body, `data-app="identity-link"`)
	assert.Contains(t, body, `"credentialName":"`+wcredCred+`"`)
	assert.Contains(t, body, `"kind":"pat"`, "the row must be a paste form, never an OAuth link to the visitor's own account")
	assert.Contains(t, body, `"status":"missing"`, "the row must render its form; a 'linked' badge would hide it")
}

// TestLinkGet_WorkshopCredential_NonStarterRefused is test (b): a clicker
// whose canonical differs from spec.starterCanonical is refused fail-closed —
// this link's authority is "started this workshop", not platform-admin, and
// nobody else may connect its credentials.
func TestLinkGet_WorkshopCredential_NonStarterRefused(t *testing.T) {
	fx := newWorkshopCredFixture(t, wcredStarterBare, nil, makeWorkshopAgentIdentity())

	raw := mintWorkshopCredLink(t, fx, wcredWorkshopNS+"/"+wcredAgentID, []string{wcredCred})
	cookie := mintCookie(t, fx.signer, wcredBystander, time.Time{})

	rec := doGET(t, fx, raw, "", cookie)
	require.Equal(t, http.StatusForbidden, rec.Code, "a non-starter holder of the link must be refused")

	body := rec.Body.String()
	assert.NotContains(t, body, "platform administrator",
		"the refusal must not send a builder to ask a platform admin — that is the WRONG authority for this purpose")
	assert.NotContains(t, body, `"credentialName"`, "the paste form must not have rendered for a refused visitor")
}

// TestLinkSubmit_WorkshopCredential_ReplacesViaAgentIdentityRefNotSessionRef
// is test (c): the outgoing agentcred.Request must carry AgentIdentityRef
// (the direct workshop-identity target), never SessionRef (the
// CredentialUpdateRequest-resolving shape credential_update uses) — mixing
// the two up would send the write down admind's OTHER branch, which requires
// a CredentialUpdateRequest a workshop-credential link never has and would
// refuse the write outright, or worse, resolve to the wrong authority.
func TestLinkSubmit_WorkshopCredential_ReplacesViaAgentIdentityRefNotSessionRef(t *testing.T) {
	writer := &fakeCredentialWriter{}
	fx := newWorkshopCredFixture(t, wcredStarterBare, writer, makeWorkshopAgentIdentity())

	raw := mintWorkshopCredLink(t, fx, wcredWorkshopNS+"/"+wcredAgentID, []string{wcredCred})
	cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})

	form := url.Values{"link": {raw}, "credential": {wcredCred}, "token": {wcredFresh}}
	rec := doPOST(t, fx, form, cookie)
	require.Equal(t, http.StatusFound, rec.Code, "the workshop's starter's submit must succeed; body=%s", rec.Body.String())

	require.Positive(t, writer.calls, "the credential writer must have been called")
	require.NotNil(t, writer.req.AgentIdentityRef, "the request must target the AgentIdentity directly via AgentIdentityRef")
	assert.Equal(t, wcredWorkshopNS, writer.req.AgentIdentityRef.Namespace)
	assert.Equal(t, wcredAgentID, writer.req.AgentIdentityRef.Name)
	assert.Equal(t, wcredCred, writer.req.Credential)
	assert.Equal(t, wcredFresh, writer.req.Token)
	assert.Equal(t, wcredStarter, writer.subject)

	assert.Empty(t, writer.req.SessionRef.Namespace, "SessionRef must NOT be set — this is the AgentIdentityRef target shape")
	assert.Empty(t, writer.req.SessionRef.Name, "SessionRef must NOT be set — this is the AgentIdentityRef target shape")
}

// TestLinkSubmit_WorkshopCredential_RealOperatorWritesTheSecret is the
// round-trip proof that spans the join between this task (identityd's UX
// gate + request-shape) and Task 6 (admind's handleAgentIdentityRefCredentialUpdate,
// the authoritative write). A fake writer proves identityd SENT the right
// shape; only a real operator over the same cluster proves that shape is
// actually accepted and moves the Secret admind is supposed to move.
func TestLinkSubmit_WorkshopCredential_RealOperatorWritesTheSecret(t *testing.T) {
	fx := newWorkshopCredFixtureWithRealOperator(t, wcredStarterBare)

	raw := mintWorkshopCredLink(t, fx, wcredWorkshopNS+"/"+wcredAgentID, []string{wcredCred})
	cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})

	form := url.Values{"link": {raw}, "credential": {wcredCred}, "token": {wcredFresh}}
	rec := doPOST(t, fx, form, cookie)
	require.Equal(t, http.StatusFound, rec.Code, "the real operator must accept the starter's write; body=%s", rec.Body.String())

	assert.Equal(t, wcredFresh, workshopSecretValue(t, fx),
		"the workshop AgentIdentity's backing Secret must carry the replacement value")
}

// TestLinkGet_WorkshopCredential_OAuthTypedCredentialRendersConnectPage is
// test (a) (Task 3): an oauth-typed AgentIdentity credential is NEVER offered
// a paste form (agentidentity.Resolve would refuse it with
// ErrNotReplaceable, and identityd must not even try — see
// handleAgentOwnedLinkGet's type peek). It renders a server-side Connect page
// instead: an "oauth"-kind row whose OAuthURL points at THIS package's own
// /link/agent-oauth/<cred> authorize entry, carrying the SAME signed d/sig
// the card itself carried. No DCR happens on this render — only a page with a
// link; DCR only starts once the starter clicks through.
func TestLinkGet_WorkshopCredential_OAuthTypedCredentialRendersConnectPage(t *testing.T) {
	fx := newWorkshopCredFixture(t, wcredStarterBare, nil, makeWorkshopOAuthAgentIdentity())

	raw := mintWorkshopCredLink(t, fx, wcredWorkshopNS+"/"+wcredAgentID, []string{wcredCred})
	d, sig, splitOK := splitRaw(raw)
	require.True(t, splitOK)
	cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})

	rec := doGET(t, fx, raw, "", cookie)
	require.Equal(t, http.StatusOK, rec.Code, "an oauth-typed credential must render a Connect page, not a refusal; body=%s", rec.Body.String())

	body := rec.Body.String()
	assert.Contains(t, body, `"credentialName":"`+wcredCred+`"`)
	assert.Contains(t, body, `"kind":"oauth"`, "the row must be an OAuth link, never a paste form")
	assert.Contains(t, body, `"status":"missing"`)
	// The bootstrap payload is JSON inside an inline <script>; Go's
	// encoding/json default-escapes '&' as & (HTML-safe encoding).
	wantOAuthURL := "/link/agent-oauth/" + wcredCred + "?d=" + d + "\\u0026sig=" + sig
	assert.Contains(t, body, `"oauthUrl":"`+wantOAuthURL+`"`,
		"the Connect link must point at the AGENT authorize entry, carrying the same signed payload, never /link/oauth/ (which links the visitor's own account)")
	assert.NotContains(t, body, `"kind":"pat"`, "no paste form may render alongside the Connect link")
}

// TestLinkGet_WorkshopCredential_NonStarterRefused_OAuthCredential is the
// non-starter counterpart of the Connect-page test: the click-time gate must
// run BEFORE the type peek, so a non-starter is refused identically whether
// the target credential is oauth or static — the Connect page must never
// leak past the permission check.
func TestLinkGet_WorkshopCredential_NonStarterRefused_OAuthCredential(t *testing.T) {
	fx := newWorkshopCredFixture(t, wcredStarterBare, nil, makeWorkshopOAuthAgentIdentity())

	raw := mintWorkshopCredLink(t, fx, wcredWorkshopNS+"/"+wcredAgentID, []string{wcredCred})
	cookie := mintCookie(t, fx.signer, wcredBystander, time.Time{})

	rec := doGET(t, fx, raw, "", cookie)
	require.Equal(t, http.StatusForbidden, rec.Code, "a non-starter holder must be refused before any type peek")
	assert.NotContains(t, rec.Body.String(), `"oauthUrl"`, "the Connect page must not have rendered for a refused visitor")
}

// TestLinkGet_WorkshopCredential_MalformedOrWrongPurposeRefused is test (e):
// a bad AgentIdentityRef, an expired link, and a link that does not actually
// carry PurposeWorkshopCredential must all refuse — none may render the
// workshop paste form or fall through to writing anything.
func TestLinkGet_WorkshopCredential_MalformedOrWrongPurposeRefused(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(p passthroughlink.Payload) passthroughlink.Payload
	}{
		{
			name: "malformed AgentIdentityRef (no namespace/name split)",
			mutate: func(p passthroughlink.Payload) passthroughlink.Payload {
				p.AgentIdentityRef = "not-a-namespaced-ref"
				return p
			},
		},
		{
			name: "expired link",
			mutate: func(p passthroughlink.Payload) passthroughlink.Payload {
				p.ExpiresAt = time.Now().Add(-1 * time.Minute).Unix()
				return p
			},
		},
		{
			name: "wrong purpose (AgentIdentityRef set, but Purpose is not workshop_credential)",
			mutate: func(p passthroughlink.Payload) passthroughlink.Payload {
				p.Purpose = passthroughlink.PurposeArtifactView
				return p
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newWorkshopCredFixture(t, wcredStarterBare, nil, makeWorkshopAgentIdentity())
			cookie := mintCookie(t, fx.signer, wcredStarter, time.Time{})

			p := passthroughlink.Payload{
				Issuer:              passthroughlink.IssuerChannelsd,
				Audience:            passthroughlink.AudienceIdentityd,
				SessionRef:          wcredBuilderNS + "/" + wcredBuilderSess,
				AgentIdentityRef:    wcredWorkshopNS + "/" + wcredAgentID,
				RequiredCredentials: []string{wcredCred},
				Purpose:             passthroughlink.PurposeWorkshopCredential,
				ExpiresAt:           time.Now().Add(5 * time.Minute).Unix(),
			}
			p = tc.mutate(p)
			raw, err := fx.signer.Mint(p)
			require.NoError(t, err)

			rec := doGET(t, fx, raw, "", cookie)
			assert.NotEqual(t, http.StatusOK, rec.Code, "none of these malformed/mismatched links may render the paste form")
			assert.NotContains(t, rec.Body.String(), `"credentialName":"`+wcredCred+`"`,
				"the paste form must not have rendered")
		})
	}
}
