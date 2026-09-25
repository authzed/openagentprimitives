package gke

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

const (
	// artifactBucketLabel marks buckets oap created; Ensure never adopts and
	// Teardown never deletes a bucket without it.
	artifactBucketLabel = "agentprimitives-owned"
	artifactStorageRole = "roles/storage.objectAdmin"
)

// gcloud seams, swapped by tests (the enableGatewayAPI pattern).
var (
	describeClusterIdentityForArtifacts = deriveClusterIdentity
	workloadPool                        = realWorkloadPool
	enableWorkloadPool                  = realEnableWorkloadPool
	nodePoolsNeedingWI                  = realNodePoolsNeedingWI
	updateNodePoolWI                    = realUpdateNodePoolWI
	bucketLabels                        = realBucketLabels
	createBucket                        = realCreateBucket
	labelBucket                         = realLabelBucket
	bindBucketIAM                       = realBindBucketIAM
	deleteBucketRecursive               = realDeleteBucketRecursive
	projectNumber                       = realProjectNumber
)

type gkeArtifactStorage struct{}

// ArtifactBucketName is the deterministic artifact bucket for a cluster:
// ap-artifacts-<project>-<hash8>, hash8 = first 8 hex chars of
// sha256("<location>/<cluster>"). Deterministic so reinstalls resolve the same
// bucket; the project segment truncates to keep the whole name within GCS's
// 63-char cap, and the hash keeps truncated names distinct.
func ArtifactBucketName(project, location, cluster string) string {
	sum := sha256.Sum256([]byte(location + "/" + cluster))
	hash := hex.EncodeToString(sum[:])[:8]
	const prefix = "ap-artifacts-"
	maxProject := 63 - len(prefix) - 1 - len(hash)
	p := project
	if len(p) > maxProject {
		p = p[:maxProject]
	}
	p = strings.Trim(p, "-")
	return prefix + p + "-" + hash
}

