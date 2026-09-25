package desktop_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
)

// vmFixture returns a VM kubeconfig carrying the single "default"
// cluster/user/context entries MergeVMContext expects, built directly from
// clientcmdapi types rather than a YAML literal.
func vmFixture(t *testing.T) []byte {
	t.Helper()
	vm := clientcmdapi.NewConfig()
	vm.Clusters["default"] = &clientcmdapi.Cluster{Server: "https://192.168.64.5:6443"}
	vm.AuthInfos["default"] = &clientcmdapi.AuthInfo{Token: "vm-token"}
	vm.Contexts["default"] = &clientcmdapi.Context{Cluster: "default", AuthInfo: "default"}
	vm.CurrentContext = "default"
	b, err := clientcmd.Write(*vm)
	require.NoError(t, err)
	return b
}

// vmKubeconfigYAML is a small fixture mirroring what the VM stages: a
// single cluster/user/context all named "default" (see
// cmd/oap/internal/desktopcmd/run_darwin.go's writeKubeconfig).
const vmKubeconfigYAML = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://192.168.64.5:6443
    certificate-authority-data: dm0tY2E=
  name: default
users:
- name: default
  user:
    client-certificate-data: dm0tY2VydA==
    client-key-data: dm0ta2V5
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
`

// vmKubeconfigYAMLv2 has the same shape as vmKubeconfigYAML but a different
// server (simulating the VM getting a new guest IP after a restage), used to
// exercise re-merge idempotency.
const vmKubeconfigYAMLv2 = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://192.168.64.9:6443
    certificate-authority-data: dm0tY2Etdji=
  name: default
users:
- name: default
  user:
    client-certificate-data: dm0tY2VydC12Mg==
    client-key-data: dm0ta2V5LXYy
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
`

// cloudUserKubeconfigYAML is a fixture standing in for a user's existing
// ~/.kube/config pointed at some other (e.g. cloud) cluster, which
// MergeVMContext must preserve untouched.
const cloudUserKubeconfigYAML = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://cloud.example.com
    certificate-authority-data: Y2xvdWQtY2E=
  name: cloud
users:
- name: cloud-user
  user:
    token: cloud-token
contexts:
- context:
    cluster: cloud
    user: cloud-user
  name: cloud-context
