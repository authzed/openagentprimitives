package cspassets

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderStampsOneSharedNonceOnEveryTag(t *testing.T) {
	var s Set
	s.Style(".a{color:red}")
	s.Script("window.x=1")
	s.ScriptSrc("/bundle.js")

	r, err := s.Render()
	require.NoError(t, err)
	require.NotEmpty(t, r.Nonce)

	assert.Contains(t, r.Styles, `<style nonce="`+r.Nonce+`">.a{color:red}</style>`)
	assert.Contains(t, r.Scripts, `<script nonce="`+r.Nonce+`">window.x=1</script>`)
	assert.Contains(t, r.Scripts, `<script src="/bundle.js" nonce="`+r.Nonce+`"></script>`)
	// exactly three nonce'd tags, all carrying the SAME token
	assert.Equal(t, 3, strings.Count(r.Styles+r.Scripts, `nonce="`+r.Nonce+`"`))
}

func TestRenderWithUsesTheProvidedNonce(t *testing.T) {
	var s Set
	s.Style(".a{}")
	s.Script("x")
	r, err := s.RenderWith("FIXED123")
	require.NoError(t, err)
	assert.Equal(t, "FIXED123", r.Nonce)
	assert.Contains(t, r.Styles, `nonce="FIXED123"`)
	assert.Contains(t, r.Scripts, `nonce="FIXED123"`)
}

func TestAddScriptEmitsTypeIdSrcThenNonce(t *testing.T) {
	var s Set
	s.AddScript(ScriptSpec{Type: "module", Src: "/assets/app.js"})
	s.AddScript(ScriptSpec{Type: "application/json", ID: "ap-bootstrap", Body: `{"k":"v"}`})
	r, err := s.RenderWith("N")
	require.NoError(t, err)
	// module + external src (no body)
	assert.Contains(t, r.Scripts, `<script type="module" src="/assets/app.js" nonce="N"></script>`)
	// typed + id + inline body
	assert.Contains(t, r.Scripts, `<script type="application/json" id="ap-bootstrap" nonce="N">{"k":"v"}</script>`)
}

func TestRenderMintsAFreshNonceEachCall(t *testing.T) {
	var s Set
	s.Style(".a{}")
	r1, err := s.Render()
	require.NoError(t, err)
	r2, err := s.Render()
	require.NoError(t, err)
	assert.NotEqual(t, r1.Nonce, r2.Nonce)
}

func TestExternalSrcIsAttributeEscaped(t *testing.T) {
	var s Set
	s.ScriptSrc(`/x.js" onload="alert(1)`)
	r, err := s.Render()
	require.NoError(t, err)
	assert.NotContains(t, r.Scripts, `onload="alert(1)"`, "a crafted src must not break out into extra attributes")
	assert.Contains(t, r.Scripts, "&#34;")
}

func TestCSPGrantsNonceOnlyForRegisteredCapabilities(t *testing.T) {
	base := Directive("default-src", "'none'")

	// Both a style and a script registered → both directives carry the nonce.
	var full Set
	full.Policy(base)
	full.Style(".a{}")
	full.Script("x")
	r, err := full.Render()
	require.NoError(t, err)
	assert.Contains(t, r.CSP, "default-src 'none'")
	assert.Contains(t, r.CSP, "script-src 'nonce-"+r.Nonce+"'")
	assert.Contains(t, r.CSP, "style-src 'nonce-"+r.Nonce+"'")

	// Script but no style → style-src is absent, so default-src 'none' denies
	// styles. This is the "tighter when a capability is off" property.
	var scriptOnly Set
	scriptOnly.Policy(base)
	scriptOnly.Script("x")
	r2, err := scriptOnly.Render()
	require.NoError(t, err)
	assert.Contains(t, r2.CSP, "script-src 'nonce-")
	assert.NotContains(t, r2.CSP, "style-src")

	// Nothing registered → neither directive appears at all.
	var empty Set
	empty.Policy(base)
	r3, err := empty.Render()
	require.NoError(t, err)
	assert.NotContains(t, r3.CSP, "script-src")
	assert.NotContains(t, r3.CSP, "style-src")
}

func TestCSPMergesPolicyDerivedAndPerItemNeeds(t *testing.T) {
	var s Set
	s.Policy(
		Directive("default-src", "'none'"),
		Directive("frame-ancestors", "https://shell.example"),
	)
	s.Script("x")
	s.Need("connect-src", "https://api.example")
	r, err := s.Render()
	require.NoError(t, err)

	assert.Contains(t, r.CSP, "default-src 'none'")
	assert.Contains(t, r.CSP, "frame-ancestors https://shell.example")
	assert.Contains(t, r.CSP, "script-src 'nonce-"+r.Nonce+"'")
	assert.Contains(t, r.CSP, "connect-src https://api.example")
	// directives joined by "; ", none empty
	for _, part := range strings.Split(r.CSP, "; ") {
		assert.NotEmpty(t, strings.TrimSpace(part))
	}
}

