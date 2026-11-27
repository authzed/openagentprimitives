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

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

func TestSpiceDBDatabaseJob_ShapeAndIdempotentCreate(t *testing.T) {
	name, doc, err := buildSpiceDBDatabaseJob(installCreateDBDeadlineSeconds, nil)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, yaml.Unmarshal(doc, &m))

	assert.Equal(t, "batch/v1", m["apiVersion"])
	assert.Equal(t, "Job", m["kind"])
	meta := m["metadata"].(map[string]any)
	assert.Equal(t, name, meta["name"], "the returned name is the one in the manifest")
	assert.Regexp(t, `^spicebox-spicedb-createdb-[0-9a-f]{10}$`, name, "base name plus a template hash")
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

// TestSpiceDBDatabaseJob_NameTracksThePodTemplate pins the property that makes
// a template change safe to apply: spec.template is immutable, so the name must
// change exactly when the template does, and never otherwise.
func TestSpiceDBDatabaseJob_NameTracksThePodTemplate(t *testing.T) {
	base, baseDoc, err := buildSpiceDBDatabaseJob(installCreateDBDeadlineSeconds, nil)
	require.NoError(t, err)

	cases := []struct {
		name      string
		deadline  int64
		mirrorMap map[string]string
		wantSame  bool
	}{
		{"identical inputs: same name, so a re-apply is a no-op", installCreateDBDeadlineSeconds, nil, true},
		{"render's deadline (none): same name, the deadline is mutable and outside the template", 0, nil, true},
		{"mirror map without the createdb image: same name", installCreateDBDeadlineSeconds, map[string]string{"busybox": "reg.example.com/busybox"}, true},
		{"mirrored image: new name, the applied template differs", installCreateDBDeadlineSeconds, map[string]string{createDBImage: "reg.example.com/pgvector-pgvector:pg17"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, doc, err := buildSpiceDBDatabaseJob(tc.deadline, tc.mirrorMap)
			require.NoError(t, err)
			if tc.wantSame {
				assert.Equal(t, base, name)
			} else {
				assert.NotEqual(t, base, name)
			}
			if tc.deadline == installCreateDBDeadlineSeconds && tc.mirrorMap == nil {
				assert.Equal(t, string(baseDoc), string(doc), "identical inputs are byte-identical")
			}
			if mirrored := tc.mirrorMap[createDBImage]; mirrored != "" {
				assert.Contains(t, string(doc), "image: "+mirrored, "the hashed image is the one in the manifest")
			}
		})
	}

	// The name is a hash of the template, whatever changed in it.
	var job batchv1.Job
	require.NoError(t, yaml.Unmarshal(baseDoc, &job))
	tmpl, err := marshalClean(job.Spec.Template)
	require.NoError(t, err)
	assert.Equal(t, base, contentAddressedName(createDBJobBaseName, tmpl))
	job.Spec.Template.Spec.Containers[0].Command[2] += "\necho changed"
	changed, err := marshalClean(job.Spec.Template)
	require.NoError(t, err)
	assert.NotEqual(t, base, contentAddressedName(createDBJobBaseName, changed), "a changed script is a new Job")
}

// installedJob is a createdb Job as an earlier `oap install` left it: under
// another name, with the installed-by annotation kube.Apply stamps (pass nil
// annotations for a Job something else owns).
func installedJob(name string, annotations map[string]string) *batchv1.Job {
	return &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agentprimitives-system", Annotations: annotations},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"authzed.com/cluster": "spicebox-spicedb"}},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyOnFailure,
				Containers:    []corev1.Container{{Name: "createdb", Image: createDBImage, Command: []string{"sh", "-c", "echo an older script"}}},
			},
		}},
	}
}

// TestPruneSupersededCreateDBJobs covers what a re-install finds once the
// current Job has succeeded. That the apply itself survives an older Job is
// TestSpiceDBDatabaseJob_ReinstallAgainstAPIServer (integration): only a real
// API server enforces spec.template immutability.
func TestPruneSupersededCreateDBJobs(t *testing.T) {
	const ns, current = "agentprimitives-system", createDBJobBaseName + "-aaaaaaaaaa"
	installed := map[string]string{kube.InstalledByAnnotation: kube.InstalledByValue}

	cases := []struct {
		name     string
		existing []*batchv1.Job
		wantLeft []string
	}{
		{
			name:     "only the current Job: nothing deleted",
			existing: []*batchv1.Job{installedJob(current, installed)},
			wantLeft: []string{current},
		},
		{
			name:     "the fixed-name Job of an install that predates the hash: deleted",
			existing: []*batchv1.Job{installedJob(current, installed), installedJob(createDBJobBaseName, installed)},
			wantLeft: []string{current},
		},
		{
			name:     "a hashed Job of an older template: deleted",
			existing: []*batchv1.Job{installedJob(current, installed), installedJob(createDBJobBaseName+"-0123456789", installed)},
			wantLeft: []string{current},
		},
		{
			name:     "a createdb Job oap install did not apply (a GitOps controller's): left alone",
			existing: []*batchv1.Job{installedJob(current, installed), installedJob(createDBJobBaseName+"-0123456789", nil)},
			wantLeft: []string{current, createDBJobBaseName + "-0123456789"},
		},
		{
			name:     "an unrelated Job oap install applied: left alone",
			existing: []*batchv1.Job{installedJob(current, installed), installedJob("spicebox-spicedb-migrate", installed)},
			wantLeft: []string{current, "spicebox-spicedb-migrate"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []runtime.Object
			for _, j := range tc.existing {
				objs = append(objs, j)
			}
			cli := fake.NewSimpleClientset(objs...)

			require.NoError(t, pruneSupersededCreateDBJobs(context.Background(), cli, ns, current))

			left, err := cli.BatchV1().Jobs(ns).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			var names []string
			for _, j := range left.Items {
				names = append(names, j.Name)
			}
			assert.ElementsMatch(t, tc.wantLeft, names)
		})
	}
}

// TestPruneSupersededCreateDBJobs_SurfacesErrors: a prune that cannot list or
// delete says so rather than reporting a clean install.
func TestPruneSupersededCreateDBJobs_SurfacesErrors(t *testing.T) {
	const ns = "agentprimitives-system"
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, "", errors.New("forbidden"))
	old := installedJob(createDBJobBaseName, map[string]string{kube.InstalledByAnnotation: kube.InstalledByValue})
	for _, verb := range []string{"list", "delete"} {
		t.Run(verb+" forbidden: returned", func(t *testing.T) {
			cli := fake.NewSimpleClientset(old)
			cli.PrependReactor(verb, "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, forbidden
			})
			require.Error(t, pruneSupersededCreateDBJobs(context.Background(), cli, ns, "spicebox-spicedb-createdb-aaaaaaaaaa"))
		})
	}
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
			_, doc, err := buildSpiceDBDatabaseJob(tc.deadline, nil)
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

	_, doc, err := buildSpiceDBDatabaseJob(0, nil)
	require.NoError(t, err)
	assert.Contains(t, string(doc), "PGCONNECT_TIMEOUT", "each connect attempt is bounded")
}

// TestSpiceDBDatabaseJob_NoStatusStanza guards against server-side-applying a
// `status: {}` stanza: batchv1.JobStatus has no omitempty fields, so marshaling
// the typed Job directly always emits one, and AGENTS.md forbids a client
// applying status.
func TestSpiceDBDatabaseJob_NoStatusStanza(t *testing.T) {
	_, doc, err := buildSpiceDBDatabaseJob(installCreateDBDeadlineSeconds, nil)
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
