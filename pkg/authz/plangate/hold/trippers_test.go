package hold_test

// Setup holds exactly one Tripper, and there is more than one reason to freeze
// a session. The denial streak asks "is this session repeatedly attempting what
// it may not do"; the trifecta closure asks "has this delegation acquired a
// dangerous combination". Neither implies the other.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/plangate/hold"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

type recordingTripper struct {
	name  string
	calls *int
	err   error
}

func (r recordingTripper) Name() string { return r.name }
func (r recordingTripper) OnSignal(context.Context, memory.Signal) error {
	*r.calls++
	return r.err
}

// TestONEFailingTripperDoesNotDisableTheOthers is the whole reason this exists
// rather than a loop at the call site.
//
// These are containment controls. If the streak tripper cannot read memory,
// that is a reason to log and keep going — not a reason the trifecta judgement
// silently stops running for every session in the cluster.
func TestONEFailingTripperDoesNotDisableTheOthers(t *testing.T) {
	var firstCalls, secondCalls int
	boom := errors.New("memory unavailable")

	c := hold.Trippers(
		recordingTripper{name: "streak", calls: &firstCalls, err: boom},
		recordingTripper{name: "closure", calls: &secondCalls},
	)
	err := c.OnSignal(context.Background(), memory.Signal{})

	assert.Equal(t, 1, firstCalls)
	assert.Equal(t, 1, secondCalls,
		"the second judgement must run even though the first failed")
	require.Error(t, err, "and the failure must still surface — silently swallowing it would hide a broken control")
	assert.ErrorIs(t, err, boom)
}

// TestEveryFailureIsReportedNotJustTheFirst: two broken trippers is a worse
// state than one, and an operator reading the log should learn both.
func TestEveryFailureIsReportedNotJustTheFirst(t *testing.T) {
	var a, b int
	first, second := errors.New("first down"), errors.New("second down")

	err := hold.Trippers(
		recordingTripper{name: "a", calls: &a, err: first},
		recordingTripper{name: "b", calls: &b, err: second},
	).OnSignal(context.Background(), memory.Signal{})

	require.Error(t, err)
	assert.ErrorIs(t, err, first)
	assert.ErrorIs(t, err, second)
}

// TestNilElementsAreSkippedNotPanicked.
//
// The operator declares trippers as interface values that stay nil when their
// feature is off — the threshold-zero case, and the --trifecta-closure-tripper
// default. Requiring every call site to pre-filter would spread the typed-nil
// hazard rather than containing it here.
func TestNilElementsAreSkippedNotPanicked(t *testing.T) {
	var calls int
	c := hold.Trippers(nil, recordingTripper{name: "live", calls: &calls}, nil)

	require.NotNil(t, c)
	require.NoError(t, c.OnSignal(context.Background(), memory.Signal{}))
	assert.Equal(t, 1, calls)
}

// TestNoLiveTrippersIsNILNotAnEmptyComposite.
//
// Setup(nil) is the documented "no tripper" state and NewScopeHooks returns a
// no-op for it. An empty composite would be a NON-NIL interface that does
// nothing — which reads as wired in every log and diagnostic, and is exactly
// the "installed but cannot fire" shape this track keeps finding.
func TestNoLiveTrippersIsNILNotAnEmptyComposite(t *testing.T) {
	assert.Nil(t, hold.Trippers())
	assert.Nil(t, hold.Trippers(nil, nil))
}

// TestASingleTripperIsReturnedUnwrapped, so the common case keeps its own
// Name() in logs rather than a composite's.
func TestASingleTripperIsReturnedUnwrapped(t *testing.T) {
	var calls int
	only := recordingTripper{name: "streak", calls: &calls}

	assert.Equal(t, "streak", hold.Trippers(nil, only).Name())
}

// TestTheCompositeNamesEveryMemberSoALogSaysWhatIsWired.
func TestTheCompositeNamesEveryMemberSoALogSaysWhatIsWired(t *testing.T) {
	var a, b int
	c := hold.Trippers(
		recordingTripper{name: "denial-streak", calls: &a},
		recordingTripper{name: "trifecta-closure", calls: &b},
	)
	assert.Equal(t, "denial-streak+trifecta-closure", c.Name())
}
