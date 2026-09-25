package hooks_test

// The same-turn half of the plural read path.
//
// info_leak_read records the pools a call touched; info_leak_audience gates
// THIS result against the channel's audience right after. They read the same
// declaration, and if only the read hook learned to see plural resources the
// same-turn gate would be blind to every memory tool while the read hook
// dutifully tagged it — a result reaching a channel member who holds no
// view_memory on the pool, with the tag recorded and never consulted.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// audienceOverPools builds the audience hook over a plural decl, recording
// every (resource, permission) pair its subject expansion was asked for.
func audienceOverPools(t *testing.T, refs []hooks.ToolReadResource, permitted []string, audience []string) (
	*hooks.InfoLeakAudience, *[][2]string,
) {
	t.Helper()
	asked := &[][2]string{}
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode: "enforcing",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{
				ResultResources: func(string) ([]hooks.ToolReadResource, error) { return refs, nil },
			}
		},
		ResolveAudience: fullAudience(audience),
		LookupSubjects: func(_ context.Context, res, perm string) ([]string, error) {
			*asked = append(*asked, [2]string{res, perm})
			return permitted, nil
		},
		BuildApprovalAsk: func(_ context.Context, _ []string, _ []infoleakagetaint.TaintRecord, _ string) (*pipeline.ApprovalAsk, error) {
			return &pipeline.ApprovalAsk{Kind: "leakage_share"}, nil
		},
	})
	return h, asked
}

// TestAudience_PoolReads_ExpandsEveryPoolUnderItsOwnPermission: the gate must
// see one record per pool. A pool missing here is a pool whose readership is
// never compared against the channel's.
func TestAudience_PoolReads_ExpandsEveryPoolUnderItsOwnPermission(t *testing.T) {
	// Everyone in the channel may view both pools, so the turn is clean and the
	// assertion is about WHAT WAS ASKED, not about the verdict.
	h, asked := audienceOverPools(t, twoPools(), []string{"dana"}, []string{"dana"})

	dec := h.Eval(memory.WithSystemApproval(context.Background(), "test"), poolReadCall(`{"entries":[]}`))

	assert.Equal(t, pipeline.Allow, dec.Verdict)
	require.Len(t, *asked, 2, "one subject expansion per pool")
	assert.Equal(t, [2]string{"customer:alpha", memory.PermissionViewMemory}, (*asked)[0],
		"a pool's readers are whoever holds view_memory on the resource")
	assert.Equal(t, [2]string{"vendor:beta", memory.PermissionViewMemory}, (*asked)[1])
}

// TestAudience_PoolReads_AsksApprovalWhenAChannelMemberCannotSeeThePool is the
// behaviour the expansion exists for: a result carrying pool entries into a
// channel containing someone outside the pool's audience is a leak, and is
// gated the same turn it happens.
func TestAudience_PoolReads_AsksApprovalWhenAChannelMemberCannotSeeThePool(t *testing.T) {
	h, _ := audienceOverPools(t, twoPools(), []string{"dana"}, []string{"dana", "outsider"})

	dec := h.Eval(memory.WithSystemApproval(context.Background(), "test"), poolReadCall(`{"entries":[]}`))

	require.NotNil(t, dec.Approval, "a channel member outside the pool's audience must not be handed the pool silently")
	assert.Equal(t, "leakage_share", dec.Approval.Kind)
}

// TestAudience_PoolReads_ASessionOnlyResultGatesNothing: no pool in the result
// means no pool audience to compare. The session's own entries are the coarse
// floor's business.
func TestAudience_PoolReads_ASessionOnlyResultGatesNothing(t *testing.T) {
	h, asked := audienceOverPools(t, nil, []string{"dana"}, []string{"dana", "outsider"})

	dec := h.Eval(memory.WithSystemApproval(context.Background(), "test"), poolReadCall(`{"entries":[{"id":"s1"}]}`))

	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.Nil(t, dec.Approval)
	assert.Empty(t, *asked, "nothing pool-scoped was read, so nothing was expanded")
}
