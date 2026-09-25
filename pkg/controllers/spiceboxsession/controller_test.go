//go:build integration

package spiceboxsession_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxtoolspec"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolchainaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	_ "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod" // register the built-in sandbox backend so spiceboxclass.Reconciler resolves it
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/registry"
)

func TestSession_PodCreatedOnValidClass(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	createClass(t, env.Client, "cls-a")
	sess := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"},
		Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "cls-a"},
	}
	mustCreate(t, env.Client, sess)

	var pod corev1.Pod
	podKey := types.NamespacedName{Name: "s1-pod", Namespace: "default"}
	eventually(t, 10*time.Second, func() bool {
		return env.Client.Get(context.Background(), podKey, &pod) == nil
	})
	require.NotEmpty(t, pod.OwnerReferences, "pod should have an owner reference")
	assert.Equal(t, "s1", pod.OwnerReferences[0].Name, "pod ownerRef name")

	// status.ResolvedClass is written in a Status().Update separate from the
	// one that creates the Pod (freeze-then-create are two different
	// reconciler writes, not one atomic transaction), so observing the Pod
	// above proves nothing about whether ResolvedClass has landed yet. A bare
	// Get here races the reconciler under load: poll for ResolvedClass itself
	// rather than assuming it arrived by the time the pod did.
	var got spiceboxv1alpha1.SpiceboxSession
	eventually(t, 10*time.Second, func() bool {
		return env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got) == nil &&
			got.Status.ResolvedClass != nil
	})
	require.NotNil(t, got.Status.ResolvedClass, "ResolvedClass never populated within the wait window")
	assert.NotEmpty(t, got.Status.ResolvedClass.Image, "ResolvedClass.Image")
	// After the session is Ready, status.resolvedAgent must equal spec.agent.
	assert.Equal(t, sess.Spec.Agent, got.Status.ResolvedAgent, "ResolvedAgent")
}

// TestSession_DefaultsSandboxImageWhenClassOmitsImage is the guard for the cloud
// portability fix: a SpiceboxClass that omits spec.image must resolve to the
// operator's --sandbox-image default (which oap install registry-qualifies),
// rather than an unpullable bare ref hardcoded in the class.
func TestSession_DefaultsSandboxImageWhenClassOmitsImage(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	createClassWithImage(t, env.Client, "cls-noimg", "") // omit spec.image
	sess := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s-noimg", Namespace: "default"},
		Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "cls-noimg"},
	}
	mustCreate(t, env.Client, sess)

	var pod corev1.Pod
	podKey := types.NamespacedName{Name: "s-noimg-pod", Namespace: "default"}
	eventually(t, 10*time.Second, func() bool {
		return env.Client.Get(context.Background(), podKey, &pod) == nil
	})
	require.NotEmpty(t, pod.Spec.Containers, "pod must have a container")
	assert.Equal(t, testDefaultSandboxImage, pod.Spec.Containers[0].Image,
		"a class with no image must use the operator's default sandbox image")

	// status.ResolvedClass is persisted in the reconcile pass BEFORE the Pod is
	// created (the freeze must be durable before anything can reach a pod), but
	// that's still a separate Status().Update from the one seen here via the pod
	// read, so poll for it rather than reading once.
	var got spiceboxv1alpha1.SpiceboxSession
	eventually(t, 10*time.Second, func() bool {
		return env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got) == nil &&
			got.Status.ResolvedClass != nil
	})
	require.NotNil(t, got.Status.ResolvedClass)
	assert.Equal(t, testDefaultSandboxImage, got.Status.ResolvedClass.Image,
		"the default must be frozen into the resolved-class snapshot")
}

func TestSession_FailedConditionOnMissingClass(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	sess := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s-missing", Namespace: "default"},
		Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "nope"},
	}
	mustCreate(t, env.Client, sess)

	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxSession
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		for _, c := range got.Status.Conditions {
			if c.Type == spiceboxv1alpha1.SpiceboxSessionConditionFailed &&
				c.Status == metav1.ConditionTrue &&
				c.Reason == spiceboxv1alpha1.ReasonClassMissing {
				return true
			}
		}
		return false
	})
}

func TestSession_DeletionRemovesFinalizerAndPod(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	createClass(t, env.Client, "cls-b")
	sess := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s-del", Namespace: "default"},
		Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "cls-b"},
	}
	mustCreate(t, env.Client, sess)

	podKey := types.NamespacedName{Name: "s-del-pod", Namespace: "default"}
	eventually(t, 10*time.Second, func() bool {
		var pod corev1.Pod
		return env.Client.Get(context.Background(), podKey, &pod) == nil
	})

	_ = env.Client.Delete(context.Background(), sess)

	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxSession
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			var pod corev1.Pod
			return env.Client.Get(context.Background(), podKey, &pod) != nil
		}
		return false
	})
}

// startManager starts a manager with no AuditMemory wired (a genuine nil
// memory.Memory interface — see AGENTS.md on typed-nil interfaces), matching
// every existing test's expectations that recordResolved no-ops.
func startManager(t *testing.T, env *testenv.Env) manager.Manager {
	t.Helper()
	return startManagerWithAudit(t, env, nil)
}

