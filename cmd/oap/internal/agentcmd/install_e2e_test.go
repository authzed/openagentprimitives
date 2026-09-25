package agentcmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
	"github.com/authzed/openagentprimitives/test/oaptest"
)

// This file closes the one CLI gap R4's review flagged: `oap agent install`'s
// capacity-clamp question (pkg/platform/capacityfit, wired via
// cmd/oap/internal/agentcmd/install_capacity.go's NewCapacityQuestions) is unit-tested
// piece by piece, but nothing drove the actual `RunE` a user's shell invokes.
// apcmd.Globals.BundleFn (root.go) is the seam that makes that possible: it swaps
// the real kube.New(...) construction for a fake-backed *kube.Bundle, so
// these tests exercise loadInstallSource -> Preflight -> Resolve ->
// NewCapacityQuestions -> install.Install exactly as `oap agent install` does,
// asserting against what actually landed in the fake cluster rather than
// only what the CLI printed (a test that only reads stdout would still pass
// if the install silently applied nothing).

const (
	capacityFixtureAgent = "cap-fixture-agent" // installName defaults to this (no --name passed)
	capacityFixtureClass = "big-class"         // SpiceboxClass name; a made-up fixture name, never an example's
	capacityFixtureNS    = "cap-e2e-ns"
)

// capacityBundleDir writes a minimal, self-contained .oap source folder: one
// AgentClass and one SpiceboxClass declaring memory=mem. It is package-local
// (pkg/platform/oap/install/extraquestions_test.go's alternative to the shared
// oaptest fixture, per this task's brief) rather than built on
// oaptest.WriteBundle, because the shared fixture's manifest carries its own
// required questions (a secret with no default) that would force every
// subtest below to answer bundle concerns unrelated to capacity. The
// manifest here declares none, so each subtest's --set/--fit-resources
// arguments speak only to the property under test.
func capacityBundleDir(t *testing.T, mem string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "manifests"), 0o755))
	// agent.name is inherited from the bundled AgentClass below.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "oap.yaml"), []byte(`
oapFormatVersion: "1"
agent:
  version: "1.0.0"
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifests", "agent.yaml"), []byte(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: `+capacityFixtureAgent+`
spec:
  description: Fixture agent for the capacity-question e2e tests.
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifests", "spicebox.yaml"), []byte(fmt.Sprintf(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SpiceboxClass
metadata:
  name: %s
spec:
  resources:
    memory: %q