// TestSetRefusesADirectiveWithNoUsableSource pins the builder's fail-closed
// contract: a directive that would render valueless is refused at
// registration, and the refusal surfaces from BOTH render entry points.
//
// Why refuse rather than silently skip the directive: an empty source list is
// a CSP parse error, so the UA discards the directive — and frame-ancestors
// has no default-src fallback, so a discarded one and an omitted one are the
// same outcome at the browser (any origin may frame the page). Skipping would
// therefore turn a caller bug into an open frame just as quietly as emitting
// "frame-ancestors ;" does. Nor can the builder pick a safe substitute: only
// the caller knows what "deny" means for a given directive ('none' for
// frame-ancestors, 'self' for style-src). So the unsafe combination is made
// unrepresentable instead — see hostPage, which chooses 'none' itself.
func TestSetRefusesADirectiveWithNoUsableSource(t *testing.T) {
	cases := []struct {
		name     string
		register func(s *Set)
	}{
		{
			name:     "Policy directive with zero sources: refused",
			register: func(s *Set) { s.Policy(Directive("frame-ancestors")) },
		},
		{
			name:     "Policy directive whose only source is empty: refused",
			register: func(s *Set) { s.Policy(Directive("frame-ancestors", "")) },
		},
		{
			name:     "Policy directive whose every source is empty: refused",
			register: func(s *Set) { s.Policy(Directive("frame-ancestors", "", "")) },
		},
		{
			name:     "Policy directive with an empty name: refused",
			register: func(s *Set) { s.Policy(Directive("", "'none'")) },
		},
		{
			name:     "Need with zero sources: refused",
			register: func(s *Set) { s.Need("connect-src") },
		},
		{
			name:     "Need whose only source is empty: refused",
			register: func(s *Set) { s.Need("connect-src", "") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s Set
			s.Policy(Directive("default-src", "'none'"))
			s.Script("x")
			tc.register(&s)

			_, err := s.Render()
			require.Error(t, err, "Render must refuse a Set carrying a valueless directive")
			assert.ErrorIs(t, err, ErrEmptyDirective)

			// RenderWith is the second door into csp(); it must refuse too,
			// or a caller that mints its own nonce bypasses the guard.
			_, err = s.RenderWith("N")
			require.Error(t, err, "RenderWith must refuse the same Set")
			assert.ErrorIs(t, err, ErrEmptyDirective)
		})
	}
}

// TestCSPNeverEmitsAValuelessDirective is the invariant behind the refusal:
// whatever a Set renders, every directive it emits carries at least one
// source. Registering an extra empty source alongside a real one must not
// leave a dangling separator either.
func TestCSPNeverEmitsAValuelessDirective(t *testing.T) {
	var s Set
	s.Policy(
		Directive("default-src", "'none'"),
		Directive("frame-ancestors", "https://shell.example", ""),
	)
	s.Style(".a{}")
	s.Script("x")
	s.Need("connect-src", "", "https://api.example")

	r, err := s.Render()
	require.NoError(t, err, "every directive here has at least one usable source")

	for _, part := range strings.Split(r.CSP, "; ") {
		fields := strings.Fields(part)
		assert.GreaterOrEqual(t, len(fields), 2,
			"directive %q must carry a name AND at least one source", part)
		assert.Equal(t, part, strings.Join(fields, " "),
			"directive %q must not carry a stray separator from an empty source", part)
	}
	assert.Contains(t, r.CSP, "frame-ancestors https://shell.example")
	assert.Contains(t, r.CSP, "connect-src https://api.example")
}

func TestNeedMergesIntoAnExistingDirectiveWithoutDuplicates(t *testing.T) {
	var s Set
	s.Policy(Directive("default-src", "'none'"))
	s.Script("x")
	s.Need("script-src", "https://cdn.example")
	s.Need("script-src", "https://cdn.example") // duplicate source, must be deduped
	r, err := s.Render()
	require.NoError(t, err)
	// one script-src directive holding both the nonce and the extra source, once
	assert.Equal(t, 1, strings.Count(r.CSP, "script-src "))
	assert.Contains(t, r.CSP, "'nonce-"+r.Nonce+"'")
	assert.Contains(t, r.CSP, "https://cdn.example")
	assert.Equal(t, 1, strings.Count(r.CSP, "https://cdn.example"))
}
