package bento

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestBuildStreamConfig_GenerateMappingInterval(t *testing.T) {
	ch := &spiceboxv1alpha1.Channel{
		Spec: spiceboxv1alpha1.ChannelSpec{
			Bento: &spiceboxv1alpha1.BentoChannelConfig{
				Generate: &spiceboxv1alpha1.BentoGenerateConfig{
					Mapping:  `root.message = "hello"`,
					Interval: "@every 1s",
					Count:    3,
				},
			},
		},
	}
	yaml, err := buildStreamYAML(ch)
	require.NoError(t, err, "buildStreamYAML")

	for _, want := range []string{
		"input:",
		"generate:",
		`mapping: 'root.message = "hello"'`,
		`interval: "@every 1s"`,
		"count: 3",
		"output:",
		"forward_to_pipeline:",
	} {
		assert.Contains(t, yaml, want, "buildStreamYAML output should contain %q", want)
	}
}
