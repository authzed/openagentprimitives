package main

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSharedOriginPredicateReadsAllowsSharedOrigin guards the wiring in run()
// (internal/cmd/webd/main.go): sharedOriginOK MUST be built from
// InstallProfile().AllowsSharedOrigin() — "whether webd may serve its
// trusted-auth and untrusted-artifact origins from one host" — and NEVER from
// InstallProfile().ServesLocalWebChat(), which asks a different question.
//
// The two genuinely diverge for a registered kind: `local` (--local's public
// tunnel) answers ServesLocalWebChat()=false, AllowsSharedOrigin()=true, while
// `desktop` (a network-confined VM) answers true for both.
// internal/cmd/webd/cloudimports_test.go pins that divergence behaviorally; this is a
// source scan on top of it, because run() binds allowsSharedOrigin inline and
// is not otherwise unit-testable without standing up the whole server.
//
// The guard below names localMode because that is the string a re-introduction
// would most plausibly use; nothing in this binary binds such a variable.
func TestSharedOriginPredicateReadsAllowsSharedOrigin(t *testing.T) {
	b, err := os.ReadFile("main.go")
	require.NoError(t, err, "read internal/cmd/webd/main.go")
	src := string(b)

	require.Regexp(t,
		regexp.MustCompile(`allowsSharedOrigin\s*:=\s*clusterStrategy\.InstallProfile\(\)\.AllowsSharedOrigin\(\)`),
		src, "run() must resolve allowsSharedOrigin from InstallProfile().AllowsSharedOrigin()")

	require.Regexp(t,
		regexp.MustCompile(`sharedOriginPredicate\(cfg\.allowSharedOrigin,\s*allowsSharedOrigin\)`),
		src, "sharedOriginOK must be built from allowsSharedOrigin, not localMode")

	assert.NotContains(t, src, "sharedOriginPredicate(cfg.allowSharedOrigin, localMode)",
		"regression guard: sharedOriginPredicate must never be wired back to localMode (ServesLocalWebChat) — "+
			"that collapses the artifact viewer's cross-origin isolation for any future kind where the two methods diverge")
}
