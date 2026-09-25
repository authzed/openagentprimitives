package wait

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// The verbatim RolloutError the spicedb-operator published during the outage
// that motivated this diagnosis path.
const rolloutErrMsg = `{"component":"datastore","error":"failed to create datastore: cannot apply bootstrap data: schema or tuples already exist in the datastore. Delete existing data or set the flag --datastore-bootstrap-overwrite=true","metadata":{}}`

func spicedbClusterCR(conds ...map[string]any) *unstructured.Unstructured {
	items := make([]any, 0, len(conds))
	for _, c := range conds {
		items = append(items, c)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "authzed.com/v1alpha1",
		"kind":       "SpiceDBCluster",
		"metadata":   map[string]any{"name": "spicebox-spicedb", "namespace": diagNS},
		"status":     map[string]any{"conditions": items},
	}}
}

func newDynFake(objs ...runtime.Object) *dynfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{spicedbClusterGVR: "SpiceDBClusterList"}, objs...)
}

// crashloopingSpiceDB returns a typed fake whose SpiceDB Deployment has one pod
// that already died fatally — the CrashLoopBackOff shape.
func crashloopingSpiceDB(t *testing.T, msg string) *fake.Clientset {
	t.Helper()
	sel := map[string]string{"app.kubernetes.io/instance": "spicebox-spicedb"}
	return fake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "spicebox-spicedb-spicedb", Namespace: diagNS},
			Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: sel}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "spicebox-spicedb-spicedb-xjd62", Namespace: diagNS, Labels: sel},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "spicedb",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1, Reason: "Error", Message: msg,
					}},
				}},
			},
		},
	)
}

func TestDiagnoseSpiceDBCluster_SurfacesFatalContainerExit(t *testing.T) {
	typed := crashloopingSpiceDB(t, "failed to create datastore: cannot apply bootstrap data")
	diag, err := DiagnoseSpiceDBCluster(context.Background(), typed, newDynFake(), diagNS,
		"spicebox-spicedb-spicedb", "spicebox-spicedb")
	require.NoError(t, err)

	require.NotNil(t, diag.Terminated, "a crashlooping pod's fatal exit must be gathered, not just its probe events")
	assert.Equal(t, "spicedb", diag.Terminated.Container)
	assert.Equal(t, int32(1), diag.Terminated.ExitCode)
	assert.Contains(t, diag.Terminated.Message, "cannot apply bootstrap data")
	assert.Contains(t, diag.Headline, "cannot apply bootstrap data",
		"the fatal error must lead the diagnosis")
	assert.NotContains(t, diag.Headline, "will retry",
		"a fatal startup error must not be reframed as a retryable wait")
}

func TestDiagnoseSpiceDBCluster_SurfacesOperatorRolloutError(t *testing.T) {
	typed := crashloopingSpiceDB(t, "")
	dyn := newDynFake(spicedbClusterCR(map[string]any{
		"type": "RolloutError", "status": "True", "reason": "PodError", "message": rolloutErrMsg,
	}))

	diag, err := DiagnoseSpiceDBCluster(context.Background(), typed, dyn, diagNS,
		"spicebox-spicedb-spicedb", "spicebox-spicedb")
	require.NoError(t, err)

	require.Len(t, diag.Conditions, 1, "the operator's own rollout conditions must reach the operator's screen")
	assert.Equal(t, "RolloutError", diag.Conditions[0].Type)
	assert.Contains(t, diag.Conditions[0].Message, "cannot apply bootstrap data")
	assert.NotContains(t, diag.Conditions[0].Message, `"component"`,
		"the JSON envelope should be unwrapped to the error itself")
}

// The Deployment is created asynchronously by the operator, so a stall can
// occur while it does not exist. The CR's explanation must survive that: the
// await loop discards the entire diagnosis when Diagnose returns an error.
func TestDiagnoseSpiceDBCluster_MissingDeploymentStillReportsCRStatus(t *testing.T) {
	dyn := newDynFake(spicedbClusterCR(map[string]any{
		"type": "RolloutError", "status": "True", "reason": "PodError", "message": rolloutErrMsg,
	}))

	diag, err := DiagnoseSpiceDBCluster(context.Background(), fake.NewSimpleClientset(), dyn, diagNS,
		"spicebox-spicedb-spicedb", "spicebox-spicedb")
	require.NoError(t, err, "an absent Deployment must not discard the CR's explanation")

	require.Len(t, diag.Conditions, 1)
	assert.Contains(t, diag.Conditions[0].Message, "cannot apply bootstrap data")
	assert.Contains(t, diag.Headline, "not readable yet",
		"the Deployment read failure is surfaced, not silently dropped")
}

// With nothing to report from either source, the underlying read error must
// still propagate rather than being swallowed into an empty diagnosis.
func TestDiagnoseSpiceDBCluster_NoCRAndNoDeploymentPropagatesError(t *testing.T) {
	_, err := DiagnoseSpiceDBCluster(context.Background(), fake.NewSimpleClientset(), newDynFake(), diagNS,
		"spicebox-spicedb-spicedb", "spicebox-spicedb")
	require.Error(t, err)
}

func TestSpiceDBConditionMessage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "operator JSON envelope is unwrapped to component + error",
			in:   rolloutErrMsg,
			want: "datastore: failed to create datastore: cannot apply bootstrap data: schema or tuples already exist in the datastore. Delete existing data or set the flag --datastore-bootstrap-overwrite=true",
		},
		{
			name: "envelope without a component yields the bare error",
			in:   `{"error":"boom"}`,
			want: "boom",
		},
		{
			name: "plain text passes through untouched",
			in:   "no TLS configured, consider setting \"tlsSecretName\"",
			want: "no TLS configured, consider setting \"tlsSecretName\"",
		},
		{
			name: "malformed JSON passes through untouched",
			in:   `{"error":`,
			want: `{"error":`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, spiceDBConditionMessage(tc.in))
		})
	}
}
