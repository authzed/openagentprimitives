package oap

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManifestValidateRequiredAgentShapes(t *testing.T) {
	tests := []struct {
		name string
		dep  RequiredAgent
		want string
	}{
		{"folder path", RequiredAgent{Path: "dependencies/worker"}, ""},
		{"absolute", RequiredAgent{Path: "/tmp/worker"}, "path must be relative"},
		{"escape", RequiredAgent{Path: "../worker"}, "path escapes bundle root"},
		{"mixed", RequiredAgent{Path: "worker", Name: "worker"}, "cannot mix path with packed descriptor"},
		{"packed", RequiredAgent{Name: "worker", Version: "1.0.0", MediaType: DependencyMediaType, Digest: "sha256:" + strings.Repeat("a", 64)}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manifest{OapFormatVersion: "1", Agent: Agent{Name: "root", Version: "1.0.0"}}
			m.Requires.Agents = []RequiredAgent{tt.dep}
			err := m.Validate()
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestManifestValidateRequiredAgentRejectsIncompletePackedDescriptor(t *testing.T) {
	base := RequiredAgent{
		Name:      "worker",
		Version:   "1.0.0",
		MediaType: DependencyMediaType,
		Digest:    "sha256:" + strings.Repeat("a", 64),
	}
	tests := []struct {
		name string
		edit func(*RequiredAgent)
		want string
	}{
		{"name", func(dep *RequiredAgent) { dep.Name = "" }, "name is required"},
		{"version", func(dep *RequiredAgent) { dep.Version = "" }, "version is required"},
		{"media type", func(dep *RequiredAgent) { dep.MediaType = "" }, "mediaType is required"},
		{"wrong media type", func(dep *RequiredAgent) { dep.MediaType = "application/octet-stream" }, "mediaType must be " + DependencyMediaType},
		{"digest", func(dep *RequiredAgent) { dep.Digest = "" }, "digest is required"},
		{"invalid digest", func(dep *RequiredAgent) { dep.Digest = "not-a-digest" }, "invalid digest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dep := base
			tt.edit(&dep)
			m := &Manifest{OapFormatVersion: "1", Agent: Agent{Name: "root", Version: "1.0.0"}}
			m.Requires.Agents = []RequiredAgent{dep}
			require.ErrorContains(t, m.Validate(), tt.want)
		})
	}
}

func TestManifestValidateRequiredAgentRejectsUncleanAndDuplicatePaths(t *testing.T) {
	tests := []struct {
		name string
		deps []RequiredAgent
		want string
	}{
		{"blank", []RequiredAgent{{}}, "path is required"},
		{"dot", []RequiredAgent{{Path: "."}}, "path must name a dependency folder"},
		{"unclean", []RequiredAgent{{Path: "dependencies/../worker"}}, "path must be clean"},
		{"duplicate", []RequiredAgent{{Path: "dependencies/worker"}, {Path: "dependencies/worker"}}, "duplicate path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manifest{OapFormatVersion: "1", Agent: Agent{Name: "root", Version: "1.0.0"}}
			m.Requires.Agents = tt.deps
			require.ErrorContains(t, m.Validate(), tt.want)
		})
	}
}

func TestParseManifest_ValidAndForwardCompatible(t *testing.T) {
	// An unknown additive field ("futureField") must NOT break parsing.
	data := []byte(`
oapFormatVersion: "1"
agent:
  name: demo-agent
  version: "1.2.0"
  displayName: Demo Agent
futureField: ignored-by-this-build
requires:
  secrets:
    - name: demo-pat
      keys: [token]
      question: demoToken
`)
	m, err := ParseManifest(data)
	require.NoError(t, err)
	require.NoError(t, m.Validate())
	assert.Equal(t, "demo-agent", m.Agent.Name)
	assert.Equal(t, "1.2.0", m.Agent.Version)
	require.Len(t, m.Requires.Secrets, 1)
	assert.Equal(t, "demoToken", m.Requires.Secrets[0].Question)
}

func TestManifestValidate_Errors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"missing format version: error", "agent:\n  name: a\n  version: \"1\"\n", "oapFormatVersion is required"},
		{"future major: upgrade oap", "oapFormatVersion: \"2\"\nagent:\n  name: a\n  version: \"1\"\n", "newer than this oap supports"},
		{"non-integer major: error", "oapFormatVersion: \"x\"\nagent:\n  name: a\n  version: \"1\"\n", "major must be an integer"},
		{"missing agent.name: error", "oapFormatVersion: \"1\"\nagent:\n  version: \"1\"\n", "agent.name is required"},
		{"missing agent.version: error", "oapFormatVersion: \"1\"\nagent:\n  name: a\n", "agent.version is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseManifest([]byte(tc.yaml))
			require.NoError(t, err, "parse should not fail; validation should")
			err = m.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestRequiredImageBuildRoundTrip(t *testing.T) {
	in := []byte(`
oapFormatVersion: "1"
agent: { name: x, version: "1.0.0" }
requires:
  images:
    - ref: sre-sandbox:dev
      build:
        dockerfile: images/sandbox/Dockerfile
        context: images/sandbox
        secrets: [github_token]
    - ref: sre-dedicated-mcp:dev
      build:
        dockerfile: images/dedicated-mcp/Dockerfile
        context: images/dedicated-mcp
        buildContexts: [dmcp]
`)
	m, err := ParseManifest(in)
	require.NoError(t, err)
	require.Len(t, m.Requires.Images, 2)
	assert.Equal(t, "sre-sandbox:dev", m.Requires.Images[0].Ref)
	require.NotNil(t, m.Requires.Images[0].Build)
	assert.Equal(t, "images/sandbox/Dockerfile", m.Requires.Images[0].Build.Dockerfile)
	assert.Equal(t, []string{"github_token"}, m.Requires.Images[0].Build.Secrets)
	assert.Equal(t, []string{"dmcp"}, m.Requires.Images[1].Build.BuildContexts)
}

func TestRequires_ChannelsRoundTripThroughYAML(t *testing.T) {
	// ParseManifest is the production loader (Bundle.FromFolder, unpack, and
	// every install path all go through it) so the round trip must exercise it
	// rather than a bare yaml.Unmarshal — see note in the task brief about
	// sigs.k8s.io/yaml's Unmarshal being lenient on unknown fields: the
	// discriminating proof here is that `m.Requires.Channels` does not compile
	// without the new field, not a decode-time error.
	const src = `
oapFormatVersion: "1"
agent: {name: demo-agent, version: "1.0.0"}
requires:
  channels:
    - kind: github
      role: input
      name: demo-agent-gh
      purpose: "Receives pull-request webhooks."
`
	m, err := ParseManifest([]byte(src))
	require.NoError(t, err)
	require.Len(t, m.Requires.Channels, 1)
	assert.Equal(t, "github", m.Requires.Channels[0].Kind)
	assert.Equal(t, "input", m.Requires.Channels[0].Role)
	assert.Equal(t, "demo-agent-gh", m.Requires.Channels[0].Name)
	assert.Equal(t, "Receives pull-request webhooks.", m.Requires.Channels[0].Purpose)
}

func TestValidateRejectsIncompleteImageBuild(t *testing.T) {
	cases := []struct{ name, build string }{
		{"no dockerfile: error", `{ context: images/x }`},
		{"no context: error", `{ dockerfile: images/x/Dockerfile }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseManifest([]byte(
				"oapFormatVersion: \"1\"\nagent: { name: x, version: \"1.0.0\" }\n" +
					"requires: { images: [ { ref: r:dev, build: " + tc.build + " } ] }\n"))
			require.NoError(t, err)
			assert.Error(t, m.Validate())
		})
	}
}
