package hooks_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// TestPerDatumNeverMoreRestrictiveThanCoarse encodes the governing invariant as
// a property: for the SAME session state and destination, the per-datum decision
// is never MORE restrictive than the coarse floor, where ALLOW < APPROVAL < DENY.
// Per-datum may only relax (ask approval LESS), never tighten.
//
// The comparison is on the tool-call egress surface (where R15 + C1 live):
//   - COARSE  = untagged args → coarseToolCallFloor (session-wide taint floor)
//   - PERDATUM = tagged args reusing one datum → the per-datum reader set
//
// The session read TWO docs: dA readable by {tim,fred,sam} and dB by {tim}. The
// coarse floor is the INTERSECTION {tim}; the reused datum dA's readers
// {tim,fred,sam} are a superset — so per-datum can clear a destination the coarse
// floor would gate. That gap is the whole point, and one scenario proves it is
// STRICT (per-datum ALLOWs where coarse PROMPTS).
func TestPerDatumNeverMoreRestrictiveThanCoarse(t *testing.T) {
	rank := func(d pipeline.Decision) int {
		switch {
		case d.Verdict == pipeline.Deny:
			return 2
		case d.Approval != nil:
			return 1
		default:
			return 0
		}
	}

	taintList := func(context.Context) ([]infoleakagetaint.TaintRecord, error) {
		return []infoleakagetaint.TaintRecord{
			{ResourceType: "doc", ResourceID: "dA", Permission: "viewer"},
			{ResourceType: "doc", ResourceID: "dB", Permission: "viewer"},
		}, nil
	}
	lookupSubjects := func(_ context.Context, resource, _ string) ([]string, error) {
		switch resource {
		case "doc:dA":
			return []string{"user:tim", "user:fred", "user:sam"}, nil
		case "doc:dB":
			return []string{"user:tim"}, nil
		}
		return nil, nil
	}
	approvalBuilder := func(context.Context, []string, []infoleakagetaint.TaintRecord, string) (*pipeline.ApprovalAsk, error) {
		return &pipeline.ApprovalAsk{Kind: "leakage_share", Summary: "review"}, nil
	}
	dAReaders := map[string][]string{"ptt-dA": {"user:tim", "user:fred", "user:sam"}}
	taggedDA := toolenvelope.WrapPt("the dA figures", "noncedA", "ptt-dA")

	newHook := func(dest []string, tagged bool) pipeline.Decision {
		fg := fineGrainedDeps(true, dAReaders)
		fg.DestinationAudience = func(context.Context, string, map[string]any) ([]string, bool, error) {
			return dest, true, nil
		}
		h := audienceHook(t, hooks.InfoLeakAudienceDeps{
			FineGrained: fg, TaintList: taintList, LookupSubjects: lookupSubjects, BuildApprovalAsk: approvalBuilder,
		})
		args := `{"body":"untagged"}`
		if tagged {
			args = taggedDA
		}
		return h.Eval(context.Background(), preToolCall("post_to_board", args))
	}

	// Ranks are pinned ABSOLUTELY, not just as coarse≥perdatum. A relative-only
	// assertion would pass even if leakApproval regressed to a hard Deny, because
	// both branches share that helper and both ranks would rise together (the
	// ordering is preserved). Pinning coarse==APPROVAL on a leak means such a
	// revert (APPROVAL→DENY) fails here — the regression this file is named for.
	const (
		rAllow    = 0
		rApproval = 1
		rDeny     = 2
	)
	for _, sc := range []struct {
		name         string
		dest         []string
		wantCoarse   int
		wantPerdatum int
	}{
		{"dest in the coarse floor {tim}: both allow", []string{"user:tim"}, rAllow, rAllow},
		{"dest in dA readers but not the coarse floor: coarse prompts, per-datum allows", []string{"user:fred"}, rApproval, rAllow},
		{"dest outside dA readers: both prompt", []string{"user:sarah"}, rApproval, rApproval},
	} {
		t.Run(sc.name, func(t *testing.T) {
			coarse := rank(newHook(sc.dest, false))
			perdatum := rank(newHook(sc.dest, true))
			assert.Equal(t, sc.wantCoarse, coarse, "coarse rank (leak→APPROVAL, not DENY, is the point)")
			assert.Equal(t, sc.wantPerdatum, perdatum, "per-datum rank")
			assert.LessOrEqual(t, perdatum, coarse,
				"the invariant: per-datum is never MORE restrictive than coarse")
		})
	}
}
