package installcmd

import (
	"context"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

const (
	artifactStoreURLEnvName = "ARTIFACT_STORE_URL"

	// `oap init --local`: a small RWO PVC on the operator; survives restarts
	// AND rescheduling (the operator is single-replica, so RWO is fine).
	// The mount path and size themselves come from the kind's
	// RequiresOperatorVolume (pkg/platform/cloud/local owns that decision); only the
	// PVC's name, namespace, and access mode — install-bundle shape — live here.
	localArtifactPVCName    = "ap-artifacts"
	localArtifactVolumeName = "artifact-store"

	// Workload-identity principal target — must match config/manager.
	operatorArtifactNamespace      = "agentprimitives-system"
	operatorArtifactServiceAccount = "spicebox-operator"
)

// ArtifactStoreOptions carries the --artifact-store-url override into
// RunInstall (the StatefulResolveOptions pattern).
type ArtifactStoreOptions struct {
	ExplicitURL string
}

// resolveArtifactStoreURL decides the operator's ARTIFACT_STORE_URL for this
// install, and reports any mount the chosen store needs.
// Precedence: --artifact-store-url > the kind's ArtifactStorage.Ensure
// (a file:// PVC on local, consented GCS provisioning on GKE, fail-closed
// RequireExplicit elsewhere). Runs on EVERY install — SSA under the single
// ap-install field manager would strip a previously-injected value otherwise.
//
// An explicit --artifact-store-url yields no volume request: Ensure is never
// called on that path, so a user pointing at their own pre-provisioned mount
// does not get a PVC injected under them.
func resolveArtifactStoreURL(ctx context.Context, bundle *kube.Bundle, strat cloud.Strategy,
	opts ArtifactStoreOptions, assumeYes bool, rep progress.Reporter) (string, *cloud.OperatorVolumeRequest, error) {
	if opts.ExplicitURL != "" {
		return opts.ExplicitURL, nil, nil
	}
	res, err := strat.ArtifactStorage().Ensure(ctx, cloud.ArtifactStorageParams{
		Clients:                cloudClients(bundle),
		Reporter:               newCloudReporterRep(rep),
		In:                     os.Stdin,
		AssumeYes:              assumeYes,
		OperatorNamespace:      operatorArtifactNamespace,
		OperatorServiceAccount: operatorArtifactServiceAccount,
	})
	if err != nil {
		return "", nil, fmt.Errorf("artifact storage: %w", err)
	}
	if res.URL == "" {
		return "", nil, fmt.Errorf("artifact storage: %s returned no store URL", strat.DisplayName())
	}
	return res.URL, res.RequiresOperatorVolume, nil
}

// injectOperatorArtifactVolume appends the oap-artifacts PVC volume + mount to
// the operator Deployment doc (idempotent by volume name), mirroring the
// imagePullSecrets pod-level injection in pull_secret.go.
func injectOperatorArtifactVolume(docs []*unstructured.Unstructured, vol cloud.OperatorVolumeRequest) error {
	for _, d := range docs {
		if d.GetKind() != "Deployment" || d.GetName() != operatorDeploymentName {
			continue
		}
		vols, _, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "volumes")
		if err != nil {
			return fmt.Errorf("%s: read volumes: %w", operatorDeploymentName, err)
		}
		for _, v := range vols {
			if m, ok := v.(map[string]any); ok && m["name"] == localArtifactVolumeName {
				return nil // already injected (idempotent re-run)
			}
		}
		vols = append(vols, map[string]any{
			"name":                  localArtifactVolumeName,
			"persistentVolumeClaim": map[string]any{"claimName": localArtifactPVCName},
		})
		if err := unstructured.SetNestedSlice(d.Object, vols, "spec", "template", "spec", "volumes"); err != nil {
			return fmt.Errorf("%s: write volumes: %w", operatorDeploymentName, err)
		}

		containers, found, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
		if err != nil || !found {
			return fmt.Errorf("%s: read containers: %w", operatorDeploymentName, err)
		}
		mutated := false
		for ci, raw := range containers {
			c, ok := raw.(map[string]any)
			if !ok || c["name"] != operatorContainerName {
				continue
			}
			mounts, _ := c["volumeMounts"].([]any)
			mounts = append(mounts, map[string]any{
				"name":      localArtifactVolumeName,
				"mountPath": vol.MountPath,
			})
			c["volumeMounts"] = mounts
			containers[ci] = c
			mutated = true
			break
		}
		if !mutated {
			return fmt.Errorf("%s: no container named %q", operatorDeploymentName, operatorContainerName)
		}
		return unstructured.SetNestedSlice(d.Object, containers, "spec", "template", "spec", "containers")
	}
	return nil
}

// localArtifactPVCDoc is the RWO PVC applied for a kind whose ArtifactStorage
// demands an operator mount (today: `oap init --local`). No storageClassName:
// the cluster default (local-path on Docker Desktop / k3s) binds on first
// consumer.
func localArtifactPVCDoc(vol cloud.OperatorVolumeRequest) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      localArtifactPVCName,
			"namespace": operatorArtifactNamespace,
		},
		"spec": map[string]any{
			"accessModes": []any{"ReadWriteOnce"},
			"resources":   map[string]any{"requests": map[string]any{"storage": vol.MinSize}},
		},
	}}
}
