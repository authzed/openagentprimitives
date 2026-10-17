package browser

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"
)

// timeNowMinus returns a timestamp safely in the past, for planting an expired
// hold entry.
func timeNowMinus(t *testing.T) time.Time {
	t.Helper()
	return time.Now().Add(-time.Minute)
}

// The stub values a fake fetcher returns. No test uses a real vault — these
// are the only "secret" bytes any filler test ever handles, so an assertion
// that an error/result does NOT contain them proves nothing leaked.
const (
	stubUsername = "alice@aa.example"
	stubPassword = "s3cr3t-pw-value"
)

// fakeFetcher returns a stub credential and counts calls, so a test can prove
// the §5.3 hold Fetches at most once per host. Each OTP() call returns a FRESH
// code, so a test can prove .otp is resolved live per placeholder.
type fakeFetcher struct {
	fetches int
	otps    int
}

func (f *fakeFetcher) fetch() CredentialFetcher {
	return func(context.Context, string) (directcred.Credential, error) {
		f.fetches++
		return directcred.Credential{
			Username: stubUsername,
			Password: stubPassword,
			OTP: func() (string, error) {
				f.otps++
				return fmt.Sprintf("%06d", 100000+f.otps), nil
			},
		}, nil
	}
}

// aaCatalog is a one-entry catalog for aa.example (non-exact: subdomains match).
func aaCatalog() []spiceboxv1alpha1.BrowserLoginRef {
	return []spiceboxv1alpha1.BrowserLoginRef{{
		ID:    "login-aa",
		Label: "AA Example",
		Hosts: []spiceboxv1alpha1.BrowserLoginHost{{Host: "aa.example"}},
	}}
}

// newTestFiller builds a real credFiller over a bare Session (its hold is
// lazily initialized) and a fake fetcher.
func newTestFiller(t *testing.T) (*credFiller, *fakeFetcher) {
	t.Helper()
	ff := &fakeFetcher{}
	return newCredFiller(&Session{}, aaCatalog(), ff.fetch()), ff
}

func TestFillSubstitutesAndFetchesOncePerHost(t *testing.T) {
	f, ff := newTestFiller(t)
	ctx := context.Background()

	// Two placeholders for the same host in one call → exactly one Fetch.
	got, kinds, err := f.Fill(ctx, "aa.example", true,
		"u={{login.aa.example.username}} p={{login.aa.example.password}}")
	require.NoError(t, err)
	assert.Equal(t, "u="+stubUsername+" p="+stubPassword, got)
	assert.True(t, kinds.username && kinds.password, "both kinds marked filled")
	assert.Equal(t, 1, ff.fetches, "two placeholders for one host must Fetch once")

	// A later Fill for the same host resolves from the hold — still one Fetch.
	got2, _, err := f.Fill(ctx, "aa.example", false, "{{login.aa.example.username}}")
	require.NoError(t, err)
	assert.Equal(t, stubUsername, got2)
	assert.Equal(t, 1, ff.fetches, "a second Fill for the held host must not Fetch again")
}

func TestFillSubtleSubdomainMatchesCatalogEntry(t *testing.T) {
	f, ff := newTestFiller(t)
	// Page host is a subdomain of the catalog entry's non-exact host.
	got, _, err := f.Fill(context.Background(), "login.aa.example", false, "{{login.aa.example.username}}")
	require.NoError(t, err)
	assert.Equal(t, stubUsername, got)
	assert.Equal(t, 1, ff.fetches)
}

func TestFillRule1OriginMismatchFailsClosed(t *testing.T) {
	f, ff := newTestFiller(t)
	// The page is a lookalike; the placeholder names aa.example.
	got, kinds, err := f.Fill(context.Background(), "aa.example.phish.test", true,
		"{{login.aa.example.password}}")
	require.Error(t, err, "a placeholder whose host is not the current page host must fail closed")
	assert.Empty(t, got, "no substituted text on a failed fill")
	assert.False(t, kinds.any())
	assert.Equal(t, 0, ff.fetches, "rule 1 must deny BEFORE any Fetch")
	assert.NotContains(t, err.Error(), stubPassword, "the error must never carry the secret")
}

func TestFillRule1EmptyPageHostFailsClosed(t *testing.T) {
	f, _ := newTestFiller(t)
	_, _, err := f.Fill(context.Background(), "", true, "{{login.aa.example.password}}")
	require.Error(t, err, "no current page (about:blank) must fail closed")
}

func TestFillRule2HostNotInCatalogFailsClosed(t *testing.T) {
	f, ff := newTestFiller(t)
	// Page host == placeholder host so rule 1 passes; the host is absent from
	// the catalog, so rule 2 must deny.
	_, _, err := f.Fill(context.Background(), "bb.example", true, "{{login.bb.example.password}}")
	require.Error(t, err, "a host with no catalog entry must fail closed")
	assert.Equal(t, 0, ff.fetches, "rule 2 must deny BEFORE any Fetch")
	assert.NotContains(t, err.Error(), stubPassword)
}

