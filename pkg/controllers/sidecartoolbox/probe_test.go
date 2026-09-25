package sidecartoolbox

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// envValue returns the probe container's env var value by name and whether it
// was set at all — distinguishing "absent" from "set to empty".
func envValue(pod *corev1.Pod, name string) (string, bool) {
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// lagGetClient simulates informer-cache read-after-write lag: the first
// `getMisses` Get calls return NotFound (as a stale cache does right after a
// Create hits the API server), after which reads delegate to the real store.
type lagGetClient struct {
	client.Client
	getMisses int
}

func (c *lagGetClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if c.getMisses > 0 {
		c.getMisses--
		return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// TestProbeOnce_TransientNotFound_IsNotVanished pins the read-after-write cache
// race: probeOnce Creates the probe pod against the API server, then Gets it
// from the informer cache, which can lag. A NotFound on those early Gets means
// "not visible YET", not "vanished" — bailing there failed every probe on a
// loaded cluster. probeOnce must tolerate the transient NotFound and keep
// polling to the deadline, never returning the "vanished" error.
func TestProbeOnce_TransientNotFound_IsNotVanished(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source:    spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/y:v1"},
			Transport: spiceboxv1alpha1.SidecarToolboxTransport{Healthcheck: spiceboxv1alpha1.SidecarToolboxHealthcheck{TimeoutSeconds: 3}},
		},
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()
	r := &Reconciler{Client: &lagGetClient{Client: base, getMisses: 2}}

	start := time.Now()
	_, err := r.probeOnce(context.Background(), cr)
	elapsed := time.Since(start)

	require.Error(t, err, "the fake pod never becomes Ready, so probeOnce must fail — but on the timeout, not 'vanished'")
	assert.False(t, strings.Contains(err.Error(), "vanished"),
		"a transient cache-lag NotFound must NOT be reported as the pod vanishing; got: %v", err)
	assert.GreaterOrEqual(t, elapsed, 2*time.Second,
		"probeOnce must tolerate the early NotFound and keep polling to the deadline, not bail immediately")
	assert.NotContains(t, err.Error(), "probe-echo-",
		"the deadline message becomes the persisted condition; it must not embed the timestamped probe-pod name (causes reconcile hot-loop)")
}

// readyPodReader returns any requested Pod as Ready with a fixed PodIP, so
// probeOnce reaches the MCP-dial branch without a live kubelet/CNI. This lets a
// test drive the fresh-pod window through the dialProbe seam alone.
type readyPodReader struct {
	client.Reader
	podIP string
}

func (r readyPodReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if pod, ok := obj.(*corev1.Pod); ok {
		pod.Name = key.Name
		pod.Namespace = key.Namespace
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		pod.Status.PodIP = r.podIP
		return nil
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

// TestProbeOnce_MCPDialRetriesWithinDeadline pins the fresh-pod-window fix: a
// probe Pod can report Ready (kubelet runs the readiness httpGet from the
// node's own netns) a beat before its MCP endpoint is reachable cross-pod from
// the operator Pod (CNI/route programming lag), surfacing as a transient
// "connection refused". probeOnce must RETRY the dial within the deadline, not
// fail the whole probe on the first refusal — a stable Pod serves the same
// endpoint fine (verified live).
func TestProbeOnce_MCPDialRetriesWithinDeadline(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source:    spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/y:v1"},
			Transport: spiceboxv1alpha1.SidecarToolboxTransport{Healthcheck: spiceboxv1alpha1.SidecarToolboxHealthcheck{TimeoutSeconds: 10}},
		},
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()
	calls := 0
	r := &Reconciler{
		Client:    base,
		APIReader: readyPodReader{Reader: base, podIP: "10.0.0.1"},
		dialProbe: func(_ context.Context, url string) ([]probe.Tool, error) {
			calls++
			if calls < 3 {
				// The exact shape of the live failure: a TCP RST to a Ready-but-
				// not-yet-routable Pod IP.
				return nil, fmt.Errorf("dial tcp %s: connect: connection refused", url)
			}
			return []probe.Tool{{Name: "noop"}}, nil
		},
	}

	res, err := r.probeOnce(context.Background(), cr)
	require.NoError(t, err, "probeOnce must retry the MCP dial within the deadline, not fail on the first connection-refused")
	assert.GreaterOrEqual(t, calls, 3, "the dial must be retried until the endpoint accepts")
	require.Len(t, res.Tools, 1)
	assert.Equal(t, "noop", res.Tools[0].Name)
}

// TestProbeOnce_FailureMessageOmitsVolatilePodIdentity pins the churn fix: the
// error probeOnce returns becomes the Reachable=False condition MESSAGE, which
// the controller persists. It must be a pure function of the failure class —
// no timestamped probe-Pod name, no Pod IP — or every reconcile mints a new
// Pod (new name, new IP), writes a different message, and the controller's own
// status watch re-fires immediately: a hot loop spawning a probe Pod every
// couple of seconds. Here every dial is refused, so probeOnce polls to the
// deadline and returns its terminal error.
func TestProbeOnce_FailureMessageOmitsVolatilePodIdentity(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source:    spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/y:v1"},
			Transport: spiceboxv1alpha1.SidecarToolboxTransport{Healthcheck: spiceboxv1alpha1.SidecarToolboxHealthcheck{TimeoutSeconds: 2}},
		},
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()
	r := &Reconciler{
		Client:    base,
		APIReader: readyPodReader{Reader: base, podIP: "10.0.0.7"},
		dialProbe: func(_ context.Context, _ string) ([]probe.Tool, error) {
			return nil, fmt.Errorf("dial tcp 10.0.0.7:8080: connect: connection refused")
		},
	}

	_, err := r.probeOnce(context.Background(), cr)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "10.0.0.7",
		"the persisted condition message must not embed the volatile pod IP (causes reconcile hot-loop)")
	assert.NotContains(t, err.Error(), "probe-echo-",
		"the persisted condition message must not embed the timestamped probe-pod name (causes reconcile hot-loop)")
}

func TestBuildProbePod_Image(t *testing.T) {
	cr := &spiceboxv1alpha1.SidecarToolbox{
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source: spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/y:v1"},
		},
	}
	r := &Reconciler{}
	pod := r.buildProbePod(cr, "probe-x")
	if pod.Spec.Containers[0].Image != "ghcr.io/x/y:v1" {
		t.Errorf("Image=%q", pod.Spec.Containers[0].Image)
	}
	if pod.Spec.RestartPolicy != "Never" {
		t.Errorf("RestartPolicy=%q", pod.Spec.RestartPolicy)
	}
	if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds <= 0 {
		t.Errorf("ActiveDeadlineSeconds must be set")
	}
}

