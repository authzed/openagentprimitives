package admind_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/agentidentity"
	// Registers the static/oauth/federated credkind.Kinds so
	// agentidentity.Store's credkindregistry.Get(cred.Type) dispatch resolves
	// in this package's tests (exercised end-to-end via the admind credential
	// update handler below).
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
	"github.com/authzed/openagentprimitives/pkg/web/admind/agentcred"
)

const (
	credNS       = "tenant-a"
	credSess     = "sess-agentcred"
	credIdentity = "billing-bot-id"
	credName     = "billing-api-key"
	credSecret   = "billing-bot-creds"
	credKey      = "apiKey"
	credStale    = "sk_dead"
	credFresh    = "sk_fresh"
	credToken    = "test-admind-token"
	credAdmin    = "user:admin@example.org"
	credOutsider = "user:outsider@example.org"
)

// credChecker answers the per-resource permission. It counts calls so a test
// can assert the operator ACTUALLY asked, rather than inferring it from an
// outcome that a trusting implementation would also produce.
type credChecker struct {
	allow map[string]bool // "<ns>/<name>|<canonical>" → allowed
	err   error
	calls int
}

func (c *credChecker) CheckAgentIdentityUpdateCredential(_ context.Context, ns, name string, canonical identity.CanonicalUserID) (bool, error) {
	c.calls++
	if c.err != nil {
		return false, c.err
	}
	return c.allow[ns+"/"+name+"|"+canonical.String()], nil
}

func (c *credChecker) CheckPlatformPermission(context.Context, string, identity.CanonicalUserID, bool) (bool, error) {
	return false, nil
}

func (c *credChecker) ListPlatformAdmins(context.Context) ([]string, error) { return nil, nil }

func allowing(subject string) *credChecker {
	canonical := strings.TrimPrefix(subject, "user:")
	return &credChecker{allow: map[string]bool{credNS + "/" + credIdentity + "|" + canonical: true}}
}

func credObjects(extra ...client.Object) []client.Object {
	return append([]client.Object{
		&spiceboxv1alpha1.AgentIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: credIdentity, Namespace: credNS},
			Spec: spiceboxv1alpha1.AgentIdentitySpec{
				Credentials: []spiceboxv1alpha1.AgentCredential{{
					Name: credName, Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: credSecret, Key: credKey},
					},
				}},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: credSecret, Namespace: credNS},
			Data:       map[string][]byte{credKey: []byte(credStale)},
		},
		credUpdateRequest(spiceboxv1alpha1.IdentityKindAgentIdentity, credNS, credIdentity, credName,
			&spiceboxv1alpha1.NamespacedRef{Namespace: credNS, Name: credSecret}),
	}, extra...)
}

func credUpdateRequest(kind, idNS, idName, cred string, secretRef *spiceboxv1alpha1.NamespacedRef) *spiceboxv1alpha1.CredentialUpdateRequest {
	return &spiceboxv1alpha1.CredentialUpdateRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "cur-" + cred, Namespace: credNS},
		Spec: spiceboxv1alpha1.CredentialUpdateRequestSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: credNS, Name: credSess},
			Origin:     "mcpserver/billing",
			ToolName:   "list_invoices",
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

// newCredAdmind builds a real Admind over a fake cluster and returns its
// handler plus the client so a test can read back what did (or did not) move.
func newCredAdmind(t *testing.T, checker admind.PlatformChecker, objs ...client.Object) (http.Handler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
	a, err := admind.New(admind.Config{
		Mem:     memory.NewLocal(inmem.NewBackend()),
		K8s:     c,
		Checker: checker,
		Token:   credToken,
		Logger:  testr.New(t),
	})
	require.NoError(t, err)
	return a.Handler(), c
}

// postCred issues the request the way pkg/web/admind/agentcred's client does:
// service token in Authorization, proven subject in X-Admin-Subject, and a body
// naming ONLY the session, credential, and value.
func postCred(t *testing.T, h http.Handler, token, subject string, req agentcred.Request) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, agentcred.Path, strings.NewReader(string(body)))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if subject != "" {
		r.Header.Set(agentcred.SubjectHeader, subject)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func validRequest() agentcred.Request {
	return agentcred.Request{
		SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: credNS, Name: credSess},
		Credential: credName,
		Token:      credFresh,
	}
}

func storedValue(t *testing.T, c client.Client) string {
	t.Helper()
	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: credNS, Name: credSecret}, &sec))
	return string(sec.Data[credKey])
}

