package v1alpha1_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestAgentClassStatus_OapInstall_RoundTrip(t *testing.T) {
	installedAt := metav1.NewTime(time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))

	ac := spiceboxv1alpha1.AgentClass{
		Status: spiceboxv1alpha1.AgentClassStatus{
			OapInstall: &spiceboxv1alpha1.OapInstallStatus{
				SourceRef:   "ghcr.io/example/demo-agent:1.2.0",
				Digest:      "sha256:abc123def456",
				Version:     "1.2.0",
				SourceKind:  "oci",
				InstalledAt: installedAt,
			},
		},
	}

	raw, err := json.Marshal(ac)
	require.NoError(t, err, "marshal AgentClass")

	var round spiceboxv1alpha1.AgentClass
	require.NoError(t, json.Unmarshal(raw, &round), "unmarshal AgentClass")

	require.NotNil(t, round.Status.OapInstall, "OapInstall should round-trip non-nil")
	assert.Equal(t, "ghcr.io/example/demo-agent:1.2.0", round.Status.OapInstall.SourceRef, "SourceRef")
	assert.Equal(t, "sha256:abc123def456", round.Status.OapInstall.Digest, "Digest")
	assert.Equal(t, "1.2.0", round.Status.OapInstall.Version, "Version")
	assert.Equal(t, "oci", round.Status.OapInstall.SourceKind, "SourceKind")
	assert.True(t, installedAt.Time.Equal(round.Status.OapInstall.InstalledAt.Time), "InstalledAt")
}

func TestAgentClassStatus_OapInstall_OmittedWhenNil(t *testing.T) {
	ac := spiceboxv1alpha1.AgentClass{
		Status: spiceboxv1alpha1.AgentClassStatus{},
	}

	raw, err := json.Marshal(ac.Status)
	require.NoError(t, err, "marshal AgentClassStatus")

	var asMap map[string]any
	require.NoError(t, json.Unmarshal(raw, &asMap), "unmarshal into map")

	_, present := asMap["oapInstall"]
	assert.False(t, present, "oapInstall must be omitted from JSON when nil")
}
