package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento" // a kind that declares no session links
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack" // the only kind that declares session links today
)

// SessionRelationLinks is the one place the build's channel-contributed
// subject-set link types are gathered. Both the guardian composer (which unions
// them into the live agentsession relations) and every gate that must admit
// exactly what that schema admits read this same call, so the union has to be
// derived from the registry rather than transcribed anywhere.
func TestSessionRelationLinks_UnionsEveryRegisteredKindsDeclaration(t *testing.T) {
	links := registry.SessionRelationLinks()

	// Derived, not transcribed: whatever each registered kind declares must be
	// present, so a newly registered kind is covered without editing this test.
	for _, k := range registry.All() {
		rl, ok := k.(channelkinds.SessionRelationLinker)
		if !ok {
			continue
		}
		for _, want := range rl.SessionRelationLinks() {
			assert.Containsf(t, links, want,
				"kind %q declares link %q; the union must carry it", k.Name(), want)
		}
	}

	// …and the union is exactly the kinds' declarations — nothing invented here.
	var declared []string
	for _, k := range registry.All() {
		if rl, ok := k.(channelkinds.SessionRelationLinker); ok {
			declared = append(declared, rl.SessionRelationLinks()...)
		}
	}
	require.NotEmpty(t, declared, "at least one registered kind must declare session links for this test to mean anything")
	for _, got := range links {
		assert.Containsf(t, declared, got, "link %q was not declared by any registered kind", got)
	}
}

// Sorted and de-duplicated: the composed schema text must be byte-stable across
// reconciles, and two kinds may legitimately name the same link type.
func TestSessionRelationLinks_SortedAndDeduplicated(t *testing.T) {
	links := registry.SessionRelationLinks()
	require.NotEmpty(t, links)

	seen := map[string]bool{}
	for i, l := range links {
		assert.Falsef(t, seen[l], "duplicate link %q", l)
		seen[l] = true
		if i > 0 {
			assert.LessOrEqualf(t, links[i-1], l, "links must be sorted; %q precedes %q", links[i-1], l)
		}
	}
}
