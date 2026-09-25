package spec

import (
	"testing"
	"time"

	toolspec "github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_HappyPath(t *testing.T) {
	sp, err := Load("testdata/linear-readonly.yaml")
	require.NoError(t, err, "Load")

	assert.Equal(t, "linear-readonly", sp.Name)
	assert.Equal(t, "1", sp.Version)
	assert.Equal(t, "https://mcp.linear.example/v1", sp.Server.URL)
	assert.Equal(t, "streamable-http", sp.Server.Transport)
	// Defaults are loaded verbatim, not auto-applied here.
	assert.Equal(t, "Authorization", sp.Auth.Header)
	assert.Equal(t, 60*time.Second, sp.CallTimeout.Duration)

	require.Len(t, sp.Tools, 2, "Tools len")
	t0 := sp.Tools[0]
	assert.Equal(t, "search_issues", t0.Name)
	require.Len(t, t0.Args.AllowedFields, 2, "Tools[0].Args.AllowedFields len")
	assert.Equal(t, "teamId", t0.Args.AllowedFields[0])
	require.Len(t, t0.Args.Constraints, 1, "Tools[0].Args.Constraints len")
	assert.NotEmpty(t, t0.Args.Constraints[0].CEL, "Tools[0].Args.Constraints[0].CEL")
	// DenyEffects reused verbatim from toolspec.
	var _ toolspec.DenyEffects = t0.Deny.Effects
	assert.True(t, t0.Deny.Effects.Creds.Writes, "Tools[0].Deny.Effects.Creds.Writes")
}

func TestLoadBytes_ToolEffectsRoundTrip(t *testing.T) {
	data := []byte(`
name: example
version: "1"
server:
  url: https://example.com/mcp
  transport: streamable-http
tools:
  - name: post_thing
    effects:
      destructive: true
      readOnly: false
      idempotent: false
      openWorld: true
`)
	sp, err := LoadBytes(data)
	require.NoError(t, err, "LoadBytes")
	require.Len(t, sp.Tools, 1, "Tools len")

	want := Effects{Destructive: true, ReadOnly: false, Idempotent: false, OpenWorld: true}
	assert.Equal(t, want, sp.Tools[0].Effects)
}

func TestLoadBytes_GenerationTestCasesRoundTrip(t *testing.T) {
	data := []byte(`
name: example
version: "1"
server:
  url: https://example.com/mcp
  transport: streamable-http
tools:
  - name: alpha
generation:
  testCases:
    - intent: "happy path"
      toolName: alpha
      args:
        foo: bar
      expectedAllow: true
    - intent: "rejects extra field"
      toolName: alpha
      args:
        bogus: x
      expectedAllow: false
`)
	sp, err := LoadBytes(data)
	require.NoError(t, err, "LoadBytes")
	require.NotNil(t, sp.Generation, "Generation set")
	require.Len(t, sp.Generation.TestCases, 2, "Generation.TestCases len")

	assert.Equal(t, "happy path", sp.Generation.TestCases[0].Intent)
	assert.True(t, sp.Generation.TestCases[0].ExpectedAllow, "TestCases[0].ExpectedAllow")
	assert.Equal(t, "alpha", sp.Generation.TestCases[1].ToolName)
	assert.False(t, sp.Generation.TestCases[1].ExpectedAllow, "TestCases[1].ExpectedAllow")
}

func TestLoadBytes_PreservesTrustAndDenyTrust(t *testing.T) {
	yamlSrc := []byte(`
name: t
version: "1"
intent: test fixture
server:
  url: https://example.com/mcp
  transport: streamable-http
tools:
  - name: send_msg
    intent: send a message
    args:
      allowedFields: [to, body]
    trust:
      maliciousActivityHint: true
      attribution: ["mcp://example/src"]
      inputMetadata: {destination: ["public"], outcomes: ["irreversible"]}
      returnMetadata: {source: "internal"}
    deny:
      trust:
        outcomesIrreversible: true
        destinationPublic: true
`)
	sp, err := LoadBytes(yamlSrc)
	require.NoError(t, err)
	require.Len(t, sp.Tools, 1)
	tr := sp.Tools[0].Trust
	assert.True(t, tr.MaliciousActivityHint)
	assert.Equal(t, []string{"mcp://example/src"}, tr.Attribution)
	assert.NotEmpty(t, tr.InputMetadata)
	assert.NotEmpty(t, tr.ReturnMetadata)
	d := sp.Tools[0].Deny.Trust
	assert.True(t, d.OutcomesIrreversible)
	assert.True(t, d.DestinationPublic)
	assert.False(t, d.SourceUntrustedPublic)
}

func TestLoad_StructuralErrors(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		wantSub string
	}{
		{
			name:    "missing top-level name is rejected",
			path:    "testdata/missing-name.yaml",
			wantSub: "name is required",
		},
		{
			name:    "missing server.url is rejected",
			path:    "testdata/missing-server.yaml",
			wantSub: "server.url is required",
		},
		{
			name:    "duplicate tool name is rejected with path to duplicate",
			path:    "testdata/duplicate-tool-name.yaml",
			wantSub: `duplicates tools[0].name`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(tc.path)
			require.Error(t, err, "Load %s must error", tc.path)
			assert.Contains(t, err.Error(), tc.wantSub)
		})
	}
}
