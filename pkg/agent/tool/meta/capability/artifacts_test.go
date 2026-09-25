package capability

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/modality/files"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	assetregistry "github.com/authzed/openagentprimitives/pkg/channels/channelassets/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
)

// fakeArtifactService returns a non-nil *artifacts.Service for tests that only
// construct the artifact_* tools (never execute them). A nil memory backend is
// fine because Offer just builds tools; it does not call into the service.
func fakeArtifactService() *artifacts.Service { return artifacts.NewService(nil, nil) }

// artifactsFakeRenderer is a minimal channelassets.Renderer for driving the
// renderer registry in these tests. Mirrors internal/cmd/runner/main_test.go's fake.
type artifactsFakeRenderer struct {
	kind         string
	outputMIMEs  []string
	delivery     channelassets.DeliveryMode
	instructions string
	operatorOnly bool
}

func (f artifactsFakeRenderer) Kind() string { return f.kind }
func (artifactsFakeRenderer) ExecutionMode() channelassets.ExecutionMode {
	return channelassets.ExecutionModeInProcess
}
func (f artifactsFakeRenderer) Delivery() channelassets.DeliveryMode { return f.delivery }
func (artifactsFakeRenderer) MaxInputSize() int64                    { return 256 << 10 }
func (artifactsFakeRenderer) MaxOutputSize() int64                   { return 1 << 20 }
func (f artifactsFakeRenderer) OutputMIMEs() []string                { return f.outputMIMEs }
func (artifactsFakeRenderer) InputMIMEs() []string                   { return []string{"text/plain"} }
func (artifactsFakeRenderer) SupportsLiveView() bool                 { return false }
func (f artifactsFakeRenderer) AgentSelectable() bool                { return !f.operatorOnly }
func (f artifactsFakeRenderer) Instructions() string                 { return f.instructions }
func (artifactsFakeRenderer) ServeTransform(content []byte) []byte {
	return content
}
func (artifactsFakeRenderer) Render(_ context.Context, _ channelassets.Input) (channelassets.Output, error) {
	return channelassets.Output{}, nil
}

// registerStandaloneRenderer resets the global renderer registry to a single
// Standalone renderer producing image/png (available only with a matching
// asset:image/png capability) and restores the empty registry on cleanup.
//
// NOTE: mutates the process-wide registry — callers must NOT run in parallel.
func registerStandaloneRenderer(t *testing.T) {
	t.Helper()
	assetregistry.Reset()
	t.Cleanup(assetregistry.Reset)
	assetregistry.Register(artifactsFakeRenderer{
		kind:        "fakestandalone",
		outputMIMEs: []string{"image/png"},
		delivery:    channelassets.DeliveryStandalone,
	})
}

// clearRenderers empties the global renderer registry so AvailableAssetKinds
// yields zero kinds regardless of capabilities (no BundledOnly kind is present
// to be "always available"). Restores the empty registry on cleanup.
func clearRenderers(t *testing.T) {
	t.Helper()
	assetregistry.Reset()
	t.Cleanup(assetregistry.Reset)
}

func TestArtifactsOptInDefaultOff(t *testing.T) {
	c, ok := Lookup("artifacts")
	require.True(t, ok, "artifacts capability must be registered")
	assert.False(t, c.DefaultOn(), "artifacts is opt-in (default-off)")
	assert.False(t, c.Infrastructural(), "artifacts is not infrastructural")
}

func TestArtifactsSkipsWhenNoRenderer(t *testing.T) {
	// With no renderers registered at all, AvailableAssetKinds returns zero
	// kinds for ANY capability set — the true "channel advertises no renderer"
	// path. (When BundledOnly kinds like svg/css ARE registered they are always
	// available, so an empty result requires an empty registry.) A non-nil
	// artifact service is supplied so we reach the renderer check rather than the
	// service-nil guard above it.
	clearRenderers(t)

	c, _ := Lookup("artifacts")
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Granted: true, Enabled: true,
		Binding: &spiceboxv1alpha1.ChannelBinding{Capabilities: []string{"asset:image/png"}},
		Env:     RunnerEnv{Artifacts: fakeArtifactService()},
	})
	assert.Empty(t, tools)
	require.NotNil(t, skip, "empty renderer registry must skip, not silently succeed")
	assert.Equal(t, "artifacts", skip.Capability)
	assert.Contains(t, skip.Reason, "no renderer")
}