func (gkeArtifactStorage) Ensure(ctx context.Context, p cloud.ArtifactStorageParams) (cloud.ArtifactStorageResult, error) {
	project, cluster, location, err := describeClusterIdentityForArtifacts(ctx, p.Clients, p.Reporter)
	if err != nil {
		return cloud.ArtifactStorageResult{}, fmt.Errorf(
			"derive GKE cluster identity for artifact storage: %w (pass --artifact-store-url to skip auto-provisioning)", err)
	}

	// 1. Workload identity: without a pool + GKE_METADATA node pools the
	// operator has no ambient GCS credentials, so this is a hard gate.
	pool, err := workloadPool(ctx, p.Reporter, project, cluster, location)
	if err != nil {
		return cloud.ArtifactStorageResult{}, fmt.Errorf("check workload-identity pool on cluster %q: %w", cluster, err)
	}
	stalePools, err := nodePoolsNeedingWI(ctx, p.Reporter, project, cluster, location)
	if err != nil {
		return cloud.ArtifactStorageResult{}, fmt.Errorf("check node-pool workload metadata on cluster %q: %w", cluster, err)
	}
	if pool == "" || len(stalePools) > 0 {
		p.Reporter.Warn("cluster %q is not fully workload-identity enabled — the operator cannot reach a GCS artifact bucket without it", cluster)
		if pool == "" {
			p.Reporter.Info("  this runs: gcloud container clusters update %s --location=%s --workload-pool=%s.svc.id.goog", cluster, location, project)
		}
		if len(stalePools) > 0 {
			p.Reporter.Info("  and per node pool: gcloud container node-pools update <pool> --cluster=%s --location=%s --workload-metadata=GKE_METADATA", cluster, location)
			p.Reporter.Warn("  updating node pools %v RECREATES THEIR NODES (workloads reschedule)", stalePools)
		}
		if !cloud.Confirm(p.In, p.Reporter, fmt.Sprintf("Enable workload identity on cluster %q now?", cluster), p.AssumeYes) {
			return cloud.ArtifactStorageResult{}, fmt.Errorf(
				"workload identity declined; artifact storage cannot be auto-provisioned on %q — enable it later and re-run `oap install`, or pass --artifact-store-url", cluster)
		}
		p.Reporter.Info("  this is a long-running GKE operation — the control plane reconfigures%s; expect several minutes with only gcloud's progress output",
			func() string {
				if len(stalePools) > 0 {
					return " and the affected node pools are recreated"
				}
				return ""
			}())
		if pool == "" {
			if err := enableWorkloadPool(ctx, p.Reporter, project, cluster, location); err != nil {
				return cloud.ArtifactStorageResult{}, fmt.Errorf("enable workload-identity pool: %w", err)
			}
		}
		for _, np := range stalePools {
			if err := updateNodePoolWI(ctx, p.Reporter, project, cluster, location, np); err != nil {
				return cloud.ArtifactStorageResult{}, fmt.Errorf("enable GKE_METADATA on node pool %q: %w", np, err)
			}
		}
	}

	// 2. Bucket: adopt only when labeled ours; never write to a foreign one.
	bucket := ArtifactBucketName(project, location, cluster)
	labels, exists, err := bucketLabels(ctx, p.Reporter, bucket)
	if err != nil {
		return cloud.ArtifactStorageResult{}, fmt.Errorf("describe bucket gs://%s: %w", bucket, err)
	}
	if exists && labels[artifactBucketLabel] != "true" {
		return cloud.ArtifactStorageResult{}, fmt.Errorf(
			"bucket gs://%s exists but is not labeled %s=true — refusing to adopt a bucket oap did not create; delete or rename it, or pass --artifact-store-url", bucket, artifactBucketLabel)
	}
	if !exists {
		region := regionFromLocation(location)
		p.Reporter.Info("  this runs: gcloud storage buckets create gs://%s --project=%s --location=%s --uniform-bucket-level-access", bucket, project, region)
		p.Reporter.Info("  then labels it %s=true and grants the operator's workload-identity principal %s on it", artifactBucketLabel, artifactStorageRole)
		if !cloud.Confirm(p.In, p.Reporter, fmt.Sprintf("Create artifact bucket gs://%s now?", bucket), p.AssumeYes) {
			return cloud.ArtifactStorageResult{}, fmt.Errorf(
				"artifact bucket creation declined — create it later and re-run `oap install`, or pass --artifact-store-url")
		}
		// created records that THIS run made the bucket. `buckets create` has
		// no --labels on all gcloud tracks, so the ownership label lands in a
		// second call — and the label is the only evidence oap created it. A
		// bucket left unlabeled is permanently unadoptable: the deterministic
		// name means every later install hits the refusal above and `oap clean
		// --delete-artifacts` refuses too, with no in-product way out. Deleting
		// it is safe here and only here: oap created it seconds ago and nothing
		// has been written to it yet.
		created := true
		if err := createBucket(ctx, p.Reporter, project, region, bucket); err != nil {
			if !cloud.IsGcloudAlreadyExists(err) {
				return cloud.ArtifactStorageResult{}, fmt.Errorf("create bucket gs://%s: %w", bucket, err)
			}
			// It already existed despite the describe above saying otherwise —
			// a concurrent create. oap cannot prove it made this bucket, so it
			// must not delete it.
			created = false
		}
		if err := labelBucket(ctx, p.Reporter, bucket); err != nil {
			if !created {
				return cloud.ArtifactStorageResult{}, fmt.Errorf("label bucket gs://%s: %w", bucket, err)
			}
			if derr := deleteBucketRecursive(ctx, p.Reporter, bucket); derr != nil {
				return cloud.ArtifactStorageResult{}, fmt.Errorf(
					"label bucket gs://%s: %w (and removing the unlabeled bucket oap just created also failed: %v — "+
						"a later install cannot adopt it, so delete it by hand: gcloud storage rm --recursive gs://%s)",
					bucket, err, derr, bucket)
			}
			return cloud.ArtifactStorageResult{}, fmt.Errorf(
				"label bucket gs://%s: %w (the bucket oap had just created was removed, so re-running `oap install` starts clean)",
				bucket, err)
		}
	}

	// 3. IAM: bucket-scoped objectAdmin for the operator KSA via the direct
	// workload-identity principal (no GSA, no keys). add-iam-policy-binding
	// is idempotent, so this runs on every install.
	num, err := projectNumber(ctx, p.Reporter, project)
	if err != nil {
		return cloud.ArtifactStorageResult{}, fmt.Errorf("resolve project number for %q: %w", project, err)
	}
	member := fmt.Sprintf(
		"principal://iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s.svc.id.goog/subject/ns/%s/sa/%s",
		num, project, p.OperatorNamespace, p.OperatorServiceAccount)
	if err := bindBucketIAM(ctx, p.Reporter, bucket, member, artifactStorageRole); err != nil {
		return cloud.ArtifactStorageResult{}, fmt.Errorf("grant %s on gs://%s to %s: %w", artifactStorageRole, bucket, member, err)
	}

	p.Reporter.OK("artifact store: gs://%s (operator %s/%s via workload identity)", bucket, p.OperatorNamespace, p.OperatorServiceAccount)
	return cloud.ArtifactStorageResult{URL: "gs://" + bucket}, nil
}

