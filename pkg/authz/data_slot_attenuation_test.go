package authz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// fakeTagAccessChecker answers "does this session hold this permission on this
// resource" from a fixture keyed by "<type>:<id>#<perm>@<ns/name>".
type fakeTagAccessChecker struct {
	holds map[string]bool
	err   error
	asked []string
}

func (f *fakeTagAccessChecker) SessionHasOnResource(
	_ context.Context, resourceType, resourceID, permission string, session authz.SessionRef,
) (bool, error) {
	key := resourceType + ":" + resourceID + "#" + permission + "@" + session.Namespace + "/" + session.Name
	f.asked = append(f.asked, key)
	if f.err != nil {
		return false, f.err
	}
	return f.holds[key], nil
}

var parentRef = authz.SessionRef{Namespace: "ns", Name: "parent"}

// TestAParentMayOnlyBindTagsItCanItselfRead is the attenuation rule.
//
// "A parent must not bind what it has no standing to delegate" — the data
// column's version of approverCanDelegateSlots. The parent's judgment selects
// WITHIN an envelope it cannot widen, so a compromised parent's worst case is
// bounded by its own reach: the plan gate's property, grants nothing, can only
// fail to deny.
func TestAParentMayOnlyBindTagsItCanItselfRead(t *testing.T) {
	c := &fakeTagAccessChecker{holds: map[string]bool{
		"pt_tag:ptt-mine#access@ns/parent": true,
	}}

	delegable, refused, err := authz.FilterDelegableDataSlots(context.Background(), c, parentRef,
		[]authz.DataSlotBinding{
			{Slot: "diff", TagID: "ptt-mine"},
			{Slot: "logs", TagID: "ptt-someone-elses"},
		})
	require.NoError(t, err)

	require.Len(t, delegable, 1)
	assert.Equal(t, "ptt-mine", delegable[0].TagID)
	require.Len(t, refused, 1)
	assert.Equal(t, "ptt-someone-elses", refused[0].TagID,
		"a tag the parent cannot read is not the parent's to hand on")

	assert.Equal(t, []string{
		"pt_tag:ptt-mine#access@ns/parent",
		"pt_tag:ptt-someone-elses#access@ns/parent",
	}, c.asked, "one check per slot, asked as access on the TAG")
}

// TestACheckFailureRefusesRatherThanDelegates pins the failure direction.
//
// An unanswerable question is not a yes. Treating a SpiceDB error as "probably
// fine" would let a transient outage widen what a parent may hand to a child,
// which is the one moment nobody is watching.
func TestACheckFailureRefusesRatherThanDelegates(t *testing.T) {
	c := &fakeTagAccessChecker{err: errors.New("spicedb unavailable")}

	_, _, err := authz.FilterDelegableDataSlots(context.Background(), c, parentRef,
		[]authz.DataSlotBinding{{Slot: "diff", TagID: "ptt-1"}})
	require.Error(t, err, "a check that could not be answered must not resolve as delegable")
}

// TestNoCheckerRefusesEverything is the fail-closed default.
//
// approverCanDelegateSlots treats "no lookup wired" as everything-delegable,
// which is safe THERE because the instance axis is re-checked at
// OrderToolCallAuthz afterwards. There is no such second gate for a data slot:
// binding the tag IS the grant, and the child reads through it immediately. So
// the same shortcut here would hand over every tag a parent named.
func TestNoCheckerRefusesEverything(t *testing.T) {
	delegable, refused, err := authz.FilterDelegableDataSlots(context.Background(), nil, parentRef,
		[]authz.DataSlotBinding{{Slot: "diff", TagID: "ptt-1"}})
	require.NoError(t, err)
	assert.Empty(t, delegable,
		"a data slot has no second gate behind it, so an unwired checker must not wave bindings through")
	require.Len(t, refused, 1)
}

func TestFilterDelegableDataSlotsDedupesRepeatedTags(t *testing.T) {
	c := &fakeTagAccessChecker{holds: map[string]bool{"pt_tag:ptt-1#access@ns/parent": true}}

	delegable, _, err := authz.FilterDelegableDataSlots(context.Background(), c, parentRef,
		[]authz.DataSlotBinding{
			{Slot: "diff", TagID: "ptt-1"},
			{Slot: "also_diff", TagID: "ptt-1"},
		})
	require.NoError(t, err)
	assert.Len(t, delegable, 2, "the same tag in two slots is two bindings")
	assert.Len(t, c.asked, 1,
		"but only one question: a parent's standing on a tag does not change between slots, and a card should not wait on repeated identical lookups")
}
