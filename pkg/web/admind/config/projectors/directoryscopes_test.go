package projectors

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// Unavailable and empty are different answers, same as
// directoryIdentitiesSection. A failed read must not render as "this sync
// wrote nothing" — the no-silent-errors failure mode this repo has already
// paid for once.
func TestDirectoryScopesSection_FailedReadSaysUnavailable(t *testing.T) {
	sec := directoryScopesSection(spicedb.SourceScopes{}, errors.New("spicedb unreachable"))

	assert.Equal(t, config.SectionText, sec.Kind, "a fault renders as text, not as an empty list")
	assert.Contains(t, sec.Text, "unavailable")
	assert.Contains(t, sec.Text, "spicedb unreachable", "the operator needs the reason, not just the fact")
}

// A successful read that found nothing renders nothing at all — appendSection
// drops an empty section, matching directoryIdentitiesSection's own contract.
func TestDirectoryScopesSection_EmptySucceedsSilently(t *testing.T) {
	sec := directoryScopesSection(spicedb.SourceScopes{}, nil)

	assert.Empty(t, sec.Items)
	assert.Empty(t, sec.Text, "a successful empty read must NOT claim to be unavailable")
	kept := appendSection(nil, sec)
	assert.Empty(t, kept, "appendSection must DROP a rowless, textless, fieldless section")
}

// Every scope id reads as its own row, named by definition, and a cap that
// truncated the list must say so — "showing N of M" — rather than silently
// presenting a short list as the whole answer.
func TestDirectoryScopesSection_ShowsRowsAndTruncationCount(t *testing.T) {
	sec := directoryScopesSection(spicedb.SourceScopes{
		Scopes: []spicedb.SourceScope{
			{Definition: "github_repo", ScopeIDs: []string{"acme/one", "acme/two"}, Total: 5, Source: "GitHub"},
			{Definition: "github_org", ScopeIDs: []string{"acme"}, Total: 1, Source: "GitHub"},
		},
	}, nil)

	require.Len(t, sec.Items, 4, "2 repo rows + 1 truncation row + 1 org row")
	assert.Equal(t, "github_repo:acme/one", sec.Items[0].Title)
	assert.Equal(t, "github_repo:acme/two", sec.Items[1].Title)
	assert.Contains(t, sec.Items[2].Title, "3", "5 total minus the 2 shown is 3 more")
	assert.Contains(t, sec.Items[2].Subtitle, "showing 2 of 5")
	assert.Equal(t, "github_org:acme", sec.Items[3].Title)
	assert.Empty(t, sec.Text, "a complete read carries no incompleteness notice")
}

// The subtitle and badge must name the SOURCE that wrote the scope, not
// repeat the definition a third time (title and badge already carry it
// otherwise) — mirrors how directoryIdentitiesSection's rows name their
// asserting source, using relsource.Source.DisplayName the same way.
func TestDirectoryScopesSection_NamesTheSourceNotTheDefinitionAgain(t *testing.T) {
	sec := directoryScopesSection(spicedb.SourceScopes{
		Scopes: []spicedb.SourceScope{
			{Definition: "github_repo", ScopeIDs: []string{"acme/one"}, Total: 1, Source: "GitHub"},
		},
	}, nil)

	require.Len(t, sec.Items, 1)
	item := sec.Items[0]
	assert.Equal(t, "github_repo:acme/one", item.Title, "title still carries the definition — that part is not redundant")
	assert.NotEqual(t, "github_repo", item.Subtitle, "subtitle must not just repeat the definition a third time")
	assert.Contains(t, item.Subtitle, "GitHub", "subtitle names the source that wrote this scope")
	require.Len(t, item.Badges, 1)
	assert.Equal(t, "source", item.Badges[0].Key, "the badge key is \"source\", matching directoryIdentitiesSection's own convention")
	assert.Equal(t, "GitHub", item.Badges[0].Value)
}