func TestBuildProbePod_InlineHasConfigMapVolume(t *testing.T) {
	cr := &spiceboxv1alpha1.SidecarToolbox{
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source: spiceboxv1alpha1.SidecarToolboxSource{
				Inline: &spiceboxv1alpha1.SidecarToolboxInlineSource{
					BaseImage: "python:3.12-slim",
					Script: spiceboxv1alpha1.SidecarToolboxScriptSource{
						ConfigMapRef: spiceboxv1alpha1.SidecarToolboxConfigMapKeyRef{Name: "src", Key: "server.py"},
					},
					Entrypoint: []string{"python", "/app/server.py"},
				},
			},
		},
	}
	r := &Reconciler{}
	pod := r.buildProbePod(cr, "probe-x")
	if pod.Spec.Containers[0].Image != "python:3.12-slim" {
		t.Errorf("Image=%q", pod.Spec.Containers[0].Image)
	}
	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].VolumeSource.ConfigMap == nil {
		t.Errorf("Volumes=%+v", pod.Spec.Volumes)
	}
	if len(pod.Spec.Containers[0].VolumeMounts) != 1 || pod.Spec.Containers[0].VolumeMounts[0].MountPath != "/app" {
		t.Errorf("VolumeMounts wrong: %+v", pod.Spec.Containers[0].VolumeMounts)
	}
}

// TestBuildProbePod_DeadlineTracksConfiguredTimeout — a timeoutSeconds larger
// than the old hardcoded 60s must extend the probe Pod's own deadline, or the
// kubelet kills the pod while probeOnce is still polling for it and the CR is
// told it "did not become Ready in time".
func TestBuildProbePod_DeadlineTracksConfiguredTimeout(t *testing.T) {
	cr := &spiceboxv1alpha1.SidecarToolbox{
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source:    spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/y:v1"},
			Transport: spiceboxv1alpha1.SidecarToolboxTransport{Healthcheck: spiceboxv1alpha1.SidecarToolboxHealthcheck{TimeoutSeconds: 120}},
		},
	}
	pod := (&Reconciler{}).buildProbePod(cr, "probe-x")
	require.NotNil(t, pod.Spec.ActiveDeadlineSeconds, "ActiveDeadlineSeconds must be set")
	assert.GreaterOrEqual(t, *pod.Spec.ActiveDeadlineSeconds, int64(120),
		"probe Pod must outlive the configured healthcheck.timeoutSeconds the poll loop waits out")
}

