package runner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
)

// TestRecordAuthzDecision_EnforceModeIsWhatApplied pins the DURABLE row, not a
// value in flight.
//
// The audit is the only thing left to read months later, and "was this denial
// disableable by an operator setting?" is exactly the question it will be asked.
// A slot-precondition denial stamps Result.EnforceOverride at dispatch — that
// stamp is what kept the call from proceeding under toolCalls.mode: permissive —
// while the class's own enforceMode is usually empty. Recording the declaration
// would answer that question with "yes" about a denial that was final.
//
// Written through the real recordAuthzDecision and read back through the real
// accessor: a helper asserted in isolation would pass while the caller kept
// stamping the declared value.
func TestRecordAuthzDecision_EnforceModeIsWhatApplied(t *testing.T) {
	cases := []struct {
		name string
		// declared is what the AgentClass wrote on the check.
		declared authz.EnforceMode
		// override is what dispatch stamped on the Result. Empty is every
		// ordinary decision: nothing overrode anything.
		override authz.EnforceMode
		want     string
		why      string
	}{
		{
			name:     "precondition denial over a class that declared nothing: the record says always",
			declared: "",
			override: authz.EnforceAlways,
			want:     "always",
			why:      "the override is the only reason the call did not proceed under permissive; the row has to say so",
		},
		{
			name:     "ordinary denial, nothing declared: the record stays empty",
			declared: "",
			override: "",
			want:     "",
			why:      "an ordinary denial IS disableable by toolCalls.mode, and the audit must not claim otherwise",
		},
		{
			name:     "class declared always, nothing overrode it: the declaration is what applied",
			declared: authz.EnforceAlways,
			override: "",
			want:     "always",
			why:      "the declared mode is still the source when dispatch stamped nothing",
		},
		{
			name:     "class declared inherit, dispatch overrode it: the override is what applied",
			declared: authz.EnforceInherit,
			override: authz.EnforceAlways,
			want:     "always",
			why:      "a precondition is not disableable by a class setting either; the stronger answer is the true one",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := memory.NewLocal(inmem.NewBackend())
			key := memory.NamespacedName{Namespace: "ns", Name: "enforce-mode"}
			scope := memory.Scope{Kind: "session", ID: key.Namespace + "/" + key.Name}
			// recordAuthzDecision reaches the facade directly, and the facade's
			// write door is capability-gated; production's runner memory is the
			// HTTP client, which carries the caller's own approval.
			ctx := memory.WithSystemApproval(context.Background(), "test")

			l := &Loop{Mem: mem, SessionKey: key}
			l.recordAuthzDecision(ctx, "inspect",
				authz.Permission{
					StateImpact: authz.Readonly,
					Check: &authz.PermissionCheck{
						ResourceType: "git_commit",
						Permission:   "read",
						EnforceMode:  tc.declared,
					},
				},
				authz.Result{
					Outcome:         authz.OutcomeDenied,
					Message:         "runner-test denial",
					EnforceOverride: tc.override,
				},
				authz.Inputs{Subject: "demo-user@example.test"})

			res, err := mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{"authz_decision"}})
			require.NoError(t, err)
			require.Len(t, res.Entries, 1, "recordAuthzDecision must write exactly one row per checked call")

			var got authzdecision.Decision
			require.NoError(t, json.Unmarshal(res.Entries[0].Content, &got))
			assert.Equal(t, tc.want, got.EnforceMode, tc.why)
		})
	}
}
