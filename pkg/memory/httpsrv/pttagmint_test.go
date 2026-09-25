package httpsrv_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// recordingMinter captures what the SERVER handed it, so a test can assert on
// what actually crossed the wire rather than on what the client meant to send.
type recordingMinter struct {
	got   memory.PtTagMintRequest
	scope memory.Scope
	id    string
	err   error
}

func (m *recordingMinter) MintPtTag(_ context.Context, scope memory.Scope, req memory.PtTagMintRequest) (string, error) {
	m.got, m.scope = req, scope
	return m.id, m.err
}

// newTestHandlerWithMinter builds a handler over an in-memory store with one
// per-session token. A nil minter leaves the route unwired, which is the
// default every existing deployment has.
func newTestHandlerWithMinter(t *testing.T, m httpsrv.PtTagMinter) (http.Handler, string) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: "nsA", Name: "sessA"}, "tok-1", "")
	if m == nil {
		return httpsrv.NewHandler(mem, reg), "tok-1"
	}
	return httpsrv.NewHandler(mem, reg, httpsrv.WithPtTagMinter(m)), "tok-1"
}

func postMint(t *testing.T, h http.Handler, token, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestMintRouteCarriesResourcesAndScopeFromTheURL pins the two facts the
// server is responsible for: the resources reach the minter intact, and the
// SCOPE comes from the URL rather than from anything in the body.
//
// Scope-from-URL matters because the body is caller-supplied: a request that
// could name its own scope would let a session mint tags into another
// session's provenance, where a later disclosure would resolve against them.
func TestMintRouteCarriesResourcesAndScopeFromTheURL(t *testing.T) {
	m := &recordingMinter{id: "ptt-1"}
	h, token := newTestHandlerWithMinter(t, m)

	w := postMint(t, h, token, "/memory/_pttag_mint/nsA/sessA",
		`{"toolUseID":"toolu_1","resources":[{"type":"doc","id":"d1","permission":"viewer"}]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "ptt-1")

	assert.Equal(t, "nsA/sessA", m.scope.ID, "the scope must come from the URL, never the body")
	require.Len(t, m.got.Resources, 1)
	assert.Equal(t, "doc", m.got.Resources[0].Type)
	assert.Equal(t, "viewer", m.got.Resources[0].Permission)
	assert.Equal(t, "toolu_1", m.got.ToolUseID)
}

// TestMintRouteRejectsAnUnknownField is why the decoder disallows them.
//
// A typo'd key silently dropped is how a caller believes it marked a datum
// untrusted while the minter records it clean — a difference nothing downstream
// could detect.
func TestMintRouteRejectsAnUnknownField(t *testing.T) {
	m := &recordingMinter{id: "ptt-1"}
	h, token := newTestHandlerWithMinter(t, m)

	w := postMint(t, h, token, "/memory/_pttag_mint/nsA/sessA",
		`{"resources":[{"type":"doc","id":"d1","permission":"viewer"}],"untrustedOrgin":true}`)
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"a misspelled field must fail loudly, not be dropped into a wrong-but-plausible record")
}

// TestMintRouteRefusesLeafAndDerivedTogether keeps leaf-XOR-derived enforced at
// the edge as well as in the composer. A tag that were both would be read
// through both arms of `reader`, whose + is a union, and resolve wider than its
// sources allow.
func TestMintRouteRefusesLeafAndDerivedTogether(t *testing.T) {
	m := &recordingMinter{id: "ptt-1"}
	h, token := newTestHandlerWithMinter(t, m)

	w := postMint(t, h, token, "/memory/_pttag_mint/nsA/sessA",
		`{"resources":[{"type":"doc","id":"d1","permission":"viewer"}],"derivedFrom":["ptt-a"]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "never both")
}

// TestMintRouteIsA405WithoutAMinter distinguishes "this platform does not
// offer per-datum provenance" from "you may not do that". A 403 would tell an
// operator they have a permissions problem they do not have.
func TestMintRouteIsA405WithoutAMinter(t *testing.T) {
	h, token := newTestHandlerWithMinter(t, nil)

	w := postMint(t, h, token, "/memory/_pttag_mint/nsA/sessA",
		`{"resources":[{"type":"doc","id":"d1","permission":"viewer"}]}`)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}
