package observe_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/observe"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
)

// prResult is the shape `gh pr view <n> --json number,headRefOid,headRepository,isCrossRepository`
// returns, as the dispatcher hands it to CEL.
func prResult(number int, oid string, cross bool) map[string]any {
	return map[string]any{
		"number":            number,
		"headRefOid":        oid,
		"isCrossRepository": cross,
		"headRepository":    map[string]any{"nameWithOwner": "demo-org/demo-repo"},
	}
}

func twoSubjectBlock() observe.Block {
	return observe.Block{
		ForEach: "[result]",
		Subjects: []observe.SubjectExpr{
			{ResourceType: `"git_commit"`, ResourceID: `item.headRefOid`},
			{ResourceType: `"github_pr"`, ResourceID: `item.headRepository.nameWithOwner + "#" + string(item.number)`},
		},
		Facts: map[string]string{"is_cross_repository": `item.isCrossRepository`},
	}
}

func TestEvaluateCoDerivesSubjectsAndFactsFromOneItem(t *testing.T) {
	got, err := observe.Evaluate(twoSubjectBlock(), map[string]any{
		"result": prResult(6, "ba03f5969a", false),
	})
	require.NoError(t, err)
	require.Len(t, got, 1, "one item yields one observation")

	assert.ElementsMatch(t, []factcontent.Subject{
		{ResourceType: "git_commit", ResourceID: "ba03f5969a"},
		{ResourceType: "github_pr", ResourceID: "demo-org/demo-repo#6"},
	}, got[0].Subjects)
	assert.Equal(t, map[string]any{"is_cross_repository": false}, got[0].Facts)
	assert.NotEmpty(t, got[0].ObservationID, "the group handle is minted per observation")
}

func TestEvaluateGroupsEachItemSeparately(t *testing.T) {
	b := twoSubjectBlock()
	b.ForEach = "result.items"
	got, err := observe.Evaluate(b, map[string]any{
		"result": map[string]any{"items": []any{
			prResult(5, "aaaa1111", false),
			prResult(6, "bbbb2222", true),
		}},
	})
	require.NoError(t, err)
	require.Len(t, got, 2)

	// Each item's facts stay with ITS OWN subjects. If these ever cross, the
	// laundering attack works: #5's `false` would attach to #6.
	assert.NotEqual(t, got[0].ObservationID, got[1].ObservationID)

	// The EXACT subject pair each fact value is allowed to arrive with. The
	// assertion has to be over the whole set, not over some member of it: an
	// allocate-once refactor that hoisted the Observation (or just its
	// subject slice) out of Evaluate's per-item loop would hand PR #5's
	// observation PR #6's subjects too, and a check asking only "is #6 in
	// here" would call that a pass. That accumulated observation IS the
	// laundering the design forbids — #6 carrying a fact derived from #5 —
	// so what must be pinned is that an observation carries its own item's
	// two subjects and NOTHING else.
	wantSubjects := map[bool][]factcontent.Subject{
		false: {
			{ResourceType: "git_commit", ResourceID: "aaaa1111"},
			{ResourceType: "github_pr", ResourceID: "demo-org/demo-repo#5"},
		},
		true: {
			{ResourceType: "git_commit", ResourceID: "bbbb2222"},
			{ResourceType: "github_pr", ResourceID: "demo-org/demo-repo#6"},
		},
	}
	seen := map[bool]int{}
	for i, o := range got {
		cross, ok := o.Facts["is_cross_repository"].(bool)
		require.True(t, ok, "observation %d must carry a bool is_cross_repository fact, got %#v",
			i, o.Facts["is_cross_repository"])
		require.Len(t, o.Subjects, 2,
			"observation %d must carry exactly its own item's two subjects, got %#v", i, o.Subjects)
		assert.ElementsMatch(t, wantSubjects[cross], o.Subjects,
			"observation %d's fact value %v must be paired with that item's subjects and no others", i, cross)
		seen[cross]++
	}
	// One observation per item, each with its own fact value. A shared facts
	// map across iterations would report the last item's value for both, and
	// this is what names that failure rather than leaving it to a subject
	// mismatch elsewhere.
	assert.Equal(t, map[bool]int{false: 1, true: 1}, seen,
		"each item must yield exactly one observation carrying its own fact value")
}

func TestEvaluateSkipsOnFalseWhen(t *testing.T) {
	b := twoSubjectBlock()
	b.When = "false"
	got, err := observe.Evaluate(b, map[string]any{"result": prResult(6, "ba03f5969a", false)})
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestEvaluateErrorsOnAnEmptySubjectID(t *testing.T) {
	// A subject expression that resolves to "" would file a fact under an
	// object nobody can name. Fail closed rather than record it.
	b := twoSubjectBlock()
	b.Subjects[0].ResourceID = `""`
	_, err := observe.Evaluate(b, map[string]any{"result": prResult(6, "ba03f5969a", false)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestEvaluateErrorsOnAnEmptySubjectType(t *testing.T) {
	// Same fail-closed rule as ResourceID, exercised on the OTHER half of the
	// pair: the whole guarantee rests on EvalStringWithItem's shared
	// non-empty-string enforcement rather than a bespoke check in this
	// package, so both resourceType and resourceID need their own pin —
	// covering only ResourceID would leave a switch of subjectTypePrgs to a
	// non-enforcing helper undetected.
	b := twoSubjectBlock()
	b.Subjects[0].ResourceType = `""`
	_, err := observe.Evaluate(b, map[string]any{"result": prResult(6, "ba03f5969a", false)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}
