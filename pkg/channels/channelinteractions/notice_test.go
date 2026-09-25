package channelinteractions_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// A notice category is a zero-action interaction: pure delivery, no decision
// leg, no park state. Validate must enforce that shape, so a row cannot declare
// a decider policy it will never invoke and thereby read like a real prompt.
func TestCategoryValidate_NoticeRowShape(t *testing.T) {
	cases := []struct {
		name    string
		cat     channelinteractions.Category
		wantErr string
	}{
		{
			name: "minimal notice: no deciders, no park, ResurfaceNone — valid",
			cat: channelinteractions.Category{
				Name:      "agent_failed",
				Notice:    true,
				Tone:      channelinteractions.ToneCritical,
				Resurface: channelinteractions.ResurfaceNone,
			},
		},
		{
			name: "notice with a semantic glyph override — valid",
			cat: channelinteractions.Category{
				Name:      "info_leakage_notice",
				Notice:    true,
				Tone:      channelinteractions.ToneRoutine,
				Glyph:     channelinteractions.GlyphMoney,
				Resurface: channelinteractions.ResurfaceNone,
			},
		},
		{
			name: "notice declaring a decider policy: rejected",
			cat: channelinteractions.Category{
				Name:      "agent_failed",
				Notice:    true,
				Tone:      channelinteractions.ToneCritical,
				Deciders:  channelinteractions.DecideRequester,
				Resurface: channelinteractions.ResurfaceNone,
			},
			wantErr: "notice category",
		},
		{
			name: "notice that parks a session: rejected",
			cat: channelinteractions.Category{
				Name:      "agent_failed",
				Notice:    true,
				Tone:      channelinteractions.ToneCritical,
				Park:      v1alpha1.AgentSessionPhaseAwaitingDecision,
				Resurface: channelinteractions.ResurfaceNone,
			},
			wantErr: "notice category",
		},
		{
			name: "notice with a pending condition: rejected",
			cat: channelinteractions.Category{
				Name:             "agent_failed",
				Notice:           true,
				Tone:             channelinteractions.ToneCritical,
				PendingCondition: v1alpha1.AgentSessionConditionToolApprovalPending,
				Resurface:        channelinteractions.ResurfaceNone,
			},
			wantErr: "notice category",
		},
		{
			name: "notice asking to be resurfaced: rejected",
			cat: channelinteractions.Category{
				Name:      "agent_failed",
				Notice:    true,
				Tone:      channelinteractions.ToneCritical,
				Resurface: channelinteractions.ResurfaceCached,
			},
			wantErr: "notice category",
		},
		{
			name: "notice with no tone: rejected",
			cat: channelinteractions.Category{
				Name:      "agent_failed",
				Notice:    true,
				Resurface: channelinteractions.ResurfaceNone,
			},
			wantErr: "tone",
		},
		{
			name: "notice with an unknown tone: rejected",
			cat: channelinteractions.Category{
				Name:      "agent_failed",
				Notice:    true,
				Tone:      channelinteractions.Tone("catastrophic"),
				Resurface: channelinteractions.ResurfaceNone,
			},
			wantErr: "tone",
		},
		{
			name: "notice with an unknown glyph: rejected",
			cat: channelinteractions.Category{
				Name:      "agent_failed",
				Notice:    true,
				Tone:      channelinteractions.ToneCritical,
				Glyph:     channelinteractions.Glyph("sparkles"),
				Resurface: channelinteractions.ResurfaceNone,
			},
			wantErr: "glyph",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cat.Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// A prompt row requires a decider policy and a tone, and may not claim to be
// terminal — a message awaiting a decision is by definition not over.
func TestCategoryValidate_PromptRowUnchanged(t *testing.T) {
	cases := []struct {
		name    string
		cat     channelinteractions.Category
		wantErr string
	}{
		{
			name: "prompt with deciders, tone and resurface — valid",
			cat: channelinteractions.Category{
				Name:      "tool_approval",
				Deciders:  channelinteractions.DecideResourceOwners,
				Tone:      channelinteractions.ToneRoutine,
				Resurface: channelinteractions.ResurfaceCached,
			},
		},
		{
			name: "prompt with no decider policy: still rejected",
			cat: channelinteractions.Category{
				Name:      "tool_approval",
				Tone:      channelinteractions.ToneRoutine,
				Resurface: channelinteractions.ResurfaceCached,
			},
			wantErr: "decider policy",
		},
		{
			name: "prompt with no tone: rejected",
			cat: channelinteractions.Category{
				Name:      "tool_approval",
				Deciders:  channelinteractions.DecideResourceOwners,
				Resurface: channelinteractions.ResurfaceCached,
			},
			wantErr: "tone",
		},
		{
			name: "prompt claiming to be terminal: rejected",
			cat: channelinteractions.Category{
				Name:      "tool_approval",
				Deciders:  channelinteractions.DecideResourceOwners,
				Tone:      channelinteractions.ToneRoutine,
				Terminal:  true,
				Resurface: channelinteractions.ResurfaceCached,
			},
			wantErr: "cannot be terminal",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cat.Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// Tone.RequiresNextStep is the single place the "don't leave the user stuck"
// rule is expressed; the conformance suite reads it rather than re-listing tones.
func TestToneRequiresNextStep(t *testing.T) {
	assert.True(t, channelinteractions.ToneCritical.RequiresNextStep())
	assert.True(t, channelinteractions.ToneDegraded.RequiresNextStep())
	assert.False(t, channelinteractions.ToneRoutine.RequiresNextStep())
	assert.True(t, channelinteractions.TonePrivacy.RequiresNextStep())
	assert.False(t, channelinteractions.ToneWaiting.RequiresNextStep())
	assert.False(t, channelinteractions.ToneHousekeeping.RequiresNextStep())
	assert.False(t, channelinteractions.ToneResolved.RequiresNextStep())
}
