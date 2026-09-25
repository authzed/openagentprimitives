package v1alpha1_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// TestAgentSessionSpec_InputChannelParses asserts that the canonical
// `inputChannel` field unmarshals onto AgentSessionSpec.InputChannel.
func TestAgentSessionSpec_InputChannelParses(t *testing.T) {
	const src = `class: foo
prompt: {inline: "hi"}
inputChannel: {name: ch, kind: slack, key: k, capabilities: [], natsSubjectPrefix: ap.x}`
	var spec spiceboxv1alpha1.AgentSessionSpec
	require.NoError(t, yaml.Unmarshal([]byte(src), &spec), "unmarshal AgentSessionSpec YAML")
	require.NotNil(t, spec.InputChannel, "InputChannel should be populated")
	assert.Equal(t, "ch", spec.InputChannel.Name, "InputChannel.Name from canonical inputChannel field")
}

// TestAgentSessionSpec_ForkedFromFields asserts that the ForkedFrom and
// ForkedAtTurn fields round-trip correctly through YAML serialization.
func TestAgentSessionSpec_ForkedFromFields(t *testing.T) {
	original := spiceboxv1alpha1.AgentSessionSpec{
		Class:        "demo",
		Prompt:       spiceboxv1alpha1.PromptSource{Inline: "hi"},
		ForkedFrom:   "parent-session",
		ForkedAtTurn: ptr.To(int32(5)),
	}

	// Marshal to YAML
	data, err := yaml.Marshal(original)
	require.NoError(t, err, "marshal AgentSessionSpec to YAML")

	// Unmarshal into a fresh struct
	var roundTripped spiceboxv1alpha1.AgentSessionSpec
	require.NoError(t, yaml.Unmarshal(data, &roundTripped), "unmarshal AgentSessionSpec from YAML")

	// Assert the fields came back correctly
	assert.Equal(t, "parent-session", roundTripped.ForkedFrom, "ForkedFrom should round-trip")
	require.NotNil(t, roundTripped.ForkedAtTurn, "ForkedAtTurn should round-trip and not be nil")
	assert.Equal(t, int32(5), *roundTripped.ForkedAtTurn, "ForkedAtTurn value should be 5")
}

// TestResolvedSidecarToolbox_LifecycleFields asserts that the lifecycle fields
// added for secret-gated separate-pod sidecar toolboxes round-trip through
// YAML serialization correctly.
func TestResolvedSidecarToolbox_LifecycleFields(t *testing.T) {
	original := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name:            "kube-access",
		Ref:             "kube-access-toolbox",
		Port:            8080,
		RunMode:         "separate-pod",
		SidecarPodName:  "kube-access-pod-abc123",
		SidecarPodIP:    "1.2.3.4",
		AwaitingSecret:  true,
		SecretTokenHash: "abc",
	}

	data, err := yaml.Marshal(original)
	require.NoError(t, err, "marshal ResolvedSidecarToolbox to YAML")

	var got spiceboxv1alpha1.ResolvedSidecarToolbox
	require.NoError(t, yaml.Unmarshal(data, &got), "unmarshal ResolvedSidecarToolbox from YAML")

	assert.Equal(t, "separate-pod", got.RunMode, "RunMode should round-trip")
	assert.Equal(t, "kube-access-pod-abc123", got.SidecarPodName, "SidecarPodName should round-trip")
	assert.Equal(t, "1.2.3.4", got.SidecarPodIP, "SidecarPodIP should round-trip")
	assert.True(t, got.AwaitingSecret, "AwaitingSecret should round-trip as true")
	assert.Equal(t, "abc", got.SecretTokenHash, "SecretTokenHash should round-trip")
}

// TestAgentSessionStatus_RestartFields asserts that the SupersededBy and
// PendingRestart fields round-trip correctly through YAML serialization.
func TestAgentSessionStatus_RestartFields(t *testing.T) {
	original := spiceboxv1alpha1.AgentSessionStatus{
		SupersededBy: "child-session",
		PendingRestart: &spiceboxv1alpha1.PendingRestart{
			CutTurnIndex:      5,
			NewUserText:       "edited prompt",
			TriggeredBy:       "user:alice",
			RequestedAt:       metav1.NewTime(time.Unix(1716800000, 0).UTC()),
			TargetSessionName: "child-session",
		},
	}
	data, err := yaml.Marshal(original)
	require.NoError(t, err)

	var got spiceboxv1alpha1.AgentSessionStatus
	require.NoError(t, yaml.Unmarshal(data, &got))

	assert.Equal(t, "child-session", got.SupersededBy)
	require.NotNil(t, got.PendingRestart)
	assert.Equal(t, int32(5), got.PendingRestart.CutTurnIndex)
	assert.Equal(t, "edited prompt", got.PendingRestart.NewUserText)
	assert.Equal(t, identity.Subject("user:alice"), got.PendingRestart.TriggeredBy)
	assert.Equal(t, "child-session", got.PendingRestart.TargetSessionName)
	assert.Equal(t, int64(1716800000), got.PendingRestart.RequestedAt.Unix())
}
