package toolscmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTempValidate(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "spec.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600), "writeTempValidate")
	return p
}

func TestToolsValidate(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantErr   bool
		wantInOut string
	}{
		{
			name: "flat MCPServer with valid CEL: OK",
			body: `
name: ok
version: "1"
server: { url: https://x, transport: streamable-http }
tools:
  - name: t1
    args:
      constraints:
        - cel: 'args.q.size() < 100'
`,
			wantInOut: "OK",
		},
		{
			name: "flat MCPServer with malformed CEL: ERROR",
			body: `
name: bad
version: "1"
server: { url: https://x, transport: streamable-http }
tools:
  - name: t
    args:
      constraints:
        - cel: 'args.q.size( < 100'
`,
			wantErr:   true,
			wantInOut: "ERROR",
		},
		{
			name: "flat SpiceboxToolspec: OK",
			body: `
name: s
version: "1"
toolkit: {name: t, revision: "r"}
allowSubcommands: [say]
constraints:
  - cel: 'call.subcommand == "say"'
`,
			wantInOut: "OK",
		},
		{
			name: "unknown kind: errors",
			body: `
something: unrecognized
fields: yes
`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempValidate(t, tc.body)
			out, err := runTools(t, "validate", "-f", path)
			if tc.wantErr {
				require.Errorf(t, err, "expected error; out=%s", out)
			} else {
				require.NoErrorf(t, err, "unexpected error; out=%s", out)
			}
			if tc.wantInOut != "" {
				assert.Contains(t, out, tc.wantInOut, "out should contain expected fragment")
			}
		})
	}
}
