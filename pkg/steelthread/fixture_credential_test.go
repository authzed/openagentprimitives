package steelthread

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
)

// An internal test, unlike the rest of this package's suite: the generator's
// contract is "produces a string the declared pattern accepts", and the
// patterns are the input. Reaching it through RewriteFixture would test the
// same thing one credential at a time.

func TestSampleMatching(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
	}{
		{name: "a prefix-only literal", pattern: `^sk-ant-api`},
		{name: "an alternation of literal prefixes", pattern: `^(gh[a-z]_|github_pat_)`},
		{name: "fully anchored with two + classes", pattern: `^tskey-auth-[A-Za-z0-9]+-[A-Za-z0-9_-]+$`},
		{name: "dotall with \\s* and .*", pattern: `(?s)^apiVersion:\s*v1\s*\n.*kind:\s*Config`},
		{name: "a bounded repeat", pattern: `^tok_[a-f0-9]{8}$`},
		{name: "an optional group contributes nothing", pattern: `^abc(def)?$`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sampleMatching(tc.pattern)
			require.NoError(t, err, "sampling %s", tc.pattern)
			assert.Regexp(t, regexp.MustCompile(tc.pattern), got,
				"the generated sample must satisfy the pattern it came from")
			assert.NotEmpty(t, got)
		})
	}
}

// The generator is only ever right if it agrees with the validator the operator
// actually runs, so every SHIPPED provider is checked against that validator
// rather than against a re-derived regexp.
//
// This is what makes the check a real one: a provider added to the catalog with
// a shape the generator cannot handle fails HERE, at build time, instead of in
// somebody's capture weeks later as an AgentClass that never went Valid.
func TestPlaceholderCredentialValue_SatisfiesEveryShippedProvider(t *testing.T) {
	shaped := 0
	for _, p := range provider.All() {
		if p.TokenShape == nil || p.TokenShape.Pattern == "" {
			continue
		}
		shaped++
		t.Run(p.ID, func(t *testing.T) {
			sample, err := sampleMatching(p.TokenShape.Pattern)
			require.NoError(t, err, "provider %q declares pattern %q", p.ID, p.TokenShape.Pattern)
			assert.NoError(t, provider.ValidateToken(p, sample),
				"the operator's own validator must accept what RewriteFixture writes for provider %q", p.ID)
		})
	}
	require.NotZero(t, shaped,
		"no shipped provider declares a token shape, so this test asserted nothing")
}

// A credential whose provider declares no shape keeps the plain placeholder —
// the value every fixture Secret carried before, and the one a reader
// recognizes.
func TestPlaceholderCredentialValue_UnshapedCredentialKeepsThePlainPlaceholder(t *testing.T) {
	got, err := placeholderCredentialValue("no-such-credential-anywhere", "token")
	require.NoError(t, err)
	assert.Equal(t, "fixture-placeholder-token", got)
}

// The whole point of the shape-aware path: this credential name resolves
// through the EMBEDDED gh toolkit to the github-pat provider, whose pattern the
// plain placeholder fails. Left generic, the replayed AgentIdentity goes
// Valid=False/CredentialShapeMismatch and the bundle is skipped.
func TestPlaceholderCredentialValue_AShapedCredentialGetsAConformingValue(t *testing.T) {
	got, err := placeholderCredentialValue("github-token", "token")
	require.NoError(t, err)

	p, ok := provider.ByID("github-pat")
	require.True(t, ok, "the embedded gh toolkit binds github-token to the github-pat provider")
	assert.NoError(t, provider.ValidateToken(*p, got), "value %q", got)
	assert.NotEqual(t, "fixture-placeholder-token", got, "the generic placeholder does not satisfy this shape")
}

// A generated credential must be unmistakably fake. A fixture Secret holding
// something that looks like a real token invites exactly the wrong reaction
// from a person or a scanner reading the repo.
func TestPlaceholderCredentialValue_IsRecognizablyFake(t *testing.T) {
	got, err := placeholderCredentialValue("github-token", "token")
	require.NoError(t, err)
	assert.Contains(t, got, fixtureMarker,
		"a prefix-only shape leaves room to say so, and %q does not", got)
}

// Determinism: RewriteFixture promises byte-identical output for the same
// input, and a placeholder that varied would break every re-capture diff.
func TestPlaceholderCredentialValue_IsDeterministic(t *testing.T) {
	first, err := placeholderCredentialValue("github-token", "token")
	require.NoError(t, err)
	for range 5 {
		again, err := placeholderCredentialValue("github-token", "token")
		require.NoError(t, err)
		assert.Equal(t, first, again)
	}
}

// A shape no value can satisfy is an ERROR, never a best-effort placeholder:
// emitting one the validator rejects moves the failure minutes downstream into
// a suite skip that names an AgentClass condition rather than this credential.
func TestSampleMatching_RefusesAPatternItCannotSample(t *testing.T) {
	_, err := sampleMatching(`^$`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty string")

	_, err = sampleMatching(`[z-a]`) // an unparseable class
	require.Error(t, err)
}

// The marker biases character-class fills so a generated run spells something
// legible rather than a wall of one repeated character.
func TestSampleMatching_SpellsTheMarkerThroughCharacterClasses(t *testing.T) {
	got, err := sampleMatching(`^[a-z]{10}$`)
	require.NoError(t, err)
	assert.Len(t, got, 10)
	assert.False(t, strings.Count(got, string(got[0])) == len(got),
		"a 10-character fill of one repeated rune reads like a redaction, not a fixture: %q", got)
}
