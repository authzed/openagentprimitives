package toolcheck_test

// web_action_two_layer_test.go unit-tests Phase E's E8 two-layer acting gate in
// checkExternalSlotGrant: a browser MUTATE (WebActionRequired) must hold BOTH the
// coarse web_host#act grant AND the per-action web_action#act grant, while an
// observe/navigate/session call (no flag) is authorized on the host grant alone.
// The recordingSpiceDB fake answers per resource#permission, so a test can allow
// the host leg while denying the action leg — the exact distinction the gate adds.

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

// actingWebHostPerm is the acting tool's REAL Permission() (Phase C, unchanged by
// E8): External on web_host#act, instance = the page host, escaped into a legal
// object id. The web_action layer rides on top via Inputs, not via this Check.
func actingWebHostPerm() authz.Permission {
	return authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:         browserschema.WebHostType,
			Permission:           browserschema.ActPermission,
			ResourceIDTemplate:   "{host}",
			ResourceIDTransforms: browserschema.WebHostValueTransforms(),
		},
	}
}

const (
	twoLayerHost   = "aa.com"
	twoLayerKeyRaw = "aa.com/click/place-order-abcd1234"
	twoLayerFP     = "fp-A"
)

// grantKeys returns the two answer-map keys (web_host and web_action), computed
// through the SAME NewObjectID escape chains the checker uses, so the fake's keys
// match the requests the checker actually issues.
func grantKeys(t *testing.T) (webHost, webAction string) {
	t.Helper()
	hostID, err := authz.NewObjectID(twoLayerHost, browserschema.WebHostValueTransforms())
	require.NoError(t, err)
	actID, err := authz.NewObjectID(twoLayerKeyRaw, browserschema.WebActionValueTransforms())
	require.NoError(t, err)
	rel := authz.SlotGrantRelationName(browserschema.ActPermission)
	return browserschema.WebHostType + ":" + hostID.String() + "#" + rel,
		browserschema.WebActionType + ":" + actID.String() + "#" + rel
}

// mutateInputs is a classified-mutate call carrying the stamped web_action key +
// fingerprint and declaring BOTH slot types.
func mutateInputs() authz.Inputs {
	return authz.Inputs{
		Subject:                  "u-1",
		Args:                     map[string]any{"host": twoLayerHost},
		AgentSessionRef:          "ns/s1",
		SlotResourceTypes:        []string{browserschema.WebHostType, browserschema.WebActionType},
		WebActionRequired:        true,
		WebActionKey:             twoLayerKeyRaw,
		PresentedPageFingerprint: twoLayerFP,
	}
}

// Both grants present + the mutate flag → ALLOWED, and BOTH legs are checked,
// with the web_action leg carrying the live fingerprint as caveat context.
func TestCheck_TwoLayer_BothGrantsAllow(t *testing.T) {
	webHost, webAction := grantKeys(t)
	cli := &recordingSpiceDB{answers: map[string]v1.CheckPermissionResponse_Permissionship{
		webHost:   v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		webAction: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
	}}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), actingWebHostPerm(), mutateInputs())

	assert.Equal(t, authz.OutcomeAllowed, res.Outcome, "both layers granted → allow; msg=%q", res.Message)
	require.Len(t, cli.calls, 2, "a mutate checks BOTH the host and the action grant")
	assert.Contains(t, cli.calls[0], browserschema.WebHostType, "layer 1 is the coarse host grant")
	assert.Contains(t, cli.calls[1], browserschema.WebActionType, "layer 2 is the per-action grant")
	require.NotNil(t, cli.contexts[1], "the web_action leg must carry a pagestate_matches Context")
	assert.Equal(t, twoLayerFP, cli.contexts[1].GetFields()["presented_fingerprint"].GetStringValue(),
		"the live fingerprint is presented on the web_action leg")
	assert.Nil(t, cli.contexts[0], "the uncaveated web_host leg carries no Context")
}

