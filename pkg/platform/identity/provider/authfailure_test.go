package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// TestAuthFailureAbsentIsNil pins the "absence is safe" contract from the
// AuthFailure doc comment: a provider that declares no authFailure: block
// must parse to a nil pointer, not a zero-value struct or an error. Nil
// means "no corroboration available" — callers must be able to distinguish
// that from an (invalid) empty block.
func TestAuthFailureAbsentIsNil(t *testing.T) {
	const doc = `
id: no-auth-failure
shape: bearer
`
	var p Provider
	require.NoError(t, yaml.Unmarshal([]byte(doc), &p))
	assert.Nil(t, p.AuthFailure, "a provider with no authFailure: block must parse to a nil AuthFailure")
}

// TestAuthFailureHTTPStatusesRoundTrips proves httpStatuses parses into the
// exact []int declared in YAML — no reordering, no defaulting at parse time
// (defaulting an empty list to [401] is the classifier's runtime concern, in
// pkg/platform/identity/credupdate, not the loader's).
func TestAuthFailureHTTPStatusesRoundTrips(t *testing.T) {
	const doc = `
id: has-auth-failure
shape: bearer
authFailure:
  httpStatuses: [401]
`
	var p Provider
	require.NoError(t, yaml.Unmarshal([]byte(doc), &p))
	require.NotNil(t, p.AuthFailure, "authFailure: block present in YAML must not be dropped")
	assert.Equal(t, []int{401}, p.AuthFailure.HTTPStatuses)
	assert.Empty(t, p.AuthFailure.ExitCodes)
	assert.Empty(t, p.AuthFailure.StderrPatterns)
}

// TestValidateAuthFailureConfig covers the load-time validation rules for an
// authFailure: block, mirroring TestValidateVerifyConfig's table shape.
func TestValidateAuthFailureConfig(t *testing.T) {
	cases := []struct {
		name    string
		af      AuthFailure
		wantErr string // substring required in the error; "" = valid
	}{
		{name: "valid: empty block", af: AuthFailure{}},
		{name: "valid: httpStatuses 401 only", af: AuthFailure{HTTPStatuses: []int{401}}},
		{name: "valid: httpStatuses 403 is allowed to VALIDATE (the 401-only DEFAULT is what keeps it opt-in)", af: AuthFailure{HTTPStatuses: []int{403}}},
		{name: "valid: httpStatuses at range boundaries 100 and 599", af: AuthFailure{HTTPStatuses: []int{100, 599}}},
		{name: "valid: compilable stderrPattern", af: AuthFailure{StderrPatterns: []string{`(?i)unauthorized`}}},
		{name: "uncompilable stderrPatterns regex rejected: message names the pattern", af: AuthFailure{StderrPatterns: []string{"(unclosed"}}, wantErr: "(unclosed"},
		{name: "httpStatuses below 100 rejected: message names the value", af: AuthFailure{HTTPStatuses: []int{99}}, wantErr: "99"},
		{name: "httpStatuses above 599 rejected: message names the value", af: AuthFailure{HTTPStatuses: []int{600}}, wantErr: "600"},
		{name: "negative exitCode rejected: message names the value", af: AuthFailure{ExitCodes: []int{-1}}, wantErr: "-1"},

		// SECURITY: exit code 0 means the process SUCCEEDED. Declaring it does
		// not merely widen corroboration, it INVERTS it — every clean call at
		// the origin would corroborate "credential rejected", and for a
		// provider with no verify: probe corroboration is decisive. Two
		// separate doc comments (tool.Result.ExitCode's pointer-ness, sandbox
		// composeResult's carry-nil-verbatim rule) already name `exitCodes: [0]`
		// as the hazard they defend against; this is validation agreeing.
		{name: "SECURITY: exitCode 0 rejected -- it means success, so it would invert the signal", af: AuthFailure{ExitCodes: []int{0}}, wantErr: "0"},
		{name: "exitCode above 255 rejected: a POSIX exit status is a byte, so it could never match", af: AuthFailure{ExitCodes: []int{256}}, wantErr: "256"},
		{name: "valid: exitCode at the 255 boundary", af: AuthFailure{ExitCodes: []int{255}}},

		// Exit code 1 VALIDATES and is documented as dangerous rather than
		// refused. Most CLIs exit 1 on any error, including one the model
		// provoked through argv, so `exitCodes: [1]` corroborates nearly every
		// failed call — the exit-code analogue of the `.*` stderr pattern
		// rejected below. It is not refused because a handful of CLIs really do
		// reserve 1 for auth rejection, and refusing it would push authors onto
		// stderrPatterns, the weaker and equally provokable signal. The gate is
		// catalog review (see AuthFailure.ExitCodes) plus the shipped-provider
		// pin in TestAllShippedProvidersDeclaringAuthFailureAreValid.
		{name: "valid: exitCode 1 passes validation but is a documented review hazard, not a safe default", af: AuthFailure{ExitCodes: []int{1}}},

		// A pattern matching the EMPTY string corroborates every failed call
		// whatever the tool printed — including a call that printed nothing at
		// all. That is never what an author meant, and it is the one
		// stderr-pattern defect that can be judged without knowing the CLI's
		// behavior, so it is the one that is enforced.
		{name: "stderrPattern matching the empty string rejected: .* corroborates every failure", af: AuthFailure{StderrPatterns: []string{".*"}}, wantErr: ".*"},
		{name: "stderrPattern matching the empty string rejected: all-optional groups", af: AuthFailure{StderrPatterns: []string{"(?i)(unauthorized)?"}}, wantErr: "(unauthorized)?"},
		{name: "stderrPattern matching the empty string rejected: bare anchors", af: AuthFailure{StderrPatterns: []string{"^"}}, wantErr: "^"},

		// Anchoring is NOT enforced — Go's ^/$ bind the whole text without
		// (?m), so a full-anchor rule would reject every pattern that works
		// against real multi-line stderr and push authors to `(?s).*x.*`, which
		// is strictly worse: it looks validated and matches more.
		{name: "valid: an unanchored substring pattern (anchoring is a review property, not a load-time rule)", af: AuthFailure{StderrPatterns: []string{`(?i)bad credentials`}}},
		{name: "valid: a line-anchored multiline pattern", af: AuthFailure{StderrPatterns: []string{`(?m)^gh: Bad credentials`}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAuthFailureConfig("test-provider", &tc.af)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "test-provider", "error must name the provider id")
			assert.Contains(t, err.Error(), tc.wantErr, "error must name the offending value")
		})
	}
}

