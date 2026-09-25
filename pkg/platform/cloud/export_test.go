package cloud

import "testing"

// SetApplyUpdateFallbackForTest enables applyDoc's fake-client Update fallback
// for the duration of one test. See applyUpdateFallback for why production must
// never take that path.
//
// It lives in a _test.go file on purpose: that makes the fallback structurally
// unreachable from any production build, rather than merely defaulted off.
//
// Tests using it must not run in parallel with each other — it flips
// package-level state.
func SetApplyUpdateFallbackForTest(t *testing.T) {
	t.Helper()
	prev := applyUpdateFallback
	applyUpdateFallback = true
	t.Cleanup(func() { applyUpdateFallback = prev })
}
