package installcmd

import (
	"context"
	"errors"
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
	doc, err := buildSpiceDBDatabaseJob()
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

// TestSpiceDBDatabaseJob_NoStatusStanza guards against server-side-applying a
// `status: {}` stanza: batchv1.JobStatus has no omitempty fields, so marshaling
// the typed Job directly always emits one, and AGENTS.md forbids a client
// applying status.
func TestSpiceDBDatabaseJob_NoStatusStanza(t *testing.T) {
	doc, err := buildSpiceDBDatabaseJob()
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
