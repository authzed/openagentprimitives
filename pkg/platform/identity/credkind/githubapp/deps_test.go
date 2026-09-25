package githubapp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

// fakeMinter satisfies THIS package's own Minter interface (the
// MintRequest/MintedToken shape Task 2 built) -- the interface Adapt
// translates INTO credkind.GitHubAppMinter, as opposed to kind_test.go's
// fakeDepsMinter, which already speaks the translated shape.
type fakeMinter struct {
	gotReq MintRequest
	calls  int
	out    MintedToken
	err    error
}

func (f *fakeMinter) Mint(_ context.Context, req MintRequest) (MintedToken, error) {
	f.calls++
	f.gotReq = req
	return f.out, f.err
}

func TestAdapt_TranslatesRequestAndSatisfiesCredkindInterface(t *testing.T) {
	var _ credkind.GitHubAppMinter = Adapt(&fakeMinter{}) // compile-time shape check

	wantExp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m := &fakeMinter{out: MintedToken{AccessToken: sensitive.NewSensitiveValue([]byte("ghs_tok")), ExpiresAt: wantExp}}

	adapted := Adapt(m)
	tok, exp, err := adapted.Mint(context.Background(), "app-1", []byte("pem-bytes"), "install-2")
	require.NoError(t, err)
	assert.Equal(t, "ghs_tok", string(tok.UnderlyingValue()))
	assert.True(t, exp.Equal(wantExp))

	require.Equal(t, 1, m.calls)
	assert.Equal(t, MintRequest{AppID: "app-1", PrivateKeyPEM: []byte("pem-bytes"), InstallationID: "install-2"}, m.gotReq,
		"Adapt must forward every field unchanged into MintRequest")
}

func TestAdapt_ErrorPassesThroughUnwrapped(t *testing.T) {
	m := &fakeMinter{err: assert.AnError}
	_, _, err := Adapt(m).Mint(context.Background(), "app-1", []byte("pem"), "install-2")
	require.Error(t, err)
	assert.ErrorIs(t, err, assert.AnError)
}

// TestAdapt_NilMinterStaysNil pins a STRUCTURAL invariant, not a behavioral
// one: Adapt(nil) must return a genuine nil credkind.GitHubAppMinter, not a
// non-nil interface wrapping a nil Minter. Go's interface representation is
// a {type, value} tuple, so returning minterAdapter{nil} unconditionally
// would make the result compare != nil even though calling Mint on it
// panics -- Kind.Resolve's `deps.GitHubApp == nil` fail-closed check exists
// specifically to catch this shape, and a caller that skips Adapt's own nil
// guard would defeat it one layer up. Asserted via `== nil` (an interface
// comparison), never by invoking Mint and checking for an error or a
// panic -- that would test a symptom of the bug, not the bug itself.
func TestAdapt_NilMinterStaysNil(t *testing.T) {
	got := Adapt(nil)
	assert.Nil(t, got, "Adapt(nil) must be a genuine nil interface, not a non-nil wrapper around a nil Minter")
}
