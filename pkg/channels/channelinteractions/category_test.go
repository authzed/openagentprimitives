package channelinteractions

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func validCategory() Category {
	return Category{
		Name:      "tool_approval",
		Park:      v1alpha1.AgentSessionPhaseAwaitingDecision,
		Deciders:  DecideApprovers,
		Tone:      ToneRoutine,
		Resurface: ResurfaceCached,
	}
}

func TestCategoryValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Category)
		wantErr string
	}{
		{name: "happy path valid", mutate: func(c *Category) {}, wantErr: ""},
		{name: "non-parking link category valid", mutate: func(c *Category) {
			*c = Category{Name: "portal_access", Deciders: DecideRequester, Tone: ToneRoutine, Resurface: ResurfaceNone}
		}, wantErr: ""},
		{name: "empty name rejected", mutate: func(c *Category) { c.Name = "" }, wantErr: "name"},
		{name: "uppercase name rejected", mutate: func(c *Category) { c.Name = "ToolApproval" }, wantErr: "snake_case"},
		{name: "hyphenated name rejected", mutate: func(c *Category) { c.Name = "tool-approval" }, wantErr: "snake_case"},
		{name: "unknown park phase rejected", mutate: func(c *Category) { c.Park = "AwaitingSnacks" }, wantErr: "park"},
		{name: "unknown decider policy rejected", mutate: func(c *Category) { c.Deciders = "anyone" }, wantErr: "decider"},
		{name: "empty decider policy rejected", mutate: func(c *Category) { c.Deciders = "" }, wantErr: "decider"},
		{name: "unknown resurface policy rejected", mutate: func(c *Category) { c.Resurface = "sometimes" }, wantErr: "resurface"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validCategory()
			tc.mutate(&c)
			err := c.Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestCategory_DecideParticipant_Valid(t *testing.T) {
	c := Category{Name: "x", Deciders: DecideParticipant, Tone: ToneRoutine, Resurface: ResurfaceNone}
	require.NoError(t, c.Validate())
}

func TestRegistryRegisterGetAll(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	Register(validCategory())
	Register(Category{Name: "credential_link", Park: v1alpha1.AgentSessionPhaseAwaitingCredentials, Deciders: DecideRequester, Tone: ToneRoutine, Resurface: ResurfaceRegenerate})

	got, ok := Get("tool_approval")
	require.True(t, ok)
	assert.Equal(t, DecideApprovers, got.Deciders)

	_, ok = Get("nonexistent")
	assert.False(t, ok)

	all := All()
	require.Len(t, all, 2)
	assert.Equal(t, "credential_link", all[0].Name, "All() is sorted by name")
	assert.Equal(t, "tool_approval", all[1].Name)
}

func TestCategorySurfaceValidation(t *testing.T) {
	base := Category{
		Name: "surface_cat", Deciders: DecideOwner, Tone: ToneRoutine,
		Resurface: ResurfaceNone,
	}

	okDefault := base
	require.NoError(t, okDefault.Validate(), "empty Surface (ephemeral+DM fallback) is valid")

	okDM := base
	okDM.Surface = SurfaceDMOnly
	require.NoError(t, okDM.Validate(), "SurfaceDMOnly is valid")

	bad := base
	bad.Surface = SurfaceType("carrier_pigeon")
	require.Error(t, bad.Validate(), "unknown Surface must be rejected")
}

func TestRegistryPanicsOnDuplicateAndInvalid(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	Register(validCategory())
	assert.Panics(t, func() { Register(validCategory()) }, "duplicate registration must panic at init time")
	assert.Panics(t, func() {
		Register(Category{Name: "bad-name", Deciders: DecideOwner, Tone: ToneRoutine, Resurface: ResurfaceNone})
	},
		"invalid category must panic at init time, not surface at dispatch time")
}
