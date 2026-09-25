package github

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
)

// fineGrainedFor maps each CLASSIC OAuth scope this kind's DirectorySync
// feature declares to the substrings a FINE-GRAINED token's permission UI
// uses for the same access.
//
// Two vocabularies exist because two token types do. relsync_scopes_test.go
// pins FeatureSupport's Scopes against every endpoint ListScopes and
// FetchScope really call, and those are classic scope names — but the setup
// flow this kind names (github-pat) mints a FINE-GRAINED token, where
// "read:org" is not a thing you can tick. The operator reading the flow's
// guidance is looking at the fine-grained UI, so the guidance has to be in
// its words.
//
// The map is the only hand-written link in the chain, and it is deliberately
// the smallest one: a translation between two names for the same access,
// rather than a second list of what the sync needs. The chain is
//
//	real API calls  →(relsync_scopes_test.go)→  FeatureSupport().Scopes
//	                →(this test)→               SetupScopes()
//
// so an endpoint added without a scope fails the first test, and a scope
// added without a line in the recommendation fails this one.
var fineGrainedFor = map[string][]string{
	"read:org": {"Organization permissions", "Members: read"},
	"repo":     {"Repository permissions", "Metadata: read"},
}

// TestSetupScopesNameEveryDeclaredDirectorySyncScope is the guard on the
// guidance an operator acts on before they have anything to test against.
//
// It exists because this recommendation fails SILENTLY when it is wrong.
// github-pat's own suggestion is three repository permissions, none of which
// grants /orgs/{org}/members; and its verification probe is GET /user, which
// any token answers, so a token granted exactly the wrong access verifies
// clean, stores clean, and 403s on the first sync — after the operator has
// already waited on an org admin to approve the wrong request.
func TestSetupScopesNameEveryDeclaredDirectorySyncScope(t *testing.T) {
	req, ok := (&Kind{}).FeatureSupport()[channelfeatures.DirectorySync]
	require.True(t, ok, "the github kind must declare a requirement for channelfeatures.DirectorySync")
	require.NotEmpty(t, req.Scopes, "the declaration names no scope; this test would pass vacuously")

	lines := (&SyncKind{}).SetupScopes()
	require.NotEmpty(t, lines,
		"SetupScopes returns nothing, so the setup flow falls back to its own repository-permission "+
			"guess — which cannot read an organization's members at all")
	printed := strings.Join(lines, "\n")

	for _, scope := range req.Scopes {
		t.Run(scope, func(t *testing.T) {
			want, known := fineGrainedFor[scope]
			require.Truef(t, known,
				"DirectorySync now declares scope %q, which fineGrainedFor does not translate — look up "+
					"the fine-grained permission GitHub lists for it and add the entry, then make sure "+
					"SetupScopes names it", scope)
			for _, phrase := range want {
				assert.Containsf(t, printed, phrase,
					"%q needs %q, which the setup recommendation does not name:\n%s", scope, phrase, printed)
			}
		})
	}
}

// TestSetupScopesNameTheResourceOwner covers the half no scope list can: a
// fine-grained token grants an Organization permission only when the ORG is
// chosen as the token's resource owner. Ticking "Members: read" on a
// personal-account token produces a token with the right-looking permission
// and no access to the org at all — the same silent failure, one dialog
// earlier.
func TestSetupScopesNameTheResourceOwner(t *testing.T) {
	printed := strings.ToLower(strings.Join((&SyncKind{}).SetupScopes(), "\n"))
	assert.Contains(t, printed, "resource owner",
		"the recommendation must say which resource owner the token is created under")
	assert.Contains(t, printed, "organization",
		"and that it is the organization rather than the operator's own account")
}

// TestSetupScopesFitTheNoteBudget keeps the recommendation readable where it
// is actually shown. The setup flow prints these lines inside a huh note,
// indented by two, and huh wraps an over-long line with no sign the halves
// belong together — a permission name broken across a wrap is one nobody can
// find in the UI they are looking at.
//
// Measured against the directory wizard's own rail rather than the flow
// package's default: that wizard's widest step label ("Configuration") is 13
// columns, one over the 12 the shared budget assumes, so every note in it is
// a column narrower than the flow's own tests measure.
func TestSetupScopesFitTheNoteBudget(t *testing.T) {
	const budget = 58 // the railed note budget, less the wizard's wider rail and the two-space indent
	for _, line := range (&SyncKind{}).SetupScopes() {
		assert.LessOrEqualf(t, len([]rune(line)), budget,
			"%q is %d columns and would wrap mid-permission", line, len([]rune(line)))
	}
}
