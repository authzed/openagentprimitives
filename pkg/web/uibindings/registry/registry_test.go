package registry_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings/registry"
)

type fakeResolver struct{ source string }

func (f fakeResolver) Source() string { return f.source }
func (f fakeResolver) Resolve(context.Context, uibindings.Deps, uibindings.Request) (uibindings.Result, error) {
	return uibindings.Result{Value: json.RawMessage(`"ok"`)}, nil
}

func TestRegistry(t *testing.T) {
	t.Cleanup(registry.Reset)
	registry.Reset()
	registry.Register(fakeResolver{source: "tool"})

	got, ok := registry.Get("tool")
	require.True(t, ok, "a registered source must resolve")
	assert.Equal(t, "tool", got.Source())

	_, ok = registry.Get("nope")
	assert.False(t, ok, "an unregistered source must fail closed, not panic")

	_, ok = registry.Get("")
	assert.False(t, ok, "the empty source must never resolve")

	assert.Equal(t, []string{"tool"}, registry.Keys())
}