// Host grant present but NO web_action grant → DENIED and routed to a human: the
// per-action revalidation grant is missing. This is the core new boundary.
//
// A third call now follows the denied caveated check: E10's reprompt-signal
// probe (webActionGrantExistsForReprompt), which asks the same relation with
// no caveat context to tell "a grant exists but its caveat failed" apart from
// "no grant at all". recordingSpiceDB keys purely on (type, id, permission)
// — it has no answer for web_action either way, so the probe also reads
// NO_PERMISSION and the denial is correctly NOT flagged as a changed-page
// re-prompt (see web_action_reprompt_test.go for the signal itself).
func TestCheck_TwoLayer_HostOnlyDeniesMutate(t *testing.T) {
	webHost, _ := grantKeys(t)
	cli := &recordingSpiceDB{answers: map[string]v1.CheckPermissionResponse_Permissionship{
		webHost: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		// web_action answer omitted → NO_PERMISSION.
	}}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), actingWebHostPerm(), mutateInputs())

	assert.Equal(t, authz.OutcomeDenied, res.Outcome, "a mutate with only the host grant must deny")
	assert.Contains(t, res.Message, "approval", "the web_action deny routes to human approval")
	assert.False(t, res.ChangedPageReprompt, "no grant exists at all, so this is a first-time ask, not a re-prompt")
	require.Len(t, cli.calls, 3, "host + the denied caveated action check + E10's reprompt-signal probe")
	assert.Contains(t, cli.calls[1], browserschema.WebActionType)
	assert.Contains(t, cli.calls[2], browserschema.WebActionType)
}

// WebActionRequired but the class did NOT declare a web_action slot → the second
// check is SKIPPED (its relation is not in the composed schema), so the call is
// allowed on the host grant alone and web_action is never queried.
func TestCheck_TwoLayer_UndeclaredWebActionSkipsSecondCheck(t *testing.T) {
	webHost, _ := grantKeys(t)
	cli := &recordingSpiceDB{answers: map[string]v1.CheckPermissionResponse_Permissionship{
		webHost: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
	}}
	in := mutateInputs()
	in.SlotResourceTypes = []string{browserschema.WebHostType} // web_action NOT declared
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), actingWebHostPerm(), in)

	assert.Equal(t, authz.OutcomeAllowed, res.Outcome,
		"a class that declares no web_action slot is gated on the host grant alone, as before Phase E")
	require.Len(t, cli.calls, 1, "web_action must not be queried when it is not a declared slot")
	assert.Contains(t, cli.calls[0], browserschema.WebHostType)
}

// An observe/navigate/session call (no mutate flag) is authorized by the host
// grant alone even when web_action IS a declared slot — the second check fires
// only for a classified mutate.
func TestCheck_TwoLayer_ObserveNeedsOnlyHostGrant(t *testing.T) {
	webHost, _ := grantKeys(t)
	cli := &recordingSpiceDB{answers: map[string]v1.CheckPermissionResponse_Permissionship{
		webHost: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
	}}
	in := mutateInputs()
	in.WebActionRequired = false // observe/navigate/session: the tool stamped no web_action args
	in.WebActionKey = ""
	in.PresentedPageFingerprint = ""
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), actingWebHostPerm(), in)

	assert.Equal(t, authz.OutcomeAllowed, res.Outcome, "a non-mutate call needs only the host grant")
	require.Len(t, cli.calls, 1, "web_action must never be queried for a non-mutate call")
}

// The host grant itself is missing → DENIED at layer 1, and the web_action leg is
// never reached (the layers are AND-ed, host first).
func TestCheck_TwoLayer_HostDenyShortCircuits(t *testing.T) {
	cli := &recordingSpiceDB{} // no answers: host resolves NO_PERMISSION
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), actingWebHostPerm(), mutateInputs())

	assert.Equal(t, authz.OutcomeDenied, res.Outcome, "no host grant → deny before the action leg")
	require.Len(t, cli.calls, 1, "the action leg must not be queried once the host leg fails")
	assert.Contains(t, cli.calls[0], browserschema.WebHostType)
}

// A mutate flag with an EMPTY web_action key cannot name the action it acts on:
// NewObjectID refuses the empty value, so the second check denies (unresolved)
// without querying SpiceDB. Fail-closed on a tool that could not derive a key.
func TestCheck_TwoLayer_EmptyKeyDeniesUnresolved(t *testing.T) {
	webHost, _ := grantKeys(t)
	cli := &recordingSpiceDB{answers: map[string]v1.CheckPermissionResponse_Permissionship{
		webHost: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
	}}
	in := mutateInputs()
	in.WebActionKey = "" // the tool failed to derive a key (fail-closed path)
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), actingWebHostPerm(), in)

	assert.Equal(t, authz.OutcomeDenied, res.Outcome, "a mutate with no action key must deny")
	assert.True(t, res.UnresolvedResource, "an underivable action key is an unresolved-resource denial")
	require.Len(t, cli.calls, 1, "the host grant passed; the web_action leg denies before any query")
}
