package toolscmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
)

func TestMCPTestCmd(t *testing.T) {
	cases := []struct {
		name         string
		spec         string
		wantNonzero  bool
		wantInStdout []string
	}{
		{
			name: "all cases pass: exit 0, '2/2 passed'",
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
generation:
  testCases:
    - intent: foo allowed
      toolName: alpha
      args: {foo: bar}
      expectedAllow: true
    - intent: baz rejected
      toolName: alpha
      args: {baz: x}
      expectedAllow: false
`,
			wantInStdout: []string{"2/2 passed"},
		},
		{
			name: "expectation mismatch: non-zero exit, names failing intent",
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
generation:
  testCases:
    - intent: should-fail
      toolName: alpha
      args: {baz: x}
      expectedAllow: true
`,
			wantNonzero:  true,
			wantInStdout: []string{"should-fail"},
		},
		{
			name: "empty generation block: exit 0 with no-cases notice",
			spec: `
name: example
version: "1"
server:
  url: https://example.com/mcp
  transport: streamable-http
tools:
  - name: alpha
`,
			wantInStdout: []string{"no test cases to run"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			specPath := aptest.WriteFile(t, dir, "spec.yaml", tc.spec)
			code, stdout, _ := runMCPCLI(t, "test", specPath)
			if tc.wantNonzero {
				require.NotEqualf(t, 0, code, "expected non-zero exit; stdout=%q", stdout)
			} else {
				require.Equalf(t, 0, code, "exit code; stdout=%q", stdout)
			}
			for _, want := range tc.wantInStdout {
				assert.Contains(t, stdout, want, "stdout should contain")
			}
		})
	}
}
