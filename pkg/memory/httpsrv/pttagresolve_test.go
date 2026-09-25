package httpsrv_test

import (
	"context"
	"encoding/json"
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

// fakeAccess grants access only to the tag ids in `allow`.
type fakeAccess struct {
	allow map[string]bool
	err   error
	asked []string
}

func (f *fakeAccess) HasTagAccess(_ context.Context, tagID string, _ memory.NamespacedName) (bool, error) {
	f.asked = append(f.asked, tagID)
	if f.err != nil {
		return false, f.err
	}
	return f.allow[tagID], nil
}

func newResolveHandler(t *testing.T, acc httpsrv.PtTagAccessChecker) (http.Handler, memory.Memory, string) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: "nsA", Name: "sessA"}, "tok-1", "")
	if acc == nil {
		return httpsrv.NewHandler(mem, reg), mem, "tok-1"
	}
	return httpsrv.NewHandler(mem, reg, httpsrv.WithPtTagResolver(acc)), mem, "tok-1"
}

func postResolve(t *testing.T, h http.Handler, token, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestResolveReturnsEntitledContentDespiteTheSessionToken is the regression for
// the same class as the mint/verify bugs: the request arrives on a per-session
// bearer and pt_tag_content is component-read, so a handler reading it under the
// request's own token would be refused. This route reads component-side, gates
// each tag on the caller's access, and returns the entitled bytes.
func TestResolveReturnsEntitledContentDespiteTheSessionToken(t *testing.T) {
	acc := &fakeAccess{allow: map[string]bool{"ptt-ok": true}}
	h, mem, token := newResolveHandler(t, acc)
	scope := memory.Scope{Kind: "session", ID: "nsA/sessA"}
	storeContent(t, mem, scope, "ptt-ok", `{"datum":"entitled"}`)

	w := postResolve(t, h, token, "/memory/_pttag_resolve/nsA/sessA", `{"tagIDs":["ptt-ok"]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp memory.PtTagResolveResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Contents, 1)
	assert.Equal(t, "ptt-ok", resp.Contents[0].TagID)
	assert.Equal(t, `{"datum":"entitled"}`, resp.Contents[0].Content)
}

// TestResolveOmitsUnentitledContent is the security half: a tag whose bytes are
// stored but which the caller lacks access to must NOT be returned — the route
// must never be a read-back channel for arbitrary pt_tag_content.
func TestResolveOmitsUnentitledContent(t *testing.T) {
	acc := &fakeAccess{allow: map[string]bool{"ptt-ok": true}} // ptt-secret NOT allowed
	h, mem, token := newResolveHandler(t, acc)
	scope := memory.Scope{Kind: "session", ID: "nsA/sessA"}
	storeContent(t, mem, scope, "ptt-ok", "entitled bytes")
	storeContent(t, mem, scope, "ptt-secret", "bytes the caller may not have")

	w := postResolve(t, h, token, "/memory/_pttag_resolve/nsA/sessA", `{"tagIDs":["ptt-ok","ptt-secret"]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "may not have",
		"content the caller lacks access to must never be returned")

	var resp memory.PtTagResolveResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Contents, 1)
	assert.Equal(t, "ptt-ok", resp.Contents[0].TagID)
}

// TestResolveAccessCheckErrorFailsClosed: an entitlement check that errors must
// fail the whole call, not hand back bytes on an unverified entitlement.
func TestResolveAccessCheckErrorFailsClosed(t *testing.T) {
	acc := &fakeAccess{err: assertErr("spicedb down")}
	h, mem, token := newResolveHandler(t, acc)
	scope := memory.Scope{Kind: "session", ID: "nsA/sessA"}
	storeContent(t, mem, scope, "ptt-ok", "bytes")

	w := postResolve(t, h, token, "/memory/_pttag_resolve/nsA/sessA", `{"tagIDs":["ptt-ok"]}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"a broken entitlement check must fail closed, not silently omit or return")
}

// TestResolveIsA405WithoutAResolver distinguishes "not offered" from "refused".
func TestResolveIsA405WithoutAResolver(t *testing.T) {
	h, _, token := newResolveHandler(t, nil)
	w := postResolve(t, h, token, "/memory/_pttag_resolve/nsA/sessA", `{"tagIDs":["ptt-ok"]}`)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

type assertErr string

func (e assertErr) Error() string { return string(e) }
