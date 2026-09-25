package agentui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// serveRedirect drives the redirect handler for one (ns, name).
//
// The path values are SET rather than parsed out of a request line, because
// that is what the framework hands the handler: net/http's pattern matcher
// decodes each segment before binding it, so a name containing a space or a
// '%' reaches PathValue in its decoded form. Building the request line from
// the raw pair instead would test the request parser, not this handler — and
// would refuse to build at all for several of the values below.
func serveRedirect(t *testing.T, ns, name string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/agent-ui/ns/name", nil)
	r.SetPathValue("ns", ns)
	r.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	redirectHandler().ServeHTTP(rec, r)
	return rec
}

// TestRedirect_AddressesTheSessionInTheShell covers the whole contract of the
// address /agent-ui/{ns}/{name}: a 302 into the session shell, the SAME answer
// for a session that does not exist, and correct escaping.
//
// The "does not exist" row is the one that matters. This route requires no
// login, so a variant that looked the session up and 404'd on NotFound would
// be an existence oracle: an unauthenticated caller could enumerate which
// sessions exist by watching which addresses answered differently. Asserting
// the status AND the Location — not merely "it redirected" — is what makes
// that row discriminating.
func TestRedirect_AddressesTheSessionInTheShell(t *testing.T) {
	cases := []struct {
		name         string
		ns, session  string
		wantLocation string
	}{
		{
			name: "an ordinary pair redirects into the shell with the session selected",
			ns:   "demo-ns", session: "demo-session",
			wantLocation: "/sessions?session=demo-ns%2Fdemo-session",
		},
		{
			name: "a session that does not exist gets the SAME answer — no lookup, no existence oracle",
			ns:   "demo-ns", session: "no-such-session-anywhere",
			wantLocation: "/sessions?session=demo-ns%2Fno-such-session-anywhere",
		},
		{
			name: "a namespace that does not exist gets the same answer too",
			ns:   "no-such-namespace", session: "demo-session",
			wantLocation: "/sessions?session=no-such-namespace%2Fdemo-session",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveRedirect(t, tc.ns, tc.session)
			assert.Equal(t, http.StatusFound, rec.Code)
			assert.Equal(t, tc.wantLocation, rec.Header().Get("Location"))
		})
	}
}

// TestRedirect_EscapesTheSelection proves the Location's selection PARSES BACK
// to the original pair. A pair is carried in one query parameter, so a value
// containing a character with meaning in a URL — a '/', a '?', a '&' — must not
// be able to alter the target or smuggle a second parameter into it.
//
// The assertion round-trips through url.Parse rather than comparing an
// expected literal: an escaping bug that produced a different-but-still-equal
// literal would pass a string compare and fail here.
func TestRedirect_EscapesTheSelection(t *testing.T) {
	cases := []struct{ ns, session string }{
		{ns: "demo-ns", session: "sess with spaces"},
		{ns: "demo-ns", session: "sess?view=ui&x=1"},
		{ns: "demo/ns", session: "sess#frag"},
		{ns: "demo-ns", session: "sess%2Fdouble"},
	}
	for _, tc := range cases {
		t.Run(tc.ns+"|"+tc.session, func(t *testing.T) {
			rec := serveRedirect(t, tc.ns, tc.session)
			require.Equal(t, http.StatusFound, rec.Code)

			u, err := url.Parse(rec.Header().Get("Location"))
			require.NoError(t, err, "the Location must parse as a URL")
			assert.Equal(t, "/sessions", u.Path, "the target is the shell, whatever the pair contains")
			assert.Equal(t, []string{"session"}, queryKeys(u),
				"the pair must never smuggle a second query parameter into the target")
			assert.Equal(t, tc.ns+"/"+tc.session, u.Query().Get("session"),
				"the selection must decode back to the pair the caller addressed")
		})
	}
}

// queryKeys returns u's query parameter names, sorted-insensitively (there is
// only ever meant to be one).
func queryKeys(u *url.URL) []string {
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	return keys
}

