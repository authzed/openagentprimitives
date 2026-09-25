// pkg/controllers/useridentity/revoke_integration_test.go
//
// Integration test: drives the operator-side RevokePublisher and the
// runner-side unified revocation subscriber against a real embedded NATS
// server. After the credential-path migration, the RevokePublisher emits a
// KindRevoked envelope on the unified ap.revocation subject, and the runner
// subscribes via revocation.RegisterSubscriber with a registry containing the
// credential Invalidator (which calls InvalidateSecret on the broker).
//
// Build tag: integration
// Run: go test -tags=integration -count=1 -v -timeout=30s
//
//	./pkg/controllers/useridentity/... -run RevokeRoundTrip
//
//go:build integration

package useridentity

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/credential"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	iduseridentity "github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// startPlainNATSServer boots an embedded NATS server with no auth and a
// randomly assigned port. Callers connect via srv.ClientURL().
// The server is shut down automatically when the test ends.
func startPlainNATSServer(t *testing.T) *natsserver.Server {
	t.Helper()
	srv := natstest.RunServer(&natsserver.Options{
		Host:   "127.0.0.1",
		Port:   -1, // OS-assigned port
		NoLog:  true,
		NoSigs: true,
	})
	t.Cleanup(srv.Shutdown)
	return srv
}

// natsEnvelopePublisher is a test-local implementation of
// revocation.EventPublisher that JSON-encodes each envelope and publishes it
// on revocation.Subject. Mirrors the operator-internal natsEnvelopePublisher
// in internal/cmd/operator/main.go without importing that package.
type natsEnvelopePublisher struct {
	nc *nats.Conn
}

func (p *natsEnvelopePublisher) Publish(_ context.Context, env channelevents.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return p.nc.Publish(revocation.Subject, data)
}

// natsSubscriberAdapter adapts a *nats.Conn to revocation.NATSSubscriber.
// Mirrors the natsConnAdapter in internal/cmd/runner/nats.go without importing that
// package.
type natsSubscriberAdapter struct{ conn *nats.Conn }

func (a *natsSubscriberAdapter) Subscribe(subject string, handler func([]byte)) error {
	_, err := a.conn.Subscribe(subject, func(msg *nats.Msg) { handler(msg.Data) })
	return err
}

// invalidateCall records one call to credential.SecretInvalidator.InvalidateSecret.
type invalidateCall struct {
	Namespace string
	Name      string
}

// secretInvalidateCh is a fake credential.SecretInvalidator that sends each
// InvalidateSecret call onto a buffered channel so the test can assert on it
// with a timeout.
type secretInvalidateCh struct {
	mu sync.Mutex
	ch chan invalidateCall
}

func newSecretInvalidateCh(buf int) *secretInvalidateCh {
	return &secretInvalidateCh{ch: make(chan invalidateCall, buf)}
}

func (r *secretInvalidateCh) InvalidateSecret(namespace, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ch <- invalidateCall{Namespace: namespace, Name: name}
	return nil
}

// sessionNamespace is a realistic runner-session namespace, deliberately
// DISTINCT from IdentitiesNamespace. Production userPassthrough sessions almost
// never run in IdentitiesNamespace, so registering the subscriber here exercises
// the real path: credential revokes must be emitted cluster-wide (scope "") to
// reach a cross-namespace session. Registering with IdentitiesNamespace (the
// old test value) masked the scope-gate regression.
const sessionNamespace = "team-a"

// registerUnifiedSubscriber wires revocation.RegisterSubscriber against subConn
// with a registry holding the credential Invalidator backed by inv. The
// subscriber's myNamespace is a realistic session namespace distinct from
// IdentitiesNamespace, so the test only passes if credential revokes are
// emitted cluster-wide (scope "") rather than scoped to IdentitiesNamespace.
func registerUnifiedSubscriber(t *testing.T, subConn *nats.Conn, inv credential.SecretInvalidator) {
	t.Helper()
	reg := revocation.NewRegistry()
	require.NoError(t, reg.Register(credential.New(inv)), "register credential invalidator")
	require.NoError(t,
		revocation.RegisterSubscriber(context.Background(),
			&natsSubscriberAdapter{conn: subConn},
			reg,
			sessionNamespace,
		),
		"RegisterSubscriber",
	)
}