// startManagerWithAudit starts a manager whose Reconciler.AuditMemory is mem
// (nil is a legitimate value, delegated to by startManager). Passing a
// *stubAuditMemory here lets a test observe the toolchain-audit "resolved"
// entry recordResolved writes, without a real backend/provenance signer.
func startManagerWithAudit(t *testing.T, env *testenv.Env, mem memory.Memory) manager.Manager {
	t.Helper()
	// SkipNameValidation is required because multiple test functions in the
	// same process register controllers with the same names.
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme: env.Scheme,
		// Disable metrics to avoid port conflicts when test packages run in parallel.
		Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: ctrlconfig.Controller{
			SkipNameValidation: ptr.To(true),
		},
	})
	require.NoError(t, err, "ctrl.NewManager")
	require.NoError(t,
		(&spiceboxclass.Reconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr),
		"spiceboxclass SetupWithManager")
	reg, err := registry.NewWithBuiltins(mgr.GetClient())
	require.NoError(t, err, "registry.NewWithBuiltins")
	require.NoError(t,
		(&spiceboxtoolspec.Reconciler{Client: mgr.GetClient(), Registry: reg}).SetupWithManager(mgr),
		"spiceboxtoolspec SetupWithManager")
	require.NoError(t,
		(&spiceboxsession.Reconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
			SandboxImage: testDefaultSandboxImage, AuditMemory: mem,
			Runtimes: testenv.SandboxRuntimes(t, mgr.GetClient()),
		}).SetupWithManager(mgr),
		"spiceboxsession SetupWithManager")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	return mgr
}

// stubAuditMemory is a minimal memory.Memory implementation that records
// every Put call, guarded by a mutex, so a test can assert exactly what
// recordResolved wrote without standing up a real backend, provenance
// signer, or authorizer. recordResolved only ever calls Put; the other three
// Memory methods exist solely to satisfy the interface and are unused here.
type stubAuditMemory struct {
	mu   sync.Mutex
	puts []memory.Entry
}

func (s *stubAuditMemory) Put(_ context.Context, e memory.Entry) (memory.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts = append(s.puts, e)
	return e, nil
}

func (s *stubAuditMemory) Query(_ context.Context, _ memory.Query) (memory.QueryResult, error) {
	return memory.QueryResult{}, nil
}

func (s *stubAuditMemory) Search(_ context.Context, _ memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{}, nil
}

func (s *stubAuditMemory) SendSignal(_ context.Context, _ memory.Signal) error {
	return nil
}

// snapshot returns a copy of every Entry Put so far, safe to read while the
// manager goroutine may still be reconciling.
func (s *stubAuditMemory) snapshot() []memory.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]memory.Entry, len(s.puts))
	copy(out, s.puts)
	return out
}

// ensureRuntimeClass registers the kata-fc RuntimeClass in envtest so that pod
// creation requests referencing it are admitted by the API server.
func ensureRuntimeClass(t *testing.T, cli client.Client) {
	t.Helper()
	rc := &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "kata-fc"},
		Handler:    "kata-fc",
	}
	if err := cli.Create(context.Background(), rc); err != nil && !errors.IsAlreadyExists(err) {
		require.NoError(t, err, "create RuntimeClass kata-fc")
	}
}

// testDefaultSandboxImage is the operator-default sandbox image the test manager
// is configured with, so a class that omits spec.image resolves to it.
const testDefaultSandboxImage = "registry.example.com/default-sandbox:test"

func createClass(t *testing.T, cli client.Client, name string) {
	t.Helper()
	createClassWithImage(t, cli, name, "registry.example.com/toolbelt:latest")
}

// createClassWithImage creates a SpiceboxClass with the given image; pass "" to
// omit spec.image and exercise the operator's --sandbox-image default.
func createClassWithImage(t *testing.T, cli client.Client, name, image string) {
	t.Helper()
	ensureRuntimeClass(t, cli)
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image: image,
			Resources: spiceboxv1alpha1.SpiceboxResources{
				CPU:              resource.MustParse("500m"),
				Memory:           resource.MustParse("256Mi"),
				EphemeralStorage: resource.MustParse("100Mi"),
			},
			Tools: []spiceboxv1alpha1.SpiceboxTool{{Name: "echo", Command: []string{"/bin/echo"}}},
		},
	}
	require.NoError(t, cli.Create(context.Background(), cls), "create SpiceboxClass %s", name)
}

func mustCreate(t *testing.T, cli client.Client, obj client.Object) {
	t.Helper()
	require.NoError(t, cli.Create(context.Background(), obj), "create %T", obj)
}

func eventually(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", within)
}