// A definition read in full (Total == len(ScopeIDs)) gets no truncation row —
// that row exists only to say a cap actually cut something off.
func TestDirectoryScopesSection_NoTruncationRowWhenNothingWasCut(t *testing.T) {
	sec := directoryScopesSection(spicedb.SourceScopes{
		Scopes: []spicedb.SourceScope{
			{Definition: "onepassword_group", ScopeIDs: []string{"g-1"}, Total: 1, Source: "1Password"},
		},
	}, nil)

	require.Len(t, sec.Items, 1)
	assert.Equal(t, "onepassword_group:g-1", sec.Items[0].Title)
}

// The third answer: some definitions read, some did not. Neither half may be
// dropped.
func TestDirectoryScopesSection_PartialReadKeepsRowsAndFlagsTheGap(t *testing.T) {
	sec := directoryScopesSection(spicedb.SourceScopes{
		Scopes: []spicedb.SourceScope{
			{Definition: "onepassword_group", ScopeIDs: []string{"g-1"}, Total: 1, Source: "1Password"},
		},
		Unavailable: []spicedb.UnavailableProbe{
			{Source: "GitHub", Definition: "github_org", Relation: "relhash", Err: "object definition `github_org` not found"},
		},
	}, nil)

	require.Len(t, sec.Items, 1, "the definition that WAS read must survive")
	assert.Equal(t, "onepassword_group:g-1", sec.Items[0].Title)
	assert.Contains(t, sec.Text, "INCOMPLETE", "a partially-read list must say so rather than look finished")
	assert.Contains(t, sec.Text, "github_org#relhash")
	assert.Contains(t, sec.Text, "GitHub", "and name the source, so an admin knows what to fix")
}

// The worst case: every probe failed. There are no rows, but the section must
// still carry the notice, and appendSection must keep it.
func TestDirectoryScopesSection_AllProbesUnavailableStillRendersTheNotice(t *testing.T) {
	sec := directoryScopesSection(spicedb.SourceScopes{
		Unavailable: []spicedb.UnavailableProbe{
			{Source: "GitHub", Definition: "github_org", Relation: "relhash", Err: "boom"},
		},
	}, nil)

	assert.Empty(t, sec.Items)
	assert.Contains(t, sec.Text, "INCOMPLETE")
	kept := appendSection(nil, sec)
	require.Len(t, kept, 1, "appendSection must KEEP a rowless section that carries a notice")
}

// The point of the whole panel change: a forge id is not a name. A resolved
// label becomes the row's title, and the raw `definition:id` moves DOWN into
// the subtitle rather than out of the page — an operator debugging SpiceDB
// needs the id they can query with, and a human needs the name.
func TestDirectoryScopesSection_ResolvedLabelTitlesTheRowAndKeepsTheRawID(t *testing.T) {
	sec := directoryScopesSection(spicedb.SourceScopes{
		Scopes: []spicedb.SourceScope{{
			Definition: "github_repo",
			ScopeIDs:   []string{"1005857813"},
			Total:      1,
			Source:     "GitHub",
			Labels: map[string]spicedb.ScopeLabel{
				"1005857813": {Title: "demo-org/widgets", Href: "https://github.com/demo-org/widgets"},
			},
		}},
	}, nil)

	require.Len(t, sec.Items, 1)
	item := sec.Items[0]
	assert.Equal(t, "demo-org/widgets", item.Title, "the human name titles the row")
	assert.Contains(t, item.Subtitle, "github_repo:1005857813",
		"the raw definition:id must stay visible — an operator queries SpiceDB with it")
	assert.Contains(t, item.Subtitle, "GitHub", "and the subtitle still names the source that wrote it")
	assert.Equal(t, "https://github.com/demo-org/widgets", item.Href)
	assert.Empty(t, sec.Text, "a fully resolved list carries no notice at all")
}

