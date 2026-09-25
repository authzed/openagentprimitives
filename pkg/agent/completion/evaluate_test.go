package completion_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/completion"
)

// fakeRequirement is a registered kind whose verdict the test dictates.
type fakeRequirement struct {
	key     string
	title   string
	finding completion.Finding
	err     error
	calls   *int
}

func (f fakeRequirement) Key() string   { return f.key }
func (f fakeRequirement) Title() string { return f.title }
func (f fakeRequirement) Check(context.Context, completion.Input) (completion.Finding, error) {
	if f.calls != nil {
		*f.calls++
	}
	return f.finding, f.err
}

// withRegistry swaps in a clean registry for the duration of one test.
func withRegistry(t *testing.T, reqs ...completion.Requirement) {
	t.Helper()
	snap := completion.SnapshotForTest()
	t.Cleanup(func() { completion.RestoreForTest(snap) })
	completion.RestoreForTest(nil)
	for _, r := range reqs {
		completion.Register(r)
	}
}

func TestEvaluate_UndeclaredRequirementNeverRuns(t *testing.T) {
	var calls int
	withRegistry(t, fakeRequirement{
		key: "never-me", title: "never me",
		finding: completion.Finding{Missing: "would have fired"},
		calls:   &calls,
	})

	// A class that declared nothing.
	unmet, err := completion.Evaluate(context.Background(), nil, completion.Input{})
	require.NoError(t, err, "an empty declaration must not be an error")
	assert.Empty(t, unmet, "a registered-but-undeclared requirement must not fire")
	assert.Zero(t, calls, "Check must not even be called for an undeclared requirement")
}

func TestEvaluate_DeclaredAndUnmetIsReported(t *testing.T) {
	withRegistry(t,
		fakeRequirement{key: "a", title: "the A obligation", finding: completion.Finding{Missing: "A is missing"}},
		fakeRequirement{key: "b", title: "the B obligation", finding: completion.Finding{Met: true}},
	)

	unmet, err := completion.Evaluate(context.Background(), []string{"a", "b"}, completion.Input{})
	require.NoError(t, err)
	require.Len(t, unmet, 1, "only the unmet requirement is reported")
	assert.Equal(t, "a", unmet[0].Key)
	assert.Equal(t, "the A obligation", unmet[0].Title, "Title travels with the finding for the human-facing record")
	assert.Equal(t, "A is missing", unmet[0].Missing)
}

func TestEvaluate_ReportsInDeclarationOrder(t *testing.T) {
	withRegistry(t,
		fakeRequirement{key: "a", finding: completion.Finding{Missing: "A"}},
		fakeRequirement{key: "b", finding: completion.Finding{Missing: "B"}},
	)

	unmet, err := completion.Evaluate(context.Background(), []string{"b", "a"}, completion.Input{})
	require.NoError(t, err)
	require.Len(t, unmet, 2)
	assert.Equal(t, []string{"b", "a"}, []string{unmet[0].Key, unmet[1].Key},
		"the operator's declaration order is the order a refusal reads in")
}

func TestEvaluate_UnknownKeyFailsClosedAndNamesTheRegisteredSet(t *testing.T) {
	withRegistry(t, fakeRequirement{key: "known", finding: completion.Finding{Met: true}})

	_, err := completion.Evaluate(context.Background(), []string{"typo-here"}, completion.Input{})
	require.Error(t, err, "an unknown key must not be silently treated as satisfied")
	assert.Contains(t, err.Error(), "typo-here", "the error must name the key the operator got wrong")
	assert.Contains(t, err.Error(), "known", "the error must name what IS registered so it is actionable")
}

func TestEvaluate_CheckErrorFailsClosed(t *testing.T) {
	withRegistry(t, fakeRequirement{key: "a", err: errors.New("no client on this session")})

	_, err := completion.Evaluate(context.Background(), []string{"a"}, completion.Input{})
	require.Error(t, err, "a requirement that cannot answer must not read as satisfied")
	assert.Contains(t, err.Error(), "no client on this session")
}

func TestRegister_DuplicateKeyPanics(t *testing.T) {
	withRegistry(t, fakeRequirement{key: "dup"})
	assert.Panics(t, func() { completion.Register(fakeRequirement{key: "dup"}) },
		"a duplicate key must be loud at init, not a silent last-wins overwrite")
}
