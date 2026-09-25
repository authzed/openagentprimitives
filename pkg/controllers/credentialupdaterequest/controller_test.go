package credentialupdaterequest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/credentialupdaterequest"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker/inproc"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/refresh"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// Fixed fixture names shared by every test in this file -- one session, one
// class, one MCPServer, one credential name, one CredentialUpdateRequest.
// Individual tests vary the credential TYPE and the identity backing the
// session, not these names.
const (
	ns          = "demo-ns"
	sessionName = "demo-session"
	className   = "demo-class"
	mcpName     = "demo-mcp"
	credName    = "demo-cred"
	curName     = "demo-cur"
)

func curKey() types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: curName}
}

// sessionUID is the fixed AgentSession UID every fixture in this file wires
// its CredentialUpdateRequest's ownerReference to, proving
// resolveIdentity's ownedBySession check (F2) passes on the legitimate path.
const sessionUID types.UID = "demo-session-uid"

// ownerRef returns the ownerReference a CredentialUpdateRequest genuinely
// created by the sessionUID-identified AgentSession would carry --
// mirroring the shape pkg/agent/tool/meta/artifact_prepare.go stamps on its
// own owned CRs (APIVersion/Kind/Name/UID + Controller/BlockOwnerDeletion).
func ownerRef() metav1.OwnerReference {
	t := true
	return metav1.OwnerReference{
		APIVersion:         spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
		Kind:               "AgentSession",
		Name:               sessionName,
		UID:                sessionUID,
		Controller:         &t,
		BlockOwnerDeletion: &t,
	}
}

// baseObjects builds a userPassthrough-mode session (AgentSession +
// AgentClass + SessionUserIdentity carrying cred) plus the MCPServer that
// requires it (bound to providerID) and the CredentialUpdateRequest naming
// its origin. userPassthrough is used for every reconcile-level test in this
// file except the dedicated AgentIdentity test: it is the primary case this
// slice exists to serve, and federated credentials are CEL-forbidden on an
// AgentIdentity, so passthrough is the only mode that can host all three
// credential types this file exercises.
func baseObjects(cred spiceboxv1alpha1.AgentCredential, providerID string) (
	*spiceboxv1alpha1.AgentSession, *spiceboxv1alpha1.AgentClass,
	*spiceboxv1alpha1.SessionUserIdentity, *spiceboxv1alpha1.MCPServer, *spiceboxv1alpha1.CredentialUpdateRequest,
) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: sessionName, Namespace: ns, UID: sessionUID},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: className},
	}
	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: className, Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentClassSpec{IdentityMode: spiceboxv1alpha1.IdentityModeUserPassthrough},
	}
	suid := &spiceboxv1alpha1.SessionUserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: sessionName, Namespace: ns},
		Spec: spiceboxv1alpha1.SessionUserIdentitySpec{
			AgentSession: sessionName,
			UserIdentity: "demo-user",
			Subject:      "user:demo-user",
			Credentials:  []spiceboxv1alpha1.AgentCredential{cred},
		},
	}
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: mcpName, Namespace: ns},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: cred.Name, Provider: providerID},
		},
	}
	cur := &spiceboxv1alpha1.CredentialUpdateRequest{
		// CreationTimestamp: the fake client does NOT auto-stamp this the way a
		// real apiserver would, and reconcileOpen's idle-TTL expiry check is
		// computed from it (cr.CreationTimestamp.Time.Add(idleTTL)). Left at
		// its Go zero value, the DEFAULT idle TTL
		// would make every Open request look already-expired against any real
		// r.now(), since "zero time + 30m" is always in the past. Stamping a
		// realistic "just created" time here is what lets tests exercise the
		// Open/no-expiry-yet path at all.
		ObjectMeta: metav1.ObjectMeta{Name: curName, Namespace: ns, OwnerReferences: []metav1.OwnerReference{ownerRef()}, CreationTimestamp: metav1.Now()},
		Spec: spiceboxv1alpha1.CredentialUpdateRequestSpec{
			SessionRef:  spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: sessionName},
			Origin:      "mcpserver/" + mcpName,
			ToolName:    "demo_tool",
			RequestedBy: identity.Subject("user:demo-user"),
		},
	}
	return sess, class, suid, mcp, cur
}

// staticCredential + staticSecret pair for a type=static credential
// resolved through the userPassthrough per-session projected Secret --
// StaticProjection wins, so cred.Static.SecretRef is never actually read;
// it is populated anyway for realism.
func staticCredential() spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{
		Name: credName,
		Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "unused-master-secret", Key: credName},
		},
	}
}

func staticSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.PassthroughCredentialSecretName(sessionName), Namespace: ns},
		Data:       map[string][]byte{credName: []byte("tok-123")},
	}
}

// federatedCredential has no backing Secret this reconciler ever reads: the
// federated CredType short-circuits both the refresh attempt (oauth-only)
// and the probe (explicitly skipped), so no fixture Secret is needed.
func federatedCredential() spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{
		Name: credName,
		Type: "federated",
		Federated: &spiceboxv1alpha1.FederatedCredentialSource{
			Resource:          "urn:demo:resource",
			ResourceServerURL: "https://mcp.example.invalid",
			IdPSecretRef:      spiceboxv1alpha1.SecretRef{Name: "demo-idp-secret"},
		},
	}
}

func oauthCredential() spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{
		Name: credName,
		Type: "oauth",
		OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretRef{Name: "demo-oauth-secret"},
		},
	}
}

// recordingBroker wraps a real broker.Broker and counts Resolve calls, so a
// test can assert a NEGATIVE claim ("no probe was attempted") against the
// actual call surface instead of a proxy signal (an HTTP handler that may
// never be reached for reasons unrelated to the guard under test -- see
// TestReconcile_TableCases' federated case, where the pre-fix version of
// this test file asserted "no HTTP call" but would have stayed green even
// with the federated skip deleted entirely).
type recordingBroker struct {
	broker.Broker
	resolveCalls atomic.Int32
}

func (b *recordingBroker) Resolve(ctx context.Context, req broker.Request) (broker.Resolution, error) {
	b.resolveCalls.Add(1)
	return b.Broker.Resolve(ctx, req)
}

// newReconciler builds a fake-client-backed Reconciler wired with the REAL
// in-process broker (pkg/platform/identity/broker/inproc), wrapped in a
// recordingBroker -- not a fake -- so the probe step exercises the same
// Secret-read/JIT-refresh path production uses; rb lets a test assert
// exactly how many times the broker was actually asked to resolve. This
// reconciler determines ONLY -- it never publishes a card (that is
// channelsd's CredentialUpdateWatcher, a different package/binary), so there
// is no publisher double to wire here.
func newReconciler(t *testing.T, objs ...client.Object) (client.Client, *credentialupdaterequest.Reconciler, *recordingBroker) {
	t.Helper()
	scheme := testfixtures.NewScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.CredentialUpdateRequest{}).
		Build()
	rb := &recordingBroker{Broker: inproc.New(c)}
	r := &credentialupdaterequest.Reconciler{
		Client: c,
		Broker: rb,
	}
	return c, r, rb
}

