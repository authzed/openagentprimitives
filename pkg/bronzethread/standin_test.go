package bronzethread

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// standInBundle is the smallest bundle Validate accepts, with a stand-in block
// the caller shapes.
//
// Trigger-started rather than userTurns-driven because the trigger-status half
// of the block is only meaningful with a trigger, and a helper that could not
// express that would make half the cases below unreachable.
func standInBundle(t *testing.T, s *StandIn) Bundle {
	t.Helper()
	return Bundle{
		Name:       "demo-scenario",
		AgentDir:   "testdata/demo-scenario",
		AgentClass: "demo-agent",
		Trigger: &Trigger{
			Channel:    "demo-hooks",
			Payload:    "delivery.json",
			Event:      "pull_request",
			ChannelKey: "pr:demo-org/platform#42",
		},
		LLM:     []LLMStep{{}},
		StandIn: s,
	}
}

// TestValidate_RefusesStandInStateTheReplayCouldNotActOn is the load precondition
// half of the stand-in contract, and every case is one a RUNTIME check could not
// catch.
//
// A stand-in handed something unusable does not error. It falls back to behaving
// exactly as it does with no seed at all — minting its own ids, advertising no
// directory — so the bundle diverges several steps later on something that reads
// as a product bug. Refusing at load is what puts the failure next to its cause.
func TestValidate_RefusesStandInStateTheReplayCouldNotActOn(t *testing.T) {
	cases := []struct {
		name    string
		standIn *StandIn
		mutate  func(b *Bundle)
		wantErr string
	}{
		{
			name:    "a blank provider id shifts every id after it",
			standIn: &StandIn{TriggerStatusIDs: []string{"11", "  ", "33"}},
			wantErr: "standIn.triggerStatusIDs[1]",
		},
		{
			name:    "provider ids with no trigger to seed them into",
			standIn: &StandIn{TriggerStatusIDs: []string{"11"}},
			mutate: func(b *Bundle) {
				b.Trigger = nil
				b.UserTurns = []UserTurn{{Text: "hello"}}
			},
			wantErr: "the bundle has no trigger",
		},
		{
			name: "directory entries with nothing advertised are unreachable",
			standIn: &StandIn{Mentions: &MentionStandIn{
				Users: []MentionUser{{Kind: "email", Value: "dana@example.test", ExternalID: "U-1"}},
			}},
			wantErr: "advertises no lookup kinds",
		},
		{
			name: "an entry recorded under an unadvertised kind",
			standIn: &StandIn{Mentions: &MentionStandIn{
				Lookups: []string{"email"},
				Users:   []MentionUser{{Kind: "name", Value: "sam", ExternalID: "U-2"}},
			}},
			wantErr: "not\nadvertised",
		},
		{
			name: "an entry that resolves to an empty provider id",
			standIn: &StandIn{Mentions: &MentionStandIn{
				Lookups: []string{"email"},
				Users:   []MentionUser{{Kind: "email", Value: "dana@example.test"}},
			}},
			wantErr: "no externalID",
		},
		{
			name: "an entry with nothing to match on",
			standIn: &StandIn{Mentions: &MentionStandIn{
				Lookups: []string{"email"},
				Users:   []MentionUser{{Kind: "email", ExternalID: "U-1"}},
			}},
			wantErr: "no value to match on",
		},
		{
			name: "an entry with no kind",
			standIn: &StandIn{Mentions: &MentionStandIn{
				Lookups: []string{"email"},
				Users:   []MentionUser{{Value: "dana@example.test", ExternalID: "U-1"}},
			}},
			wantErr: "has no kind",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := standInBundle(t, tc.standIn)
			if tc.mutate != nil {
				tc.mutate(&b)
			}
			err := b.Validate()
			require.Error(t, err, "the replay cannot act on this and would fall back silently")
			// The wantErr strings are matched loosely on purpose: the messages
			// wrap, and pinning their exact wrapping would make a reworded
			// explanation a test failure.
			assert.Contains(t, flattenSpaces(err.Error()), flattenSpaces(tc.wantErr))
		})
	}
}

// TestValidate_AcceptsTheStandInShapesAReplayCanActdOn is the negative control,
// and it is what keeps the cases above from being satisfied by a Validate that
// refuses every stand-in block.
func TestValidate_AcceptsTheStandInShapesAReplayCanActOn(t *testing.T) {
	cases := []struct {
		name    string
		standIn *StandIn
	}{
		{name: "no stand-in block at all"},
		{name: "an empty block", standIn: &StandIn{}},
		{
			name:    "provider ids alongside a trigger",
			standIn: &StandIn{TriggerStatusIDs: []string{"99044729080"}},
		},
		{
			name: "lookups advertised with NO users, which is the ordinary capture",
			standIn: &StandIn{Mentions: &MentionStandIn{
				Lookups: []string{"email", "name", "any"},
			}},
		},
		{
			name: "a fully populated directory",
			standIn: &StandIn{Mentions: &MentionStandIn{
				Lookups: []string{"email"},
				Users: []MentionUser{
					{Kind: "email", Value: "dana@example.test", ExternalID: "U-1", DisplayName: "Dana"},
				},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.NoError(t, standInBundle(t, tc.standIn).Validate())
		})
	}
}

// TestFamilyTriggerStatus_IsNotAShapeMatchedFamily pins the one thing that makes
// the provider ids safe to carry.
//
// FamilyOf shape-matches strings against MintedIDFamilies, and a bare integer is
// indistinguishable from a line count or a byte total. Adding the trigger-status
// family to that table would set the fold collecting arbitrary numbers as mints,
// and Bundle.validateMintedIDs would start admitting them into the mintedIDs map
// — where a replay would hand one to a component that mints operation ids.
func TestFamilyTriggerStatus_IsNotAShapeMatchedFamily(t *testing.T) {
	for _, f := range MintedIDFamilies {
		assert.NotEqual(t, FamilyTriggerStatus, f.Name,
			"a provider's identifiers are carried in standIn precisely because they cannot be "+
				"recognized by shape; putting the family here would make FamilyOf start guessing")
	}

	_, ok := FamilyOf("99044729080")
	assert.False(t, ok, "a check run id must never be read as one of the system's own minted ids")
}

// flattenSpaces collapses whitespace runs so a wrapped message and its
// single-line expectation compare equal.
func flattenSpaces(s string) string {
	out := make([]rune, 0, len(s))
	prevSpace := false
	for _, r := range s {
		isSpace := r == ' ' || r == '\n' || r == '\t'
		if isSpace {
			if !prevSpace {
				out = append(out, ' ')
			}
			prevSpace = true
			continue
		}
		prevSpace = false
		out = append(out, r)
	}
	return string(out)
}
