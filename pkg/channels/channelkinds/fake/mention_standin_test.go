package fake

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestFake_MentionLookupsAreOffByDefault is worth more than everything the
// opt-in enables, for exactly the reason the delivery-surface default is.
//
// A kind's advertised lookups decide whether lookup_user_for_mention is offered
// at all, so this default is what the whole bronze suite's recorded tool
// catalogs are standing on. A seed that leaked from a scenario that forgot to
// restore it would widen the tool list under 39 bundles at once.
func TestFake_MentionLookupsAreOffByDefault(t *testing.T) {
	require.Empty(t, MentionLookupsEnabled(),
		"the package must start with no directory; a leak from another test in this process is itself the bug")
	assert.Nil(t, Kind{}.SupportedMentionLookups())

	_, _, err := Kind{}.LookupUser(context.Background(), channelkinds.LookupDeps{}, channelkinds.MentionLookupEmail, "someone@example.test")
	assert.ErrorIs(t, err, channelkinds.ErrMentionUnsupported,
		"with nothing advertised the kind must refuse as unsupported, not as not-found")
}

// TestFake_EnableMentionLookupsAdvertisesAndRestores covers the opt-in and,
// just as importantly, that it hands back exactly what it took.
func TestFake_EnableMentionLookupsAdvertisesAndRestores(t *testing.T) {
	restore := EnableMentionLookups(
		[]channelkinds.MentionLookupKind{channelkinds.MentionLookupEmail},
		[]MentionUser{{Kind: channelkinds.MentionLookupEmail, Value: "dana@example.test", ExternalID: "U-DEMO-1", DisplayName: "Dana"}},
	)
	assert.Equal(t, []channelkinds.MentionLookupKind{channelkinds.MentionLookupEmail}, Kind{}.SupportedMentionLookups())

	restore()
	assert.Nil(t, Kind{}.SupportedMentionLookups(), "restore must return the exact default")
	assert.Empty(t, MentionLookupsEnabled())
}

// TestFake_LookupUserAnswersOnlyFromTheSeed pins that every refusal is the
// KIND's own sentinel, so the tool's real mapping onto three distinct messages
// runs rather than being stood in for.
//
// The seed decides only WHICH USERS EXIST. Advertised-but-absent is not-found,
// unadvertised is unsupported, and two entries under one name is ambiguous —
// none of which the bundle says anything about.
func TestFake_LookupUserAnswersOnlyFromTheSeed(t *testing.T) {
	t.Cleanup(EnableMentionLookups(
		[]channelkinds.MentionLookupKind{channelkinds.MentionLookupEmail, channelkinds.MentionLookupName},
		[]MentionUser{
			{Kind: channelkinds.MentionLookupEmail, Value: "dana@example.test", ExternalID: "U-DEMO-1", DisplayName: "Dana"},
			{Kind: channelkinds.MentionLookupName, Value: "sam", ExternalID: "U-DEMO-2", DisplayName: "Sam A"},
			{Kind: channelkinds.MentionLookupName, Value: "sam", ExternalID: "U-DEMO-3", DisplayName: "Sam B"},
		},
	))

	cases := []struct {
		name    string
		kind    channelkinds.MentionLookupKind
		value   string
		wantID  string
		wantErr error
	}{
		{name: "a seeded email resolves to the recorded id", kind: channelkinds.MentionLookupEmail, value: "dana@example.test", wantID: "U-DEMO-1"},
		{name: "case folds, because a directory is not case sensitive", kind: channelkinds.MentionLookupEmail, value: "Dana@Example.Test", wantID: "U-DEMO-1"},
		{name: "an advertised kind with no entry is not-found", kind: channelkinds.MentionLookupEmail, value: "nobody@example.test", wantErr: channelkinds.ErrMentionNotFound},
		{name: "an unadvertised kind is unsupported", kind: channelkinds.MentionLookupAny, value: "dana@example.test", wantErr: channelkinds.ErrMentionUnsupported},
		{name: "two entries under one name is ambiguous", kind: channelkinds.MentionLookupName, value: "sam", wantErr: channelkinds.ErrMentionAmbiguous},
		{name: "a value seeded under another kind is not-found", kind: channelkinds.MentionLookupName, value: "dana@example.test", wantErr: channelkinds.ErrMentionNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, _, err := Kind{}.LookupUser(context.Background(), channelkinds.LookupDeps{}, tc.kind, tc.value)
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				assert.Empty(t, id, "a refusal must resolve nobody")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantID, id)
		})
	}
}

// TestFake_MentionLookupAnyMatchesAcrossKinds covers the one asymmetry in the
// matcher: `any` is the caller asking the kind to apply its own heuristic, so
// an entry recorded under a specific kind is still a candidate for it.
func TestFake_MentionLookupAnyMatchesAcrossKinds(t *testing.T) {
	t.Cleanup(EnableMentionLookups(
		[]channelkinds.MentionLookupKind{channelkinds.MentionLookupAny},
		[]MentionUser{{Kind: channelkinds.MentionLookupEmail, Value: "dana@example.test", ExternalID: "U-DEMO-1"}},
	))
	id, _, err := Kind{}.LookupUser(context.Background(), channelkinds.LookupDeps{}, channelkinds.MentionLookupAny, "dana@example.test")
	require.NoError(t, err)
	assert.Equal(t, "U-DEMO-1", id)
}
