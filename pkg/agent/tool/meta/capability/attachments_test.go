package capability

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/modality/files"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
)

func TestAttachmentsCapability_RegisteredAndShaped(t *testing.T) {
	c, ok := Lookup("attachments")
	require.True(t, ok, "attachments must be registered via init()")

	assert.Equal(t, "attachments", c.Name())
	assert.False(t, c.DefaultOn(), "attachments is opt-in: absent from spec.capabilities must mean disabled")
	assert.False(t, c.Infrastructural())

	cfg, err := c.ParseConfig(nil)
	require.NoError(t, err)
	assert.Nil(t, cfg)
}

// TestAttachmentsCapability_NoBindingInactive mirrors
// TestArtifactsNoBindingInactive: a kubectl-driven (not channel-attached)
// session has nothing to read an attachment from, so Offer is simply
// inactive — not a skip, since there's no failure to report.
func TestAttachmentsCapability_NoBindingInactive(t *testing.T) {
	c, ok := Lookup("attachments")
	require.True(t, ok)

	tools, skip := c.Offer(OfferContext{Granted: true, Enabled: true, Binding: nil})
	assert.Nil(t, tools)
	assert.Nil(t, skip)
}

// TestAttachmentsCapability_SkipsWhenReaderNil proves the "never silent"
// half of this capability's contract: channel-attached, granted, but no
// ArtifactReader wired (a runner build that never wires one, or an e2e
// harness that forgot Harness.SetArtifactStore) must produce a logged
// SkipReason, not a quiet absence indistinguishable from "nothing to offer."
func TestAttachmentsCapability_SkipsWhenReaderNil(t *testing.T) {
	c, ok := Lookup("attachments")
	require.True(t, ok)

	tools, skip := c.Offer(OfferContext{
		Granted: true, Enabled: true,
		Binding: &spiceboxv1alpha1.ChannelBinding{},
		Env:     RunnerEnv{}, // ArtifactReader nil
	})
	assert.Empty(t, tools)
	require.NotNil(t, skip)
	assert.Equal(t, "attachments", skip.Capability)
	assert.Contains(t, skip.Reason, "artifact reader")
}

// TestAttachmentsCapability_OffersFetchArtifactWhenReaderWired is C1's core
// fix, proven at the Offer level (mirrors
// TestArtifactsOffersFetchArtifactWhenReaderWired): channel-attached,
// granted, ArtifactReader wired — fetch_artifact is offered, and ONLY
// fetch_artifact. No artifact_prepare/await/history/offer_view: those ride
// the separate, broader "artifacts" grant (see this capability's doc for
// why requiring it here would be a least-privilege violation).
func TestAttachmentsCapability_OffersFetchArtifactWhenReaderWired(t *testing.T) {
	c, ok := Lookup("attachments")
	require.True(t, ok)

	tools, skip := c.Offer(OfferContext{
		Granted: true, Enabled: true,
		Binding: &spiceboxv1alpha1.ChannelBinding{},
		Env:     RunnerEnv{ArtifactReader: files.StoreReader{Store: blob.NewMem()}},
	})
	assert.Nil(t, skip)
	assert.ElementsMatch(t, []string{"fetch_artifact"}, toolNames(tools))
}

// TestAttachmentsOnly_AssembleYieldsFetchArtifact_WithoutArtifactsGrant is
// the C1 regression: the actual bug this fix round exists for was that a
// user who granted ONLY "attachments" (satisfying every requirement the
// shipped Channel CRD doc describes — see channel_types.go's
// ChannelAttachmentsSpec) got a turn manifest line telling them to call
// fetch_artifact, and no such tool in the assembled table, because
// fetch_artifact secretly also required the broader "artifacts" capability
// PLUS a registered channelassets renderer. This test isolates the registry
// to the REAL attachmentsCapability (resetRegistryForTest + Register — same
// pattern assemble_test.go uses for its fake offerCap capabilities, applied
// here to the actual production type so the assertion is about real
// behavior, not a stand-in), drives it through Assemble with an AgentClass
// granting attachments alone, and asserts fetch_artifact is present.
// Deliberately registers NO channelassets renderer at all — proving "no
// renderer registered" (the failure this fix round's own e2e run hit before
// this fix) is no longer a requirement for this path. Every OTHER
// capability is left unregistered so this test is not incidentally coupled
// to whatever RunnerEnv fields some unrelated default-on capability (e.g.
// channelhistory) happens to dereference.
func TestAttachmentsOnly_AssembleYieldsFetchArtifact_WithoutArtifactsGrant(t *testing.T) {
	resetRegistryForTest(t)
	Register(&attachmentsCapability{})

	class := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Capabilities: caps("attachments", `{}`), // attachments ONLY — no "artifacts" key at all
		},
	}
	binding := &spiceboxv1alpha1.ChannelBinding{}
	env := RunnerEnv{ArtifactReader: files.StoreReader{Store: blob.NewMem()}}

	got := Assemble(context.Background(), AssembleDeps{
		Class:   class,
		Binding: binding,
		Env:     env,
		Logger:  logr.Discard(),
	})

	require.Len(t, got, 1, "attachments-only must yield exactly fetch_artifact, nothing else")
	assert.Equal(t, "fetch_artifact", got[0].Name(),
		"granting attachments alone must yield fetch_artifact — it must not require the separate artifacts grant")
}

// TestAttachmentsAndArtifactsBothGranted_NoDuplicateFetchArtifact proves
// Assemble's name-based dedup: both capabilities call the same
// modalityMetaTools helper, so granting both must not double-inject
// fetch_artifact (which no LLM provider's tool-use API tolerates). Isolates
// the registry to the two REAL capabilities under test, same rationale as
// the test above.
func TestAttachmentsAndArtifactsBothGranted_NoDuplicateFetchArtifact(t *testing.T) {
	resetRegistryForTest(t)
	Register(&attachmentsCapability{})
	Register(&artifactsCapability{})
	registerStandaloneRenderer(t) // artifacts requires a registered renderer; attachments does not

	class := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Capabilities: map[string]apiextensionsv1.JSON{
				"attachments": {Raw: []byte(`{}`)},
				"artifacts":   {Raw: []byte(`{}`)},
			},
		},
	}

	binding := &spiceboxv1alpha1.ChannelBinding{Capabilities: []string{"asset:image/png"}}
	env := RunnerEnv{
		Artifacts:      fakeArtifactService(),
		ArtifactReader: files.StoreReader{Store: blob.NewMem()},
	}

	got := Assemble(context.Background(), AssembleDeps{
		Class:   class,
		Binding: binding,
		Env:     env,
		Logger:  logr.Discard(),
	})

	count := 0
	for _, tt := range got {
		if tt.Name() == "fetch_artifact" {
			count++
		}
	}
	assert.Equal(t, 1, count, "fetch_artifact must appear exactly once even when both capabilities offer it")
	assert.Len(t, got, 5, "artifact_prepare/await/history/offer_view + one fetch_artifact")
}
