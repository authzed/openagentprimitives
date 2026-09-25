package workshopprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// buildScheme returns a fresh scheme carrying every type prober.go's fake-
// client tests create: corev1 (Pod/ConfigMap), networkingv1 (NetworkPolicy),
// and the WorkshopProbe API group itself.
func buildScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, networkingv1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	return scheme
}

// newFakeClient builds a fake client carrying buildScheme's types, with the
// deduced type converter forced for every SSA apply. probeWith SSA-applies
// the probe NetworkPolicy (BuildProbeNetworkPolicy); the fake client's
// DEFAULT converter pair rejects that apply with "expected objects with
// types from the same schema" — the deduced converter handles it. A
// fake-client-only limitation (the same apply works against a real
// apiserver); see pkg/controllers/agentsession/bundle_epoch_reconcile_test.go's
// fakeReconciler for the same workaround with the same justification.
//
// WithStatusSubresource(&v1alpha1.WorkshopProbe{}) is required for the
// controller tests (controller_test.go), which write status via
// Client.Status().Update(): without it the fake client's tracker treats
// EVERY status subresource write as a 404 (versioned_tracker.go's
// updateObject falls through to apierrors.NewNotFound whenever isStatus is
// true and the GVK was never registered as carrying a status subresource) —
// a fake-client-only registration requirement, not a statement about
// WorkshopProbe's own CRD (which does declare
// +kubebuilder:subresource:status). Harmless for prober.go's own tests,
// which never call Status() at all.
func newFakeClient(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithStatusSubresource(&v1alpha1.WorkshopProbe{}).
		WithScheme(buildScheme(t)).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		Build()
}

func TestMapProbedTools(t *testing.T) {
	in := []probe.Tool{{Name: "a", Description: "da", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	got := mapProbedTools(in)
	require.Len(t, got, 1)
	assert.Equal(t, "a", got[0].Name)
	assert.Equal(t, "da", got[0].Description)
	assert.JSONEq(t, `{"type":"object"}`, got[0].InputSchema)
}

func TestMapProbedTools_EmptyInputSchema_NotFabricated(t *testing.T) {
	in := []probe.Tool{{Name: "a"}}
	got := mapProbedTools(in)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].InputSchema, "a tool that advertised no schema must not gain a fabricated one")
}