func TestSession_ReflectsPodReady(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	createClass(t, env.Client, "cls-ready")
	sess := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s-ready", Namespace: "default"},
		Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "cls-ready"},
	}
	mustCreate(t, env.Client, sess)

	podKey := types.NamespacedName{Name: "s-ready-pod", Namespace: "default"}
	eventually(t, 10*time.Second, func() bool {
		var pod corev1.Pod
		return env.Client.Get(context.Background(), podKey, &pod) == nil
	})

	// Patch pod readiness directly (envtest has no kubelet).
	var pod corev1.Pod
	require.NoError(t, env.Client.Get(context.Background(), podKey, &pod), "Get pod")
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, env.Client.Status().Update(context.Background(), &pod), "update pod status")

	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxSession
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		for _, c := range got.Status.Conditions {
			if c.Type == spiceboxv1alpha1.SpiceboxSessionConditionReady && c.Status == metav1.ConditionTrue {
				return true
			}
		}
		return false
	})
}

func TestSession_OOMKilledMarksFailed(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	createClass(t, env.Client, "cls-oom")
	sess := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s-oom", Namespace: "default"},
		Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "cls-oom"},
	}
	mustCreate(t, env.Client, sess)

	podKey := types.NamespacedName{Name: "s-oom-pod", Namespace: "default"}
	eventually(t, 10*time.Second, func() bool {
		var pod corev1.Pod
		return env.Client.Get(context.Background(), podKey, &pod) == nil
	})

	var pod corev1.Pod
	require.NoError(t, env.Client.Get(context.Background(), podKey, &pod), "Get pod")
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "sandbox",
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				Reason:   "OOMKilled",
				ExitCode: 137,
			},
		},
	}}
	require.NoError(t, env.Client.Status().Update(context.Background(), &pod), "update pod status to OOMKilled")

	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxSession
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		for _, c := range got.Status.Conditions {
			if c.Type == spiceboxv1alpha1.SpiceboxSessionConditionFailed &&
				c.Status == metav1.ConditionTrue &&
				c.Reason == sandboxkinds.ReasonOOMKilled {
				return true
			}
		}
		return false
	})
}

func TestSession_RecoversWhenClassAppears(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	// Create the session referencing a class that doesn't exist yet.
	sess := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s-late", Namespace: "default"},
		Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "cls-late"},
	}
	mustCreate(t, env.Client, sess)

	// Wait for ClassMissing failure.
	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxSession
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		for _, c := range got.Status.Conditions {
			if c.Type == spiceboxv1alpha1.SpiceboxSessionConditionFailed &&
				c.Status == metav1.ConditionTrue &&
				c.Reason == spiceboxv1alpha1.ReasonClassMissing {
				return true
			}
		}
		return false
	})

	// Now create the class.
	createClass(t, env.Client, "cls-late")

	// Session should recover: Failed=ClassMissing flips to False, ResolvedClass populates.
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxSession
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		if got.Status.ResolvedClass == nil {
			return false
		}
		for _, c := range got.Status.Conditions {
			if c.Type == spiceboxv1alpha1.SpiceboxSessionConditionFailed &&
				c.Status == metav1.ConditionFalse {
				return true
			}
		}
		return false
	})
}

func TestSession_TTLSweepExpiresIdle(t *testing.T) {
	// Shorten the sweep interval for this test.
	orig := spiceboxsession.SweepInterval
	spiceboxsession.SweepInterval = 200 * time.Millisecond
	t.Cleanup(func() { spiceboxsession.SweepInterval = orig })

	env := testenv.Shared(t)
	startManager(t, env)

	createClass(t, env.Client, "cls-ttl")
	idleTTL := metav1.Duration{Duration: 100 * time.Millisecond}
	sess := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s-ttl", Namespace: "default"},
		Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "cls-ttl", IdleTTL: &idleTTL},
	}
	mustCreate(t, env.Client, sess)

	// Wait for the session to get a resolved class snapshot.
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxSession
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		return got.Status.ResolvedClass != nil
	})

	// Force a stale LastActivityAt so the sweep marks it expired. This is an
	// out-of-band read-modify-write against status the live SpiceboxSession
	// controller also writes (pod-status reflection, conditions), so between
	// our Get and our Update the reconciler can write status first, bumping
	// resourceVersion and turning our Update into a 409 Conflict. Retry with a
	// fresh Get each attempt rather than racing the controller once.
	stale := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var got spiceboxv1alpha1.SpiceboxSession
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return err
		}
		got.Status.LastActivityAt = &stale
		return env.Client.Status().Update(context.Background(), &got)
	}), "stamp stale LastActivityAt")

	// The sweeper should delete the session.
	eventually(t, 10*time.Second, func() bool {
		var g spiceboxv1alpha1.SpiceboxSession
		err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &g)
		return err != nil // NotFound = swept
	})
}