// --- AgentIdentityRef branch: the workshop bot-credential write path (plan
// 5a). This authority is deliberately NOT agentidentity#update_credential —
// see agentcred.Request.AgentIdentityRef's doc — it is that the caller
// STARTED the workshop the target namespace belongs to, re-derived by admind
// from ref.Namespace itself rather than trusted from the body.

const (
	wsSessionNS   = "tenant-b"
	wsSessionName = "build-sess"
	wsNamespace   = "ws-abc123"
	wsIdentity    = "weather-ai"
	wsCredential  = "api_key"
	wsSecret      = "weather-ai-creds"
	wsKey         = "apiKey"
	wsStale       = "sk_dead_ws"
	wsFresh       = "sk_fresh_ws"
	wsStarter     = "user:workshop-starter@example.org"
	wsNotStarter  = "user:not-the-starter@example.org"

	// wsOAuthCredential is a SECOND credential on the same workshop
	// AgentIdentity, declared type=oauth, alongside the pre-existing
	// type=static wsCredential above — so the PAT arm's fixtures and
	// assertions stay byte-unchanged while the OAuth arm gets its own
	// pre-declared credential + backing Secret to write into.
	wsOAuthCredential = "oauth_cred"
	wsOAuthSecret     = "weather-ai-oauth-creds"
)

// workshopObjects seeds a genuine workshop W (wsNamespace): a Namespace
// carrying the session-attribution labels, the owning Workshop CR whose
// status.namespace claims W, and an AgentIdentity + backing Secret inside W.
// mutate (optional) lets a case corrupt one fact — a missing label, a
// mismatched status.namespace — to exercise the forgery-refusal branches.
func workshopObjects(mutate ...func(ws *spiceboxv1alpha1.Workshop, ns *corev1.Namespace)) []client.Object {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: wsNamespace,
			Labels: map[string]string{
				spiceboxv1alpha1.LabelWorkshopSessionNamespace: wsSessionNS,
				spiceboxv1alpha1.LabelWorkshopSessionName:      wsSessionName,
			},
		},
	}
	ws := &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spiceboxv1alpha1.WorkshopName(wsSessionName),
			Namespace: wsSessionNS,
		},
		Spec: spiceboxv1alpha1.WorkshopSpec{
			Session:          spiceboxv1alpha1.NamespacedRef{Namespace: wsSessionNS, Name: wsSessionName},
			StarterCanonical: strings.TrimPrefix(wsStarter, "user:"),
			SidecarToolbox:   "builder-sidecar",
		},
		Status: spiceboxv1alpha1.WorkshopStatus{
			Namespace: wsNamespace,
		},
	}
	for _, m := range mutate {
		m(ws, ns)
	}
	return []client.Object{
		ns, ws,
		&spiceboxv1alpha1.AgentIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: wsIdentity, Namespace: wsNamespace},
			Spec: spiceboxv1alpha1.AgentIdentitySpec{
				Credentials: []spiceboxv1alpha1.AgentCredential{
					{
						Name: wsCredential, Type: "static",
						Static: &spiceboxv1alpha1.StaticCredentialSource{
							SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: wsSecret, Key: wsKey},
						},
					},
					{
						Name: wsOAuthCredential, Type: "oauth",
						OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
							SecretRef: spiceboxv1alpha1.SecretRef{Name: wsOAuthSecret},
						},
					},
				},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: wsSecret, Namespace: wsNamespace},
			Data:       map[string][]byte{wsKey: []byte(wsStale)},
		},
	}
}

func wsValidRequest() agentcred.Request {
	return agentcred.Request{
		AgentIdentityRef: &spiceboxv1alpha1.NamespacedRef{Namespace: wsNamespace, Name: wsIdentity},
		Credential:       wsCredential,
		Token:            wsFresh,
	}
}

func wsStoredValue(t *testing.T, c client.Client) string {
	t.Helper()
	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: wsNamespace, Name: wsSecret}, &sec))
	return string(sec.Data[wsKey])
}

// wsOAuthValidRequest targets wsOAuthCredential (type=oauth) rather than the
// static wsCredential — the OAuth arm of the same AgentIdentityRef branch.
func wsOAuthValidRequest() agentcred.Request {
	return agentcred.Request{
		AgentIdentityRef: &spiceboxv1alpha1.NamespacedRef{Namespace: wsNamespace, Name: wsIdentity},
		Credential:       wsOAuthCredential,
		OAuth: &agentcred.OAuthBundle{
			AccessToken:   "access-fresh",
			RefreshToken:  "refresh-fresh",
			TokenType:     "Bearer",
			Scope:         "read write",
			TokenEndpoint: "https://idp.example.org/token",
			ClientID:      "client-abc",
			ClientSecret:  "client-secret-abc",
			ExpiresAt:     1893456000,
		},
	}
}