`, capacityFixtureClass, mem)), 0o644))
	return dir
}

// vmNode and vmPod model the "desktop VM" cluster the brief asks for: one
// node with 4Gi allocatable memory, plus pods booking 2Gi of it, so the
// ceiling (4Gi) and the headroom-driven suggestion (2Gi) are different
// numbers — proving the suggestion comes from headroom, not the raw ceiling.
func vmNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-node"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
	}
}

func vmPod(name, mem string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
		Spec: corev1.PodSpec{
			NodeName: "vm-node",
			Containers: []corev1.Container{{
				Name: "main",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(mem)},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// tinyNode has less allocatable memory than the default tmpSize+workSize+
// margin floor (50Mi+100Mi+512Mi=662Mi) — used only by the floor-above-
// ceiling case, where no value could ever be offered.
func tinyNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "tiny-node"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
	}
}

// installedSpiceboxClassCR seeds the fake Controller client with a
// SpiceboxClass that already exists on the cluster from a prior install of
// THIS SAME bundle. It carries instance.LabelInstall=capacityFixtureAgent —
// without it, install.Install's ownership guard (planBundledResources)
// refuses the re-apply as seizing a foreign object it never created (see
// pkg/platform/oap/install/apply.go's checkResourceOwnership) — a real install would
// hit the exact same guard on a genuine re-install.
func installedSpiceboxClassCR(mem string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": spiceboxv1alpha1.SchemeGroupVersion.String(),
		"kind":       "SpiceboxClass",
		"metadata": map[string]any{
			"name":   capacityFixtureClass,
			"labels": map[string]any{instance.LabelInstall: capacityFixtureAgent},
		},
		"spec": map[string]any{"resources": map[string]any{"memory": mem}},
	}}
}

// fakeBundle returns a *kube.Bundle backed entirely by fakes: a client-go
// fake clientset for the node/pod capacity reads (cloud.Strategy.
// SchedulingCeiling), and a controller-runtime fake client for the
// "installed" SpiceboxClass lookup and install.Install's applies. objs is
// dispatched by type — *corev1.Node/*corev1.Pod feed the Typed clientset,
// everything else (an already-installed SpiceboxClass, typically) seeds the
// Controller client — so one variadic call site covers every case below
// instead of two separate parameter lists.
//
// The Controller client deliberately uses an EMPTY scheme, not kube.Scheme —
// confirmed by running this file's --set case against both: registering the
// typed v1alpha1.SpiceboxClass makes the fake client's SSA-apply machinery
// round-trip the unstructured object through the typed Go struct, which
// discards resource.Quantity's cached original text ("3.5Gi" came back as
// "3584Mi"). That round trip is a FAKE-CLIENT artifact, not a real apiserver
// behavior, and registering the typed scheme would make the fake LESS
// faithful, not more: a live SpiceboxClass's spec.resources.memory is stored
// as an unstructured CRD field (no built-in "quantity" OpenAPI type — CRDs
// only get x-kubernetes-int-or-string on fields explicitly annotated that
// way), so the apiserver keeps whatever string bytes were posted; it never
// canonicalizes them the way a typed Go round-trip does. pkg/platform/oap/install's
// own tests (extraquestions_test.go, apply_idempotency_test.go) and
// agent_install_capacity_test.go already use an empty scheme for exactly this
// reason; every CR here already carries an explicit GVK, which is all the
// fake client needs.
//
// DYNAMIC IS ALWAYS POPULATED, because kube.New always populates it. A Bundle
// missing it is not a smaller fixture, it is a different one: the channel
// half of install checks Dynamic for nil and refuses the whole pass with
// "this install has no cluster connection to create them with" — a sentence
// no production run can produce, arriving from a fixture gap. That failure is
// indistinguishable from a real one at the command's exit code, which is how
// a fixture stops modelling the thing it stands in for. The list kinds are
// the ones a channel wizard applies through; the capacity cases never touch
// it.
//
// TWO TYPES ARE REGISTERED ON THE OTHERWISE-EMPTY CONTROLLER SCHEME, and
// SpiceboxClass is deliberately not among them. channelplan.PlanChannels does
// a TYPED read of each — Channel, for whether the declared name is already
// taken, and ConfigMap, for webd's published external URL — and an
// unregistered type there is not a soft miss: the Channel lookup returns "no
// kind is registered", which lookupWiring reports as an error and which fails
// the install. Registering these two cannot disturb the quantity behaviour
// argued above, because that argument is about SpiceboxClass's unstructured
// round trip and neither of these is SpiceboxClass.
func fakeBundle(t *testing.T, objs ...client.Object) *kube.Bundle {
	t.Helper()
	var clusterObjs []runtime.Object
	var ctrlObjs []client.Object
	for _, o := range objs {
		switch o.(type) {
		case *corev1.Node, *corev1.Pod:
			clusterObjs = append(clusterObjs, o)
		default:
			ctrlObjs = append(ctrlObjs, o)
		}
	}
	return &kube.Bundle{
		Typed:      k8sfake.NewSimpleClientset(clusterObjs...),
		Controller: fake.NewClientBuilder().WithScheme(planReadableScheme(t)).WithObjects(ctrlObjs...).Build(),
		Dynamic: dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
			map[schema.GroupVersionResource]string{
				channelGVR:    "ChannelList",
				agentClassGVR: "AgentClassList",
				secretGVR:     "SecretList",
			}),
		Namespace: capacityFixtureNS,
	}
}

// planReadableScheme is the empty scheme plus exactly the two types
// channelplan.PlanChannels reads through the controller client. See
// fakeBundle's doc for why it is these two and not kube.Scheme.
// It registers Channel by hand rather than calling
// spiceboxv1alpha1.AddToScheme, which would bring SpiceboxClass with it and
// re-introduce the typed round trip the quantity cases are measuring.
func planReadableScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	gv := spiceboxv1alpha1.SchemeGroupVersion
	s.AddKnownTypes(gv, &spiceboxv1alpha1.Channel{}, &spiceboxv1alpha1.ChannelList{})
	metav1.AddToGroupVersion(s, gv)
	return s
}

// fakeDynamicOf is the dynamic client fakeBundle built, for a test that needs
// to assert on what was applied through it rather than only on what the
// command printed. The assertion is safe by construction — fakeBundle is the
// only thing that ever sets this field on a fixture — and is checked rather
// than bare so a future change there fails here by name.
func fakeDynamicOf(t *testing.T, kb *kube.Bundle) *dynfake.FakeDynamicClient {
	t.Helper()
	dyn, ok := kb.Dynamic.(*dynfake.FakeDynamicClient)
	require.True(t, ok, "fakeBundle must back Dynamic with a fake dynamic client")
	return dyn
}

// forceNonInteractiveStdin pins the process's real os.Stdin to a pipe for the
// rest of the calling test. agent_install.go's extra-question phase reads
// apcmd.StdinIsInteractive(os.Stdin) directly rather than cmd.InOrStdin(), so
// cobra's SetIn cannot influence it — and this harness's own stdin may or may
// not be a TTY depending on how the test runner was invoked (observed both
// ways across environments). Every case below that expects the non-
// interactive Default-fallback (no --set, no --fit-resources) needs a
// deterministic answer regardless of that ambient state, so it pins stdin to
// a pipe — never a character device, per
// TestStdinIsInteractive_DetectsPipe in agent_chat_test.go — rather than
// trusting the environment.
func forceNonInteractiveStdin(t *testing.T) {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		_ = r.Close()
		_ = w.Close()
	})
}

// runAgentInstall builds the `oap agent` command tree over g (whose BundleFn the
// caller has already wired to a fake cluster) and executes
// `install <dir> <extra...>`, returning combined stdout+stderr and
// RunE's error. It drives cobra's Execute — not install.Install directly —
// because the whole point of this file is proving the CLI's own wiring
// (flag parsing, loadInstallSource, Preflight, Resolve, the capacity hook)
// behaves end to end.
func runAgentInstall(t *testing.T, g *apcmd.Globals, dir string, extra ...string) (string, error) {
	t.Helper()
	root := NewCmd(g)
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"install", dir}, extra...))
	err := root.Execute()
	return buf.String(), err
}

// getSpiceboxClass reads back the applied CR from the fake Controller
// client — the assertion every case below anchors on, per the brief's core
// requirement: a test that only reads stdout would pass even if nothing
// were actually applied.
func getSpiceboxClass(t *testing.T, kb *kube.Bundle) (*unstructured.Unstructured, error) {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetAPIVersion(spiceboxv1alpha1.SchemeGroupVersion.String())
	got.SetKind("SpiceboxClass")
	err := kb.Controller.Get(context.Background(), client.ObjectKey{Name: capacityFixtureClass}, got)
	return got, err
}

func memoryField(t *testing.T, cr *unstructured.Unstructured) string {
	t.Helper()
	v, found, err := unstructured.NestedString(cr.Object, "spec", "resources", "memory")
	require.NoError(t, err)
	require.True(t, found, "spec.resources.memory must be set on the applied CR")
	return v
}

func TestAgentInstall_CapacityQuestion_EndToEnd(t *testing.T) {
	forceNonInteractiveStdin(t)

	t.Run("class fits the cluster: no prompt, no clamp, CR keeps its declared value", func(t *testing.T) {
		kb := fakeBundle(t, vmNode(), vmPod("p1", "1Gi"), vmPod("p2", "1Gi"))
		g := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) { return kb, nil }}
		dir := capacityBundleDir(t, "512Mi")

		out, err := runAgentInstall(t, g, dir)
		require.NoErrorf(t, err, "a class within the ceiling must install cleanly; out=%s", out)
		assert.NotContains(t, out, "capacity", "a class that already fits must never mention a capacity question")

		cr, gerr := getSpiceboxClass(t, kb)
		require.NoError(t, gerr, "the bundled SpiceboxClass must be applied")
		assert.Equal(t, "512Mi", memoryField(t, cr), "an unclamped class keeps exactly its declared value")
	})

	t.Run("--fit-resources clamps an oversized class to the rounded headroom suggestion without prompting", func(t *testing.T) {
		kb := fakeBundle(t, vmNode(), vmPod("p1", "1Gi"), vmPod("p2", "1Gi"))
		g := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) { return kb, nil }}
		dir := capacityBundleDir(t, "8Gi") // exceeds the 4Gi ceiling

		out, err := runAgentInstall(t, g, dir, "--fit-resources")
		require.NoErrorf(t, err, "fit-resources must clamp rather than fail; out=%s", out)
		assert.Contains(t, out, "2Gi", "the clamp must be reported (2Gi = headroom 2Gi rounded down to 256Mi)")

		cr, gerr := getSpiceboxClass(t, kb)
		require.NoError(t, gerr, "the bundled SpiceboxClass must still be applied")
		assert.Equal(t, "2Gi", memoryField(t, cr), "the applied CR must carry the rounded-down headroom suggestion")
	})

	t.Run("--set capacity.<class>.memory answers non-interactively with the exact literal supplied", func(t *testing.T) {
		kb := fakeBundle(t, vmNode(), vmPod("p1", "1Gi"), vmPod("p2", "1Gi"))
		g := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) { return kb, nil }}
		dir := capacityBundleDir(t, "8Gi") // exceeds the 4Gi ceiling

		// 3.5Gi is deliberately NOT a whole-Gi value: resource.NewQuantity's own
		// canonical BinarySI rendering of these bytes is "3584Mi", not "3.5Gi".
		// A regression that re-derived the answer through int64 (the prior
		// Critical) would silently rewrite the text; asserting the literal
		// catches that even though today's code never re-parses a --set answer.
		out, err := runAgentInstall(t, g, dir, "--set", "capacity.big-class.memory=3.5Gi")
		require.NoErrorf(t, err, "an explicit --set must satisfy the capacity question; out=%s", out)

		cr, gerr := getSpiceboxClass(t, kb)
		require.NoError(t, gerr, "the bundled SpiceboxClass must be applied")
		assert.Equal(t, "3.5Gi", memoryField(t, cr), "the applied CR must carry the --set value byte-for-byte, not a re-rendered quantity")
	})

	t.Run("non-interactive with neither --fit-resources nor --set: aborts naming the class, no CR applied", func(t *testing.T) {
		// This was a real regression (see the task report's "Fix round 1"):
		// pkg/platform/capacityfit's capacityQuestion ALWAYS sets a non-nil Default, and
		// install.Resolve's non-interactive branch used to consume it BEFORE
		// ever reaching the "missing required question" check — so a capacity
		// question could never land in Resolve's missingRequired list, and a
		// bare non-interactive `oap agent install` would silently ship a class
		// SMALLER than the bundle declared, with only a warning. The approved
		// policy is CLI-only consent: requireCapacityConsent
		// (agent_install_capacity.go) wraps the hook so any question --set
		// doesn't already answer aborts instead of falling through to that
		// Default. Desktop and admind deliberately keep auto-fitting — this
		// abort is CLI-specific (see requireCapacityConsent's doc comment).
		kb := fakeBundle(t, vmNode(), vmPod("p1", "1Gi"), vmPod("p2", "1Gi"))
		g := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) { return kb, nil }}
		dir := capacityBundleDir(t, "8Gi") // exceeds the 4Gi ceiling

		out, err := runAgentInstall(t, g, dir)
		require.Errorf(t, err, "no consent (no --fit-resources, no --set) must abort rather than silently clamp; out=%s", out)
		assert.Contains(t, err.Error(), capacityFixtureClass, "the error must name the class")
		assert.Contains(t, err.Error(), "--fit-resources", "the error must offer the auto-fit remediation")
		assert.Contains(t, err.Error(), "capacity.big-class.memory", "the error must offer the exact --set key to answer")

		_, gerr := getSpiceboxClass(t, kb)
		assert.Error(t, gerr, "no CR may be applied when the operator never consented to a clamp")
	})

	t.Run("class fits: --set capacity.<class>.<dim> naming no synthesized question errors, no CR applied", func(t *testing.T) {
		// This is the finding-1 regression: apply.go used to gate its ENTIRE
		// extra-question Resolve() call (which is also what runs
		// rejectUnknownKeys) on len(extra) > 0. A class that already fits gets
		// ZERO synthesized questions, so a --set capacity.* key — a typo'd class
		// name, or a stale override from a since-shrunk class — sailed straight
		// through unvalidated: the operator's explicit --set silently never
		// applied, with nothing printed. Fixed by running Resolve whenever
		// there is opts.Sets to validate, not only when there is a question to
		// answer it against.
		kb := fakeBundle(t, vmNode(), vmPod("p1", "1Gi"), vmPod("p2", "1Gi"))
		g := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) { return kb, nil }}
		dir := capacityBundleDir(t, "512Mi") // fits the 4Gi ceiling: capacityHook synthesizes NO question

		out, err := runAgentInstall(t, g, dir, "--set", "capacity.big-class.memory=2Gi")
		require.Errorf(t, err, "a --set capacity key naming no synthesized question must be rejected, not silently dropped; out=%s", out)
		assert.Contains(t, err.Error(), "capacity.big-class.memory", "the error must name the offending --set key")

		_, gerr := getSpiceboxClass(t, kb)
		assert.Error(t, gerr, "nothing may be applied when a stray capacity --set is rejected")
	})

	t.Run("--fit-resources on that SAME non-TTY input clamps successfully (pins the two apart)", func(t *testing.T) {
		// Identical fixture and args to the case above, plus --fit-resources —
		// proving the flag is what makes non-interactive resolution possible at
		// all now, not a no-op (which it silently was before the fix: forcing
		// Interactive=false was equivalent to leaving it non-interactive either
		// way, since both used to fall through to the same Default).
		kb := fakeBundle(t, vmNode(), vmPod("p1", "1Gi"), vmPod("p2", "1Gi"))
		g := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) { return kb, nil }}
		dir := capacityBundleDir(t, "8Gi") // exceeds the 4Gi ceiling

		out, err := runAgentInstall(t, g, dir, "--fit-resources")
		require.NoErrorf(t, err, "--fit-resources is the explicit consent that lets this proceed; out=%s", out)
		assert.Contains(t, out, "lowering", "the auto-adopted clamp must still be reported as a warning, not applied silently")

		cr, gerr := getSpiceboxClass(t, kb)
		require.NoError(t, gerr, "the class must be applied once --fit-resources gives consent")
		assert.Equal(t, "2Gi", memoryField(t, cr), "with consent, the suggested headroom-fit default is what lands")
	})

	t.Run("floor above ceiling: install fails naming the class and applies no CR", func(t *testing.T) {
		kb := fakeBundle(t, tinyNode())
		g := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) { return kb, nil }}
		dir := capacityBundleDir(t, "8Gi") // exceeds tinyNode's 512Mi ceiling

		out, err := runAgentInstall(t, g, dir)
		require.Errorf(t, err, "a class whose floor exceeds the ceiling must abort; out=%s", out)
		assert.Contains(t, err.Error(), capacityFixtureClass, "the error must name the class that cannot fit")

		_, gerr := getSpiceboxClass(t, kb)
		assert.Error(t, gerr, "no CR may be applied when the class cannot fit at all")
	})

	t.Run("unknown ceiling (no nodes): install proceeds unchanged with one skip notice", func(t *testing.T) {
		kb := fakeBundle(t) // no nodes at all
		g := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) { return kb, nil }}
		dir := capacityBundleDir(t, "8Gi") // would exceed any real ceiling, but none is provable here

		out, err := runAgentInstall(t, g, dir)
		require.NoErrorf(t, err, "an unprovable ceiling must never block an install; out=%s", out)
		assert.Contains(t, out, "capacity check skipped", "the skip reason must reach the operator")

		cr, gerr := getSpiceboxClass(t, kb)
		require.NoError(t, gerr, "the class must still be applied")
		assert.Equal(t, "8Gi", memoryField(t, cr), "with no provable ceiling the class is applied exactly as declared")
	})

	t.Run("re-install adopts the already-installed in-range, non-granular value verbatim (no flag needed)", func(t *testing.T) {
		// 1.8Gi is in [floor, ceiling] but not on the 256Mi rounding granularity —
		// the end-to-end guard that a re-install is a byte-identical SSA no-op
		// (pkg/platform/capacityfit's installedDefault) even when driven through the real
		// CLI, not just capacityfit's own unit tests. This is the consent
		// carve-out (oap.Question.Unchanged, set by capacityfit and read by
		// requireCapacityConsent): the offered Default IS the live value read
		// back verbatim, so applying it changes NOTHING on the cluster — a CI
		// job re-installing an unchanged bundle must keep working with no
		// --fit-resources/--set at all, or the whole point of the adoption path
		// (a byte-identical re-apply) is defeated by a consent prompt with
		// nothing to consent to.
		existing := installedSpiceboxClassCR("1.8Gi")
		kb := fakeBundle(t, vmNode(), vmPod("p1", "1Gi"), vmPod("p2", "1Gi"), existing)
		g := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) { return kb, nil }}
		dir := capacityBundleDir(t, "16Gi") // the bundle's own ask still exceeds the ceiling

		out, err := runAgentInstall(t, g, dir)
		require.NoErrorf(t, err, "adopting the installed value must succeed without prompting or a flag; out=%s", out)
		assert.Contains(t, out, "keeping", "the notice must explain the installed value was kept")
		assert.Contains(t, out, "1.8Gi", "the notice must name the exact value kept")

		cr, gerr := getSpiceboxClass(t, kb)
		require.NoError(t, gerr, "the re-applied SpiceboxClass must still be present")
		assert.Equal(t, "1.8Gi", memoryField(t, cr), "the applied CR must keep the installed value byte-for-byte, not a re-rendered quantity")
	})

	t.Run("re-install where the installed value no longer fits (ceiling shrank): still aborts, no CR applied", func(t *testing.T) {
		// This is the case that PROVES the carve-out is scoped to "no new
		// decision", not to "any re-install of an already-installed class":
		// 8Gi was fine on whatever cluster it was installed against, but this
		// cluster's ceiling is only 4Gi, so installedDefault can no longer
		// adopt it — Questions() falls through to a FRESH suggestFit clamp
		// (oap.Question.Unchanged=false), which IS a new decision and still
		// needs consent exactly like a brand-new oversized class does.
		existing := installedSpiceboxClassCR("8Gi") // now exceeds the 4Gi ceiling
		kb := fakeBundle(t, vmNode(), vmPod("p1", "1Gi"), vmPod("p2", "1Gi"), existing)
		g := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) { return kb, nil }}
		dir := capacityBundleDir(t, "16Gi") // still exceeds the 4Gi ceiling

		out, err := runAgentInstall(t, g, dir)
		require.Errorf(t, err, "an installed value that no longer fits is a NEW decision and must still require consent; out=%s", out)
		assert.Contains(t, err.Error(), capacityFixtureClass, "the error must name the class")

		cr, gerr := getSpiceboxClass(t, kb)
		// The install aborts before any write, so the STALE pre-existing value
		// must survive untouched — the assertion that distinguishes "aborted"
		// from "silently re-applied the old value anyway".
		require.NoError(t, gerr, "the previously-installed CR is untouched, not deleted")
		assert.Equal(t, "8Gi", memoryField(t, cr), "the stale installed value must be untouched — no write landed")
	})
}

// This section closes the fix-round-2 gap flagged on Task 9: checkSkillsShape
// (install.go) is unit-tested in isolation (TestCheckSkillsShape,
// install_test.go), but nothing had driven the real `oap agent install` RunE
// against a bundle carrying the pre-migration skills shape — the only
// evidence it was wired in ahead of any cluster write was a manual smoke
// test recorded in the task report, never an automated one. This is exactly
// the shape of gap this file already exists to close for the capacity
// question (see the file-level comment above): a test that only inspects
// stdout would still pass even if the guard were missing and the write
// landed anyway.

const (
	skillsShapeFixtureAgent = "shape-fixture-agent" // installName defaults to this (no --name passed)
	skillsShapeFixtureClass = "shape-fixture-class"
)

// skillsShapeBundleDir writes a minimal, self-contained .oap source folder:
// one AgentClass whose spec.skills is exactly skills — a raw YAML list body,
// spliced in verbatim, so the caller can supply either the pre-migration
// bare-string shape or the current {name, ref, target} object shape.
// Package-local rather than built on oaptest.WriteBundle, matching
// capacityBundleDir's own rationale: the shared fixture's manifest carries
// required questions unrelated to skills that would only add noise here.
func skillsShapeBundleDir(t *testing.T, skills string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "manifests"), 0o755))
	// agent.name is inherited from the bundled AgentClass below.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "oap.yaml"), []byte(`
