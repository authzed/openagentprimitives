package projectors

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// partialFailureSource is the shape the motivating failure produced: a pass
// that enumerated and visited all 156 scopes, wrote nothing, and failed every
// one of them. Ready is True/Synced — correctly, since per-scope failures are
// non-fatal — so before the PartialFailure condition the console had nothing
// to show an operator but "Ready" and "156 scopes".
func partialFailureSource() *spiceboxv1alpha1.RelationshipSource {
	return &spiceboxv1alpha1.RelationshipSource{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-forge", Namespace: "default"},
		Spec: spiceboxv1alpha1.RelationshipSourceSpec{
			Kind: "github",
			Auth: spiceboxv1alpha1.RelationshipSourceAuth{
				AgentIdentity: "forge-identity", Credential: "forge-pat",
			},
		},
		Status: spiceboxv1alpha1.RelationshipSourceStatus{
			Sync: spiceboxv1alpha1.RelationshipSourceSyncStatus{
				LastPass: &spiceboxv1alpha1.RelationshipSourcePassStats{
					ScopesProcessed: 156, ScopeErrors: 156,
					ScopeErrorSamples: []spiceboxv1alpha1.RelationshipSourceScopeError{
						{Scope: "repo-000", Message: "github: GET https://api.github.com/repos/demo-org/repo-000/teams: unexpected status 403"},
						{Scope: "repo-001", Message: "github: GET https://api.github.com/repos/demo-org/repo-001/teams: unexpected status 403"},
					},
				},
			},
			Conditions: []metav1.Condition{
				cond(spiceboxv1alpha1.RelationshipSourceConditionReady, metav1.ConditionTrue,
					spiceboxv1alpha1.ReasonRelationshipSourceSynced),
				{
					Type:               spiceboxv1alpha1.RelationshipSourceConditionPartialFailure,
					Status:             metav1.ConditionTrue,
					Reason:             spiceboxv1alpha1.ReasonRelationshipSourceScopeErrors,
					Message:            "156 scope(s) failed this pass; the pass was otherwise applied",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}
}

// directoryRows lists the directory slug against exactly src.
func directoryRows(t *testing.T, src *spiceboxv1alpha1.RelationshipSource) config.ResourceRow {
	t.Helper()
	p, ok := config.Get("directory")
	require.True(t, ok, "the directory slug must be registered")
	rows, err := p.List(context.Background(), newClient(t, src))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return rows[0]
}

// directoryDetail resolves the directory detail for src.
func directoryDetail(t *testing.T, src *spiceboxv1alpha1.RelationshipSource) *config.ResourceDetail {
	t.Helper()
	p, ok := config.GetDetail("directory")
	require.True(t, ok, "the directory detail projector must be registered")
	d, err := p.Detail(context.Background(), newClient(t, src), src.Namespace, src.Name)
	require.NoError(t, err)
	require.NotNil(t, d)
	return d
}

// The console half of the motivating failure: an operator looking at the
// directory list must not see a healthy source.
func TestDirectoryProjector_PartialFailureIsNotReportedHealthy(t *testing.T) {
	row := directoryRows(t, partialFailureSource())

	assert.Equal(t, "Degraded", row.Status,
		"Ready is True by design; the row must still not read as healthy when every scope failed")
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceScopeErrors, row.StatusReason)
	assert.Equal(t, 156, countVal(row, "errors"),
		"the error count is what distinguishes 156 scopes synced from 156 scopes failed")
	assert.Equal(t, 156, countVal(row, "scopes"),
		"precondition: the count that used to be the only thing this row showed")
}

// A healthy source is not downgraded. The PartialFailure condition is PRESENT
// and False on every clean pass, so a projector reading its presence rather
// than its status would mark every working directory in the cluster Degraded.
func TestDirectoryProjector_CleanPassStaysReady(t *testing.T) {
	src := partialFailureSource()
	src.Status.Sync.LastPass.ScopeErrors = 0
	src.Status.Sync.LastPass.ScopeErrorSamples = nil
	src.Status.Conditions[1].Status = metav1.ConditionFalse
	src.Status.Conditions[1].Reason = spiceboxv1alpha1.ReasonRelationshipSourceAllScopesSynced
	src.Status.Conditions[1].Message = ""

	row := directoryRows(t, src)
	assert.Equal(t, "Ready", row.Status, "PartialFailure=False is a healthy source")
	assert.Equal(t, 0, countVal(row, "errors"))
}

// A hard failure dominates: a source that cannot sync at all is the strictly
// worse state, and its reason — not the partial one — is what an operator has
// to act on.
func TestDirectoryProjector_HardFailureDominatesPartial(t *testing.T) {
	src := partialFailureSource()
	src.Status.Conditions[0] = cond(spiceboxv1alpha1.RelationshipSourceConditionReady,
		metav1.ConditionFalse, spiceboxv1alpha1.ReasonRelationshipSourceAuthResolveFailed)

	row := directoryRows(t, src)
	assert.Equal(t, "Degraded", row.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceAuthResolveFailed, row.StatusReason)
}

// A source that has never completed a pass has no verdict either way, and must
// read Unknown rather than borrowing a neighbouring condition's.
func TestDirectoryProjector_NoPassYetIsUnknownNotDegraded(t *testing.T) {
	row := directoryRows(t, &spiceboxv1alpha1.RelationshipSource{
		ObjectMeta: metav1.ObjectMeta{Name: "fresh-forge", Namespace: "default"},
		Spec:       spiceboxv1alpha1.RelationshipSourceSpec{Kind: "github"},
	})
	assert.Equal(t, "Unknown", row.Status)
	assert.Empty(t, row.Counts, "no pass has run, so there is nothing to count — errors included")
}

// The detail page: the degraded condition, the sampled errors, and — because
// only a sample is persisted — an explicit notice that the list is partial.
func TestDirectoryDetail_ShowsDegradedConditionAndSampledErrors(t *testing.T) {
	d := directoryDetail(t, partialFailureSource())

	assert.Equal(t, "Degraded", d.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceScopeErrors, d.StatusReason)

	sync := sectionByID(t, d, "sync")
	assert.Equal(t, "156", fieldVal(sync, "Scope errors"))
	assert.Equal(t, "156", fieldVal(sync, "Scopes processed"))

	errs := sectionByID(t, d, "scopeerrors")
	assert.Equal(t, config.SectionList, errs.Kind)
	require.Len(t, errs.Items, 2)
	assert.Equal(t, "repo-000", errs.Items[0].Title)
	assert.Contains(t, errs.Items[0].Subtitle, "unexpected status 403",
		"the error text is the whole point of a sample")
	assert.Contains(t, errs.Text, "156 scopes failed",
		"a capped sample presented as a complete list reads as 2 broken repos, not 156")

	// Ready is True, so the only message worth a Health tab is the partial
	// failure's — and before this feature there was no Health tab here at all.
	health := sectionByID(t, d, "health")
	assert.Equal(t, config.SectionText, health.Kind)
	assert.Contains(t, health.Text, "156 scope(s) failed this pass")
}

// A scope-less failure (relsync reports enumeration and reap-scan errors with
// no scope id at all — see relsync.ScopeError) must still be legible; a blank
// title reads as a scope whose name was lost.
func TestDirectoryDetail_LabelsScopelessErrors(t *testing.T) {
	src := partialFailureSource()
	src.Status.Sync.LastPass.ScopeErrors = 1
	src.Status.Sync.LastPass.ScopeErrorSamples = []spiceboxv1alpha1.RelationshipSourceScopeError{
		{Message: "relsync: enumeration returned zero scopes; refusing to reap"},
	}

	errs := sectionByID(t, directoryDetail(t, src), "scopeerrors")
	require.Len(t, errs.Items, 1)
	assert.Equal(t, "(source-level)", errs.Items[0].Title)
	assert.Empty(t, errs.Text, "one sampled error out of one is not a partial list")
}

// A clean source shows no Scope errors tab at all — an empty list rendered as
// a tab is a place to look that never has anything in it.
func TestDirectoryDetail_CleanPassHasNoScopeErrorsTab(t *testing.T) {
	src := partialFailureSource()
	src.Status.Sync.LastPass.ScopeErrors = 0
	src.Status.Sync.LastPass.ScopeErrorSamples = nil
	src.Status.Conditions[1].Status = metav1.ConditionFalse
	src.Status.Conditions[1].Reason = spiceboxv1alpha1.ReasonRelationshipSourceAllScopesSynced
	src.Status.Conditions[1].Message = ""

	d := directoryDetail(t, src)
	assert.False(t, hasSection(d, "scopeerrors"))
	assert.False(t, hasSection(d, "health"), "a healthy source has nothing to report")
	assert.Equal(t, "0", fieldVal(sectionByID(t, d, "sync"), "Scope errors"))
}

// A hard failure keeps the Health tab reporting the reason the sync stopped,
// never the stale partial-failure verdict left by the last pass that ran. Both
// branches emit id "health", so this also pins that only one is ever produced.
func TestDirectoryDetail_HardFailureHealthDominates(t *testing.T) {
	src := partialFailureSource()
	src.Status.Conditions[0] = metav1.Condition{
		Type:               spiceboxv1alpha1.RelationshipSourceConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             spiceboxv1alpha1.ReasonRelationshipSourceAuthResolveFailed,
		Message:            `resolve credential "forge-pat": secret not found`,
		LastTransitionTime: metav1.Now(),
	}

	d := directoryDetail(t, src)
	var healthCount int
	for _, s := range d.Sections {
		if s.ID == "health" {
			healthCount++
		}
	}
	assert.Equal(t, 1, healthCount, "two sections sharing one tab id is a rendering bug")
	assert.Contains(t, sectionByID(t, d, "health").Text, "secret not found")
}
