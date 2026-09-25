package probe

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSessionKey pins what may and may not share one persistent MCP session.
//
// The key carries two obligations at once, and an earlier revision satisfied only
// the first. It is the whole guard against two MCPServer CRs on one endpoint
// inheriting each other's credential — so a different credential must key
// differently. But it also has to survive that credential's routine lifecycle: an
// OAuth value is refreshed by the operator and by authTransport's own
// reauth-on-401, and a key that moved with the bytes made every refresh a cache
// miss — a second MCP session, the server-side state gone, the superseded entry
// left holding a live session. Hence identity, not value.
func TestSessionKey(t *testing.T) {
	const (
		url  = "https://mcp.example.test/mcp"
		hdr  = "Authorization"
		high = "cred/high-privilege"
		low  = "cred/low-privilege"
	)
	cases := []struct {
		name     string
		a, b     [3]string // {url, header, credID}
		wantSame bool
	}{
		{"same url+header+credential: one session, reused", [3]string{url, hdr, high}, [3]string{url, hdr, high}, true},
		{"same url, different credential: separate sessions", [3]string{url, hdr, high}, [3]string{url, hdr, low}, false},
		{"same url+credential, different header name: separate sessions", [3]string{url, hdr, high}, [3]string{url, "X-Api-Key", high}, false},
		{"same credential, different url: separate sessions", [3]string{url, hdr, high}, [3]string{"https://other.example.test/mcp", hdr, high}, false},
		{"no credential on either: one shared unauthenticated session", [3]string{url, "", ""}, [3]string{url, "", ""}, true},
		{"credential vs none on one url: separate sessions", [3]string{url, hdr, high}, [3]string{url, "", ""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ka := sessionKey(tc.a[0], tc.a[1], tc.a[2])
			kb := sessionKey(tc.b[0], tc.b[1], tc.b[2])
			assert.Equal(t, tc.wantSame, ka == kb)
		})
	}

	t.Run("no credential value reaches the key at all", func(t *testing.T) {
		const token = "Bearer sk-the-actual-secret"
		k := sessionKey(url, hdr, high)
		assert.NotContains(t, k, token, "a bearer token in a map key can leak through any dump of the cache")
		assert.NotContains(t, k, strings.TrimPrefix(token, "Bearer "))
		assert.Equal(t, k, sessionKey(url, hdr, high),
			"the key is a pure function of (url, header, credential identity) — nothing per-call feeds it")
	})
}

// TestClassifyFailure pins the only question the cached-session retry is allowed
// to ask: did the server dispatch this tools/call? Re-issuing is safe ONLY when
// the answer is a provable no.
func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		name       string
		postStatus int
		want       failureKind
	}{
		{"200 with a JSON-RPC error member: server dispatched, must not re-issue", http.StatusOK, serverDispatched},
		{"202 accepted: server dispatched, must not re-issue", http.StatusAccepted, serverDispatched},
		{"404 session terminated (MCP §2.5.3): refused before dispatch, safe to re-issue", http.StatusNotFound, sessionGone},
		{"no response at all: undetermined, must not re-issue", 0, undetermined},
		{"500: undetermined, must not re-issue", http.StatusInternalServerError, undetermined},
		{"502 from a proxy: undetermined, must not re-issue", http.StatusBadGateway, undetermined},
		{"401: undetermined, must not re-issue", http.StatusUnauthorized, undetermined},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyFailure(tc.postStatus))
		})
	}
}
