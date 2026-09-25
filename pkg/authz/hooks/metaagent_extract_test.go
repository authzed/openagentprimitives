package hooks_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// recordingExtract captures the MetaagentExtract hook's side effects (scratch
// write, task write, notice, audit) and scripts the extractor.
type recordingExtract struct {
	ext    scope.ColdStartExtraction
	extErr error

	setDelta   scope.ScopeDelta
	setShape   string
	setCleaned string
	setCalled  bool

	writes  []coldstarttask.Content
	notices []string
	audits  []string
}

func (r *recordingExtract) deps() hooks.MetaagentExtractDeps {
	return hooks.MetaagentExtractDeps{
		Envelope: scope.AgentClassEnvelope{
			BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "linear_issue"}, {ResourceType: "github_repo"}},
			Tools:         []scope.EnvelopeTool{{Name: "linear.search_issues"}},
		},
		RequesterPerms: scope.RequesterPerms{AllowedIDsByType: map[string]map[string]bool{
			"github_repo": {"foo/bar": true},
		}},
		Extract: func(context.Context, string) (scope.ColdStartExtraction, error) {
			return r.ext, r.extErr
		},
		SetExtract: func(d scope.ScopeDelta, shape, cleaned string) {
			r.setDelta, r.setShape, r.setCleaned, r.setCalled = d, shape, cleaned, true
		},
		WriteTask: func(_ context.Context, c coldstarttask.Content) error {
			r.writes = append(r.writes, c)
			return nil
		},
		NotifyRequester: func(_ context.Context, body string) { r.notices = append(r.notices, body) },
		Audit:           func(_ context.Context, action string, _ *scope.ScopeDelta) { r.audits = append(r.audits, action) },
	}
}

func extractInput(kind string) pipeline.Input {
	return pipeline.Input{
		Point:     pipeline.MetaagentExtract,
		Session:   pipeline.SessionRef{Namespace: "ns", Name: "a"},
		Requester: identity.CanonicalFromTrusted("user:alice", "test fixture"),
		Metaagent: &pipeline.MetaagentInfo{Kind: kind, Requester: "user:alice", Text: "do thing", InboxIdx: 0},
	}
}

// TestMetaagentExtract_HardDeny_WritesNarrowShape verifies a HardDeny delta is
// classified, the narrow shape derived, and both written to scratch (mid_session).
func TestMetaagentExtract_HardDeny_WritesNarrowShape(t *testing.T) {
	rec := &recordingExtract{ext: scope.ColdStartExtraction{
		ScopeDelta: scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}},
	}}
	h := hooks.NewMetaagentExtract(rec.deps())
	dec := h.Eval(context.Background(), extractInput("mid_session"))
	assert.Equal(t, pipeline.Allow, dec.Verdict, "non-empty delta → Allow (proceed to Decide)")
	require.True(t, rec.setCalled, "Extract must write the classified delta to scratch")
	assert.Equal(t, []string{"linear.search_issues"}, rec.setDelta.HardDeny.Tools)
	assert.Equal(t, "narrow", rec.setShape)
	assert.Empty(t, rec.writes, "mid_session does not write cold_start_task")
}

// TestMetaagentExtract_AddWiden_DerivesWidenShape verifies an Add delta the
// requester can access derives the widen shape and is kept by ClassifySkipped
// (cold_start).
func TestMetaagentExtract_AddWiden_DerivesWidenShape(t *testing.T) {
	rec := &recordingExtract{ext: scope.ColdStartExtraction{
		ScopeDelta:  scope.ScopeDelta{Add: scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}}}},
		CleanedTask: "summarize foo/bar",
	}}
	h := hooks.NewMetaagentExtract(rec.deps())
	dec := h.Eval(context.Background(), extractInput("cold_start"))
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	require.True(t, rec.setCalled)
	assert.Equal(t, "widen", rec.setShape)
	require.Len(t, rec.setDelta.Add.Resources, 1, "in-perm Add kept by ClassifySkipped")
	assert.Equal(t, "summarize foo/bar", rec.setCleaned, "cleaned task threaded to scratch (cold_start)")
}

// TestMetaagentExtract_ExtractorError_ColdStart_FailsClosed verifies an
// extractor error on cold_start halts and writes StatusScopeReviewFailed (the
// runner halts the session rather than running unscoped).
func TestMetaagentExtract_ExtractorError_ColdStart_FailsClosed(t *testing.T) {
	rec := &recordingExtract{extErr: errors.New("llm down")}
	h := hooks.NewMetaagentExtract(rec.deps())
	dec := h.Eval(context.Background(), extractInput("cold_start"))
	assert.Equal(t, pipeline.Halt, dec.Verdict, "extractor error fails closed (Halt)")
	require.NotEmpty(t, dec.Notices, "a requester notice is delivered on fail-closed")
	require.Len(t, rec.writes, 1)
	assert.Equal(t, coldstarttask.StatusScopeReviewFailed, rec.writes[0].Status)
}

