//go:build integration

// pkg/controllers/credentialupdaterequest/integration_test.go
//
// The credential-update slice's ONE cross-process test. Everything else in
// this package (controller_test.go, unpark_test.go) drives a single
// reconciler against its own fake client; this file drives the three
// independent actors the design actually splits the work across, over ONE
// REAL apiserver:
//
//	operator   pkg/controllers/credentialupdaterequest  decides (phase -> Open)
//	channelsd  pkg/channels/channelsd/pipeline                   publishes the card,
//	                                                    stamps InteractionRef +
//	                                                    CardDelivered
//	operator   pkg/controllers/agentsession             parks/unparks the session
//
// That seam has no coverage anywhere else, and it is not incidental: the two
// halves live in different BINARIES with no shared memory, so the only thing
// connecting "the operator decided" to "a human was asked" is CR status
// observed across a watch. Two of the review findings on this reconciler were possible
// precisely because a fake client per reconciler can never exercise it --
// notably the inverted never-delivered signal (see
// TestSeam_NeverDeliveredRequestExpiresSayingSo), where an undelivered
// request expired telling the agent "nobody updated the credential" about a
// card nobody was ever shown.
//
// Cost note: every test here is serial (testenv.Shared wipes the cluster on
// each test's cleanup) and several boot a controller-runtime manager, so the
// file deliberately holds ONE full-stack scenario and drives the narrower
// cases with only the actors they actually need.
package credentialupdaterequest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentrunner "github.com/authzed/openagentprimitives/pkg/agent/runner"
	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	channelsdpipeline "github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/credentialupdaterequest"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker/inproc"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	lifecyclekind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"

	// Registers MCP + CLI + toolspec authkinds so the AgentSession
	// reconciler's passthrough gate can compute RequiredCredentials from the
	// class's MCPServer refs.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/loader"
)

// Fixture names for this file. Deliberately distinct from controller_test.go's
// (ns/sessionName/...) -- both files compile into the SAME test binary under
// -tags=integration, so a shared name would be a redeclaration.
const (
	itNS       = "default"
	itSubject  = "user:agent-owner@example.invalid"
	itExtID    = "U0DEMO"
	itEmail    = "agent-owner@example.invalid"
	itCredName = "demo-upstream-token"
	itProvider = "github-pat"

	// itUnverifiableProvider is a real catalog entry that declares NO verify:
	// block, so VerifyCredential returns unsupported without any HTTP call at
	// all. That is the branch where the platform's own observation is the only
	// evidence there is: without Corroborated reaching Determine, it is a
	// branch that can only ever refuse.
	//
	// It must ALSO declare an authFailure: block, and "oauth-mcp" is the one
	// shipped entry with both. The reconciler gates corroboration on the
	// provider declaring a shape at all -- an origin whose provider declares
	// none is one the runner never records for, so an entry there could not
	// have come through the classification path and must not vouch for a card.
	// Pairing an observation with a shapeless provider here would be asserting
	// against a state honest operation cannot produce.
	itUnverifiableProvider = "oauth-mcp"
)

// itPollTimeout bounds every convergence poll in this file. Generous because
// channelsd's watcher polls on a 5s ticker (CredentialUpdateWatcherInterval)
// and a manager-driven reconcile adds a cache round trip on top; the tier also
// runs at -p=4 with an apiserver per package, so a tight budget here reads as
// flakiness rather than as a real regression.
const itPollTimeout = 90 * time.Second

// ---------------------------------------------------------------------------
// Actor wiring
// ---------------------------------------------------------------------------

// itNewManager builds a controller-runtime manager against the shared envtest
// apiserver. SkipNameValidation is required: several tests in this package
// each register a controller under the same name in their own manager.
func itNewManager(t *testing.T, env *testenv.Env) ctrl.Manager {
	t.Helper()
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:         env.Scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		Controller:     ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
		LeaderElection: false,
	})
	require.NoError(t, err, "ctrl.NewManager")
	return mgr
}

func itStartManager(t *testing.T, mgr ctrl.Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(memory.WithSystemApproval(context.Background(), "test"))
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx), "cache sync")
}

// itStartCredentialUpdateOperator wires the CredentialUpdateRequest reconciler
// into a real manager -- including its Watches(&Secret{}) mapping, which is the
// mechanism the unpark depends on and which a direct-Reconcile test cannot
// exercise at all. idleTTL is the caller's, so the expiry path can be driven
// deliberately (short) or kept out of the way (long).
func itStartCredentialUpdateOperator(t *testing.T, env *testenv.Env, idleTTL time.Duration) {
	t.Helper()
	mgr := itNewManager(t, env)
	r := &credentialupdaterequest.Reconciler{
		Client:  mgr.GetClient(),
		Broker:  inproc.New(mgr.GetClient()),
		IdleTTL: idleTTL,
	}
	require.NoError(t, r.SetupWithManager(mgr), "credentialupdaterequest.SetupWithManager")
	itStartManager(t, mgr)
}

// itStubRunnerFactory stands in for the pod runner factory by creating an
// inert Pod under the name the reconciler expects.
//
// A factory that creates NOTHING (the shape passthrough_envtest_test.go uses)
// is not sufficient here: AgentSession.Reconcile stops at the runner-spawn step
// when it cannot find RunnerPodName -- it stamps RunnerReady=False/RunnerCreating
// and returns, several hundred lines BEFORE the credential-update park. The park
// would then never run and the test would fail for a reason that has nothing to
// do with credential updates. The pod only has to EXIST and be non-terminal;
// nothing in this file asserts anything about it.
type itStubRunnerFactory struct{ c client.Client }

func (f itStubRunnerFactory) Start(ctx context.Context, sess *spiceboxv1alpha1.AgentSession,
	_ *spiceboxv1alpha1.AgentClass, _ agentsession.StartOpts) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: agentsession.RunnerPodName(sess), Namespace: sess.Namespace},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "runner", Image: "agentprimitives-runner:dev"}},
		},
	}
	if err := f.c.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (f itStubRunnerFactory) Stop(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: agentsession.RunnerPodName(sess), Namespace: sess.Namespace}}
	return client.IgnoreNotFound(f.c.Delete(ctx, pod))
}

// ObservedName returns "" — the Pod this stub creates exists only so the
// AgentSession reconciler can observe a non-terminal runner and proceed to the
// credential-update park. Session status deliberately does not surface it, the
// same way the in-process e2e factory's placeholder Pod is not surfaced.
func (f itStubRunnerFactory) ObservedName(_ *spiceboxv1alpha1.AgentSession) string {
	return ""
}

