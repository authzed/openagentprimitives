package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// FOLD-IN FROM E8 (leak guard). The web_action stamps — the 64-char page
// fingerprint ABOVE ALL — must be LIFTED into typed authz.Inputs and DELETED
// from the arg map by BuildInputs, BEFORE the approval card / args-hash is built
// from that map. E8 relies on the hook calling BuildInputs before
// BuildApprovalAsk (consumer ordering in toolcallauthz.go), but nothing pinned
// the strip itself: a reorder, or a BuildInputs that stopped deleting, would
// leak the fingerprint into the human approval card. This pins it.
func TestBuildInputs_StripsAndLiftsWebActionStamps(t *testing.T) {
	l := &Loop{AgentName: "demo-agent", PlanGateSlotTypes: []string{browserschema.WebActionType}}
	deps := l.toolCallAuthzDeps()

	in := pipeline.Input{Tool: &pipeline.ToolCallInfo{Name: "browser_click"}}
	// What NamedArgs stamps for a classified mutate, on top of the page host.
	named := map[string]any{
		"host":                             "aa.com",
		browserschema.WebActionRequiredArg: true,
		browserschema.WebActionKeyArg:      "aa.com/click/place-order-abcd1234",
		browserschema.WebActionFingerprintArg: "0123456789abcdef0123456789abcdef" +
			"0123456789abcdef0123456789abcdef", // 64 hex chars
	}

	got := deps.BuildInputs(in, named, authz.Permission{
		StateImpact: authz.External,
		Check:       &authz.PermissionCheck{ResourceType: browserschema.WebHostType, Permission: browserschema.ActPermission},
	})

	// Lifted into typed Inputs.
	assert.True(t, got.WebActionRequired, "the mutate flag must be lifted")
	assert.Equal(t, "aa.com/click/place-order-abcd1234", got.WebActionKey, "the action key must be lifted")
	assert.Equal(t, named["host"], "aa.com", "the host is NOT a web_action stamp and must remain for the web_host check")
	assert.Len(t, got.PresentedPageFingerprint, 64, "the fingerprint must be lifted whole")

	// STRIPPED from the arg map the approval card + args-hash are built from.
	assert.NotContains(t, named, browserschema.WebActionKeyArg,
		"the web_action key must be stripped before the approval card is built")
	assert.NotContains(t, named, browserschema.WebActionFingerprintArg,
		"the 64-char fingerprint must never reach the human approval card or the args hash")
	assert.NotContains(t, named, browserschema.WebActionRequiredArg,
		"the mutate flag must be stripped too")
	// The real resource arg survives (the web_host check still resolves {host}).
	assert.Contains(t, got.Args, "host", "stripping the gate keys must not disturb the resource args")
}

// A non-browser call carries none of the stamps, so BuildInputs is a no-op for
// them — the strip never touches a map that has nothing to strip.
func TestBuildInputs_NonBrowserCallUntouched(t *testing.T) {
	l := &Loop{AgentName: "demo-agent"}
	deps := l.toolCallAuthzDeps()

	in := pipeline.Input{Tool: &pipeline.ToolCallInfo{Name: "some_mcp_tool"}}
	named := map[string]any{"companyId": "acme"}
	got := deps.BuildInputs(in, named, authz.Permission{StateImpact: authz.Readonly})

	assert.False(t, got.WebActionRequired)
	assert.Empty(t, got.WebActionKey)
	assert.Empty(t, got.PresentedPageFingerprint)
	assert.Equal(t, map[string]any{"companyId": "acme"}, named, "a non-browser arg map is untouched")
}

// webActionGrantBinding is the SINGLE place the approved-mutate grant is
// constructed, so its shape (escaped id, caveat name, caveat parameter) is the
// one the E8 dispatch check must accept. Pin it.
func TestWebActionGrantBinding_CaveatedOnApprovedFingerprint(t *testing.T) {
	const rawKey = "aa.com/click/place-order-abcd1234"
	b, err := webActionGrantBinding(rawKey, "fp-A")
	require.NoError(t, err)

	assert.Equal(t, browserschema.WebActionType, b.ResourceType)
	assert.Equal(t, browserschema.ActPermission, b.Permission)
	assert.Equal(t, browserschema.PagestateMatchesCaveat, b.CaveatName,
		"the grant must carry the pagestate_matches caveat the web_action relation requires")
	assert.Equal(t, map[string]any{browserschema.ApprovedFingerprintParam: "fp-A"}, b.CaveatContext,
		"the caveat must freeze the approved page fingerprint")

	// The object id must be the escaped form the check keys on.
	wantID, derr := authz.NewObjectID(rawKey, browserschema.WebActionValueTransforms())
	require.NoError(t, derr)
	assert.Equal(t, wantID.String(), b.ResourceID.String(),
		"the grant id must be the spicedb_escape'd action key the E8 check derives")
}

// The degenerate case: an empty fingerprint is refused rather than turned into a
// permanently-dead grant (approved_fingerprint "" can never match a live page
// past the caveat's `presented_fingerprint != ""` guard).
func TestWebActionGrantBinding_EmptyFingerprintRefused(t *testing.T) {
	_, err := webActionGrantBinding("aa.com/click/x", "")
	require.Error(t, err, "an empty fingerprint must not mint a grant")
	assert.ErrorIs(t, err, errEmptyWebActionFingerprint)
}
