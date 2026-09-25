package toolscmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
)

func TestMCPExplain(t *testing.T) {
	cases := []struct {
		name         string
		spec         string
		args         string
		wantInStdout []string
		notInStdout  []string
	}{
		{
			name: "args within allowedFields: Allow=true, tool-pass trace",
			spec: `
name: example
version: "1"
server:
  url: https://example.com/mcp
  transport: streamable-http
tools:
  - name: alpha
    args:
      allowedFields: ["foo"]
`,
			args:         `{"foo": "bar"}`,
			wantInStdout: []string{"Allow: true", "[pass] tool"},
		},
		{
			name: "args outside allowedFields: Allow=false, FailedOn=allowedFields",
			spec: `
name: example
version: "1"
server:
  url: https://example.com/mcp
  transport: streamable-http
tools:
  - name: alpha
    args:
      allowedFields: ["foo"]
`,
			args:         `{"foo": "x", "baz": "y"}`,
			wantInStdout: []string{"Allow: false", "FailedOn: allowedFields"},
		},
		{
			name: "sensitive field redacted: value never appears in output",
			spec: `
name: example
version: "1"
server:
  url: https://example.com/mcp
  transport: streamable-http
tools:
  - name: alpha
    args:
      sensitiveFields: ["apiKey"]
      constraints:
        - cel: 'args.apiKey == "right"'
          message: "wrong apiKey"
`,
			args:        `{"apiKey": "DO-NOT-LEAK"}`,
			notInStdout: []string{"DO-NOT-LEAK"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			specPath := aptest.WriteFile(t, dir, "spec.yaml", tc.spec)
			code, stdout, stderr := runMCPCLI(t,
				"explain",
				"--spec", specPath,
				"--tool", "alpha",
				"--args", tc.args,
			)
			require.Equalf(t, 0, code, "exit code; stderr=%q", stderr)
			for _, want := range tc.wantInStdout {
				assert.Contains(t, stdout, want, "stdout should contain")
			}
			for _, banned := range tc.notInStdout {
				assert.NotContains(t, stdout, banned, "stdout should not leak")
			}
		})
	}
}
