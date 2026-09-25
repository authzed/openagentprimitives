package nats

import (
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startJWTServer boots an embedded nats-server configured for the
// decentralized-JWT auth produced by GenerateIdentity: the operator JWT
// is decoded into TrustedOperators and the account JWT is preloaded into
// an in-memory account resolver. This is the same trust material `oap
// install` writes into the in-cluster NATS server config, so a creds
// file minted by MintUser against `id` authenticates here exactly as it
// would in production.
func startJWTServer(t *testing.T, id *Identity) *natsserver.Server {
	t.Helper()

	opClaims, err := jwt.DecodeOperatorClaims(id.OperatorJWT)
	require.NoError(t, err, "decode operator JWT into TrustedOperators")

	resolver := &natsserver.MemAccResolver{}
	require.NoError(t, resolver.Store(id.AccountPublicKey, id.AccountJWT),
		"preload account JWT into MemAccResolver")

	srv := natstest.RunServer(&natsserver.Options{
		Host:             "127.0.0.1",
		Port:             -1,
		NoLog:            true,
		NoSigs:           true,
		TrustedOperators: []*jwt.OperatorClaims{opClaims},
		AccountResolver:  resolver,
	})
	t.Cleanup(srv.Shutdown)
	return srv
}

// connectAs mints a user for the grant, writes its creds to a temp file,
// and dials the server with them. Fails the test if the connection
// cannot be established.
func connectAs(t *testing.T, srv *natsserver.Server, id *Identity, g UserGrant) *natsgo.Conn {
	t.Helper()
	creds, err := MintUser(id, g)
	require.NoError(t, err, "mint user %q", g.Name)
	credsPath := writeTemp(t, t.TempDir(), g.Name+".creds", creds)

	nc, err := Connect(Options{
		URL:       srv.ClientURL(),
		CredsPath: credsPath,
		Name:      g.Name,
	})
	require.NoError(t, err, "connect user %q", g.Name)
	t.Cleanup(func() { nc.Close() })
	return nc
}

// TestRequestReplyRoundTrip exercises a full authenticated NATS request/reply
// against a real embedded server configured with the decentralized-JWT auth
// from GenerateIdentity. It guards against a minted grant that omits a subject
// the component actually needs.
//
// `m.Respond` publishes to the requester's `_INBOX.<id>.*` auto-inbox, a
// top-level subject NOT covered by `ap.>`. A responder grant without `_INBOX.>`
// in PubAllow therefore has its reply denied by the server, breaking
// channelsd's read_thread_history responder. The negative case below is the
// non-vacuity control.
func TestRequestReplyRoundTrip(t *testing.T) {
	id, err := GenerateIdentity()
	require.NoError(t, err, "generate NATS identity")

	srv := startJWTServer(t, id)

	const subject = "ap.session.ns.name.history.read"

	t.Run("responder granted _INBOX.> can reply: Request succeeds", func(t *testing.T) {
		// Mirrors the broadened channelsd broker grant: ap.> + _INBOX.>.
		responder := connectAs(t, srv, id, UserGrant{
			Name:     "responder-ok",
			PubAllow: []string{"ap.>", "_INBOX.>"},
			SubAllow: []string{"ap.>", "_INBOX.>"},
		})
		requester := connectAs(t, srv, id, UserGrant{
			Name:     "requester",
			PubAllow: []string{"ap.>", "_INBOX.>"},
			SubAllow: []string{"ap.>", "_INBOX.>"},
		})

		sub, err := responder.Subscribe(subject, func(m *natsgo.Msg) {
			// m.Respond publishes to the requester's _INBOX subject.
			_ = m.Respond([]byte("pong"))
		})
		require.NoError(t, err, "responder subscribe")
		require.NoError(t, responder.Flush(), "responder flush subscription")
		t.Cleanup(func() { _ = sub.Unsubscribe() })

		reply, err := requester.Request(subject, []byte("ping"), 2*time.Second)
		require.NoError(t, err, "Request must succeed when responder may publish to _INBOX")
		assert.Equal(t, "pong", string(reply.Data), "reply payload round-trips")
	})

	t.Run("responder lacking _INBOX.> cannot reply: Request times out", func(t *testing.T) {
		// C1 reproduction: PubAllow has only ap.>, omitting _INBOX.>.
		// The responder still receives the request (SubAllow covers ap.>)
		// and runs its handler, but the server denies the m.Respond
		// publish to the requester's _INBOX subject, so the requester
		// never sees a reply.
		responder := connectAs(t, srv, id, UserGrant{
			Name:     "responder-no-inbox",
			PubAllow: []string{"ap.>"},
			SubAllow: []string{"ap.>", "_INBOX.>"},
		})
		requester := connectAs(t, srv, id, UserGrant{
			Name:     "requester-2",
			PubAllow: []string{"ap.>", "_INBOX.>"},
			SubAllow: []string{"ap.>", "_INBOX.>"},
		})

		sub, err := responder.Subscribe(subject, func(m *natsgo.Msg) {
			_ = m.Respond([]byte("pong"))
		})
		require.NoError(t, err, "responder subscribe")
		require.NoError(t, responder.Flush(), "responder flush subscription")
		t.Cleanup(func() { _ = sub.Unsubscribe() })

		_, err = requester.Request(subject, []byte("ping"), 1*time.Second)
		require.Error(t, err, "Request must fail when responder cannot publish to _INBOX")
		// nats.go surfaces this as ErrTimeout (the reply was denied,
		// nothing ever lands on the inbox) or ErrNoResponders.
		assert.True(t,
			err == natsgo.ErrTimeout || err == natsgo.ErrNoResponders,
			"want timeout/no-responders, got %v", err)
	})
}

// TestReplyInboxIsPerPrincipal is the regression test for the cross-session
// read that a shared `_INBOX.` root allows.
//
// channelsd answers read_thread_history over request/reply
// (pkg/channels/channelsd/historyresp). Its handler is careful: it derives the channel
// key from the SESSION NAMED IN THE SUBJECT, never from the payload, so a
// runner can only ASK for the thread its own session is bound to — its grant
// scopes the request subject to its own session subtree.
//
// The reply does not travel on that subject, though — it travels on the
// requester's auto-inbox. With every principal's inbox under one `_INBOX.` root
// and every grant allowing `_INBOX.>`, a runner for session B can subscribe to
// the root and read session A's channel history off the wire, reaching around
// the side of the check the responder performs.
//
// A per-principal inbox prefix derived from the grant's own Name closes it, on
// both sides of the wire: the minted SubAllow entry (InboxSubjectFor) and the
// client's CustomInboxPrefix (buildOptions, from the Name claim in the creds
// file). One function, one input, so the grant and the client cannot disagree.
func TestReplyInboxIsPerPrincipal(t *testing.T) {
	id, err := GenerateIdentity()
	require.NoError(t, err, "generate NATS identity")
	srv := startJWTServer(t, id)

	// channelsd: the history responder. Broad `_INBOX.>` on PUBLISH is correct
	// and stays — a responder must be able to reply into whichever principal
	// asked, and every narrowed prefix is still under the `_INBOX.` root.
	responder := connectAs(t, srv, id, UserGrant{
		Name:     "channelsd",
		PubAllow: []string{"ap.>", "_INBOX.>"},
		SubAllow: []string{"ap.>", InboxSubjectFor("channelsd")},
	})

	const (
		sessionA = "ap.session.default.session-a"
		sessionB = "ap.session.default.session-b"
		secret   = "session A private thread history"
	)
	sub, err := responder.Subscribe(sessionA+".history.request", func(m *natsgo.Msg) {
		_ = m.Respond([]byte(secret))
	})
	require.NoError(t, err, "responder subscribe")
	require.NoError(t, responder.Flush(), "responder flush subscription")
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// Two runners, minted exactly as the AgentSession reconciler mints them.
	runnerA := connectAs(t, srv, id, runnerGrantForTest(t, "session-a"))
	runnerB := connectAs(t, srv, id, runnerGrantForTest(t, "session-b"))

	// runner B tries to read every reply on the bus.
	eavesdrop, err := runnerB.SubscribeSync("_INBOX.>")
	require.NoError(t, err, "Subscribe returns nil even when the server refuses it")
	require.NoError(t, runnerB.Flush(), "flush so the server has ruled on the subscription")

	// runner A asks for its own history and gets it. This must keep working:
	// a narrowed inbox that breaks request/reply is worse than the over-grant.
	reply, err := runnerA.Request(sessionA+".history.request", []byte("ping"), 3*time.Second)
	require.NoError(t, err, "runner A must still be able to read its OWN history")
	assert.Equal(t, secret, string(reply.Data), "reply round-trips to the requester")

	// runner B must not have seen it.
	leaked, err := eavesdrop.NextMsg(500 * time.Millisecond)
	if err == nil {
		t.Fatalf("runner B read session A's reply off the shared inbox root: %q", string(leaked.Data))
	}
	assert.ErrorIs(t, err, natsgo.ErrTimeout,
		"want no message on B's eavesdrop subscription")
	assert.ErrorIs(t, runnerB.LastError(), natsgo.ErrPermissionViolation,
		"the server must have REFUSED the _INBOX.> subscription outright, not merely failed to route to it")

	// Sanity: B's own inbox still works, so the refusal above is scoping and
	// not a blanket loss of request/reply for B.
	sub2, err := responder.Subscribe(sessionB+".history.request", func(m *natsgo.Msg) {
		_ = m.Respond([]byte("session B history"))
	})
	require.NoError(t, err, "responder subscribe for B")
	require.NoError(t, responder.Flush(), "responder flush B subscription")
	t.Cleanup(func() { _ = sub2.Unsubscribe() })

	replyB, err := runnerB.Request(sessionB+".history.request", []byte("ping"), 3*time.Second)
	require.NoError(t, err, "runner B must still be able to read its own history")
	assert.Equal(t, "session B history", string(replyB.Data))
}

// runnerGrantForTest mirrors runnerNATSUserGrant in the AgentSession
// reconciler. Duplicated rather than imported: pkg/controllers/agentsession
// imports pkg/platform/nats, so importing it back here would be an import cycle. The
// reconciler's own test asserts the real grant's shape against the same
// natstest harness.
func runnerGrantForTest(t *testing.T, session string) UserGrant {
	t.Helper()
	prefix := "ap.session.default." + session
	// Built with PrincipalName for the same reason the reconciler does: a
	// hand-joined "runner-default-"+session is exactly the non-injective shape
	// that let two sessions share one inbox, and a mirror that keeps the old
	// join would let this test go on passing after the real grant regressed.
	name, err := PrincipalName("runner", "default", session)
	require.NoError(t, err, "principal name for session %q", session)
	return UserGrant{
		Name: name,
		PubAllow: []string{
			prefix + ".out.>",
			prefix + ".history.request",
			"_INBOX.>",
		},
		SubAllow: []string{prefix + ".>", InboxSubjectFor(name)},
	}
}
