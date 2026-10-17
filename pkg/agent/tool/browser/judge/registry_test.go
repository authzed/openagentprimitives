package judge_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/browser/judge"
)

// stubJudge is a minimal registrable ConsequenceJudge for registry tests.
type stubJudge struct{ name string }

func (s stubJudge) Name() string { return s.name }
func (s stubJudge) Classify(context.Context, judge.Action, judge.PageContext) (judge.Verdict, error) {
	return judge.Verdict{Tier: judge.TierObserve, Probs: map[judge.Tier]float64{judge.TierObserve: 1.0}}, nil
}

func TestRegisterAndGet(t *testing.T) {
	judge.Reset()
	t.Cleanup(judge.Reset)

	judge.Register(stubJudge{name: "stub"})

	got, ok := judge.Get("stub")
	require.True(t, ok, "a registered judge must be found")
	assert.Equal(t, "stub", got.Name())
}

func TestGetUnknownIsNotFound(t *testing.T) {
	judge.Reset()
	t.Cleanup(judge.Reset)

	got, ok := judge.Get("nope")
	assert.False(t, ok, "an unregistered name must not be found")
	assert.Nil(t, got, "a not-found lookup must return a nil interface, not a typed nil")
}

func TestRegisterDuplicatePanics(t *testing.T) {
	judge.Reset()
	t.Cleanup(judge.Reset)

	judge.Register(stubJudge{name: "dup"})
	assert.Panics(t, func() {
		judge.Register(stubJudge{name: "dup"})
	}, "a duplicate registration must panic")
}

func TestRegisterEmptyNamePanics(t *testing.T) {
	judge.Reset()
	t.Cleanup(judge.Reset)

	assert.Panics(t, func() {
		judge.Register(stubJudge{name: ""})
	}, "an empty-named registration must panic")
}

func TestAllReturnsSortedByName(t *testing.T) {
	judge.Reset()
	t.Cleanup(judge.Reset)

	judge.Register(stubJudge{name: "zeta"})
	judge.Register(stubJudge{name: "alpha"})

	all := judge.All()
	require.Len(t, all, 2)
	assert.Equal(t, "alpha", all[0].Name())
	assert.Equal(t, "zeta", all[1].Name())
}
