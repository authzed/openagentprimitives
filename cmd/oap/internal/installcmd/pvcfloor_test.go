package installcmd

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
)

func hyperdiskDefaultSC() *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "hyperdisk-balanced", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}},
		Provisioner: "pd.csi.storage.gke.io",
		Parameters:  map[string]string{"type": "hyperdisk-balanced"},
	}
}

func pvcDoc(name, size string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"resources": map[string]any{"requests": map[string]any{"storage": size}}},
	}}
}

func pvcSize(t *testing.T, d *unstructured.Unstructured) string {
	t.Helper()
	s, found, err := unstructured.NestedString(d.Object, "spec", "resources", "requests", "storage")
	require.NoError(t, err)
	require.True(t, found)
	return s
}

func TestValidatePVCFloors(t *testing.T) {
	// Undersized against a known hyperdisk floor, clamp flag set (non-interactive):
	// the doc's storage is rewritten up to 4Gi and validation passes.
	t.Run("clamp flag rewrites undersized doc to floor", func(t *testing.T) {
		doc := pvcDoc("spicebox-operator-memory", "1Gi")
		kc := k8sfake.NewSimpleClientset(hyperdiskDefaultSC())
		checks := []pvcToCheck{{doc: doc, tmplIndex: -1, name: "spicebox-operator-memory", className: "", clampable: true}}
		err := validatePVCFloors(context.Background(), kc, checks, nil, nil, true /*assumeYes*/, false /*isTTY*/, true /*clampFlag*/)
		require.NoError(t, err)
		assert.Equal(t, "4Gi", pvcSize(t, doc), "undersized request must be clamped to the floor")
	})

	// Undersized, non-interactive, NO clamp flag -> abort with an actionable error.
	t.Run("non-interactive without clamp aborts", func(t *testing.T) {
		doc := pvcDoc("spicebox-operator-memory", "1Gi")
		kc := k8sfake.NewSimpleClientset(hyperdiskDefaultSC())
		checks := []pvcToCheck{{doc: doc, tmplIndex: -1, name: "spicebox-operator-memory", className: "", clampable: true}}
		err := validatePVCFloors(context.Background(), kc, checks, nil, nil, true, false, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spicebox-operator-memory")
		assert.Contains(t, err.Error(), "4Gi")
		assert.Equal(t, "1Gi", pvcSize(t, doc), "abort must not mutate the doc")
	})

	// At/above the floor -> pass, unchanged.
	t.Run("at or above floor passes unchanged", func(t *testing.T) {
		doc := pvcDoc("spicebox-operator-memory", "10Gi")
		kc := k8sfake.NewSimpleClientset(hyperdiskDefaultSC())
		checks := []pvcToCheck{{doc: doc, tmplIndex: -1, name: "spicebox-operator-memory", className: "", clampable: true}}
		err := validatePVCFloors(context.Background(), kc, checks, nil, nil, true, false, false)
		require.NoError(t, err)
		assert.Equal(t, "10Gi", pvcSize(t, doc))
	})

	// Filestore multishare: an install-managed PVC below the 10 GiB share floor
	// is clamped up (the same StorageFloor knowledge the runtime workspace clamp
	// uses, applied here at install time).
	t.Run("filestore multishare clamps undersized install PVC to 10Gi", func(t *testing.T) {
		fsSC := &storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: "enterprise-multishare-rwx"},
			Provisioner: "filestore.csi.storage.gke.io",
			Parameters:  map[string]string{"multishare": "true"},
		}
		doc := pvcDoc("spicebox-operator-memory", "2Gi")
		kc := k8sfake.NewSimpleClientset(fsSC)
		checks := []pvcToCheck{{doc: doc, tmplIndex: -1, name: "spicebox-operator-memory", className: "enterprise-multishare-rwx", clampable: true}}
		err := validatePVCFloors(context.Background(), kc, checks, nil, nil, true, false, true)
		require.NoError(t, err)
		assert.Equal(t, "10Gi", pvcSize(t, doc))
	})

	// Unknown-floor class (local-path) -> pass even when tiny.
	t.Run("unknown floor never blocks", func(t *testing.T) {
		localSC := &storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: "standard", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}},
			Provisioner: "rancher.io/local-path",
		}
		doc := pvcDoc("ap-artifacts", "1Gi")
		kc := k8sfake.NewSimpleClientset(localSC)
		checks := []pvcToCheck{{doc: doc, tmplIndex: -1, name: "ap-artifacts", className: "", clampable: true}}
		err := validatePVCFloors(context.Background(), kc, checks, nil, nil, true, false, false)
		require.NoError(t, err)
		assert.Equal(t, "1Gi", pvcSize(t, doc))
	})

	// Missing class -> skipped (can't validate), no error.
	t.Run("missing class is skipped", func(t *testing.T) {
		doc := pvcDoc("x", "1Gi")
		kc := k8sfake.NewSimpleClientset() // no StorageClasses at all
		checks := []pvcToCheck{{doc: doc, tmplIndex: -1, name: "x", className: "does-not-exist", clampable: true}}
		err := validatePVCFloors(context.Background(), kc, checks, nil, nil, true, false, false)
		require.NoError(t, err)
	})

	// Abort message names an already-Pending existing PVC with the delete+recreate
	// remediation (spec §3.4 nicety).
	t.Run("abort hints delete+recreate for an existing Pending PVC", func(t *testing.T) {
		doc := pvcDoc("spicebox-operator-memory", "1Gi")
		doc.SetNamespace("agentprimitives-system")
		existing := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "spicebox-operator-memory", Namespace: "agentprimitives-system"},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
		}
		kc := k8sfake.NewSimpleClientset(hyperdiskDefaultSC(), existing)
		checks := []pvcToCheck{{doc: doc, tmplIndex: -1, name: "spicebox-operator-memory", className: "", clampable: true}}
		err := validatePVCFloors(context.Background(), kc, checks, nil, nil, true, false, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "delete the PVC")
	})

	// Interactive clamp: scripted "c" answer clamps and proceeds.
	t.Run("interactive c answer clamps", func(t *testing.T) {
		doc := pvcDoc("spicebox-operator-memory", "1Gi")
		kc := k8sfake.NewSimpleClientset(hyperdiskDefaultSC())
		checks := []pvcToCheck{{doc: doc, tmplIndex: -1, name: "spicebox-operator-memory", className: "", clampable: true}}
		rep := newPvcFloorStubReporter()
		err := validatePVCFloors(context.Background(), kc, checks, rep, strings.NewReader("c\n"), false, true, false)
		require.NoError(t, err)
		assert.Equal(t, "4Gi", pvcSize(t, doc))
	})

	// Clamp flag set in an interactive TTY must clamp directly, without
	// prompting: in=nil means there is no scripted stdin to read, so if the
	// code wrongly fell through to promptClampOrAbort it would either panic or
	// block reading from a nil source.
	t.Run("clamp flag clamps in TTY without prompting", func(t *testing.T) {
		doc := pvcDoc("spicebox-operator-memory", "1Gi")
		kc := k8sfake.NewSimpleClientset(hyperdiskDefaultSC())
		checks := []pvcToCheck{{doc: doc, tmplIndex: -1, name: "spicebox-operator-memory", className: "", clampable: true}}
		// in=nil + isTTY=true + clampFlag=true: must clamp without a prompt (no stdin to read).
		err := validatePVCFloors(context.Background(), kc, checks, newPvcFloorStubReporter(), nil, false /*assumeYes*/, true /*isTTY*/, true /*clampFlag*/)
		require.NoError(t, err)
		assert.Equal(t, "4Gi", pvcSize(t, doc))
	})

	// Stateful (validate-only) checks are never clampable: a violation must
	// abort even with the clamp flag set, and must never mutate the doc, since
	// clamping a stateful check's doc mutates a throwaway copy, not anything
	// install actually applies.
	t.Run("non-clampable violation aborts even with clamp flag", func(t *testing.T) {
		doc := pvcDoc("data-spicebox-neo4j-0", "1Gi")
		kc := k8sfake.NewSimpleClientset(hyperdiskDefaultSC())
		checks := []pvcToCheck{{doc: doc, tmplIndex: -1, name: "data-spicebox-neo4j-0", className: "", clampable: false}}
		err := validatePVCFloors(context.Background(), kc, checks, nil, nil, true /*assumeYes*/, false /*isTTY*/, true /*clampFlag*/)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot auto-clamp")
		assert.Equal(t, "1Gi", pvcSize(t, doc), "a non-clampable violation must never mutate the doc")
	})
}