// TestPodProber_ImageValidationFailure_ReturnsPodFailure_NoPod is the test
// that makes the Task-2 image guard LIVE: every spec variant's image is
// validated before any pod/NetworkPolicy/ConfigMap is created for it.
func TestPodProber_ImageValidationFailure_ReturnsPodFailure_NoPod(t *testing.T) {
	cases := []struct {
		name string
		spec v1alpha1.WorkshopProbeSpec
	}{
		{
			name: "image mode: first-party image refused before any pod",
			spec: v1alpha1.WorkshopProbeSpec{Image: "registry.internal.example/ap/x:v1"},
		},
		{
			name: "script mode: first-party base image refused before any pod or configmap",
			spec: v1alpha1.WorkshopProbeSpec{
				Script: &v1alpha1.WorkshopProbeScript{BaseImage: "registry.internal.example/ap/x:v1", Script: "#!/bin/sh\necho hi\n"},
			},
		},
		{
			name: "cliHelp mode: first-party image refused before any pod",
			spec: v1alpha1.WorkshopProbeSpec{
				CliHelp: &v1alpha1.WorkshopProbeCliHelp{Image: "registry.internal.example/ap/x:v1", Binary: "democli"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeClient(t)
			p := &PodProber{Client: c, OperatorNamespace: "spicebox-system"}
			wp := &v1alpha1.WorkshopProbe{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ws-x"}, Spec: tc.spec}

			// with trustedImageRegistry injected as registry.internal.example/ap, this ref is refused before any pod
			res, err := p.probeWith(context.Background(), wp, "registry.internal.example/ap")
			require.NoError(t, err, "a refused image is a probe RESULT (podFailure), not a controller error")
			assert.NotEmpty(t, res.PodFailure)

			var pods corev1.PodList
			require.NoError(t, c.List(context.Background(), &pods, client.InNamespace("ws-x")))
			assert.Empty(t, pods.Items, "no pod created for a refused image")

			var cms corev1.ConfigMapList
			require.NoError(t, c.List(context.Background(), &cms, client.InNamespace("ws-x")))
			assert.Empty(t, cms.Items, "no configmap created for a refused image")

			var nps networkingv1.NetworkPolicyList
			require.NoError(t, c.List(context.Background(), &nps, client.InNamespace("ws-x")))
			assert.Empty(t, nps.Items, "no NetworkPolicy created for a refused image")
		})
	}
}

func TestProbePodStatus(t *testing.T) {
	readyCond := corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue}

	cases := []struct {
		name        string
		pod         corev1.Pod
		cliHelp     bool
		wantUsable  bool
		wantFailure bool
	}{
		{
			name: "image/script mode: Ready + PodIP is usable",
			pod: corev1.Pod{Status: corev1.PodStatus{
				PodIP:      "10.0.0.5",
				Conditions: []corev1.PodCondition{readyCond},
			}},
			cliHelp:    false,
			wantUsable: true,
		},
		{
			name: "image/script mode: Ready without PodIP is not yet usable",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{readyCond},
			}},
			cliHelp: false,
		},
		{
			name: "image/script mode: container terminated before reachable is a failure",
			pod: corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  probeContainerName,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
				}},
			}},
			cliHelp:     false,
			wantFailure: true,
		},
		{
			name: "image/script mode: ImagePullBackOff is a failure, not a keep-polling state",
			pod: corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  probeContainerName,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "no such image"}},
				}},
			}},
			cliHelp:     false,
			wantFailure: true,
		},
		{
			name: "image/script mode: ordinary ContainerCreating keeps polling",
			pod: corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  probeContainerName,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
				}},
			}},
			cliHelp: false,
		},
		{
			name: "cliHelp mode: terminated (even nonzero exit) is usable",
			pod: corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  probeContainerName,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
				}},
			}},
			cliHelp:    true,
			wantUsable: true,
		},
		{
			name: "cliHelp mode: still running is not yet usable",
			pod: corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  probeContainerName,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}},
			}},
			cliHelp: true,
		},
		{
			name: "cliHelp mode: CrashLoopBackOff is a failure",
			pod: corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  probeContainerName,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				}},
			}},
			cliHelp:     true,
			wantFailure: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usable, failure := probePodStatus(&tc.pod, tc.cliHelp)
			assert.Equal(t, tc.wantUsable, usable)
			if tc.wantFailure {
				assert.NotEmpty(t, failure)
			} else {
				assert.Empty(t, failure)
			}
		})
	}
}

func TestDigestFromProbePod(t *testing.T) {
	t.Run("nil pod returns empty, not a panic", func(t *testing.T) {
		assert.Empty(t, digestFromProbePod(nil))
	})
	t.Run("no matching container status returns empty", func(t *testing.T) {
		pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "sidecar", ImageID: "docker-pullable://x@sha256:aaaa"}}}}
		assert.Empty(t, digestFromProbePod(pod))
	})
	t.Run("probe container's ImageID is resolved to a digest", func(t *testing.T) {
		pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:    probeContainerName,
			ImageID: "docker-pullable://ghcr.io/demo/mcp@sha256:" + fmtHash(),
		}}}}
		got := digestFromProbePod(pod)
		assert.Equal(t, "sha256:"+fmtHash(), got)
	})
}

func fmtHash() string {
	return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}

// probeKey builds the deterministic ObjectKey of the pod probeWith creates
// for wp, matching ProbePodName.
func probeKey(wp *v1alpha1.WorkshopProbe) client.ObjectKey {
	return client.ObjectKey{Namespace: wp.Namespace, Name: ProbePodName(wp)}
}

