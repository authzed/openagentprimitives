package gke

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// stubReporter is a minimal Reporter for tests that need Interactive() control.
// Mirrors pkg/platform/cloud/component_test.go's stubReporter (unexported, package-local).
type stubReporter struct {
	interactive bool
	steps       []string
	suspended   bool
	suspendOut  bytes.Buffer
}

func (r *stubReporter) Step(format string, args ...any) {}
func (r *stubReporter) OK(format string, args ...any)   {}
func (r *stubReporter) Info(format string, args ...any) {}
func (r *stubReporter) Warn(format string, args ...any) {}
func (r *stubReporter) Interactive() bool               { return r.interactive }
func (r *stubReporter) Suspend(fn func(io.Writer, io.Reader)) {
	r.suspended = true
	fn(&r.suspendOut, nil)
}

func TestArtifactBucketName(t *testing.T) {
	a := ArtifactBucketName("my-project", "us-central1", "zap2--internal")
	b := ArtifactBucketName("my-project", "us-central1", "zap2--internal")
	assert.Equal(t, a, b, "deterministic")
	assert.True(t, strings.HasPrefix(a, "ap-artifacts-my-project-"))
	assert.LessOrEqual(t, len(a), 63, "GCS name cap")

	c := ArtifactBucketName("my-project", "us-central1", "other-cluster")
	assert.NotEqual(t, a, c, "hash disambiguates clusters")

	long := ArtifactBucketName(strings.Repeat("p", 80), "us-central1", "c")
	assert.LessOrEqual(t, len(long), 63, "project segment truncated to fit")
	assert.False(t, strings.Contains(long, "--"), "no double dash from truncation")
}

// artifactFakes swaps every gcloud-calling seam var, records calls, and
// restores on cleanup — the enableGatewayAPI/describeClusterIdentity pattern.
type artifactFakes struct {
	calls []string

	pool         string
	poolErr      error
	pools        []string // node pools still needing GKE_METADATA
	labels       map[string]string
	bucketExists bool
	labelsErr    error
	projectNum   string
	createErr    error
	labelErr     error
	deleteErr    error
}

func installArtifactFakes(t *testing.T, f *artifactFakes) {
	t.Helper()
	origIdentity := describeClusterIdentityForArtifacts
	origPool := workloadPool
	origEnable := enableWorkloadPool
	origNodePools := nodePoolsNeedingWI
	origUpdatePool := updateNodePoolWI
	origLabels := bucketLabels
	origCreate := createBucket
	origLabel := labelBucket
	origBind := bindBucketIAM
	origNum := projectNumber
	origDelete := deleteBucketRecursive
	t.Cleanup(func() {
		describeClusterIdentityForArtifacts = origIdentity
		workloadPool = origPool
		enableWorkloadPool = origEnable
		nodePoolsNeedingWI = origNodePools
		updateNodePoolWI = origUpdatePool
		bucketLabels = origLabels
		createBucket = origCreate
		labelBucket = origLabel
		bindBucketIAM = origBind
		projectNumber = origNum
		deleteBucketRecursive = origDelete
	})
	describeClusterIdentityForArtifacts = func(ctx context.Context, cl cloud.Clients, rep cloud.Reporter) (string, string, string, error) {
		f.calls = append(f.calls, "identity")
		return "my-project", "my-cluster", "us-central1", nil
	}
	workloadPool = func(ctx context.Context, rep cloud.Reporter, project, cluster, location string) (string, error) {
		f.calls = append(f.calls, "workloadPool")
		return f.pool, f.poolErr
	}
	enableWorkloadPool = func(ctx context.Context, rep cloud.Reporter, project, cluster, location string) error {
		f.calls = append(f.calls, "enableWorkloadPool")
		return nil
	}
	nodePoolsNeedingWI = func(ctx context.Context, rep cloud.Reporter, project, cluster, location string) ([]string, error) {
		f.calls = append(f.calls, "nodePoolsNeedingWI")
		return f.pools, nil
	}
	updateNodePoolWI = func(ctx context.Context, rep cloud.Reporter, project, cluster, location, pool string) error {
		f.calls = append(f.calls, "updateNodePoolWI:"+pool)
		return nil
	}
	bucketLabels = func(ctx context.Context, rep cloud.Reporter, bucket string) (map[string]string, bool, error) {
		f.calls = append(f.calls, "bucketLabels:"+bucket)
		return f.labels, f.bucketExists, f.labelsErr
	}
	createBucket = func(ctx context.Context, rep cloud.Reporter, project, region, bucket string) error {
		f.calls = append(f.calls, fmt.Sprintf("createBucket:%s:%s", region, bucket))
		return f.createErr
	}
	labelBucket = func(ctx context.Context, rep cloud.Reporter, bucket string) error {
		f.calls = append(f.calls, "labelBucket:"+bucket)
		return f.labelErr
	}
	bindBucketIAM = func(ctx context.Context, rep cloud.Reporter, bucket, member, role string) error {
		f.calls = append(f.calls, "bindBucketIAM:"+member)
		return nil
	}
	projectNumber = func(ctx context.Context, rep cloud.Reporter, project string) (string, error) {
		f.calls = append(f.calls, "projectNumber")
		return "123456", nil
	}
	deleteBucketRecursive = func(ctx context.Context, rep cloud.Reporter, bucket string) error {
		f.calls = append(f.calls, "deleteBucketRecursive:"+bucket)
		return f.deleteErr
	}
}

