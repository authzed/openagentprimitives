package goals

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// TestGrantGroupRun: a group run's own session is granted execute on its
// execution (the run tuple) only while the domain's consent holds
// (execute_group), expiring with the run. A private goal is never granted a
// run tuple.
func TestGrantGroupRun(t *testing.T) {
	expires := time.Date(2026, 11, 17, 9, 5, 0, 0, time.UTC)
	group := core.Goal{ID: "goal-1", Domain: core.Domain{Namespace: groupNS, Owner: groupOwner, Class: "assistant", ClassUID: "class-uid"}, Execution: &core.ExecutionConsent{Digest: "exec-digest"}}
	private := group
	private.Domain.Owner = "alice"
	runTuple := authz.Relation{ResourceType: ExecutionResourceType, ResourceID: "exec-digest", Relation: "run", SubjectType: "agentsession", SubjectID: groupNS + "/run-1", ExpiresAt: expires}

	cases := []struct {
		name     string
		goal     core.Goal
		approved bool
		wantErr  error
		want     []authz.Relation
	}{
		{name: "group consent in force: run tuple written with the run's expiry", goal: group, approved: true, want: []authz.Relation{runTuple}},
		{name: "group consent not in force: refused, nothing written", goal: group, wantErr: core.ErrDenied},
		{name: "private goal: refused, nothing written", goal: private, approved: true, wantErr: core.ErrDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGroupFixture(t)
			if tc.approved {
				for _, r := range executionGrant(group) {
					f.auth.tuples[r] = true
				}
			}
			err := f.s.GrantGroupRun(context.Background(), tc.goal, groupNS+"/run-1", expires)
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			var written []authz.Relation
			for _, r := range f.auth.written {
				if r.Relation == "run" {
					written = append(written, r)
				}
			}
			assert.Equal(t, tc.want, written)
		})
	}
}