// simulateKubelet runs in the background and mutates the probe pod's status
// once it exists, standing in for the kubelet a fake client (and envtest,
// which has none at all) never runs. mutate is applied to the fetched pod
// before the status update is written.
func simulateKubelet(t *testing.T, c client.Client, key client.ObjectKey, mutate func(*corev1.Pod)) {
	t.Helper()
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var got corev1.Pod
			if err := c.Get(context.Background(), key, &got); err == nil {
				mutate(&got)
				_ = c.Status().Update(context.Background(), &got)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
}

func TestPodProber_ImageMode_Success_PollsUntilReady_CallsToolsList_CapturesDigest_DeletesPod(t *testing.T) {
	c := newFakeClient(t)
	wp := &v1alpha1.WorkshopProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ws-x", UID: "u1"},
		Spec:       v1alpha1.WorkshopProbeSpec{Image: "ghcr.io/demo/mcp:v1", TimeoutSeconds: 20},
	}

	var gotURL string
	p := &PodProber{
		Client:            c,
		OperatorNamespace: "spicebox-system",
		pollInterval:      5 * time.Millisecond,
		ListTools: func(ctx context.Context, url string) ([]probe.Tool, error) {
			gotURL = url
			return []probe.Tool{{Name: "echo", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
		},
	}

	simulateKubelet(t, c, probeKey(wp), func(pod *corev1.Pod) {
		pod.Status.PodIP = "10.0.0.5"
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:    probeContainerName,
			ImageID: "docker-pullable://ghcr.io/demo/mcp@sha256:" + fmtHash(),
		}}
	})

	res, err := p.probeWith(context.Background(), wp, "")
	require.NoError(t, err)
	assert.Empty(t, res.PodFailure)
	require.Len(t, res.Tools, 1)
	assert.Equal(t, "echo", res.Tools[0].Name)
	assert.JSONEq(t, `{"type":"object"}`, res.Tools[0].InputSchema)
	assert.Equal(t, "sha256:"+fmtHash(), res.ResolvedDigest)
	assert.Equal(t, "http://10.0.0.5:8080", gotURL)

	var pods corev1.PodList
	require.NoError(t, c.List(context.Background(), &pods, client.InNamespace("ws-x")))
	assert.Empty(t, pods.Items, "probe pod deleted after use")

	var nps networkingv1.NetworkPolicyList
	require.NoError(t, c.List(context.Background(), &nps, client.InNamespace("ws-x")))
	require.Len(t, nps.Items, 1, "NetworkPolicy applied for the probe")
	assert.Equal(t, ProbeNetworkPolicyName(wp), nps.Items[0].Name)
}

func TestPodProber_CliHelpMode_Success_ReadsLogsIntoHelpText(t *testing.T) {
	c := newFakeClient(t)
	wp := &v1alpha1.WorkshopProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ws-x", UID: "u1"},
		Spec: v1alpha1.WorkshopProbeSpec{
			CliHelp:        &v1alpha1.WorkshopProbeCliHelp{Image: "ghcr.io/demo/cli:v1", Binary: "democli", Args: []string{"--help"}},
			TimeoutSeconds: 20,
		},
	}

	var loggedFor string
	p := &PodProber{
		Client:            c,
		OperatorNamespace: "spicebox-system",
		pollInterval:      5 * time.Millisecond,
		podLogs: func(ctx context.Context, namespace, podName, container string) (string, error) {
			loggedFor = namespace + "/" + podName + "/" + container
			return "usage: democli [flags]", nil
		},
	}

	simulateKubelet(t, c, probeKey(wp), func(pod *corev1.Pod) {
		pod.Status.Phase = corev1.PodSucceeded
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:    probeContainerName,
			ImageID: "docker-pullable://ghcr.io/demo/cli@sha256:" + fmtHash(),
			State:   corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}}
	})

	res, err := p.probeWith(context.Background(), wp, "")
	require.NoError(t, err)
	assert.Empty(t, res.PodFailure)
	assert.Equal(t, "usage: democli [flags]", res.HelpText)
	assert.Equal(t, "sha256:"+fmtHash(), res.ResolvedDigest)
	assert.Equal(t, "ws-x/"+ProbePodName(wp)+"/"+probeContainerName, loggedFor)

	var pods corev1.PodList
	require.NoError(t, c.List(context.Background(), &pods, client.InNamespace("ws-x")))
	assert.Empty(t, pods.Items, "probe pod deleted after use")
}