// hasTrueCondition returns true if the session has the given condition with Status=True.
func hasTrueCondition(sess *spiceboxv1alpha1.SpiceboxSession, condType string) bool {
	for _, c := range sess.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// hasTrueConditionClass returns true if the SpiceboxClass has the given condition with Status=True.
func hasTrueConditionClass(cls *spiceboxv1alpha1.SpiceboxClass, condType string) bool {
	for _, c := range cls.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// hasFalseConditionWithReason returns true if the session has the given condition
// with Status=False and the specified reason.
func hasFalseConditionWithReason(sess *spiceboxv1alpha1.SpiceboxSession, condType, reason string) bool {
	for _, c := range sess.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionFalse && c.Reason == reason {
			return true
		}
	}
	return false
}

// hasTrueConditionToolspec returns true if the SpiceboxToolspec has the given condition with Status=True.
func hasTrueConditionToolspec(ts *spiceboxv1alpha1.SpiceboxToolspec, condType string) bool {
	for _, c := range ts.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// createReadyToolspec creates a Toolspec and waits until its controller marks Valid=True.
func createReadyToolspec(t *testing.T, cli client.Client, name, toolkitName, toolkitRev string) {
	t.Helper()
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:             name,
			Version:          "1",
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: toolkitName, Revision: toolkitRev},
			AllowSubcommands: []string{""},
		},
	}
	if err := cli.Create(context.Background(), ts); err != nil && !errors.IsAlreadyExists(err) {
		require.NoError(t, err, "create toolspec %s", name)
	}
	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxToolspec
		if err := cli.Get(context.Background(), client.ObjectKeyFromObject(ts), &got); err != nil {
			return false
		}
		return hasTrueConditionToolspec(&got, spiceboxv1alpha1.SpiceboxToolspecConditionValid)
	})
}

// createReadyClass creates a SpiceboxClass with the given tools and toolspecs and waits for Valid=True.
func createReadyClass(t *testing.T, cli client.Client, name string, tools []string, toolspecs []string) {
	t.Helper()
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image: "python",
			Resources: spiceboxv1alpha1.SpiceboxResources{
				CPU:              resource.MustParse("100m"),
				Memory:           resource.MustParse("128Mi"),
				EphemeralStorage: resource.MustParse("100Mi"),
				PidsLimit:        64,
			},
		},
	}
	for _, tn := range tools {
		cls.Spec.Tools = append(cls.Spec.Tools, spiceboxv1alpha1.SpiceboxTool{
			Name: tn, Command: []string{"/bin/" + tn},
		})
	}
	for _, ts := range toolspecs {
		cls.Spec.Toolspecs = append(cls.Spec.Toolspecs, spiceboxv1alpha1.ToolspecRef{Name: ts})
	}
	require.NoError(t, cli.Create(context.Background(), cls), "create class %s", name)
	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxClass
		if err := cli.Get(context.Background(), client.ObjectKeyFromObject(cls), &got); err != nil {
			return false
		}
		return hasTrueConditionClass(&got, spiceboxv1alpha1.SpiceboxClassConditionValid)
	})
}

func TestSession_InheritsClassToolspecs(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	createReadyToolspec(t, env.Client, "echo-default", "echo", "2026-04-25")
	createReadyClass(t, env.Client, "cls-inh", []string{"echo"}, []string{"echo-default"})

	sess := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-inh", Namespace: "default"},
		Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "cls-inh"},
	}
	mustCreate(t, env.Client, sess)
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxSession
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		return len(got.Status.EffectiveToolspecs) == 1 && got.Status.EffectiveToolspecs[0] == "echo-default"
	})
}

func TestSession_NarrowsToSubset(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	createReadyToolspec(t, env.Client, "echo-default", "echo", "2026-04-25")
	createReadyToolspec(t, env.Client, "cat-default", "cat", "2026-04-25")
	createReadyClass(t, env.Client, "cls-narrow", []string{"echo", "cat"},
		[]string{"echo-default", "cat-default"})

	sess := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-narrow", Namespace: "default"},
		Spec: spiceboxv1alpha1.SpiceboxSessionSpec{
			Class:     "cls-narrow",
			Toolspecs: []spiceboxv1alpha1.ToolspecRef{{Name: "echo-default"}},
		},
	}
	mustCreate(t, env.Client, sess)
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxSession
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		return len(got.Status.EffectiveToolspecs) == 1 && got.Status.EffectiveToolspecs[0] == "echo-default"
	})
}

func TestSession_NotInClass_Invalid(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	createReadyToolspec(t, env.Client, "echo-default", "echo", "2026-04-25")
	createReadyClass(t, env.Client, "cls-bad", []string{"echo"}, []string{"echo-default"})

	sess := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-bad", Namespace: "default"},
		Spec: spiceboxv1alpha1.SpiceboxSessionSpec{
			Class:     "cls-bad",
			Toolspecs: []spiceboxv1alpha1.ToolspecRef{{Name: "not-in-class"}},
		},
	}
	mustCreate(t, env.Client, sess)
	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxSession
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		return hasFalseConditionWithReason(&got, "Ready", spiceboxv1alpha1.ReasonToolspecNotInClass)
	})
}

