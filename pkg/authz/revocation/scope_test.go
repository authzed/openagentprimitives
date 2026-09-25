package revocation

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestApplies(t *testing.T) {
	cases := []struct {
		name, scope, myNS string
		want              bool
	}{
		{"cluster-wide applies to any runner", "", "team-a", true},
		{"matching namespace applies", "team-a", "team-a", true},
		{"other namespace does not apply", "team-a", "team-b", false},
		// A cluster-wide consumer (the operator, whose broker caches credentials
		// from every namespace) must receive namespace-scoped revocations too.
		// Without this it would depend on the accident that credential revokes
		// are currently emitted with scope "" — a silent security regression the
		// day someone scopes them per-namespace.
		{"AllNamespaces consumer receives namespace-scoped revoke", "team-a", AllNamespaces, true},
		{"AllNamespaces consumer receives cluster-wide revoke", "", AllNamespaces, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Applies(tc.scope, tc.myNS))
		})
	}
}