func TestArtifactsSkipsWhenServiceNil(t *testing.T) {
	// Renderer available (BundledOnly kinds make availableKinds non-empty), the
	// class granted artifacts, but no artifact service is wired. Offer must skip
	// (fail closed) rather than build artifact_* tools that would nil-panic in
	// FinalizeRevision on first use. This is the in-process factory's shape and
	// hardens a misconfigured runner too.
	registerStandaloneRenderer(t)

	c, _ := Lookup("artifacts")
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Granted: true, Enabled: true,
		Binding: &spiceboxv1alpha1.ChannelBinding{Capabilities: []string{"asset:image/png"}},
		Env:     RunnerEnv{Artifacts: nil}, // no artifact service wired
	})
	assert.Empty(t, tools, "no tools when the artifact service is absent")
	require.NotNil(t, skip, "missing artifact service must skip, not silently succeed")
	assert.Equal(t, "artifacts", skip.Capability)
	assert.Contains(t, skip.Reason, "artifact service not available")
}

func TestArtifactsInjectsFourToolsWhenAvailable(t *testing.T) {
	registerStandaloneRenderer(t)

	c, _ := Lookup("artifacts")
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Granted: true, Enabled: true,
		Binding: &spiceboxv1alpha1.ChannelBinding{Capabilities: []string{"asset:image/png"}},
		Env:     RunnerEnv{Artifacts: fakeArtifactService()},
	})
	assert.Nil(t, skip)
	assert.ElementsMatch(t,
		[]string{"artifact_prepare", "artifact_await", "artifact_history", "artifact_offer_view"},
		toolNames(tools))
}

// TestArtifactsOffersFetchArtifactWhenReaderWired asserts the files
// modality's Tier-1 read-by-reference tool rides the artifacts grant
// (design doc §5.4): present alongside the four artifact_* tools when
// RunnerEnv.ArtifactReader is wired, absent when it is nil. This is
// registry-driven (pkg/agent/modality/registry.All()), not a name check on
// "files" — the capability doesn't know which modality is registered.
func TestArtifactsOffersFetchArtifactWhenReaderWired(t *testing.T) {
	registerStandaloneRenderer(t)
	c, _ := Lookup("artifacts")

	t.Run("ArtifactReader wired: fetch_artifact is offered alongside the four artifact_* tools", func(t *testing.T) {
		tools, skip := c.Offer(OfferContext{
			Ctx:     context.Background(),
			Granted: true, Enabled: true,
			Binding: &spiceboxv1alpha1.ChannelBinding{Capabilities: []string{"asset:image/png"}},
			Env: RunnerEnv{
				Artifacts:      fakeArtifactService(),
				ArtifactReader: files.StoreReader{Store: blob.NewMem()},
			},
		})
		assert.Nil(t, skip)
		assert.ElementsMatch(t,
			[]string{"artifact_prepare", "artifact_await", "artifact_history", "artifact_offer_view", "fetch_artifact"},
			toolNames(tools))
	})

	t.Run("ArtifactReader nil: fetch_artifact is absent", func(t *testing.T) {
		tools, skip := c.Offer(OfferContext{
			Ctx:     context.Background(),
			Granted: true, Enabled: true,
			Binding: &spiceboxv1alpha1.ChannelBinding{Capabilities: []string{"asset:image/png"}},
			Env:     RunnerEnv{Artifacts: fakeArtifactService()},
		})
		assert.Nil(t, skip)
		names := toolNames(tools)
		assert.NotContains(t, names, "fetch_artifact")
		assert.ElementsMatch(t,
			[]string{"artifact_prepare", "artifact_await", "artifact_history", "artifact_offer_view"},
			names)
	})
}

func TestArtifactsNoBindingInactive(t *testing.T) {
	registerStandaloneRenderer(t)
	c, _ := Lookup("artifacts")
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Granted: true, Enabled: true,
		Binding: nil, // kubectl-driven: not channel-attached
		Env:     RunnerEnv{},
	})
	assert.Nil(t, skip, "no binding → not a skip, just inactive")
	assert.Empty(t, tools)
}

func TestArtifactsRendererAllowlist(t *testing.T) {
	registerStandaloneRenderer(t)
	c, _ := Lookup("artifacts")

	// Allowlist that includes the one available kind → tools injected.
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Granted: true, Enabled: true,
		Config:  artifactsConfig{Renderers: []string{"fakestandalone"}},
		Binding: &spiceboxv1alpha1.ChannelBinding{Capabilities: []string{"asset:image/png"}},
		Env:     RunnerEnv{Artifacts: fakeArtifactService()},
	})
	assert.Nil(t, skip)
	assert.Len(t, tools, 4)

	// Allowlist that excludes every available kind → skip.
	tools, skip = c.Offer(OfferContext{
		Ctx:     context.Background(),
		Granted: true, Enabled: true,
		Config:  artifactsConfig{Renderers: []string{"not-on-channel"}},
		Binding: &spiceboxv1alpha1.ChannelBinding{Capabilities: []string{"asset:image/png"}},
		Env:     RunnerEnv{Artifacts: fakeArtifactService()},
	})
	assert.Empty(t, tools)
	require.NotNil(t, skip)
	assert.Equal(t, "artifacts", skip.Capability)
	assert.Contains(t, skip.Reason, "no granted renderer")
}