// TestSession_CreatesPodAgainstUnboundWorkspacePVC is a regression guard for the
// WaitForFirstConsumer fix: missingPVC gates bundle-Pod creation on the workspace
// PVC existing, not on it being Bound. A session with a shared-workspace mount
// and an unbound (Pending) PVC must still get its bundle Pod created, with no
// spurious BundleFailed condition.
func TestSession_CreatesPodAgainstUnboundWorkspacePVC(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	ctx := context.Background()

	// Create a workspace PVC that stays Pending (unbound) — envtest has no
	// scheduler or provisioner, so WaitForFirstConsumer PVCs never bind
	// naturally. This is exactly the state we want to exercise.
	wsPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "ws", Namespace: "default"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}
	mustCreate(t, env.Client, wsPVC)

	// The PVC exists but is unbound (Phase == Pending) — assert that so the
	// test documents the starting state clearly.
	var gotPVC corev1.PersistentVolumeClaim
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ws"}, &gotPVC),
		"Get workspace PVC")
	assert.Equal(t, corev1.ClaimPending, gotPVC.Status.Phase, "workspace PVC must be Pending (unbound) at test start")

	createClass(t, env.Client, "cls-wfc")

	sess := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s-wfc", Namespace: "default"},
		Spec: spiceboxv1alpha1.SpiceboxSessionSpec{
			Class: "cls-wfc",
			Workspace: spiceboxv1alpha1.WorkspaceConfig{
				Mode:            spiceboxv1alpha1.WorkspaceShared,
				SharedClaimName: "ws",
			},
		},
	}
	mustCreate(t, env.Client, sess)

	podName := podspec.PodNameFor(sess)
	sessKey := client.ObjectKeyFromObject(sess)

	// The bundle Pod must be created despite the PVC being unbound.
	eventually(t, 10*time.Second, func() bool {
		var pod corev1.Pod
		return env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: podName}, &pod) == nil
	}) // pod IS created against the unbound (Pending) WFC PVC

	// An unbound WFC PVC must not produce a spurious BundleFailed condition.
	var got spiceboxv1alpha1.SpiceboxSession
	require.NoError(t, env.Client.Get(ctx, sessKey, &got), "Get SpiceboxSession")
	assert.Nil(t, apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxSessionConditionFailed),
		"an unbound WFC PVC must not produce a BundleFailed")
}

// classNamingToolchains builds (but does not create) a SpiceboxClass whose
// spec.toolchains names the given toolchain names, for exercising the
// resolve/freeze path. RuntimeClassName is left unset (podspec.Build leaves
// pod.Spec.RuntimeClassName nil in that case), so no RuntimeClass object is
// required for the resulting pod to admit.
func classNamingToolchains(t *testing.T, name string, toolchainNames ...string) *spiceboxv1alpha1.SpiceboxClass {
	t.Helper()
	return &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image: "registry.example.com/toolbelt:latest",
			Resources: spiceboxv1alpha1.SpiceboxResources{
				CPU:              resource.MustParse("500m"),
				Memory:           resource.MustParse("256Mi"),
				EphemeralStorage: resource.MustParse("100Mi"),
			},
			Tools:      []spiceboxv1alpha1.SpiceboxTool{{Name: "echo", Command: []string{"/bin/echo"}}},
			Toolchains: toolchainNames,
		},
	}
}

// sessionForClass builds (but does not create) a minimal SpiceboxSession
// bound to the given class, in the "default" namespace.
func sessionForClass(t *testing.T, name, className string) *spiceboxv1alpha1.SpiceboxSession {
	t.Helper()
	return &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: className},
	}
}

// TestSession_ToolchainMissing_ReadyFalseAndNoPod is the fail-closed guard: a
// class naming a toolchain that does not exist must leave the session
// Ready=False/ToolchainMissing and must NOT create a pod. A session whose class
// asks for a compiler that isn't in the catalog must not quietly start without
// it.
func TestSession_ToolchainMissing_ReadyFalseAndNoPod(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	mustCreate(t, env.Client, classNamingToolchains(t, "cls-missing-tc", "ghost"))
	sess := sessionForClass(t, "sess-missing-tc", "cls-missing-tc")
	mustCreate(t, env.Client, sess)

	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxSession
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		c := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.SpiceboxSessionConditionReady)
		return c != nil && c.Status == metav1.ConditionFalse &&
			c.Reason == spiceboxv1alpha1.ReasonToolchainMissing
	})

	// The pod must not exist: a session that cannot get its compiler must not run.
	var pod corev1.Pod
	err := env.Client.Get(context.Background(),
		client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name + "-pod"}, &pod)
	assert.True(t, errors.IsNotFound(err),
		"no pod may be created for a session whose toolchain never resolved")
}

