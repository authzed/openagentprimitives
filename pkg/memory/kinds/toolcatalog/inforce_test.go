package toolcatalog_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolcatalog"
)

// InForce is the step function ForTurn and the steelthread replay both resolve
// through. It is tested directly, without a memory scope, because the replay's
// only input is a slice carried in a bundle — and a second copy of "last change
// at or before the turn" would drift at exactly the boundary that matters.
func TestInForce(t *testing.T) {
	changes := []toolcatalog.Content{
		{FromTurnIndex: 1, Tools: []string{"a", "b"}},
		{FromTurnIndex: 5, Tools: []string{"a", "b", "c"}},
	}
	cases := []struct {
		name string
		turn int
		want []string
	}{
		{name: "before the first change: UNKNOWN, not empty", turn: 0, want: nil},
		{name: "the turn a change takes effect is covered by it", turn: 1, want: []string{"a", "b"}},
		{name: "between changes: the earlier set stays in force", turn: 4, want: []string{"a", "b"}},
		{name: "the turn the second change takes effect", turn: 5, want: []string{"a", "b", "c"}},
		{name: "after the last change: it stays in force indefinitely", turn: 99, want: []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, toolcatalog.InForce(changes, tc.turn))
		})
	}

	t.Run("no changes at all: UNKNOWN at every turn", func(t *testing.T) {
		assert.Nil(t, toolcatalog.InForce(nil, 7))
	})
}
