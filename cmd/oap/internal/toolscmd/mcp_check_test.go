package toolscmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
)

// runMCPCLI executes the root cobra command with "tools mcp" prepended
// to args. Mirrors runToolspecCLI for the `oap tools mcp` sub-tree.
func runMCPCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	cmd := newToolsCmd(t)
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(append([]string{"mcp"}, args...))
	code := 0
	if err := cmd.Execute(); err != nil {
		code = 1
		fmt.Fprintln(&errb, err)
	}
	return code, out.String(), errb.String()
}

func TestMCPCheck(t *testing.T) {
	cases := []struct {
		name          string
		spec          string
		wantCode      int
		wantInStdout  []string
		notInStdout   []string
		assertNonzero bool
	}{
		{
			name: "valid spec: exit 0, stdout reports ok",
			spec: `
name: example
version: "1"
server:
  url: https://example.com/mcp
  transport: streamable-http
tools:
  - name: alpha
`,
			wantCode:     0,
			wantInStdout: []string{`ok: spec "example"`},
		},
		{
			name: "malformed CEL constraint: non-zero exit",
			spec: `
name: example
version: "1"
server:
  url: https://example.com/mcp
  transport: streamable-http
tools:
  - name: alpha
    args:
      constraints:
        - cel: "args.foo (bogus"
`,
			assertNonzero: true,
		},
		{
			name: "unenforceable deny.effects.writes: exit 0 with warning",
			spec: `
name: example
version: "1"
server:
  url: https://example.com/mcp
  transport: streamable-http
tools:
  - name: alpha
    deny:
      effects:
        writes: ["network"]
`,
			wantCode:     0,
			wantInStdout: []string{"warning", "Writes is not enforceable"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			specPath := filepath.Join(dir, "spec.yaml")
			aptest.WriteFile(t, dir, "spec.yaml", tc.spec)
			code, stdout, stderr := runMCPCLI(t, "check", specPath)
			if tc.assertNonzero {
				require.NotEqualf(t, 0, code, "expected non-zero exit; stdout=%q stderr=%q", stdout, stderr)
				return
			}
			require.Equalf(t, tc.wantCode, code, "exit code; stdout=%q stderr=%q", stdout, stderr)
			for _, want := range tc.wantInStdout {
				assert.Contains(t, stdout, want, "stdout should contain")
			}
		})
	}
}

// runMCPCheck marshals sp to YAML, writes it to a temp file, and
// invokes "oap tools mcp check <path>". Returns stdout.
func runMCPCheck(t *testing.T, sp *mcpspec.Spec) string {
	t.Helper()
	raw, err := yaml.Marshal(sp)
	require.NoError(t, err, "marshal spec to YAML")
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.yaml")
	aptest.WriteFile(t, dir, "spec.yaml", string(raw))
	_, stdout, _ := runMCPCLI(t, "check", specPath)
	return stdout
}

func TestMCPCheck_SurfacesTrustAndDenyTrust(t *testing.T) {
	sp := &mcpspec.Spec{
		Name: "s", Version: "1",
		Server: mcpspec.Server{URL: "https://x.example", Transport: mcpspec.TransportStreamableHTTP},
		Tools: []mcpspec.Tool{{
			Name: "send_msg",
			Args: mcpspec.Args{AllowedFields: []string{}},
			Trust: mcpspec.Trust{
				InputMetadata:  json.RawMessage(`{"outcomes":["irreversible"]}`),
				ReturnMetadata: json.RawMessage(`{"source":"internal"}`),
			},
			Deny: mcpspec.Deny{Trust: mcpspec.DenyTrust{
				OutcomesIrreversible: true,
				DestinationPublic:    true, // declared but Trust doesn't carry destination — MISS
			}},
		}},
	}
	out := runMCPCheck(t, sp)
	assert.Contains(t, out, "trust.outcomesIrreversible", "deny.trust HIT must render")
	assert.Contains(t, out, "HIT", "HIT marker visible for outcomesIrreversible")
	assert.Contains(t, out, "trust.destinationPublic", "deny.trust MISS row still rendered")
	assert.Contains(t, out, "MISS", "MISS marker visible for destinationPublic")
}