func reconcileOnce(t *testing.T, r *credentialupdaterequest.Reconciler) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: curKey()})
}

func getCUR(t *testing.T, c client.Client) *spiceboxv1alpha1.CredentialUpdateRequest {
	t.Helper()
	var got spiceboxv1alpha1.CredentialUpdateRequest
	require.NoError(t, c.Get(context.Background(), curKey(), &got), "Get CredentialUpdateRequest")
	return &got
}

// redirectTransport rewrites every outbound request's scheme+host to
// target's, regardless of what the request originally pointed at. This is
// what lets a test drive a REAL catalog provider (provider.ByID only ever
// returns embedded, real entries -- there is no injectable test catalog)
// through its declarative verify: probe (a hardcoded https://api.github.com/
// endpoint, in practice) without ever touching the real network.
type redirectTransport struct{ target *url.URL }

func (t *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.URL.Scheme = t.target.Scheme
	r2.URL.Host = t.target.Host
	r2.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(r2)
}

// installRedirectClient points builtins' package-level verify HTTP client
// (builtins.SetVerifyHTTPClient) at srv for the test's lifetime. Package-
// level, global, mutable state -- tests using this must NOT run
// t.Parallel() with each other or with anything else that touches the same
// seam (mirrors the hazard the repo already documents for
// refresh.SetHTTPClient).
func installRedirectClient(t *testing.T, srv *httptest.Server) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	builtins.SetVerifyHTTPClient(func() *http.Client {
		return &http.Client{Transport: &redirectTransport{target: u}}
	})
	t.Cleanup(func() { builtins.SetVerifyHTTPClient(nil) })
}

