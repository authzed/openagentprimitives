package workspacesource

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	_ "github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/git"
)

// recordingSink collects every logged message so a test can assert that a
// failure was surfaced rather than swallowed.
type recordingSink struct {
	mu   sync.Mutex
	msgs []string
}

func (s *recordingSink) Init(logr.RuntimeInfo)                {}
func (s *recordingSink) Enabled(int) bool                     { return true }
func (s *recordingSink) WithValues(...any) logr.LogSink       { return s }
func (s *recordingSink) WithName(string) logr.LogSink         { return s }
func (s *recordingSink) Error(_ error, msg string, kv ...any) { s.record(msg, kv...) }
func (s *recordingSink) Info(_ int, msg string, kv ...any)    { s.record(msg, kv...) }
func (s *recordingSink) record(msg string, kv ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msg+" "+fmt.Sprint(kv...))
}

func (s *recordingSink) joined() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.msgs, "\n")
}

// TestSpecChanged_AbandonedRefreshDeleteFailure_IsLogged — when the base was
// materialized at an older generation, the controller best-effort deletes any
// in-flight refresh Job for the generation being abandoned. That delete
// discarded its error outright, so an RBAC regression or an apiserver 5xx left
// the Job orphaned with nothing in the logs — while the two sibling deletes in
// the same closure both log. AGENTS.md: never silently drop errors.
func TestSpecChanged_AbandonedRefreshDeleteFailure_IsLogged(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	ws := &spiceboxv1alpha1.WorkspaceSource{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-source", Namespace: "default", Generation: 2},
		Spec: spiceboxv1alpha1.WorkspaceSourceSpec{
			Source: spiceboxv1alpha1.WorkspaceSourceRef{Kind: "git", Locator: "https://example.com/o/r.git", Ref: "main"},
		},
	}
	basePVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: BaseClaimName(ws), Namespace: "default"},
	}
	// Materialized at generation 1, but the spec is now at generation 2 — the
	// branch that abandons the older generation's refresh Job.
	matJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      MaterializeJobName(ws),
			Namespace: "default",
			Labels:    map[string]string{BaseGenerationLabel: "1"},
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	}

	sink := &recordingSink{}
	denied := apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"},
		RefreshJobName(ws), assert.AnError)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ws, basePVC, matJob).
		WithStatusSubresource(&spiceboxv1alpha1.WorkspaceSource{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				if obj.GetName() == RefreshJobName(ws) {
					return denied
				}
				return nil
			},
		}).Build()

	r := &Reconciler{Client: c, BaseStorageClass: "standard", MaterializeImage: "busybox:1.36"}
	ctx := ctrllog.IntoContext(context.Background(), logr.New(sink))
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "demo-source", Namespace: "default"}})
	require.NoError(t, err, "the abandoned-refresh cleanup is best-effort; it must not fail the reconcile")

	assert.Contains(t, sink.joined(), "refresh Job",
		"a Forbidden delete of the abandoned refresh Job must be logged, not discarded")
}
