package config_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// stubProjector returns canned rows and ignores the client — enough to prove
// the registry dispatch + row shape without a real CRD.
type stubProjector struct {
	resource string
	rows     []config.ResourceRow
}

func (s stubProjector) Resource() string { return s.resource }
func (s stubProjector) List(_ context.Context, _ client.Client) ([]config.ResourceRow, error) {
	return s.rows, nil
}

func TestRegistry_RegisterGetAll(t *testing.T) {
	// Package-global registry; isolate this test from any real projectors a
	// blank import might add by resetting, then register a uniquely-named stub.
	config.Reset()
	t.Cleanup(config.Reset)

	stub := stubProjector{resource: "widgets", rows: []config.ResourceRow{
		{Name: "w1", Scope: "cluster", Status: "Valid"},
		{Name: "w2", Namespace: "ns", Scope: "namespaced", Status: "Degraded", StatusReason: "boom"},
	}}
	config.Register(stub)

	got, ok := config.Get("widgets")
	require.True(t, ok, "registered projector must be found")
	rows, err := got.List(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "w1", rows[0].Name)

	_, ok = config.Get("nope")
	assert.False(t, ok, "unregistered slug must miss")

	all := config.All()
	require.Len(t, all, 1)
	assert.Equal(t, "widgets", all[0].Resource())
}

func TestRegistry_DuplicateRegistrationPanics(t *testing.T) {
	config.Reset()
	t.Cleanup(config.Reset)
	config.Register(stubProjector{resource: "dup"})
	assert.Panics(t, func() { config.Register(stubProjector{resource: "dup"}) },
		"duplicate slug must panic (init-time programmer error)")
}
