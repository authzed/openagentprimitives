package artifactview

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// Blank-imported for their registry.Register(New()) init() side effects
	// only, shared by every test in this package's test binary:
	// rewrite_test.go dispatches through the real channelassets registry
	// (fakeDeps.RenderKind defaults to "html"), and the image kind is a REAL
	// registered renderer that does NOT implement channelassets.RefRewriter —
	// used to prove rewriteArtifactRefs' generic dispatch no-ops for a kind
	// with no reference concept, not just an unregistered one.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/image"
)

// fakeDeps implements only the Deps methods urlsFor/hostHandler/
// rewriteArtifactRefs exercise. Deps is embedded (as a nil interface) so
// every other method panics if called — no test in this package exercises
// them.
type fakeDeps struct {
	Deps
	sandbox      string
	token        string
	signCalls    int
	trusted      string   // TrustedOrigin() return value (hosthandler_test.go)
	verifyOK     bool     // VerifyContentToken() success/failure (hosthandler_test.go)
	sessionViews []string // SessionViews() return value (hosthandler_test.go)
	// resolveAsset backs ResolveAssetURL (rewrite_test.go). nil means every
	// handle is unresolvable (ok=false, nil error) — the safe default.
	resolveAsset func(ctx context.Context, ns, sess, handle string) (string, bool, error)
	// renderKind backs RenderKind (rewrite_test.go). nil defaults to "html" —
	// every test in this file predates per-kind dispatch and implicitly
	// assumed html; a test proving genericity overrides this directly.
	renderKind func(ctx context.Context, ns, sess, renderName string) (string, error)
}

// Logger returns a discard logger so rewriteArtifactRefs's error-path
// logging (a ResolveAssetURL error) doesn't panic on the embedded nil Deps.
func (f *fakeDeps) Logger() logr.Logger { return logr.Discard() }

func (f *fakeDeps) ResolveAssetURL(ctx context.Context, ns, sess, handle string) (string, bool, error) {
	if f.resolveAsset == nil {
		return "", false, nil
	}
	return f.resolveAsset(ctx, ns, sess, handle)
}

func (f *fakeDeps) RenderKind(ctx context.Context, ns, sess, renderName string) (string, error) {
	if f.renderKind == nil {
		return "html", nil
	}
	return f.renderKind(ctx, ns, sess, renderName)
}

func (f *fakeDeps) SessionViews(ctx context.Context, ns, sess string) []string {
	return f.sessionViews
}

func (f *fakeDeps) SignContentToken(ns, sess, renderName, artifactID string) (string, error) {
	f.signCalls++
	return f.token, nil
}

func (f *fakeDeps) SandboxBaseURL() string { return f.sandbox }

func (f *fakeDeps) TrustedOrigin() string { return f.trusted }

// VerifyContentToken returns a fixed (ns, sess, renderName) on success —
// hostHandler doesn't act on these beyond passing them to hostPage's ct
// echo, so the exact values don't matter to the tests that use this fake.
func (f *fakeDeps) VerifyContentToken(token string) (ns, sess, renderName string, err error) {
	if !f.verifyOK {
		return "", "", "", errors.New("fakeDeps: token verification failed")
	}
	return "ns1", "sess1", "render-1", nil
}

func TestUrlsFor_ShareOneTokenAcrossHostAndContent(t *testing.T) {
	av := &fakeDeps{sandbox: "https://sandbox.example/", token: "TOK123"}
	host, content, err := urlsFor(av, "ns1", "sess1", "render-7", "artifact-9")
	require.NoError(t, err)
	assert.Equal(t, "https://sandbox.example/artifact-host?ct=TOK123", host)
	assert.Equal(t, "https://sandbox.example/content?ct=TOK123", content)
	assert.Equal(t, 1, av.signCalls, "one token per push, reused for both URLs")
}

// TestUrlsFor_RefusesASandboxBaseThatIsNotAnHTTPOrigin covers the fail-closed
// guard on the value that ends up as the iframe src.
//
// Both URLs urlsFor mints are framed by the shell: hostUrl lands on an iframe
// carrying sandbox="allow-scripts allow-same-origin" (ui/ArtifactView.tsx), so
// whatever origin it resolves to gets script execution AND same-origin access
// to that origin. Two failure shapes matter:
//
//   - A non-http(s) base ("javascript:alert(1)") would build a javascript: URL
//     that executes in the TRUSTED shell origin.
//   - An EMPTY base — which is not hypothetical: `oap install` seeds the
//     spicebox-webd-external-url ConfigMap with "" whenever it manages external
//     access, and webd prints a warning and keeps serving until the real value
//     lands — builds the RELATIVE "/artifact-host?ct=…", which the shell also
//     loads from its own trusted origin. That silently collapses the two-origin
//     split the artifact viewer exists to maintain.
//
// Both must fail the mint rather than produce a URL. Every caller of urlsFor
// already handles the error (page → 500 PageError, revision → 500, live →
// error), so the viewer degrades to its generating state instead of framing
// agent-authored HTML in the trusted origin.
func TestUrlsFor_RefusesASandboxBaseThatIsNotAnHTTPOrigin(t *testing.T) {
	cases := []struct {
		name    string
		sandbox string
	}{
		{name: "javascript: base: refused, never reaches the iframe src", sandbox: "javascript:alert(1)"},
		{name: "data: base: refused", sandbox: "data:text/html,<script>alert(1)</script>"},
		{name: "empty base (ConfigMap not yet populated): refused, no shell-relative URL", sandbox: ""},
		{name: "scheme-less host: refused", sandbox: "sandbox.example"},
		{name: "file: base: refused", sandbox: "file:///etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			av := &fakeDeps{sandbox: tc.sandbox, token: "TOK123"}
			host, content, err := urlsFor(av, "ns1", "sess1", "render-7", "artifact-9")
			require.Error(t, err, "a sandbox base that is not an http(s) origin must not mint URLs")
			assert.Empty(t, host)
			assert.Empty(t, content)
		})
	}
}

// TestUrlsFor_NormalizesTheSandboxBaseToABareOrigin proves the accepted values
// are also reduced to scheme://host. The same value is handed to the client as
// the postMessage target origin (page.go's SandboxOrigin), which the browser
// compares byte-for-byte against event.origin — a trailing path or a stray
// injection character would break the bridge or widen a CSP source list.
func TestUrlsFor_NormalizesTheSandboxBaseToABareOrigin(t *testing.T) {
	cases := []struct {
		name    string
		sandbox string
		want    string
	}{
		{name: "trailing slash: trimmed", sandbox: "https://sandbox.example/", want: "https://sandbox.example"},
		{name: "port preserved", sandbox: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{name: "path/query dropped", sandbox: "https://sandbox.example/a/b?x=1", want: "https://sandbox.example"},
		{name: "space host-widening injection: truncated", sandbox: "https://sandbox.example evil.example", want: "https://sandbox.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			av := &fakeDeps{sandbox: tc.sandbox, token: "TOK"}
			host, content, err := urlsFor(av, "ns1", "sess1", "render-7", "artifact-9")
			require.NoError(t, err)
			assert.Equal(t, tc.want+"/artifact-host?ct=TOK", host)
			assert.Equal(t, tc.want+"/content?ct=TOK", content)
		})
	}
}