func ensureParams(interactive bool, stdin string) cloud.ArtifactStorageParams {
	return cloud.ArtifactStorageParams{
		Reporter:               &stubReporter{interactive: interactive},
		In:                     strings.NewReader(stdin),
		AssumeYes:              false,
		OperatorNamespace:      "agentprimitives-system",
		OperatorServiceAccount: "spicebox-operator",
	}
}

func TestGKEArtifactEnsure(t *testing.T) {
	bucket := ArtifactBucketName("my-project", "us-central1", "my-cluster")

	t.Run("everything exists: adopt labeled bucket, re-bind IAM, return URL", func(t *testing.T) {
		f := &artifactFakes{pool: "my-project.svc.id.goog", labels: map[string]string{"agentprimitives-owned": "true"}, bucketExists: true}
		installArtifactFakes(t, f)
		res, err := gkeArtifactStorage{}.Ensure(context.Background(), ensureParams(true, ""))
		require.NoError(t, err)
		assert.Equal(t, "gs://"+bucket, res.URL)
		assert.NotContains(t, f.calls, "enableWorkloadPool")
		assert.NotContains(t, f.calls, "createBucket:us-central1:"+bucket)
		assert.Contains(t, strings.Join(f.calls, ","), "bindBucketIAM:principal://iam.googleapis.com/projects/123456/locations/global/workloadIdentityPools/my-project.svc.id.goog/subject/ns/agentprimitives-system/sa/spicebox-operator")
	})

	t.Run("bucket absent: consented create + label + bind", func(t *testing.T) {
		f := &artifactFakes{pool: "my-project.svc.id.goog", bucketExists: false}
		installArtifactFakes(t, f)
		res, err := gkeArtifactStorage{}.Ensure(context.Background(), ensureParams(true, "y\n"))
		require.NoError(t, err)
		assert.Equal(t, "gs://"+bucket, res.URL)
		assert.Contains(t, f.calls, "createBucket:us-central1:"+bucket)
		assert.Contains(t, f.calls, "labelBucket:"+bucket)
	})

	// The ownership label is the ONLY evidence oap created a bucket, and it is
	// applied by a SECOND gcloud call (buckets create has no --labels). If that
	// call fails, the bucket exists unlabeled — and because the name is
	// deterministic per cluster, every later `oap install` hits "refusing to
	// adopt a bucket oap did not create" and `oap clean --delete-artifacts`
	// refuses for the same reason. Nothing in the product recovers. Since oap
	// created the bucket moments ago and nothing has been written to it, the
	// unlabeled bucket must be removed so the next install starts clean.
	t.Run("label fails after create: the just-created bucket is deleted, not left unadoptable", func(t *testing.T) {
		f := &artifactFakes{
			pool:     "my-project.svc.id.goog",
			labelErr: fmt.Errorf("transient gcloud failure"),
		}
		installArtifactFakes(t, f)
		_, err := gkeArtifactStorage{}.Ensure(context.Background(), ensureParams(true, "y\n"))
		require.Error(t, err)
		assert.Contains(t, f.calls, "deleteBucketRecursive:"+bucket,
			"an unlabeled bucket oap just created must be removed, or every later install is wedged")
		assert.Contains(t, err.Error(), "transient gcloud failure", "the original cause must survive")
	})

	t.Run("label fails AND cleanup fails: error names the bucket and the manual command", func(t *testing.T) {
		f := &artifactFakes{
			pool:      "my-project.svc.id.goog",
			labelErr:  fmt.Errorf("transient gcloud failure"),
			deleteErr: fmt.Errorf("delete denied"),
		}
		installArtifactFakes(t, f)
		_, err := gkeArtifactStorage{}.Ensure(context.Background(), ensureParams(true, "y\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), bucket, "the operator must be told WHICH bucket is stranded")
		assert.Contains(t, err.Error(), "gcloud storage rm", "and the exact command that clears it")
		assert.Contains(t, err.Error(), "delete denied", "neither error may be dropped")
	})

	// The create raced with someone else (bucketLabels said absent, create said
	// exists). oap cannot prove it created that bucket, so it must NOT delete it.
	t.Run("create reports already-exists then label fails: never delete a bucket oap cannot prove it made", func(t *testing.T) {
		f := &artifactFakes{
			pool:      "my-project.svc.id.goog",
			createErr: fmt.Errorf("ERROR: (gcloud) Resource ALREADY_EXISTS"),
			labelErr:  fmt.Errorf("transient gcloud failure"),
		}
		installArtifactFakes(t, f)
		_, err := gkeArtifactStorage{}.Ensure(context.Background(), ensureParams(true, "y\n"))
		require.Error(t, err)
		assert.NotContains(t, f.calls, "deleteBucketRecursive:"+bucket,
			"a bucket that already existed is not ours to delete")
	})

	t.Run("bucket create declined: fail-closed error suggests --artifact-store-url", func(t *testing.T) {
		f := &artifactFakes{pool: "my-project.svc.id.goog", bucketExists: false}
		installArtifactFakes(t, f)
		_, err := gkeArtifactStorage{}.Ensure(context.Background(), ensureParams(true, "n\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--artifact-store-url")
		assert.NotContains(t, f.calls, "createBucket:us-central1:"+bucket, "declined ⇒ no mutation")
	})

	t.Run("bucket exists WITHOUT our label: fail-closed, never adopt", func(t *testing.T) {
		f := &artifactFakes{pool: "my-project.svc.id.goog", bucketExists: true, labels: map[string]string{}}
		installArtifactFakes(t, f)
		_, err := gkeArtifactStorage{}.Ensure(context.Background(), ensureParams(true, "y\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "agentprimitives-owned")
		assert.NotContains(t, strings.Join(f.calls, ","), "bindBucketIAM", "no IAM on a foreign bucket")
	})

	t.Run("no workload pool + consent: enable pool and stale node pools, then proceed", func(t *testing.T) {
		f := &artifactFakes{pool: "", pools: []string{"default-pool"}, labels: map[string]string{"agentprimitives-owned": "true"}, bucketExists: true}
		installArtifactFakes(t, f)
		res, err := gkeArtifactStorage{}.Ensure(context.Background(), ensureParams(true, "y\n"))
		require.NoError(t, err)
		assert.Equal(t, "gs://"+bucket, res.URL)
		assert.Contains(t, f.calls, "enableWorkloadPool")
		assert.Contains(t, f.calls, "updateNodePoolWI:default-pool")
	})

	t.Run("no workload pool + declined: fail-closed error", func(t *testing.T) {
		f := &artifactFakes{pool: ""}
		installArtifactFakes(t, f)
		_, err := gkeArtifactStorage{}.Ensure(context.Background(), ensureParams(true, "n\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "workload identity")
		assert.NotContains(t, f.calls, "enableWorkloadPool")
	})

	t.Run("non-interactive without --yes: declined consents fail closed", func(t *testing.T) {
		f := &artifactFakes{pool: "my-project.svc.id.goog", bucketExists: false}
		installArtifactFakes(t, f)
		_, err := gkeArtifactStorage{}.Ensure(context.Background(), ensureParams(false, ""))
		require.Error(t, err, "cloud.Confirm returns false on non-interactive")
	})
}

func TestGKEArtifactTeardown(t *testing.T) {
	cases := []struct {
		name        string
		params      cloud.ArtifactTeardownParams
		fakes       artifactFakes
		wantErr     string // "" ⇒ no error
		wantDeleted bool
		wantKeptHas string // substring of KeptMessage; "" ⇒ empty
		wantRMCall  bool
	}{
		{
			name:        "default keep: loud kept line with manual delete command",
			params:      cloud.ArtifactTeardownParams{URL: "gs://ap-artifacts-p-abcd1234"},
			wantKeptHas: "gcloud storage rm --recursive gs://ap-artifacts-p-abcd1234",
		},
		{
			name:        "delete: labeled bucket is removed",
			params:      cloud.ArtifactTeardownParams{URL: "gs://ap-artifacts-p-abcd1234", Delete: true},
			fakes:       artifactFakes{bucketExists: true, labels: map[string]string{"agentprimitives-owned": "true"}},
			wantDeleted: true,
			wantRMCall:  true,
		},
		{
			name:    "delete: UNLABELED bucket is refused",
			params:  cloud.ArtifactTeardownParams{URL: "gs://someones-bucket", Delete: true},
			fakes:   artifactFakes{bucketExists: true, labels: map[string]string{}},
			wantErr: "agentprimitives-owned",
		},
		{
			name:        "delete: already-absent bucket reports and succeeds",
			params:      cloud.ArtifactTeardownParams{URL: "gs://gone", Delete: true},
			fakes:       artifactFakes{bucketExists: false},
			wantKeptHas: "already absent",
		},
		{
			name:        "non-gs URL: kept untouched",
			params:      cloud.ArtifactTeardownParams{URL: "s3://b", Delete: true},
			wantKeptHas: "s3://b",
		},
		{name: "empty URL: silent no-op", params: cloud.ArtifactTeardownParams{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fakes
			installArtifactFakes(t, &f)
			tc.params.Reporter = &stubReporter{}
			res, err := gkeArtifactStorage{}.Teardown(context.Background(), tc.params)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantDeleted, res.Deleted)
			if tc.wantKeptHas == "" {
				assert.Empty(t, res.KeptMessage)
			} else {
				assert.Contains(t, res.KeptMessage, tc.wantKeptHas)
			}
			hasRM := false
			for _, c := range f.calls {
				if strings.HasPrefix(c, "deleteBucketRecursive:") {
					hasRM = true
				}
			}
			assert.Equal(t, tc.wantRMCall, hasRM)
		})
	}
}
