package httpsrv_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// These tests drive the REAL HTTP path — an httptest.Server over a Local with
// verify-on-write enabled and a tokens.Registry as the publisher-key lookup,
// which is the operator's own shape (internal/cmd/operator/main.go) — because the
// publisher↔writer binding is only meaningful for a caller the production path
// can actually produce. A unit test that hands VerifyEntry a hand-built caller
// string proves nothing about the runner, whose token registers with an EMPTY
// callerID (agentsession/controller.go) and therefore reaches the verifier with
// no caller at all.

// signingKey returns a deterministic Ed25519 key so a test's publisher
// identities are stable across runs.
func signingKey(t *testing.T, seed byte) ed25519.PrivateKey {
	t.Helper()
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = seed
	}
	return ed25519.NewKeyFromSeed(s)
}

// registerSigner builds a Signer for publisher and registers its public half in
// reg, so entries it signs pass the key lookup. Mirrors what the AgentSession
// reconciler does per session and what /memory/_publisher_key does per
// component: both land in the same lookup the verifier consults.
func registerSigner(t *testing.T, reg *tokens.Registry, publisher string, seed byte) *provenance.Signer {
	t.Helper()
	priv := signingKey(t, seed)
	s := provenance.NewSigner(priv, publisher)
	reg.SetPublisherKey(publisher, s.KeyID(), priv.Public().(ed25519.PublicKey))
	return s
}

// newVerifyingServer builds a server whose Local ENFORCES verify-on-write, with
// the token registry doubling as the publisher-key lookup.
func newVerifyingServer(t *testing.T) (*httptest.Server, *memory.Local, *tokens.Registry) {
	t.Helper()
	reg := tokens.NewRegistry()
	mem := memory.NewLocal(inmem.NewBackend(),
		memory.WithProvenanceVerifier(provenance.NewWriteVerifier(reg)))
	srv := httptest.NewServer(httpsrv.NewHandler(mem, reg))
	t.Cleanup(srv.Close)
	return srv, mem, reg
}

// postSignedTurn signs a turn entry for the scope the URL will resolve to, then
// POSTs it. Signing AFTER fixing scope/kind/id matters: putEntry overwrites all
// three from the URL, and the digest covers them.
func postSignedTurn(t *testing.T, srv *httptest.Server, token, ns, name, id string, signer *provenance.Signer) *http.Response {
	t.Helper()
	content, err := json.Marshal(map[string]string{"text": "hello"})
	require.NoError(t, err, "marshal turn content")
	e := memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: ns + "/" + name},
		Kind:      "turn",
		ID:        id,
		CreatedAt: time.Unix(1770000000, 0).UTC(),
		Content:   content,
	}
	require.NoError(t, signer.Sign(&e), "sign the turn entry")

	body, err := json.Marshal(e)
	require.NoError(t, err, "marshal signed entry")
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/memory/turn/"+ns+"/"+name, bytes.NewReader(body))
	require.NoError(t, err, "NewRequest POST turn")
	req.Header.Set("Content-Type", "application/json")
	return do(t, authed(req, token))
}

// TestAppendOnlyPut_SessionTokenMayOnlyAuthorItsOwnPublisher pins the binding
// the WriteVerifier doc comment promises, on the caller class that actually
// exercises it.
//
// The publisher binding is the layer that says WHO a write may be attributed
// to, independently of which scope it lands in. Without it the only thing
// standing between a session bearer and an entry forged under another
// publisher's name is possession of that publisher's signature — so any key
// compromise or key-registration tamper upgrades straight to "author as that
// publisher".
//
// Every case here writes into the token's OWN session. The token is still
// registered with a per-bundle extra scope, because that is the production
// shape, but writing into an extra is now refused at the scope door before the
// binding is ever consulted — see TestBundleExtraScope_ReadableNotWritable in
// kind_write_authority_test.go, which owns that fact. Asserting the binding
// through a bundle-scope write, as this test used to, would now pass for the
// wrong reason.
func TestAppendOnlyPut_SessionTokenMayOnlyAuthorItsOwnPublisher(t *testing.T) {
	const ns = "ns"
	srv, _, reg := newVerifyingServer(t)

	// One per-session token: session "sess-a", additionally authorized for the
	// per-bundle scope "bundle" exactly as the AgentSession reconciler does.
	reg.Set(memory.NamespacedName{Namespace: ns, Name: "sess-a"}, "tok-a", "",
		memory.NamespacedName{Namespace: ns, Name: "bundle"})

	// Three REGISTERED publishers. Registration is the point: each of these
	// signatures verifies, so the only thing that can refuse the foreign ones is
	// the writer binding.
	own := registerSigner(t, reg, provenance.SessionPublisher(ns, "sess-a"), 0x01)
	otherSession := registerSigner(t, reg, provenance.SessionPublisher(ns, "sess-b"), 0x02)
	component := registerSigner(t, reg, "system:channelsd", 0x03)

	cases := []struct {
		name       string
		scopeName  string
		signer     *provenance.Signer
		entryID    string
		wantStatus int
	}{
		{
			name:       "own publisher into own scope: 201 Created",
			scopeName:  "sess-a",
			signer:     own,
			entryID:    "turn-0-user",
			wantStatus: http.StatusCreated,
		},
		{
			name:       "another session's registered publisher: 403 Forbidden",
			scopeName:  "sess-a",
			signer:     otherSession,
			entryID:    "turn-2-user",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "a component's registered publisher: 403 Forbidden",
			scopeName:  "sess-a",
			signer:     component,
			entryID:    "turn-3-user",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postSignedTurn(t, srv, "tok-a", ns, tc.scopeName, tc.entryID, tc.signer)
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			assert.Equal(t, tc.wantStatus, resp.StatusCode, "POST turn body=%s", b)
		})
	}
}