// TestSession_ToolchainInvalid_ReadyFalseNotValidReason is the fail-closed
// guard for a toolchain that exists but is not Valid=True: the session must
// still refuse to start, with a distinct reason from ToolchainMissing so an
// operator can tell "doesn't exist" from "exists but broken" apart.
func TestSession_ToolchainInvalid_ReadyFalseNotValidReason(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	// Created but never reconciled to Valid=True (no toolchain controller running
	// in this manager), so it stands in for a Valid=False catalog entry.
	mustCreate(t, env.Client, &spiceboxv1alpha1.SpiceboxToolchain{
		ObjectMeta: metav1.ObjectMeta{Name: "half-baked"},
		Spec: spiceboxv1alpha1.SpiceboxToolchainSpec{
			Source:    spiceboxv1alpha1.ToolchainSource{Kind: "image", Image: "x:dev", Prefix: "/opt/ap-toolchains/half-baked"},
			SizeBytes: 1,
		},
	})
	mustCreate(t, env.Client, classNamingToolchains(t, "cls-invalid-tc", "half-baked"))
	sess := sessionForClass(t, "sess-invalid-tc", "cls-invalid-tc")
	mustCreate(t, env.Client, sess)

	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxSession
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		c := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.SpiceboxSessionConditionReady)
		return c != nil && c.Status == metav1.ConditionFalse &&
			c.Reason == spiceboxv1alpha1.ReasonToolchainNotValid
	})
}

// TestSession_ToolchainValid_FreezesResolvedToolchains is the happy path: a
// class naming a Valid=True toolchain must freeze ResolvedToolchains with env
// already expanded (the pod builder must never see a template) and a 64-char
// hex set digest.
func TestSession_ToolchainValid_FreezesResolvedToolchains(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	tc := &spiceboxv1alpha1.SpiceboxToolchain{
		ObjectMeta: metav1.ObjectMeta{Name: "go"},
		Spec: spiceboxv1alpha1.SpiceboxToolchainSpec{
			Source:    spiceboxv1alpha1.ToolchainSource{Kind: "image", Image: "ap-toolchain-go:dev", Prefix: "/opt/ap-toolchains/go"},
			Bin:       []string{"bin"},
			Env:       map[string]string{"GOCACHE": "{{ .Cache }}/gobuild"},
			SizeBytes: 100,
		},
	}
	mustCreate(t, env.Client, tc)
	// Force Valid=True without running the toolchain controller.
	conditions.SetTrue(tc, &tc.Status.Conditions,
		spiceboxv1alpha1.SpiceboxToolchainConditionValid, spiceboxv1alpha1.ReasonToolchainValid)
	require.NoError(t, env.Client.Status().Update(context.Background(), tc))

	mustCreate(t, env.Client, classNamingToolchains(t, "cls-go", "go"))
	sess := sessionForClass(t, "sess-go", "cls-go")
	mustCreate(t, env.Client, sess)

	var got spiceboxv1alpha1.SpiceboxSession
	eventually(t, 5*time.Second, func() bool {
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		return len(got.Status.ResolvedToolchains) == 1
	})
	assert.Equal(t, "go", got.Status.ResolvedToolchains[0].Name)
	assert.Equal(t, "/var/ap-cache/gobuild", got.Status.ResolvedToolchains[0].Env["GOCACHE"],
		"env is expanded once, at freeze time — the pod builder never sees a template")
	assert.Len(t, got.Status.ToolchainSetDigest, 64)
}

// TestSession_ToolchainValid_RecordsAuditEntryOnce is the headline-behavior
// regression guard this feature exists for: a session whose class resolves a
// non-empty toolchain set must produce exactly one toolchain-audit "resolved"
// entry — attributing to this standalone session's own scope, since no parent
// AgentSession label is set on this fixture — and must never write a second
// one on a later reconcile of the already-frozen session.
func TestSession_ToolchainValid_RecordsAuditEntryOnce(t *testing.T) {
	env := testenv.Shared(t)
	mem := &stubAuditMemory{}
	startManagerWithAudit(t, env, mem)

	tc := &spiceboxv1alpha1.SpiceboxToolchain{
		ObjectMeta: metav1.ObjectMeta{Name: "go-audit"},
		Spec: spiceboxv1alpha1.SpiceboxToolchainSpec{
			Source:    spiceboxv1alpha1.ToolchainSource{Kind: "image", Image: "ap-toolchain-go:dev", Prefix: "/opt/ap-toolchains/go-audit"},
			Bin:       []string{"bin"},
			SizeBytes: 100,
		},
	}
	mustCreate(t, env.Client, tc)
	// Force Valid=True without running the toolchain controller.
	conditions.SetTrue(tc, &tc.Status.Conditions,
		spiceboxv1alpha1.SpiceboxToolchainConditionValid, spiceboxv1alpha1.ReasonToolchainValid)
	require.NoError(t, env.Client.Status().Update(context.Background(), tc))

	mustCreate(t, env.Client, classNamingToolchains(t, "cls-go-audit", "go-audit"))
	sess := sessionForClass(t, "sess-go-audit", "cls-go-audit")
	mustCreate(t, env.Client, sess)

	var got spiceboxv1alpha1.SpiceboxSession
	eventually(t, 5*time.Second, func() bool {
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		return len(got.Status.ResolvedToolchains) == 1
	})

	// recordResolved fires from inside persistStatus, right after the
	// Status().Update that makes ResolvedToolchains visible above — so the Put
	// should already be there, but poll briefly rather than assume ordering.
	eventually(t, 5*time.Second, func() bool { return len(mem.snapshot()) >= 1 })

	entries := mem.snapshot()
	require.Len(t, entries, 1, "exactly one audit entry must be written for one toolchain resolution")

	entry := entries[0]
	assert.Equal(t, "toolchain_audit", entry.Kind)
	assert.Equal(t, memory.Scope{Kind: "session", ID: "default/sess-go-audit"}, entry.Scope,
		"a standalone session (no parent agentsession label) attests under its own scope")

	var content toolchainaudit.Content
	require.NoError(t, json.Unmarshal(entry.Content, &content), "unmarshal audit Content")
	assert.Equal(t, "resolved", content.Phase)
	require.Len(t, content.Resolved, 1, "one toolchain was resolved")
	assert.Equal(t, "go-audit", content.Resolved[0].Name)
	assert.Equal(t, "ap-toolchain-go:dev", content.Resolved[0].Image)
	assert.Equal(t, got.Status.ToolchainSetDigest, content.SetDigest,
		"the audited digest must match the frozen status digest")

	// Give the controller a few more reconcile cycles' worth of time (Pod
	// creation on the next pass, potential resyncs) and confirm the count
	// stays 1 — this is a one-time attestation of the freeze, not a
	// per-reconcile log entry.
	time.Sleep(1 * time.Second)
	assert.Len(t, mem.snapshot(), 1, "audit entry must be written once, not on every reconcile")
}

