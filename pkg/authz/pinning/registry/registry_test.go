package registry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
)

type fakeKind struct{ name string }

func (f *fakeKind) Name() string { return f.name }
func (f *fakeKind) ParseRef(spec string) (pinning.Ref, error) {
	return pinning.Ref{Kind: f.name, Spec: spec, Strength: pinning.StrengthUnpinned}, nil
}
func (f *fakeKind) Resolve(context.Context, pinning.Ref) (pinning.Frozen, error) {
	return pinning.Frozen{}, nil
}
func (f *fakeKind) Verify(context.Context, pinning.Ref, pinning.Frozen) (pinning.DriftReport, error) {
	return pinning.DriftReport{}, nil
}

func TestRegisterGetAndAll(t *testing.T) {
	t.Cleanup(Reset)
	Register(&fakeKind{name: "beta"})
	Register(&fakeKind{name: "alpha"})

	k, ok := Get("alpha")
	require.True(t, ok, "registered kind must be retrievable")
	assert.Equal(t, "alpha", k.Name())

	_, ok = Get("nope")
	assert.False(t, ok)

	all := All()
	require.Len(t, all, 2, "All returns all registered kinds")
	assert.Equal(t, "alpha", all[0].Name(), "All is sorted: first element")
	assert.Equal(t, "beta", all[1].Name(), "All is sorted: second element")
}

func TestRegisterDuplicatePanics(t *testing.T) {
	t.Cleanup(Reset)
	Register(&fakeKind{name: "dup"})
	assert.Panics(t, func() { Register(&fakeKind{name: "dup"}) })
}