// TestAppendOnlyPut_SystemTokenBindingUnchanged guards the other half: the
// system tokens carry a caller and must keep being bound to it, so tightening
// the session-token path cannot have loosened theirs.
func TestAppendOnlyPut_SystemTokenBindingUnchanged(t *testing.T) {
	const ns = "ns"
	srv, _, reg := newVerifyingServer(t)
	reg.SetChannelsdToken("tok-channelsd")

	channelsd := registerSigner(t, reg, "system:channelsd", 0x03)
	foreign := registerSigner(t, reg, provenance.SessionPublisher(ns, "sess-a"), 0x01)

	t.Run("channelsd signs as itself: 201 Created", func(t *testing.T) {
		resp := postSignedTurn(t, srv, "tok-channelsd", ns, "sess-a", "turn-0-user", channelsd)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		assert.Equal(t, http.StatusCreated, resp.StatusCode, "POST turn body=%s", b)
	})

	t.Run("channelsd signs as a session publisher: 403 Forbidden", func(t *testing.T) {
		resp := postSignedTurn(t, srv, "tok-channelsd", ns, "sess-a", "turn-1-user", foreign)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

// TestSignalRoute_LifecycleHookWritesAsTheOperatorNotTheTokenHolder is the
// regression guard for the binding's blast radius. Local.SendSignal fans a
// signal out to every Kind's ScopeHooks on the REQUEST's context, and the
// lifecycle hook writes an APPEND-ONLY entry through the operator's own signing
// facade (publisher system:operator). Authorship hands off there — the entry is
// the operator's testimony about a signal it received, not the token holder's
// write — so a binding that followed the request context into the hook would
// refuse every lifecycle entry a runner's signal produces, which is every one of
// them in production.
func TestSignalRoute_LifecycleHookWritesAsTheOperatorNotTheTokenHolder(t *testing.T) {
	const ns = "ns"
	srv, mem, reg := newVerifyingServer(t)
	reg.Set(memory.NamespacedName{Namespace: ns, Name: "sess-a"}, "tok-a", "")

	// The operator's own signer, exactly as internal/cmd/operator wires lifecycle.Setup.
	opSigner := registerSigner(t, reg, "system:operator", 0x04)
	lifecycleSetup(t, provenance.NewSigningMemory(mem, opSigner))

	sig := memory.Signal{Kind: "lifecycle/session.started", At: time.Unix(1770000000, 0).UTC()}
	body, err := json.Marshal(sig)
	require.NoError(t, err, "marshal Signal")
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/memory/_signal/"+ns+"/sess-a", bytes.NewReader(body))
	require.NoError(t, err, "NewRequest POST _signal")
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, authed(req, "tok-a"))
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "_signal → 204, body=%s", b)

	res, qErr := mem.Query(memory.WithSystemApproval(context.Background(), "test"), memory.Query{
		Scope: memory.Scope{Kind: "session", ID: ns + "/sess-a"}, Kinds: []string{"lifecycle"},
	})
	require.NoError(t, qErr, "Query lifecycle")
	require.Len(t, res.Entries, 1, "the operator-signed lifecycle entry was recorded")
	require.NotNil(t, res.Entries[0].Provenance)
	assert.Equal(t, "system:operator", res.Entries[0].Provenance.Publisher,
		"the hook write is attributed to the operator, not to the session whose token triggered it")
}
