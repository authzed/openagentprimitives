package spicedbauthorizer_test

import (
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/spicedbauthorizer"
)

// Marks the claim table complete directly rather than importing
// pkg/authz/spicedb/relsource/imports: this test only needs
// spicedbauthorizer's own claims, already registered by the plain import
// above, and pulling in the rest of the bundle's packages for one file's
// two tests buys nothing. CheckDeleteFilter otherwise refuses every call
// regardless of what spicedbauthorizer.Source claims.
func init() {
	relsource.MarkComplete()
}

// cleanupScopeFilter builds the exact RelationshipFilter shape
// (*Authorizer).CleanupScope issues: memory_entry, an id prefix, and
// deliberately no OptionalRelation — the could-match case CheckDeleteFilter
// exists to police. Mirroring the production shape here (rather than a
// simplified stand-in) is what makes this test a statement about
// CleanupScope's own behaviour, not a generic relsource exercise.
func cleanupScopeFilter() *v1.RelationshipFilter {
	scope := memory.Scope{Kind: "session", ID: "default/my-session"}
	return &v1.RelationshipFilter{
		ResourceType:             "memory_entry",
		OptionalResourceIdPrefix: spicedbauthorizer.ScopePrefix(scope),
	}
}

// spicedbauthorizer.Source claims BOTH memory_entry relations
// (memory_entry#session, memory_entry#creator) — the type's only two
// claims, both owned by this one Source. CleanupScope's empty-relation
// delete filter therefore passes the could-match rule's allow arm: it
// matches every claim on the type, and this Source owns every claim on the
// type. That equivalence is exactly what would break, silently, the day a
// different source claims a third memory_entry relation — see the sibling
// test below for the other half of the pin.
func TestCleanupScope_DeleteFilter_OwnSourceAllowed(t *testing.T) {
	err := relsource.CheckDeleteFilter(spicedbauthorizer.Source, cleanupScopeFilter())
	assert.NoError(t, err, "CleanupScope's own empty-relation filter must be allowed: spicedbauthorizer owns every claim on memory_entry")
}

// The same filter, issued by a source that owns none of memory_entry's
// claims, must be refused — an empty-relation filter could match either
// claimed relation, and only the owner of ALL of them may sweep with no
// relation named.
func TestCleanupScope_DeleteFilter_OtherSourceRefused(t *testing.T) {
	other := relsource.Source{Name: "some-other-source"}

	err := relsource.CheckDeleteFilter(other, cleanupScopeFilter())

	require.Error(t, err, "an empty-relation filter on memory_entry must be refused for a source that owns none of its claims")
	assert.Contains(t, err.Error(), "some-other-source")
	assert.Contains(t, err.Error(), "memory_entry")
}
