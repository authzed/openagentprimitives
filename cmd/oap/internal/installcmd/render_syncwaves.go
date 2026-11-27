package installcmd

import (
	"fmt"

	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/platform/bootstrap"
)

// Argo CD sync-waves order the cold start. Everything not named here is wave 0.
//
//	-3  the agentprimitives-system Namespace and the bootstrap Job's own
//	    ServiceAccount/Role/RoleBinding (the Job's prerequisites)
//	-2  the bootstrap Job — mints every internal Secret; Argo waits for it to
//	    complete before moving on, so no consumer starts without its Secret
//	-1  Postgres — needs the Job's spicebox-postgres-token; must be up before
//	    the SpiceDB database Job and the SpiceDBCluster migrate into it
//	 0  everything else, SpiceDB and the operator included
//
// Nothing at wave 0 is waited on by an earlier wave, so no wave can deadlock on
// a later one. Flux has no waves: it applies one Kustomization in a single pass
// and the same order emerges from the Job's retries and the workloads waiting on
// their Secrets (site/content/docs/gitops.mdx).
const (
	argoSyncWaveAnnotation    = "argocd.argoproj.io/sync-wave"
	argoSyncOptionsAnnotation = "argocd.argoproj.io/sync-options"

	syncWaveBootstrapPrereqs = "-3"
	syncWaveBootstrapJob     = "-2"
	syncWavePostgres         = "-1"

	syncWavePatchFile = "patches/sync-waves.yaml"
)

// syncWavePatch places the base's agentprimitives-system Namespace in the
// bootstrap-prerequisites wave, so the Job's ServiceAccount has somewhere to
// land.
func syncWavePatch() []byte {
	return []byte("apiVersion: v1\n" +
		"kind: Namespace\n" +
		"metadata:\n" +
		"  name: " + bootstrap.TokenSecretNamespace + "\n" +
		"  annotations:\n" +
		"    " + argoSyncWaveAnnotation + ": \"" + syncWaveBootstrapPrereqs + "\"\n")
}

// annotateDoc adds annotations to one marshaled YAML document.
func annotateDoc(doc []byte, annotations map[string]string) ([]byte, error) {
	var m map[string]any
	if err := yaml.Unmarshal(doc, &m); err != nil {
		return nil, fmt.Errorf("parse document to annotate: %w", err)
	}
	meta, _ := m["metadata"].(map[string]any)
	if meta == nil {
		return nil, fmt.Errorf("document to annotate has no metadata")
	}
	ann, _ := meta["annotations"].(map[string]any)
	if ann == nil {
		ann = map[string]any{}
	}
	for k, v := range annotations {
		ann[k] = v
	}
	meta["annotations"] = ann
	return yaml.Marshal(m)
}
