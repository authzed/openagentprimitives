package v1alpha1

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func TestToolCallModeInteractive(t *testing.T) {
	assert.Equal(t, ToolCallMode("interactive"), ToolCallModeInteractive)
}

func TestToolCallSpec_InteractiveTimeouts(t *testing.T) {
	s := ToolCallSpec{
		Mode:        ToolCallModeInteractive,
		IdleTimeout: metav1.Duration{Duration: 15 * time.Minute},
		MaxDuration: metav1.Duration{Duration: 2 * time.Hour},
	}
	assert.Equal(t, 15*time.Minute, s.IdleTimeout.Duration)
	assert.Equal(t, 2*time.Hour, s.MaxDuration.Duration)
}

func TestToolCallSpec_PreDispatchSnapshot(t *testing.T) {
	original := ToolCallSpec{
		Session: "sess-bundle",
		Tool:    "bash_run",
		PreDispatchSnapshot: &PreDispatchSnapshot{
			SessionUID: "uid-1",
			TurnIndex:  7,
			Sequence:   0,
		},
	}
	data, err := yaml.Marshal(original)
	require.NoError(t, err)

	var got ToolCallSpec
	require.NoError(t, yaml.Unmarshal(data, &got))

	require.NotNil(t, got.PreDispatchSnapshot)
	assert.Equal(t, "uid-1", got.PreDispatchSnapshot.SessionUID)
	assert.Equal(t, int32(7), got.PreDispatchSnapshot.TurnIndex)
	assert.Equal(t, int32(0), got.PreDispatchSnapshot.Sequence)
}
