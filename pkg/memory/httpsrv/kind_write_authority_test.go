package httpsrv_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all" // register every Kind
	"github.com/authzed/openagentprimitives/pkg/memory/sentinel"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// These tests drive the REAL HTTP path with a real per-session bearer, because
// that is the only caller class the defect had: a session token minted
// ForBearerToken(WriteMemory, ns/name) for ANY registered Kind, so a runner
// could author every component-owned record in its own scope —
// `cold_start_task` (which authzd reads as "this session's scope review is
// already decided" and the runner blocks on), the metaagent audit trail,
// channelsd's parked prompts. A unit test against Local.Put alone would not
// have caught it, because the capability that identifies the door is attached
// by the handler.

const (
	authorityNS      = "ns"
	authoritySession = "sess-a"
	authorityBundle  = "bundle"
	authorityToken   = "tok-session"
	authoritySysTok  = "tok-channelsd"
)

// newAuthorityServer builds the operator's shape MINUS the provenance verifier,
// so an append-only Kind can be POSTed unsigned. That isolation is the point:
// with verify-on-write configured, a refused append-only write is ambiguous
// between "unsigned" and "this credential may not author this Kind", and only
// the second is under test here. The mutable Kinds have no such confound.
func newAuthorityServer(t *testing.T) (*httptest.Server, *memory.Local, *tokens.Registry) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	srv := httptest.NewServer(httpsrv.NewHandler(mem, reg, httpsrv.WithDeleter(mem)))
	t.Cleanup(srv.Close)
	// The production registration shape: the session's own path plus the
	// per-bundle SpiceboxSession extras the AgentSession reconciler passes.
	reg.Set(memory.NamespacedName{Namespace: authorityNS, Name: authoritySession}, authorityToken, "",
		memory.NamespacedName{Namespace: authorityNS, Name: authorityBundle})
	reg.SetChannelsdToken(authoritySysTok)
	return srv, mem, reg
}

// postKind POSTs a minimal Entry of kind into {ns}/{name}. ID is left empty so
// putEntry mints one with the Kind's own prefix — the same thing every real
// client relies on.
func postKind(t *testing.T, srv *httptest.Server, token, kind, ns, name string) *http.Response {
	t.Helper()
	body, err := json.Marshal(memory.Entry{Content: json.RawMessage(`{}`)})
	require.NoError(t, err, "marshal Entry")
	req, err := http.NewRequest(http.MethodPost,
		srv.URL+"/memory/"+kind+"/"+ns+"/"+name, bytes.NewReader(body))
	require.NoError(t, err, "NewRequest POST %s", kind)
	req.Header.Set("Content-Type", "application/json")
	return do(t, authed(req, token))
}

// deleteEntry issues the per-entry DELETE the `oap memory delete` path uses.
func deleteEntry(t *testing.T, srv *httptest.Server, token, kind, id, ns, name string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete,
		srv.URL+"/memory/_entry/"+ns+"/"+name+"?kind="+kind+"&id="+id, nil)
	require.NoError(t, err, "NewRequest DELETE _entry")
	return do(t, authed(req, token))
}

// TestSessionToken_MayAuthorOnlySessionWrittenKinds sweeps EVERY registered
// Kind through the session-bearer POST path and requires the answer to be the
// Kind's own declaration — 201 for SessionWritten, 403 for ComponentWritten.
//
// Sweeping the registry rather than listing kinds is deliberate: a list would
// cover the kinds that existed when it was written, which is exactly the
// failure mode the WriteAuthority declaration exists to prevent. A Kind added
// later is tested the day it is registered.
func TestSessionToken_MayAuthorOnlySessionWrittenKinds(t *testing.T) {
	srv, _, _ := newAuthorityServer(t)

	kinds := memory.RegisteredKinds()
	require.NotEmpty(t, kinds, "no Kinds registered — every assertion below would be vacuous")

	sawComponent, sawSession := false, false
	for _, k := range kinds {
		want, wantSentinel := http.StatusCreated, ""
		if k.WriteAuthority() != memory.SessionWritten {
			want, wantSentinel = http.StatusForbidden, "kind_not_session_writable"
			sawComponent = true
		} else {
			sawSession = true
		}
		t.Run(k.Name()+" ("+k.WriteAuthority().String()+"): POST by a session bearer", func(t *testing.T) {
			resp := postKind(t, srv, authorityToken, k.Name(), authorityNS, authoritySession)
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			assert.Equal(t, want, resp.StatusCode, "POST %s body=%s", k.Name(), b)
			assert.Equal(t, wantSentinel, resp.Header.Get(sentinel.Header),
				"the refusal must be nameable on the wire so the caller does not read it as a transient fault")
		})
	}

	// Both arms have to be populated, or the sweep is asserting one thing.
	assert.True(t, sawComponent, "no component-written Kind is registered; the refusal arm never ran")
	assert.True(t, sawSession, "no session-written Kind is registered; the permit arm never ran")
}