func TestPodProber_ScriptMode_StagesConfigMapBeforePod(t *testing.T) {
	c := newFakeClient(t)
	wp := &v1alpha1.WorkshopProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ws-x", UID: "u1"},
		Spec: v1alpha1.WorkshopProbeSpec{
			Script:         &v1alpha1.WorkshopProbeScript{BaseImage: "ghcr.io/demo/base:v1", Script: "#!/bin/sh\necho hi\n"},
			TimeoutSeconds: 20,
		},
	}

	p := &PodProber{
		Client:            c,
		OperatorNamespace: "spicebox-system",
		pollInterval:      5 * time.Millisecond,
		ListTools: func(ctx context.Context, url string) ([]probe.Tool, error) {
			return nil, nil
		},
	}

	simulateKubelet(t, c, probeKey(wp), func(pod *corev1.Pod) {
		pod.Status.PodIP = "10.0.0.6"
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	})

	res, err := p.probeWith(context.Background(), wp, "")
	require.NoError(t, err)
	assert.Empty(t, res.PodFailure)

	var cm corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "ws-x", Name: ProbeScriptConfigMapName(wp)}, &cm),
		"script configmap must exist (created before the pod, whose volume mount requires it)")
	assert.Equal(t, "#!/bin/sh\necho hi\n", cm.Data[ProbeScriptConfigMapKey])
	require.Len(t, cm.OwnerReferences, 1)
	assert.Equal(t, "WorkshopProbe", cm.OwnerReferences[0].Kind)
}

// TestEnsureScriptConfigMap_SSAApply_IdempotentOnRetry pins the fix for a
// real bug: ensureScriptConfigMap used to Create, then on AlreadyExists
// Get-then-Update — and that Get went through the SAME cache-backed client
// every other read in this package uses. The operator's manager cache
// filters ConfigMap reads to adoptguard.AdoptedLabel-carrying objects (this
// ConfigMap carries no such label), so on a real cluster that retry Get
// would spuriously report NotFound for a ConfigMap that genuinely exists,
// breaking a retried Script-mode probe. SSA-applying instead (client.Apply)
// needs no read at all: calling ensureScriptConfigMap twice for the
// identical WorkshopProbe — the shape of a retried probeWith — must succeed
// both times with no AlreadyExists/NotFound dance in between.
func TestEnsureScriptConfigMap_SSAApply_IdempotentOnRetry(t *testing.T) {
	c := newFakeClient(t)
	wp := &v1alpha1.WorkshopProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ws-retry", UID: "u1"},
		Spec: v1alpha1.WorkshopProbeSpec{
			Script: &v1alpha1.WorkshopProbeScript{BaseImage: "ghcr.io/demo/base:v1", Script: "#!/bin/sh\necho hi\n"},
		},
	}
	p := &PodProber{Client: c, OperatorNamespace: "spicebox-system"}

	require.NoError(t, p.ensureScriptConfigMap(context.Background(), wp), "first apply")
	require.NoError(t, p.ensureScriptConfigMap(context.Background(), wp),
		"second apply (the shape of a retry) must be a no-op, not an AlreadyExists/NotFound error")

	var cm corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "ws-retry", Name: ProbeScriptConfigMapName(wp)}, &cm))
	assert.Equal(t, "#!/bin/sh\necho hi\n", cm.Data[ProbeScriptConfigMapKey])
	require.Len(t, cm.OwnerReferences, 1)
	assert.Equal(t, "WorkshopProbe", cm.OwnerReferences[0].Kind)
}

func TestPodProber_MCPContainerCrashes_ReturnsPodFailure_CapturesDigest_DeletesPod(t *testing.T) {
	c := newFakeClient(t)
	wp := &v1alpha1.WorkshopProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ws-x", UID: "u1"},
		Spec:       v1alpha1.WorkshopProbeSpec{Image: "ghcr.io/demo/mcp:v1", TimeoutSeconds: 20},
	}
	p := &PodProber{Client: c, OperatorNamespace: "spicebox-system", pollInterval: 5 * time.Millisecond}

	simulateKubelet(t, c, probeKey(wp), func(pod *corev1.Pod) {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:    probeContainerName,
			ImageID: "docker-pullable://ghcr.io/demo/mcp@sha256:" + fmtHash(),
			State:   corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off restarting failed container"}},
		}}
	})

	res, err := p.probeWith(context.Background(), wp, "")
	require.NoError(t, err, "a crashed probe container is a RESULT, not a controller error")
	assert.NotEmpty(t, res.PodFailure)
	assert.Contains(t, res.PodFailure, "CrashLoopBackOff")
	assert.Equal(t, "sha256:"+fmtHash(), res.ResolvedDigest, "digest is captured even when the probe itself failed")

	var pods corev1.PodList
	require.NoError(t, c.List(context.Background(), &pods, client.InNamespace("ws-x")))
	assert.Empty(t, pods.Items, "probe pod deleted after a failed probe too")
}