func TestArtifactsParseConfig(t *testing.T) {
	c, _ := Lookup("artifacts")

	// Valid allowlist.
	cfg, err := c.ParseConfig(json.RawMessage(`{"renderers":["html"]}`))
	require.NoError(t, err)
	ac, ok := cfg.(artifactsConfig)
	require.True(t, ok, "ParseConfig must return artifactsConfig")
	assert.Equal(t, []string{"html"}, ac.Renderers)

	// Common {enabled} field is tolerated (ignored by artifacts' own parse).
	cfg, err = c.ParseConfig(json.RawMessage(`{"enabled":false}`))
	require.NoError(t, err)
	ac, ok = cfg.(artifactsConfig)
	require.True(t, ok)
	assert.Empty(t, ac.Renderers)

	// Empty raw is fine.
	_, err = c.ParseConfig(nil)
	require.NoError(t, err)

	// Wrong-typed renderers → error (Task 12's validation depends on this).
	_, err = c.ParseConfig(json.RawMessage(`{"renderers":"not-a-list"}`))
	require.Error(t, err, "a wrong-typed renderers field must be rejected")
}

// AvailableAssetKinds is the shared helper both the capability and internal/cmd/runner
// call to compute the renderer-kind names an agent may PRODUCE + live-view.
func TestAvailableAssetKinds(t *testing.T) {
	registerStandaloneRenderer(t) // fakestandalone, image/png, Standalone

	// Production + live-view is UNGATED by the channel's attach capability: a
	// Standalone kind (image/html) is available for artifact_prepare even when
	// the channel advertises no matching asset:* capability. It can still be
	// produced and live-viewed via a browser link; only ATTACHING it to a reply
	// needs the channel's asset:* cap (respond_to_user's `attached` field).
	assert.Equal(t, []string{"fakestandalone"},
		AvailableAssetKinds(&spiceboxv1alpha1.ChannelBinding{Capabilities: nil}),
		"a Standalone kind must be produceable without the channel's attach cap")

	// The matching capability does not change that — still available.
	assert.Equal(t, []string{"fakestandalone"},
		AvailableAssetKinds(&spiceboxv1alpha1.ChannelBinding{Capabilities: []string{"asset:image/png"}}))

	// Nil binding → nil (not channel-attached; no artifact tools at all).
	assert.Nil(t, AvailableAssetKinds(nil))

	// A kind that answers AgentSelectable=false is registered — the operator
	// renders it — but is never on the agent's menu.
	assetregistry.Register(artifactsFakeRenderer{
		kind: "operatoronly", outputMIMEs: []string{"application/x-demo"},
		delivery: channelassets.DeliveryStandalone, operatorOnly: true,
	})
	assert.Equal(t, []string{"fakestandalone"},
		AvailableAssetKinds(&spiceboxv1alpha1.ChannelBinding{Capabilities: nil}),
		"an operator-only kind is registered but not selectable")
}

// TestArtifactsInjectsToolsWithoutChannelAttachCapability is the decouple at the
// Offer level: a channel-attached session whose channel advertises NO asset:*
// attach capability STILL gets the artifact tools, because the agent can
// produce + live-view a Standalone artifact even on a channel that can't attach
// one. This is the web-chat case (builtin advertises only text/markdown/plan
// yet must be able to make + live-view an HTML artifact).
func TestArtifactsInjectsToolsWithoutChannelAttachCapability(t *testing.T) {
	registerStandaloneRenderer(t)
	c, _ := Lookup("artifacts")
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Granted: true, Enabled: true,
		Binding: &spiceboxv1alpha1.ChannelBinding{Capabilities: []string{"text", "markdown", "plan"}}, // no asset:*
		Env:     RunnerEnv{Artifacts: fakeArtifactService()},
	})
	assert.Nil(t, skip, "no attach cap must NOT skip — production/live-view is ungated")
	assert.ElementsMatch(t,
		[]string{"artifact_prepare", "artifact_await", "artifact_history", "artifact_offer_view"},
		toolNames(tools))
}