// TestRevokeRoundTrip_NATSDelivery drives the full chain against a real
// embedded NATS server:
//
//  1. Operator-side RevokePublisher emits a KindRevoked envelope on
//     spec.credentials diff (kind="credential").
//  2. Runner-side revocation.RegisterSubscriber receives the envelope via the
//     unified NATS subject ap.revocation.
//  3. The credential Invalidator calls InvalidateSecret with the master
//     Secret coords for the removed (subject, cred) within 2 s.
func TestRevokeRoundTrip_NATSDelivery(t *testing.T) {
	srv := startPlainNATSServer(t)

	// Publisher side — operator's NATS connection.
	pubConn, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err, "publisher nats.Connect")
	t.Cleanup(func() { pubConn.Close() })

	rp := NewRevokePublisher(revocation.NewPublisher(&natsEnvelopePublisher{nc: pubConn}))

	// Subscriber side — runner's NATS connection + invalidator.
	subConn, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err, "subscriber nats.Connect")
	t.Cleanup(func() { subConn.Close() })

	inv := newSecretInvalidateCh(4)
	registerUnifiedSubscriber(t, subConn, inv)

	// Ensure the subscription is flushed before we publish.
	require.NoError(t, subConn.Flush(), "flush subscriber connection")

	// Step 1 — prime: first Observe registers state without emitting.
	rp.Observe(context.Background(),
		uiWithCreds("u-alice", "user:alice",
			patCred("linear-pat", "u-alice-linear-pat"),
		),
	)

	// Step 2 — revoke: second Observe removes the credential, which triggers
	// a KindRevoked envelope keyed by the master Secret coords.
	rp.Observe(context.Background(),
		uiWithCreds("u-alice", "user:alice"), // credential list is now empty
	)

	// Step 3 — assert the subscriber received and dispatched the invalidation
	// within 2 s, with the master Secret coords for (u-alice, linear-pat).
	select {
	case got := <-inv.ch:
		assert.Equal(t, spiceboxv1alpha1.IdentitiesNamespace, got.Namespace,
			"InvalidateSecret must target the identities namespace")
		assert.Equal(t, iduseridentity.MasterSecretName("u-alice", "linear-pat"), got.Name,
			"InvalidateSecret must target the removed credential's master Secret")
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber never received revoke event within 2 s")
	}
}

// TestRevokeRoundTrip_NATSDelivery_Replaced verifies the REPLACED path: same
// credential name but different fingerprint also reaches the subscriber.
func TestRevokeRoundTrip_NATSDelivery_Replaced(t *testing.T) {
	srv := startPlainNATSServer(t)

	pubConn, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err, "publisher nats.Connect")
	t.Cleanup(func() { pubConn.Close() })

	rp := NewRevokePublisher(revocation.NewPublisher(&natsEnvelopePublisher{nc: pubConn}))

	subConn, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err, "subscriber nats.Connect")
	t.Cleanup(func() { subConn.Close() })

	inv := newSecretInvalidateCh(4)
	registerUnifiedSubscriber(t, subConn, inv)
	require.NoError(t, subConn.Flush(), "flush subscriber connection")

	// Prime with the original secret reference.
	rp.Observe(context.Background(),
		uiWithCreds("u-bob", "user:bob",
			patCred("github-pat", "u-bob-github-pat-v1"),
		),
	)

	// Replace: same credential name, different Secret name → re-emits a
	// credential revoke (same master Secret coords, derived from cred name).
	rp.Observe(context.Background(),
		uiWithCreds("u-bob", "user:bob",
			patCred("github-pat", "u-bob-github-pat-v2"),
		),
	)

	select {
	case got := <-inv.ch:
		assert.Equal(t, spiceboxv1alpha1.IdentitiesNamespace, got.Namespace)
		assert.Equal(t, iduseridentity.MasterSecretName("u-bob", "github-pat"), got.Name)
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber never received refreshed event within 2 s")
	}
}