// TestComponentWrittenKind_OtherDoorsStillWrite pins the half that a
// too-enthusiastic fix breaks. The gate is on the SESSION-CREDENTIAL door only:
// the component that legitimately owns cold_start_task (authzd, here standing
// in as the equivalent system channelsd bearer) and the operator writing
// in-process through its own facade must be untouched.
func TestComponentWrittenKind_OtherDoorsStillWrite(t *testing.T) {
	srv, mem, _ := newAuthorityServer(t)

	t.Run("a system component bearer: 201 Created", func(t *testing.T) {
		resp := postKind(t, srv, authoritySysTok, "cold_start_task", authorityNS, authoritySession)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		assert.Equal(t, http.StatusCreated, resp.StatusCode, "POST cold_start_task body=%s", b)
	})

	t.Run("the operator writing in-process: no error", func(t *testing.T) {
		k, ok := memory.LookupKind("cold_start_task")
		require.True(t, ok, "cold_start_task must be registered")
		ctx := memory.WithSystemApproval(context.Background(), "system:operator")
		_, err := mem.Put(ctx, memory.Entry{
			Scope:   memory.Scope{Kind: "session", ID: authorityNS + "/" + authoritySession},
			Kind:    k.Name(),
			ID:      memory.NewID(k),
			Content: json.RawMessage(`{}`),
		})
		assert.NoError(t, err, "an in-process write carries no token-session mark and must not be gated")
	})
}

// TestSessionToken_MayNotDeleteAComponentWrittenKind covers the door the Put
// gate alone leaves open. cold_start_task is mutable, so per-entry DELETE is
// permitted for its owner — and authzd's coldStartAlreadyDecided reads the
// entry's mere EXISTENCE as "this session's cold start is settled". A runner
// that could delete it replays the review it is not allowed to re-request.
func TestSessionToken_MayNotDeleteAComponentWrittenKind(t *testing.T) {
	srv, mem, _ := newAuthorityServer(t)
	scope := memory.Scope{Kind: "session", ID: authorityNS + "/" + authoritySession}

	k, ok := memory.LookupKind("cold_start_task")
	require.True(t, ok, "cold_start_task must be registered")
	id := memory.NewID(k)
	_, err := mem.Put(memory.WithSystemApproval(context.Background(), "system:authzd"), memory.Entry{
		Scope: scope, Kind: k.Name(), ID: id, Content: json.RawMessage(`{}`),
	})
	require.NoError(t, err, "seed the component-written entry the session must not be able to remove")

	t.Run("component-written kind: 403 Forbidden", func(t *testing.T) {
		resp := deleteEntry(t, srv, authorityToken, "cold_start_task", id, authorityNS, authoritySession)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, "DELETE cold_start_task body=%s", b)

		_, found, gErr := mem.Get(context.Background(), scope, "cold_start_task", id)
		require.NoError(t, gErr, "Get after the refused delete")
		assert.True(t, found, "the refused delete must not have removed the entry")
	})

	t.Run("session-written kind: 204 No Content", func(t *testing.T) {
		lk, lok := memory.LookupKind("label")
		require.True(t, lok, "label must be registered")
		resp := deleteEntry(t, srv, authorityToken, "label", memory.NewID(lk), authorityNS, authoritySession)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		assert.Equal(t, http.StatusNoContent, resp.StatusCode,
			"deleting a kind the session authors must still work, body=%s", b)
	})
}

// TestBundleExtraScope_ReadableNotWritable pins the contract
// tokens.Registry.Set documents and nothing enforced: the per-bundle
// SpiceboxSession scopes a runner's token is registered with exist so it can
// FETCH artifacts its ToolCalls produced there. httpsrv answered every method
// with Authorizes, so the same registration also let the runner Put and DELETE
// across every bundle scope — a strictly wider surface than its own session,
// which no code path in the runner uses.
func TestBundleExtraScope_ReadableNotWritable(t *testing.T) {
	srv, _, _ := newAuthorityServer(t)

	t.Run("GET an extra scope: 200 OK", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet,
			srv.URL+"/memory/artifact/"+authorityNS+"/"+authorityBundle, nil)
		require.NoError(t, err, "NewRequest GET artifact")
		resp := do(t, authed(req, authorityToken))
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		assert.Equal(t, http.StatusOK, resp.StatusCode,
			"artifact reads across bundle scopes are why the extras exist, body=%s", b)
	})

	t.Run("POST a session-written kind into an extra scope: 403 Forbidden", func(t *testing.T) {
		resp := postKind(t, srv, authorityToken, "turn", authorityNS, authorityBundle)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode,
			"a kind the session may author in its OWN scope is still not authorable in a foreign one, body=%s", b)
	})

	t.Run("DELETE in an extra scope: 403 Forbidden", func(t *testing.T) {
		resp := deleteEntry(t, srv, authorityToken, "label", "label-x", authorityNS, authorityBundle)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("POST into the token's own scope: 201 Created", func(t *testing.T) {
		resp := postKind(t, srv, authorityToken, "turn", authorityNS, authoritySession)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		assert.Equal(t, http.StatusCreated, resp.StatusCode,
			"the read/write split must not have cost the token its own session, body=%s", b)
	})
}
