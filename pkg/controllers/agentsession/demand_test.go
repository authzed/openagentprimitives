package agentsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	_ "github.com/authzed/openagentprimitives/pkg/agent/harness/apnative" // the default harness the runner pod is built with
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	_ "github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/promptinjection" // a DetectorProvider inspector
	"github.com/authzed/openagentprimitives/pkg/controllers/admission"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	"github.com/authzed/openagentprimitives/pkg/platform/schedfit"
)

const demandMiB = int64(1024 * 1024)

// demandSpiceboxClass is a cluster-scoped SpiceboxClass whose sandbox
// container is limited to the given cpu/memory/ephemeral-storage.
func demandSpiceboxClass(name, cpu, mem, eph string) *spiceboxv1alpha1.SpiceboxClass {
	return &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image: "ghcr.io/example/sandbox:v1",
			Resources: spiceboxv1alpha1.SpiceboxResources{
				CPU:              resource.MustParse(cpu),
				Memory:           resource.MustParse(mem),
				EphemeralStorage: resource.MustParse(eph),
			},
		},
	}
}

// demandAgentClass declares one tool bundle per SpiceboxClass name, named
// "b-<class>", plus the given sidecar toolbox refs.
func demandAgentClass(bundleClasses []string, sidecars ...spiceboxv1alpha1.AgentClassSidecarToolboxRef) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo-class"}}
	for _, c := range bundleClasses {
		ac.Spec.ToolBundles = append(ac.Spec.ToolBundles, spiceboxv1alpha1.ToolBundle{Name: "b-" + c, Class: c})
	}
	ac.Spec.SidecarToolboxes = sidecars
	return ac
}

// demandSession is a session whose settings are resolved, with a
// prompt-injection inspector when withDetector is set.
func demandSession(t *testing.T, withDetector bool) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	eff := &spiceboxv1alpha1.EffectiveSettings{
		Model: spiceboxv1alpha1.ModelConfig{APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm", Key: "token"}},
	}
	if withDetector {
		eff.ContentInspectors = []spiceboxv1alpha1.ContentInspectorConfig{{
			ID:     "prompt-injection",
			Config: apiextv1.JSON{Raw: []byte(`{"detectorImage":"ghcr.io/example/detector:v1","port":9080}`)},
		}}
	}
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo-session", UID: "uid-1"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{EffectiveSettings: eff},
	}
}

func demandSidecarToolbox(name string, secretGated bool) *spiceboxv1alpha1.SidecarToolbox {
	tb := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:   name,
			Source: spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/example/" + name + ":v1"},
		},
	}
	if secretGated {
		tb.Spec.SecretInputs = []spiceboxv1alpha1.SidecarToolboxSecretInput{{Name: "TOKEN", Deliver: "env", From: "tok"}}
	}
	return tb
}

func demandClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// podDemandOf prices pod the way the admitter will: schedfit.EffectiveRequests
// over the spec the real builder produced.
func podDemandOf(pod *corev1.Pod) admission.PodDemand {
	cpu, mem, eph := schedfit.EffectiveRequests(&pod.Spec)
	return admission.PodDemand{Name: pod.Name, CPUMilli: cpu, MemBytes: mem, EphemeralBytes: eph}
}

func demandByName(d admission.Demand) map[string]admission.PodDemand {
	out := make(map[string]admission.PodDemand, len(d.Pods))
	for _, p := range d.Pods {
		out[p.Name] = p
	}
	return out
}

