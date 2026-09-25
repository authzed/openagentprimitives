package agentcmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

// fakeSpiceboxClassCR builds an unstructured SpiceboxClass declaring memory —
// the same minimal shape pkg/platform/capacityfit's own tests use (sandboxClass),
// rebuilt locally here since that helper is unexported. A made-up fixture
// name — never an example's name.
func fakeSpiceboxClassCR(name, mem string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "SpiceboxClass",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"resources": map[string]any{"memory": mem}},
	}}
}

// fakeNode builds a Node reporting mem allocatable memory — enough for
// cloud/local.Strategy.SchedulingCeiling (which reads node.status.allocatable)
// to derive a known ceiling in tests.
func TestNewCapacityQuestions(t *testing.T) {
	ctx := context.Background()
	// An empty scheme is sufficient: every read/write below goes through
	// *unstructured.Unstructured with its GVK explicitly set, the same pattern
	// pkg/platform/oap/install/extraquestions_test.go already relies on against a fake
	// client with no types registered.
	scheme := runtime.NewScheme()

	t.Run("no readable nodes: ceiling unknown -> no questions, one notice naming why", func(t *testing.T) {
		kb := &kube.Bundle{
			Typed:      k8sfake.NewSimpleClientset(),
			Controller: fake.NewClientBuilder().WithScheme(scheme).Build(),
		}
		hook := NewCapacityQuestions(ctx, kb)
		qs, notices, err := hook(ctx, []*unstructured.Unstructured{fakeSpiceboxClassCR("demo-class", "4Gi")})

		require.NoError(t, err)
		assert.Empty(t, qs, "an unknown ceiling must never synthesize a question")
		require.Len(t, notices, 1)
		assert.Contains(t, notices[0], "capacity check skipped")
	})

	t.Run("oversized class against a small node -> one question with a fitting default", func(t *testing.T) {
		kb := &kube.Bundle{
			Typed:      k8sfake.NewSimpleClientset(aptest.NodeWithCapacity("small-node", "1Gi")),
			Controller: fake.NewClientBuilder().WithScheme(scheme).Build(),
		}
		hook := NewCapacityQuestions(ctx, kb)
		qs, notices, err := hook(ctx, []*unstructured.Unstructured{fakeSpiceboxClassCR("demo-class", "4Gi")})

		require.NoError(t, err)
		require.Len(t, qs, 1, "the class exceeds the 1Gi node's ceiling")
		assert.Equal(t, "capacity.demo-class.memory", qs[0].Name)
		assert.NotEmpty(t, qs[0].Default, "capacityfit always proposes a fitting default")
		assert.NotEmpty(t, notices, "a clamp must never be silent")
	})

	t.Run("class already fits -> no question, no notice", func(t *testing.T) {
		kb := &kube.Bundle{
			Typed:      k8sfake.NewSimpleClientset(aptest.NodeWithCapacity("big-node", "8Gi")),
			Controller: fake.NewClientBuilder().WithScheme(scheme).Build(),
		}
		hook := NewCapacityQuestions(ctx, kb)
		qs, notices, err := hook(ctx, []*unstructured.Unstructured{fakeSpiceboxClassCR("demo-class", "1Gi")})

		require.NoError(t, err)
		assert.Empty(t, qs)
		assert.Empty(t, notices)
	})

	t.Run("installed closure Gets as unstructured: a non-canonical quantity is adopted verbatim", func(t *testing.T) {
		// This is the load-bearing regression from the SDD ledger's C1 finding: a
		// typed Get would round-trip "1.8Gi" through resource.Quantity and lose
		// the text, rewriting the CR's value on the next install even though
		// nothing the operator asked for changed.
		existing := fakeSpiceboxClassCR("demo-class", "1.8Gi")
		kb := &kube.Bundle{
			Typed:      k8sfake.NewSimpleClientset(aptest.NodeWithCapacity("big-node", "8Gi")),
			Controller: fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build(),
		}
		hook := NewCapacityQuestions(ctx, kb)
		// The BUNDLED ask (16Gi) exceeds the ceiling, so a question IS
		// synthesized; the installed 1.8Gi already fits [floor, ceiling] and must
		// be offered back byte-for-byte as the default.
		qs, notices, err := hook(ctx, []*unstructured.Unstructured{fakeSpiceboxClassCR("demo-class", "16Gi")})

		require.NoError(t, err)
		require.Len(t, qs, 1)
		assert.Equal(t, "1.8Gi", qs[0].Default, "the installed value must be read back verbatim, not re-rendered from a parsed Quantity")
		require.Len(t, notices, 1)
		assert.Contains(t, notices[0], "keeping")
	})

	t.Run("installed SpiceboxClass absent -> installed closure reports absence, not an error", func(t *testing.T) {
		kb := &kube.Bundle{
			Typed:      k8sfake.NewSimpleClientset(aptest.NodeWithCapacity("big-node", "8Gi")),
			Controller: fake.NewClientBuilder().WithScheme(scheme).Build(),
		}
		hook := NewCapacityQuestions(ctx, kb)
		qs, _, err := hook(ctx, []*unstructured.Unstructured{fakeSpiceboxClassCR("demo-class", "16Gi")})

		require.NoError(t, err)
		require.Len(t, qs, 1)
		assert.NotEmpty(t, qs[0].Default, "with nothing installed, the default falls back to the suggested fit")
	})
}

// splitCapacitySets moved to pkg/platform/oap as SplitReserved (shared with admind,
// which cannot import cmd/oap) — see pkg/platform/oap/question_test.go's
// TestSplitReserved for its coverage.
