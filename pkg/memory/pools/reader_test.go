package pools_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/pools"
)

// fakeReader returns a fixed relationship set and records the filter it was
// asked for, so a test can assert the query was SUBJECT-scoped rather than a
// full scan.
type fakeReader struct {
	rels []authz.Relation
	got  pools.RelFilter
	err  error
}

func (f *fakeReader) ReadRelationships(_ context.Context, filter pools.RelFilter) ([]authz.Relation, error) {
	f.got = filter
	return f.rels, f.err
}

func grant(objType, objID, perm, ns, name string) authz.Relation {
	return authz.SlotGrantRelation(objType, objID, perm, authz.SessionRef{Namespace: ns, Name: name})
}

func TestForSession_AnySlotMakesAReadPool(t *testing.T) {
	r := &fakeReader{rels: []authz.Relation{
		grant("customer", "alpha", "read", "default", "sess-1"),
		grant("gadget", "beta", "browse", "default", "sess-1"),
	}}

	got, err := pools.ForSession(context.Background(), r, "default", "sess-1")
	require.NoError(t, err)

	want := []string{"customer:alpha", "gadget:beta"}
	assert.ElementsMatch(t, want, scopeIDs(got.Read),
		"any slot on a resource makes it a read pool, whatever the permission")
	assert.Empty(t, got.Write, "no write_memory slot means no write pool")
}

func TestForSession_OnlyWriteMemoryMakesAWritePool(t *testing.T) {
	// TWO resources, and only one carries write_memory: a fixture with a
	// single resource cannot tell "only write_memory grants write" apart from
	// "every slot grants write" — both produce the same one-element Write set.
	// gadget:beta is held only under read, so it must never appear in Write.
	r := &fakeReader{rels: []authz.Relation{
		grant("customer", "alpha", "read", "default", "sess-1"),
		grant("customer", "alpha", "write_memory", "default", "sess-1"),
		grant("gadget", "beta", "read", "default", "sess-1"),
	}}

	got, err := pools.ForSession(context.Background(), r, "default", "sess-1")
	require.NoError(t, err)

	assert.Equal(t, []string{"customer:alpha"}, scopeIDs(got.Write),
		"only a write_memory slot authorizes writing into a pool — gadget:beta must NOT appear")
	assert.Equal(t, []string{"customer:alpha", "gadget:beta"}, scopeIDs(got.Read),
		"both resources are read pools — write does not exclude read, and a read-only slot still reads")
}

// A read pool must appear ONCE however many slots the session holds on it.
// Duplicates would mint duplicate approvals and duplicate pt-tags.
func TestForSession_DeduplicatesAResourceHeldUnderSeveralSlots(t *testing.T) {
	r := &fakeReader{rels: []authz.Relation{
		grant("customer", "alpha", "read", "default", "sess-1"),
		grant("customer", "alpha", "write", "default", "sess-1"),
		grant("customer", "alpha", "browse", "default", "sess-1"),
	}}

	got, err := pools.ForSession(context.Background(), r, "default", "sess-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"customer:alpha"}, scopeIDs(got.Read))
}

// Pools come back in exact sorted order, not merely "however this run's map
// iteration happened to land". FIVE resources, not two: with only two, Go's
// independently-randomized map iteration coincidentally agrees across two
// calls about half the time, which makes "compare two runs" a coin flip
// rather than a guard — see task-2-report.md Finding 2. Five resources drops
// coincidental full-order agreement to roughly 1 in 120 (5!), and asserting
// the exact expected order (rather than cross-run agreement) means "it came
// out in insertion order by chance" cannot pass either — the names are chosen
// so sorted order differs from the insertion order below.
func TestForSession_ReadPoolsComeBackInSortedOrder(t *testing.T) {
	r := &fakeReader{rels: []authz.Relation{
		grant("zulu", "z", "read", "default", "sess-1"),
		grant("mike", "m", "read", "default", "sess-1"),
		grant("alpha", "a", "read", "default", "sess-1"),
		grant("tango", "t", "read", "default", "sess-1"),
		grant("bravo", "b", "read", "default", "sess-1"),
	}}

	got, err := pools.ForSession(context.Background(), r, "default", "sess-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha:a", "bravo:b", "mike:m", "tango:t", "zulu:z"}, scopeIDs(got.Read),
		"pools must come back sorted — they become minted approvals and pt-tags, "+
			"which must be a pure function of the grant set, not of SpiceDB's row order")
}

// A tuple whose relation is not a slot grant must be ignored outright. The
// filter asks SpiceDB for this session's tuples, and a resource can carry
// many relations naming an agentsession for reasons unrelated to slots.
func TestForSession_IgnoresNonSlotRelations(t *testing.T) {
	r := &fakeReader{rels: []authz.Relation{
		{ResourceType: "customer", ResourceID: "alpha", Relation: "watcher",
			SubjectType: "agentsession", SubjectID: "default/sess-1"},
	}}

	got, err := pools.ForSession(context.Background(), r, "default", "sess-1")
	require.NoError(t, err)
	assert.Empty(t, got.Read, "a non-slot_grant relation is not a pool")
}

func TestForSession_ScopesTheQueryToThisSession(t *testing.T) {
	r := &fakeReader{}
	_, err := pools.ForSession(context.Background(), r, "default", "sess-1")
	require.NoError(t, err)
	assert.Equal(t, "agentsession", r.got.SubjectType)
	assert.Equal(t, "default/sess-1", r.got.SubjectID,
		"the read must be subject-scoped, not a scan the caller filters")
}

func TestForSession_SurfacesAReadFailure(t *testing.T) {
	r := &fakeReader{err: assert.AnError}
	_, err := pools.ForSession(context.Background(), r, "default", "sess-1")
	require.Error(t, err, "a failed relationship read must not be reported as 'no pools'")
}

func scopeIDs(ss []memory.Scope) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.ID)
	}
	return out
}