// TestReconcile_TableCases covers the four credential-type/probe-outcome
// combinations from the task brief: the static/verified, static/still-live,
// static/unverifiable, and federated/never-probed paths through Determine.
//
// "github-pat" is used as the real catalog provider for the Rejected/Valid
// cases (it declares a verify: block with no registered Go flow in this
// test binary -- see installRedirectClient's doc -- so VerifyCredential
// takes the declarative HTTP-probe path). "tailscale-authkey" (no verify:
// block at all) drives the Unsupported case without any HTTP call. The
// federated case's "no probe attempted" claim is pinned via the
// recordingBroker's call count (see the assertion below), not just the
// absence of an HTTP hit -- see recordingBroker's doc for why the HTTP
// signal alone is not guard-sensitive for that specific claim.
func TestReconcile_TableCases(t *testing.T) {
	cases := []struct {
		name              string
		credType          string
		verifyStatus      builtins.VerifyStatus
		wantPhase         string
		wantDetermination string
	}{
		{
			name:              "static credential rejected by the provider: Open, awaiting channelsd to publish",
			credType:          "static",
			verifyStatus:      builtins.VerifyRejected,
			wantPhase:         spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified,
		},
		{
			name:              "static credential still valid: Refused, no card",
			credType:          "static",
			verifyStatus:      builtins.VerifyValid,
			wantPhase:         spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationCredentialLive,
		},
		{
			name:              "unverifiable provider with no corroboration: Refused, no card",
			credType:          "static",
			verifyStatus:      builtins.VerifyUnsupported,
			wantPhase:         spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationUnverified,
		},
		{
			name:              "federated credential: Refused NotUpdatable, no card, no probe attempted",
			credType:          "federated",
			verifyStatus:      builtins.VerifyRejected, // must be ignored
			wantPhase:         spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationNotUpdatable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var probed atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				probed.Store(true)
				switch tc.verifyStatus {
				case builtins.VerifyValid:
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"login":"demo"}`))
				case builtins.VerifyRejected:
					w.WriteHeader(http.StatusUnauthorized)
				default:
					w.WriteHeader(http.StatusTeapot)
				}
			}))
			t.Cleanup(srv.Close)
			installRedirectClient(t, srv)

			providerID := "github-pat" // declares verify:, no registered Go flow in this binary
			if tc.verifyStatus == builtins.VerifyUnsupported {
				providerID = "tailscale-authkey" // no verify: block -> Unsupported, no HTTP call ever
			}

			var cred spiceboxv1alpha1.AgentCredential
			switch tc.credType {
			case "static":
				cred = staticCredential()
			case "federated":
				cred = federatedCredential()
				providerID = "" // irrelevant: the probe never runs for federated
			default:
				t.Fatalf("unhandled credType %q in test table", tc.credType)
			}

			sess, class, suid, mcp, cur := baseObjects(cred, providerID)
			objs := []client.Object{sess, class, suid, mcp, cur}
			if tc.credType == "static" {
				objs = append(objs, staticSecret())
			}

			c, r, rb := newReconciler(t, objs...)
			_, err := reconcileOnce(t, r)
			require.NoError(t, err, "Reconcile")

			got := getCUR(t, c)
			assert.Equal(t, tc.wantPhase, got.Status.Phase, "phase; status=%+v", got.Status)
			assert.Equal(t, tc.wantDetermination, got.Status.Determination, "determination")
			assert.NotEmpty(t, got.Status.Reason, "no-silent-errors: Reason must never be empty on a decision")
			require.NotNil(t, got.Status.ResolvedCredential, "ResolvedCredential must be populated once Origin resolves")
			assert.Equal(t, credName, got.Status.ResolvedCredential.Credential)

			// This reconciler determines only -- it never publishes, so
			// InteractionRef is ALWAYS empty here regardless of phase; that is
			// channelsd's CredentialUpdateWatcher's job once it observes Open.
			assert.Empty(t, got.Status.InteractionRef, "the operator never stamps InteractionRef -- channelsd does, once it publishes")

			if tc.wantPhase == spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen {
				// The Open transition must still record the backing-Secret
				// baseline channelsd's eventual publish and the unpark Secret
				// watch both depend on.
				require.NotNil(t, got.Status.CredentialSecretRef, "Open must record the backing Secret ref")
				assert.NotEmpty(t, got.Status.CredentialSecretObservedHash, "Open must record the baseline content hash")
			}

			if tc.verifyStatus != builtins.VerifyRejected && tc.verifyStatus != builtins.VerifyValid {
				assert.False(t, probed.Load(), "no HTTP probe should have been attempted")
			}

			if tc.credType == "federated" {
				// The actual claim the test name makes ("no probe attempted"):
				// the broker's Resolve must never be called at all. The HTTP-probe
				// assertion above is a weaker proxy -- VerifyCredential would
				// return Indeterminate (no HTTP) for a federated resolve failure
				// even if the CredType=="federated" guard in Reconcile were
				// deleted, since inproc.Broker.Resolve errors on federated with no
				// Minter configured, and Determine step 1 refuses NotUpdatable
				// regardless of Probe either way -- so only counting the actual
				// Resolve calls is guard-sensitive here.
				assert.Equal(t, int32(0), rb.resolveCalls.Load(), "federated credentials must never reach the broker at all")
			}
		})
	}
}

// TestReconcile_HonorsCredentialRemap pins the AgentClass's per-MCPServer
// credentialRemap being carried into ResolveOrigin.
//
// The two halves of credential naming live in different places and the remap
// is the bridge: mcpkind.SetupRequirements suggests the name the TOOL declares
// (MCPServer.spec.auth.credential -- here the RAW "upstream-token"), while a
// SessionUserIdentity's credentials are keyed by the POST-remap name (here
// "demo-cred"). The reconciler built its ResolveInput without Remap, so the
// lookup asked the identity for a name it does not contain and every remapped
// MCPServer refused NoCredential -- on the only identity mode this flow supports.
//
// Asserting ResolvedCredential.Credential (not just the phase) is what makes
// this guard-sensitive: an implementation that resolved the raw name against
// an identity that happened to carry BOTH would still reach Open, just naming
// the wrong -- healthy -- credential.
func TestReconcile_HonorsCredentialRemap(t *testing.T) {
	// The name the MCPServer (and therefore SetupRequirements) declares. It is
	// deliberately ABSENT from the identity: only the post-remap name is there.
	const rawCredName = "upstream-token"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // definitive rejection -> Open
	}))
	t.Cleanup(srv.Close)
	installRedirectClient(t, srv)

	sess, class, suid, mcp, cur := baseObjects(staticCredential(), "github-pat")
	// The MCPServer declares the raw name...
	mcp.Spec.Auth.Credential = rawCredName
	// ...and the class remaps it onto the credential the identity actually has.
	class.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{
		Name:            "up",
		Ref:             mcpName,
		CredentialRemap: map[string]string{rawCredName: credName},
	}}

	c, r, _ := newReconciler(t, sess, class, suid, mcp, cur, staticSecret())
	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "Reconcile")

	got := getCUR(t, c)
	require.NotNil(t, got.Status.ResolvedCredential,
		"the remap must resolve to a credential; a missing Remap refuses NoCredential with a nil ref. status=%+v", got.Status)
	assert.Equal(t, credName, got.Status.ResolvedCredential.Credential,
		"the POST-remap credential name is what the identity holds and what a human would be asked to replace")
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, got.Status.Phase,
		"a rejected, remapped credential opens a card; status=%+v", got.Status)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified, got.Status.Determination)
}

// TestReconcile_UnremappedNameStillRefuses is TestReconcile_HonorsCredentialRemap's
// control: with the SAME raw-vs-identity name mismatch but NO remap declared on
// the class, there genuinely is no credential to update, and the refusal is
// correct rather than a bug. Without this pair, a "remap works" test could pass
// against an implementation that ignored the declared mapping and simply
// resolved whatever single credential the identity happened to hold.
func TestReconcile_UnremappedNameStillRefuses(t *testing.T) {
	sess, class, suid, mcp, cur := baseObjects(staticCredential(), "github-pat")
	mcp.Spec.Auth.Credential = "upstream-token"
	class.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "up", Ref: mcpName}}

	c, r, _ := newReconciler(t, sess, class, suid, mcp, cur, staticSecret())
	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "Reconcile")

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, got.Status.Phase)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationNoCredential, got.Status.Determination)
}

// TestReconcile_OAuthRefreshSucceedsRefusesSelfHealed is the brief's named
// standalone OAuth test: an expired access_token with a live refresh_token,
// backed by an httptest token endpoint that returns a fresh token. The
// decisive assertion is that the Secret's access_token actually CHANGED --
// proof the machine fix really ran, not merely that it was reported.
func TestReconcile_OAuthRefreshSucceedsRefusesSelfHealed(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access-token","refresh_token":"new-refresh-token","expires_in":3600,"token_type":"Bearer"}`))
	}))
	t.Cleanup(tokenSrv.Close)
	refresh.SetHTTPClient(tokenSrv.Client())
	t.Cleanup(func() { refresh.SetHTTPClient(safehttp.Client()) })

	// providerID is left empty -- irrelevant either way, since a successful
	// refresh (RefreshOK) makes Reconcile skip the probe ENTIRELY (F5:
	// Determine's SelfHealed branch, step 2, ignores Probe regardless of its
	// value, so calling VerifyCredential at all would just waste egress).
	// Asserted below via rb.resolveCalls.
	cred := oauthCredential()
	sess, class, suid, mcp, cur := baseObjects(cred, "")
	oauthSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-oauth-secret", Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		Data: map[string][]byte{
			"access_token":   []byte("old-access-token"),
			"refresh_token":  []byte("old-refresh-token"),
			"expires_at":     []byte(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)),
			"token_endpoint": []byte(tokenSrv.URL),
			"client_id":      []byte("client-abc"),
		},
	}

	c, r, rb := newReconciler(t, sess, class, suid, mcp, cur, oauthSecret)
	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "Reconcile")

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, got.Status.Phase, "status=%+v", got.Status)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationSelfHealed, got.Status.Determination)
	assert.Empty(t, got.Status.InteractionRef)
	assert.Equal(t, int32(0), rb.resolveCalls.Load(),
		"a successful refresh must skip the probe entirely (F5) -- Determine's SelfHealed verdict never looks at Probe")

	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: "demo-oauth-secret"}, &sec))
	assert.Equal(t, "new-access-token", string(sec.Data["access_token"]),
		"the Secret's access_token must actually change -- this is what proves the refresh really ran")
	assert.NotEqual(t, "old-access-token", string(sec.Data["access_token"]))
}

