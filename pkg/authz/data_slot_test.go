package authz_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// recordingRelWriter (slot_grant_test.go) is reused: data slots and authz
// slots write through the same RelWriter, and a second recorder would let the
// two halves of one mechanism be asserted against different fakes.

var dataSlotScope = authz.SessionRef{Namespace: "ns", Name: "child"}

// TestGrantDataSlotsBindsTheTagToTheChild pins the tuple a data slot writes.
//
// It points at the TAG, not at the session: `pt_tag:<T>#granted_to@agentsession
// :<ns/child>`. That direction is what makes `access` resolve for the child and
// what makes the grant revocable by deleting one tuple the granter named.
func TestGrantDataSlotsBindsTheTagToTheChild(t *testing.T) {
	w := &recordingRelWriter{}
	exp := time.Now().Add(time.Hour)

	require.NoError(t, authz.GrantDataSlots(context.Background(), w, dataSlotScope,
		[]authz.DataSlotBinding{{Slot: "diff", TagID: "ptt-7"}}, exp))

	require.Len(t, w.wrote, 1)
	rel := w.wrote[0]
	assert.Equal(t, "pt_tag", rel.ResourceType)
	assert.Equal(t, "ptt-7", rel.ResourceID)
	assert.Equal(t, "granted_to", rel.Relation)
	assert.Equal(t, "agentsession", rel.SubjectType)
	assert.Equal(t, "ns/child", rel.SubjectID)
	assert.Equal(t, exp, rel.ExpiresAt,
		"granted_to is declared `agentsession with expiration`, so a tuple without one is refused at WRITE time")
}

// TestGrantDataSlotsRefusesAZeroExpiry mirrors the authz-slot rule.
//
// The schema requires an expiration, so SpiceDB would refuse this anyway — but
// the refusal belongs here, where the caller can read WHY, rather than as a
// caveat error surfacing from three layers down.
func TestGrantDataSlotsRefusesAZeroExpiry(t *testing.T) {
	w := &recordingRelWriter{}
	err := authz.GrantDataSlots(context.Background(), w, dataSlotScope,
		[]authz.DataSlotBinding{{Slot: "diff", TagID: "ptt-7"}}, time.Time{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expiry")
	assert.Empty(t, w.wrote, "nothing may be written when the expiry is missing")
}

func TestGrantDataSlotsRefusesAnIncompleteBinding(t *testing.T) {
	cases := []struct {
		name string
		b    authz.DataSlotBinding
	}{
		{name: "no slot name", b: authz.DataSlotBinding{TagID: "ptt-7"}},
		{name: "no tag id", b: authz.DataSlotBinding{Slot: "diff"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &recordingRelWriter{}
			err := authz.GrantDataSlots(context.Background(), w, dataSlotScope,
				[]authz.DataSlotBinding{tc.b}, time.Now().Add(time.Hour))
			require.Error(t, err)
			assert.Empty(t, w.wrote)
		})
	}
}

// TestRevokeDataSlotsDeletesExactlyWhatWasGranted keeps the pair symmetric.
// Revocation must be possible for every grant that can be written, or a
// binding outlives the reason it was made.
func TestRevokeDataSlotsDeletesExactlyWhatWasGranted(t *testing.T) {
	w := &recordingRelWriter{}
	bindings := []authz.DataSlotBinding{{Slot: "diff", TagID: "ptt-7"}, {Slot: "logs", TagID: "ptt-12"}}
	require.NoError(t, authz.GrantDataSlots(context.Background(), w, dataSlotScope, bindings, time.Now().Add(time.Hour)))
	require.NoError(t, authz.RevokeDataSlots(context.Background(), w, dataSlotScope, bindings))

	require.Len(t, w.deleted, 2)
	for i, rel := range w.deleted {
		assert.Equal(t, w.wrote[i].ResourceType, rel.ResourceType)
		assert.Equal(t, w.wrote[i].ResourceID, rel.ResourceID)
		assert.Equal(t, w.wrote[i].Relation, rel.Relation)
		assert.Equal(t, w.wrote[i].SubjectID, rel.SubjectID)
	}
}

// TestDataSlotBindingCarriesAReferenceAndNothingElse looks like it tests the
// obvious, and it is the most important test in this file.
//
// Data passes BY REFERENCE, never inlined: a slot binds a tag id, and the
// child reads the content through the tag's own audience check. That is the
// architectural separation of instruction and data channels the whole handoff
// design rests on — the parent structurally CANNOT launder content into a data
// slot, because there is nowhere to put it.
//
// A `Content string` field added later "for convenience" would defeat that
// entirely, and it would pass every other test here. This is the only thing
// standing in its way, so it asserts on the STRUCT rather than on behaviour.
func TestDataSlotBindingCarriesAReferenceAndNothingElse(t *testing.T) {
	tv := reflect.TypeOf(authz.DataSlotBinding{})
	got := make([]string, 0, tv.NumField())
	for i := range tv.NumField() {
		got = append(got, tv.Field(i).Name)
	}
	assert.Equal(t, []string{"Slot", "TagID"}, got,
		"a data slot binds a REFERENCE. A field able to hold content would let a parent inline data "+
			"into the slot, which is exactly the laundering the instruction/data channel separation exists "+
			"to prevent — and it would pass every other test in this package. If a field is genuinely "+
			"needed, it must not be able to carry the datum itself.")

	for i := range tv.NumField() {
		f := tv.Field(i)
		assert.NotContains(t, strings.ToLower(f.Name), "content",
			"field %q reads as payload rather than reference", f.Name)
		assert.NotContains(t, strings.ToLower(f.Name), "body",
			"field %q reads as payload rather than reference", f.Name)
	}
}