func TestPodProber_DeadlineExceeded_ReturnsPodFailure(t *testing.T) {
	c := newFakeClient(t)
	wp := &v1alpha1.WorkshopProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ws-x", UID: "u1"},
		// Minimum plausible timeout; pollInterval is shrunk so the test
		// doesn't actually wait out a whole real second per iteration.
		Spec: v1alpha1.WorkshopProbeSpec{Image: "ghcr.io/demo/mcp:v1", TimeoutSeconds: 1},
	}
	p := &PodProber{Client: c, OperatorNamespace: "spicebox-system", pollInterval: 20 * time.Millisecond}
	// No simulateKubelet: the pod is created and never becomes Ready.

	start := time.Now()
	res, err := p.probeWith(context.Background(), wp, "")
	elapsed := time.Since(start)

	require.NoError(t, err, "a timed-out probe is a RESULT, not a controller error")
	assert.NotEmpty(t, res.PodFailure)
	assert.Contains(t, res.PodFailure, "did not become ready")
	assert.Less(t, elapsed, 5*time.Second, "must not wait past its own configured timeout")

	var pods corev1.PodList
	require.NoError(t, c.List(context.Background(), &pods, client.InNamespace("ws-x")))
	assert.Empty(t, pods.Items, "probe pod deleted after timing out too")
}

func TestPodProber_ToolsListError_ReturnsPodFailure_DeletesPod(t *testing.T) {
	c := newFakeClient(t)
	wp := &v1alpha1.WorkshopProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ws-x", UID: "u1"},
		Spec:       v1alpha1.WorkshopProbeSpec{Image: "ghcr.io/demo/mcp:v1", TimeoutSeconds: 20},
	}
	p := &PodProber{
		Client: c, OperatorNamespace: "spicebox-system", pollInterval: 5 * time.Millisecond,
		ListTools: func(ctx context.Context, url string) ([]probe.Tool, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}
	simulateKubelet(t, c, probeKey(wp), func(pod *corev1.Pod) {
		pod.Status.PodIP = "10.0.0.7"
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	})

	res, err := p.probeWith(context.Background(), wp, "")
	require.NoError(t, err, "a tools/list failure is a RESULT, not a controller error")
	assert.Contains(t, res.PodFailure, "connection refused")

	var pods corev1.PodList
	require.NoError(t, c.List(context.Background(), &pods, client.InNamespace("ws-x")))
	assert.Empty(t, pods.Items)
}

// TestPodProber_CancelledContext_ReturnsError — the poll loop's only
// ctx-taking call (Client.Get) is informer-cache served in production, so a
// cancelled ctx cannot break it; the select on ctx.Done() is the only
// cancellation point. Mirrors sidecartoolbox/probe_test.go's own version of
// this test.
func TestPodProber_CancelledContext_ReturnsError(t *testing.T) {
	c := newFakeClient(t)
	wp := &v1alpha1.WorkshopProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ws-x", UID: "u1"},
		Spec:       v1alpha1.WorkshopProbeSpec{Image: "ghcr.io/demo/mcp:v1", TimeoutSeconds: 20},
	}
	p := &PodProber{Client: c, OperatorNamespace: "spicebox-system", pollInterval: time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := p.probeWith(ctx, wp, "")
	elapsed := time.Since(start)

	require.Error(t, err, "a cancelled probe must fail loudly, not report a fabricated success/failure result")
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, 2*time.Second, "must abandon the poll on cancellation, not wait out timeoutSeconds")
}