// TestReconcile_RefreshInvalidatesBrokerCache pins the fix for F1: the
// broker caches oauth resolutions with a ZERO (never-expires) TTL
// (pkg/platform/identity/broker/inproc), so refresh.Run writing a fresh access_token
// straight to the Secret is not enough on its own -- without an explicit
// InvalidateSecret, the broker would keep serving whatever it last resolved
// forever, and "SelfHealed" (the agent is told "retry your call") would be a
// lie: the retry would still get the pre-refresh, now-invalid token.
//
// This warms the broker's cache with the PRE-refresh token first (exactly as
// an earlier, unrelated resolve -- e.g. a prior tool dispatch -- would),
// reconciles (which refreshes AND must invalidate), then resolves through
// the SAME broker instance again: the decisive assertion is that this
// second resolve returns the NEW token, not the warmed-up stale one.
func TestReconcile_RefreshInvalidatesBrokerCache(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access-token","refresh_token":"new-refresh-token","expires_in":3600,"token_type":"Bearer"}`))
	}))
	t.Cleanup(tokenSrv.Close)
	refresh.SetHTTPClient(tokenSrv.Client())
	t.Cleanup(func() { refresh.SetHTTPClient(safehttp.Client()) })

	const oauthSecretName = "demo-oauth-secret"
	cred := oauthCredential()
	sess, class, suid, mcp, cur := baseObjects(cred, "")
	oauthSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: oauthSecretName, Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		Data: map[string][]byte{
			"access_token":  []byte("old-access-token"),
			"refresh_token": []byte("old-refresh-token"),
			// NOT yet expired: the warm-up resolve below must succeed by
			// reading the Secret directly, without the broker's OWN internal
			// JIT refresh firing first (which would immediately overwrite the
			// very staleness this test needs to set up).
			"expires_at":     []byte(time.Now().Add(time.Hour).UTC().Format(time.RFC3339)),
			"token_endpoint": []byte(tokenSrv.URL),
			"client_id":      []byte("client-abc"),
		},
	}

	ctx := context.Background()
	c, r, _ := newReconciler(t, sess, class, suid, mcp, cur, oauthSecret)

	desc := spiceboxv1alpha1.CredentialDescriptor{
		Source: spiceboxv1alpha1.CredentialSource{Type: "oauth", Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: oauthSecretName},
		Inject: spiceboxv1alpha1.CredentialInjection{EnvVar: "TOKEN"},
	}
	warmed, err := r.Broker.Resolve(ctx, broker.Request{Credentials: []spiceboxv1alpha1.CredentialDescriptor{desc}})
	require.NoError(t, err, "warm-up resolve")
	require.Equal(t, "old-access-token", warmed.EnvVars["TOKEN"], "warm-up must cache the PRE-refresh token")

	// Time passes for real: the token actually expires now, which is what
	// makes Reconcile's attemptRefresh fire.
	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: oauthSecretName}, &sec))
	sec.Data["expires_at"] = []byte(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))
	require.NoError(t, c.Update(ctx, &sec))

	_, err = reconcileOnce(t, r)
	require.NoError(t, err, "Reconcile")

	got := getCUR(t, c)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationSelfHealed, got.Status.Determination,
		"sanity: the refresh must have succeeded for this test to mean anything")

	// The decisive assertion: resolving through the SAME broker instance the
	// reconciler used now returns the NEW token. Without the InvalidateSecret
	// fix this would still return "old-access-token" -- the cache never
	// naturally expires, and Reconcile's refresh wrote directly to the
	// Secret, never through the broker.
	fresh, err := r.Broker.Resolve(ctx, broker.Request{Credentials: []spiceboxv1alpha1.CredentialDescriptor{desc}})
	require.NoError(t, err, "post-refresh resolve")
	assert.Equal(t, "new-access-token", fresh.EnvVars["TOKEN"])
	assert.NotEqual(t, "old-access-token", fresh.EnvVars["TOKEN"])
}

// TestReconcile_ProbeInvalidatesBrokerCache pins the other half of F1: even
// when NO refresh runs at all (a static credential; equally true of an
// oauth credential whose refresh attempt fails for a reason other than
// invalid_grant), the probe must not read a stale, previously-cached
// broker resolution. Without invalidating first, a credential a human
// already fixed out-of-band (or, for oauth, one the broker cached before it
// actually expired) would be probed with the WRONG value -- failing in the
// unsafe direction: a live credential could be wrongly reported rejected and
// a card wrongly opened.
func TestReconcile_ProbeInvalidatesBrokerCache(t *testing.T) {
	var mu sync.Mutex
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		if gotAuth == "token new-token" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"login":"demo"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	installRedirectClient(t, srv)

	cred := staticCredential()
	sess, class, suid, mcp, cur := baseObjects(cred, "github-pat")
	secretName := spiceboxv1alpha1.PassthroughCredentialSecretName(sessionName)
	oldSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
		Data:       map[string][]byte{credName: []byte("old-token")},
	}

	ctx := context.Background()
	c, r, _ := newReconciler(t, sess, class, suid, mcp, cur, oldSecret)

	// Warm the cache with the OLD value, exactly as an earlier, unrelated
	// resolve of this same static credential (e.g. a prior tool dispatch)
	// would.
	desc := spiceboxv1alpha1.CredentialDescriptor{
		Source: spiceboxv1alpha1.CredentialSource{Type: "static", Namespace: ns, Name: secretName, Key: credName},
		Inject: spiceboxv1alpha1.CredentialInjection{Header: &spiceboxv1alpha1.HeaderInjection{Name: "Authorization"}},
	}
	warmed, err := r.Broker.Resolve(ctx, broker.Request{Credentials: []spiceboxv1alpha1.CredentialDescriptor{desc}})
	require.NoError(t, err, "warm-up resolve")
	require.Equal(t, "old-token", warmed.HTTPHeaders["Authorization"])

	// The credential is fixed out from under the warm cache entry -- the
	// CURRENT value is fine; only the broker's cache is stale.
	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, &sec))
	sec.Data[credName] = []byte("new-token")
	require.NoError(t, c.Update(ctx, &sec))

	_, err = reconcileOnce(t, r)
	require.NoError(t, err, "Reconcile")

	got := getCUR(t, c)
	// With the fix, the probe sees the CURRENT ("new-token") value and it
	// verifies fine: Refused/CredentialLive, no card. Without it, the probe
	// would present the stale cached "old-token", the server would 401, and
	// the reconciler would wrongly open a card for a credential that is
	// actually fine -- exactly the unsafe-direction failure F1 describes.
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, got.Status.Phase, "status=%+v", got.Status)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationCredentialLive, got.Status.Determination)
	assert.Empty(t, got.Status.InteractionRef, "a live credential must never open a card")
	mu.Lock()
	assert.Equal(t, "token new-token", gotAuth, "the probe must present the CURRENT token, not the stale cached one")
	mu.Unlock()
}