// itStartSessionOperator wires the AgentSession reconciler into a real manager
// and returns the memory the lifecycle log lives in, so the caller can seed the
// session's transition log.
//
// LifecycleMemory is wired ON PURPOSE. Without it foldLifecycle returns the
// zero state, derivePhase projects the bootstrap Pending, and reconcilePhase
// preserves whatever phase is already on the CR -- which would leave the
// session stuck at AwaitingCredentials after the unpark and make the unpark
// assertion pass for a reason unrelated to the code under test.
func itStartSessionOperator(t *testing.T, env *testenv.Env) memory.Memory {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	mgr := itNewManager(t, env)
	r := &agentsession.Reconciler{
		Client:          mgr.GetClient(),
		APIReader:       mgr.GetAPIReader(),
		Tokens:          tokens.NewRegistry(),
		Memory:          mem,
		LifecycleMemory: mem,
		RunnerFactory:   itStubRunnerFactory{c: env.Client},
	}
	require.NoError(t, r.SetupWithManager(mgr), "agentsession.SetupWithManager")
	itStartManager(t, mgr)
	return mem
}

// itCapturingPublisher stands in for channelsd's NATS connection, recording
// every envelope the watcher publishes. Guarded because the watcher runs on its
// own goroutine while the test polls.
type itCapturingPublisher struct {
	mu       chan struct{} // 1-buffered, used as a mutex
	messages []itCaptured
}

type itCaptured struct {
	subject string
	data    []byte
}

func itNewPublisher() *itCapturingPublisher {
	p := &itCapturingPublisher{mu: make(chan struct{}, 1)}
	p.mu <- struct{}{}
	return p
}

func (p *itCapturingPublisher) publish(subject string, data []byte) error {
	<-p.mu
	defer func() { p.mu <- struct{}{} }()
	p.messages = append(p.messages, itCaptured{subject: subject, data: append([]byte(nil), data...)})
	return nil
}

// interactionRequests decodes every KindInteractionRequest envelope captured so
// far. Returning the decoded payloads (not the raw envelopes) keeps the
// assertions about what a HUMAN would see.
func (p *itCapturingPublisher) interactionRequests(t *testing.T) []channelevents.InteractionRequestPayload {
	t.Helper()
	<-p.mu
	defer func() { p.mu <- struct{}{} }()
	var out []channelevents.InteractionRequestPayload
	for _, m := range p.messages {
		var env channelevents.Envelope
		if err := json.Unmarshal(m.data, &env); err != nil {
			continue
		}
		if env.Kind != channelevents.KindInteractionRequest {
			continue
		}
		var payload channelevents.InteractionRequestPayload
		require.NoError(t, json.Unmarshal(env.Payload, &payload), "decode interaction_request payload")
		out = append(out, payload)
	}
	return out
}

// itStartChannelsd runs channelsd's CredentialUpdateWatcher against the same
// apiserver, on its own goroutine, exactly as the channelsd binary does. This
// is the "process B" half of the seam: it observes phase=Open through the API
// and nothing else.
func itStartChannelsd(t *testing.T, env *testenv.Env) *itCapturingPublisher {
	t.Helper()
	pub := itNewPublisher()
	w := &channelsdpipeline.CredentialUpdateWatcher{
		K8s:             env.Client,
		LinkSigner:      passthroughlink.New([]byte("integration-test-signing-key-not-for-production")),
		ExternalBaseURL: func() string { return "https://agent.example.invalid" },
		NATSPublish:     pub.publish,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Run(ctx)
	return pub
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// itRejectingProvider points the credential probe at a server that answers 401
// for every request, so Determine lands on RejectedVerified (TierVerified) and
// the request opens. Returns nothing: the redirect is installed process-wide
// for the test's lifetime.
func itRejectingProvider(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Bad credentials"))
	}))
	t.Cleanup(srv.Close)
	installRedirectClient(t, srv)
}

