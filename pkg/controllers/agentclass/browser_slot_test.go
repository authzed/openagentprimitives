package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

func classWithBrowserToolbox() *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			BrowserToolboxes: []spiceboxv1alpha1.AgentClassBrowserToolboxRef{{Ref: "web", Name: "web"}},
		},
	}
}

// A class that references a BrowserToolbox gets the builtin browser schema
// toolkit, so web_host AND web_action flow into the SAME standing/display
// resolvers a real toolkit uses. A class with no browser toolbox gets nothing —
// neither resource pollutes an unrelated class's status.
func TestBrowserSchemaToolkits_GatedOnReference(t *testing.T) {
	assert.Empty(t, browserSchemaToolkits(&spiceboxv1alpha1.AgentClass{}),
		"a class with no BrowserToolbox must not carry the browser schema toolkit")

	got := browserSchemaToolkits(classWithBrowserToolbox())
	require.Len(t, got, 1, "a browser-referencing class gets exactly the one builtin toolkit")
	require.NotNil(t, got[0].Spec.SpiceDBSchema)
	// ONE toolkit carrying BOTH resources (like git.yaml's git_repo + git_repo_url),
	// so the single SyntheticToolkitName keys no collision.
	names := make([]string, 0, len(got[0].Spec.SpiceDBSchema.Resources))
	for _, r := range got[0].Spec.SpiceDBSchema.Resources {
		names = append(names, r.Name)
	}
	assert.ElementsMatch(t, []string{"web_host", "web_action"}, names,
		"the browser toolkit carries both the host slot and the revalidation slot")
}

// web_host resolves session-only standing through resolveResourceStandings when
// the builtin toolkit is present — the exact path git_repo's standing takes.
func TestResolveResourceStandings_IncludesWebHostForBrowserClass(t *testing.T) {
	tks := browserSchemaToolkits(classWithBrowserToolbox())

	standings, err := resolveResourceStandings(nil, tks, nil, nil)
	require.NoError(t, err, "web_host declares session-only standing, so resolution must not error")

	var found *spiceboxv1alpha1.ResolvedResourceStanding
	for i := range standings {
		if standings[i].ResourceType == "web_host" {
			found = &standings[i]
		}
	}
	require.NotNil(t, found, "web_host standing must be published for a browser class")
	assert.Equal(t, spiceboxv1alpha1.StandingSessionOnly, found.Standing)
	assert.Empty(t, found.ApproverPermission, "session-only names no approver permission")

	// Without the toolkit, web_host is not a resolved type at all.
	none, err := resolveResourceStandings(nil, nil, nil, nil)
	require.NoError(t, err)
	for _, s := range none {
		assert.NotEqual(t, "web_host", s.ResourceType)
	}
}

// web_action resolves session-only standing through the SAME resolver, so a
// class declaring a web_action slot is not refused with "resource type
// \"web_action\" declares no standing". This is the per-class RESOLUTION half of
// the Phase-E substrate; without the toolkit carrying web_action it would fail.
func TestResolveResourceStandings_IncludesWebActionForBrowserClass(t *testing.T) {
	tks := browserSchemaToolkits(classWithBrowserToolbox())

	standings, err := resolveResourceStandings(nil, tks, nil, nil)
	require.NoError(t, err, "web_action declares session-only standing, so resolution must not error")

	var found *spiceboxv1alpha1.ResolvedResourceStanding
	for i := range standings {
		if standings[i].ResourceType == browserschema.WebActionType {
			found = &standings[i]
		}
	}
	require.NotNil(t, found, "web_action standing must be published for a browser class")
	assert.Equal(t, spiceboxv1alpha1.StandingSessionOnly, found.Standing)
	assert.Empty(t, found.ApproverPermission, "session-only names no approver permission")
}

// web_host's declared display reaches status through resolveResourceDisplays, so
// the approval card shows the host behind a web mark, not the wire type name.
func TestResolveResourceDisplays_IncludesWebHostForBrowserClass(t *testing.T) {
	tks := browserSchemaToolkits(classWithBrowserToolbox())
	displays := resolveResourceDisplays(nil, tks, nil)

	var found *spiceboxv1alpha1.ResolvedResourceDisplay
	for i := range displays {
		if displays[i].ResourceType == "web_host" {
			found = &displays[i]
		}
	}
	require.NotNil(t, found, "web_host display must be published for a browser class")
	assert.Equal(t, browserschema.Resource().Display.Icon, found.Icon)
	assert.NotEmpty(t, found.Label)
}

// A browser class declaring a web_host slot must publish web_host's
// spicedb_escape chain on status.resolvedSlots[].valueTransforms. The acting
// tools are a synthesized capability (not an MCPServer/toolkit/sidecar tool) and
// their Check is template-keyed, so resolveSlotValueKeying's tool walk never sees
// it; the chain is seeded from browserschema instead. Without this, SlotTransformsOf
// reports web_host as not value-keyed and every grant-write leg (approval bind,
// unattended grant) mints a RAW dotted id SpiceDB rejects, while the tool's Check
// escapes — the dots bug.
func TestResolveSlotValueKeying_PublishesWebHostEscapeForBrowserClass(t *testing.T) {
	ac := classWithBrowserToolbox()
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
		Slots: []spiceboxv1alpha1.AuthzSlot{
			{ResourceType: browserschema.WebHostType, Permission: browserschema.ActPermission, Description: "a web host"},
		},
	}
	// browserSchemaToolkits supplies web_host's session-only standing, exactly as
	// the controller folds it in before calling resolveSlotValueKeying.
	tks := browserSchemaToolkits(ac)

	got, reason, msg := resolveSlotValueKeying(ac, nil, tks, nil, nil)
	require.Empty(t, reason, "msg=%s", msg)
	require.Len(t, got, 1)
	assert.Equal(t, browserschema.WebHostType, got[0].ResourceType)
	assert.Equal(t, browserschema.WebHostValueTransforms(), got[0].ValueTransforms,
		"web_host must publish the spicedb_escape chain so every grant-write leg mints the escaped id the Check resolves")
}

// A class with NO browser toolbox must not have web_host seeded — the seed is
// reference-scoped, mirroring how web_host's standing/display are gated.
func TestResolveSlotValueKeying_NoWebHostSeedWithoutBrowserToolbox(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{}
	got, reason, _ := resolveSlotValueKeying(ac, nil, nil, nil, nil)
	require.Empty(t, reason)
	for _, rs := range got {
		assert.NotEqual(t, browserschema.WebHostType, rs.ResourceType,
			"web_host must not appear for a class that references no BrowserToolbox")
	}
}
