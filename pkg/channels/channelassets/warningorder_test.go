package channelassets_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
)

func TestSortWarnings_IsTotalAndStable(t *testing.T) {
	in := []channelassets.Warning{
		{Kind: "tag", Name: "title", Action: "removed", Count: 1},
		{Kind: "attr", Name: "lang", Action: "stripped", Count: 1},
		{Kind: "css", Name: "@import", Action: "stripped", Count: 2},
		{Kind: "tag", Name: "meta", Action: "unwrapped", Count: 1},
		{Kind: "attr", Name: "charset", Action: "stripped", Count: 1},
	}
	want := []channelassets.Warning{
		{Kind: "attr", Name: "charset", Action: "stripped", Count: 1},
		{Kind: "attr", Name: "lang", Action: "stripped", Count: 1},
		{Kind: "css", Name: "@import", Action: "stripped", Count: 2},
		{Kind: "tag", Name: "meta", Action: "unwrapped", Count: 1},
		{Kind: "tag", Name: "title", Action: "removed", Count: 1},
	}

	got := append([]channelassets.Warning(nil), in...)
	channelassets.SortWarnings(got)
	assert.Equal(t, want, got, "kind, then name, then action")

	// Idempotent, which is what lets the encoder sort defensively over input a
	// renderer already sorted.
	channelassets.SortWarnings(got)
	assert.Equal(t, want, got, "sorting an already-canonical list changes nothing")
}

func TestWarningSortKey_SeparatorCannotBeForged(t *testing.T) {
	// Without a separator no field can contain, "tag"+"ab" and "ta"+"gab" would
	// key identically, and two different warnings would compare equal.
	assert.NotEqual(t,
		channelassets.WarningSortKey("tag", "ab", "removed"),
		channelassets.WarningSortKey("ta", "gab", "removed"))
}

func TestSortWarnings_EmptyAndSingle(t *testing.T) {
	channelassets.SortWarnings(nil) // must not panic
	one := []channelassets.Warning{{Kind: "tag", Name: "x", Action: "removed", Count: 1}}
	channelassets.SortWarnings(one)
	assert.Len(t, one, 1)
}