// TestSession_ZeroToolchainClass_PodCreatedSamePassAsResolvedClass is the
// regression guard for the requeue-gating fix: a class naming ZERO
// toolchains has no resolution to persist and no audit entry to attest (see
// frozeThisPass in controller.go), so the freeze gate must NOT force the
// extra persist-then-requeue reconcile pass that a toolchain-bearing session
// pays for. If it paid that extra pass anyway, Pod creation would be deferred
// to a second reconcile: the FIRST status update to set Status.ResolvedClass
// would carry an empty Status.PodName, and a later, separate status update
// would set PodName.
//
// A poll-then-assert ("eventually PodName != "" && ResolvedClass != nil") is
// unusable proof here: by the time the poll observes both fields set, an
// arbitrary number of reconciles could already have run, so it cannot tell
// "set together in one pass" apart from "set in two passes that both
// completed before the poll happened to fire" — it would pass whether or not
// this fix regressed. Instead this test watches the SpiceboxSession's status
// updates directly and inspects the very FIRST one where ResolvedClass
// becomes non-nil: that update's PodName is the proof. If gating regresses to
// boundThisPass alone, that first update's PodName will be empty and this
// test fails.
func TestSession_ZeroToolchainClass_PodCreatedSamePassAsResolvedClass(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	createClass(t, env.Client, "cls-same-pass")

	watchCli, err := client.NewWithWatch(env.Cfg, client.Options{Scheme: env.Scheme})
	require.NoError(t, err, "client.NewWithWatch")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	w, err := watchCli.Watch(ctx, &spiceboxv1alpha1.SpiceboxSessionList{}, client.InNamespace("default"))
	require.NoError(t, err, "Watch SpiceboxSessionList")
	defer w.Stop()

	sess := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s-same-pass", Namespace: "default"},
		Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "cls-same-pass"},
	}
	mustCreate(t, env.Client, sess)

	var (
		sawResolvedClass     bool
		podNameAtFirstFreeze string
	)
watchLoop:
	for {
		select {
		case ev, ok := <-w.ResultChan():
			if !ok {
				t.Fatal("watch channel closed before Status.ResolvedClass was observed")
			}
			got, ok := ev.Object.(*spiceboxv1alpha1.SpiceboxSession)
			if !ok || got.Name != sess.Name {
				continue
			}
			if got.Status.ResolvedClass != nil {
				sawResolvedClass = true
				podNameAtFirstFreeze = got.Status.PodName
				break watchLoop
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for Status.ResolvedClass to be populated")
		}
	}

	require.True(t, sawResolvedClass, "must observe a status update with ResolvedClass populated")
	assert.NotEmpty(t, podNameAtFirstFreeze,
		"the FIRST status update that freezes ResolvedClass for a zero-toolchain session must already carry "+
			"PodName, proving Pod creation happened in the SAME reconcile pass as the freeze — no extra "+
			"persist-then-requeue round-trip was paid for a resolution with nothing to attest")
}