// A scope with no entry in Labels renders exactly as it did before labels
// existed. This is the Slack/1Password steady state, not a degradation: those
// kinds store no names, so a raw id is the honest answer and must not gain a
// warning.
func TestDirectoryScopesSection_UnlabelledScopeRendersExactlyAsBefore(t *testing.T) {
	sec := directoryScopesSection(spicedb.SourceScopes{
		Scopes: []spicedb.SourceScope{
			{Definition: "slack_channel", ScopeIDs: []string{"C0123"}, Total: 1, Source: "Slack"},
			// A source WITH a labeler, whose bridge simply had nothing for this
			// id — a repo whose bridge tuple has not been written yet.
			{
				Definition: "github_repo",
				ScopeIDs:   []string{"1005857813", "2000000000"},
				Total:      2,
				Source:     "GitHub",
				Labels:     map[string]spicedb.ScopeLabel{"2000000000": {Title: "demo-org/widgets"}},
			},
		},
	}, nil)

	require.Len(t, sec.Items, 3)
	assert.Equal(t, "slack_channel:C0123", sec.Items[0].Title)
	assert.Equal(t, "synced by Slack", sec.Items[0].Subtitle)
	assert.Empty(t, sec.Items[0].Href, "an unlabelled row is never linked")
	assert.Equal(t, "github_repo:1005857813", sec.Items[1].Title, "a miss falls back to the raw id")
	assert.Equal(t, "demo-org/widgets", sec.Items[2].Title)
	assert.Empty(t, sec.Items[2].Href, "a label with no href is a name, not a link")
	assert.Empty(t, sec.Text, "nothing FAILED here — a miss must not raise an alarm")
}

// The binding constraint, and the mutation this test exists to catch: when the
// bridge read FAILS, the rows must still render (with raw ids) AND the page
// must say the names could not be resolved. Dropping the notice leaves a page
// identical to a directory that legitimately has no labels — which is the
// silent-partial-failure mode this repo has already paid for once.
func TestDirectoryScopesSection_FailedLabelBridgeKeepsRawIDsAndSaysSo(t *testing.T) {
	sec := directoryScopesSection(spicedb.SourceScopes{
		Scopes: []spicedb.SourceScope{
			{Definition: "github_repo", ScopeIDs: []string{"1005857813"}, Total: 1, Source: "GitHub"},
		},
		LabelsUnavailable: []spicedb.UnavailableProbe{{
			Source: "GitHub", Definition: "github_repo_url", Relation: "repo",
			Err: "object definition `github_repo_url` not found",
		}},
	}, nil)

	require.Len(t, sec.Items, 1, "the rows are complete and must all render")
	assert.Equal(t, "github_repo:1005857813", sec.Items[0].Title, "a failed bridge degrades to the raw id")
	assert.Empty(t, sec.Items[0].Href)
	require.NotEmpty(t, sec.Text, "an unlabelled list that FAILED to resolve names must never be silent")
	assert.Contains(t, sec.Text, "raw ids", "the notice says what the reader is now looking at")
	assert.Contains(t, sec.Text, "github_repo_url#repo", "and names the probe, so an operator knows what to fix")
	assert.Contains(t, sec.Text, "object definition `github_repo_url` not found", "with the reason, not just the fact")
	assert.NotContains(t, sec.Text, "INCOMPLETE",
		"the LIST is complete — calling it incomplete is a false alarm about missing data")
}

