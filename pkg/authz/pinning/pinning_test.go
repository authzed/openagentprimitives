package pinning

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStrengthLevelOrdersFrozenAboveNamedAboveUnpinned(t *testing.T) {
	cases := []struct {
		name  string
		s     Strength
		level int
	}{
		{name: "frozen is strongest (2)", s: StrengthFrozen, level: 2},
		{name: "named is middle (1)", s: StrengthNamed, level: 1},
		{name: "unpinned is weakest (0)", s: StrengthUnpinned, level: 0},
		{name: "unknown value maps to 0", s: Strength("bogus"), level: 0},
		{name: "empty value maps to 0", s: Strength(""), level: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.level, tc.s.Level())
		})
	}
}
