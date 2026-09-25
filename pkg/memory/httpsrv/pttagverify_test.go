package httpsrv_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttagcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// storeContent writes a pt_tag_content record COMPONENT-side (no token on the
// ctx), the same shape the minter's writeContent produces. The test store has no
// provenance verifier, so an unsigned component write is accepted — the point
// under test is the READ door, not the write signature.
func storeContent(t *testing.T, mem memory.Memory, scope memory.Scope, tagID, content string) {
	t.Helper()
	body, err := json.Marshal(pttagcontent.ContentRecord{
		TagID:    tagID,
		Content:  content,
		MIME:     "application/json",
		StoredAt: time.Unix(0, 0).UTC(),
	})
	require.NoError(t, err)
	_, err = mem.Put(context.Background(), memory.Entry{
		Scope:   scope,
		Kind:    pttagcontent.KindName,
		ID:      memory.NewID(pttagcontent.Kind{}),
		Content: body,
	})
	require.NoError(t, err)
}

func postVerify(t *testing.T, h http.Handler, token, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func newVerifyHandler(t *testing.T) (http.Handler, memory.Memory, string) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: "nsA", Name: "sessA"}, "tok-1", "")
	return httpsrv.NewHandler(mem, reg), mem, "tok-1"
}

// TestVerifyRouteBindsMatchingContentDespiteTheSessionToken is the regression
// for the live-only failure that kept per-datum egress on the coarse floor.
//
// The request arrives on a per-session bearer, and pt_tag_content is
// component-read (SessionReadable false), so a handler that read it under the
// request's own token would be refused by the per-kind read door — exactly what
// happened when the runner tried to read it directly over HTTP. This route reads
// component-side, after the router has authorized the scope, so a matching
// region binds and a fabricated one does not.
func TestVerifyRouteBindsMatchingContentDespiteTheSessionToken(t *testing.T) {
	h, mem, token := newVerifyHandler(t)
	scope := memory.Scope{Kind: "session", ID: "nsA/sessA"}
	storeContent(t, mem, scope, "ptt-real", `{"amount":"1890.00"}`)

	w := postVerify(t, h, token, "/memory/_pttag_verify/nsA/sessA",
		`{"regions":[{"id":"ptt-real","content":"{\"amount\":\"1890.00\"}"}]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp memory.PtTagVerifyResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.AllBound, "a region whose bytes match the stored content must bind")
	assert.Equal(t, []string{"ptt-real"}, resp.Verified)
}

// TestVerifyRouteRefusesAFabricatedRegion is the security half: a region that
// pairs a real (witnessed) id with bytes the platform never stored under it must
// NOT bind, and its presence drops the whole payload's coverage — the caller
// then falls to the coarse floor rather than honour a partial forge.
func TestVerifyRouteRefusesAFabricatedRegion(t *testing.T) {
	h, mem, token := newVerifyHandler(t)
	scope := memory.Scope{Kind: "session", ID: "nsA/sessA"}
	storeContent(t, mem, scope, "ptt-real", `{"amount":"1890.00"}`)

	w := postVerify(t, h, token, "/memory/_pttag_verify/nsA/sessA",
		`{"regions":[{"id":"ptt-real","content":"FABRICATED wide-audience text"}]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp memory.PtTagVerifyResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.AllBound, "content that does not match the stored bytes must not bind")
	assert.Empty(t, resp.Verified)
}

// TestVerifyRouteReturnsIdsNeverContent pins that the response is a provenance
// confirmation, not a read-back channel: it carries ids, never the stored bytes
// a session may not read.
func TestVerifyRouteReturnsIdsNeverContent(t *testing.T) {
	h, mem, token := newVerifyHandler(t)
	scope := memory.Scope{Kind: "session", ID: "nsA/sessA"}
	storeContent(t, mem, scope, "ptt-real", `secret-bytes-the-session-must-not-read-back`)

	w := postVerify(t, h, token, "/memory/_pttag_verify/nsA/sessA",
		`{"regions":[{"id":"ptt-real","content":"secret-bytes-the-session-must-not-read-back"}]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "secret-bytes",
		"verify confirms provenance; it must never echo stored content back to the caller")
}

// TestVerifyRouteRejectsAnUnknownField mirrors the mint route: a typo'd key must
// fail loudly rather than be silently dropped into a wrong verification.
func TestVerifyRouteRejectsAnUnknownField(t *testing.T) {
	h, _, token := newVerifyHandler(t)

	w := postVerify(t, h, token, "/memory/_pttag_verify/nsA/sessA",
		`{"regions":[{"id":"ptt-real","content":"x"}],"regons":[]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
