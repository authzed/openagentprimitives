package links

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	const ref = "ghcr.io/acme/agents/demo-agent:1.0"
	cases := []struct {
		name    string
		raw     string
		wantErr error
		wantRef string
	}{
		{name: "valid install link: ok", raw: "oap://install/agent?v=1&ref=" + ref, wantRef: ref},
		{name: "percent-encoded ref: ok and decoded", raw: "oap://install/agent?v=1&ref=ghcr.io%2Facme%2Fagents%2Fdemo-agent%3A1.0", wantRef: ref},
		{name: "https scheme: invalid", raw: "https://install/agent?v=1&ref=x", wantErr: ErrInvalid},
		{name: "opaque form: invalid", raw: "oap:install/agent?v=1&ref=x", wantErr: ErrInvalid},
		{name: "userinfo: invalid", raw: "oap://u@install/agent?v=1&ref=x", wantErr: ErrInvalid},
		{name: "over MaxLen: invalid", raw: "oap://install/agent?v=1&ref=" + strings.Repeat("a", MaxLen), wantErr: ErrInvalid},
		{name: "missing v: invalid", raw: "oap://install/agent?ref=x", wantErr: ErrInvalid},
		{name: "v=01: invalid", raw: "oap://install/agent?v=01&ref=x", wantErr: ErrInvalid},
		{name: "v=1.0: invalid", raw: "oap://install/agent?v=1.0&ref=x", wantErr: ErrInvalid},
		{name: "v=0: invalid", raw: "oap://install/agent?v=0&ref=x", wantErr: ErrInvalid},
		{name: "v=-1: invalid", raw: "oap://install/agent?v=-1&ref=x", wantErr: ErrInvalid},
		{name: "v repeated: invalid", raw: "oap://install/agent?v=1&v=1&ref=x", wantErr: ErrInvalid},
		{name: "v=2: unsupported", raw: "oap://install/agent?v=2&ref=x", wantErr: ErrUnsupported},
		{name: "unknown key: unsupported", raw: "oap://open/session/x?v=1", wantErr: ErrUnsupported},
		{name: "uppercase key is case-sensitive: unsupported", raw: "oap://INSTALL/agent?v=1&ref=x", wantErr: ErrUnsupported},
		{name: "ref missing: invalid", raw: "oap://install/agent?v=1", wantErr: ErrInvalid},
		{name: "ref empty: invalid", raw: "oap://install/agent?v=1&ref=", wantErr: ErrInvalid},
		{name: "ref twice: invalid", raw: "oap://install/agent?v=1&ref=a&ref=b", wantErr: ErrInvalid},
		{name: "extra param: invalid", raw: "oap://install/agent?v=1&ref=x&foo=1", wantErr: ErrInvalid},
		{name: "fragment: invalid", raw: "oap://install/agent?v=1&ref=x#frag", wantErr: ErrInvalid},
		{name: "empty fragment marker: invalid", raw: "oap://install/agent?v=1&ref=x#", wantErr: ErrInvalid},
		{name: "malformed query escape: invalid", raw: "oap://install/agent?v=1&ref=%zz", wantErr: ErrInvalid},
		{name: "empty string: invalid", raw: "", wantErr: ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.raw)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, KeyInstallAgent, got.Key)
			assert.Equal(t, 1, got.Version)
			assert.Equal(t, tc.wantRef, got.Params["ref"])
			assert.Equal(t, tc.raw, got.Raw)
		})
	}
}

func TestParseErrorsAreDistinct(t *testing.T) {
	_, err := Parse("oap://install/agent?v=2&ref=x")
	require.ErrorIs(t, err, ErrUnsupported)
	assert.NotErrorIs(t, err, ErrInvalid)
}

func TestKeys(t *testing.T) {
	assert.Contains(t, Keys(), KeyInstallAgent)
	keys := Keys()
	for i := 1; i < len(keys); i++ {
		assert.Less(t, keys[i-1], keys[i], "Keys must be sorted")
	}
}

func TestRegisterPanicsOnDuplicate(t *testing.T) {
	assert.Panics(t, func() { Register(Spec{Key: KeyInstallAgent, MaxVersion: 1}) })
}
