package meta_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

type fakeTool struct{ name string }

func (f *fakeTool) Name() string                 { return f.name }
func (f *fakeTool) Kind() tool.Kind              { return tool.KindMeta }
func (f *fakeTool) Description() string          { return "" }
func (f *fakeTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (f *fakeTool) Execute(_ context.Context, _ json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	return tool.Result{}, nil
}
func (*fakeTool) Permission() authz.Permission                  { return authz.Permission{StateImpact: authz.Stateless} }
func (*fakeTool) PermissionVariants() []authz.PermissionVariant { return nil }

// restoreRegistryAfter snapshots the package-global meta registry and
// restores it via t.Cleanup. Tests that Register() into the global
// registry must use this: init() seeds the registry exactly once and
// nothing repopulates it, so any test that mutates the global registry
// must restore it afterward or it breaks every sibling test that does a
// lookup.
func restoreRegistryAfter(t *testing.T) {
	t.Helper()
	snap := meta.SnapshotForTest()
	t.Cleanup(func() { meta.RestoreForTest(snap) })
}

func TestRegisterAndLoad(t *testing.T) {
	restoreRegistryAfter(t)
	// agent_work_complete is already registered via init(); we add two more.
	meta.Register(&fakeTool{name: "alpha"})
	meta.Register(&fakeTool{name: "beta"})
	got := meta.Load()
	names := map[string]bool{}
	for _, tl := range got {
		names[tl.Name()] = true
	}
	assert.True(t, names["alpha"], "alpha must be loaded from registry; got %v", names)
	assert.True(t, names["beta"], "beta must be loaded from registry; got %v", names)
}

func TestRegisterDuplicatePanics(t *testing.T) {
	restoreRegistryAfter(t)
	meta.Register(&fakeTool{name: "x"})
	assert.Panics(t, func() {
		meta.Register(&fakeTool{name: "x"})
	}, "duplicate Register must panic")
}
