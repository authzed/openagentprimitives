package onepassword_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/onepassword"
)

// The sentinel rides a relation like any other, so the source must claim it.
// An unclaimed sentinel is writable by anything, and "nothing else can forge
// the sentinel" is exactly what makes the hash short-circuit trustworthy. The
// display-name label is claimed for the same reason with one extra edge: it is
// what the admin console SHOWS, so an unclaimed one would let any other writer
// decide what a synced group is called.
func TestDirectorySyncSource_ClaimsEveryRelationTheKindWrites(t *testing.T) {
	claims := onepassword.DirectorySyncSource.Claims

	assert.ElementsMatch(t, []string{
		"onepassword_group#member",
		"onepassword_group#relhash",
		"onepassword_group#label",
	}, claims, "exactly the relations this kind writes — no more, no less")
}

// The bundle is a hand-maintained transcription; a claim owner missing from
// it yields a partial table in EVERY binary with the sentinel latched true,
// which is the Critical sub-project 2 shipped once already. The tree-walking
// guard in relsource/imports catches it generically; this asserts it for THIS
// package, so the failure names the package rather than the bundle.
func TestSourceIsRegistered(t *testing.T) {
	var found bool
	for _, s := range relsource.All() {
		if s.Name == "onepassworddirectorysync" {
			found = true
			assert.ElementsMatch(t, []string{
				"onepassword_group#member",
				"onepassword_group#relhash",
				"onepassword_group#label",
			}, s.Claims, "the registered source's claims must match the declared var")
		}
	}
	assert.True(t, found, "init() must register this source; an unregistered claim owner is inert")
}