// TestAgentCredentialUpdate_WorkshopStarterWritesTheWorkshopAgentIdentity is
// case (a): the caller who started the workshop writes the Secret named by
// the AgentIdentity's own SecretRef inside W — and the platform-admin
// checker is never even consulted, because this branch's authority is the
// workshop starter relationship, not agentidentity#update_credential.
func TestAgentCredentialUpdate_WorkshopStarterWritesTheWorkshopAgentIdentity(t *testing.T) {
	checker := &credChecker{}
	h, c := newCredAdmind(t, checker, workshopObjects()...)

	w := postCred(t, h, credToken, wsStarter, wsValidRequest())
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp agentcred.Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, wsNamespace, resp.SecretRef.Namespace)
	assert.Equal(t, wsSecret, resp.SecretRef.Name, "the operator must report the destination it chose")
	assert.Equal(t, wsFresh, wsStoredValue(t, c))
	assert.Zero(t, checker.calls, "the workshop-starter authority must not fall through to agentidentity#update_credential")
}

// TestAgentCredentialUpdate_WorkshopNonStarterWritesNothing is case (b): the
// same request from someone other than the workshop's starter is refused,
// and nothing moves.
func TestAgentCredentialUpdate_WorkshopNonStarterWritesNothing(t *testing.T) {
	checker := &credChecker{}
	h, c := newCredAdmind(t, checker, workshopObjects()...)

	w := postCred(t, h, credToken, wsNotStarter, wsValidRequest())
	require.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, wsStale, wsStoredValue(t, c), "a caller who did not start this workshop must move nothing")
	assert.Zero(t, checker.calls)
}

// --- OAuth arm of the same AgentIdentityRef branch (plan 5a-oauth, task 2):
// the SAME workshop-starter authority as above, branching only at the
// terminal write (agentidentity.PutOAuthToken instead of PutToken).

// TestAgentCredentialUpdate_WorkshopStarterWritesOAuthBundle is case (a): a
// request carrying OAuth (not Token) from the workshop starter writes the
// full bundle via PutOAuthToken onto the pre-declared type=oauth credential
// — and, exactly like the PAT arm, the platform-admin checker is never
// consulted.
func TestAgentCredentialUpdate_WorkshopStarterWritesOAuthBundle(t *testing.T) {
	checker := &credChecker{}
	h, c := newCredAdmind(t, checker, workshopObjects()...)

	req := wsOAuthValidRequest()
	w := postCred(t, h, credToken, wsStarter, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp agentcred.Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, wsNamespace, resp.SecretRef.Namespace)
	assert.Equal(t, wsOAuthSecret, resp.SecretRef.Name, "the operator must report the destination it chose")

	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: wsNamespace, Name: wsOAuthSecret}, &sec))
	assert.Equal(t, req.OAuth.AccessToken, string(sec.Data["access_token"]))
	assert.Equal(t, req.OAuth.RefreshToken, string(sec.Data["refresh_token"]))
	assert.Equal(t, req.OAuth.TokenType, string(sec.Data["token_type"]))
	assert.Equal(t, req.OAuth.Scope, string(sec.Data["scope"]))
	assert.Equal(t, req.OAuth.TokenEndpoint, string(sec.Data["token_endpoint"]))
	assert.Equal(t, req.OAuth.ClientID, string(sec.Data["client_id"]))
	assert.Equal(t, req.OAuth.ClientSecret, string(sec.Data["client_secret"]))
	assert.NotEmpty(t, sec.Data["expires_at"])
	assert.Zero(t, checker.calls, "the workshop-starter authority must not fall through to agentidentity#update_credential")
}

// TestAgentCredentialUpdate_WorkshopNonStarterOAuthWritesNothing is case (b)
// for the OAuth arm: the SAME 403 as the PAT arm for a non-starter, and no
// Secret is created.
func TestAgentCredentialUpdate_WorkshopNonStarterOAuthWritesNothing(t *testing.T) {
	checker := &credChecker{}
	h, c := newCredAdmind(t, checker, workshopObjects()...)

	w := postCred(t, h, credToken, wsNotStarter, wsOAuthValidRequest())
	require.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	err := c.Get(context.Background(), client.ObjectKey{Namespace: wsNamespace, Name: wsOAuthSecret}, &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err), "a caller who did not start this workshop must move nothing; err=%v", err)
	assert.Zero(t, checker.calls)
}

