package dotenv_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/dotenv"
)

func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestRead(t *testing.T) {
	cases := []struct {
		name    string
		content string
		key     string
		want    string
	}{
		{
			name:    "happy path: bare KEY=VAL returns value",
			content: "FOO=bar\nBAZ=qux\n",
			key:     "FOO",
			want:    "bar",
		},
		{
			name:    "value containing equals signs is preserved verbatim",
			content: "TOKEN=abc=def==xyz\n",
			key:     "TOKEN",
			want:    "abc=def==xyz",
		},
		{
			name:    "double-quoted value has quotes stripped",
			content: `API_KEY="my-secret-key"` + "\n",
			key:     "API_KEY",
			want:    "my-secret-key",
		},
		{
			name:    "single-quoted value has quotes stripped",
			content: "API_KEY='my-secret-key'\n",
			key:     "API_KEY",
			want:    "my-secret-key",
		},
		{
			name: "blank lines and comments are skipped",
			content: `
# this is a comment
FIRST=one

# another comment
SECOND=two
`,
			key:  "SECOND",
			want: "two",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeEnvFile(t, tc.content)
			got, err := dotenv.Read(path, tc.key)
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(got))
		})
	}
}

func TestRead_MissingKey(t *testing.T) {
	path := writeEnvFile(t, "FOO=bar\n")
	_, err := dotenv.Read(path, "MISSING")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `dotenv: key "MISSING" not found in`)
}

func TestRead_MissingFile(t *testing.T) {
	_, err := dotenv.Read("/nonexistent/path/.env", "FOO")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dotenv: read /nonexistent/path/.env:")
}
