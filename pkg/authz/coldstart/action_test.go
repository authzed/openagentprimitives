package coldstart_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz/coldstart"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
)

// TestActionForDecision verifies the dec → cold-start action mapping: an
// explicit Action wins; absent an Action the bool is mapped (approve →
// ApproveCleaned, deny → Deny). Moved from internal/cmd/authzd metaagent_worker_test.go.
func TestActionForDecision(t *testing.T) {
	cases := []struct {
		name string
		dec  approval.Decision
		want string
	}{
		{name: "explicit approve_cleaned", dec: approval.Decision{Approved: true, Action: coldstart.ActionApproveCleaned}, want: coldstart.ActionApproveCleaned},
		{name: "explicit approve_original", dec: approval.Decision{Approved: true, Action: coldstart.ActionApproveOriginal}, want: coldstart.ActionApproveOriginal},
		{name: "explicit run_without_scope", dec: approval.Decision{Approved: true, Action: coldstart.ActionRunWithoutScope}, want: coldstart.ActionRunWithoutScope},
		{name: "explicit deny", dec: approval.Decision{Approved: false, Action: coldstart.ActionDeny}, want: coldstart.ActionDeny},
		{name: "no action, approved → cleaned", dec: approval.Decision{Approved: true}, want: coldstart.ActionApproveCleaned},
		{name: "no action, denied → deny", dec: approval.Decision{Approved: false}, want: coldstart.ActionDeny},
		{name: "unknown action, approved → cleaned (bool fallback)", dec: approval.Decision{Approved: true, Action: "bogus"}, want: coldstart.ActionApproveCleaned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, coldstart.ActionForDecision(tc.dec))
		})
	}
}
