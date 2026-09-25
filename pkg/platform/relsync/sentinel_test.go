package relsync

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// The sentinel relation name is declared in relsource (the package every
// consumer can reach — see its own doc) while the sync engine keeps its
// internal constant. Nothing but this test stops the two from drifting, and a
// drift would silently stop readers skipping the sentinel.
func TestSentinelRelationMatchesRelsource(t *testing.T) {
	assert.Equal(t, relsource.SentinelRelation, relhashRelation)
}
