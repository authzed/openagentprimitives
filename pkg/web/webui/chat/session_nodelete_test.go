package chat

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/go-logr/logr"
)

// --- invariant: the ADOPT half never deletes a created session -------------
//
// The whole point of an AgentSession is that, once created, the operator owns
// its lifecycle: it may legitimately park (AwaitingCredentials /
// AwaitingIdentityChoice), come up slowly, or surface a Failed phase — and the
// health watcher reports all of that to the browser. The in-process wiring
// erroring (a caller context cancelled while the adopt waits on readiness) is
// NOT a reason to destroy the k8s objects the user's session lives in.
// Deleting them on such an error is how a session the user started silently
// vanishes ("stuck on starting, then gone") instead of showing its real state.
//
// The CREATE half of the same invariant lives with the code that writes those
// objects — see pkg/web/browsersession's
// TestCreate_TouchStartedByFails_NeverDeletesTheCreatedSession — so neither
// half is pinned twice and neither is unpinned.

// scriptedAuthz is a pipeline.Authz whose only real method is TouchStartedBy,
// so a test can make a started_by write succeed (nil) or fail (a real error)
// without standing up SpiceDB. Every other method is promoted from the
// embedded nil interface and must not be called on this path.
type scriptedAuthz struct {
	pipeline.Authz
	touchErr error
}

func (a *scriptedAuthz) TouchStartedBy(context.Context, string, string, identity.CanonicalUserID) error {
	return a.touchErr
}

// TouchInteractor is not on pipeline.Authz (the embedded interface), so it
// must be stubbed directly for scriptedAuthz to keep satisfying authz.Granter.
func (a *scriptedAuthz) TouchInteractor(context.Context, string, string, string) error { return nil }

// nodeleteBrowserDeps adapts this package's fixtures to browsersession.Deps.
// Authz is declared as the authz.Granter INTERFACE and assigned only from a
// real value, never from a typed-nil pointer (AGENTS.md's typed-nil rule).
type nodeleteBrowserDeps struct {
	k8s     client.Client
	granter authz.Granter
}

func (d nodeleteBrowserDeps) K8s() client.Client   { return d.k8s }
func (d nodeleteBrowserDeps) Authz() authz.Granter { return d.granter }
func (d nodeleteBrowserDeps) Logger() logr.Logger  { return logr.Discard() }

// StartChecker: nil. Every fixture in this file is ungated (no
// spec.authz.session.allowedStarters), so browsersession.Create's gate is
// skipped before a nil checker would ever be dereferenced.
func (d nodeleteBrowserDeps) StartChecker() browsersession.StartChecker { return nil }

func TestAdoptRealSession_WiringError_NeverDeletesTheSession(t *testing.T) {
	const name = "s1"
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))

	_, err := browsersession.Create(context.Background(),
		nodeleteBrowserDeps{k8s: k8s, granter: &scriptedAuthz{}}, browsersession.Params{
			Namespace:   newChatSessionNamespace,
			AgentClass:  "demo-agent",
			Prompt:      "hello",
			Subject:     identity.Subject("user:owner"),
			SessionName: name,
		})
	require.NoError(t, err, "the create half must succeed so the adopt half is what fails")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// No memory-token Secret is ever seeded, so waitForSessionReady can only
	// exit via the caller context — exactly the production shape for a session
	// that parks before the operator mints the token.
	entry, err := adoptRealSession(ctx, &fakeDeps{k8s: k8s},
		sessionKey{Namespace: newChatSessionNamespace, Name: name}, "user:owner", func(terminalNotice) {})
	require.Error(t, err, "an adopt whose readiness wait is cancelled must fail")
	require.Nil(t, entry, "a failed adopt returns no entry")
	assert.ErrorContains(t, err, "become ready")

	// The invariant: the session the user started must still exist. An error
	// deleting it is the "session vanished" bug.
	assert.NoError(t, k8s.Get(context.Background(),
		client.ObjectKey{Namespace: newChatSessionNamespace, Name: name}, &spiceboxv1alpha1.AgentSession{}),
		"AgentSession must NOT be deleted when the adopt errors")

	// The Channel owns the AgentSession (owner ref); deleting it would
	// cascade-GC the session in a real cluster. The creds Secret is owned by
	// the Channel in turn. Neither may be rolled back either.
	assert.NoError(t, k8s.Get(context.Background(),
		client.ObjectKey{Namespace: newChatSessionNamespace, Name: name + "-chan"}, &spiceboxv1alpha1.Channel{}),
		"the session's Channel must NOT be deleted (its deletion GCs the AgentSession)")
	assert.NoError(t, k8s.Get(context.Background(),
		client.ObjectKey{Namespace: newChatSessionNamespace, Name: name + "-chan-creds"}, &corev1.Secret{}),
		"the session's creds Secret must NOT be deleted")
}
