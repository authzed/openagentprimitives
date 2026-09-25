package credential

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeBroker struct {
	got [][2]string
	err error
}

func (f *fakeBroker) InvalidateSecret(ns, name string) error {
	f.got = append(f.got, [2]string{ns, name})
	return f.err
}

func TestCredentialInvalidatorParsesKey(t *testing.T) {
	fb := &fakeBroker{}
	inv := New(fb)
	assert.Equal(t, "credential", inv.Kind())
	require.NoError(t, inv.Invalidate("id-ns/sec-1"))
	assert.Equal(t, [][2]string{{"id-ns", "sec-1"}}, fb.got)
}

func TestCredentialInvalidatorRejectsMalformedKey(t *testing.T) {
	assert.Error(t, New(&fakeBroker{}).Invalidate("no-slash"))
}

// The noun is substituted into chat copy a surface renders as trusted markup,
// so it has to be a phrase a person recognises — not the wire token Kind()
// returns, and not something a reader has to be an operator to parse.
func TestCredentialInvalidatorNamesWhatItWithdraws(t *testing.T) {
	noun := New(&fakeBroker{}).Noun()
	assert.Equal(t, "a connected account", noun)
	assert.NotContains(t, noun, "credential",
		"the wire kind is not the reader's word for the thing they connected")
}

// One credential revoke must reach every holder of that credential, not just
// the broker cache. In the runner the frozen (header, value) inside each MCPTool
// is a SECOND copy of the token that the broker cache knows nothing about;
// invalidating only the broker leaves that copy live. Fan-out is therefore the
// contract, not an optimisation.
func TestCredentialInvalidatorFansOutToEveryTarget(t *testing.T) {
	brokerCache, mcpTools := &fakeBroker{}, &fakeBroker{}

	require.NoError(t, New(brokerCache, mcpTools).Invalidate("id-ns/gh-pat"))

	assert.Equal(t, [][2]string{{"id-ns", "gh-pat"}}, brokerCache.got, "broker cache must be dropped")
	assert.Equal(t, [][2]string{{"id-ns", "gh-pat"}}, mcpTools.got, "in-memory MCPTool credentials must be invalidated too")
}

// A failing target must not short-circuit the others: a revocation that reaches
// only some holders of the credential is worse than one that reports failure.
func TestCredentialInvalidatorReportsFailureWithoutSkippingTargets(t *testing.T) {
	boom := errors.New("broker unreachable")
	failing, healthy := &fakeBroker{err: boom}, &fakeBroker{}

	err := New(failing, healthy).Invalidate("id-ns/gh-pat")

	require.Error(t, err, "a failing target must be surfaced, never swallowed")
	assert.ErrorIs(t, err, boom)
	assert.Equal(t, [][2]string{{"id-ns", "gh-pat"}}, healthy.got,
		"the healthy target must still have been invalidated despite the earlier failure")
}

// Zero targets is a programming error, not a silent no-op: it would make every
// credential revoke a lie.
func TestCredentialInvalidatorWithNoTargetsErrors(t *testing.T) {
	assert.Error(t, New().Invalidate("id-ns/gh-pat"))
}