// TestRedirect_AnswerDoesNotDependOnSessionState is the behavioral half of
// "no lookup": three sessions in three different states — one live, one ended,
// one that does not exist at all — must produce three answers that differ only
// in the pair they carry.
//
// A lookup-shaped variant of this handler would answer 410 for the ended one
// and 404 for the missing one, and both of those are exactly the states a
// caller would use to probe. (The structural half is that redirectHandler takes
// no Deps: there is nothing it could read.)
func TestRedirect_AnswerDoesNotDependOnSessionState(t *testing.T) {
	for _, name := range []string{startAttached, startEnded, "no-such-session"} {
		t.Run(name, func(t *testing.T) {
			rec := serveRedirect(t, startNS, name)
			assert.Equal(t, http.StatusFound, rec.Code,
				"every session state answers the same way; what to show for it is the shell's decision, not this address's")
			assert.Equal(t, SessionShellHref(startNS, name), rec.Header().Get("Location"))
		})
	}
}

// TestRedirect_RouteShapeAndTheAPIRoutesItMustNotDisturb pins the redirect's
// own route shape AND the three session-scoped API routes beside it.
//
// The API routes are asserted BY EXACT PATTERN and auth level: they are what
// the agent-defined view calls from inside the shell, and moving or relaxing
// one of them while retiring the page it used to sit beside is the mistake
// this row exists to catch.
func TestRedirect_RouteShapeAndTheAPIRoutesItMustNotDisturb(t *testing.T) {
	d := newStartDeps(t)
	routes := New().Routes(d)

	byPattern := map[string]webui.Route{}
	for _, r := range routes {
		byPattern[r.Pattern] = r
	}

	page, ok := byPattern["/agent-ui/{ns}/{name}"]
	require.True(t, ok, "the address must still be routed")
	assert.Equal(t, webui.AuthNone, page.Auth, "the redirect performs no lookup and discloses nothing")
	assert.Nil(t, page.Page, "it no longer serves a document")
	assert.NotNil(t, page.Handler)
	assert.Equal(t, []string{http.MethodGet}, page.Methods)
	assert.Equal(t, webui.OriginTrusted, page.Origin)

	for _, pattern := range []string{
		"/agent-ui/{ns}/{name}/bindings",
		"/agent-ui/{ns}/{name}/actions",
		"/agent-ui/{ns}/{name}/live",
	} {
		rt, ok := byPattern[pattern]
		require.Truef(t, ok, "%s must still be routed", pattern)
		assert.Equalf(t, webui.AuthAuthenticated, rt.Auth, "%s must stay authenticated", pattern)
		assert.NotNilf(t, rt.Handler, "%s must keep its handler", pattern)
	}
}

// TestRedirect_TargetIsTheAddressStartRoutesHandOut is the join: the address
// this route redirects to and the href a start route answers with must be the
// same string for the same session, because they are the same promise made in
// two places. They share SessionShellHref for exactly that reason; this asserts
// they still do.
func TestRedirect_TargetIsTheAddressStartRoutesHandOut(t *testing.T) {
	d := newStartDepsWithLive(t, &startFakeLive{})

	rec := postStart(t, d, startNS, startEnded, `{"prompt":"again"}`)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var got startResponse
	require.NoError(t, decodeJSON(rec.Body.String(), &got))

	redirected := serveRedirect(t, got.Ns, got.Name)
	require.Equal(t, http.StatusFound, redirected.Code)
	assert.Equal(t, got.Href, redirected.Header().Get("Location"),
		"the redirect target and the start route's href must address the same session the same way")

	// And the session really is the one that was created, so this is not two
	// producers agreeing on a wrong answer.
	var created spiceboxv1alpha1.AgentSession
	require.NoError(t, d.k8s.Get(context.Background(),
		client.ObjectKey{Namespace: got.Ns, Name: got.Name}, &created))
}

// decodeJSON is a thin wrapper so the test above reads as one line.
func decodeJSON(body string, out any) error {
	return json.NewDecoder(strings.NewReader(body)).Decode(out)
}
