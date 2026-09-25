package browser

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spyLaunch replaces the OS dispatch for the duration of the test and returns a
// pointer to the number of times it fired.
//
// Counting the real launcher — rather than inspecting Open's return value — is
// what makes these tests a proof rather than a description. A test that only
// asserted "Open returned ErrSuppressed" would pass while a regression opened a
// window first and returned the right error afterwards, and it would report the
// failure only after the damage it exists to prevent had already been done.
func spyLaunch(t *testing.T) *int {
	t.Helper()
	fired := 0
	prev := launch
	launch = func(string) error {
		fired++
		return nil
	}
	t.Cleanup(func() { launch = prev })
	return &fired
}

// TestOpenForgottenEntirely: no opener, no seam, no setup — the OS is not
// reached and the caller is told why.
//
// This is the mutation proof. Deleting the testing.Testing() guard in Open
// makes `fired` 1 and fails here by name. It is the exact regression that put
// two real browser windows on a maintainer's desktop during `go test`, and it
// is caught with the test having done nothing to protect itself.
func TestOpenForgottenEntirely_SuppressedAndOSNeverReached(t *testing.T) {
	fired := spyLaunch(t)

	err := Open("https://example.invalid/forgotten")

	require.Error(t, err, "Open must not report success for an open it did not perform")
	assert.ErrorIs(t, err, ErrSuppressed, "the refusal must be distinguishable from a host with no browser")
	assert.Contains(t, err.Error(), "https://example.invalid/forgotten",
		"the error names the URL, so a test that trips this can see what the flow wanted")
	assert.Zero(t, *fired, "the OS dispatch must never be reached from a test binary")
}

// TestOpenWithOpener: an installed opener receives the URL, and even then the
// OS dispatch stays unreachable — the seam redirects, it does not re-enable.
func TestOpenWithOpener_ReceivesURL_OSStillNeverReached(t *testing.T) {
	fired := spyLaunch(t)

	var got string
	restore := SetOpenerForTest(func(u string) error { got = u; return nil })
	t.Cleanup(restore)

	require.NoError(t, Open("https://example.invalid/seam"), "an installed opener reporting success is success")
	assert.Equal(t, "https://example.invalid/seam", got, "the opener sees the URL verbatim")
	assert.Zero(t, *fired, "installing an opener must not hand the OS dispatch back")
}

// TestOpenerErrorPropagates: a seam that reports failure is reported as
// failure, not smoothed into success — the headless/SSH/CI story every call
// site's "print the URL anyway" branch depends on.
func TestOpenerErrorPropagates_NotSuppressedSentinel(t *testing.T) {
	fired := spyLaunch(t)
	sentinel := errors.New("no browser on this machine")

	restore := SetOpenerForTest(func(string) error { return sentinel })
	t.Cleanup(restore)

	err := Open("https://example.invalid/headless")
	assert.ErrorIs(t, err, sentinel, "the opener's own error reaches the caller unchanged")
	assert.NotErrorIs(t, err, ErrSuppressed, "a real open failure must not look like test suppression")
	assert.Zero(t, *fired, "the OS dispatch must never be reached from a test binary")
}

// TestSetOpenerForTestRestores: restore puts the previous opener back, so one
// test cannot leak an opener into the next.
func TestSetOpenerForTestRestores_PreviousOpener(t *testing.T) {
	spyLaunch(t)

	outer := 0
	restoreOuter := SetOpenerForTest(func(string) error { outer++; return nil })
	t.Cleanup(restoreOuter)

	inner := 0
	restoreInner := SetOpenerForTest(func(string) error { inner++; return nil })
	require.NoError(t, Open("https://example.invalid/inner"))
	restoreInner()

	require.NoError(t, Open("https://example.invalid/outer"))
	assert.Equal(t, 1, inner, "the inner opener handled exactly the call made while it was installed")
	assert.Equal(t, 1, outer, "restore returned the outer opener, it did not clear the seam")
}

// TestRestoreToNoneReturnsToSuppressed: restoring past the outermost opener
// leaves suppression in charge, rather than a nil opener that falls through to
// the OS.
func TestRestoreToNoneReturnsToSuppressed(t *testing.T) {
	fired := spyLaunch(t)

	restore := SetOpenerForTest(func(string) error { return nil })
	require.NoError(t, Open("https://example.invalid/installed"))
	restore()

	assert.ErrorIs(t, Open("https://example.invalid/after"), ErrSuppressed,
		"with no opener installed, suppression is the floor")
	assert.Zero(t, *fired, "the OS dispatch must never be reached from a test binary")
}