// TestProbeOnce_CancelledContext_ReturnsPromptly — the poll loop's only
// ctx-taking call (Client.Get) is informer-cache served in production, so a
// cancelled reconcile ctx cannot break it. With MaxConcurrentReconciles=1 that
// pins the sole worker for the whole configured timeout, and shutdown cannot
// interrupt it either.
func TestProbeOnce_CancelledContext_ReturnsPromptly(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source: spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/y:v1"},
			// Long enough that a loop ignoring cancellation is unmistakable.
			Transport: spiceboxv1alpha1.SidecarToolboxTransport{Healthcheck: spiceboxv1alpha1.SidecarToolboxHealthcheck{TimeoutSeconds: 5}},
		},
	}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := r.probeOnce(ctx, cr)
	elapsed := time.Since(start)

	require.Error(t, err, "a cancelled probe must fail, not report success")
	assert.ErrorIs(t, err, context.Canceled, "the failure must name cancellation, not a fabricated readiness timeout")
	assert.Less(t, elapsed, 2*time.Second, "probeOnce must abandon the poll on cancellation, not wait out timeoutSeconds")
}

// TestBuildProbePod_DeliversSidecarConfig — MAJOR-2 part 1: the probe must
// boot the image the way a session will, so spec.config rides the probe pod
// as AP_SIDECAR_CONFIG exactly like cosidecar.BuildContainer delivers it to
// the runtime sidecar container.
func TestBuildProbePod_DeliversSidecarConfig(t *testing.T) {
	cr := &spiceboxv1alpha1.SidecarToolbox{
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source: spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/y:v1"},
			Config: "baseURL: https://api.example.test\nauth: {type: none}\n",
		},
	}
	pod := (&Reconciler{}).buildProbePod(cr, "probe-x")
	v, ok := envValue(pod, "AP_SIDECAR_CONFIG")
	require.True(t, ok, "AP_SIDECAR_CONFIG must be set when spec.config is non-empty")
	assert.Equal(t, cr.Spec.Config, v)
}

func TestBuildProbePod_NoConfigMeansNoConfigEnvVar(t *testing.T) {
	cr := &spiceboxv1alpha1.SidecarToolbox{
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{Source: spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/y:v1"}},
	}
	pod := (&Reconciler{}).buildProbePod(cr, "probe-x")
	_, ok := envValue(pod, "AP_SIDECAR_CONFIG")
	assert.False(t, ok, "an empty spec.config must leave the env var unset, not set to empty")
}

// TestBuildProbePod_DeliversPlaceholderUpstreamCredential — MAJOR-2 part 2: a
// fail-closed image (ap-api-adapter) fatals on a missing credential env var,
// so the probe supplies a fixed placeholder. The probe only calls tools/list,
// never an operation, so the placeholder never reaches any upstream.
func TestBuildProbePod_DeliversPlaceholderUpstreamCredential(t *testing.T) {
	cr := &spiceboxv1alpha1.SidecarToolbox{
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source:       spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/y:v1"},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{EnvVar: "UPSTREAM_TOKEN"},
		},
	}
	pod := (&Reconciler{}).buildProbePod(cr, "probe-x")
	v, ok := envValue(pod, "UPSTREAM_TOKEN")
	require.True(t, ok, "the upstream auth env var must be set when upstreamAuth.envVar is non-empty")
	assert.Equal(t, "admission-probe-placeholder", v)
}

func TestBuildProbePod_NoUpstreamAuthEnvVarMeansNoPlaceholder(t *testing.T) {
	cr := &spiceboxv1alpha1.SidecarToolbox{
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{Source: spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/y:v1"}},
	}
	pod := (&Reconciler{}).buildProbePod(cr, "probe-x")
	// The property is that NO upstream-credential placeholder is injected —
	// not a fixed env count (MCP_PORT and AP_PROBE_MODE are always present).
	_, ok := envValue(pod, "UPSTREAM_TOKEN")
	assert.False(t, ok, "no upstreamAuth.envVar means no placeholder credential env is injected")
}

// TestBuildProbePod_TerminationMessagePolicy — MAJOR-2 part 3: without this,
// a fail-closed image's fatal exit message never reaches Pod status, and the
// deferred delete in probeOnce destroys the only copy of the binary's words.
func TestBuildProbePod_TerminationMessagePolicy(t *testing.T) {
	cr := &spiceboxv1alpha1.SidecarToolbox{
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{Source: spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/y:v1"}},
	}
	pod := (&Reconciler{}).buildProbePod(cr, "probe-x")
	assert.Equal(t, corev1.TerminationMessageFallbackToLogsOnError, pod.Spec.Containers[0].TerminationMessagePolicy)
}

// getInterceptor wraps a client.Client and, on the FIRST Get of a Pod object,
// synchronously writes a terminated container status into the underlying
// store before returning to the caller. A real kubelet would set this status
// once the probe container actually exits; the fake client cannot run a
// container, so this fakes that effect deterministically — no goroutine, no
// timing race — since the write completes, in-line, before probeOnce's LATER
// (post-timeout) Get of the same object ever runs.
type getInterceptor struct {
	client.Client
	once    sync.Once
	message string
}

func (g *getInterceptor) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := g.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	g.once.Do(func() {
		p := pod.DeepCopy()
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  probeContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: g.message}},
		}}
		// Pod is one of the fake client's built-in status-subresource kinds, so
		// a plain Update leaves Status untouched — the write must go through
		// Status().Update, exactly like a real kubelet reports container state.
		_ = g.Client.Status().Update(context.Background(), p)
	})
	return nil
}

