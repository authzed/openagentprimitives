// Package natstest answers one question for tests: given a minted
// nats.UserGrant, does the NATS server permit this subject?
//
// It answers by asking a real embedded nats-server, configured with the same
// decentralized-JWT trust material `oap install` writes, rather than by matching
// the grant's allow-list strings in Go. A hand-rolled matcher can disagree with
// the server, and a grant assertion that disagrees with the server is worse
// than no assertion — it reads as coverage while permitting the over-grant it
// claims to forbid. Two traps a string assertion walks straight into:
//
//   - `assert.NotContains(g.PubAllow, "…in.interaction_decision")` passes
//     against a grant of `ap.session.<ns>.<name>.>`, which permits exactly that
//     subject. The absent string proves nothing; the wildcard covers it.
//   - An empty PubAllow is publish-ANYWHERE, not deny-all (see the UserGrant
//     doc comment). `assert.Empty(g.PubAllow)` therefore asserts the opposite
//     of what it looks like.
//
// Both are settled here by observation: mint the grant, connect with it, try
// the subject, and report what the server did.
//
// Import this from _test.go files only. It is a normal (non-test) package so
// several packages' tests can share it; nothing in a production binary imports
// it, so nats-server stays out of every shipped binary.
package natstest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	natsservertest "github.com/nats-io/nats-server/v2/test"
	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
)

// Harness is an embedded nats-server plus the operator/account identity whose
// signing key mints the users under test. Safe to reuse across subtests; each
// probe gets its own short-lived connection so one probe's permission error
// cannot leak into the next (nats.Conn.LastError is sticky).
type Harness struct {
	Identity *apnats.Identity
	Server   *natsserver.Server
	dir      string
}

// New boots the embedded server and registers its shutdown with t.Cleanup.
func New(t *testing.T) *Harness {
	t.Helper()

	id, err := apnats.GenerateIdentity()
	require.NoError(t, err, "generate NATS identity")

	opClaims, err := jwt.DecodeOperatorClaims(id.OperatorJWT)
	require.NoError(t, err, "decode operator JWT into TrustedOperators")

	resolver := &natsserver.MemAccResolver{}
	require.NoError(t, resolver.Store(id.AccountPublicKey, id.AccountJWT),
		"preload account JWT into MemAccResolver")

	srv := natsservertest.RunServer(&natsserver.Options{
		Host:             "127.0.0.1",
		Port:             -1,
		NoLog:            true,
		NoSigs:           true,
		TrustedOperators: []*jwt.OperatorClaims{opClaims},
		AccountResolver:  resolver,
	})
	t.Cleanup(srv.Shutdown)

	return &Harness{Identity: id, Server: srv, dir: t.TempDir()}
}

// Connect mints g and dials the embedded server through the production
// pkg/platform/nats.Connect path, so whatever that path configures — TLS, the
// per-principal inbox prefix — is what the test exercises.
func (h *Harness) Connect(t *testing.T, g apnats.UserGrant) *natsgo.Conn {
	t.Helper()

	creds, err := apnats.MintUser(h.Identity, g)
	require.NoError(t, err, "mint user %q", g.Name)

	// Unique per call: two connections for the same grant in one test must not
	// race on the same file, and a stale file must never satisfy a later probe.
	f, err := os.CreateTemp(h.dir, filepath.Base(g.Name)+"-*.creds")
	require.NoError(t, err, "create creds file for %q", g.Name)
	_, err = f.WriteString(creds)
	require.NoError(t, err, "write creds for %q", g.Name)
	require.NoError(t, f.Close(), "close creds for %q", g.Name)

	nc, err := apnats.Connect(apnats.Options{
		URL:       h.Server.ClientURL(),
		CredsPath: f.Name(),
		Name:      g.Name,
	})
	require.NoError(t, err, "connect user %q", g.Name)
	t.Cleanup(nc.Close)
	return nc
}

// PublishAllowed reports whether the server let a client minted from g publish
// on subject.
//
// Determinism note: a permissions violation is a transient -ERR, so the server
// keeps the connection open and still answers the PING that Flush sends. The
// client's reader goroutine parses the -ERR before the PONG and sets
// LastError synchronously (nats.go processTransientError), so by the time
// Flush returns the verdict is already recorded. No sleeps, no polling.
func (h *Harness) PublishAllowed(t *testing.T, g apnats.UserGrant, subject string) bool {
	t.Helper()
	nc := h.Connect(t, g)
	defer nc.Close()

	require.NoError(t, nc.Publish(subject, []byte("probe")), "publish %q", subject)
	require.NoError(t, nc.Flush(), "flush after publish %q", subject)
	return !isPermissionViolation(nc.LastError())
}

// SubscribeAllowed reports whether the server let a client minted from g
// subscribe to subject. Subscribe itself always returns nil — the server's
// refusal arrives asynchronously — which is exactly why a caller cannot check
// this without a round trip.
func (h *Harness) SubscribeAllowed(t *testing.T, g apnats.UserGrant, subject string) bool {
	t.Helper()
	nc := h.Connect(t, g)
	defer nc.Close()

	_, err := nc.SubscribeSync(subject)
	require.NoError(t, err, "subscribe %q returns nil even when denied", subject)
	require.NoError(t, nc.Flush(), "flush after subscribe %q", subject)
	return !isPermissionViolation(nc.LastError())
}

func isPermissionViolation(err error) bool {
	return err != nil && errors.Is(err, natsgo.ErrPermissionViolation)
}
