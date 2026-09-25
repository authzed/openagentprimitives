package spec

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPathKey(t *testing.T) {
	cases := []struct {
		name    string
		section string
		index   int
		want    string
	}{
		{name: "allowSubcommands[0] indexes single-digit", section: "allowSubcommands", index: 0, want: "allowSubcommands[0]"},
		{name: "allowSubcommands[12] indexes two-digit", section: "allowSubcommands", index: 12, want: "allowSubcommands[12]"},
		{name: "constraints[1] indexes constraints", section: "constraints", index: 1, want: "constraints[1]"},
		{name: "exceptions[2] indexes exceptions", section: "exceptions", index: 2, want: "exceptions[2]"},
		{name: "negative index returns bare section (deny.effects.destructive)", section: "deny.effects.destructive", index: -1, want: "deny.effects.destructive"},
		{name: "negative index returns bare section (allow.network.destinations)", section: "allow.network.destinations", index: -1, want: "allow.network.destinations"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, PathKey(tc.section, tc.index), "PathKey(%q, %d)", tc.section, tc.index)
		})
	}
}