func TestFillRule3PasswordIntoNonPasswordInputFailsClosed(t *testing.T) {
	f, ff := newTestFiller(t)
	// inputIsPassword=false: a password placeholder must be refused.
	_, _, err := f.Fill(context.Background(), "aa.example", false, "{{login.aa.example.password}}")
	require.Error(t, err, "a password may only land in a type=password input")
	assert.Equal(t, 0, ff.fetches, "rule 3 must deny BEFORE any Fetch")
	assert.NotContains(t, err.Error(), stubPassword)
}

func TestFillUsernameIntoNonPasswordInputAllowed(t *testing.T) {
	f, _ := newTestFiller(t)
	// Rule 3 restricts only .password; a username into a text input is fine.
	got, kinds, err := f.Fill(context.Background(), "aa.example", false, "{{login.aa.example.username}}")
	require.NoError(t, err)
	assert.Equal(t, stubUsername, got)
	assert.True(t, kinds.username)
	assert.False(t, kinds.password)
}

func TestFillOTPCallsOTPFuncFreshPerPlaceholder(t *testing.T) {
	f, ff := newTestFiller(t)
	ctx := context.Background()

	first, kinds, err := f.Fill(ctx, "aa.example", false, "{{login.aa.example.otp}}")
	require.NoError(t, err)
	assert.True(t, kinds.otp)
	assert.Equal(t, 1, ff.otps, "first .otp calls the held OTP func once")
	assert.Equal(t, 1, ff.fetches)

	second, _, err := f.Fill(ctx, "aa.example", false, "{{login.aa.example.otp}}")
	require.NoError(t, err)
	assert.Equal(t, 2, ff.otps, "a later .otp calls the held OTP func again (fresh code)")
	assert.Equal(t, 1, ff.fetches, "the OTP func comes from the held credential — no re-Fetch")
	assert.NotEqual(t, first, second, "each .otp resolution is a fresh code")
}

func TestFillOTPWithNoConfiguredCodeFailsClosed(t *testing.T) {
	// A credential whose OTP func is nil (no one-time code configured).
	fetch := func(context.Context, string) (directcred.Credential, error) {
		return directcred.Credential{Username: stubUsername, Password: stubPassword, OTP: nil}, nil
	}
	f := newCredFiller(&Session{}, aaCatalog(), fetch)
	_, _, err := f.Fill(context.Background(), "aa.example", false, "{{login.aa.example.otp}}")
	require.Error(t, err, "requesting .otp on a login with no OTP must fail closed")
}

func TestFillMalformedPlaceholderFailsClosed(t *testing.T) {
	f, _ := newTestFiller(t)
	for _, text := range []string{
		"{{login.aa.example}}",                                   // no field
		"{{login.aa.example.pasword}}",                           // typo'd field
		"before {{login.aa.example.username}} and {{login.bad}}", // one good, one malformed
	} {
		t.Run(text, func(t *testing.T) {
			_, _, err := f.Fill(context.Background(), "aa.example", true, text)
			require.Error(t, err, "a malformed login placeholder must fail closed, never be typed literally")
		})
	}
}

func TestFillNoPlaceholdersPassesThrough(t *testing.T) {
	f, ff := newTestFiller(t)
	got, kinds, err := f.Fill(context.Background(), "aa.example", true, "just some search text")
	require.NoError(t, err)
	assert.Equal(t, "just some search text", got)
	assert.False(t, kinds.any())
	assert.Equal(t, 0, ff.fetches, "no placeholders means no Fetch")
}

func TestSessionHoldClearedOnClose(t *testing.T) {
	s := newSession("http://127.0.0.1:1", "tok")
	s.putHeldCredential("aa.example", directcred.Credential{Username: stubUsername})
	s.markBlanked(42)

	_, ok := s.heldCredential("aa.example")
	require.True(t, ok, "a just-held credential resolves from the hold")
	require.Equal(t, []int64{42}, s.blankedNodes())

	s.Close()

	_, ok = s.heldCredential("aa.example")
	assert.False(t, ok, "Close must clear the credential hold")
	assert.Empty(t, s.blankedNodes(), "Close must clear the blanked set")
}

func TestSessionHoldExpires(t *testing.T) {
	s := newSession("http://127.0.0.1:1", "tok")
	// Plant an already-expired entry.
	s.hold = map[string]heldCred{"aa.example": {
		cred:      directcred.Credential{Username: stubUsername},
		expiresAt: timeNowMinus(t),
	}}
	_, ok := s.heldCredential("aa.example")
	assert.False(t, ok, "an expired hold entry must not resolve")
}

func TestFillGrammarParsesMixedText(t *testing.T) {
	f, _ := newTestFiller(t)
	got, kinds, err := f.Fill(context.Background(), "aa.example", true,
		"login: {{login.aa.example.username}} / {{login.aa.example.password}} !")
	require.NoError(t, err)
	assert.Equal(t, "login: "+stubUsername+" / "+stubPassword+" !", got)
	assert.True(t, kinds.username && kinds.password)
	// The literal surrounding text survives verbatim.
	assert.True(t, strings.HasPrefix(got, "login: "))
	assert.True(t, strings.HasSuffix(got, " !"))
}