// The two notices say different things and both must survive: rows really are
// missing (Unavailable) AND the rows that remain are unnamed
// (LabelsUnavailable). Collapsing either into the other loses a fact.
func TestDirectoryScopesSection_BothNoticesSurviveTogether(t *testing.T) {
	sec := directoryScopesSection(spicedb.SourceScopes{
		Scopes: []spicedb.SourceScope{
			{Definition: "github_repo", ScopeIDs: []string{"1005857813"}, Total: 1, Source: "GitHub"},
		},
		Unavailable: []spicedb.UnavailableProbe{
			{Source: "GitHub", Definition: "github_team", Relation: "relhash", Err: "boom"},
		},
		LabelsUnavailable: []spicedb.UnavailableProbe{
			{Source: "GitHub", Definition: "github_repo_url", Relation: "repo", Err: "bridge boom"},
		},
	}, nil)

	require.Len(t, sec.Items, 1)
	assert.Contains(t, sec.Text, "INCOMPLETE", "rows are genuinely missing and the page must say so")
	assert.Contains(t, sec.Text, "github_team#relhash")
	assert.Contains(t, sec.Text, "raw ids", "and the names could not be resolved either")
	assert.Contains(t, sec.Text, "github_repo_url#repo")
}

// A rowless section whose ONLY content is the label notice must still be kept
// by appendSection — the same rule the scope-probe notice already obeys.
func TestDirectoryScopesSection_LabelNoticeAloneKeepsTheSection(t *testing.T) {
	sec := directoryScopesSection(spicedb.SourceScopes{
		LabelsUnavailable: []spicedb.UnavailableProbe{
			{Source: "GitHub", Definition: "github_repo_url", Relation: "repo", Err: "boom"},
		},
	}, nil)

	assert.Empty(t, sec.Items)
	require.Len(t, appendSection(nil, sec), 1, "appendSection must KEEP a rowless section that carries a notice")
}

// A truncation row is a synthetic row about a DEFINITION, not about any one
// scope, so it must never pick up a label or a link from the scope map.
func TestDirectoryScopesSection_TruncationRowIsNeverLabelled(t *testing.T) {
	sec := directoryScopesSection(spicedb.SourceScopes{
		Scopes: []spicedb.SourceScope{{
			Definition: "github_repo",
			ScopeIDs:   []string{"1005857813"},
			Total:      4,
			Source:     "GitHub",
			Labels: map[string]spicedb.ScopeLabel{
				"1005857813": {Title: "demo-org/widgets", Href: "https://github.com/demo-org/widgets"},
			},
		}},
	}, nil)

	require.Len(t, sec.Items, 2)
	assert.Equal(t, "demo-org/widgets", sec.Items[0].Title)
	assert.Contains(t, sec.Items[1].Subtitle, "showing 1 of 4")
	assert.Empty(t, sec.Items[1].Href, "the +N more row names a definition, not a linkable resource")
}

func demoSource(kind string) *spiceboxv1alpha1.RelationshipSource {
	return &spiceboxv1alpha1.RelationshipSource{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-source", Namespace: "default"},
		Spec:       spiceboxv1alpha1.RelationshipSourceSpec{Kind: kind},
	}
}

// parkedSource is a RelationshipSource the relationshipsource controller has
// parked because another CR already claims its spec.kind (see
// controller.go's kind-claim gate) — Ready=False, reason KindClaimed. A
// parked source never binds a writer and never syncs a single tuple, so it
// has nothing of its own on the resource side either.
func parkedSource(kind string) *spiceboxv1alpha1.RelationshipSource {
	src := demoSource(kind)
	src.Status.Conditions = []metav1.Condition{
		cond(spiceboxv1alpha1.RelationshipSourceConditionReady, metav1.ConditionFalse,
			spiceboxv1alpha1.ReasonRelationshipSourceKindClaimed),
	}
	return src
}

// With no reader installed (the state of every build until admind.New wires
// one — see SetSourceScopeReader's doc), the directory detail page carries no
// Scopes tab at all rather than an empty or unavailable one.
func TestDirectoryDetail_ScopesReaderNilOmitsSection(t *testing.T) {
	SetSourceScopeReader(nil)
	c := newClient(t, demoSource("github"))

	d, err := (directoryDetailProjector{}).Detail(context.Background(), c, "default", "demo-source")
	require.NoError(t, err)
	require.NotNil(t, d)
	assert.False(t, hasSection(d, "directoryscopes"), "no reader installed → no tab")
}

