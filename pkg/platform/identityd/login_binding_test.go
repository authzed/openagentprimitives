package identityd

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// A login flow's `state` is anti-replay, not proof of who is walking it. Nothing
// tied it to a browser: identityd minted it, the callback consumed it by value,
// and whoever HELD the value could finish the flow in any browser.
//
// That is login CSRF, and it runs in the attacker's favour. The attacker starts
// a flow (webd hands a session-less login link to any cookie-less GET on the
// admin paths), authenticates at the IdP AS THEMSELVES to obtain a code, and
// then lures the victim to the callback carrying that pair. The victim's
// browser is issued a session for the ATTACKER's subject — and the credential
// portal then links whatever upstream token the victim connects into the
// attacker's account.
//
// The fix is the standard one: mint a per-flow binding nonce at begin, put it in
// a cookie, and require it back at consume. The attacker can hand over the state
// but not the victim's cookie jar.
//
// These tests drive the store directly. It is the single chokepoint every login
// entry mints through and every callback consumes through, so binding it here is
// what makes a new entry point unable to skip the check — the signature no
// longer compiles without one.

func TestLinkStateStore_ConsumeRequiresTheBindingItWasMintedWith(t *testing.T) {
	s := newLinkStateStore()

	tok, err := s.NewStateWithNext("link-raw", "/next", "binding-abc", "idp")
	require.NoError(t, err)

	_, _, refusal := s.Consume(tok, "binding-xyz", "idp")
	assert.Equal(t, stateWrongBrowser, refusal, "a state presented with someone else's binding must not resolve")
}

func TestLinkStateStore_ConsumeWithNoBindingIsRefused(t *testing.T) {
	s := newLinkStateStore()

	tok, err := s.NewStateWithNext("link-raw", "", "binding-abc", "idp")
	require.NoError(t, err)

	_, _, refusal := s.Consume(tok, "", "idp")
	assert.Equal(t, stateWrongBrowser, refusal, "a browser presenting no binding cookie has not proven it started this flow")
}

func TestLinkStateStore_ConsumeWithTheMatchingBindingResolves(t *testing.T) {
	s := newLinkStateStore()

	tok, err := s.NewStateWithNext("link-raw", "/next", "binding-abc", "idp")
	require.NoError(t, err)

	linkRaw, next, refusal := s.Consume(tok, "binding-abc", "idp")
	require.Equal(t, stateAccepted, refusal, "the browser that started the flow must be able to finish it")
	assert.Equal(t, "link-raw", linkRaw)
	assert.Equal(t, "/next", next)
}

// Single-use survives the binding check, and it must be spent on a WRONG
// binding too: leaving the state alive would let an attacker keep re-luring
// victims at the same token until one of them happened to hold a matching
// cookie.
func TestLinkStateStore_AWrongBindingStillSpendsTheState(t *testing.T) {
	s := newLinkStateStore()

	tok, err := s.NewStateWithNext("link-raw", "", "binding-abc", "idp")
	require.NoError(t, err)

	_, _, refusal := s.Consume(tok, "wrong", "idp")
	require.Equal(t, stateWrongBrowser, refusal)

	_, _, refusal = s.Consume(tok, "binding-abc", "idp")
	assert.Equal(t, stateMissing, refusal, "a refused attempt spends the state; the right browser must start over")
}

// The end-to-end half: the login entry must SET the cookie, and the callback
// must reject a request that does not carry it. Driven through the real
// handlers so a begin site that mints a state without setting a cookie is
// caught here rather than in review.
func TestOIDCLogin_SetsTheBindingCookie(t *testing.T) {
	const canon = "user:alice@example.com"
	auth := &beginRecordingAuth{}
	srv := newOIDCLoginFixture(t, "https://identityd.example.org",
		map[string]channelkinds.WebAuthenticator{"slack": auth},
		makeAgentSessionWithKind("default", "s1", canon, "slack"))

	raw := mintLink(t, srv.deps.LinkSigner, "default/s1", canon, []string{"cred"}, time.Time{})
	d, sig, ok := splitSignedLink(raw)
	require.True(t, ok, "the fixture link must split into d/sig")

	rec := doOIDCLogin(t, srv, d, sig, "/artifacts/v/abc")
	require.Equal(t, http.StatusFound, rec.Code, "body: %s", rec.Body.String())

	binding := findCookie(rec, loginBindingCookie)
	require.NotNil(t, binding, "starting a login must bind the flow to this browser")
	assert.NotEmpty(t, binding.Value)
	assert.True(t, binding.HttpOnly, "the binding is never read by page script")
	assert.Equal(t, http.SameSiteLaxMode, binding.SameSite,
		"Lax so the IdP's top-level redirect back still carries it")
	assert.True(t, binding.Secure, "https external base ⇒ Secure")
}

func TestOIDCCallback_WithoutTheBindingCookieIsRefused(t *testing.T) {
	srv := newOIDCFixture(t, "https://ap.example")
	stateTok, _ := primeStateStore(t, srv, identity.Subject("user:alice"))

	// The attacker hands over (code, state); the victim's browser holds no
	// binding for this flow.
	rec := doOIDCCallbackNoBinding(t, srv, "fake", "code-1", stateTok)

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	for _, c := range rec.Result().Cookies() {
		assert.NotEqual(t, cookieName, c.Name,
			"a refused callback must never issue a session cookie")
	}
}

// A state carried no record of WHICH sign-in path minted it, and the callback
// route takes its kind from the URL (/oidc/callback/{kind}). So a state minted
// for the cluster IdP was redeemable at a channel-kind callback, and vice
// versa — and whichever authenticator's Complete() then ran would interpret a
// code issued for a different one.
//
// This is also why "ignore the kind override" was an insufficient fix on its
// own: the override is gone, but every registered callback is still directly
// addressable by URL.
func TestLinkStateStore_ConsumeRequiresTheAuthenticatorThatMintedIt(t *testing.T) {
	s := newLinkStateStore()

	tok, err := s.NewStateWithNext("link-raw", "", "binding-abc", "idp")
	require.NoError(t, err)

	_, _, refusal := s.Consume(tok, "binding-abc", "slack")
	assert.Equal(t, stateWrongAuthenticator, refusal,
		"an IdP-path state must not be redeemable at a channel-kind callback")
}

func TestLinkStateStore_ConsumeAcceptsTheMintingAuthenticator(t *testing.T) {
	s := newLinkStateStore()

	tok, err := s.NewStateWithNext("link-raw", "/next", "binding-abc", "slack")
	require.NoError(t, err)

	linkRaw, _, refusal := s.Consume(tok, "binding-abc", "slack")
	require.Equal(t, stateAccepted, refusal)
	assert.Equal(t, "link-raw", linkRaw)
}

// The browser binding is checked FIRST, so a wrong-browser attempt reports the
// login-CSRF diagnosis rather than being masked by a co-occurring authenticator
// mismatch. Both spend the state either way.
func TestLinkStateStore_WrongBrowserWinsTheDiagnosis(t *testing.T) {
	s := newLinkStateStore()

	tok, err := s.NewStateWithNext("link-raw", "", "binding-abc", "idp")
	require.NoError(t, err)

	_, _, refusal := s.Consume(tok, "wrong-binding", "slack")
	assert.Equal(t, stateWrongBrowser, refusal)
}
