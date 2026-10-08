package authz_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

const urlChain = "normalize_url"

func urlSlot(autoGrantFrom []string) authz.ThreadSeedRequest {
	return authz.ThreadSeedRequest{
		ResourceType:    "http_target",
		ValueTransforms: []string{urlChain, "sha256"},
		AutoGrantFrom:   autoGrantFrom,
	}
}

func msg(author, text string) authz.ThreadMessage {
	return authz.ThreadMessage{AuthorCanonical: author, Text: text}
}

// TestSeedFromThread_TrustPolicyDecidesWhoseValuesBind is the security core.
// "Found in the thread" must never mean "anyone's value": a thread is
// multi-author, so scraping every message would let any participant make a
// target reachable just by pasting it.
func TestSeedFromThread_TrustPolicyDecidesWhoseValuesBind(t *testing.T) {
	msgs := []authz.ThreadMessage{
		msg("owner@example.com", "look at https://owner.example/a"),
		msg("other@example.com", "and https://hostile.example/b"),
		msg("", "anonymous https://unknown.example/c"),
	}

	cases := []struct {
		name          string
		autoGrantFrom []string
		wantHosts     []string
	}{
		{
			name:          "unset: only the owner's contribution binds",
			autoGrantFrom: nil,
			wantHosts:     []string{"https://owner.example/a"},
		},
		{
			name:          "participants: any attributable author binds",
			autoGrantFrom: []string{"participants"},
			wantHosts:     []string{"https://owner.example/a", "https://hostile.example/b"},
		},
		{
			// The nil/empty distinction is load-bearing: an explicitly empty
			// list means "the thread is a source, but a human decides".
			// Collapsing it into the unset default would widen a policy
			// written to be restrictive.
			name:          "none: nothing auto-grants",
			autoGrantFrom: []string{"none"},
			wantHosts:     nil,
		},
		{
			name:          "unrecognised value falls back to owner, never wider",
			autoGrantFrom: []string{"everyone"},
			wantHosts:     []string{"https://owner.example/a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := authz.SeedFromThread(msgs, "owner@example.com",
				[]authz.ThreadSeedRequest{urlSlot(tc.autoGrantFrom)}, 0)
			assert.Zero(t, dropped)

			// Compare by re-deriving the expected ids through the same chain,
			// so the test asserts WHICH values bound without hardcoding hashes.
			want := make([]authz.SlotBinding, 0, len(tc.wantHosts))
			for _, h := range tc.wantHosts {
				id, err := authz.NewObjectID(h, []string{urlChain, "sha256"})
				require.NoError(t, err)
				want = append(want, authz.SlotBinding{ResourceType: "http_target", ResourceID: id})
			}
			assert.ElementsMatch(t, want, got)
		})
	}
}

// TestSeedFromThread_CarriesOccupancyAndRebindOntoBindings: a thread seed must
// stamp the slot's occupancy/rebind onto every binding it emits, or a multi
// slot's thread-seeded instances arrive at GrantSlots reading as single — the
// second one refused, with nothing red. Empty on the request stays empty on the
// binding (which pkg/authz reads as single downstream).
func TestSeedFromThread_CarriesOccupancyAndRebindOntoBindings(t *testing.T) {
	msgs := []authz.ThreadMessage{msg("owner@example.com", "https://owner.example/a and https://owner.example/b")}

	t.Run("multi/approval propagate", func(t *testing.T) {
		req := urlSlot(nil)
		req.Occupancy, req.Rebind = "multi", "approval"
		got, _ := authz.SeedFromThread(msgs, "owner@example.com", []authz.ThreadSeedRequest{req}, 0)
		require.NotEmpty(t, got)
		for _, b := range got {
			assert.Equal(t, "multi", b.Occupancy, "the slot's occupancy must ride onto every seeded binding")
			assert.Equal(t, "approval", b.Rebind)
		}
	})
	t.Run("unset stays empty (reads as single)", func(t *testing.T) {
		got, _ := authz.SeedFromThread(msgs, "owner@example.com", []authz.ThreadSeedRequest{urlSlot(nil)}, 0)
		require.NotEmpty(t, got)
		for _, b := range got {
			assert.Empty(t, b.Occupancy)
			assert.Empty(t, b.Rebind)
		}
	})
}

// An unattributable author is never trusted, even under participants: an
// unsigned message is unknown provenance, not "some participant".
func TestSeedFromThread_UnattributableAuthorNeverBinds(t *testing.T) {
	got, _ := authz.SeedFromThread(
		[]authz.ThreadMessage{msg("", "https://unknown.example/x")},
		"owner@example.com",
		[]authz.ThreadSeedRequest{urlSlot([]string{"participants"})}, 0)
	assert.Empty(t, got)
}

// TestSeedFromThread_SlotWithNoDerivableValueKindIsNotSeedable: nothing
// identifies the shape of the slot's values, so there is no honest way to find
// them in text. Fail closed rather than guess at what its ids look like.
func TestSeedFromThread_SlotWithNoDerivableValueKindIsNotSeedable(t *testing.T) {
	got, _ := authz.SeedFromThread(
		[]authz.ThreadMessage{msg("owner@example.com", "https://owner.example/a")},
		"owner@example.com",
		[]authz.ThreadSeedRequest{{
			ResourceType:    "tracker_issue",
			ValueTransforms: nil, // not value-keyed
		}}, 0)
	assert.Empty(t, got)
}

