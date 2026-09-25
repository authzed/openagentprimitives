package spicedb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// These are pure-logic tests: scopeBearingDefinitions never touches the
// network, and ListSourceScopes' capPerDefinition guard returns before it
// ever would. Neither calls relsource.Register, so — unlike
// subject_probes_test.go — nothing here needs the "!integration && !e2e"
// build constraint: there is no global registry write to leak into the
// integration binary. The read itself (readScopeSentinels, a real
// ReadRelationships call) is exercised only in source_scopes_integration_test.go.

// scopeBearingDefinitions derives which definitions are scope-bearing from
// Claims, never from a hand-maintained list: a Claims entry whose relation is
// the #relhash sentinel names one.
func TestScopeBearingDefinitions_DerivesFromSentinelClaimsOnly(t *testing.T) {
	src := relsource.Source{
		Name: "fixture-source",
		Claims: []string{
			"github_org#member",
			"github_org#relhash",
			"github_team#relhash",
			"github_repo#admin",
			"onepassword_group#member",
		},
	}

	got := scopeBearingDefinitions(src)

	assert.Equal(t, []string{"github_org", "github_team"}, got,
		"only claims whose relation is the #relhash sentinel name a scope-bearing definition, sorted")
}

func TestScopeBearingDefinitions_NoSentinelClaimsIsEmpty(t *testing.T) {
	src := relsource.Source{Name: "fixture-source", Claims: []string{"slack_channel#member"}}
	assert.Empty(t, scopeBearingDefinitions(src))
}

// Not expected in practice — a source names each definition#relation once in
// Claims — but the derivation must not double-list a definition if it ever
// appears more than once.
func TestScopeBearingDefinitions_DedupesRepeatedDefinition(t *testing.T) {
	src := relsource.Source{Name: "fixture-source", Claims: []string{
		"slack_channel#relhash",
		"slack_channel#relhash",
	}}
	assert.Equal(t, []string{"slack_channel"}, scopeBearingDefinitions(src))
}

// A caller-programming-error guard: it must fail before any read starts,
// which is exactly what proves it's safe to exercise with a zero-value
// *Client (no real SpiceDB connection) rather than needing the integration
// harness.
func TestListSourceScopes_RejectsNonPositiveCap(t *testing.T) {
	c := &Client{}
	_, err := c.ListSourceScopes(context.Background(), relsource.Source{Name: "fixture-source"}, nil, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "capPerDefinition")
}