// TestAllShippedProvidersDeclaringAuthFailureAreValid asserts every embedded
// provider under providers/ that declares an authFailure: block passes the
// same validation the loader enforces at load, mirroring how
// loader_verify_test.go guards verify:.
//
// It is also the catalog-review gate for the two declarations that VALIDATE
// but weaken the corroboration gate, and so must not reach the shipped
// catalog without someone deliberately changing this test:
//
//   - httpStatuses 403. An LLM cannot forge a 401, but it CAN provoke a 403 by
//     deliberately requesting resources it isn't entitled to, manufacturing
//     corroboration to pair with an indeterminate probe.
//   - exitCodes 1. Most CLIs exit 1 on any error, including one the model
//     provoked through argv, so it corroborates nearly every failed call at
//     that origin (see AuthFailure.ExitCodes).
func TestAllShippedProvidersDeclaringAuthFailureAreValid(t *testing.T) {
	all := All()
	var found int
	for _, p := range all {
		if p.AuthFailure == nil {
			continue
		}
		found++
		err := validateAuthFailureConfig(p.ID, p.AuthFailure)
		assert.NoError(t, err, "provider %q: shipped authFailure block failed re-validation", p.ID)
		assert.NotContains(t, p.AuthFailure.HTTPStatuses, 403,
			"provider %q: declares httpStatuses: [403] — an agent can provoke a 403 on demand, so no shipped provider may corroborate on one", p.ID)
		assert.NotContains(t, p.AuthFailure.ExitCodes, 1,
			"provider %q: declares exitCodes: [1] — most CLIs exit 1 on any error the agent's argv can cause, so no shipped provider may corroborate on one", p.ID)
	}
	require.Greater(t, found, 0,
		"expected at least one shipped provider under providers/ to declare authFailure:; with none, every assertion in this test is vacuous and the catalog-review gate above guards nothing")
}

// TestEmbeddedGithubPatHasAuthFailure pins the catalog contract: github-pat
// declares an authFailure: block using only the safe 401-only default,
// alongside its existing verify: block.
func TestEmbeddedGithubPatHasAuthFailure(t *testing.T) {
	p, ok := ByID("github-pat")
	require.True(t, ok, "github-pat provider missing from embedded catalog")
	require.NotNil(t, p.AuthFailure, "github-pat must declare an authFailure: block")
	assert.Equal(t, []int{401}, p.AuthFailure.HTTPStatuses)
}

// TestEmbeddedOAuthMCPHasAuthFailure pins the same contract for oauth-mcp, and
// it carries more weight than github-pat's: oauth-mcp declares no verify: block
// and its builtin flow reports unsupported (an audience-bound token has no
// endpoint to probe), so this block is the ONLY evidence that can open a
// credential-update card for an MCP origin. It is also the provider every
// generated MCPServer defaults to.
//
// Exact-match on [401], not a Contains: 403 is spec-assigned to "invalid scopes
// or insufficient permissions" on a VALID token and an agent can provoke one on
// demand, and stderrPatterns/exitCodes read program output the agent steers
// through argv. Widening this entry must fail CI rather than depend on a
// reviewer noticing a one-line YAML diff.
func TestEmbeddedOAuthMCPHasAuthFailure(t *testing.T) {
	p, ok := ByID("oauth-mcp")
	require.True(t, ok, "oauth-mcp provider missing from embedded catalog")
	require.NotNil(t, p.AuthFailure,
		"oauth-mcp must declare an authFailure: block — without it no MCP-origin credential can ever be corroborated, and its flow reports unsupported, so no card can open at all")
	assert.Equal(t, []int{401}, p.AuthFailure.HTTPStatuses,
		"oauth-mcp must corroborate on exactly [401]: only the server's own auth layer emits one")
	assert.Empty(t, p.AuthFailure.ExitCodes,
		"oauth-mcp is an HTTP origin: an exit code is not something it can produce, and the field reads output the agent's argv steers")
	assert.Empty(t, p.AuthFailure.StderrPatterns,
		"oauth-mcp is an HTTP origin: stderr is not something it can produce, and the field reads output the agent's argv steers")
}