// The reader is called with spec.kind verbatim — the kind → relsource.Source
// resolution is admind.New's job, not this projector's (mirrors
// SubjectIdentityReaderFunc's own split for the subject side).
func TestDirectoryDetail_ScopesReaderCalledWithSpecKindAndCap(t *testing.T) {
	var gotKind string
	var gotCap int
	SetSourceScopeReader(func(_ context.Context, kind string, capPerDefinition int) (spicedb.SourceScopes, error) {
		gotKind = kind
		gotCap = capPerDefinition
		return spicedb.SourceScopes{Scopes: []spicedb.SourceScope{
			{Definition: "github_org", ScopeIDs: []string{"acme"}, Total: 1},
		}}, nil
	})
	t.Cleanup(func() { SetSourceScopeReader(nil) })

	c := newClient(t, demoSource("github"))
	d, err := (directoryDetailProjector{}).Detail(context.Background(), c, "default", "demo-source")
	require.NoError(t, err)
	require.NotNil(t, d)

	assert.Equal(t, "github", gotKind, "the reader is called with spec.kind verbatim")
	assert.Positive(t, gotCap, "the projector must bound how many scope ids it asks for")
	sec := sectionByID(t, d, "directoryscopes")
	require.Len(t, sec.Items, 1)
	assert.Equal(t, "github_org:acme", sec.Items[0].Title)
}

// A failed read surfaces as the "unavailable: <reason>" text tab, not a
// silently empty or absent one.
func TestDirectoryDetail_ScopesReaderErrorRendersUnavailable(t *testing.T) {
	SetSourceScopeReader(func(_ context.Context, _ string, _ int) (spicedb.SourceScopes, error) {
		return spicedb.SourceScopes{}, errors.New("spicedb unreachable")
	})
	t.Cleanup(func() { SetSourceScopeReader(nil) })

	c := newClient(t, demoSource("slack"))
	d, err := (directoryDetailProjector{}).Detail(context.Background(), c, "default", "demo-source")
	require.NoError(t, err)
	require.NotNil(t, d)

	sec := sectionByID(t, d, "directoryscopes")
	assert.Equal(t, config.SectionText, sec.Kind)
	assert.Contains(t, sec.Text, "unavailable")
}

// A parked RelationshipSource (Ready=False/KindClaimed — see
// controller.go's kind-claim gate and newKindConflictFixtures) shares its
// spec.kind with whichever CR actually won incumbency, and the #relhash
// sentinel ListSourceScopes reads carries no source identity (its subject is
// a string digest, not a reference back to the writing CR) — so a reader
// keyed on kind alone cannot tell the two apart. Showing the incumbent's
// scopes on the PARKED CR's page would be a false positive claim ("this sync
// wrote these") for a source that never bound a writer at all. The Scopes
// tab must be absent, and the reader must never even be called — there is
// nothing this CR can truthfully claim credit for.
func TestDirectoryDetail_ScopesSectionAbsentWhenSourceIsParked(t *testing.T) {
	called := false
	SetSourceScopeReader(func(_ context.Context, _ string, _ int) (spicedb.SourceScopes, error) {
		called = true
		return spicedb.SourceScopes{Scopes: []spicedb.SourceScope{
			{Definition: "github_org", ScopeIDs: []string{"acme"}, Total: 1, Source: "GitHub"},
		}}, nil
	})
	t.Cleanup(func() { SetSourceScopeReader(nil) })

	c := newClient(t, parkedSource("github"))
	d, err := (directoryDetailProjector{}).Detail(context.Background(), c, "default", "demo-source")
	require.NoError(t, err)
	require.NotNil(t, d)

	assert.False(t, called, "a parked source must never even reach the injected reader")
	assert.False(t, hasSection(d, "directoryscopes"), "a parked source's page must show no Scopes tab at all")
}
