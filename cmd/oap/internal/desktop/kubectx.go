package desktop

import (
	"fmt"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/imageload"
)

// APContextName is the oap-desktop VM's kube-context name. Defined in imageload
// (which classifies contexts and must know this one) and aliased here so the
// name has a single definition.
const APContextName = imageload.APContextName

// vmEntryName is the name the VM's staged kubeconfig gives its single
// cluster/user/context (see writeKubeconfig in
// cmd/oap/internal/desktopcmd/run_darwin.go and
// cmd/oap/internal/desktop/vz.Provider.Kubeconfig, which fetch it fresh over
// SSH from k3s — k3s always names these "default").
const vmEntryName = "default"

// MergeVMContext merges the VM's k3s kubeconfig (vmKubeconfig) into the
// user's kubeconfig (userKubeconfig) as a distinct, named context
// (APContextName) and makes it current, WITHOUT disturbing any other
// cluster/user/context already present in userKubeconfig.
//
// vmKubeconfig is expected to carry exactly one cluster, one user, and one
// context — all named vmEntryName ("default"), per how the desktop app
// stages it. Those three entries are copied into the returned config under
// APContextName (renaming the context's cluster/user references to match),
// and current-context is set to APContextName.
//
// userKubeconfig may be empty (no ~/.kube/config yet); the merge then starts
// from a fresh, empty config. Malformed YAML in either input is an error.
// Re-running this (e.g. after the VM is restaged) is idempotent: a
// pre-existing APContextName entry in userKubeconfig is overwritten in
// place, never duplicated.
//
// MergeVMContext is also the single write path for the ap-desktop ->
// oap-desktop rename, so the legacy-name migration lives here: a leftover
// imageload.LegacyAPContextName context describes the same VM the fresh
// APContextName entries now describe, so it is deleted rather than left
// dangling alongside them. Its cluster/user references are deleted too, but
// only when they are themselves named LegacyAPContextName — a hand-edited
// legacy context pointing at differently-named cluster/user entries has
// only its context entry removed, mirroring the repo's "remove, don't
// restore" rule for external state: we remove what we recognize as ours and
// leave what we don't. A legacy current-context is repointed to
// APContextName (which it would already become today, since the merge
// always sets current-context to APContextName).
func MergeVMContext(userKubeconfig, vmKubeconfig []byte) ([]byte, error) {
	user, err := clientcmd.Load(userKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("desktop: parse user kubeconfig: %w", err)
	}
	vm, err := clientcmd.Load(vmKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("desktop: parse VM kubeconfig: %w", err)
	}

	cluster, ok := vm.Clusters[vmEntryName]
	if !ok {
		return nil, fmt.Errorf("desktop: VM kubeconfig has no cluster named %q", vmEntryName)
	}
	authInfo, ok := vm.AuthInfos[vmEntryName]
	if !ok {
		return nil, fmt.Errorf("desktop: VM kubeconfig has no user named %q", vmEntryName)
	}
	vmContext, ok := vm.Contexts[vmEntryName]
	if !ok {
		return nil, fmt.Errorf("desktop: VM kubeconfig has no context named %q", vmEntryName)
	}

	if user.Clusters == nil {
		user.Clusters = map[string]*clientcmdapi.Cluster{}
	}
	if user.AuthInfos == nil {
		user.AuthInfos = map[string]*clientcmdapi.AuthInfo{}
	}
	if user.Contexts == nil {
		user.Contexts = map[string]*clientcmdapi.Context{}
	}

	mergedCluster := *cluster
	mergedAuthInfo := *authInfo
	mergedContext := *vmContext
	mergedContext.Cluster = APContextName
	mergedContext.AuthInfo = APContextName

	user.Clusters[APContextName] = &mergedCluster
	user.AuthInfos[APContextName] = &mergedAuthInfo
	user.Contexts[APContextName] = &mergedContext
	user.CurrentContext = APContextName

	if legacyContext, ok := user.Contexts[imageload.LegacyAPContextName]; ok {
		if legacyContext.Cluster == imageload.LegacyAPContextName {
			delete(user.Clusters, imageload.LegacyAPContextName)
		}
		if legacyContext.AuthInfo == imageload.LegacyAPContextName {
			delete(user.AuthInfos, imageload.LegacyAPContextName)
		}
		delete(user.Contexts, imageload.LegacyAPContextName)
	}

	out, err := clientcmd.Write(*user)
	if err != nil {
		return nil, fmt.Errorf("desktop: write merged kubeconfig: %w", err)
	}
	return out, nil
}

// IsVMContextCurrent reports whether userKubeconfig's current-context is the
// merged VM context — either the current APContextName or the pre-rename
// imageload.LegacyAPContextName, so the menubar checkmark stays truthful for
// a user who last toggled before upgrading and never re-toggles. Malformed
// or empty input reports false rather than erroring: callers use this purely
// to decide a checkbox's display state, where "unknown" and "not current"
// render identically.
func IsVMContextCurrent(userKubeconfig []byte) bool {
	cfg, err := clientcmd.Load(userKubeconfig)
	if err != nil {
		return false
	}
	return cfg.CurrentContext == APContextName || cfg.CurrentContext == imageload.LegacyAPContextName
}
