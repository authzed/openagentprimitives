package hooks_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// recordingApply scripts the prior scratch state (delta/approved/action/cleaned)
// and captures the Apply hook's apply / write / notice / audit effects.
type recordingApply struct {
	delta    scope.ScopeDelta
	cleaned  string
	approved bool
	action   string

	applies []scope.ScopeDelta
	writes  []coldstarttask.Content
	notices []string
	audits  []string
}

func (r *recordingApply) deps() hooks.MetaagentApplyDeps {
	return hooks.MetaagentApplyDeps{
		PeekExtract:  func() (scope.ScopeDelta, string, string) { return r.delta, "", r.cleaned },
		PeekApproved: func() (bool, string) { return r.approved, r.action },
		ApplyScope: func(_ context.Context, d scope.ScopeDelta) error {
			r.applies = append(r.applies, d)
			return nil
		},
		WriteTask: func(_ context.Context, c coldstarttask.Content) error {
			r.writes = append(r.writes, c)
			return nil
		},
		NotifyRequester: func(_ context.Context, body string) { r.notices = append(r.notices, body) },
		Audit:           func(_ context.Context, action string, _ *scope.ScopeDelta) { r.audits = append(r.audits, action) },
	}
}

func applyInput(kind string) pipeline.Input {
	return pipeline.Input{
		Point:     pipeline.MetaagentApply,
		Session:   pipeline.SessionRef{Namespace: "ns", Name: "a"},
		Requester: identity.CanonicalFromTrusted("user:alice", "test fixture"),
		Metaagent: &pipeline.MetaagentInfo{Kind: kind, Requester: "user:alice", InboxIdx: 0},
	}
}

// TestMetaagentApply_ColdStart_PerAction covers the four cold-start outcomes:
// the right cold_start_task status, and apply only on the two approve actions.
func TestMetaagentApply_ColdStart_PerAction(t *testing.T) {
	cases := []struct {
		name       string
		approved   bool
		action     string
		cleaned    string
		wantStatus string
		wantApply  bool
		wantClean  string
	}{
		{name: "approve_cleaned → apply + StatusApprovedCleaned + cleaned text", approved: true, action: "approve_cleaned", cleaned: "summarize L-140", wantStatus: coldstarttask.StatusApprovedCleaned, wantApply: true, wantClean: "summarize L-140"},
		{name: "approve_original → apply + StatusApprovedOriginal, no cleaned text", approved: true, action: "approve_original", cleaned: "summarize L-140", wantStatus: coldstarttask.StatusApprovedOriginal, wantApply: true, wantClean: ""},
		{name: "run_without_scope → no apply + StatusRanWithoutScope", approved: false, action: "run_without_scope", wantStatus: coldstarttask.StatusRanWithoutScope, wantApply: false},
		{name: "deny → no apply + StatusDenied", approved: false, action: "deny", wantStatus: coldstarttask.StatusDenied, wantApply: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingApply{
				delta:    scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}},
				cleaned:  tc.cleaned,
				approved: tc.approved,
				action:   tc.action,
			}
			h := hooks.NewMetaagentApply(rec.deps())
			dec := h.Eval(context.Background(), applyInput("cold_start"))
			assert.Equal(t, pipeline.Allow, dec.Verdict)
			require.Len(t, rec.writes, 1, "exactly one cold_start_task write")
			assert.Equal(t, tc.wantStatus, rec.writes[0].Status)
			if tc.wantApply {
				require.Len(t, rec.applies, 1, "approve applies scope")
				assert.Equal(t, tc.wantClean, rec.writes[0].CleanedText)
			} else {
				assert.Empty(t, rec.applies, "non-approve does not apply scope")
			}
			assert.NotEmpty(t, rec.notices, "the requester is notified of the outcome")
		})
	}
}

// TestMetaagentApply_MidSession_Approve_AppliesDelta verifies a mid-session
// approve applies the classified delta verbatim (no task write).
func TestMetaagentApply_MidSession_Approve_AppliesDelta(t *testing.T) {
	delta := scope.ScopeDelta{Add: scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}}}}
	rec := &recordingApply{delta: delta, approved: true}
	h := hooks.NewMetaagentApply(rec.deps())
	dec := h.Eval(context.Background(), applyInput("mid_session"))
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	require.Len(t, rec.applies, 1, "approve applies the widening")
	assert.Equal(t, delta, rec.applies[0])
	assert.Empty(t, rec.writes, "mid_session writes no cold_start_task")
	assert.NotEmpty(t, rec.notices)
}

// TestMetaagentApply_MidSession_Deny_ConvertsAddToHardDeny verifies a
// mid-session deny converts the Add into a sticky Layer-2 HardDeny (the
// deny→sticky-disallow rule preserved).
func TestMetaagentApply_MidSession_Deny_ConvertsAddToHardDeny(t *testing.T) {
	delta := scope.ScopeDelta{Add: scope.ScopePartial{
		Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}},
		Tools:     []string{"some.tool"},
	}}
	rec := &recordingApply{delta: delta, approved: false}
	h := hooks.NewMetaagentApply(rec.deps())
	dec := h.Eval(context.Background(), applyInput("mid_session"))
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	require.Len(t, rec.applies, 1, "deny applies the sticky HardDeny conversion")
	conv := rec.applies[0]
	assert.True(t, conv.Add.IsEmpty(), "the deny conversion drops the Add")
	assert.Equal(t, []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}}, conv.HardDeny.Resources,
		"the Add resources convert to sticky HardDeny")
	assert.Equal(t, []string{"some.tool"}, conv.HardDeny.Tools, "the Add tools convert to sticky HardDeny")
	assert.NotEmpty(t, rec.notices)
}

// TestMetaagentApply_MidSession_Deny_NarrowOnly_NoConversionApply verifies a
// deny on a narrow-only delta (no Add) applies nothing (nothing to make sticky)
// and notifies.
func TestMetaagentApply_MidSession_Deny_NarrowOnly_NoConversionApply(t *testing.T) {
	delta := scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}}
	rec := &recordingApply{delta: delta, approved: false}
	h := hooks.NewMetaagentApply(rec.deps())
	dec := h.Eval(context.Background(), applyInput("mid_session"))
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.Empty(t, rec.applies, "no Add to convert → nothing to apply on deny")
	assert.NotEmpty(t, rec.notices)
}

func TestMetaagentApply_Points(t *testing.T) {
	h := hooks.NewMetaagentApply(hooks.MetaagentApplyDeps{})
	assert.Equal(t, []pipeline.Point{pipeline.MetaagentApply}, h.Points())
	assert.Equal(t, "metaagent_apply", h.Name())
}

var _ pipeline.Hook = hooks.NewMetaagentApply(hooks.MetaagentApplyDeps{})
