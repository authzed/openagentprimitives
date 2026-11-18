package installcmd

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

func TestSpiceDBDatabaseJob_ShapeAndIdempotentCreate(t *testing.T) {
	doc, err := buildSpiceDBDatabaseJob(installCreateDBDeadlineSeconds)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, yaml.Unmarshal(doc, &m))

	assert.Equal(t, "batch/v1", m["apiVersion"])
	assert.Equal(t, "Job", m["kind"])
	meta := m["metadata"].(map[string]any)
	assert.Equal(t, "spicebox-spicedb-createdb", meta["name"], "fixed name → idempotent re-apply")
	assert.Equal(t, "agentprimitives-system", meta["namespace"])

	// Reuses the postgres image (already a DependencyImage) and admin URI, and
	// creates the 'spicedb' database only when absent.
	s := string(doc)
	assert.Contains(t, s, "pgvector/pgvector:pg17")
	assert.Contains(t, s, "spicebox-postgres-token")
	assert.Contains(t, s, "CREATE DATABASE spicedb")
	assert.Contains(t, s, "pg_database")

	// The pod template must carry the spicedb-operator's owner label, or the
	// postgres ingress / spicedb-egress NetworkPolicies (which select on it)
	// block the createdb pod from reaching Postgres under a default-deny CNI.
	spec := m["spec"].(map[string]any)
	tmpl := spec["template"].(map[string]any)
	tmplMeta := tmpl["metadata"].(map[string]any)
	tmplLabels := tmplMeta["labels"].(map[string]any)
	assert.Equal(t, "spicebox-spicedb", tmplLabels["authzed.com/cluster"],
		"createdb Job pod template must carry the owner label the postgres/spicedb-egress NetworkPolicies select on")
}

// TestSpiceDBDatabaseJob_Deadline pins that only oap install bounds the Job:
// it applies the Job after Postgres is Ready, while the GitOps overlay's Job
// lands in the same apply as Postgres and must survive a slow cold start.
func TestSpiceDBDatabaseJob_Deadline(t *testing.T) {
	cases := []struct {
		name     string
		deadline int64
		want     *int64
	}{
		{"install: 120s deadline", installCreateDBDeadlineSeconds, ptr(int64(120))},
		{"render (0): no deadline", 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := buildSpiceDBDatabaseJob(tc.deadline)
			require.NoError(t, err)
			var job batchv1.Job
			require.NoError(t, yaml.Unmarshal(doc, &job))
			assert.Equal(t, tc.want, job.Spec.ActiveDeadlineSeconds)
			assert.Equal(t, ptr(int32(6)), job.Spec.BackoffLimit, "backoffLimit bounds the Job either way")
		})
	}
}

// TestCreateDBScript_WaitsForPostgresThenStaysIdempotent checks the script
// parses and keeps its shape: a bounded wait for Postgres, each connect bounded
// by PGCONNECT_TIMEOUT, then the pg_database-guarded create.
func TestCreateDBScript_WaitsForPostgresThenStaysIdempotent(t *testing.T) {
	out, err := exec.Command("sh", "-n", "-c", createDBScript).CombinedOutput()
	require.NoError(t, err, "script must parse: %s", out)

	wait := strings.Index(createDBScript, `-tAc "SELECT 1" `)
	guard := strings.Index(createDBScript, "pg_database")
	require.GreaterOrEqual(t, wait, 0, "script waits for Postgres")
	assert.Less(t, wait, guard, "the wait precedes the existence check")
	assert.Contains(t, createDBScript, `-ge 60`, "the wait is bounded")

	doc, err := buildSpiceDBDatabaseJob(0)
	require.NoError(t, err)
	assert.Contains(t, string(doc), "PGCONNECT_TIMEOUT", "each connect attempt is bounded")
}

// TestSpiceDBDatabaseJob_NoStatusStanza guards against server-side-applying a
// `status: {}` stanza: batchv1.JobStatus has no omitempty fields, so marshaling
// the typed Job directly always emits one, and AGENTS.md forbids a client
// applying status.
func TestSpiceDBDatabaseJob_NoStatusStanza(t *testing.T) {
	doc, err := buildSpiceDBDatabaseJob(installCreateDBDeadlineSeconds)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, yaml.Unmarshal(doc, &m))

	_, hasStatus := m["status"]
	assert.False(t, hasStatus, "manifest must not carry a status stanza before being SSA-applied")
}

func TestJobSucceeded(t *testing.T) {
	const ns, name = "agentprimitives-system", "spicebox-spicedb-createdb"

	cases := []struct {
		name      string
		job       *batchv1.Job // nil → Job absent (NotFound)
		reactErr  error        // non-nil → Get returns this error instead
		wantReady bool
		wantErr   bool
	}{
		{
			name:      "Job absent (NotFound) → not ready, no error, keep polling",
			job:       nil,
			wantReady: false,
			wantErr:   false,
		},
		{
			name: "Job present, Succeeded == 0 → not ready, no error",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Status:     batchv1.JobStatus{Succeeded: 0},
			},
			wantReady: false,
			wantErr:   false,
		},
		{
			name: "Job present, Succeeded >= 1 → ready, no error",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Status:     batchv1.JobStatus{Succeeded: 1},
			},
			wantReady: true,
			wantErr:   false,
		},
		{
			name:      "Get returns a non-NotFound error (RBAC Forbidden) → surfaced, not swallowed",
			job:       nil,
			reactErr:  apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, name, errors.New("forbidden")),
			wantReady: false,
			wantErr:   true,
		},
		{
			name: "Job present, JobFailed/True condition (backoff exhausted) → surfaced, not a silent hang",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Status: batchv1.JobStatus{
					Succeeded: 0,
					Conditions: []batchv1.JobCondition{{
						Type:    batchv1.JobFailed,
						Status:  corev1.ConditionTrue,
						Reason:  "BackoffLimitExceeded",
						Message: "Job has reached the specified backoff limit",
					}},
				},
			},
			wantReady: false,
			wantErr:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []runtime.Object
			if tc.job != nil {
				objs = append(objs, tc.job)
			}
			cli := fake.NewSimpleClientset(objs...)
			if tc.reactErr != nil {
				cli.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tc.reactErr
				})
			}

			ready, err := jobSucceeded(cli, ns, name)(context.Background())

			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantReady, ready)
		})
	}
}