// Teardown keeps the artifact bucket by default (artifacts survive `oap
// clean`); `--delete-artifacts` empties and deletes it, but only when it
// carries the agentprimitives-owned=true label — oap never deletes a bucket
// it did not create.
func (gkeArtifactStorage) Teardown(ctx context.Context, p cloud.ArtifactTeardownParams) (cloud.ArtifactTeardownResult, error) {
	if p.URL == "" {
		return cloud.ArtifactTeardownResult{}, nil
	}
	if !strings.HasPrefix(p.URL, "gs://") {
		return cloud.ArtifactTeardownResult{
			KeptMessage: fmt.Sprintf("artifact store %s was not created by oap's GKE provisioning; left untouched", p.URL),
		}, nil
	}
	bucket := strings.TrimPrefix(p.URL, "gs://")
	if !p.Delete {
		return cloud.ArtifactTeardownResult{
			KeptMessage: fmt.Sprintf(
				"artifact bucket %s kept — artifacts survive teardown and a reinstall adopts it; delete with `oap clean --delete-artifacts` or: gcloud storage rm --recursive %s",
				p.URL, p.URL),
		}, nil
	}
	labels, exists, err := bucketLabels(ctx, p.Reporter, bucket)
	if err != nil {
		return cloud.ArtifactTeardownResult{}, fmt.Errorf("describe bucket %s before delete: %w", p.URL, err)
	}
	if !exists {
		return cloud.ArtifactTeardownResult{KeptMessage: fmt.Sprintf("artifact bucket %s already absent", p.URL)}, nil
	}
	if labels[artifactBucketLabel] != "true" {
		return cloud.ArtifactTeardownResult{}, fmt.Errorf(
			"refusing to delete %s: it is not labeled %s=true (oap did not create it)", p.URL, artifactBucketLabel)
	}
	if err := deleteBucketRecursive(ctx, p.Reporter, bucket); err != nil {
		return cloud.ArtifactTeardownResult{}, fmt.Errorf("delete %s: %w", p.URL, err)
	}
	return cloud.ArtifactTeardownResult{Deleted: true}, nil
}

// regionFromLocation maps a zonal location (us-central1-a) to its region
// (us-central1); regional locations pass through. Same trailing-segment rule
// as RegistryFromProviderID.
func regionFromLocation(location string) string {
	i := strings.LastIndex(location, "-")
	if i <= 0 {
		return location
	}
	// A zone's last segment is a single letter (us-central1-a).
	if last := location[i+1:]; len(last) == 1 {
		return location[:i]
	}
	return location
}

// --- real gcloud implementations ---

