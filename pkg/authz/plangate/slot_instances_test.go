package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSlotInstanceConflicts(t *testing.T) {
	slot := func(typ, id string) Slot { return Slot{Type: typ, ID: id} }
	phase := func(slots ...Slot) Phase { return Phase{Label: "p", Slots: slots} }
	cases := []struct {
		name string
		plan Plan
		want []SlotInstanceConflict
	}{
		{name: "one instance per type: no conflict", plan: Plan{Phases: []Phase{phase(slot("repo", "a"), slot("board", "b"))}}},
		{name: "same instance named twice: one instance, no conflict", plan: Plan{Phases: []Phase{phase(slot("repo", "a")), phase(slot("repo", "a"))}}},
		{name: "unnamed slot beside a named one: no conflict", plan: Plan{Phases: []Phase{phase(slot("repo", ""), slot("repo", "a"))}}},
		{
			name: "two instances in one phase: conflict on that type",
			plan: Plan{Phases: []Phase{phase(slot("repo", "a"), slot("repo", "b"), slot("board", "x"))}},
			want: []SlotInstanceConflict{{ResourceType: "repo", IDs: []string{"a", "b"}}},
		},
		{
			name: "two instances across phases: conflict, in plan order",
			plan: Plan{Phases: []Phase{phase(slot("board", "x"), slot("repo", "a")), phase(slot("repo", "b"), slot("board", "y"))}},
			want: []SlotInstanceConflict{{ResourceType: "board", IDs: []string{"x", "y"}}, {ResourceType: "repo", IDs: []string{"a", "b"}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, SlotInstanceConflicts(tc.plan))
		})
	}
}

func TestSlotInstanceConflictRefusalSaysWhy(t *testing.T) {
	text := SlotInstanceConflict{ResourceType: "github_repo", IDs: []string{"acme/a", "acme/b"}}.RefusalText()
	assert.Contains(t, text, "a run can hold one github_repo at a time")
	assert.Contains(t, text, "acme/a, acme/b")
}