current-context: cloud-context
`

func TestMergeVMContext(t *testing.T) {
	cases := []struct {
		name             string
		userKubeconfig   []byte
		wantOtherCluster string // non-empty: assert this cluster name is still present
	}{
		{
			name:           "empty user config -> merge starts from a fresh config",
			userKubeconfig: nil,
		},
		{
			name:             "existing cloud context is preserved",
			userKubeconfig:   []byte(cloudUserKubeconfigYAML),
			wantOtherCluster: "cloud",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := desktop.MergeVMContext(tc.userKubeconfig, []byte(vmKubeconfigYAML))
			require.NoError(t, err)

			merged, err := clientcmd.Load(out)
			require.NoError(t, err)

			assert.Equal(t, desktop.APContextName, merged.CurrentContext)
			require.Contains(t, merged.Clusters, desktop.APContextName)
			assert.Equal(t, "https://192.168.64.5:6443", merged.Clusters[desktop.APContextName].Server)
			require.Contains(t, merged.AuthInfos, desktop.APContextName)
			require.Contains(t, merged.Contexts, desktop.APContextName)
			assert.Equal(t, desktop.APContextName, merged.Contexts[desktop.APContextName].Cluster)
			assert.Equal(t, desktop.APContextName, merged.Contexts[desktop.APContextName].AuthInfo)

			if tc.wantOtherCluster != "" {
				assert.Contains(t, merged.Clusters, tc.wantOtherCluster, "pre-existing cluster must be preserved")
				assert.Contains(t, merged.Contexts, "cloud-context", "pre-existing context must be preserved")
			}
		})
	}
}

func TestMergeVMContext_ReMergeIsIdempotent(t *testing.T) {
	first, err := desktop.MergeVMContext([]byte(cloudUserKubeconfigYAML), []byte(vmKubeconfigYAML))
	require.NoError(t, err)

	second, err := desktop.MergeVMContext(first, []byte(vmKubeconfigYAMLv2))
	require.NoError(t, err)

	merged, err := clientcmd.Load(second)
	require.NoError(t, err)

	assert.Len(t, merged.Clusters, 2, "re-merge must overwrite the oap-desktop entry, not duplicate it")
	assert.Len(t, merged.AuthInfos, 2)
	assert.Len(t, merged.Contexts, 2)
	assert.Equal(t, desktop.APContextName, merged.CurrentContext)
	assert.Equal(t, "https://192.168.64.9:6443", merged.Clusters[desktop.APContextName].Server,
		"the second merge's VM server must win")
	assert.Contains(t, merged.Clusters, "cloud", "the unrelated cloud cluster must survive both merges")
}

func TestMergeVMContext_MalformedInput(t *testing.T) {
	cases := []struct {
		name           string
		userKubeconfig []byte
		vmKubeconfig   []byte
	}{
		{
			name:           "malformed user kubeconfig",
			userKubeconfig: []byte("not: [valid yaml"),
			vmKubeconfig:   []byte(vmKubeconfigYAML),
		},
		{
			name:           "malformed VM kubeconfig",
			userKubeconfig: []byte(cloudUserKubeconfigYAML),
			vmKubeconfig:   []byte("not: [valid yaml"),
		},
		{
			name:           "VM kubeconfig missing the default cluster/user/context",
			userKubeconfig: nil,
			vmKubeconfig:   []byte("apiVersion: v1\nkind: Config\n"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := desktop.MergeVMContext(tc.userKubeconfig, tc.vmKubeconfig)
			require.Error(t, err)
		})
	}
}

func TestIsVMContextCurrent(t *testing.T) {
	apDesktopCurrent, err := desktop.MergeVMContext(nil, []byte(vmKubeconfigYAML))
	require.NoError(t, err)

	cases := []struct {
		name           string
		userKubeconfig []byte
		want           bool
	}{
		{
			name:           "current-context is ap-desktop -> true",
			userKubeconfig: apDesktopCurrent,
			want:           true,
		},
		{
			name:           "current-context is something else -> false",
			userKubeconfig: []byte(cloudUserKubeconfigYAML),
			want:           false,
		},
		{
			name:           "empty kubeconfig -> false",
			userKubeconfig: nil,
			want:           false,
		},
		{
			name:           "malformed kubeconfig -> false",
			userKubeconfig: []byte("not: [valid yaml"),
			want:           false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, desktop.IsVMContextCurrent(tc.userKubeconfig))
		})
	}
}

// TestMergeVMContext_MigratesLegacyEntries covers the rename's migration
// contract: legacy ap-desktop cluster/user/context entries are deleted (not
// left dangling alongside the new oap-desktop ones), a legacy current-context
// is repointed to the new name, and unrelated contexts survive untouched.
func TestMergeVMContext_MigratesLegacyEntries(t *testing.T) {
	user := clientcmdapi.NewConfig()
	user.Clusters["ap-desktop"] = &clientcmdapi.Cluster{Server: "https://old:6443"}
	user.AuthInfos["ap-desktop"] = &clientcmdapi.AuthInfo{Token: "old"}
	user.Contexts["ap-desktop"] = &clientcmdapi.Context{Cluster: "ap-desktop", AuthInfo: "ap-desktop"}
	user.Contexts["other"] = &clientcmdapi.Context{Cluster: "c", AuthInfo: "u"}
	user.CurrentContext = "ap-desktop"
	userBytes, err := clientcmd.Write(*user)
	require.NoError(t, err)

	merged, err := desktop.MergeVMContext(userBytes, vmFixture(t))
	require.NoError(t, err)
	got, err := clientcmd.Load(merged)
	require.NoError(t, err)

	assert.Equal(t, "oap-desktop", got.CurrentContext)
	assert.Contains(t, got.Contexts, "oap-desktop")
	assert.NotContains(t, got.Contexts, "ap-desktop")
	assert.NotContains(t, got.Clusters, "ap-desktop")
	assert.NotContains(t, got.AuthInfos, "ap-desktop")
	assert.Contains(t, got.Contexts, "other", "foreign contexts must survive")
}

// TestMergeVMContext_BothPresent_LegacyDeletedNewRewritten covers the case
// where the user config already carries BOTH a legacy ap-desktop entry and a
// (stale) oap-desktop entry: the legacy one is deleted outright and the new
// one is overwritten with the fresh VM data, not left stale.
func TestMergeVMContext_BothPresent_LegacyDeletedNewRewritten(t *testing.T) {
	user := clientcmdapi.NewConfig()
	user.Clusters["ap-desktop"] = &clientcmdapi.Cluster{Server: "https://old:6443"}
	user.AuthInfos["ap-desktop"] = &clientcmdapi.AuthInfo{Token: "old"}
	user.Contexts["ap-desktop"] = &clientcmdapi.Context{Cluster: "ap-desktop", AuthInfo: "ap-desktop"}
	user.Clusters["oap-desktop"] = &clientcmdapi.Cluster{Server: "https://stale-new:6443"}
	user.AuthInfos["oap-desktop"] = &clientcmdapi.AuthInfo{Token: "stale-new"}
	user.Contexts["oap-desktop"] = &clientcmdapi.Context{Cluster: "oap-desktop", AuthInfo: "oap-desktop"}
	user.CurrentContext = "oap-desktop"
	userBytes, err := clientcmd.Write(*user)
	require.NoError(t, err)

	merged, err := desktop.MergeVMContext(userBytes, vmFixture(t))
	require.NoError(t, err)
	got, err := clientcmd.Load(merged)
	require.NoError(t, err)

	assert.NotContains(t, got.Contexts, "ap-desktop")
	assert.NotContains(t, got.Clusters, "ap-desktop")
	assert.NotContains(t, got.AuthInfos, "ap-desktop")
	require.Contains(t, got.Clusters, "oap-desktop")
	assert.NotEqual(t, "https://stale-new:6443", got.Clusters["oap-desktop"].Server,
		"the fresh VM entry must win over the stale pre-existing oap-desktop entry")
	assert.Equal(t, "oap-desktop", got.CurrentContext)
}

// TestMergeVMContext_HandEditedLegacyPartialRemoval covers a hand-edited
// legacy config where the ap-desktop context points at differently-named
// cluster/user entries: only the recognized ap-desktop context is removed,
// mirroring the repo's "remove, don't restore" rule for external state — we
// remove what we recognize as ours, and leave what we don't.
func TestMergeVMContext_HandEditedLegacyPartialRemoval(t *testing.T) {
	user := clientcmdapi.NewConfig()
	user.Clusters["my-cluster"] = &clientcmdapi.Cluster{Server: "https://handedited:6443"}
	user.AuthInfos["my-user"] = &clientcmdapi.AuthInfo{Token: "handedited"}
	user.Contexts["ap-desktop"] = &clientcmdapi.Context{Cluster: "my-cluster", AuthInfo: "my-user"}
	user.CurrentContext = "ap-desktop"
	userBytes, err := clientcmd.Write(*user)
	require.NoError(t, err)

	merged, err := desktop.MergeVMContext(userBytes, vmFixture(t))
	require.NoError(t, err)
	got, err := clientcmd.Load(merged)
	require.NoError(t, err)

	assert.NotContains(t, got.Contexts, "ap-desktop", "the legacy context entry is always removed")
	assert.Contains(t, got.Clusters, "my-cluster", "a differently-named cluster is not recognized as legacy and must survive")
	assert.Contains(t, got.AuthInfos, "my-user", "a differently-named user is not recognized as legacy and must survive")
	assert.Equal(t, "oap-desktop", got.CurrentContext)
}

// TestMergeVMContext_HandEditedLegacyMixedRefs covers the mixed hand-edited
// case: the legacy context references a foreign cluster name but the legacy
// user name. Only the references we recognize as ours are removed — the
// context entry itself and the ap-desktop-named user — while the
// differently-named cluster survives.
func TestMergeVMContext_HandEditedLegacyMixedRefs(t *testing.T) {
	user := clientcmdapi.NewConfig()
	user.Clusters["my-cluster"] = &clientcmdapi.Cluster{Server: "https://handedited:6443"}
	user.AuthInfos["ap-desktop"] = &clientcmdapi.AuthInfo{Token: "legacy-user"}
	user.Contexts["ap-desktop"] = &clientcmdapi.Context{Cluster: "my-cluster", AuthInfo: "ap-desktop"}
	user.CurrentContext = "ap-desktop"
	userBytes, err := clientcmd.Write(*user)
	require.NoError(t, err)

	merged, err := desktop.MergeVMContext(userBytes, vmFixture(t))
	require.NoError(t, err)
	got, err := clientcmd.Load(merged)
	require.NoError(t, err)

	assert.NotContains(t, got.Contexts, "ap-desktop", "the legacy context entry is always removed")
	assert.NotContains(t, got.AuthInfos, "ap-desktop", "the legacy-named user the legacy context pointed at is removed")
	assert.Contains(t, got.Clusters, "my-cluster", "a differently-named cluster is not recognized as legacy and must survive")
	assert.Equal(t, "oap-desktop", got.CurrentContext)
}

// TestIsVMContextCurrent_AcceptsLegacyName covers rule 5 of the migration
// contract: a user who toggled the checkbox before upgrading, and never
// re-toggles, must still see it checked.
func TestIsVMContextCurrent_AcceptsLegacyName(t *testing.T) {
	cfg := clientcmdapi.NewConfig()
	cfg.CurrentContext = "ap-desktop"
	b, err := clientcmd.Write(*cfg)
	require.NoError(t, err)
	assert.True(t, desktop.IsVMContextCurrent(b))
}