// TestAgentCredentialUpdate_WorkshopExactlyOneOfTokenOrOAuth is case (c):
// both Token and OAuth set, or neither, is a 400 — checked at the terminal
// write, AFTER the starter gate (so both cases here go through as the
// starter, to prove the 400 is reached rather than masked by a 403).
func TestAgentCredentialUpdate_WorkshopExactlyOneOfTokenOrOAuth(t *testing.T) {
	cases := []struct {
		name string
		req  agentcred.Request
	}{
		{
			name: "both token and oauth set",
			req: func() agentcred.Request {
				r := wsOAuthValidRequest()
				r.Token = wsFresh
				return r
			}(),
		},
		{
			name: "neither token nor oauth set",
			req: agentcred.Request{
				AgentIdentityRef: &spiceboxv1alpha1.NamespacedRef{Namespace: wsNamespace, Name: wsIdentity},
				Credential:       wsOAuthCredential,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checker := &credChecker{}
			h, c := newCredAdmind(t, checker, workshopObjects()...)

			w := postCred(t, h, credToken, wsStarter, tc.req)
			require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
			err := c.Get(context.Background(), client.ObjectKey{Namespace: wsNamespace, Name: wsOAuthSecret}, &corev1.Secret{})
			assert.True(t, apierrors.IsNotFound(err), "an ambiguous request must move nothing")
			assert.Equal(t, wsStale, wsStoredValue(t, c), "the static credential must also be untouched")
			assert.Zero(t, checker.calls)
		})
	}
}

// TestAgentCredentialUpdate_WorkshopOAuthAgainstStaticCredentialFailsClosed
// is case (d): an OAuth request naming a type=static credential is a type
// mismatch. PutOAuthToken refuses it (ErrNotOAuthCredential); CodeFor maps
// that to a clean 4xx via agentidentity.CodeNotOAuthCredential, not a 500 —
// the exact wiring plan 5a-oauth task 2 adds to store.go.
func TestAgentCredentialUpdate_WorkshopOAuthAgainstStaticCredentialFailsClosed(t *testing.T) {
	checker := &credChecker{}
	h, c := newCredAdmind(t, checker, workshopObjects()...)

	req := wsOAuthValidRequest()
	req.Credential = wsCredential // type=static, not type=oauth
	w := postCred(t, h, credToken, wsStarter, req)

	require.NotEqual(t, http.StatusInternalServerError, w.Code, "a type mismatch is a clean refusal, not a fault; body=%s", w.Body.String())
	assert.GreaterOrEqual(t, w.Code, 400)
	assert.Less(t, w.Code, 500)
	var eb agentcred.ErrorBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &eb))
	assert.Equal(t, agentidentity.CodeNotOAuthCredential, eb.Code)
	assert.Equal(t, wsStale, wsStoredValue(t, c), "no write on a type mismatch")
}

// TestAgentCredentialUpdate_WorkshopMissingNamespaceRefused covers the
// target namespace not existing at all — the first Get in the re-derivation
// chain fails closed.
func TestAgentCredentialUpdate_WorkshopMissingNamespaceRefused(t *testing.T) {
	checker := &credChecker{}
	objs := []client.Object{
		&spiceboxv1alpha1.AgentIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: wsIdentity, Namespace: wsNamespace},
			Spec: spiceboxv1alpha1.AgentIdentitySpec{
				Credentials: []spiceboxv1alpha1.AgentCredential{{
					Name: wsCredential, Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: wsSecret, Key: wsKey},
					},
				}},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: wsSecret, Namespace: wsNamespace},
			Data:       map[string][]byte{wsKey: []byte(wsStale)},
		},
	}
	h, c := newCredAdmind(t, checker, objs...)

	w := postCred(t, h, credToken, wsStarter, wsValidRequest())
	require.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, wsStale, wsStoredValue(t, c))
	assert.Zero(t, checker.calls)
}