// TestReconcile_IsIdempotent reconciles the same request twice and asserts
// the second pass leaves status byte-identical -- the SSA-idempotency
// constraint (AGENTS.md) expressed as a test. Open is deliberately excluded
// from IsCredentialUpdateRequestTerminal (the meta tool's poll loop must keep
// waiting past it), so this pins that the reconciler routes a second pass to
// reconcileOpen (a no-op when nothing changed) rather than re-running the
// determination pipeline (which would re-probe, re-refresh, etc.).
func TestReconcile_IsIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // Rejected -> Open
	}))
	t.Cleanup(srv.Close)
	installRedirectClient(t, srv)

	cred := staticCredential()
	sess, class, suid, mcp, cur := baseObjects(cred, "github-pat")
	c, r, rb := newReconciler(t, sess, class, suid, mcp, cur, staticSecret())

	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "first Reconcile")
	first := getCUR(t, c)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, first.Status.Phase)
	firstResolveCalls := rb.resolveCalls.Load()

	_, err = reconcileOnce(t, r)
	require.NoError(t, err, "second Reconcile")
	second := getCUR(t, c)

	assert.Equal(t, first.Status, second.Status, "status must be byte-identical across the two reconciles")
	assert.Equal(t, firstResolveCalls, rb.resolveCalls.Load(),
		"second reconcile must not re-probe -- it must route to reconcileOpen, not re-run the determination pipeline")
}

// TestReconcile_ProcessesPendingPhase pins F3: CredentialUpdateRequestPhasePending
// is explicitly pinned NON-terminal (IsCredentialUpdateRequestTerminal), and
// a future creator may well stamp it on a freshly-created request.
// The idempotency guard must NOT treat Pending as "already decided" -- a
// request starting from Pending must still be processed to a real verdict,
// not silently left there forever with no trace.
func TestReconcile_ProcessesPendingPhase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // Rejected -> Open, card published
	}))
	t.Cleanup(srv.Close)
	installRedirectClient(t, srv)

	cred := staticCredential()
	sess, class, suid, mcp, cur := baseObjects(cred, "github-pat")
	cur.Status.Phase = spiceboxv1alpha1.CredentialUpdateRequestPhasePending // explicit non-terminal starting phase

	c, r, _ := newReconciler(t, sess, class, suid, mcp, cur, staticSecret())
	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "Reconcile")

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, got.Status.Phase,
		"Pending must not be treated as already-decided; status=%+v", got.Status)
}

// TestIsKnownIdentityKind pins the slice-3 gate directly against all three
// documented IdentityKind spellings (see ResolvedCredentialRef.IdentityKind's
// doc): AgentIdentity, UserIdentity, and SessionUserIdentity are ALL allowed
// through to Determine now (slice 1's AgentIdentity-only refusal is retired
// -- Task 1 modelled who may approve replacing an agent's own shared
// credential). What must NOT regress is the ALLOWLIST shape itself: an
// empty kind still fails closed, and -- the case a careless widening most
// easily loses, per this task's brief -- so does a non-empty kind nobody
// documented. A `kind != ""` rewrite (turning the allowlist into a
// blacklist-of-just-empty-string) would pass the empty case here but wrongly
// admit "BogusIdentityKind"; that case exists specifically to catch that
// mutation.
func TestIsKnownIdentityKind(t *testing.T) {
	cases := []struct {
		name string
		kind string
		want bool
	}{
		{name: "AgentIdentity: the agent's own shared credential, now allowed (slice 3)", kind: "AgentIdentity", want: true},
		{name: "UserIdentity: user-owned, allowed (regression guard)", kind: "UserIdentity", want: true},
		{name: "SessionUserIdentity: user-owned (session projection of UserIdentity), allowed (regression guard)", kind: "SessionUserIdentity", want: true},
		{name: "empty kind: fails closed, refused (regression guard)", kind: "", want: false},
		{name: "non-empty but undocumented kind: fails closed, refused -- catches an allowlist-to-blacklist widening", kind: "BogusIdentityKind", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, credentialupdaterequest.IsKnownIdentityKind(tc.kind))
		})
	}
}

// TestReconcile_RefusesCrossNamespaceSessionRef pins F2's first guard: a
// CredentialUpdateRequest naming a session in a DIFFERENT namespace than its
// own must refuse immediately, before ever touching that namespace, its
// MCPServer, or its ask budget. Without this, a CR in namespace A naming a
// session in namespace B would resolve B's identity/credential against A's
// MCPServer and A's ask budget -- the cross-tenant confused-deputy this
// reconciler exists to prevent. Not reachable through any path that exists
// at HEAD (nothing creates these CRs yet), but a future creator will be.
func TestReconcile_RefusesCrossNamespaceSessionRef(t *testing.T) {
	const otherNS = "other-ns"
	cred := staticCredential()

	// The REAL session (and everything it needs to resolve an identity) lives
	// in otherNS, name-and-owner-matching what SessionRef/ownerRef claim --
	// so if the namespace guard were bypassed, resolution would proceed
	// (successfully past resolveIdentity) instead of failing for some OTHER,
	// coincidental reason. That is what makes the Determination assertion
	// below discriminating: without the guard this ends in "Unverified"
	// (having actually resolved otherNS's session), not "NoCredential".
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: sessionName, Namespace: otherNS, UID: sessionUID},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: className},
	}
	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: className, Namespace: otherNS},
		Spec:       spiceboxv1alpha1.AgentClassSpec{IdentityMode: spiceboxv1alpha1.IdentityModeUserPassthrough},
	}
	suid := &spiceboxv1alpha1.SessionUserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: sessionName, Namespace: otherNS},
		Spec: spiceboxv1alpha1.SessionUserIdentitySpec{
			AgentSession: sessionName, UserIdentity: "demo-user", Subject: "user:demo-user",
			Credentials: []spiceboxv1alpha1.AgentCredential{cred},
		},
	}
	// The MCPServer lives in the CR's OWN namespace (ns) -- ResolveOrigin
	// always resolves the origin against cr.Namespace, never SessionRef's.
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: mcpName, Namespace: ns},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: credName, Provider: "tailscale-authkey"},
		},
	}
	cur := &spiceboxv1alpha1.CredentialUpdateRequest{
		ObjectMeta: metav1.ObjectMeta{Name: curName, Namespace: ns, OwnerReferences: []metav1.OwnerReference{ownerRef()}},
		Spec: spiceboxv1alpha1.CredentialUpdateRequestSpec{
			SessionRef:  spiceboxv1alpha1.NamespacedRef{Namespace: otherNS, Name: sessionName}, // mismatched vs cur.Namespace (ns)
			Origin:      "mcpserver/" + mcpName,
			ToolName:    "demo_tool",
			RequestedBy: identity.Subject("user:demo-user"),
		},
	}

	c, r, rb := newReconciler(t, sess, class, suid, mcp, cur)
	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "Reconcile")

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, got.Status.Phase, "status=%+v", got.Status)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationNoCredential, got.Status.Determination)
	assert.Contains(t, got.Status.Reason, "does not match this request's own", "reason must name the namespace mismatch specifically")
	assert.Equal(t, int32(0), rb.resolveCalls.Load(), "must refuse before ever touching the broker")
}

