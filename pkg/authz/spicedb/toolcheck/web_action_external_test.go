package toolcheck_test

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// externalWebActionPerm is the shape a browser MUTATE tool's Permission()
// returns in Phase E: External, web_action, permission act, instance = the page
// host. web_action is web_host's revalidation sibling — its slot_grant_act is
// caveated with pagestate_matches, so the acting check presents a live page
// fingerprint that web_host never does.
func externalWebActionPerm() authz.Permission {
	return authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:       browserschema.WebActionType,
			Permission:         browserschema.ActPermission,
			ResourceIDTemplate: "{host}",
		},
	}
}

func webActionGrantKey() string {
	return "web_action:aa.com#" + authz.SlotGrantRelationName(browserschema.ActPermission)
}

// A web_action acting check presents the live page fingerprint as the
// pagestate_matches caveat's check-time Context — the fact that distinguishes it
// from the uncaveated web_host check. The grant resolves HAS_PERMISSION only when
// the fingerprint matches (SpiceDB decides that; here the fake stands in), and the
// request that reaches SpiceDB must carry the fingerprint for it to ever be able to.
func TestCheck_External_WebAction_PresentsFingerprintContext(t *testing.T) {
	cli := &recordingSpiceDB{answers: map[string]v1.CheckPermissionResponse_Permissionship{
		webActionGrantKey(): v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
	}}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), externalWebActionPerm(), authz.Inputs{
		Subject:                  "u-1",
		Args:                     map[string]any{"host": "aa.com"},
		AgentSessionRef:          "ns/s1",
		SlotResourceTypes:        []string{browserschema.WebActionType},
		PresentedPageFingerprint: "fp-A",
	})
	assert.Equal(t, authz.OutcomeAllowed, res.Outcome,
		"a matching fingerprint on an existing grant must be permitted")
	require.NotEmpty(t, cli.contexts, "the web_action check must reach SpiceDB")
	require.NotNil(t, cli.contexts[0], "a web_action check must carry a caveat Context")
	assert.Equal(t, "fp-A", cli.contexts[0].GetFields()["presented_fingerprint"].GetStringValue(),
		"the live page fingerprint must be presented as the pagestate_matches check-time context")
}

// THE security crux. A PERMISSIONSHIP_CONDITIONAL_PERMISSION response — what
// SpiceDB returns when a caveat cannot be fully evaluated (e.g. a required arg was
// absent) — must map to DENY, never allow. checkExternalSlotGrant requires an
// unambiguous HAS_PERMISSION; a CONDITIONAL acting call routes to a human, because
// a mutate whose page-state revalidation did not run must not proceed on its own.
func TestCheck_External_WebAction_ConditionalMapsToDeny(t *testing.T) {
	cli := &recordingSpiceDB{answers: map[string]v1.CheckPermissionResponse_Permissionship{
		webActionGrantKey(): v1.CheckPermissionResponse_PERMISSIONSHIP_CONDITIONAL_PERMISSION,
	}}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), externalWebActionPerm(), authz.Inputs{
		Subject:                  "u-1",
		Args:                     map[string]any{"host": "aa.com"},
		AgentSessionRef:          "ns/s1",
		SlotResourceTypes:        []string{browserschema.WebActionType},
		PresentedPageFingerprint: "fp-A",
	})
	assert.Equal(t, authz.OutcomeDenied, res.Outcome,
		"CONDITIONAL is not HAS_PERMISSION: a partially-evaluated caveat must deny, not allow")
	assert.Contains(t, res.Message, "approval",
		"a CONDITIONAL acting call routes to human approval")
}

// An empty presented fingerprint is a definitive deny too — but the fake cannot
// model the caveat guard, so this asserts the request still goes out carrying the
// empty value (never omitting the arg, which is what would make SpiceDB return
// CONDITIONAL). The integration test proves the real caveat denies it.
func TestCheck_External_WebAction_EmptyFingerprintStillPresentsArg(t *testing.T) {
	cli := &recordingSpiceDB{} // no answer: NO_PERMISSION, as the caveat guard yields
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), externalWebActionPerm(), authz.Inputs{
		Subject:           "u-1",
		Args:              map[string]any{"host": "aa.com"},
		AgentSessionRef:   "ns/s1",
		SlotResourceTypes: []string{browserschema.WebActionType},
		// PresentedPageFingerprint left empty.
	})
	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
	require.NotEmpty(t, cli.contexts)
	require.NotNil(t, cli.contexts[0], "the check must still carry a Context, never omit the arg")
	_, present := cli.contexts[0].GetFields()["presented_fingerprint"]
	assert.True(t, present,
		"presented_fingerprint must be present (empty, not absent) so SpiceDB denies via the caveat guard rather than returning CONDITIONAL")
}

// The web_host sibling carries NO caveat context: its grant is uncaveated, so a
// web_host acting check must not present a presented_fingerprint. This guards the
// keyed-on-resource-type branch against leaking the caveat context onto the wrong
// resource type.
func TestCheck_External_WebHost_PresentsNoContext(t *testing.T) {
	cli := &recordingSpiceDB{answers: map[string]v1.CheckPermissionResponse_Permissionship{
		"web_host:aa.com#" + authz.SlotGrantRelationName(browserschema.ActPermission): v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
	}}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:       browserschema.WebHostType,
			Permission:         browserschema.ActPermission,
			ResourceIDTemplate: "{host}",
		},
	}, authz.Inputs{
		Subject:           "u-1",
		Args:              map[string]any{"host": "aa.com"},
		AgentSessionRef:   "ns/s1",
		SlotResourceTypes: []string{browserschema.WebHostType},
		// A fingerprint is set, to prove it is IGNORED for a non-web_action type.
		PresentedPageFingerprint: "fp-A",
	})
	assert.Equal(t, authz.OutcomeAllowed, res.Outcome)
	require.NotEmpty(t, cli.contexts)
	assert.Nil(t, cli.contexts[0],
		"an uncaveated web_host check must carry no caveat Context, even when a fingerprint is set")
}
