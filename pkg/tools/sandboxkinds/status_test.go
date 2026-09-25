package sandboxkinds_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

// The sentinel must survive wrapping: a backend adds context to say WHAT it is
// waiting on, and the controller still has to recognise the wait.
func TestErrPreconditionPending_SurvivesWrapping(t *testing.T) {
	wrapped := fmt.Errorf("waiting for claim %q: %w", "demo-workspace", sandboxkinds.ErrPreconditionPending)

	assert.True(t, errors.Is(wrapped, sandboxkinds.ErrPreconditionPending),
		"a wrapped precondition error must still be recognisable")
	assert.Contains(t, wrapped.Error(), "demo-workspace",
		"the wrapper must carry what is being waited on")
	assert.False(t, errors.Is(errors.New("some other failure"), sandboxkinds.ErrPreconditionPending))
}

// The standard reasons are a shared vocabulary, not a closed enum — a backend
// may report its own string where none of these fit. This asserts the set is
// enumerated so the list and the constants cannot drift apart.
func TestStandardReasons_AreEnumerated(t *testing.T) {
	got := sandboxkinds.StandardReasons()
	require.Len(t, got, 9)

	assert.ElementsMatch(t, []string{
		sandboxkinds.ReasonCreating,
		sandboxkinds.ReasonWaitingForPrereq,
		sandboxkinds.ReasonReady,
		sandboxkinds.ReasonNotReady,
		sandboxkinds.ReasonStartFailed,
		sandboxkinds.ReasonCrashed,
		sandboxkinds.ReasonOOMKilled,
		sandboxkinds.ReasonTerminating,
		sandboxkinds.ReasonGone,
	}, got)
}

func TestRuntimes_ForIsFailClosed(t *testing.T) {
	var rt sandboxkinds.Runtime // genuine nil interface, never assigned
	rs := sandboxkinds.Runtimes{"demo-kind": rt}

	_, ok := rs.For("demo-kind")
	assert.True(t, ok, "a registered kind must hit even when its runtime is nil")

	_, ok = rs.For("never-constructed")
	assert.False(t, ok, "an unknown kind must miss, never fall back")

	_, ok = rs.For("")
	assert.False(t, ok, "an empty kind must miss, never fall back")

	var empty sandboxkinds.Runtimes
	_, ok = empty.For("demo-kind")
	assert.False(t, ok, "a nil Runtimes must miss rather than panic")
}
