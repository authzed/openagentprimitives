package desktop_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
)

func TestRewriteKubeconfigServer(t *testing.T) {
	in := []byte(`apiVersion: v1
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: default
`)
	out, err := desktop.RewriteKubeconfigServer(in, "192.168.64.7")
	require.NoError(t, err)
	assert.Contains(t, string(out), "server: https://192.168.64.7:6443")
	assert.NotContains(t, string(out), "127.0.0.1")
}

func TestRewriteKubeconfigServer_RejectsEmptyIP(t *testing.T) {
	_, err := desktop.RewriteKubeconfigServer([]byte("server: https://127.0.0.1:6443"), "")
	require.Error(t, err)
}
