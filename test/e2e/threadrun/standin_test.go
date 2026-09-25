//go:build e2e

package threadrun

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestStandInMintedCredentialTypesAreAllServed is the one thing standing between
// a loud capture refusal and a silent replay failure.
//
// bt.StandInMintedCredentialTypes is a PROMISE the capture believes: a minted
// credential of a listed type is emitted without a finding, because this driver
// is supposed to stand a minter up for it. A name added to that list with
// nothing wired here would make the capture stop refusing and the replay start
// failing at dispatch — with the replayed identity reporting perfectly healthy,
// which is the exact trap the whole check exists to close.
//
// It asserts against the driver's own constant rather than re-listing the types,
// so the failure names the type nobody wired.
func TestStandInMintedCredentialTypesAreAllServed(t *testing.T) {
	served := map[string]bool{
		// wireGitHubAppIdentities, via Harness.WireGitHubAppIdentity.
		githubAppCredentialType: true,
	}

	for _, typ := range bt.StandInMintedCredentialTypes {
		assert.True(t, served[typ],
			"bronzethread promises the replay harness serves credential type %q, and this driver "+
				"stands no minter up for it. A capture will now emit a bundle carrying such a "+
				"credential and the replay will fail at dispatch with the identity reporting "+
				"healthy — strictly worse than the refusal the promise replaced. Wire it here, or "+
				"take the name off StandInMintedCredentialTypes.", typ)
	}
	require.NotEmpty(t, bt.StandInMintedCredentialTypes,
		"an empty list makes the loop above vacuous; if the last served type was removed, this "+
			"test is no longer guarding anything")
}

// TestProviderIDMap_PinsOnlyWhatTheBundleRecorded covers the compatibility path,
// which is the half that decides whether 39 authored bundles keep working.
//
// A bundle recording no provider ids must leave the stand-in's own counter in
// place: it is making no claim about what a provider named its statuses, and a
// sequence with an empty family would exhaust on the first mint and fail every
// such bundle.
func TestProviderIDMap_PinsOnlyWhatTheBundleRecorded(t *testing.T) {
	cases := []struct {
		name string
		in   bt.Bundle
		want map[string][]string
	}{
		{name: "no standIn block at all yields no pinning", in: bt.Bundle{}},
		{name: "an empty id list yields no pinning", in: bt.Bundle{StandIn: &bt.StandIn{}}},
		{
			name: "recorded ids are keyed under the trigger-status family",
			in:   bt.Bundle{StandIn: &bt.StandIn{TriggerStatusIDs: []string{"11", "22"}}},
			want: map[string][]string{bt.FamilyTriggerStatus: {"11", "22"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := providerIDMap(tc.in)
			assert.Equal(t, tc.want, got)

			// The consequence, stated rather than assumed: nil in means a nil
			// minter out, which is what leaves the fixture's counter alone.
			minter := bt.NewMintedIDSequence(got, func(string) { t.Error("nothing should have been minted") }).
				Minter(bt.FamilyTriggerStatus)
			if tc.want == nil {
				assert.Nil(t, minter, "an unpinned family must mint nothing, or every authored bundle breaks")
				return
			}
			require.NotNil(t, minter)
			assert.Equal(t, "11", minter(), "the stand-in draws the recorded ids in mint order")
		})
	}
}

// TestMentionStandIn_ConvertsBothWaysWithoutLoss pins the string/typed boundary.
//
// The bundle format holds plain strings so no consumer of it drags a channel-kind
// import along, and the conversion happens at each end. A conversion that dropped
// a field would seed a directory that resolves nobody, and the tool would then
// answer not-found on a run whose recorded catalog says it was offered — a
// difference nothing else in the suite would explain.
func TestMentionStandIn_ConvertsBothWaysWithoutLoss(t *testing.T) {
	m := &bt.MentionStandIn{
		Lookups: []string{"email", "name"},
		Users: []bt.MentionUser{
			{Kind: "email", Value: "dana@example.test", ExternalID: "U-DEMO-1", DisplayName: "Dana"},
		},
	}

	assert.Equal(t,
		[]channelkinds.MentionLookupKind{channelkinds.MentionLookupEmail, channelkinds.MentionLookupName},
		mentionLookups(m))

	users := mentionUsers(m)
	require.Len(t, users, 1)
	assert.Equal(t, channelkinds.MentionLookupEmail, users[0].Kind)
	assert.Equal(t, "dana@example.test", users[0].Value)
	assert.Equal(t, "U-DEMO-1", users[0].ExternalID,
		"the provider's own id is the one value nothing in our code could reproduce; dropping it "+
			"is the whole failure this seeding exists to avoid")
	assert.Equal(t, "Dana", users[0].DisplayName)
}
