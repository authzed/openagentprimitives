package toolkit

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envDefaultsBody wraps an envDefaults stanza in the minimal valid toolkit
// shape, so the only variable across cases is the stanza itself.
func envDefaultsBody(t *testing.T, stanza string) []byte {
	t.Helper()
	return []byte(`
name: x
version: "1"
toolkitRevision: "r"
target: {binary: x}
parser: {kind: declarative}
env: {allowed: [{name: X_TOKEN, sensitive: true, credential: x-token}]}
subcommands: []
` + stanza)
}

func TestLoadBytes_EnvDefaults(t *testing.T) {
	cases := []struct {
		name    string
		stanza  string
		wantErr string // "" means LoadBytes must succeed
		want    map[string]string
	}{
		{
			name:   "absent envDefaults: loads with a nil map, no empty-map surprise",
			stanza: "",
			want:   nil,
		},
		{
			name:   "declared envDefaults: parsed verbatim, value stays a string",
			stanza: "envDefaults:\n  X_MAX_RETRIES: \"0\"\n",
			want:   map[string]string{"X_MAX_RETRIES": "0"},
		},
		{
			name:    "key that is a sensitive env.allowed name: rejected, error names the key",
			stanza:  "envDefaults:\n  X_TOKEN: \"nope\"\n",
			wantErr: "X_TOKEN",
		},
		{
			name:    "key outside the POSIX env-name grammar: rejected, error names the key",
			stanza:  "envDefaults:\n  \"NOT-A-NAME\": \"v\"\n",
			wantErr: "NOT-A-NAME",
		},
		{
			name:    "empty key: rejected",
			stanza:  "envDefaults:\n  \"\": \"v\"\n",
			wantErr: "envDefaults",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tk, err := LoadBytes(envDefaultsBody(t, tc.stanza))
			if tc.wantErr != "" {
				require.Error(t, err, "expected an error containing %q", tc.wantErr)
				assert.ErrorContains(t, err, tc.wantErr, "error message")
				return
			}
			require.NoError(t, err, "LoadBytes must succeed")
			assert.Equal(t, tc.want, tk.EnvDefaults, "EnvDefaults")
		})
	}
}

// TestToolkit_EnvDefaults_JSONRoundTrip pins the json tag, which is the
// contract SpiceboxToolkitSpec.ToToolkit round-trips through: a mismatch
// there silently drops a CR-declared envDefaults block.
func TestToolkit_EnvDefaults_JSONRoundTrip(t *testing.T) {
	in := Toolkit{
		Name: "x", Version: "1", ToolkitRevision: "r",
		Target:      Target{Binary: "x"},
		Parser:      ParserConfig{Kind: "declarative"},
		EnvDefaults: map[string]string{"X_MAX_RETRIES": "0"},
	}
	data, err := json.Marshal(in)
	require.NoError(t, err, "marshal")
	assert.Contains(t, string(data), `"envDefaults":{"X_MAX_RETRIES":"0"}`, "json tag")

	var out Toolkit
	require.NoError(t, json.Unmarshal(data, &out), "unmarshal")
	assert.Equal(t, in.EnvDefaults, out.EnvDefaults, "EnvDefaults survives the round trip")

	in.EnvDefaults = nil
	data, err = json.Marshal(in)
	require.NoError(t, err, "marshal without envDefaults")
	assert.NotContains(t, string(data), "envDefaults", "omitempty keeps an undeclared block out of the CR")
}
