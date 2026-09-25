package projectors

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestSourcesProjector(t *testing.T) {
	nsSrc := &spiceboxv1alpha1.SkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: "repo-a", Namespace: "ns1"},
		Spec:       spiceboxv1alpha1.SkillSourceSpec{RepoURL: "https://github.com/o/r", Ref: "v1.2.0"},
		Status: spiceboxv1alpha1.SkillSourceStatus{
			ResolvedSHA:      "abcdef1234567890",
			DiscoveredSkills: 4,
			Conditions:       []metav1.Condition{cond(spiceboxv1alpha1.SkillSourceConditionReady, metav1.ConditionTrue, "Synced")},
		},
	}
	// Synced once (carries a stale ResolvedSHA) then broke: Ready=False must
	// surface the failure reason, NOT mask it behind the persisted SHA.
	clusterSrc := &spiceboxv1alpha1.ClusterSkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: "repo-b"},
		Spec:       spiceboxv1alpha1.ClusterSkillSourceSpec{RepoURL: "https://github.com/o/b"},
		Status: spiceboxv1alpha1.SkillSourceStatus{
			ResolvedSHA: "deadbeef0000",
			Conditions:  []metav1.Condition{cond(spiceboxv1alpha1.SkillSourceConditionReady, metav1.ConditionFalse, "FetchFailed")},
		},
	}
	// No conditions stamped → Unknown, never healthy by default.
	unstamped := &spiceboxv1alpha1.ClusterSkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: "repo-c"},
		Spec:       spiceboxv1alpha1.ClusterSkillSourceSpec{RepoURL: "https://github.com/o/c"},
	}

	c := newClient(t, nsSrc, clusterSrc, unstamped)
	rows, err := sourcesProjector{}.List(context.Background(), c)
	require.NoError(t, err)
	require.Len(t, rows, 3)

	a := rowByName(t, rows, "repo-a", "skillsource")
	assert.Equal(t, "namespaced", a.Scope)
	assert.Equal(t, "Ready", a.Status)
	assert.Equal(t, "abcdef123456", a.StatusReason) // short SHA when healthy
	assert.Equal(t, 4, countVal(a, "discoveredSkills"))
	assert.Equal(t, "v1.2.0", badgeVal(a, "ref"))
	assert.Equal(t, "https://github.com/o/r", badgeVal(a, "repo"))

	b := rowByName(t, rows, "repo-b", "clusterskillsource")
	assert.Equal(t, "cluster", b.Scope)
	assert.Equal(t, "Degraded", b.Status)
	// Stale SHA must NOT mask the failure reason on a broken source.
	assert.Equal(t, "FetchFailed", b.StatusReason)

	u := rowByName(t, rows, "repo-c", "clusterskillsource")
	assert.Equal(t, "cluster", u.Scope)
	assert.Equal(t, "Unknown", u.Status)
}

// TestSourceDetail_CommitLinkAndSkillsSection proves the namespaced source
// detail (a) exposes a github commit deep-link for a resolved SHA — short SHA as
// the value label, full URL as the Href — and (b) lists only the Skills this
// source materialized, each linked to its own detail page.
func TestSourceDetail_CommitLinkAndSkillsSection(t *testing.T) {
	src := &spiceboxv1alpha1.SkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: "repo-a", Namespace: "ns1"},
		Spec:       spiceboxv1alpha1.SkillSourceSpec{RepoURL: "https://github.com/o/r", Ref: "v1"},
		Status: spiceboxv1alpha1.SkillSourceStatus{
			ResolvedSHA:      "abcdef1234567890",
			DiscoveredSkills: 1,
			LastSyncTime:     &metav1.Time{Time: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)},
			Conditions:       []metav1.Condition{cond(spiceboxv1alpha1.SkillSourceConditionReady, metav1.ConditionTrue, "Synced")},
		},
	}
	mine := &spiceboxv1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "skill-one", Namespace: "ns1"},
		Spec: spiceboxv1alpha1.SkillSpec{
			CanonicalName: "github.com/o/r//skills/one@v1",
			Description:   "does one thing",
			Source:        &spiceboxv1alpha1.SkillProvenance{SourceName: "repo-a", ResolvedSHA: "abcdef1234567890"},
		},
	}
	// Owned by a different source → must NOT appear in repo-a's Skills tab.
	other := &spiceboxv1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "skill-two", Namespace: "ns1"},
		Spec: spiceboxv1alpha1.SkillSpec{
			CanonicalName: "github.com/x/y//skills/two@v1",
			Description:   "unrelated",
			Source:        &spiceboxv1alpha1.SkillProvenance{SourceName: "repo-b"},
		},
	}
	c := newClient(t, src, mine, other)

	d, err := (sourcesDetailProjector{}).Detail(context.Background(), c, "ns1", "repo-a")
	require.NoError(t, err)
	require.NotNil(t, d)

	over := sectionByID(t, d, "overview")
	assert.Equal(t, "abcdef123456", fieldVal(over, "Commit"),
		"Commit value is the short SHA label, not the verbose URL")
	assert.Equal(t, "https://github.com/o/r/commit/abcdef1234567890", fieldHrefOf(over, "Commit"),
		"Commit Href carries the full github commit deep-link")

	skills := sectionByID(t, d, "skills")
	require.Len(t, skills.Items, 1, "only this source's skill is listed")
	assert.Equal(t, "skill-one", skills.Items[0].Title)
	require.NotNil(t, skills.Items[0].Link)
	assert.Equal(t, "skill", skills.Items[0].Link.Entity)
	assert.Equal(t, "ns1/skill-one", skills.Items[0].Link.ID, "namespaced skill link id is <ns>/<name>")
}

// TestSourceDetail_NonGithubRepoNoCommitField proves a non-github repo (or an
// unresolved SHA) drops the Commit field rather than building a bad link.
func TestSourceDetail_NonGithubRepoNoCommitField(t *testing.T) {
	src := &spiceboxv1alpha1.ClusterSkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: "repo-gl"},
		Spec:       spiceboxv1alpha1.ClusterSkillSourceSpec{RepoURL: "https://gitlab.com/o/r"},
		Status: spiceboxv1alpha1.SkillSourceStatus{
			ResolvedSHA: "abcdef1234567890",
			Conditions:  []metav1.Condition{cond(spiceboxv1alpha1.SkillSourceConditionReady, metav1.ConditionTrue, "Synced")},
		},
	}
	c := newClient(t, src)
	d, err := (sourcesDetailProjector{}).Detail(context.Background(), c, "", "repo-gl")
	require.NoError(t, err)
	require.NotNil(t, d)
	assert.Empty(t, fieldVal(sectionByID(t, d, "overview"), "Commit"),
		"non-github repo → no commit field")
}