// TestSeedFromThread_IDsMatchWhatTheToolWillCheck is the property that makes a
// seeded grant usable at all: the seeder must mint the id the tool's own Check
// computes, or the grant is written, never matched, and nothing errors.
func TestSeedFromThread_IDsMatchWhatTheToolWillCheck(t *testing.T) {
	chain := []string{"normalize_url", "sha256"}
	got, _ := authz.SeedFromThread(
		// Written with a default port and a dot segment; the tool would
		// canonicalise the same way at Check time.
		[]authz.ThreadMessage{msg("o@example.com", "see HTTPS://Api.Example:443/v1/../v1/x")},
		"o@example.com",
		[]authz.ThreadSeedRequest{{ResourceType: "http_target", ValueTransforms: chain}}, 0)

	require.Len(t, got, 1)
	wantID, err := authz.NewObjectID("https://api.example/v1/x", chain)
	require.NoError(t, err)
	assert.Equal(t, wantID, got[0].ResourceID,
		"a differently-spelled but identical target must produce the id the Check will compute")
}

func TestSeedFromThread_DeduplicatesAcrossMessages(t *testing.T) {
	got, _ := authz.SeedFromThread([]authz.ThreadMessage{
		msg("o@example.com", "https://x.example/a"),
		msg("o@example.com", "again https://x.example/a"),
		msg("o@example.com", "and http://x.example/a"), // different scheme = different target
	}, "o@example.com", []authz.ThreadSeedRequest{urlSlot(nil)}, 0)
	assert.Len(t, got, 2, "one binding per distinct target, not per mention")
}

// The cap must REPORT what it dropped. A seeder that quietly stops reads as
// "the thread had nothing else in it".
func TestSeedFromThread_CapReportsWhatItDropped(t *testing.T) {
	got, dropped := authz.SeedFromThread([]authz.ThreadMessage{
		msg("o@example.com", "https://a.example/1 https://b.example/2 https://c.example/3"),
	}, "o@example.com", []authz.ThreadSeedRequest{urlSlot(nil)}, 2)

	assert.Len(t, got, 2)
	assert.Equal(t, 1, dropped, "the overflow must be counted, not silently truncated")
}

func TestSeedFromThread_NoOpInputs(t *testing.T) {
	got, dropped := authz.SeedFromThread(nil, "o@example.com", []authz.ThreadSeedRequest{urlSlot(nil)}, 0)
	assert.Empty(t, got)
	assert.Zero(t, dropped)

	got, _ = authz.SeedFromThread([]authz.ThreadMessage{msg("o@example.com", "https://x.example/")}, "o@example.com", nil, 0)
	assert.Empty(t, got)
}

func TestExtractCandidates_URL(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "bare url",
			text: "see https://x.example/a for details",
			want: []string{"https://x.example/a"},
		},
		{
			// Chat transports wrap links; stopping at the bracket is plain
			// tokenisation, so this stays out of the channel kinds.
			name: "angle-bracket wrapped (chat transport style)",
			text: "see <https://x.example/a> please",
			want: []string{"https://x.example/a"},
		},
		{
			name: "wrapped with a label",
			text: "see <https://x.example/a|the docs>",
			want: []string{"https://x.example/a"},
		},
		{
			name: "trailing sentence punctuation is not part of the url",
			text: "go to https://x.example/a.",
			want: []string{"https://x.example/a"},
		},
		{
			name: "two urls",
			text: "https://a.example/1 and https://b.example/2",
			want: []string{"https://a.example/1", "https://b.example/2"},
		},
		{
			name: "repeated url appears once",
			text: "https://a.example/1 https://a.example/1",
			want: []string{"https://a.example/1"},
		},
		{
			name: "query is kept — it is part of the target",
			text: "https://x.example/s?q=1",
			want: []string{"https://x.example/s?q=1"},
		},
		{
			name: "a scheme with no host grants nothing",
			text: "https:// and http://",
			want: nil,
		},
		{
			name: "non-http schemes are not extracted",
			text: "ftp://x.example/a mailto:a@b.example",
			want: nil,
		},
		{
			name: "no urls",
			text: "just talking about things",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, authz.ExtractCandidates(authz.ValueKindURL, tc.text))
		})
	}
}

func TestValueKindOf_DerivedFromTheChain(t *testing.T) {
	assert.Equal(t, authz.ValueKindURL, authz.ValueKindOf([]string{"normalize_url", "sha256"}))
	assert.Equal(t, authz.ValueKindURL, authz.ValueKindOf([]string{"normalize_url"}))
	assert.Equal(t, authz.ValueKindUnknown, authz.ValueKindOf([]string{"sha256"}),
		"a bare hash says nothing about the shape of what was hashed")
	assert.Equal(t, authz.ValueKindUnknown, authz.ValueKindOf(nil))
}

// A scheme is case-insensitive per RFC 3986 and normalize_url lowercases it, so
// extraction must not be the one case-sensitive step in the chain.
func TestExtractCandidates_SchemeIsCaseInsensitive(t *testing.T) {
	for _, text := range []string{
		"HTTPS://x.example/a", "Https://x.example/a", "HTTP://x.example/a",
	} {
		got := authz.ExtractCandidates(authz.ValueKindURL, text)
		require.Len(t, got, 1, "text=%q", text)
		// The AUTHOR's spelling is returned; normalize_url canonicalises later.
		assert.Equal(t, text, got[0])
	}
}