func realWorkloadPool(ctx context.Context, rep cloud.Reporter, project, cluster, location string) (string, error) {
	out, err := cloud.Gcloud(ctx, rep, "container", "clusters", "describe", cluster,
		"--project="+project, "--location="+location,
		"--format=value(workloadIdentityConfig.workloadPool)")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func realEnableWorkloadPool(ctx context.Context, rep cloud.Reporter, project, cluster, location string) error {
	// --verbosity=info makes gcloud log each operation-poll line, so progress is
	// visible even if it falls back to non-interactive mode (no animated spinner);
	// the update is a several-minute control-plane operation otherwise silent.
	return cloud.GcloudStreaming(ctx, rep, "container", "clusters", "update", cluster,
		"--project="+project, "--location="+location,
		"--workload-pool="+project+".svc.id.goog", "--verbosity=info")
}

func realNodePoolsNeedingWI(ctx context.Context, rep cloud.Reporter, project, cluster, location string) ([]string, error) {
	out, err := cloud.Gcloud(ctx, rep, "container", "node-pools", "list",
		"--cluster="+cluster, "--project="+project, "--location="+location, "--format=json")
	if err != nil {
		return nil, err
	}
	var pools []struct {
		Name   string `json:"name"`
		Config struct {
			WorkloadMetadataConfig struct {
				Mode string `json:"mode"`
			} `json:"workloadMetadataConfig"`
		} `json:"config"`
	}
	if err := json.Unmarshal([]byte(out), &pools); err != nil {
		return nil, fmt.Errorf("parse node-pools list: %w", err)
	}
	var stale []string
	for _, np := range pools {
		if np.Config.WorkloadMetadataConfig.Mode != "GKE_METADATA" {
			stale = append(stale, np.Name)
		}
	}
	return stale, nil
}

func realUpdateNodePoolWI(ctx context.Context, rep cloud.Reporter, project, cluster, location, pool string) error {
	// See realEnableWorkloadPool re: --verbosity=info. Node-pool updates recreate
	// every node in the pool, so this is the longest-running of the two.
	return cloud.GcloudStreaming(ctx, rep, "container", "node-pools", "update", pool,
		"--cluster="+cluster, "--project="+project, "--location="+location,
		"--workload-metadata=GKE_METADATA", "--verbosity=info")
}

func realBucketLabels(ctx context.Context, rep cloud.Reporter, bucket string) (map[string]string, bool, error) {
	out, err := cloud.Gcloud(ctx, rep, "storage", "buckets", "describe", "gs://"+bucket,
		"--format=json(labels)")
	if err != nil {
		if cloud.IsGcloudNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var parsed struct {
		Labels map[string]string `json:"labels"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return nil, true, fmt.Errorf("parse bucket describe: %w", err)
	}
	return parsed.Labels, true, nil
}

func realCreateBucket(ctx context.Context, rep cloud.Reporter, project, region, bucket string) error {
	_, err := cloud.Gcloud(ctx, rep, "storage", "buckets", "create", "gs://"+bucket,
		"--project="+project, "--location="+region, "--uniform-bucket-level-access")
	return err
}

func realLabelBucket(ctx context.Context, rep cloud.Reporter, bucket string) error {
	// buckets create has no --labels on all gcloud tracks; update is universal.
	_, err := cloud.Gcloud(ctx, rep, "storage", "buckets", "update", "gs://"+bucket,
		"--update-labels="+artifactBucketLabel+"=true")
	return err
}

func realBindBucketIAM(ctx context.Context, rep cloud.Reporter, bucket, member, role string) error {
	_, err := cloud.Gcloud(ctx, rep, "storage", "buckets", "add-iam-policy-binding", "gs://"+bucket,
		"--member="+member, "--role="+role)
	return err
}

func realDeleteBucketRecursive(ctx context.Context, rep cloud.Reporter, bucket string) error {
	return cloud.GcloudStreaming(ctx, rep, "storage", "rm", "--recursive", "gs://"+bucket)
}

func realProjectNumber(ctx context.Context, rep cloud.Reporter, project string) (string, error) {
	out, err := cloud.Gcloud(ctx, rep, "projects", "describe", project,
		"--format=value(projectNumber)")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}