// TestReconcile_RefusesUnownedSession pins F2's second guard: SessionRef
// naming the right namespace and name is not enough -- the CR's own
// ownerReference UID must actually match the resolved AgentSession's UID.
// Neither an absent ownerReference nor one pointing at a stale/forged UID
// (e.g. a session that was deleted and recreated under the same name) may
// resolve that session's identity.
func TestReconcile_RefusesUnownedSession(t *testing.T) {
	cases := []struct {
		name  string
		owner []metav1.OwnerReference
	}{
		{name: "no ownerReferences at all", owner: nil},
		{
			name: "ownerReference present but UID does not match the real AgentSession",
			owner: []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(), Kind: "AgentSession",
				Name: sessionName, UID: "wrong-uid",
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cred := staticCredential()
			sess, class, suid, mcp, cur := baseObjects(cred, "tailscale-authkey")
			cur.OwnerReferences = tc.owner

			c, r, rb := newReconciler(t, sess, class, suid, mcp, cur, staticSecret())
			_, err := reconcileOnce(t, r)
			require.NoError(t, err, "Reconcile")

			got := getCUR(t, c)
			assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, got.Status.Phase, "status=%+v", got.Status)
			assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationNoCredential, got.Status.Determination)
			assert.Equal(t, int32(0), rb.resolveCalls.Load(), "must refuse before ever touching the broker")
		})
	}
}

// TestReconcile_AgentIdentityReachesDetermination proves the slice-3 gate
// widening end to end (not just at the pure-function level above): a session
// running in identityMode=agent, backed by an AgentIdentity, now runs the
// FULL determination pipeline for its own shared credential -- probe,
// Determine, Open -- instead of being short-circuited to Refused/
// NotUpdatable before ever reaching Determine. If the slice-1 gate were
// reinstated (or the widening reverted to exclude AgentIdentity again), this
// would go back to Refused/NotUpdatable with the probe server never hit --
// exactly the regression this test is built to catch.
//
// Task 1 is what makes this safe to allow through: the `agentidentity`
// SpiceDB object's `update_credential = editor + platform->can_admin` now
// models WHO may approve replacing it. Reaching Open is where this
// reconciler's job ends: channelsd's CredentialUpdateWatcher then routes the
// card to the humans who hold that permission -- the monitoring channel, plus
// the thread when the turn's author passes the check (Task 3,
// pkg/channels/channelsd/pipeline/credential_update.go) -- and identityd authorizes
// the click. None of that is observable from here.
func TestReconcile_AgentIdentityReachesDetermination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // definitive rejection -> Open
	}))
	t.Cleanup(srv.Close)
	installRedirectClient(t, srv)

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: sessionName, Namespace: ns, UID: sessionUID},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: className, AgentIdentity: "demo-agent-identity"},
	}
	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: className, Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentClassSpec{IdentityMode: spiceboxv1alpha1.IdentityModeAgent},
	}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent-identity", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: credName, Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "demo-agent-secret", Key: credName},
				},
			}},
		},
	}
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: mcpName, Namespace: ns},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: credName, Provider: "github-pat"},
		},
	}
	cur := &spiceboxv1alpha1.CredentialUpdateRequest{
		// CreationTimestamp stamped explicitly, same reason baseObjects' fixture
		// does: reconcileOpen's idle-TTL math is computed from it, and the fake
		// client does not auto-stamp it the way a real apiserver would. This
		// test asserts an Open transition, so it needs a realistic "just
		// created" time.
		ObjectMeta: metav1.ObjectMeta{Name: curName, Namespace: ns, OwnerReferences: []metav1.OwnerReference{ownerRef()}, CreationTimestamp: metav1.Now()},
		Spec: spiceboxv1alpha1.CredentialUpdateRequestSpec{
			SessionRef:  spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: sessionName},
			Origin:      "mcpserver/" + mcpName,
			ToolName:    "demo_tool",
			RequestedBy: identity.Subject("user:demo-user"),
		},
	}
	agentSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent-secret", Namespace: ns},
		Data:       map[string][]byte{credName: []byte("tok-123")},
	}

	c, r, _ := newReconciler(t, sess, class, ai, mcp, cur, agentSecret)
	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "Reconcile")

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, got.Status.Phase, "status=%+v", got.Status)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified, got.Status.Determination,
		"the provider's live rejection must be what decides this -- not a short-circuit on IdentityKind")
	require.NotNil(t, got.Status.ResolvedCredential)
	assert.Equal(t, "AgentIdentity", got.Status.ResolvedCredential.IdentityKind)
	require.NotNil(t, got.Status.CredentialSecretRef, "Open must record the backing Secret ref, same as any other credential")
}

// observedAuthFailure is the corroboration observation the RUNNER records on
// AgentSession.status when it has itself seen calls at origin fail the way the
// provider's declared authFailure: shape says its credentials fail. The
// reconciler only ever reads its PRESENCE -- the classification already
// happened at record time, in the one process that saw the upstream outcome --
// so Count/ObservedAt are realistic filler rather than inputs to the verdict.
func observedAuthFailure(origin string) spiceboxv1alpha1.CredentialAuthFailure {
	now := metav1.Now()
	return spiceboxv1alpha1.CredentialAuthFailure{Origin: origin, Count: 3, ObservedAt: &now}
}

// corroborableProvider is a real catalog entry that declares an authFailure:
// block ("httpStatuses: [401]") and NO verify: block. Both halves matter, and
// no other shipped provider has both:
//
//   - no verify: means VerifyCredential returns unsupported without any HTTP
//     call, which is the ONLY branch where corroboration decides the verdict;
//   - an authFailure: block is what makes an observation for this origin
//     LEGITIMATELY producible at all. The runner refuses to record one for a
//     provider that declares no shape (AuthFailureOrigins.RecordProvider), so a
//     test that paired an observation with such a provider would be asserting
//     against a state honest operation cannot reach.
const corroborableProvider = "oauth-mcp"

// uncorroborableProvider is a real catalog entry that declares NEITHER a
// verify: block NOR an authFailure: block — "no corroboration is available for
// this credential", a deliberate per-provider catalog decision (see the essay
// in providers/anthropic-api-key.yaml on why declaring a forgeable shape would
// be a phishing primitive rather than a weak signal).
const uncorroborableProvider = "tailscale-authkey"

