package registry_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

type fakeInspector struct{ id string }

func (f fakeInspector) ID() string { return f.id }
func (f fakeInspector) Configure(json.RawMessage) (contentguard.Instance, error) {
	return fakeInstance{}, nil
}

type fakeInstance struct{}

func (fakeInstance) Points() []pipeline.Point { return []pipeline.Point{pipeline.PostToolCall} }
func (fakeInstance) Inspect(context.Context, contentguard.Subject) (contentguard.Finding, error) {
	return contentguard.Finding{Action: contentguard.Pass}, nil
}

func TestRegistry_RegisterGetAll(t *testing.T) {
	t.Cleanup(registry.Reset)
	registry.Register(fakeInspector{id: "fake"})

	got, ok := registry.Get("fake")
	require.True(t, ok)
	assert.Equal(t, "fake", got.ID())

	_, ok = registry.Get("missing")
	assert.False(t, ok)
	assert.Len(t, registry.All(), 1)
}
