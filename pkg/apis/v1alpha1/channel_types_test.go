package v1alpha1_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestChannelSpec_YAMLFields covers field-level YAML decoding for ChannelSpec.
// Each case decodes a ChannelSpec from YAML and asserts the field(s) of
// interest are populated correctly.
func TestChannelSpec_YAMLFields(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		check func(t *testing.T, spec spiceboxv1alpha1.ChannelSpec)
	}{
		{
			name: "role field: parses to ChannelSpec.Role",
			src: `
kind: slack
role: input
agentClass: foo
credentialsRef:
  secretName: foo-creds
`,
			check: func(t *testing.T, spec spiceboxv1alpha1.ChannelSpec) {
				assert.Equal(t, "input", spec.Role, "Role")
			},
		},
		{
			name: "authzSubject field: parses to ChannelSpec.AuthzSubject",
			src: `
kind: bento
role: input
agentClass: foo
authzSubject: "service:hubspot-digest-bot"
credentialsRef:
  secretName: foo-creds
`,
			check: func(t *testing.T, spec spiceboxv1alpha1.ChannelSpec) {
				assert.Equal(t, "service:hubspot-digest-bot", spec.AuthzSubject, "AuthzSubject")
			},
		},
		{
			name: "bento config: Generate.Interval populated",
			src: `
kind: bento
role: input
agentClass: foo
authzSubject: "service:foo-bot"
credentialsRef:
  secretName: foo-creds
bento:
  generate:
    interval: "@every 168h"
    mapping: |
      root.message = "hi"
    count: 0
`,
			check: func(t *testing.T, spec spiceboxv1alpha1.ChannelSpec) {
				require.NotNil(t, spec.Bento, "Bento")
				require.NotNil(t, spec.Bento.Generate, "Bento.Generate")
				assert.Equal(t, "@every 168h", spec.Bento.Generate.Interval, "Bento.Generate.Interval")
			},
		},
		{
			name: "slack outputDefaults: ChannelID populated",
			src: `
kind: slack
role: output
agentClass: foo
credentialsRef:
  secretName: foo-creds
slack:
  outputDefaults:
    channelId: "C0123"
    threadStrategy: "new-thread-per-session"
`,
			check: func(t *testing.T, spec spiceboxv1alpha1.ChannelSpec) {
				require.NotNil(t, spec.Slack, "Slack")
				require.NotNil(t, spec.Slack.OutputDefaults, "Slack.OutputDefaults")
				assert.Equal(t, "C0123", spec.Slack.OutputDefaults.ChannelID, "Slack.OutputDefaults.ChannelID")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var spec spiceboxv1alpha1.ChannelSpec
			require.NoError(t, yaml.Unmarshal([]byte(tc.src), &spec), "unmarshal ChannelSpec YAML")
			tc.check(t, spec)
		})
	}
}

func TestChannelOwnerPolicy_RoundTrips(t *testing.T) {
	in := spiceboxv1alpha1.Channel{Spec: spiceboxv1alpha1.ChannelSpec{
		Kind:       "bento",
		AgentClass: "digest",
		Owner: &spiceboxv1alpha1.ChannelOwnerPolicy{
			Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{FromOutputChannel: true},
		},
	}}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	var back spiceboxv1alpha1.Channel
	require.NoError(t, json.Unmarshal(b, &back))
	require.NotNil(t, back.Spec.Owner)
	require.NotNil(t, back.Spec.Owner.Ownerless)
	assert.True(t, back.Spec.Owner.Ownerless.FromOutputChannel)
	assert.Equal(t, "", back.Spec.Owner.Explicit)
}

func TestChannelCRD_KindEnum_IncludesLocal(t *testing.T) {
	data, err := os.ReadFile("../../../config/crds/agentprimitives.authzed.com_channels.yaml")
	require.NoError(t, err, "generated Channel CRD must exist")
	// The kind field's enum block lists each accepted value on its own
	// "- <value>" line under the kind property.
	assert.Contains(t, string(data), "- local",
		"the Channel CRD kind enum must accept 'local' after gen:api")
}

// TestGitHubChannelConfig_DeepCopyDoesNotAliasSlices asserts DeepCopy gives
// GitHubChannelConfig.Repositories its own backing array, not a shared slice
// header with the original. This is the one property Go construction and
// DeepCopy can actually prove at this layer; kubebuilder-default behavior
// (e.g. the nil-means-skip-drafts semantic) is apiserver structural-schema
// defaulting and is not observable from a pkg/apis unit test.
func TestGitHubChannelConfig_DeepCopyDoesNotAliasSlices(t *testing.T) {
	in := spiceboxv1alpha1.ChannelSpec{
		Kind: "github", Role: spiceboxv1alpha1.ChannelRoleInput, AgentClass: "demo-reviewbot",
		GitHub: &spiceboxv1alpha1.GitHubChannelConfig{
			AppSlug:      "demo-reviewbot",
			Repositories: []string{"demo-org/platform"},
			Events:       []string{"opened", "synchronize"},
			SkipDrafts:   ptr.To(false),
		},
	}
	out := in.DeepCopy()
	require.NotNil(t, out.GitHub)
	assert.Equal(t, []string{"demo-org/platform"}, out.GitHub.Repositories)

	out.GitHub.Repositories[0] = "mutated"
	assert.Equal(t, "demo-org/platform", in.GitHub.Repositories[0], "deepcopy must not alias slices")
}