// itAcceptingProvider is itRejectingProvider's twin for the refusal path: the
// provider says the token still authenticates, so Determine must refuse
// CredentialLive and no card may ever be built.
func itAcceptingProvider(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"login":"demo"}`))
	}))
	t.Cleanup(srv.Close)
	installRedirectClient(t, srv)
}

func itEnsureIdentitiesNamespace(t *testing.T, ctx context.Context, c client.Client) {
	t.Helper()
	n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.IdentitiesNamespace}}
	if err := c.Create(ctx, n); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "create identities namespace")
	}
}

// itMCPServer is the upstream the failing tool belongs to: one credential, one
// catalog provider. spec.origin on the request is "mcpserver/<name>".
func itMCPServer(name string) *spiceboxv1alpha1.MCPServer {
	return itMCPServerWithProvider(name, itProvider)
}

// itMCPServerWithProvider is itMCPServer with the catalog provider chosen by
// the caller. The provider is what decides the operator's live re-probe: the
// default (itProvider) declares a verify: block and therefore reaches a
// DEFINITIVE verdict, while itUnverifiableProvider declares none and lands on
// unsupported -- the branch where corroboration is the only thing that can
// open a card.
func itMCPServerWithProvider(name, providerID string) *spiceboxv1alpha1.MCPServer {
	return &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: itNS},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://upstream.example.invalid/mcp"},
			Tools:  []spiceboxv1alpha1.MCPServerTool{},
			Auth:   spiceboxv1alpha1.MCPServerAuth{Credential: itCredName, Provider: providerID},
		},
	}
}

// itClass is a userPassthrough AgentClass. This file's scenarios exercise the
// userPassthrough path specifically -- it is what makes the resolved
// credential a SessionUserIdentity one -- not because any other mode is
// gated out: since slice 3, IsKnownIdentityKind admits AgentIdentity too
// (see controller_test.go's TestReconcile_AgentIdentityReachesDetermination
// for that path's own coverage).
func itClass(name string, mcpNames ...string) *spiceboxv1alpha1.AgentClass {
	refs := make([]spiceboxv1alpha1.AgentClassMCPServerRef, 0, len(mcpNames))
	for _, m := range mcpNames {
		refs = append(refs, spiceboxv1alpha1.AgentClassMCPServerRef{Name: m, Ref: m})
	}
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: itNS},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			IdentityMode: spiceboxv1alpha1.IdentityModeUserPassthrough,
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic", Name: "claude-opus-4-7",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns: 50, MaxTokens: 100000,
				MaxDuration: metav1.Duration{Duration: time.Hour},
			},
			MCPServers: refs,
		},
	}
}

func itMarkClassValid(t *testing.T, ctx context.Context, c client.Client, ac *spiceboxv1alpha1.AgentClass) {
	t.Helper()
	ac.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentClassConditionValid,
		Status:             metav1.ConditionTrue,
		Reason:             spiceboxv1alpha1.ReasonAllReferencesResolve,
		LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, c.Status().Update(ctx, ac), "stamp AgentClass %s Valid=True", ac.Name)
}

// itSession is a channel-attached session started by a known human. All three
// started-by annotations are set: channelsd builds the card's recipient from
// them, and an empty canonical id is one of the watcher's silent skip paths.
func itSession(name, class string, withChannel bool) *spiceboxv1alpha1.AgentSession {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: itNS,
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: itSubject,
				spiceboxv1alpha1.AnnotationStartedByExternalID:  itExtID,
				spiceboxv1alpha1.AnnotationStartedByEmail:       itEmail,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  class,
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "do the task"},
		},
	}
	if withChannel {
		sess.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{
			Name: "demo-channel", Kind: "slack",
			Key:               "thread:C0DEMO:1700000000.000100",
			Capabilities:      []string{"text"},
			NATSSubjectPrefix: "ap.session." + itNS + "." + name,
		}
	}
	return sess
}

// itSessionUserIdentity is the per-session projection the AgentSession
// reconciler would otherwise build. Tests that do NOT run the AgentSession
// reconciler create it directly; the one that does must NOT, since the
// passthrough gate server-side-applies its own.
func itSessionUserIdentity(sessName string, creds ...spiceboxv1alpha1.AgentCredential) *spiceboxv1alpha1.SessionUserIdentity {
	return &spiceboxv1alpha1.SessionUserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: sessName, Namespace: itNS},
		Spec: spiceboxv1alpha1.SessionUserIdentitySpec{
			AgentSession: sessName,
			UserIdentity: useridentity.NameForSubject(itSubject),
			Subject:      itSubject,
			Credentials:  creds,
		},
	}
}

func itStaticCredential(name string) spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{
		Name: name, Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "unused-master", Key: name},
		},
	}
}

// itProjectedSecret is the per-session Secret a userPassthrough session's
// static credentials are projected into -- the value the broker resolves and
// the reconciler hashes as its unpark baseline.
func itProjectedSecret(sessName string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spiceboxv1alpha1.PassthroughCredentialSecretName(sessName),
			Namespace: itNS,
		},
		Data: data,
	}
}

// itRequest builds a CredentialUpdateRequest owned by the LIVE session (its
// real, apiserver-assigned UID -- resolveIdentity's ownedBySession check
// compares UIDs, and a fabricated one is refused).
func itRequest(name string, sess *spiceboxv1alpha1.AgentSession, origin string) *spiceboxv1alpha1.CredentialUpdateRequest {
	tv := true
	return &spiceboxv1alpha1.CredentialUpdateRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: itNS,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:               "AgentSession",
				Name:               sess.Name,
				UID:                sess.UID,
				Controller:         &tv,
				BlockOwnerDeletion: &tv,
			}},
		},
		Spec: spiceboxv1alpha1.CredentialUpdateRequestSpec{
			SessionRef:  spiceboxv1alpha1.NamespacedRef{Namespace: itNS, Name: sess.Name},
			Origin:      origin,
			ToolName:    "demo_upstream_create_issue",
			Why:         "the last three calls came back 401 Unauthorized",
			RequestedBy: spiceboxv1alpha1.StartedBySubject(sess),
		},
	}
}

// itGetRequest re-reads a CredentialUpdateRequest by name.
func itGetRequest(t *testing.T, c client.Client, name string) *spiceboxv1alpha1.CredentialUpdateRequest {
	t.Helper()
	var got spiceboxv1alpha1.CredentialUpdateRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: itNS, Name: name}, &got),
		"get CredentialUpdateRequest %s", name)
	return &got
}

// itPhaseReaches polls until the named request's phase equals want.
func itPhaseReaches(t *testing.T, c client.Client, name, want string) {
	t.Helper()
	var last string
	ok := func() bool {
		var got spiceboxv1alpha1.CredentialUpdateRequest
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: itNS, Name: name}, &got); err != nil {
			return false
		}
		last = got.Status.Phase
		return got.Status.Phase == want
	}
	deadline := time.Now().Add(itPollTimeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(testfixtures.PollInterval)
	}
	t.Fatalf("CredentialUpdateRequest %s never reached phase %q within %v; last phase was %q",
		name, want, itPollTimeout, last)
}

// ---------------------------------------------------------------------------
// 1 + 2: the full seam, park, and unpark
// ---------------------------------------------------------------------------

// TestSeam_OperatorOpensChannelsdPublishesSessionParksThenUnparks is the one
// test that runs all three actors at once, and the only place the
// operator->channelsd handoff is exercised at all.
//
// It deliberately does NOT hand channelsd anything the operator did not write
// to the API: the watcher gets its own client and its own goroutine, so a
// regression that leaves phase/resolvedCredential unwritten (or writes them in
// a second, later status update the watcher's List predates) fails here and
// nowhere else.
//
// The unpark half is equally end-to-end: the human's fix is applied to the
// MASTER Secret in the identities namespace, and the AgentSession reconciler's
// own projection is what carries the new value into the per-session Secret the
// CredentialUpdateRequest recorded. Patching the per-session Secret directly
// would have tested the reconciler against a value its own sibling controller
// overwrites on the next pass.
func TestSeam_OperatorOpensChannelsdPublishesSessionParksThenUnparks(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	itRejectingProvider(t)

	const (
		sessName  = "seam-sess"
		className = "seam-cls"
		mcpName   = "seam-mcp"
		curName   = "seam-cur"
	)

	itEnsureIdentitiesNamespace(t, ctx, env.Client)

	// The human's real credential, in the identities namespace: this is what a
	// human edits, and what the session's projection reads.
	masterSecretName := useridentity.NameForSubject(itSubject) + "-" + itCredName
	master := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: masterSecretName, Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		Data:       map[string][]byte{"token": []byte("dead-token")},
	}
	require.NoError(t, env.Client.Create(ctx, master), "create master Secret")

	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: useridentity.NameForSubject(itSubject)},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject: itSubject,
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: itCredName, Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: masterSecretName, Key: "token"},
				},
			}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ui), "create UserIdentity")

	require.NoError(t, env.Client.Create(ctx, itMCPServer(mcpName)), "create MCPServer")
	ac := itClass(className, mcpName)
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	itMarkClassValid(t, ctx, env.Client, ac)

	sess := itSession(sessName, className, true)
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Start all three actors only once the fixtures exist, so the first
	// reconcile already sees a resolvable world.
	mem := itStartSessionOperator(t, env)

	// Seed the session's durable transition log as a live, turning session:
	// the request_credential_update tool blocks a RUNNING runner in-process, so
	// this is the state the park actually interrupts. It is also what makes the
	// unpark observable -- see itStartSessionOperator's LifecycleMemory note.
	var live spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &live), "re-read session for UID")
	itSeedRunning(t, mem, sessName, string(live.UID))

	// The passthrough gate must clear before anything else is meaningful: the
	// session's credential is PRESENT (that is the whole premise -- it is
	// present and dead), so the identity gate must not be what parks it.
	itAwaitPassthroughReady(t, env, sessName)

	itStartCredentialUpdateOperator(t, env, 10*time.Minute)
	pub := itStartChannelsd(t, env)

	// The agent's ask.
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &live), "re-read session before request")
	require.NoError(t, env.Client.Create(ctx, itRequest(curName, &live, "mcpserver/"+mcpName)), "create request")

	// --- operator decides -------------------------------------------------
	itPhaseReaches(t, env.Client, curName, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen)
	opened := itGetRequest(t, env.Client, curName)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified, opened.Status.Determination,
		"a provider that answers 401 is a definitive rejection")
	require.NotNil(t, opened.Status.ResolvedCredential, "an Open request must record what the origin resolved to")
	assert.Equal(t, "SessionUserIdentity", opened.Status.ResolvedCredential.IdentityKind)
	assert.Equal(t, itCredName, opened.Status.ResolvedCredential.Credential)
	require.NotNil(t, opened.Status.CredentialSecretRef, "the Open transition must record the backing Secret")
	assert.Equal(t, spiceboxv1alpha1.PassthroughCredentialSecretName(sessName), opened.Status.CredentialSecretRef.Name)
	assert.NotEmpty(t, opened.Status.CredentialSecretObservedHash, "and its content baseline")

	// --- channelsd publishes, in its own process -------------------------
	testfixtures.Eventually(t, itPollTimeout, func() bool {
		return itGetRequest(t, env.Client, curName).Status.InteractionRef != ""
	})
	delivered := itGetRequest(t, env.Client, curName)
	assert.NotEmpty(t, delivered.Status.InteractionRef,
		"channelsd must stamp the published card's requestRef back onto the CR")
	cardCond := apimeta.FindStatusCondition(delivered.Status.Conditions,
		spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered)
	require.NotNil(t, cardCond, "CardDelivered must be stamped alongside InteractionRef")
	assert.Equal(t, metav1.ConditionTrue, cardCond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonCredentialUpdateCardDelivered, cardCond.Reason)

	cards := pub.interactionRequests(t)
	require.Len(t, cards, 1, "exactly one card, published once")
	card := cards[0]
	assert.Equal(t, categories.CredentialUpdate, card.Category)
	assert.Equal(t, delivered.Status.InteractionRef, card.RequestRef,
		"the recorded ref must be the ref of the card that was actually sent")
	assert.Equal(t, delivered.Status.Reason, card.Lead,
		"the card's verdict line is the operator's platform-authored reason")
	assert.Contains(t, card.Body, "the last three calls came back 401 Unauthorized",
		"the agent's own why reaches the card, attributed")
	require.Len(t, card.Actions, 1, "one action: the credential-entry link")
	assert.NotEmpty(t, card.Actions[0].URL)
	require.NotNil(t, card.Audience.Requester, "the card is addressed to the credential's owner")
	assert.Equal(t, "slack", string(card.Audience.Requester.Kind),
		"the recipient carries the session's own channel kind, or the send is silently dropped")

	// REGRESSION GUARD for slice 3. A person's own credential must keep going
	// to that person and nowhere else -- in particular it must NOT be broadcast
	// to the monitoring channel the agent-owned route added. Widening the
	// routing to a standing admin surface is exactly the kind of change that
	// quietly turns a private DM about someone's personal token into a
	// cluster-wide announcement, and this file is the only place both routes
	// are driven by the same real channelsd process.
	assert.Empty(t, pub.monitoringEvents(t),
		"a user-owned credential must never be broadcast to the platform's monitoring channel")

	// --- the session parks -------------------------------------------------
	itAwaitCredentialUpdatePending(t, env, sessName, metav1.ConditionTrue)
	parked := itGetSession(t, env.Client, sessName)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, parked.Status.Phase,
		"an Open request parks the session")

	// --- the human fixes it: master Secret, not the projection -------------
	var liveMaster corev1.Secret
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(master), &liveMaster), "re-read master Secret")
	liveMaster.Data["token"] = []byte("freshly-pasted-token")
	require.NoError(t, env.Client.Update(ctx, &liveMaster), "human replaces the credential")
	// Nudge the AgentSession reconciler so its projection re-runs: it watches
	// UserIdentity (sessionsForUserIdentityChange), not Secrets.
	itTouchUserIdentity(t, ctx, env.Client, ui.Name)

	itPhaseReaches(t, env.Client, curName, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled)
	fulfilled := itGetRequest(t, env.Client, curName)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified, fulfilled.Status.Determination,
		"the ORIGINAL verdict that justified the card is preserved through the fulfilment")
	assert.Contains(t, fulfilled.Status.Reason, "Retry your call")

	// --- and the session unparks -------------------------------------------
	itAwaitCredentialUpdatePending(t, env, sessName, metav1.ConditionFalse)
	unparked := itGetSession(t, env.Client, sessName)
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, unparked.Status.Phase,
		"with no Open request left, the session must leave the credential-update park")
	cond := apimeta.FindStatusCondition(unparked.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending)
	require.NotNil(t, cond)
	assert.Equal(t, spiceboxv1alpha1.ReasonCredentialUpdateResolved, cond.Reason)

	// Still exactly one card: the fulfilled request must not be re-published.
	assert.Len(t, pub.interactionRequests(t), 1, "no second card for a request that already had one")
}

// itSeedRunning appends the transition-log events a session that has actually
// booted and claimed a runner would carry, so derivePhase projects Running.
func itSeedRunning(t *testing.T, mem memory.Memory, sessName, uid string) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	scope := memory.Scope{Kind: "session", ID: itNS + "/" + sessName}
	for i, ev := range []lifecyclecore.Event{
		lifecyclecore.SettingsAccepted{},
		lifecyclecore.RunnerClaimed{},
	} {
		require.NoError(t, lifecyclekind.Append(ctx, mem, scope, ev, time.Now().UTC(),
			lifecyclekind.OrderKey{
				Seq:        channelevents.PackSeq(0, i),
				Region:     string(lifecyclecore.RegionRunner),
				SessionUID: uid,
			}), "seed lifecycle event %T", ev)
	}
}

func itGetSession(t *testing.T, c client.Client, name string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: itNS, Name: name}, &got),
		"get AgentSession %s", name)
	return &got
}

// itAwaitPassthroughReady waits for the identity gate to CLEAR. Without this,
// a session parked at AwaitingCredentials for a missing credential would look
// exactly like one parked for a credential-update request, and the park
// assertion would pass for the wrong reason.
func itAwaitPassthroughReady(t *testing.T, env *testenv.Env, sessName string) {
	t.Helper()
	testfixtures.Eventually(t, itPollTimeout, func() bool {
		var sess spiceboxv1alpha1.AgentSession
		if err := env.Client.Get(context.Background(),
			types.NamespacedName{Namespace: itNS, Name: sessName}, &sess); err != nil {
			return false
		}
		c := apimeta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialsReady)
		if c == nil || c.Status != metav1.ConditionTrue {
			return false
		}
		var projected corev1.Secret
		return env.Client.Get(context.Background(), types.NamespacedName{
			Namespace: itNS, Name: spiceboxv1alpha1.PassthroughCredentialSecretName(sessName),
		}, &projected) == nil
	})
}

func itAwaitCredentialUpdatePending(t *testing.T, env *testenv.Env, sessName string, want metav1.ConditionStatus) {
	t.Helper()
	var last string
	deadline := time.Now().Add(itPollTimeout)
	for time.Now().Before(deadline) {
		var sess spiceboxv1alpha1.AgentSession
		if err := env.Client.Get(context.Background(),
			types.NamespacedName{Namespace: itNS, Name: sessName}, &sess); err == nil {
			c := apimeta.FindStatusCondition(sess.Status.Conditions,
				spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending)
			if c != nil {
				last = string(c.Status) + "/" + c.Reason
				if c.Status == want {
					return
				}
			}
		}
		time.Sleep(testfixtures.PollInterval)
	}
	// Dump the whole status: the park sits deep in AgentSession.Reconcile, so a
	// miss is nearly always some EARLIER gate returning first, and the phase +
	// condition set names which one.
	t.Fatalf("AgentSession %s never reached CredentialUpdatePending=%s within %v; last was %q\nsession status: %s",
		sessName, want, itPollTimeout, last, itDumpSessionStatus(t, env, sessName))
}

func itDumpSessionStatus(t *testing.T, env *testenv.Env, sessName string) string {
	t.Helper()
	var sess spiceboxv1alpha1.AgentSession
	if err := env.Client.Get(context.Background(),
		types.NamespacedName{Namespace: itNS, Name: sessName}, &sess); err != nil {
		return "get failed: " + err.Error()
	}
	b, err := json.MarshalIndent(sess.Status, "", "  ")
	if err != nil {
		return "marshal failed: " + err.Error()
	}
	return string(b)
}

// itTouchUserIdentity bumps an annotation on the UserIdentity so the
// AgentSession reconciler re-enqueues the sessions projecting from it.
func itTouchUserIdentity(t *testing.T, ctx context.Context, c client.Client, name string) {
	t.Helper()
	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: name}, &ui), "get UserIdentity")
	if ui.Annotations == nil {
		ui.Annotations = map[string]string{}
	}
	ui.Annotations["integration-test/nudge"] = time.Now().Format(time.RFC3339Nano)
	require.NoError(t, c.Update(ctx, &ui), "nudge UserIdentity")
}

// ---------------------------------------------------------------------------
// 3: owner-ref GC readiness
// ---------------------------------------------------------------------------

// TestRequest_CarriesAGCReadyOwnerRefToItsLiveSession pins the owner-ref that
// makes a CredentialUpdateRequest live exactly as long as its AgentSession --
// the property the ask budget is DERIVED from (budgetExceeded lists this
// session's already-SPENT asks rather than storing a counter, which is only
// sound because the requests die with the session).
//
// It asserts the ref the request_credential_update meta tool actually stamps,
// against the session's REAL apiserver-assigned UID, then proves the operator
// agrees by driving the request to a decision. A fabricated or stale UID is the
// failure that matters: Kubernetes' GC would collect such a request IMMEDIATELY
// (its owner does not exist), silently deleting a request a human is looking
// at, and resolveIdentity's ownedBySession refuses it outright.
//
// What this canNOT assert: that the apiserver actually deletes the request when
// the session goes away. envtest runs kube-apiserver + etcd only -- there is no
// kube-controller-manager, so no garbage collector runs at all. The repo's
// existing convention for owned objects is the same structural assertion (see
// the PVC and SessionUserIdentity owner-ref tests in
// pkg/controllers/agentsession); the deletion itself is Kubernetes' behavior,
// not ours, and the ref is the whole of our contribution to it.
func TestRequest_CarriesAGCReadyOwnerRefToItsLiveSession(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	itRejectingProvider(t)

	const (
		sessName  = "gc-sess"
		className = "gc-cls"
		mcpName   = "gc-mcp"
	)

	require.NoError(t, env.Client.Create(ctx, itMCPServer(mcpName)), "create MCPServer")
	ac := itClass(className, mcpName)
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	itMarkClassValid(t, ctx, env.Client, ac)
	require.NoError(t, env.Client.Create(ctx, itSession(sessName, className, true)), "create AgentSession")
	require.NoError(t, env.Client.Create(ctx,
		itSessionUserIdentity(sessName, itStaticCredential(itCredName))), "create SessionUserIdentity")
	require.NoError(t, env.Client.Create(ctx,
		itProjectedSecret(sessName, map[string][]byte{itCredName: []byte("dead-token")})), "create projected Secret")

	live := itGetSession(t, env.Client, sessName)
	require.NotEmpty(t, live.UID, "the apiserver must have assigned a UID")

	// Create through the REAL meta tool, not a hand-built CR: the ownerRef under
	// test is the one production stamps.
	created := itCreateViaMetaTool(t, env, live, "mcpserver/"+mcpName)

	require.Len(t, created.OwnerReferences, 1, "exactly one owner: the session that asked")
	owner := created.OwnerReferences[0]
	assert.Equal(t, "AgentSession", owner.Kind)
	assert.Equal(t, spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(), owner.APIVersion)
	assert.Equal(t, sessName, owner.Name)
	assert.Equal(t, live.UID, owner.UID,
		"the ref must name the LIVE session's UID; a stale or fabricated one is collected immediately by GC")
	require.NotNil(t, owner.Controller)
	assert.True(t, *owner.Controller, "the session is the controlling owner")
	require.NotNil(t, owner.BlockOwnerDeletion)
	assert.True(t, *owner.BlockOwnerDeletion)

	// The operator agrees the ref is genuine: ownedBySession passes, so the
	// request is decided rather than refused NoCredential.
	itStartCredentialUpdateOperator(t, env, 10*time.Minute)
	itPhaseReaches(t, env.Client, created.Name, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen)
}

// ---------------------------------------------------------------------------
// 4 + 5: the ask budget, and that it is keyed by CREDENTIAL
// ---------------------------------------------------------------------------

// itTerminalRequest creates a request already at a terminal phase with a
// resolved credential -- a prior ask, from the budget's point of view.
func itTerminalRequest(t *testing.T, ctx context.Context, c client.Client, name string,
	sess *spiceboxv1alpha1.AgentSession, origin string, ref spiceboxv1alpha1.ResolvedCredentialRef) {
	t.Helper()
	cr := itRequest(name, sess, origin)
	require.NoError(t, c.Create(ctx, cr), "create prior request %s", name)
	cr.Status.Phase = spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired
	cr.Status.Determination = spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified
	cr.Status.Reason = "a prior ask, already terminal"
	cr.Status.ResolvedCredential = &ref
	require.NoError(t, c.Status().Update(ctx, cr), "stamp prior request %s terminal", name)
}

// TestBudget_ExhaustsPerCredentialAndNotPerOrigin covers both halves of the
// budget in one fixture, because the second half is only meaningful against the
// first: two prior terminal asks for credential A refuse a third ask for A, and
// the SAME two asks leave credential B untouched.
//
// Keying the budget by origin instead of by resolved credential is the mistake
// this pins. It is invisible in a single-origin test -- and it is the wrong
// key in BOTH directions: two origins sharing one token would each get their
// own budget (pestering a human twice as often for the same token), and this
// test's B case would refuse an ask about a credential nobody has been asked
// about yet.
func TestBudget_ExhaustsPerCredentialAndNotPerOrigin(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	itRejectingProvider(t)

	const (
		sessName  = "budget-sess"
		className = "budget-cls"
		mcpA      = "budget-mcp-a"
		mcpB      = "budget-mcp-b"
		credB     = "second-upstream-token"
	)

	mcpAObj := itMCPServer(mcpA)
	mcpBObj := itMCPServer(mcpB)
	mcpBObj.Spec.Auth.Credential = credB
	require.NoError(t, env.Client.Create(ctx, mcpAObj), "create MCPServer A")
	require.NoError(t, env.Client.Create(ctx, mcpBObj), "create MCPServer B")

	ac := itClass(className, mcpA, mcpB)
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	itMarkClassValid(t, ctx, env.Client, ac)
	require.NoError(t, env.Client.Create(ctx, itSession(sessName, className, true)), "create AgentSession")
	require.NoError(t, env.Client.Create(ctx, itSessionUserIdentity(sessName,
		itStaticCredential(itCredName), itStaticCredential(credB))), "create SessionUserIdentity")
	require.NoError(t, env.Client.Create(ctx, itProjectedSecret(sessName, map[string][]byte{
		itCredName: []byte("dead-token-a"),
		credB:      []byte("dead-token-b"),
	})), "create projected Secret")

	live := itGetSession(t, env.Client, sessName)
	refA := spiceboxv1alpha1.ResolvedCredentialRef{
		IdentityKind: "SessionUserIdentity", Namespace: itNS, Name: sessName,
		Credential: itCredName, ProviderID: itProvider,
	}

	// Two prior terminal asks for credential A -- deliberately through two
	// DIFFERENT origins, so a per-origin budget would count one each and let the
	// third ask through.
	itTerminalRequest(t, ctx, env.Client, "budget-prior-1", live, "mcpserver/"+mcpA, refA)
	itTerminalRequest(t, ctx, env.Client, "budget-prior-2", live, "mcpserver/"+mcpB, refA)

	itStartCredentialUpdateOperator(t, env, 10*time.Minute)

	// A third ask for credential A is refused outright.
	require.NoError(t, env.Client.Create(ctx, itRequest("budget-third-a", live, "mcpserver/"+mcpA)),
		"create third request for credential A")
	itPhaseReaches(t, env.Client, "budget-third-a", spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused)
	refused := itGetRequest(t, env.Client, "budget-third-a")
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationBudgetExhausted, refused.Status.Determination)
	assert.NotEmpty(t, refused.Status.Reason, "a refusal the agent reads verbatim is never empty")

	// The same two prior asks must NOT have spent credential B's budget.
	require.NoError(t, env.Client.Create(ctx, itRequest("budget-first-b", live, "mcpserver/"+mcpB)),
		"create first request for credential B")
	itPhaseReaches(t, env.Client, "budget-first-b", spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen)
	openedB := itGetRequest(t, env.Client, "budget-first-b")
	require.NotNil(t, openedB.Status.ResolvedCredential)
	assert.Equal(t, credB, openedB.Status.ResolvedCredential.Credential,
		"credential B's first ask opens: the budget is keyed by credential, not by session or origin")
}

// ---------------------------------------------------------------------------
// 6: a refusal, end to end, reaching the agent verbatim
// ---------------------------------------------------------------------------

// TestRefusal_CredentialLiveReachesTheAgentVerbatimAndPublishesNoCard drives
// the refusal an operator most needs to be right: the agent says the token is
// dead, the provider says it still authenticates, so no human is bothered.
//
// Three claims, none of which any single-component test makes together:
//   - the operator refuses CredentialLive rather than opening;
//   - channelsd -- running, watching, and demonstrably capable of publishing --
//     publishes NOTHING, because nothing ever reached phase Open;
//   - the blocked meta tool returns the operator's platform-authored reason
//     VERBATIM. That text is the agent's ONLY signal about why it was refused,
//     and the whole point of the refusal is that it redirects the agent to a
//     scope/permissions problem. Paraphrasing it downstream would destroy that.
func TestRefusal_CredentialLiveReachesTheAgentVerbatimAndPublishesNoCard(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	itAcceptingProvider(t)

	const (
		sessName  = "live-sess"
		className = "live-cls"
		mcpName   = "live-mcp"
	)

	require.NoError(t, env.Client.Create(ctx, itMCPServer(mcpName)), "create MCPServer")
	ac := itClass(className, mcpName)
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	itMarkClassValid(t, ctx, env.Client, ac)
	require.NoError(t, env.Client.Create(ctx, itSession(sessName, className, true)), "create AgentSession")
	require.NoError(t, env.Client.Create(ctx,
		itSessionUserIdentity(sessName, itStaticCredential(itCredName))), "create SessionUserIdentity")
	require.NoError(t, env.Client.Create(ctx,
		itProjectedSecret(sessName, map[string][]byte{itCredName: []byte("perfectly-good-token")})), "create projected Secret")

	itStartCredentialUpdateOperator(t, env, 10*time.Minute)
	pub := itStartChannelsd(t, env)

	live := itGetSession(t, env.Client, sessName)
	res := itExecuteMetaTool(t, env, live, "mcpserver/"+mcpName)

	decided := itGetRequest(t, env.Client, itLastRequestName(t, env.Client))
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, decided.Status.Phase)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationCredentialLive, decided.Status.Determination)

	assert.True(t, res.IsError, "a refusal is an error result the agent must not read as success")
	assert.True(t, res.Trusted, "the refusal text is entirely platform-authored")
	assert.Contains(t, res.Content, decided.Status.Reason,
		"the operator's reason must reach the agent VERBATIM, not paraphrased")
	assert.Contains(t, decided.Status.Reason, "permissions or scope",
		"and that reason is the one that redirects the agent away from re-asking")

	// channelsd is running and watching. It must have published nothing.
	assert.Empty(t, pub.interactionRequests(t),
		"a refused request never reaches phase Open, so no human is ever shown a card")
}

// ---------------------------------------------------------------------------
// The unverified tier, over a real apiserver
// ---------------------------------------------------------------------------

// TestUnverifiedTier_ObservedFailureOpensACardSayingItCouldNotBeConfirmed is
// the one place the corroboration path runs across BOTH processes that own its
// halves, against a real apiserver:
//
//	runner    writes AgentSession.status.credentialAuthFailures  (this test uses
//	          the REAL runner.StatusPatcher, not a hand-built status object)
//	operator  reads it back and turns "unsupported probe" into an OPEN request
//	channelsd publishes the card whose verdict line the human reads
//
// A fake-client unit test cannot make the first claim at all. A runner-owned
// status field the CRD schema does not actually persist is silently dropped by
// the apiserver: the write succeeds, the read comes back empty, and
// corroboration is permanently impossible in a real cluster while every unit
// test stays green. That failure shape has shipped here before, so it is
// asserted directly (the read-back below) as well as through the behavior that
// depends on it.
//
// The provider deliberately declares no verify: probe. With a definitive
// verdict available the observation would be irrelevant -- this is the only
// configuration where corroboration decides the outcome.
func TestUnverifiedTier_ObservedFailureOpensACardSayingItCouldNotBeConfirmed(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	const (
		sessName  = "unverified-sess"
		className = "unverified-cls"
		mcpName   = "unverified-mcp"
		curName   = "unverified-cur"
	)
	origin := "mcpserver/" + mcpName

	require.NoError(t, env.Client.Create(ctx, itMCPServerWithProvider(mcpName, itUnverifiableProvider)), "create MCPServer")
	ac := itClass(className, mcpName)
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	itMarkClassValid(t, ctx, env.Client, ac)
	require.NoError(t, env.Client.Create(ctx, itSession(sessName, className, true)), "create AgentSession")
	require.NoError(t, env.Client.Create(ctx,
		itSessionUserIdentity(sessName, itStaticCredential(itCredName))), "create SessionUserIdentity")
	require.NoError(t, env.Client.Create(ctx,
		itProjectedSecret(sessName, map[string][]byte{itCredName: []byte("dead-token")})), "create projected Secret")

	// --- the runner's half -------------------------------------------------
	// Written through runner.StatusPatcher, the same optimistic-locked status
	// patch internal/cmd/runner uses, so the write path under test is production's and
	// not a test-only Status().Update the CRD might tolerate differently.
	require.NoError(t,
		agentrunner.NewStatusPatcher(env.Client, types.NamespacedName{Namespace: itNS, Name: sessName}).
			RecordCredentialAuthFailure(ctx, origin, 3),
		"the runner must be able to record its auth-failure observation")

	persisted := itGetSession(t, env.Client, sessName)
	require.NotNil(t, spiceboxv1alpha1.FindCredentialAuthFailure(persisted.Status.CredentialAuthFailures, origin),
		"the apiserver must actually PERSIST the observation; a status field the CRD schema drops would make "+
			"corroboration impossible in a real cluster while every fake-client test stayed green. status=%+v",
		persisted.Status.CredentialAuthFailures)

	// --- the operator's half ------------------------------------------------
	itStartCredentialUpdateOperator(t, env, 10*time.Minute)
	pub := itStartChannelsd(t, env)

	live := itGetSession(t, env.Client, sessName)
	require.NoError(t, env.Client.Create(ctx, itRequest(curName, live, origin)), "create request")

	itPhaseReaches(t, env.Client, curName, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen)
	opened := itGetRequest(t, env.Client, curName)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationRejectedUnverified, opened.Status.Determination,
		"an unprobeable provider plus the platform's own observation is the UNVERIFIED tier, not a verified rejection")
	assert.Contains(t, opened.Status.Reason, "could not confirm",
		"the verdict must say plainly that the failure was not confirmed with the provider")
	require.NotNil(t, opened.Status.CredentialSecretRef,
		"an unverified-tier Open is a real Open: without the recorded baseline the unpark watch can never fire")

	// --- what the human actually sees ---------------------------------------
	var card channelevents.InteractionRequestPayload
	testfixtures.Eventually(t, itPollTimeout, func() bool {
		reqs := pub.interactionRequests(t)
		if len(reqs) == 0 {
			return false
		}
		card = reqs[0]
		return true
	})
	assert.Equal(t, categories.CredentialUpdate, card.Category)
	assert.Contains(t, card.Lead, "could not confirm",
		"the card's verdict line is the human's only signal about how strong the evidence is; "+
			"an unverified-tier card that reads like a confirmed rejection asks for more trust than the platform earned")
	assert.Equal(t, opened.Status.Reason, card.Lead,
		"and it is the operator's verdict verbatim, not a paraphrase channelsd invented")
}

// ---------------------------------------------------------------------------
// 7: never delivered != nobody acted
// ---------------------------------------------------------------------------

// TestSeam_NeverDeliveredRequestExpiresSayingSo pins the inverted-signal bug at
// the level it actually lives: ACROSS the operator/channelsd split.
//
// The operator decides Open. channelsd, in its own process, cannot deliver --
// here because the session is not channel-attached, one of the watcher's real
// silent skip paths. Nobody is ever asked. When the ask window elapses, the
// expiry Reason must say NOBODY WAS ASKED, not "nobody updated the credential"
// -- the latter is a claim about a human's inaction that never happened, and it
// is the message an agent (and, through it, a user) would be told.
//
// InteractionRef=="" is the only discriminator either side has, and it is
// exactly the fact that only exists because a DIFFERENT process writes it. A
// fake-client test of either half alone cannot distinguish the two cases at all.
func TestSeam_NeverDeliveredRequestExpiresSayingSo(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	itRejectingProvider(t)

	const (
		sessName  = "undelivered-sess"
		className = "undelivered-cls"
		mcpName   = "undelivered-mcp"
		curName   = "undelivered-cur"
	)

	require.NoError(t, env.Client.Create(ctx, itMCPServer(mcpName)), "create MCPServer")
	ac := itClass(className, mcpName)
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	itMarkClassValid(t, ctx, env.Client, ac)
	// No InputChannel: there is no surface to deliver a card on.
	require.NoError(t, env.Client.Create(ctx, itSession(sessName, className, false)), "create AgentSession")
	require.NoError(t, env.Client.Create(ctx,
		itSessionUserIdentity(sessName, itStaticCredential(itCredName))), "create SessionUserIdentity")
	require.NoError(t, env.Client.Create(ctx,
		itProjectedSecret(sessName, map[string][]byte{itCredName: []byte("dead-token")})), "create projected Secret")

	// A short ask window: the point is the expiry text, not the wait.
	itStartCredentialUpdateOperator(t, env, 5*time.Second)
	pub := itStartChannelsd(t, env)

	live := itGetSession(t, env.Client, sessName)
	require.NoError(t, env.Client.Create(ctx, itRequest(curName, live, "mcpserver/"+mcpName)), "create request")

	itPhaseReaches(t, env.Client, curName, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen)
	assert.Empty(t, itGetRequest(t, env.Client, curName).Status.InteractionRef,
		"nothing was delivered, so nothing may claim it was")

	itPhaseReaches(t, env.Client, curName, spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired)
	expired := itGetRequest(t, env.Client, curName)
	assert.Contains(t, expired.Status.Reason, "never delivered",
		"an undelivered request must expire saying nobody was ASKED")
	assert.NotContains(t, expired.Status.Reason, "Nobody updated the credential",
		"claiming a human ignored a card nobody was shown is the exact inversion this pins")
	assert.Empty(t, pub.interactionRequests(t), "and channelsd genuinely published nothing")
}

// ---------------------------------------------------------------------------
// The meta tool, driven against the real apiserver
// ---------------------------------------------------------------------------

// itOriginTool is the failing upstream tool the agent names. It implements
// tool.OriginTool, which is what the meta tool requires before it will write a
// CR at all.
type itOriginTool struct{ origin string }

func (itOriginTool) Name() string                 { return "demo_upstream_create_issue" }
func (itOriginTool) Kind() agenttool.Kind         { return agenttool.KindMCP }
func (itOriginTool) Description() string          { return "create an issue upstream" }
func (itOriginTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (itOriginTool) Execute(context.Context, json.RawMessage, *agenttool.SessionContext) (agenttool.Result, error) {
	return agenttool.Result{}, nil
}
func (itOriginTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (itOriginTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (o itOriginTool) Origin() string                              { return o.origin }

// itMetaTool builds request_credential_update wired to the envtest apiserver
// and to a single origin-bearing tool, with test-sized polling.
func itMetaTool(env *testenv.Env, origin string) agenttool.Tool {
	return meta.NewCredentialUpdate(meta.CredentialUpdateConfig{
		Client: env.Client,
		ToolLookup: func(name string) (agenttool.Tool, bool) {
			ot := itOriginTool{origin: origin}
			if name != ot.Name() {
				return nil, false
			}
			return ot, true
		},
		PollInterval: 100 * time.Millisecond,
		MaxWait:      itPollTimeout,
	})
}

// itExecuteMetaTool runs the blocking tool to completion and returns its
// Result -- the exact bytes the LLM would receive as its tool_result.
func itExecuteMetaTool(t *testing.T, env *testenv.Env, sess *spiceboxv1alpha1.AgentSession, origin string) agenttool.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itPollTimeout)
	defer cancel()
	res, err := itMetaTool(env, origin).Execute(ctx,
		json.RawMessage(`{"tool":"demo_upstream_create_issue","why":"the last three calls came back 401 Unauthorized"}`),
		&agenttool.SessionContext{Namespace: itNS, Name: sess.Name, AgentSessionUID: sess.UID})
	require.NoError(t, err, "request_credential_update must never return a transport error")
	return res
}

// itCreateViaMetaTool runs the meta tool only long enough for it to create the
// CR, then returns the created object. The tool blocks until a decision, so it
// runs on its own goroutine and the test reads the CR the moment it appears --
// no operator is running yet, so nothing can decide it out from under us.
func itCreateViaMetaTool(t *testing.T, env *testenv.Env, sess *spiceboxv1alpha1.AgentSession,
	origin string) *spiceboxv1alpha1.CredentialUpdateRequest {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_, _ = itMetaTool(env, origin).Execute(ctx,
			json.RawMessage(`{"tool":"demo_upstream_create_issue","why":"the last three calls came back 401 Unauthorized"}`),
			&agenttool.SessionContext{Namespace: itNS, Name: sess.Name, AgentSessionUID: sess.UID})
	}()
	var created *spiceboxv1alpha1.CredentialUpdateRequest
	testfixtures.Eventually(t, itPollTimeout, func() bool {
		var list spiceboxv1alpha1.CredentialUpdateRequestList
		if err := env.Client.List(context.Background(), &list, client.InNamespace(itNS)); err != nil {
			return false
		}
		if len(list.Items) == 0 {
			return false
		}
		created = &list.Items[0]
		return true
	})
	require.NotNil(t, created, "request_credential_update must create a CredentialUpdateRequest")
	return created
}

// itLastRequestName returns the single CredentialUpdateRequest in the test
// namespace. The meta tool names its CRs with a random suffix, so tests that
// create through it look the name up rather than assuming one.
func itLastRequestName(t *testing.T, c client.Client) string {
	t.Helper()
	var list spiceboxv1alpha1.CredentialUpdateRequestList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace(itNS)), "list requests")
	require.Len(t, list.Items, 1, "exactly one request expected in this test")
	return list.Items[0].Name
}
