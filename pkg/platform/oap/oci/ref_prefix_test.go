package oci

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestRefHasPrefix(t *testing.T) {
	const prefix = "ghcr.io/acme/agents/"
	cases := []struct {
		name    string
		ref     string
		want    bool
		wantErr bool
	}{
		{name: "tagged ref under prefix: true", ref: "ghcr.io/acme/agents/x:1", want: true},
		{name: "digest ref in nested path under prefix: true", ref: "ghcr.io/acme/agents/sub/x@" + testDigest, want: true},
		{name: "sibling repo sharing a string prefix: false", ref: "ghcr.io/acme/agents-evil/x", want: false},
		{name: "repo whose name extends the prefix dir: false", ref: "ghcr.io/acme/agentsx", want: false},
		{name: "explicit :443 is the same host: true", ref: "ghcr.io:443/acme/agents/x", want: true},
		{name: "zero-padded :0443 is the same host: true", ref: "ghcr.io:0443/acme/agents/x", want: true},
		{name: "trailing-dot FQDN is the same host: true", ref: "ghcr.io./acme/agents/x", want: true},
		{name: "trailing dot plus :443 is the same host: true", ref: "ghcr.io.:443/acme/agents/x", want: true},
		{name: "uppercase host is the same host: true", ref: "GHCR.IO/acme/agents/x", want: true},
		{name: "non-default port is a different host: false", ref: "ghcr.io:8443/acme/agents/x", want: false},
		{name: "double trailing dot has no canonical form: error", ref: "ghcr.io../acme/agents/x", wantErr: true},
		{name: "different registry: false", ref: "docker.io/acme/agents/x", want: false},
		{name: "shorter repo outside prefix: false", ref: "ghcr.io/acme/x", want: false},
		{name: "repository equal to the prefix directory matches (intentional)", ref: "ghcr.io/acme/agents", want: true},
		{name: "unparseable ref: error", ref: "::bad", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RefHasPrefix(tc.ref, prefix)
			if tc.wantErr {
				require.Error(t, err)
				assert.False(t, got, "an error must never come with true")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("invalid prefix: error", func(t *testing.T) {
		got, err := RefHasPrefix("ghcr.io/acme/agents/x", "ghcr.io/acme/agents")
		require.Error(t, err)
		assert.False(t, got)
	})
}

func TestValidatePrefix(t *testing.T) {
	cases := []struct {
		prefix  string
		wantErr bool
	}{
		{prefix: "ghcr.io/acme/"},
		{prefix: "ghcr.io/acme/agents/"},
		{prefix: "localhost:5000/acme/"},
		{prefix: "GHCR.io/acme/", wantErr: true},
		{prefix: "ghcr.io:443/a/", wantErr: true},
		{prefix: "ghcr.io:0443/a/", wantErr: true},
		{prefix: "ghcr.io./a/", wantErr: true},
		{prefix: "ghcr.io.:443/a/", wantErr: true},
		{prefix: "ghcr.io:8443/a/"},
		{prefix: "ghcr.io/acme", wantErr: true},
		{prefix: "https://ghcr.io/acme/", wantErr: true},
		{prefix: "ghcr.io/", wantErr: true},
		{prefix: "ghcr.io/acme:1/", wantErr: true},
		{prefix: "ghcr.io/acme@" + testDigest + "/", wantErr: true},
		{prefix: "ghcr.io/acme//x/", wantErr: true},
		{prefix: "/acme/", wantErr: true},
		{prefix: "", wantErr: true},
	}
	for _, tc := range cases {
		name := tc.prefix
		if tc.wantErr {
			name += ": error"
		} else {
			name += ": ok"
		}
		t.Run(name, func(t *testing.T) {
			err := ValidatePrefix(tc.prefix)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCanonicalHost(t *testing.T) {
	cases := []struct {
		host, want string
		wantErr    bool
	}{
		{host: "ghcr.io", want: "ghcr.io"},
		{host: "GHCR.IO", want: "ghcr.io"},
		{host: "ghcr.io.", want: "ghcr.io"},
		{host: "ghcr.io:443", want: "ghcr.io"},
		{host: "ghcr.io.:443", want: "ghcr.io"},
		{host: "localhost:5000", want: "localhost:5000"},
		{host: "[::1]:443", want: "[::1]"},
		{host: "[::1]:5000", want: "[::1]:5000"},
		{host: "ghcr.io..", wantErr: true},
		{host: ".", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			got, err := canonicalHost(tc.host)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