// TestAgentCredentialUpdate_WorkshopNamespaceForgeryRefused is case (c): a
// target namespace that is not GENUINELY a workshop — no session labels, a
// labeled session with no Workshop CR, or a Workshop CR that does not claim
// this namespace as its own status.namespace (a forged label pointing at
// someone else's namespace) — is refused, never trusted from the body.
func TestAgentCredentialUpdate_WorkshopNamespaceForgeryRefused(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(ws *spiceboxv1alpha1.Workshop, ns *corev1.Namespace)
	}{
		{
			name: "target namespace carries no workshop-session labels",
			mutate: func(_ *spiceboxv1alpha1.Workshop, ns *corev1.Namespace) {
				ns.Labels = nil
			},
		},
		{
			name: "target namespace carries only the session-namespace label",
			mutate: func(_ *spiceboxv1alpha1.Workshop, ns *corev1.Namespace) {
				delete(ns.Labels, spiceboxv1alpha1.LabelWorkshopSessionName)
			},
		},
		{
			name: "labeled session has no Workshop CR",
			mutate: func(_ *spiceboxv1alpha1.Workshop, ns *corev1.Namespace) {
				ns.Labels[spiceboxv1alpha1.LabelWorkshopSessionName] = "no-such-session"
			},
		},
		{
			name: "Workshop CR does not claim the target namespace as its own status.namespace",
			mutate: func(ws *spiceboxv1alpha1.Workshop, _ *corev1.Namespace) {
				ws.Status.Namespace = "ws-someone-elses-namespace"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checker := &credChecker{}
			h, c := newCredAdmind(t, checker, workshopObjects(tc.mutate)...)

			w := postCred(t, h, credToken, wsStarter, wsValidRequest())
			require.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
			assert.Equal(t, wsStale, wsStoredValue(t, c), "a namespace that does not resolve to a genuine workshop must move nothing")
			assert.Zero(t, checker.calls)
		})
	}
}

// TestAgentCredentialUpdate_ExactlyOneOfSessionRefOrAgentIdentityRef is case
// (d): the two target shapes are mutually exclusive.
func TestAgentCredentialUpdate_ExactlyOneOfSessionRefOrAgentIdentityRef(t *testing.T) {
	cases := []struct {
		name string
		req  agentcred.Request
	}{
		{
			name: "both sessionRef and agentIdentityRef set",
			req: agentcred.Request{
				SessionRef:       spiceboxv1alpha1.NamespacedRef{Namespace: credNS, Name: credSess},
				AgentIdentityRef: &spiceboxv1alpha1.NamespacedRef{Namespace: wsNamespace, Name: wsIdentity},
				Credential:       credName,
				Token:            credFresh,
			},
		},
		{
			name: "neither sessionRef nor agentIdentityRef set",
			req: agentcred.Request{
				Credential: credName,
				Token:      credFresh,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checker := allowing(credAdmin)
			h, _ := newCredAdmind(t, checker, append(credObjects(), workshopObjects()...)...)

			w := postCred(t, h, credToken, credAdmin, tc.req)
			require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
		})
	}
}

// TestAgentCredentialUpdate_AuthorizedWriteMovesTheBackingSecret — the happy
// path, and the destination is the one the CredentialUpdateRequest recorded
// (which is what the reconciler watches to mark the request Fulfilled).
func TestAgentCredentialUpdate_AuthorizedWriteMovesTheBackingSecret(t *testing.T) {
	checker := allowing(credAdmin)
	h, c := newCredAdmind(t, checker, credObjects()...)

	w := postCred(t, h, credToken, credAdmin, validRequest())
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp agentcred.Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, credNS, resp.SecretRef.Namespace)
	assert.Equal(t, credSecret, resp.SecretRef.Name, "the operator must report the destination it chose")
	assert.Equal(t, credFresh, storedValue(t, c))
	assert.Equal(t, 1, checker.calls, "the operator must run its own check exactly once")
}

// TestAgentCredentialUpdate_DeniedSubjectWritesNothing — THE test for the trust
// boundary. The caller is fully authenticated as a service (correct token) and
// asserts a subject; the operator's own check says that subject may not act. It
// must refuse. A design that trusted the caller — because it had a valid token,
// or because webd "already checked" — would write here.
func TestAgentCredentialUpdate_DeniedSubjectWritesNothing(t *testing.T) {
	checker := allowing(credAdmin) // credOutsider is NOT granted
	h, c := newCredAdmind(t, checker, credObjects()...)

	w := postCred(t, h, credToken, credOutsider, validRequest())
	require.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, credStale, storedValue(t, c), "a denied subject must move nothing")
	assert.Equal(t, 1, checker.calls, "the denial must come from an actual check, not a shortcut")
}