// TestSession_ToolchainBecomesValidLater_SelfHealsWithoutResync is the watch
// regression guard for the SpiceboxToolchain watch added to SetupWithManager:
// a toolchain flipping Valid=True after a session already fail-closed on it
// does not touch the SpiceboxClass that names it (mapClassToSessions never
// fires), and there is no Pod yet for Owns(&corev1.Pod{}) to fire on either —
// only a SpiceboxToolchain watch can re-enqueue the session. Without that
// watch the session would stall Ready=False/ToolchainNotValid until the
// manager's ~10h default resync; this test's `eventually` bound (10s, many
// orders of magnitude below that) is what makes a pass here proof of the
// watch, not the resync.
func TestSession_ToolchainBecomesValidLater_SelfHealsWithoutResync(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	// Created but never marked Valid=True (no toolchain controller running in
	// this manager) — stands in for a catalog entry that hasn't converged yet,
	// e.g. an apply-ordering race between the session and the operator marking
	// the toolchain Valid=True.
	tc := &spiceboxv1alpha1.SpiceboxToolchain{
		ObjectMeta: metav1.ObjectMeta{Name: "late-valid"},
		Spec: spiceboxv1alpha1.SpiceboxToolchainSpec{
			Source:    spiceboxv1alpha1.ToolchainSource{Kind: "image", Image: "x:dev", Prefix: "/opt/ap-toolchains/late-valid"},
			SizeBytes: 1,
		},
	}
	mustCreate(t, env.Client, tc)

	mustCreate(t, env.Client, classNamingToolchains(t, "cls-late-valid-tc", "late-valid"))
	sess := sessionForClass(t, "sess-late-valid-tc", "cls-late-valid-tc")
	mustCreate(t, env.Client, sess)

	// The session must fail closed first: Ready=False/ToolchainNotValid, no
	// ResolvedToolchains yet.
	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxSession
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		c := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.SpiceboxSessionConditionReady)
		return c != nil && c.Status == metav1.ConditionFalse &&
			c.Reason == spiceboxv1alpha1.ReasonToolchainNotValid
	})

	// Now flip the toolchain to Valid=True — nothing about the SpiceboxClass or
	// the SpiceboxSession changes.
	conditions.SetTrue(tc, &tc.Status.Conditions,
		spiceboxv1alpha1.SpiceboxToolchainConditionValid, spiceboxv1alpha1.ReasonToolchainValid)
	require.NoError(t, env.Client.Status().Update(context.Background(), tc), "mark toolchain Valid=True")

	// The session must self-heal WITHOUT any resync: ResolvedToolchains
	// populates and Ready is no longer False/ToolchainNotValid.
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxSession
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		return len(got.Status.ResolvedToolchains) == 1 && got.Status.ResolvedToolchains[0].Name == "late-valid"
	})
}

// TestSession_BoundSessionNotEnqueuedByToolchainEvent proves the watch
// narrowing itself, as a complement to the self-heal test above: a session
// that is ALREADY bound (Status.ResolvedClass != nil, so its toolchains are
// frozen for the session's lifetime — resolveToolchains never runs again)
// must NOT be re-enqueued by an unrelated SpiceboxToolchain event.
//
// The resourceVersion check below is meaningful, not just a proxy for
// "content changed": empirically (verified against this envtest apiserver
// with a standalone probe before writing this test) a Status().Update call
// bumps resourceVersion even when the status content is byte-identical to
// what's already stored — the apiserver does not short-circuit no-op
// writes. So if the map func enqueued this bound session, the resulting
// Reconcile pass's persistStatus() would still tick resourceVersion forward
// even though every field it recomputes lands on the same values. That
// makes an unchanged resourceVersion across the window below real evidence
// that Reconcile never ran for this session in response to the toolchain
// event — not a coincidence of idempotent output.
func TestSession_BoundSessionNotEnqueuedByToolchainEvent(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	createClass(t, env.Client, "cls-bound-noenqueue")
	sess := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s-bound-noenqueue", Namespace: "default"},
		Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "cls-bound-noenqueue"},
	}
	mustCreate(t, env.Client, sess)

	// Wait until the session has fully settled: ResolvedClass frozen (bound)
	// AND ObservedGeneration stamped. ObservedGeneration is only set on the
	// reconcile pass that finds the already-created Pod (the pass AFTER pod
	// creation), so waiting for both together — rather than just
	// ResolvedClass != nil — avoids racing the in-flight settling writes that
	// would otherwise make the resourceVersion snapshot below unstable for
	// reasons that have nothing to do with the toolchain watch.
	var got spiceboxv1alpha1.SpiceboxSession
	eventually(t, 10*time.Second, func() bool {
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		return got.Status.ResolvedClass != nil && got.Status.ObservedGeneration == got.Generation
	})
	require.NotNil(t, got.Status.ResolvedClass, "session must be bound before this assertion is meaningful")
	boundRV := got.ResourceVersion

	// An unrelated toolchain — named by no class in this test — must not
	// touch the already-bound session.
	mustCreate(t, env.Client, &spiceboxv1alpha1.SpiceboxToolchain{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated-noenqueue"},
		Spec: spiceboxv1alpha1.SpiceboxToolchainSpec{
			Source:    spiceboxv1alpha1.ToolchainSource{Kind: "image", Image: "x:dev", Prefix: "/opt/ap-toolchains/unrelated-noenqueue"},
			SizeBytes: 1,
		},
	})

	// Poll across the whole window rather than sleeping once and checking at
	// the end: a transient write-then-revert would be invisible to a single
	// end-of-window check but would still prove the watch fired.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var cur spiceboxv1alpha1.SpiceboxSession
		require.NoError(t, env.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &cur),
			"Get SpiceboxSession")
		require.Equal(t, boundRV, cur.ResourceVersion,
			"a bound session's resourceVersion must not change in response to an unrelated SpiceboxToolchain event")
		time.Sleep(100 * time.Millisecond)
	}
}