func TestDemandForMatchesBuiltPods(t *testing.T) {
	half := demandSpiceboxClass("half", "500m", "512Mi", "1Gi")
	one := demandSpiceboxClass("one", "1", "1Gi", "2Gi")
	ac := demandAgentClass([]string{"half", "one"})
	sess := demandSession(t, true)
	c := demandClient(t, half, one)

	got, err := DemandFor(context.Background(), c, sess, ac)
	require.NoError(t, err, "DemandFor")
	byName := demandByName(got)
	require.Len(t, got.Pods, 4, "runner + two bundles + one detector, got %v", got.Pods)

	// Each bundle's demand equals what the real sandbox builder produces for it.
	for _, tc := range []struct {
		class            *spiceboxv1alpha1.SpiceboxClass
		cpuMilli, memMiB int64
	}{
		{half, 500, 512},
		{one, 1000, 1024},
	} {
		bundle := spiceboxv1alpha1.ToolBundle{Name: "b-" + tc.class.Name, Class: tc.class.Name}
		pod, berr := podspec.Build(BuildBundleSession(sess, bundle, "", "", nil, nil, nil), tc.class.Spec)
		require.NoError(t, berr, "build bundle pod for %s", tc.class.Name)
		want := podDemandOf(pod)

		assert.Equal(t, want, byName[pod.Name], "bundle %s demand equals the built pod's effective requests", bundle.Name)
		assert.Equal(t, tc.cpuMilli, byName[pod.Name].CPUMilli,
			"bundle %s is priced at its class's cpu limit (requests default to limits)", bundle.Name)
		assert.Equal(t, tc.memMiB*demandMiB, byName[pod.Name].MemBytes, "bundle %s memory", bundle.Name)
	}

	// The detector's demand equals what cosidecar.BuildPod produces for it.
	dets, derr := resolveContentGuardDetectors(sess.Status.EffectiveSettings)
	require.NoError(t, derr, "resolve detectors")
	require.Len(t, dets, 1)
	detPod, _, perr := cosidecar.BuildPod(sess, detectorPodSpec(sess, dets[0], ""))
	require.NoError(t, perr, "build detector pod")
	assert.Equal(t, podDemandOf(detPod), byName[detPod.Name], "detector demand equals the built pod's")

	// The runner pod is present under its deterministic name.
	_, ok := byName[RunnerPodName(sess)]
	assert.True(t, ok, "runner pod %q is in the demand", RunnerPodName(sess))
}

func TestDemandForSidecars(t *testing.T) {
	ac := demandAgentClass(nil,
		spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "inpod", Ref: "inpod"},
		spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "gated", Ref: "gated"},
	)
	sess := demandSession(t, false)
	c := demandClient(t, demandSidecarToolbox("inpod", false), demandSidecarToolbox("gated", true))

	got, err := DemandFor(context.Background(), c, sess, ac)
	require.NoError(t, err, "DemandFor")
	byName := demandByName(got)

	assert.Len(t, got.Pods, 2, "runner + the one separate-pod sidecar; the in-pod sidecar rides in the runner pod")
	assert.Contains(t, byName, RunnerPodName(sess), "runner pod")
	assert.Contains(t, byName, SidecarPodName(sess, "gated"), "secret-gated sidecar gets its own pod")
	assert.NotContains(t, byName, SidecarPodName(sess, "inpod"), "in-pod sidecar has no pod of its own")
}

func TestDemandForErrors(t *testing.T) {
	cases := []struct {
		name     string
		ac       *spiceboxv1alpha1.AgentClass
		sess     func(t *testing.T) *spiceboxv1alpha1.AgentSession
		objs     []client.Object
		contains string
	}{
		{
			name:     "missing SpiceboxClass: error naming the class, not a zero demand",
			ac:       demandAgentClass([]string{"absent"}),
			sess:     func(t *testing.T) *spiceboxv1alpha1.AgentSession { return demandSession(t, false) },
			contains: `SpiceboxClass "absent"`,
		},
		{
			name:     "missing SidecarToolbox: error naming the toolbox",
			ac:       demandAgentClass(nil, spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "x", Ref: "ghost"}),
			sess:     func(t *testing.T) *spiceboxv1alpha1.AgentSession { return demandSession(t, false) },
			contains: `SidecarToolbox "ghost"`,
		},
		{
			name: "missing toolchain on a bundle's class: error naming the toolchain",
			ac:   demandAgentClass([]string{"withgo"}),
			sess: func(t *testing.T) *spiceboxv1alpha1.AgentSession { return demandSession(t, false) },
			objs: func() []client.Object {
				cls := demandSpiceboxClass("withgo", "500m", "512Mi", "1Gi")
				cls.Spec.Toolchains = []string{"go"}
				return []client.Object{cls}
			}(),
			contains: `"go"`,
		},
		{
			name: "settings not yet resolved: error, since detectors and backends derive from them",
			ac:   demandAgentClass(nil),
			sess: func(t *testing.T) *spiceboxv1alpha1.AgentSession {
				s := demandSession(t, false)
				s.Status.EffectiveSettings = nil
				return s
			},
			contains: "effectiveSettings",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DemandFor(context.Background(), demandClient(t, tc.objs...), tc.sess(t), tc.ac)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.contains)
			assert.Empty(t, got.Pods, "no partial demand on error")
		})
	}
}
