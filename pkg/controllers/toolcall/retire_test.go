package toolcall

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestStrandedRetiredSweep covers the startup heal for calls an older operator
// left carrying both the retired label and the finalizer: only those lose the
// finalizer, and nothing else the sweep lists or skips is touched.
func TestStrandedRetiredSweep(t *testing.T) {
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))

	call := func(name string, retired, finalized bool) *spiceboxv1alpha1.ToolCall {
		tc := &spiceboxv1alpha1.ToolCall{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
		if retired {
			tc.Labels = map[string]string{spiceboxv1alpha1.LabelToolCallRetired: "true"}
		}
		if finalized {
			tc.Finalizers = []string{spiceboxv1alpha1.FinalizerToolCall, "example.com/other"}
		}
		return tc
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		call("stranded", true, true),
		call("retired-clean", true, false),
		call("in-flight", false, true),
	).Build()

	require.NoError(t, (&strandedRetiredSweep{reader: c, writer: c}).Start(context.Background()))

	get := func(name string) *spiceboxv1alpha1.ToolCall {
		t.Helper()
		var got spiceboxv1alpha1.ToolCall
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &got))
		return &got
	}
	assert.Equal(t, []string{"example.com/other"}, get("stranded").Finalizers,
		"stranded retired call: only the toolcall finalizer is removed, others are kept")
	assert.Empty(t, get("retired-clean").Finalizers, "already-clean retired call: untouched")
	assert.Contains(t, get("in-flight").Finalizers, spiceboxv1alpha1.FinalizerToolCall,
		"unretired call: its finalizer still guards a running call, so the sweep leaves it alone")
}

// TestStrandedRetiredSweep_ListErrorDoesNotStopManager pins the best-effort
// contract: a Runnable returning an error stops the whole manager.
func TestStrandedRetiredSweep_ListErrorDoesNotStopManager(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build() // ToolCall not registered: List fails
	assert.NoError(t, (&strandedRetiredSweep{reader: c, writer: c}).Start(context.Background()))
}