// TestAgentCredentialUpdate_RefusalsAndFaults enumerates every non-happy branch.
// Each asserts the status AND that the stored value did not move, because a
// refusal that still wrote would be the worst of both worlds.
func TestAgentCredentialUpdate_RefusalsAndFaults(t *testing.T) {
	cases := []struct {
		name     string
		checker  *credChecker
		objs     []client.Object
		token    string
		subject  string
		req      agentcred.Request
		want     int
		wantHint string
	}{
		{
			name:    "no service token: 401, and no permission check is even attempted",
			checker: allowing(credAdmin), objs: credObjects(),
			token: "", subject: credAdmin, req: validRequest(), want: http.StatusUnauthorized,
		},
		{
			name:    "wrong service token: 401",
			checker: allowing(credAdmin), objs: credObjects(),
			token: "not-the-token", subject: credAdmin, req: validRequest(), want: http.StatusUnauthorized,
		},
		{
			name:    "no asserted subject: 401 — a valid token alone authorizes nothing",
			checker: allowing(credAdmin), objs: credObjects(),
			token: credToken, subject: "", req: validRequest(), want: http.StatusUnauthorized,
		},
		{
			name:    "non-user subject: 401",
			checker: allowing(credAdmin), objs: credObjects(),
			token: credToken, subject: "group:admins#member", req: validRequest(), want: http.StatusUnauthorized,
		},
		{
			name:    "user-owned credential: 403 — this route can only touch an AgentIdentity",
			checker: allowing(credAdmin),
			objs: []client.Object{
				credUpdateRequest(spiceboxv1alpha1.IdentityKindUserIdentity, "", "ui-someone", credName, nil),
			},
			token: credToken, subject: credAdmin, req: validRequest(), want: http.StatusForbidden,
		},
		{
			name:    "no matching request at all: 403",
			checker: allowing(credAdmin), objs: []client.Object{},
			token: credToken, subject: credAdmin, req: validRequest(), want: http.StatusForbidden,
		},
		{
			name:    "authorization service errors: 500 — a fault is neither a grant nor a denial",
			checker: &credChecker{err: assertErr{}}, objs: credObjects(),
			token: credToken, subject: credAdmin, req: validRequest(), want: http.StatusInternalServerError,
		},
		{
			name:    "same value re-pasted: 409 with the value_unchanged code",
			checker: allowing(credAdmin), objs: credObjects(),
			token: credToken, subject: credAdmin,
			req:  agentcred.Request{SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: credNS, Name: credSess}, Credential: credName, Token: credStale},
			want: http.StatusConflict, wantHint: agentidentity.CodeValueUnchanged,
		},
		{
			name:    "missing fields: 400",
			checker: allowing(credAdmin), objs: credObjects(),
			token: credToken, subject: credAdmin,
			req:  agentcred.Request{SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: credNS, Name: credSess}},
			want: http.StatusBadRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, c := newCredAdmind(t, tc.checker, tc.objs...)
			w := postCred(t, h, tc.token, tc.subject, tc.req)
			require.Equal(t, tc.want, w.Code, "body=%s", w.Body.String())
			if tc.wantHint != "" {
				var eb agentcred.ErrorBody
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &eb))
				assert.Equal(t, tc.wantHint, eb.Code,
					"the machine-readable code is what lets the browser render the RIGHT next step")
			}
			// The Secret exists only in the fixtures that seeded it.
			var sec corev1.Secret
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: credNS, Name: credSecret}, &sec); err == nil {
				assert.Equal(t, credStale, string(sec.Data[credKey]), "no refused or faulted request may write")
			}
		})
	}
}

// TestAgentCredentialUpdate_TargetComesFromTheClusterNotTheCaller — the request
// body cannot name an identity. Pointing it at a session whose request resolves
// elsewhere reaches THAT session's identity or nothing at all; there is no field
// through which a caller could aim the write.
func TestAgentCredentialUpdate_TargetComesFromTheClusterNotTheCaller(t *testing.T) {
	checker := allowing(credAdmin)
	h, c := newCredAdmind(t, checker, credObjects()...)

	req := validRequest()
	req.SessionRef.Name = "some-other-session" // no CredentialUpdateRequest names it
	w := postCred(t, h, credToken, credAdmin, req)

	require.Equal(t, http.StatusForbidden, w.Code,
		"a session with no matching request resolves to no target, so there is nothing to authorize; body=%s", w.Body.String())
	assert.Equal(t, credStale, storedValue(t, c))
	assert.Zero(t, checker.calls, "no permission is checked when no target resolves — nothing was authorized into existence")
}