oapFormatVersion: "1"
agent:
  version: "1.0.0"
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifests", "agent.yaml"), []byte(fmt.Sprintf(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: %s
spec:
  description: Fixture agent for the skills-shape install e2e test.
  systemPrompt:
    inline: "You are a fixture agent used only by this test."
  skills:
%s
`, skillsShapeFixtureClass, skills)), 0o644))
	return dir
}

// getAgentClass reads back the applied CR from the fake Controller client —
// mirrors getSpiceboxClass above for the AgentClass kind this test bundles.
func getAgentClass(t *testing.T, kb *kube.Bundle) (*unstructured.Unstructured, error) {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetAPIVersion(spiceboxv1alpha1.SchemeGroupVersion.String())
	got.SetKind("AgentClass")
	err := kb.Controller.Get(context.Background(), client.ObjectKey{Name: skillsShapeFixtureClass, Namespace: capacityFixtureNS}, got)
	return got, err
}

// TestAgentInstall_OldSkillsShapeRefusesBeforeAnyClusterWrite drives the
// real `oap agent install` command — not checkSkillsShape directly — against
// a fake cluster, so a future change that removes or misplaces the call site
// in RunE is caught by an assertion on what actually reached the cluster,
// not just on what checkSkillsShape returns in isolation.
func TestAgentInstall_OldSkillsShapeRefusesBeforeAnyClusterWrite(t *testing.T) {
	forceNonInteractiveStdin(t)

	cases := []struct {
		name        string
		skills      string
		wantErr     bool
		wantApplied bool
	}{
		{
			name:    "old bare-string shape: refused, the AgentClass never reaches the cluster",
			skills:  "    - github.com/demo-org/demo-skills//skills/code-review@v1.0.0",
			wantErr: true,
		},
		{
			name:        "current {name, ref, target} shape: install proceeds, the AgentClass is applied",
			skills:      "    - name: code-review\n      ref: \"github.com/demo-org/demo-skills//skills/code-review@v1.0.0\"\n      target: sandbox",
			wantApplied: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kb := fakeBundle(t) // no nodes/pods: this bundle carries no SpiceboxClass, so no capacity question is ever synthesized
			g := &apcmd.Globals{BundleFn: func() (*kube.Bundle, error) { return kb, nil }}
			dir := skillsShapeBundleDir(t, tc.skills)

			out, err := runAgentInstall(t, g, dir)

			if tc.wantErr {
				require.Errorf(t, err, "the old shape must refuse the install; out=%s", out)
				assert.Contains(t, err.Error(), "name: code-review",
					"the rewrite must be shown, not merely a problem reported")
				assert.Contains(t, err.Error(), "ref: github.com/demo-org/demo-skills//skills/code-review@v1.0.0")
			} else {
				require.NoErrorf(t, err, "the current shape must install cleanly; out=%s", out)
			}

			_, gerr := getAgentClass(t, kb)
			if tc.wantApplied {
				assert.NoError(t, gerr, "the AgentClass must be applied when the shape is valid")
			} else {
				assert.True(t, apierrors.IsNotFound(gerr),
					"the AgentClass must never reach the cluster when the shape is refused; got err=%v", gerr)
			}
		})
	}
}

// TestAgentInstall_DependencySynthesizedSecretAndRoster_EndToEnd drives the
// command boundary over the two graph rewrites that are easiest for a surface
// adapter to miss: a child's required-secret probe must use its physical
// private name, and the parent's logical roster entry must point at that same
// private AgentClass.
func TestAgentInstall_DependencySynthesizedSecretAndRoster_EndToEnd(t *testing.T) {
	forceNonInteractiveStdin(t)
	kb := fakeBundle(t)
	dir := graphInstallFixture(t, graphInstallFixtureOptions{})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dependencies", graphFixtureChild, "manifests", "agent.yaml"), []byte(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: reviewer
spec:
  description: Child graph fixture.
  agentIdentity: reviewer
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentIdentity
metadata:
  name: reviewer
spec:
  credentials:
    - name: fangs
      type: static
      static:
        secretRef:
          name: review-token
          key: token
`), 0o644))
	const secretValue = "dependency-e2e-secret"

	out, err := runAgentInstall(t, graphGlobals(kb), dir,
		"--set", "agents.reviewer.requires.secrets.review-token.token="+secretValue)

	require.NoErrorf(t, err, "the graph secret and roster must install together; out=%s", out)
	root := graphAgentClass(t, kb, graphFixtureRoot)
	roster, found, rosterErr := unstructured.NestedStringSlice(root.Object, "spec", "subagents")
	require.NoError(t, rosterErr)
	require.True(t, found)
	assert.Equal(t, []string{"test-coordinator-reviewer"}, roster)

	var secret corev1.Secret
	require.NoError(t, kb.Controller.Get(context.Background(), client.ObjectKey{
		Namespace: capacityFixtureNS,
		Name:      "test-coordinator-reviewer-review-token",
	}, &secret))
	assert.Equal(t, secretValue, secret.StringData["token"],
		"the controller-runtime fake preserves stringData without apiserver conversion")
	assert.NotContains(t, out, secretValue)
}

