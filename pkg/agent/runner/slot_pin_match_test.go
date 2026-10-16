package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// TestMentionsResourceType pins the token-boundary match slotPinRefusalFor uses
// to tie a recorded pin refusal to the denial it explains — including the edge
// that a declared type whose name is a tail of another must NOT match the
// longer one's refusal, and that both refusal shapes (drift and multi-arrival)
// name the type on a non-identifier boundary.
func TestMentionsResourceType(t *testing.T) {
	const drift = "slot already pinned to a different instance: this session is pinned to " +
		"github_pull_request:acme/original; to work on github_pull_request:acme/widgets, propose an updated plan"
	const multi = "slot type github_repo cannot hold 2 distinct instances at once; it is single-occupancy"

	cases := []struct {
		name         string
		text         string
		resourceType string
		want         bool
	}{
		{name: "drift refusal names the exact type", text: drift, resourceType: "github_pull_request", want: true},
		{name: "tail type must not match the longer type's refusal", text: drift, resourceType: "pull_request", want: false},
		{name: "unrelated type does not match", text: drift, resourceType: "github_repo", want: false},
		{name: "multi-arrival refusal names the type before a space", text: multi, resourceType: "github_repo", want: true},
		{name: "tail type must not match the multi-arrival refusal", text: multi, resourceType: "repo", want: false},
		{name: "empty text never matches", text: "", resourceType: "github_repo", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mentionsResourceType(tc.text, tc.resourceType))
		})
	}
}

// fakePinReader is the minimal authz.RelWriter + authz.SlotPinner the refusal
// record needs: ReadPin answers from a canned map; everything else no-ops.
type fakePinReader struct {
	pins map[string]string // keyed by resource type
}

func (f *fakePinReader) WriteRelationships(context.Context, []authz.Relation) error  { return nil }
func (f *fakePinReader) DeleteRelationships(context.Context, []authz.Relation) error { return nil }
func (f *fakePinReader) EnsurePin(_ context.Context, rt, id string, _ authz.SessionRef) (bool, string, error) {
	if cur, ok := f.pins[rt]; ok {
		return true, cur, nil
	}
	f.pins[rt] = id
	return false, id, nil
}
func (f *fakePinReader) WriteGrantsPinned(context.Context, []authz.Relation, string, string, authz.SessionRef) error {
	return nil
}
func (f *fakePinReader) MovePin(_ context.Context, rt, _, toID string, _ []authz.Relation, _ authz.SessionRef) error {
	f.pins[rt] = toID
	return nil
}
func (f *fakePinReader) ReadPin(_ context.Context, rt string, _ authz.SessionRef) (string, error) {
	return f.pins[rt], nil
}
func (f *fakePinReader) ListGrantsFor(context.Context, string, string, authz.SessionRef) ([]authz.Relation, error) {
	return nil, nil
}

const pinRefusalToB = "slot already pinned to a different instance: this session is pinned to " +
	"github_repo:demo-org/a; to work on github_repo:demo-org/b, propose an updated plan naming it — an approved plan moves the pin"

const pinRefusalToC = "slot already pinned to a different instance: this session is pinned to " +
	"github_repo:demo-org/a; to work on github_repo:demo-org/c, propose an updated plan naming it — an approved plan moves the pin"

// TestSlotPinRefusals_NewestWinsAndClearedOnSuccess pins the per-type record's
// two lifecycle rules. Newest wins: two refusals recorded for one type keep
// only the later, so the model is never handed an explanation older than the
// most recent ruling. Cleared on success: the clear the promote nil-error and
// narrowToApproved success paths invoke empties the type's entry, so after an
// approved move (or a promotion that now binds) a subsequent denial for that
// type carries NO stale append — slotPinRefusalFor finds nothing to attach.
func TestSlotPinRefusals_NewestWinsAndClearedOnSuccess(t *testing.T) {
	l := &Loop{
		SessionKey: memory.NamespacedName{Namespace: "default", Name: "s1"},
		SlotBinder: &fakePinReader{pins: map[string]string{"github_repo": "demo-org/a"}},
	}
	specs := []authz.BoundEntitySpec{{ResourceType: "github_repo"}}
	ctx := context.Background()

	l.recordSlotPinRefusal(ctx, specs, pinRefusalToB)
	l.recordSlotPinRefusal(ctx, specs, pinRefusalToC)

	ref, ok := l.slotPinRefusalFor("github_repo")
	require.True(t, ok, "a recorded refusal must be retrievable by its type")
	assert.Equal(t, pinRefusalToC, ref.Text, "newest refusal must win, not the oldest")
	assert.Equal(t, "demo-org/a", ref.PinnedID, "the pinned id is read back at record time")

	// Simulate the success/move path's clear (the call narrowToApproved and the
	// promote nil-error branches make with the same type list in hand).
	l.clearSlotPinRefusals(bindingResourceTypes([]authz.SlotBinding{
		{ResourceType: "github_repo", Permission: "read"},
	}))

	_, ok = l.slotPinRefusalFor("github_repo")
	assert.False(t, ok,
		"after the type's bind/move succeeds, no refusal remains — the attachment seam "+
			"appends only on a slotPinRefusalFor hit, so a subsequent denial carries no stale append")
}

// TestSlotPinRefusals_ClearForSpecs covers the promote-success shape of the
// clear, which derives the type list from the specs in hand.
func TestSlotPinRefusals_ClearForSpecs(t *testing.T) {
	l := &Loop{SessionKey: memory.NamespacedName{Namespace: "default", Name: "s1"}}
	specs := []authz.BoundEntitySpec{{ResourceType: "github_repo"}}
	l.recordSlotPinRefusal(context.Background(), specs, pinRefusalToB)
	_, ok := l.slotPinRefusalFor("github_repo")
	require.True(t, ok)

	l.clearSlotPinRefusalsForSpecs(specs)
	_, ok = l.slotPinRefusalFor("github_repo")
	assert.False(t, ok, "a nil-error promotion clears its specs' types")
}

// TestPinRefusalApplies pins the mislabel guard: a denial whose call resolved
// to the PINNED instance itself (refused by some other gate) must not get the
// "move" advice, while every uncertain case fails open to attaching.
func TestPinRefusalApplies(t *testing.T) {
	check := authz.PermissionCheck{
		ResourceType:       "github_repo",
		ResourceIDTemplate: "{repo}",
		Permission:         "read",
	}
	ref := slotPinRefusal{Text: pinRefusalToB, PinnedID: "demo-org/a"}

	cases := []struct {
		name string
		ref  slotPinRefusal
		args map[string]any
		want bool
	}{
		{name: "call names the pinned instance itself: skip the move advice",
			ref: ref, args: map[string]any{"repo": "demo-org/a"}, want: false},
		{name: "call names a different instance: attach",
			ref: ref, args: map[string]any{"repo": "demo-org/b"}, want: true},
		{name: "no pinned id recorded: fail open to attaching",
			ref: slotPinRefusal{Text: pinRefusalToB}, args: map[string]any{"repo": "demo-org/a"}, want: true},
		{name: "id does not resolve (arg absent): fail open to attaching",
			ref: ref, args: map[string]any{}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, pinRefusalApplies(tc.ref, check, tc.args))
		})
	}
}
