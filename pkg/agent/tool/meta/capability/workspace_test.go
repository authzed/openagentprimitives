package capability

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds"
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/git"
	wsregistry "github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/registry"
)

// withGitDriver resets the workspacekinds registry to contain only the real
// git driver (an Applier) and restores an empty registry on cleanup. Mirrors
// artifacts_test.go's registerStandaloneRenderer/clearRenderers pattern: each
// test that needs registry state sets it up itself rather than relying on
// package-level blank-import + test ordering, which would make tests
// order-dependent (a Reset in one test would otherwise strand the registry
// empty for tests that run later in the same binary).
func withGitDriver(t *testing.T) {
	t.Helper()
	wsregistry.Reset()
	wsregistry.Register(git.New())
	t.Cleanup(wsregistry.Reset)
}

// nonApplierDriver is a minimal workspacekinds.Kind that deliberately does
// NOT implement workspacekinds.Applier, used to prove apply_workspace is
// withheld from a driver with no write-back support even when the grant asks
// for it.
type nonApplierDriver struct{}

func (nonApplierDriver) Name() string                       { return "fakekind" }
func (nonApplierDriver) Validate(workspacekinds.Spec) error { return nil }
func (nonApplierDriver) MaterializeCommands(workspacekinds.Spec, string) ([]workspacekinds.Command, error) {
	return nil, nil
}
func (nonApplierDriver) SyncCommands(workspacekinds.Spec, string) ([]workspacekinds.Command, error) {
	return nil, nil
}

func TestWorkspace_OptInDefaultOff(t *testing.T) {
	c, ok := Lookup("workspace")
	require.True(t, ok, "workspace capability must be registered")
	assert.Equal(t, "workspace", c.Name())
	assert.False(t, c.DefaultOn(), "workspace is opt-in — a class must explicitly list it")
	assert.False(t, c.Infrastructural())
}

func TestWorkspace_SkipsWhenNoSourceBound(t *testing.T) {
	c, _ := Lookup("workspace")
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Granted: true, Enabled: true,
		Env: RunnerEnv{WorkspaceSource: nil},
	})
	assert.Empty(t, tools)
	require.NotNil(t, skip, "no bound source must skip, not silently succeed")
	assert.Equal(t, "workspace", skip.Capability)
}

func TestWorkspace_SyncOnlyByDefault(t *testing.T) {
	// Present + granted, no {"apply":true} config → sync_workspace only. This
	// pins the security-relevant default: write-back is never silently active.
	c, _ := Lookup("workspace")
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Granted: true, Enabled: true,
		Env: RunnerEnv{WorkspaceSource: &WorkspaceSourceRuntime{
			Kind: "git", Locator: "https://example.invalid/repo.git", WorkDir: "/workspace", OverlayPVC: "overlay-pvc",
		}},
	})
	assert.Nil(t, skip)
	assert.Equal(t, []string{"sync_workspace"}, toolNames(tools))
}

func TestWorkspace_ApplyOffersWhenConfigEnabledAndDriverIsApplier(t *testing.T) {
	withGitDriver(t)
	c, _ := Lookup("workspace")
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Granted: true, Enabled: true,
		Config: workspaceConfig{Apply: true},
		Env: RunnerEnv{WorkspaceSource: &WorkspaceSourceRuntime{
			Kind: "git", Locator: "https://example.invalid/repo.git", WorkDir: "/workspace",
			OverlayPVC: "overlay-pvc", CredSecret: "ws-cred",
		}},
	})
	assert.Nil(t, skip)
	assert.ElementsMatch(t, []string{"sync_workspace", "apply_workspace"}, toolNames(tools))
}

func TestWorkspace_ApplyWithheldWhenDriverIsNotApplier(t *testing.T) {
	// Config asks for apply, but the bound source's driver kind has no
	// write-back support. apply_workspace must be withheld — never a silent
	// no-op tool that would fail at Execute time.
	wsregistry.Reset()
	t.Cleanup(wsregistry.Reset)
	wsregistry.Register(nonApplierDriver{})

	c, _ := Lookup("workspace")
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Granted: true, Enabled: true,
		Config: workspaceConfig{Apply: true},
		Env: RunnerEnv{WorkspaceSource: &WorkspaceSourceRuntime{
			Kind: "fakekind", Locator: "loc", WorkDir: "/workspace", OverlayPVC: "overlay-pvc",
		}},
	})
	assert.Nil(t, skip)
	assert.Equal(t, []string{"sync_workspace"}, toolNames(tools))
}

func TestWorkspace_ParseConfig(t *testing.T) {
	c, _ := Lookup("workspace")

	cfg, err := c.ParseConfig(nil)
	require.NoError(t, err)
	assert.Equal(t, workspaceConfig{}, cfg)

	cfg, err = c.ParseConfig(json.RawMessage(`{"apply":true}`))
	require.NoError(t, err)
	wc, ok := cfg.(workspaceConfig)
	require.True(t, ok)
	assert.True(t, wc.Apply)

	_, err = c.ParseConfig(json.RawMessage(`{"apply":"not-a-bool"}`))
	assert.Error(t, err)
}

func TestWorkspace_RegisteredAndValidates(t *testing.T) {
	require.NoError(t, ValidateGrant("workspace", json.RawMessage(`{}`)))
	require.NoError(t, ValidateGrant("workspace", json.RawMessage(`{"apply":true}`)))
	require.Error(t, ValidateGrant("workspace", json.RawMessage(`{"apply":"not-a-bool"}`)))
}

// TestWorkspace_ApplyIsExternal pins the security invariant this whole
// capability exists to enforce: apply_workspace's Permission is External, so
// the runner's authz hook routes every call through human approval — it can
// never silently reconcile overlay edits back to the source origin.
func TestWorkspace_ApplyIsExternal(t *testing.T) {
	withGitDriver(t)
	c, _ := Lookup("workspace")
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Granted: true, Enabled: true,
		Config: workspaceConfig{Apply: true},
		Env: RunnerEnv{WorkspaceSource: &WorkspaceSourceRuntime{
			Kind: "git", Locator: "https://example.invalid/repo.git", WorkDir: "/workspace", OverlayPVC: "overlay-pvc",
		}},
	})
	require.Nil(t, skip)
	var apply, sync bool
	for _, tl := range tools {
		switch tl.Name() {
		case "apply_workspace":
			apply = true
			assert.Equal(t, authz.External, tl.Permission().StateImpact, "apply_workspace must be External (human-approval gated)")
		case "sync_workspace":
			sync = true
			assert.Equal(t, authz.Passthrough, tl.Permission().StateImpact, "sync_workspace must be Passthrough (no Check, exempt from the dispatch gate)")
		}
	}
	assert.True(t, apply, "apply_workspace must be offered in this scenario")
	assert.True(t, sync, "sync_workspace must be offered in this scenario")
}