// TestMetaagentExtract_ExtractorError_MidSession_HaltsNotifies verifies an
// extractor error on mid_session halts + notifies, but writes NO cold_start_task
// (there is no session to halt via task — the change is simply aborted).
func TestMetaagentExtract_ExtractorError_MidSession_HaltsNotifies(t *testing.T) {
	rec := &recordingExtract{extErr: errors.New("llm down")}
	h := hooks.NewMetaagentExtract(rec.deps())
	dec := h.Eval(context.Background(), extractInput("mid_session"))
	assert.Equal(t, pipeline.Halt, dec.Verdict, "extractor error aborts the change (Halt)")
	assert.NotEmpty(t, rec.notices, "the requester is notified the change was aborted")
	assert.Empty(t, rec.writes, "mid_session writes no cold_start_task")
}

// TestMetaagentExtract_EmptyDelta_ColdStart_WritesCleaned verifies the no-op
// fast path: an empty delta with a cleaned task short-circuits the sequence
// (non-Allow) after writing StatusApprovedCleaned — no approval needed.
func TestMetaagentExtract_EmptyDelta_ColdStart_WritesCleaned(t *testing.T) {
	rec := &recordingExtract{ext: scope.ColdStartExtraction{CleanedTask: "summarize L-140"}}
	h := hooks.NewMetaagentExtract(rec.deps())
	dec := h.Eval(context.Background(), extractInput("cold_start"))
	assert.NotEqual(t, pipeline.Allow, dec.Verdict, "no-op fast path short-circuits (no Decide/Apply)")
	require.Len(t, rec.writes, 1)
	assert.Equal(t, coldstarttask.StatusApprovedCleaned, rec.writes[0].Status)
	assert.Equal(t, "summarize L-140", rec.writes[0].CleanedText)
}

// TestMetaagentExtract_EmptyDelta_ColdStart_NoTask_WritesOriginal verifies the
// no-op path with no cleaned task writes StatusApprovedOriginal.
func TestMetaagentExtract_EmptyDelta_ColdStart_NoTask_WritesOriginal(t *testing.T) {
	rec := &recordingExtract{ext: scope.ColdStartExtraction{}}
	h := hooks.NewMetaagentExtract(rec.deps())
	dec := h.Eval(context.Background(), extractInput("cold_start"))
	assert.NotEqual(t, pipeline.Allow, dec.Verdict)
	require.Len(t, rec.writes, 1)
	assert.Equal(t, coldstarttask.StatusApprovedOriginal, rec.writes[0].Status)
}

// TestMetaagentExtract_EmptyDelta_MidSession_NotifiesAborts verifies the
// mid-session empty/cannot-address path short-circuits + notifies, no task write.
func TestMetaagentExtract_EmptyDelta_MidSession_NotifiesAborts(t *testing.T) {
	rec := &recordingExtract{ext: scope.ColdStartExtraction{}}
	h := hooks.NewMetaagentExtract(rec.deps())
	dec := h.Eval(context.Background(), extractInput("mid_session"))
	assert.NotEqual(t, pipeline.Allow, dec.Verdict, "nothing applicable → short-circuit")
	assert.NotEmpty(t, rec.notices, "requester is told the request could not be applied")
	assert.Empty(t, rec.writes)
}

// TestMetaagentExtract_AddDroppedByClassify_MidSession_Aborts verifies that an
// Add the requester CANNOT access is dropped by ClassifySkipped → applied empty
// → mid_session aborts (CannotAddress), no task write.
func TestMetaagentExtract_AddDroppedByClassify_MidSession_Aborts(t *testing.T) {
	rec := &recordingExtract{ext: scope.ColdStartExtraction{
		ScopeDelta: scope.ScopeDelta{Add: scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "secret/repo"}}}},
	}}
	h := hooks.NewMetaagentExtract(rec.deps())
	dec := h.Eval(context.Background(), extractInput("mid_session"))
	assert.NotEqual(t, pipeline.Allow, dec.Verdict, "Add for inaccessible resource → applied empty → abort")
	assert.NotEmpty(t, rec.notices)
}

func TestMetaagentExtract_Points(t *testing.T) {
	h := hooks.NewMetaagentExtract(hooks.MetaagentExtractDeps{})
	assert.Equal(t, []pipeline.Point{pipeline.MetaagentExtract}, h.Points())
	assert.Equal(t, "metaagent_extract", h.Name())
}

var _ pipeline.Hook = hooks.NewMetaagentExtract(hooks.MetaagentExtractDeps{})