func TestStatefulPVCChecks(t *testing.T) {
	checks, err := statefulPVCChecks("ap-stateful-hyperdisk")
	require.NoError(t, err)
	// Expect the postgres PVC and the neo4j volumeClaimTemplate.
	require.GreaterOrEqual(t, len(checks), 2)
	var sawPVC, sawTmpl bool
	for _, c := range checks {
		assert.Equal(t, "ap-stateful-hyperdisk", c.className)
		if c.tmplIndex < 0 {
			sawPVC = true
		} else {
			sawTmpl = true
		}
		size, ok := readPVCStorage(c)
		require.True(t, ok, "each stateful check must expose a storage request")
		q := resource.MustParse(size)
		assert.GreaterOrEqual(t, q.Cmp(resource.MustParse("4Gi")), 0, "bundled stateful sizes are >= 4Gi")
	}
	assert.True(t, sawPVC, "postgres PVC present")
	assert.True(t, sawTmpl, "neo4j volumeClaimTemplate present")
}

func TestBaseBundlePVCChecks(t *testing.T) {
	docs := []*unstructured.Unstructured{
		pvcDoc("spicebox-operator-memory", "10Gi"),
		{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x"}}},
		pvcDoc("ap-artifacts", "5Gi"),
	}
	checks := baseBundlePVCChecks(docs)
	require.Len(t, checks, 2)
	names := []string{checks[0].name, checks[1].name}
	assert.ElementsMatch(t, []string{"spicebox-operator-memory", "ap-artifacts"}, names)
	for _, c := range checks {
		assert.Equal(t, -1, c.tmplIndex)
		assert.Equal(t, "", c.className, "base-bundle PVCs resolve to the cluster default class")
	}
}

type pvcFloorStubReporter struct{ progress.Reporter }

func newPvcFloorStubReporter() *pvcFloorStubReporter { return &pvcFloorStubReporter{} }

// Suspend hands fn a discard writer and the scripted stdin is supplied by the
// caller via validatePVCFloors' `in` arg, so Suspend just needs to not draw.
func (r *pvcFloorStubReporter) Suspend(fn func(out io.Writer, in io.Reader)) { fn(io.Discard, nil) }
func (r *pvcFloorStubReporter) Info(string, ...any)                          {}
func (r *pvcFloorStubReporter) Warn(string, ...any)                          {}