// TestReconcile_CorroborationOpensTheUnverifiedTier is the trio that makes the
// unverified tier reachable at all, and bounds WHERE it is reachable. Every row
// probes a provider with no verify: block, so VerifyCredential returns
// unsupported without an HTTP call and corroboration is the only evidence
// there is.
//
// They are one table on purpose. The positive alone would pass against an
// implementation that hardcoded Corroborated=true; the no-observation row alone
// is the refuse-everything-unverifiable behavior that predates corroboration;
// and the third row is the one that pins the gate to the provider's DECLARED
// auth-failure shape rather than to the mere presence of a runner-written
// entry. Only the three together pin that the observation moves the verdict
// exactly where the catalog says it may.
func TestReconcile_CorroborationOpensTheUnverifiedTier(t *testing.T) {
	cases := []struct {
		name              string
		providerID        string
		observations      []spiceboxv1alpha1.CredentialAuthFailure
		wantPhase         string
		wantDetermination string
		wantReason        string
	}{
		{
			name:              "unsupported probe WITH the platform's own observation: Open, unverified tier, RejectedUnverified",
			providerID:        corroborableProvider,
			observations:      []spiceboxv1alpha1.CredentialAuthFailure{observedAuthFailure("mcpserver/" + mcpName)},
			wantPhase:         spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedUnverified,
			wantReason:        "could not confirm",
		},
		{
			name:              "unsupported probe with NO observation: Refused, Unverified (an uncorroborated claim must never open a card)",
			providerID:        corroborableProvider,
			observations:      nil,
			wantPhase:         spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationUnverified,
			wantReason:        "no independent evidence",
		},
		{
			// The provider declares no auth-failure shape, so the runner could
			// never have written this entry through the sanctioned path
			// (AuthFailureOrigins.RecordProvider refuses it, IsAuthShaped's
			// af == nil returns false). An entry that is nonetheless present is
			// therefore either forged by a compromised runner or stale garbage,
			// and either way it must not vouch for a credential-entry form put
			// in front of a human. The operator re-derives the precondition from
			// the same catalog rather than trusting the writer to have applied
			// it.
			name:              "observation for a provider that declares NO auth-failure shape: Refused, Unverified (a shape the runner could not legitimately have observed must not vouch)",
			providerID:        uncorroborableProvider,
			observations:      []spiceboxv1alpha1.CredentialAuthFailure{observedAuthFailure("mcpserver/" + mcpName)},
			wantPhase:         spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationUnverified,
			wantReason:        "no independent evidence",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess, class, suid, mcp, cur := baseObjects(staticCredential(), tc.providerID)
			sess.Status.CredentialAuthFailures = tc.observations

			c, r, _ := newReconciler(t, sess, class, suid, mcp, cur, staticSecret())
			_, err := reconcileOnce(t, r)
			require.NoError(t, err, "Reconcile")

			got := getCUR(t, c)
			assert.Equal(t, tc.wantPhase, got.Status.Phase, "phase; status=%+v", got.Status)
			assert.Equal(t, tc.wantDetermination, got.Status.Determination, "determination")
			assert.Contains(t, got.Status.Reason, tc.wantReason,
				"the reason is what the agent reads verbatim and what the card's verdict line shows")

			if tc.wantPhase == spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen {
				// An unverified-tier open is a REAL open: it must record the same
				// backing-Secret baseline the unpark watch compares against, or the
				// card could never detect the human's fix.
				require.NotNil(t, got.Status.CredentialSecretRef, "an unverified-tier Open must record the backing Secret ref")
				assert.NotEmpty(t, got.Status.CredentialSecretObservedHash, "an unverified-tier Open must record the baseline content hash")
			}
		})
	}
}

// TestReconcile_ObservationForAnotherOriginDoesNotCorroborate is the
// scoping control for the table above. The session carries an observation, so
// the "is there any evidence at all" question answers yes -- but it is for a
// DIFFERENT origin, i.e. a different credential behind a different upstream.
// Treating it as corroboration would let one dead credential open a card for
// every other credential the same session touches.
//
// corroborableProvider specifically: with a provider that declares no
// auth-failure shape this test would still go green while asserting nothing
// about ORIGIN scoping, because the shape gate would refuse the observation
// first and the wrong-origin question would never be reached.
func TestReconcile_ObservationForAnotherOriginDoesNotCorroborate(t *testing.T) {
	sess, class, suid, mcp, cur := baseObjects(staticCredential(), corroborableProvider)
	sess.Status.CredentialAuthFailures = []spiceboxv1alpha1.CredentialAuthFailure{
		observedAuthFailure("mcpserver/some-other-upstream"),
	}

	c, r, _ := newReconciler(t, sess, class, suid, mcp, cur, staticSecret())
	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "Reconcile")

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, got.Status.Phase,
		"an observation for another origin must not open a card; status=%+v", got.Status)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationUnverified, got.Status.Determination)
}