// TestProbeOnce_TimeoutRecordsContainerTerminationMessage — MAJOR-2 part 3's
// diagnosability claim end to end: a probe pod that never becomes Ready but
// whose probe container already terminated with a message must surface that
// message in the timeout error (which controller.go's probePhase writes
// verbatim as the Reachable=False condition's Message), not just "did not
// become Ready in time".
func TestProbeOnce_TimeoutRecordsContainerTerminationMessage(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source: spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/y:v1"},
			// Kept short so the test doesn't have to wait out a real timeout.
			Transport: spiceboxv1alpha1.SidecarToolboxTransport{Healthcheck: spiceboxv1alpha1.SidecarToolboxHealthcheck{TimeoutSeconds: 1}},
		},
	}
	const wantMsg = "ap-api-adapter: AP_SIDECAR_CONFIG is required"
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()
	r := &Reconciler{Client: &getInterceptor{Client: base, message: wantMsg}}

	_, err := r.probeOnce(context.Background(), cr)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not become Ready in time")
	assert.Contains(t, err.Error(), wantMsg, "the recorded failure must name the container's actual startup error, not just a timeout")
}

// TestProbeURL_CarriesTransportPath is the regression net for the admission
// probe blocker: both first-party sidecar images serve MCP only at "/mcp",
// nothing at the pod root, so a probe URL missing that suffix 404s against
// the ServeMux before tools/list is ever reached — silently failing every
// SidecarToolbox that declares transport.path (which includes the shipped
// ap-workshop and ap-api-adapter manifests). probeOnce's own Pod-Ready branch
// isn't independently testable without a live listener bound to the fixed
// probePort, so this asserts on probeURL — the pure function that branch
// calls to build the exact string handed to probe.Client.
func TestProbeURL_CarriesTransportPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{name: "explicit /mcp path is appended", path: "/mcp", want: "http://10.0.0.5:8080/mcp"},
		{name: "path missing its leading slash is normalized", path: "mcp", want: "http://10.0.0.5:8080/mcp"},
		{name: "empty path means pod root, no trailing junk", path: "", want: "http://10.0.0.5:8080"},
		{name: "bare slash path means pod root, no trailing junk", path: "/", want: "http://10.0.0.5:8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := &spiceboxv1alpha1.SidecarToolbox{
				Spec: spiceboxv1alpha1.SidecarToolboxSpec{
					Transport: spiceboxv1alpha1.SidecarToolboxTransport{Path: tc.path},
				},
			}
			assert.Equal(t, tc.want, probeURL("10.0.0.5", cr))
		})
	}
}

func TestPickHealthPath_Default(t *testing.T) {
	cr := &spiceboxv1alpha1.SidecarToolbox{}
	if got := pickHealthPath(cr); got != "/healthz" {
		t.Errorf("default=%q", got)
	}
	cr.Spec.Transport.Healthcheck.Path = "/custom"
	if got := pickHealthPath(cr); got != "/custom" {
		t.Errorf("custom=%q", got)
	}
}

// countingReader counts Get calls, delegating to a wrapped reader. Used to prove
// probeOnce reads the probe Pod through the uncached APIReader, not the cache.
type countingReader struct {
	client.Reader
	gets int
}

func (c *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.gets++
	return c.Reader.Get(ctx, key, obj, opts...)
}

// TestProbeOnce_ReadsViaAPIReader pins the uncached-read fix: when APIReader is
// wired, probeOnce reads the probe Pod through it (the live API-server view),
// NOT the informer cache — which lags on a loaded cluster and returned stale
// probe-Pod state (NotFound after Create, then a stale Ready+podIP for a Pod
// already terminating → "connection refused").
func TestProbeOnce_ReadsViaAPIReader(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source:    spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/y:v1"},
			Transport: spiceboxv1alpha1.SidecarToolboxTransport{Healthcheck: spiceboxv1alpha1.SidecarToolboxHealthcheck{TimeoutSeconds: 2}},
		},
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()
	reader := &countingReader{Reader: base}
	r := &Reconciler{Client: base, APIReader: reader}

	// Pod is created (into base via Client) but never becomes Ready in the fake,
	// so probeOnce polls to the deadline — every poll reading through APIReader.
	_, err := r.probeOnce(context.Background(), cr)
	require.Error(t, err)
	assert.GreaterOrEqual(t, reader.gets, 1,
		"probeOnce must read the probe Pod through the uncached APIReader, not the cache")
}