// TestAgentInstall_DependencyBundlesInstallDirectlyAndAsAComposition keeps the
// shared dependency fixture on the same command path users run. The child is a
// complete OAP in its own right, while installing the parent must bring in its
// private child without a separate pre-install step, rewrite the parent's
// delegation contract, and uninstall the graph.
func TestAgentInstall_DependencyBundlesInstallDirectlyAndAsAComposition(t *testing.T) {
	forceNonInteractiveStdin(t)
	fixtureDir := oaptest.WriteDependencyBundle(t)
	childDir := filepath.Join(fixtureDir, "dependencies", oaptest.DependencyChildName)

	direct := fakeBundle(t)
	directOut, err := runAgentInstall(t, graphGlobals(direct), childDir)
	require.NoErrorf(t, err, "the nested child OAP must install independently; out=%s", directOut)
	child := graphAgentClass(t, direct, oaptest.DependencyChildName)
	childPrompt, found, promptErr := unstructured.NestedString(child.Object, "spec", "systemPrompt", "inline")
	require.NoError(t, promptErr)
	require.True(t, found)
	assert.Contains(t, childPrompt, "fixture child translator",
		"the independent child must preserve its authored prompt")
	directUninstallOut, err := runAgentUninstall(t, direct, oaptest.DependencyChildName)
	require.NoErrorf(t, err, "the independently installed child must uninstall through oap; out=%s", directUninstallOut)
	assert.Contains(t, directUninstallOut, "1 resources deleted for install "+oaptest.DependencyChildName)
	directChild := &unstructured.Unstructured{}
	directChild.SetAPIVersion(spiceboxv1alpha1.SchemeGroupVersion.String())
	directChild.SetKind("AgentClass")
	directGetErr := direct.Controller.Get(context.Background(), client.ObjectKey{
		Namespace: capacityFixtureNS,
		Name:      oaptest.DependencyChildName,
	}, directChild)
	assert.True(t, apierrors.IsNotFound(directGetErr), "the direct child must be removed by uninstall")

	composed := fakeBundle(t)
	installOut, err := runAgentInstall(t, graphGlobals(composed), fixtureDir)
	require.NoErrorf(t, err, "the parent OAP must install its embedded child; out=%s", installOut)
	assert.Contains(t, installOut, oaptest.DependencyChildName+" → "+oaptest.DependencyPhysicalChildName)

	root := graphAgentClass(t, composed, oaptest.DependencyRootName)
	graphAgentClass(t, composed, oaptest.DependencyPhysicalChildName)
	roster, found, rosterErr := unstructured.NestedStringSlice(root.Object, "spec", "subagents")
	require.NoError(t, rosterErr)
	require.True(t, found)
	assert.Equal(t, []string{oaptest.DependencyPhysicalChildName}, roster)
	modes, found, modesErr := unstructured.NestedStringSlice(root.Object,
		"spec", "subagentModes", oaptest.DependencyPhysicalChildName)
	require.NoError(t, modesErr)
	require.True(t, found)
	assert.Equal(t, []string{"single_turn", "task"}, modes)
	_, found, capabilityErr := unstructured.NestedMap(root.Object, "spec", "capabilities", "subagents")
	require.NoError(t, capabilityErr)
	assert.True(t, found, "the parent must retain its subagent capability")
	rootPrompt, found, rootPromptErr := unstructured.NestedString(root.Object, "spec", "systemPrompt", "inline")
	require.NoError(t, rootPromptErr)
	require.True(t, found)
	assert.Contains(t, rootPrompt, "fixture coordinator")

	uninstallOut, err := runAgentUninstall(t, composed, oaptest.DependencyRootName)
	require.NoErrorf(t, err, "the composed install must uninstall as one graph; out=%s", uninstallOut)
	assert.Contains(t, uninstallOut, "2 resources deleted for install "+oaptest.DependencyRootName)
	for _, name := range []string{oaptest.DependencyRootName, oaptest.DependencyPhysicalChildName} {
		got := &unstructured.Unstructured{}
		got.SetAPIVersion(spiceboxv1alpha1.SchemeGroupVersion.String())
		got.SetKind("AgentClass")
		getErr := composed.Controller.Get(context.Background(), client.ObjectKey{
			Namespace: capacityFixtureNS,
			Name:      name,
		}, got)
		assert.True(t, apierrors.IsNotFound(getErr), "%s must be removed by graph uninstall", name)
	}
}