// TestReconcile_LiveProbeBeatsCorroboration is the security-critical row of
// the verdict table, and it gets its own test because it is the one place
// corroboration must LOSE.
//
// The setup is the most suspicious-looking one the feature can produce: the
// platform itself observed auth-shaped failures at this origin, AND the agent
// is asking for a replacement. The provider nevertheless says the credential
// authenticates fine. That combination is exactly what a SCOPE problem looks
// like -- an observed 403 sitting next to a live token -- and re-entering the
// same token cannot fix it. Opening a card here would ask a human to replace a
// working credential, which is the phishing shape the whole corroboration gate
// exists to prevent.
//
// The assertion is therefore not just "Refused": it is that the determination
// stays CredentialLive and the agent is told, verbatim, that this is a
// permissions problem it must not re-ask about.
func TestReconcile_LiveProbeBeatsCorroboration(t *testing.T) {
	var probed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probed.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"demo"}`)) // the provider accepts the token
	}))
	t.Cleanup(srv.Close)
	installRedirectClient(t, srv)

	sess, class, suid, mcp, cur := baseObjects(staticCredential(), "github-pat")
	sess.Status.CredentialAuthFailures = []spiceboxv1alpha1.CredentialAuthFailure{
		observedAuthFailure("mcpserver/" + mcpName),
	}

	c, r, _ := newReconciler(t, sess, class, suid, mcp, cur, staticSecret())
	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "Reconcile")

	require.True(t, probed.Load(),
		"sanity: the live probe must actually have run, or this test proves nothing about it winning")

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, got.Status.Phase,
		"a live probe must refuse even WITH corroboration present; status=%+v", got.Status)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationCredentialLive, got.Status.Determination)
	assert.Contains(t, got.Status.Reason, "permissions or scope problem",
		"the agent must be redirected at the real problem, not merely refused")
	assert.Empty(t, got.Status.CredentialSecretRef,
		"a refusal never records an unpark baseline -- there is no card to unpark")
}

// budgetRef is the ResolvedCredentialRef every "matching" prior in
// TestReconcile_BudgetExhausted shares with the CR under reconciliation
// (IdentityKind/Namespace/Name/Credential -- see baseObjects/staticCredential).
var budgetRef = spiceboxv1alpha1.ResolvedCredentialRef{
	IdentityKind: "SessionUserIdentity",
	Namespace:    ns,
	Name:         sessionName,
	Credential:   credName,
}

// priorRequest builds a terminal, already-decided CredentialUpdateRequest
// for the budget check to List and count (or correctly ignore).
func priorRequest(name, origin, phase string, ref spiceboxv1alpha1.ResolvedCredentialRef) *spiceboxv1alpha1.CredentialUpdateRequest {
	return &spiceboxv1alpha1.CredentialUpdateRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spiceboxv1alpha1.CredentialUpdateRequestSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: sessionName},
			Origin:     origin, RequestedBy: identity.Subject("user:demo-user"),
		},
		Status: spiceboxv1alpha1.CredentialUpdateRequestStatus{Phase: phase, ResolvedCredential: ref.DeepCopy()},
	}
}

// TestReconcile_BudgetExhausted proves the ask budget is keyed on the
// RESOLVED CREDENTIAL, not the origin: two prior terminal requests for the
// same credential (even via a DIFFERENT MCPServer origin) exhaust the
// budget, while a prior for a DIFFERENT credential (even via the SAME
// origin) must never count toward it. The probe never runs in either case
// (budget is checked before Determine) -- providerID ("tailscale-authkey")
// declares no verify: block at all, so an accidental probe would surface as
// a wrong DETERMINATION (e.g. "Unverified" instead of "BudgetExhausted"),
// not a connection error; that determination assert is what actually
// discriminates the guard, not the absence of network activity.
func TestReconcile_BudgetExhausted(t *testing.T) {
	otherRef := budgetRef
	otherRef.Credential = "other-cred"

	cases := []struct {
		name              string
		priors            []*spiceboxv1alpha1.CredentialUpdateRequest
		wantExhausted     bool
		wantDetermination string
	}{
		{
			name: "two priors for the SAME credential via DIFFERENT origins: exhausted",
			priors: []*spiceboxv1alpha1.CredentialUpdateRequest{
				priorRequest("demo-cur-prior-1", "mcpserver/"+mcpName, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, budgetRef),
				priorRequest("demo-cur-prior-2", "mcpserver/other-mcp", spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled, budgetRef),
			},
			wantExhausted:     true,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationBudgetExhausted,
		},
		{
			name: "one prior for the SAME credential plus one for a DIFFERENT credential via the SAME origin: not exhausted",
			priors: []*spiceboxv1alpha1.CredentialUpdateRequest{
				priorRequest("demo-cur-prior-1", "mcpserver/"+mcpName, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, budgetRef),
				priorRequest("demo-cur-prior-2", "mcpserver/"+mcpName, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, otherRef),
			},
			wantExhausted:     false,
			wantDetermination: spiceboxv1alpha1.CredentialUpdateDeterminationUnverified, // tailscale-authkey: no verify:, no corroboration
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cred := staticCredential()
			sess, class, suid, mcp, cur := baseObjects(cred, "tailscale-authkey")
			objs := []client.Object{sess, class, suid, mcp, cur, staticSecret()}
			for _, p := range tc.priors {
				objs = append(objs, p)
			}

			c, r, _ := newReconciler(t, objs...)
			_, err := reconcileOnce(t, r)
			require.NoError(t, err, "Reconcile")

			got := getCUR(t, c)
			assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, got.Status.Phase, "status=%+v", got.Status)
			assert.Equal(t, tc.wantDetermination, got.Status.Determination)
		})
	}
}

// TestReconcile_AgentOwnedOAuthIsRefusedAtDetermination closes the honesty gap
// nothing gated before the card published.
//
// An agent's own type=oauth credential has no value a human can paste: the
// Secret is a multi-key bundle only the OAuth ceremony can mint, and the
// ceremony a card would offer links the CLICKER's own account. The write path
// already refuses it -- but only at CLICK time, with a 409. So an admin was
// broadcast a card, opened it, was told "this credential can't be replaced
// here", and the request then EXPIRED announcing that nobody had acted. Every
// part of that sequence is a lie of exactly the kind this flow exists to
// remove, and OAuth refresh tokens really do expire, so it is reachable.
//
// Mutation sensitivity: drop `AgentOwned: ref.AgentOwned()` from the reconciler's
// credupdate.Input and this goes red at Open/RejectedRefreshDead -- the pure
// determination test alone would stay green, because Determine would still be
// correct about an input nothing gave it.
func TestReconcile_AgentOwnedOAuthIsRefusedAtDetermination(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: sessionName, Namespace: ns, UID: sessionUID},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: className, AgentIdentity: "demo-agent-identity"},
	}
	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: className, Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentClassSpec{IdentityMode: spiceboxv1alpha1.IdentityModeAgent},
	}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent-identity", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: credName, Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretRef{Name: "demo-agent-secret"},
				},
			}},
		},
	}
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: mcpName, Namespace: ns},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: credName, Provider: "github-pat"},
		},
	}
	cur := &spiceboxv1alpha1.CredentialUpdateRequest{
		ObjectMeta: metav1.ObjectMeta{Name: curName, Namespace: ns, OwnerReferences: []metav1.OwnerReference{ownerRef()}, CreationTimestamp: metav1.Now()},
		Spec: spiceboxv1alpha1.CredentialUpdateRequestSpec{
			SessionRef:  spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: sessionName},
			Origin:      "mcpserver/" + mcpName,
			ToolName:    "demo_tool",
			RequestedBy: identity.Subject("user:demo-user"),
		},
	}
	// A DEAD refresh token: the refresh runs, fails with invalid_grant, and
	// would otherwise be the most definitive card this flow can open
	// (RejectedRefreshDead, TierVerified). That is the strongest case for
	// asking a human -- and still the wrong thing to do here, which is why the
	// fixture uses it rather than a credential with nothing to refresh.
	agentSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent-secret", Namespace: ns},
		Data: map[string][]byte{
			"access_token":   []byte("tok-expired"),
			"refresh_token":  []byte("rt-revoked"),
			"token_endpoint": []byte("https://oauth.invalid/token"),
			"client_id":      []byte("demo-client"),
		},
	}

	c, r, _ := newReconciler(t, sess, class, ai, mcp, cur, agentSecret)
	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "Reconcile")

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, got.Status.Phase,
		"an agent's own oauth credential must be refused HERE, not broadcast and 409'd at click time; status=%+v", got.Status)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationNotUpdatable, got.Status.Determination)
	assert.Contains(t, got.Status.Reason, "re-authorized by an operator",
		"and the agent must be told what actually has to happen, not just that it was refused")
	require.NotNil(t, got.Status.ResolvedCredential)
	assert.Equal(t, spiceboxv1alpha1.IdentityKindAgentIdentity, got.Status.ResolvedCredential.IdentityKind,
		"the refusal must still record what the origin resolved to")
}
