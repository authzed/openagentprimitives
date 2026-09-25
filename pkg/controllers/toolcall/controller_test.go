//go:build integration

package toolcall_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxtoolkit"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxtoolspec"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/controllers/toolcall"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	execpkg "github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
	_ "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod" // register the built-in sandbox backend so spiceboxclass.Reconciler resolves it
	toolspecregistry "github.com/authzed/openagentprimitives/pkg/tools/toolspec/registry"
	"github.com/authzed/openagentprimitives/pkg/web/gateway"
)

func TestToolCall_SucceedsAndCapturesStdout(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	createReadyClassAndSession(t, env.Client, "cls-a", "sess-a")

	fakeExec.Program("default/sess-a-pod:sandbox", fake.Response{
		Stdout:   []byte("hello world\n"),
		ExitCode: 0,
	})

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-1", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-a",
			Tool:    "echo",
			Args:    []string{"hello", "world"},
			Timeout: metav1.Duration{Duration: 10 * time.Second},
		},
	}
	mustCreate(t, env.Client, tc)

	var final spiceboxv1alpha1.ToolCall
	eventually(t, 10*time.Second, func() bool {
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &final); err != nil {
			return false
		}
		return hasTrueCondition(&final, spiceboxv1alpha1.ToolCallConditionSucceeded)
	})

	require.NotNil(t, final.Status.ExitCode, "ExitCode should be set after Succeeded")
	assert.Equal(t, int32(0), *final.Status.ExitCode, "ExitCode for happy path")
	assert.NotEmpty(t, final.Status.StdoutArtifactRef, "StdoutArtifactRef should be populated")

	got, err := store.Get(context.Background(), artifactstore.Ref(final.Status.StdoutArtifactRef))
	require.NoError(t, err, "store.Get stdout artifact")
	defer got.Close()
	buf, _ := io.ReadAll(got)
	assert.Equal(t, "hello world\n", string(buf), "stdout artifact contents")
}

func TestToolCall_NonZeroExitMarksFailed(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	createReadyClassAndSession(t, env.Client, "cls-b", "sess-b")
	fakeExec.Program("default/sess-b-pod:sandbox", fake.Response{
		Stderr:   []byte("boom\n"),
		ExitCode: 7,
	})

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-fail", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-b", Tool: "echo", Timeout: metav1.Duration{Duration: 10 * time.Second},
		},
	}
	mustCreate(t, env.Client, tc)

	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got); err != nil {
			return false
		}
		if !hasTrueCondition(&got, spiceboxv1alpha1.ToolCallConditionFailed) {
			return false
		}
		return got.Status.ExitCode != nil && *got.Status.ExitCode == 7
	})
}

func TestToolCall_UnknownToolMarksFailed(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	createReadyClassAndSession(t, env.Client, "cls-c", "sess-c")

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-unknown", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ToolCallSpec{Session: "sess-c", Tool: "nosuchtool"},
	}
	mustCreate(t, env.Client, tc)

	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got); err != nil {
			return false
		}
		for _, c := range got.Status.Conditions {
			if c.Type == spiceboxv1alpha1.ToolCallConditionFailed &&
				c.Status == metav1.ConditionTrue &&
				c.Reason == spiceboxv1alpha1.ReasonToolUnknown {
				return true
			}
		}
		return false
	})
}

func TestToolCall_TimeoutMarksTimeout(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	createReadyClassAndSession(t, env.Client, "cls-t", "sess-t")
	fakeExec.Program("default/sess-t-pod:sandbox", fake.Response{
		Delay:    2 * time.Second,
		ExitCode: 0,
	})

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-timeout", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-t", Tool: "echo",
			Timeout: metav1.Duration{Duration: 200 * time.Millisecond},
		},
	}
	mustCreate(t, env.Client, tc)

	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got); err != nil {
			return false
		}
		return hasTrueCondition(&got, spiceboxv1alpha1.ToolCallConditionTimeout)
	})
}

// --- helpers ---

func startManagerWithExec(t *testing.T, env *testenv.Env, binder *fake.Binder, store artifactstore.Store) {
	t.Helper()
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:  env.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: ctrlconfig.Controller{
			SkipNameValidation: ptr.To(true),
		},
	})
	require.NoError(t, err, "NewManager")
	require.NoError(t,
		(&spiceboxclass.Reconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr),
		"class setup")
	// The session and toolcall controllers share one Runtimes map — built
	// from the same binder the test programs — so the sandbox handle the
	// session controller records is one the toolcall controller can actually
	// resolve an executor for.
	runtimes := testenv.SandboxRuntimesWithExec(t, mgr.GetClient(), binder.For)
	require.NoError(t,
		(&spiceboxsession.Reconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
			Runtimes: runtimes,
		}).SetupWithManager(mgr),
		"session setup")
	toolkitReg, err := toolspecregistry.NewWithBuiltins(mgr.GetClient())
	require.NoError(t, err, "toolspec registry")
	require.NoError(t,
		(&spiceboxtoolkit.Reconciler{Client: mgr.GetClient(), Registry: toolkitReg}).SetupWithManager(mgr),
		"toolkit setup")
	require.NoError(t,
		(&spiceboxtoolspec.Reconciler{Client: mgr.GetClient(), Registry: toolkitReg}).SetupWithManager(mgr),
		"toolspec setup")
	require.NoError(t, (&toolcall.Reconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Runtimes:        runtimes,
		Store:           store,
		Registry:        gateway.NewRegistry(),
		GatewayEndpoint: "test:8443",
		ToolkitRegistry: toolkitReg,
		APIReader:       mgr.GetAPIReader(),
	}).SetupWithManager(mgr), "toolcall setup")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
}

// createReadyClassAndSession creates a SpiceboxClass with "echo" and "cat" tools (with toolspecs),
// a SpiceboxSession, then patches the pod to Ready so the session controller flips Ready=True.
func createReadyClassAndSession(t *testing.T, cli client.Client, className, sessName string) {
	t.Helper()

	// Toolspecs covering echo and cat (built-in toolkits at revision 2026-04-25).
	// Use a per-class name suffix so parallel tests don't collide.
	echoSpecName := "echo-default-" + className
	catSpecName := "cat-default-" + className
	createReadyToolspec(t, cli, echoSpecName, "echo", "2026-04-25")
	createReadyToolspec(t, cli, catSpecName, "cat", "2026-04-25")

	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: className},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image: "registry.example.com/toolbelt:latest",
			Resources: spiceboxv1alpha1.SpiceboxResources{
				CPU: resource.MustParse("500m"), Memory: resource.MustParse("256Mi"),
				EphemeralStorage: resource.MustParse("100Mi"),
			},
			Tools: []spiceboxv1alpha1.SpiceboxTool{
				{Name: "echo", Command: []string{"/bin/echo"}},
				{Name: "cat", Command: []string{"/bin/cat"}},
			},
			Toolspecs: []spiceboxv1alpha1.ToolspecRef{
				{Name: echoSpecName},
				{Name: catSpecName},
			},
		},
	}
	mustCreate(t, cli, cls)

	sess := newOwnedBundle(t, cli, sessName, className)
	mustCreate(t, cli, sess)

	podKey := types.NamespacedName{Name: sessName + "-pod", Namespace: "default"}
	eventually(t, 10*time.Second, func() bool {
		var pod corev1.Pod
		if err := cli.Get(context.Background(), podKey, &pod); err != nil {
			return false
		}
		var got spiceboxv1alpha1.SpiceboxSession
		return cli.Get(context.Background(), client.ObjectKeyFromObject(sess), &got) == nil &&
			got.Status.ResolvedClass != nil
	})

	// envtest has no kubelet, so we mark the Pod ready manually.
	var pod corev1.Pod
	require.NoError(t, cli.Get(context.Background(), podKey, &pod), "get pod")
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, cli.Status().Update(context.Background(), &pod), "patch pod status")

	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxSession
		if err := cli.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
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

func hasTrueCondition(tc *spiceboxv1alpha1.ToolCall, condType string) bool {
	for _, c := range tc.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func hasTrueConditionToolspec(ts *spiceboxv1alpha1.SpiceboxToolspec, condType string) bool {
	for _, c := range ts.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func createReadyToolspec(t *testing.T, cli client.Client, name, toolkitName, toolkitRev string) {
	t.Helper()
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			// Name and Version are required by spec.LoadBytes (called at validation time).
			Name:             name,
			Version:          "1",
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: toolkitName, Revision: toolkitRev},
			AllowSubcommands: []string{""},
		},
	}
	mustCreate(t, cli, ts)
	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxToolspec
		if err := cli.Get(context.Background(), client.ObjectKeyFromObject(ts), &got); err != nil {
			return false
		}
		return hasTrueConditionToolspec(&got, spiceboxv1alpha1.SpiceboxToolspecConditionValid)
	})
}

// agentSessionNameFor is the AgentSession that owns the bundle SpiceboxSession
// named bundleName in these fixtures. Deterministic so no test has to thread a
// handle around.
func agentSessionNameFor(bundleName string) string { return "ag-" + bundleName }

// newOwnedBundle returns the bundle SpiceboxSession for className, having first
// created the AgentSession that owns it and labelled the bundle back to it.
//
// Every bundle in these fixtures goes through here, because the reconcile-path
// session-binding guard reads exactly that pair — an unowned bundle, or one
// with no back-label, is a shape the AgentSession reconciler never produces.
func newOwnedBundle(t *testing.T, cli client.Client, bundleName, className string) *spiceboxv1alpha1.SpiceboxSession {
	t.Helper()
	agentSess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: agentSessionNameFor(bundleName), Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: className},
	}
	mustCreate(t, cli, agentSess)
	return &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: bundleName, Namespace: "default",
			// The literal, not the package const: this file is an external test
			// package, and pinning the wire value is what a fixture should do
			// anyway — matching whatever the const happens to say would assert
			// nothing about the label the controller actually reads.
			Labels: map[string]string{"agentprimitives.authzed.com/agentsession": agentSess.Name},
		},
		Spec: spiceboxv1alpha1.SpiceboxSessionSpec{Class: className},
	}
}

// mustCreate creates obj, and for a ToolCall FIRST binds it to the AgentSession
// that owns its bundle.
//
// Production always does this: pkg/agent/tool/sandbox stamps an AgentSession
// controller ownerReference on every ToolCall it creates. The fixtures did not,
// and that gap was invisible for as long as nothing read the owner — the moment
// the reconcile-path session-binding guard did, every test in this file failed
// at once. An unowned ToolCall is not a shape the system produces, so leaving
// the fixtures that way would have meant weakening the guard to match a
// fiction.
func mustCreate(t *testing.T, cli client.Client, obj client.Object) {
	t.Helper()
	if tc, ok := obj.(*spiceboxv1alpha1.ToolCall); ok && tc.Spec.Session != "" && len(tc.OwnerReferences) == 0 {
		bindToolCallToAgentSession(t, cli, tc)
	}
	require.NoError(t, cli.Create(context.Background(), obj),
		"create %T (%s)", obj, client.ObjectKeyFromObject(obj))
}

// bindToolCallToAgentSession stamps the controller ownerReference production
// stamps, resolving the AgentSession's real UID from the cluster (an
// ownerReference with a wrong UID is rejected by garbage collection, and a
// fabricated one would make the fixture lie in a second way).
func bindToolCallToAgentSession(t *testing.T, cli client.Client, tc *spiceboxv1alpha1.ToolCall) {
	t.Helper()
	var ag spiceboxv1alpha1.AgentSession
	key := client.ObjectKey{Namespace: tc.Namespace, Name: agentSessionNameFor(tc.Spec.Session)}
	require.NoError(t, cli.Get(context.Background(), key, &ag),
		"the owning AgentSession must exist before a ToolCall names its bundle")
	tval := true
	tc.OwnerReferences = []metav1.OwnerReference{{
		APIVersion:         spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
		Kind:               "AgentSession",
		Name:               ag.Name,
		UID:                ag.UID,
		Controller:         &tval,
		BlockOwnerDeletion: &tval,
	}}
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

func TestToolCall_BumpsSessionActivity(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	createReadyClassAndSession(t, env.Client, "cls-bump", "sess-bump")
	fakeExec.Program("default/sess-bump-pod:sandbox", fake.Response{ExitCode: 0})

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-bump", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-bump", Tool: "echo",
			Timeout: metav1.Duration{Duration: 5 * time.Second},
		},
	}
	mustCreate(t, env.Client, tc)

	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got); err != nil {
			return false
		}
		return hasTrueCondition(&got, spiceboxv1alpha1.ToolCallConditionSucceeded)
	})

	eventually(t, 5*time.Second, func() bool {
		var sess spiceboxv1alpha1.SpiceboxSession
		if err := env.Client.Get(context.Background(),
			types.NamespacedName{Name: "sess-bump", Namespace: "default"}, &sess); err != nil {
			return false
		}
		return sess.Status.CallCount >= 1 && sess.Status.LastActivityAt != nil
	})
}

func TestToolCall_DeleteWhileNotRunningCleansUp(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	createReadyClassAndSession(t, env.Client, "cls-del", "sess-del")
	fakeExec.Program("default/sess-del-pod:sandbox", fake.Response{ExitCode: 0})

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-del", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-del", Tool: "echo",
			Timeout: metav1.Duration{Duration: 5 * time.Second},
		},
	}
	mustCreate(t, env.Client, tc)

	// Wait until terminal.
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got); err != nil {
			return false
		}
		return hasTrueCondition(&got, spiceboxv1alpha1.ToolCallConditionSucceeded)
	})

	// Delete: finalizer runs, object disappears.
	require.NoError(t, env.Client.Delete(context.Background(), tc), "delete ToolCall")
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got)
		return err != nil
	})
}

func TestToolCall_EnvPassedToExec(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	createReadyClassAndSession(t, env.Client, "cls-env", "sess-env")
	fakeExec.Program("default/sess-env-pod:sandbox", fake.Response{ExitCode: 0})

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-env", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-env", Tool: "echo",
			Env:     map[string]string{"FOO": "bar", "BAZ": "qux"},
			Timeout: metav1.Duration{Duration: 5 * time.Second},
		},
	}
	mustCreate(t, env.Client, tc)

	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got); err != nil {
			return false
		}
		return hasTrueCondition(&got, spiceboxv1alpha1.ToolCallConditionSucceeded)
	})

	calls := fakeExec.Calls()
	require.NotEmpty(t, calls, "fake exec should have observed at least one call")
	assert.Equal(t, "bar", calls[len(calls)-1].Request.Env["FOO"], "FOO env passed to exec")
}

func TestToolCall_StdinPipedThrough(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	createReadyClassAndSession(t, env.Client, "cls-stdin", "sess-stdin")
	fakeExec.Program("default/sess-stdin-pod:sandbox", fake.Response{ExitCode: 0})

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-stdin", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-stdin", Tool: "echo",
			Stdin:   "payload-under-test",
			Timeout: metav1.Duration{Duration: 5 * time.Second},
		},
	}
	mustCreate(t, env.Client, tc)

	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got); err != nil {
			return false
		}
		return hasTrueCondition(&got, spiceboxv1alpha1.ToolCallConditionSucceeded)
	})

	calls := fakeExec.Calls()
	require.NotEmpty(t, calls, "fake exec should have observed at least one call")
	last := calls[len(calls)-1]
	assert.Equal(t, "payload-under-test", string(last.Stdin), "stdin piped through to exec")
}

func TestToolCall_SessionNotReadyMarksFailed(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	// Create class + session but DO NOT mark the pod ready; session stays not-Ready.
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-nr"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image: "x",
			Resources: spiceboxv1alpha1.SpiceboxResources{
				CPU: resource.MustParse("500m"), Memory: resource.MustParse("256Mi"),
				EphemeralStorage: resource.MustParse("100Mi"),
			},
			Tools: []spiceboxv1alpha1.SpiceboxTool{{Name: "echo", Command: []string{"/bin/echo"}}},
		},
	}
	mustCreate(t, env.Client, cls)
	sess := newOwnedBundle(t, env.Client, "sess-nr", "cls-nr")
	mustCreate(t, env.Client, sess)

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-nr", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ToolCallSpec{Session: "sess-nr", Tool: "echo"},
	}
	mustCreate(t, env.Client, tc)

	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got); err != nil {
			return false
		}
		for _, c := range got.Status.Conditions {
			if c.Type == spiceboxv1alpha1.ToolCallConditionFailed &&
				c.Status == metav1.ConditionTrue &&
				c.Reason == spiceboxv1alpha1.ReasonSessionNotActive {
				return true
			}
		}
		return false
	})
}

func TestToolCall_OperatorRestartMidExecMarksFailed(t *testing.T) {
	env := testenv.Shared(t)
	store := blobstore.NewMem()

	// Phase 1: run a manager with a long-delay exec so we can catch the ToolCall in Running state,
	// then cancel that manager to simulate an operator crash mid-exec.
	fakeExec1 := fake.New()
	fakeExec1.Program("default/sess-rs-pod:sandbox", fake.Response{
		Delay:    30 * time.Second, // long enough to survive the test
		ExitCode: 0,
	})
	ctx1, cancel1 := context.WithCancel(context.Background())
	mgr1, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:  env.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: ctrlconfig.Controller{
			SkipNameValidation: ptr.To(true),
		},
	})
	require.NoError(t, err, "NewManager1")
	require.NoError(t, (&spiceboxclass.Reconciler{Client: mgr1.GetClient(), Scheme: mgr1.GetScheme()}).SetupWithManager(mgr1), "class setup1")
	runtimes1 := testenv.SandboxRuntimesWithExec(t, mgr1.GetClient(), fakeExec1.For)
	require.NoError(t, (&spiceboxsession.Reconciler{
		Client: mgr1.GetClient(), Scheme: mgr1.GetScheme(),
		Runtimes: runtimes1,
	}).SetupWithManager(mgr1), "session setup1")
	toolkitReg1, err := toolspecregistry.NewWithBuiltins(mgr1.GetClient())
	require.NoError(t, err, "registry1")
	require.NoError(t, (&spiceboxtoolkit.Reconciler{Client: mgr1.GetClient(), Registry: toolkitReg1}).SetupWithManager(mgr1), "toolkit setup1")
	require.NoError(t, (&spiceboxtoolspec.Reconciler{Client: mgr1.GetClient(), Registry: toolkitReg1}).SetupWithManager(mgr1), "toolspec setup1")
	require.NoError(t, (&toolcall.Reconciler{
		Client:          mgr1.GetClient(),
		Scheme:          mgr1.GetScheme(),
		Runtimes:        runtimes1,
		Store:           store,
		Registry:        gateway.NewRegistry(),
		GatewayEndpoint: "test:8443",
		ToolkitRegistry: toolkitReg1,
		APIReader:       mgr1.GetAPIReader(),
	}).SetupWithManager(mgr1), "toolcall setup1")
	go func() { _ = mgr1.Start(ctx1) }()
	t.Cleanup(cancel1)

	// Manager1 must be running before createReadyClassAndSession so the session controller runs.
	createReadyClassAndSession(t, env.Client, "cls-rs", "sess-rs")

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-rs", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ToolCallSpec{Session: "sess-rs", Tool: "echo"},
	}
	mustCreate(t, env.Client, tc)

	// Wait for Running=True (manager1 is blocked in the 30s-delayed exec).
	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		return env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got) == nil &&
			hasTrueCondition(&got, spiceboxv1alpha1.ToolCallConditionRunning)
	})

	// Simulate operator crash: cancel manager1. The in-flight exec goroutine is cancelled.
	cancel1()

	// Phase 2: start a fresh manager (simulating operator restart).
	fakeExec2 := fake.New()
	fakeExec2.Program("default/sess-rs-pod:sandbox", fake.Response{ExitCode: 0})
	ctx2, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(cancel2)
	mgr2, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:  env.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: ctrlconfig.Controller{
			SkipNameValidation: ptr.To(true),
		},
	})
	require.NoError(t, err, "NewManager2")
	require.NoError(t, (&spiceboxclass.Reconciler{Client: mgr2.GetClient(), Scheme: mgr2.GetScheme()}).SetupWithManager(mgr2), "class setup2")
	runtimes2 := testenv.SandboxRuntimesWithExec(t, mgr2.GetClient(), fakeExec2.For)
	require.NoError(t, (&spiceboxsession.Reconciler{
		Client: mgr2.GetClient(), Scheme: mgr2.GetScheme(),
		Runtimes: runtimes2,
	}).SetupWithManager(mgr2), "session setup2")
	toolkitReg2, err := toolspecregistry.NewWithBuiltins(mgr2.GetClient())
	require.NoError(t, err, "registry2")
	require.NoError(t, (&spiceboxtoolkit.Reconciler{Client: mgr2.GetClient(), Registry: toolkitReg2}).SetupWithManager(mgr2), "toolkit setup2")
	require.NoError(t, (&spiceboxtoolspec.Reconciler{Client: mgr2.GetClient(), Registry: toolkitReg2}).SetupWithManager(mgr2), "toolspec setup2")
	require.NoError(t, (&toolcall.Reconciler{
		Client:          mgr2.GetClient(),
		Scheme:          mgr2.GetScheme(),
		Runtimes:        runtimes2,
		Store:           store,
		Registry:        gateway.NewRegistry(),
		GatewayEndpoint: "test:8443",
		ToolkitRegistry: toolkitReg2,
		APIReader:       mgr2.GetAPIReader(),
	}).SetupWithManager(mgr2), "toolcall setup2")
	go func() { _ = mgr2.Start(ctx2) }()

	// The fresh manager reconciles tc-rs, sees Running=True with no terminal condition,
	// and stamps Failed/OperatorRestart.
	eventually(t, 10*time.Second, func() bool {
		var g spiceboxv1alpha1.ToolCall
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &g); err != nil {
			return false
		}
		for _, c := range g.Status.Conditions {
			if c.Type == spiceboxv1alpha1.ToolCallConditionFailed &&
				c.Status == metav1.ConditionTrue &&
				c.Reason == spiceboxv1alpha1.ReasonOperatorRestart {
				return true
			}
		}
		return false
	})
}

func TestToolCall_DeleteWhileRunningMarksCanceled(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	createReadyClassAndSession(t, env.Client, "cls-drw", "sess-drw")

	// Long delay so we can catch the ToolCall in Running state.
	fakeExec.Program("default/sess-drw-pod:sandbox", fake.Response{
		Delay:    3 * time.Second,
		ExitCode: 0,
	})

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-drw", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-drw", Tool: "echo",
			Timeout: metav1.Duration{Duration: 10 * time.Second},
		},
	}
	mustCreate(t, env.Client, tc)

	// Wait for Running=True
	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got); err != nil {
			return false
		}
		return hasTrueCondition(&got, spiceboxv1alpha1.ToolCallConditionRunning)
	})

	// Delete mid-execution.
	require.NoError(t, env.Client.Delete(context.Background(), tc), "delete mid-exec ToolCall")

	// Eventually the object disappears — verifies finalizer ran (which stamps Canceled).
	eventually(t, 15*time.Second, func() bool {
		var g spiceboxv1alpha1.ToolCall
		err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &g)
		return err != nil
	})
}

func TestToolCall_HarvestsOutputs(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	createReadyClassAndSession(t, env.Client, "cls-out", "sess-out")

	// Main exec: sync mode, produces exit 0.
	fakeExec.Program("default/sess-out-pod:sandbox", fake.Response{ExitCode: 0})

	// Harvest exec: stream mode, emits a tar archive with one file.
	var harvestTar bytes.Buffer
	tw := tar.NewWriter(&harvestTar)
	_ = tw.WriteHeader(&tar.Header{Name: "results.json", Mode: 0o644, Size: int64(len(`{"ok":true}`))})
	_, _ = tw.Write([]byte(`{"ok":true}`))
	_ = tw.Close()

	fakeExec.ProgramStream("default/sess-out-pod:sandbox", fake.StreamResponse{
		Stdout:   harvestTar.Bytes(),
		ExitCode: 0,
	})

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-out", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-out", Tool: "echo",
			Timeout:        metav1.Duration{Duration: 10 * time.Second},
			CaptureOutputs: []string{"/work/out/{{.callId}}"},
		},
	}
	mustCreate(t, env.Client, tc)

	var final spiceboxv1alpha1.ToolCall
	eventually(t, 15*time.Second, func() bool {
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &final); err != nil {
			return false
		}
		return hasTrueCondition(&final, spiceboxv1alpha1.ToolCallConditionSucceeded)
	})

	require.Len(t, final.Status.OutputArtifacts, 1,
		"OutputArtifacts: %+v", final.Status.OutputArtifacts)
	oa := final.Status.OutputArtifacts[0]
	assert.Equal(t, "results.json", oa.Path, "OutputArtifact.Path")
	assert.Equal(t, int64(len(`{"ok":true}`)), oa.Size, "OutputArtifact.Size")

	// Verify the stored artifact contents.
	rc, err := store.Get(context.Background(), artifactstore.Ref(oa.ArtifactRef))
	require.NoError(t, err, "store.Get harvested artifact")
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	assert.Equal(t, `{"ok":true}`, string(body), "stored artifact body")
}

// TestToolCall_CapturesSingleDeclaredFile exercises the file-capture path the
// sandbox secretOutput "file:<path>" source relies on: a single absolute file
// path in CaptureOutputs is harvested into one OutputArtifact whose bytes match
// the file. tar strips the leading '/', so the recorded Path is the slash-
// stripped form of the requested absolute path.
func TestToolCall_CapturesSingleDeclaredFile(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	createReadyClassAndSession(t, env.Client, "cls-file", "sess-file")

	// Main exec: sync mode, produces exit 0 (the producer ran).
	fakeExec.Program("default/sess-file-pod:sandbox", fake.Response{
		Stdout:   []byte("wrote /home/agent/.kube/config\n"),
		ExitCode: 0,
	})

	// Harvest exec: stream mode, emits a tar archive with the single file the
	// producer wrote, named with the leading '/' stripped (GNU/BusyBox tar).
	const fileBody = "apiVersion: v1\nkind: Config\n"
	var harvestTar bytes.Buffer
	tw := tar.NewWriter(&harvestTar)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "home/agent/.kube/config", Mode: 0o600, Size: int64(len(fileBody)),
	}), "write tar header")
	_, _ = tw.Write([]byte(fileBody))
	require.NoError(t, tw.Close(), "close tar writer")

	fakeExec.ProgramStream("default/sess-file-pod:sandbox", fake.StreamResponse{
		Stdout:   harvestTar.Bytes(),
		ExitCode: 0,
	})

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-file", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-file", Tool: "echo",
			Timeout:        metav1.Duration{Duration: 10 * time.Second},
			CaptureOutputs: []string{"/home/agent/.kube/config"},
		},
	}
	mustCreate(t, env.Client, tc)

	var final spiceboxv1alpha1.ToolCall
	eventually(t, 15*time.Second, func() bool {
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &final); err != nil {
			return false
		}
		return hasTrueCondition(&final, spiceboxv1alpha1.ToolCallConditionSucceeded)
	})

	require.Len(t, final.Status.OutputArtifacts, 1,
		"OutputArtifacts: %+v", final.Status.OutputArtifacts)
	oa := final.Status.OutputArtifacts[0]
	assert.Equal(t, "home/agent/.kube/config", oa.Path, "OutputArtifact.Path (slash-stripped)")
	assert.Equal(t, int64(len(fileBody)), oa.Size, "OutputArtifact.Size")

	rc, err := store.Get(context.Background(), artifactstore.Ref(oa.ArtifactRef))
	require.NoError(t, err, "store.Get harvested file artifact")
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	assert.Equal(t, fileBody, string(body), "stored file artifact body must match")
}

// TestToolCall_CapturesFileFromRecordPaddedTar is the regression guard for the
// harvest stall that made "file:" secret outputs look "not produced". Real
// busybox/GNU tar pads the whole archive to a full record (20*512 = 10240 bytes)
// with zero blocks AFTER the end-of-archive marker; Go's archive/tar, used by
// the other harvest tests, does not. tar.Reader stops at the marker and never
// consumes that padding, so the harvest loop broke with bytes still buffered in
// the pod's stdout — which the executor relays over a SYNCHRONOUS io.Pipe. The
// executor goroutine then blocked forever writing the unread padding, its
// exit-code channel never fired, and Wait() hung to the ctx deadline. The tool
// exited 0 and the file was captured, but the caller discards harvested
// artifacts on any harvest error, so OutputArtifacts came back empty.
// This test emulates the real record padding; it fails (empty OutputArtifacts /
// timeout) without the trailing-stdout drain in harvestOutputs.
func TestToolCall_CapturesFileFromRecordPaddedTar(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	createReadyClassAndSession(t, env.Client, "cls-pad", "sess-pad")

	fakeExec.Program("default/sess-pad-pod:sandbox", fake.Response{ExitCode: 0})

	const fileBody = "apiVersion: v1\nkind: Config\n"
	var harvestTar bytes.Buffer
	tw := tar.NewWriter(&harvestTar)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "out/kubeconfig", Mode: 0o600, Size: int64(len(fileBody)),
	}), "write tar header")
	_, _ = tw.Write([]byte(fileBody))
	require.NoError(t, tw.Close(), "close tar writer")

	// Pad the archive to a full 10240-byte record with zero blocks, exactly as
	// busybox/GNU tar does when writing to stdout. tar.Reader will not consume
	// this residue — the harvest drain must.
	const recordSize = 10240
	if pad := recordSize - harvestTar.Len()%recordSize; pad < recordSize {
		harvestTar.Write(make([]byte, pad))
	}
	require.Zero(t, harvestTar.Len()%recordSize, "test setup: padded archive must be record-aligned")

	fakeExec.ProgramStream("default/sess-pad-pod:sandbox", fake.StreamResponse{
		Stdout:   harvestTar.Bytes(),
		ExitCode: 0,
	})

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-pad", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-pad", Tool: "echo",
			Timeout:        metav1.Duration{Duration: 10 * time.Second},
			CaptureOutputs: []string{"/out/kubeconfig"},
		},
	}
	mustCreate(t, env.Client, tc)

	var final spiceboxv1alpha1.ToolCall
	eventually(t, 15*time.Second, func() bool {
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &final); err != nil {
			return false
		}
		return hasTrueCondition(&final, spiceboxv1alpha1.ToolCallConditionSucceeded)
	})

	require.Len(t, final.Status.OutputArtifacts, 1,
		"record-padded tar must still yield the captured file (harvest must not stall on trailing padding); OutputArtifacts: %+v",
		final.Status.OutputArtifacts)
	oa := final.Status.OutputArtifacts[0]
	assert.Equal(t, "out/kubeconfig", oa.Path, "OutputArtifact.Path (slash-stripped)")
	assert.Equal(t, int64(len(fileBody)), oa.Size, "OutputArtifact.Size")

	rc, err := store.Get(context.Background(), artifactstore.Ref(oa.ArtifactRef))
	require.NoError(t, err, "store.Get harvested file artifact")
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	assert.Equal(t, fileBody, string(body), "stored file artifact body must match through the padding")
}

func TestToolCall_HydratesInputArtifacts(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	createReadyClassAndSession(t, env.Client, "cls-hi", "sess-hi")

	// Preload an artifact.
	ref, err := store.Put(context.Background(), "default/sess-hi/preseed/script.sh",
		strings.NewReader("echo hello from script"))
	require.NoError(t, err, "store.Put preload script")

	// Capture the hydration stdin so we can verify tar contents.
	hydrateKey := "default/sess-hi-pod:sandbox"
	hydrateStdin := make(chan []byte, 1)
	fakeExec.ProgramStream(hydrateKey, fake.StreamResponse{
		ExitCode: 0,
		OnStdin:  func(b []byte) { hydrateStdin <- b },
	})
	// The main exec (after hydration) uses the sync path.
	fakeExec.Program(hydrateKey, fake.Response{ExitCode: 0, Stdout: []byte("ran")})

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-hi", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-hi", Tool: "echo",
			Timeout: metav1.Duration{Duration: 10 * time.Second},
			InputArtifacts: []spiceboxv1alpha1.InputArtifact{
				{Path: "script.sh", ArtifactRef: string(ref)},
			},
		},
	}
	mustCreate(t, env.Client, tc)

	// End-to-end: hydrate then exec → Succeeded.
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got); err != nil {
			return false
		}
		return hasTrueCondition(&got, spiceboxv1alpha1.ToolCallConditionSucceeded)
	})

	// Verify a tar stream was written to hydrate stdin.
	select {
	case b := <-hydrateStdin:
		tr := tar.NewReader(bytes.NewReader(b))
		hdr, err := tr.Next()
		require.NoError(t, err, "tar.Next on hydrate stdin")
		assert.Equal(t, "script.sh", hdr.Name, "tar entry name")
		body, _ := io.ReadAll(tr)
		assert.Equal(t, "echo hello from script", string(body), "tar entry body")
	case <-time.After(5 * time.Second):
		t.Fatal("hydrate stdin never observed")
	}
}

func TestToolCall_StreamModePopulatesStreamingStatus(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()

	// Custom manager setup because ToolCall needs Registry + GatewayEndpoint.
	registry := gateway.NewRegistry()
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:  env.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: ctrlconfig.Controller{
			SkipNameValidation: ptr.To(true),
		},
	})
	require.NoError(t, err, "NewManager")
	require.NoError(t,
		(&spiceboxclass.Reconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr),
		"class setup")
	runtimes := testenv.SandboxRuntimesWithExec(t, mgr.GetClient(), fakeExec.For)
	require.NoError(t,
		(&spiceboxsession.Reconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
			Runtimes: runtimes,
		}).SetupWithManager(mgr),
		"session setup")
	toolkitReg, err := toolspecregistry.NewWithBuiltins(mgr.GetClient())
	require.NoError(t, err, "toolspec registry")
	require.NoError(t,
		(&spiceboxtoolkit.Reconciler{Client: mgr.GetClient(), Registry: toolkitReg}).SetupWithManager(mgr),
		"toolkit setup")
	require.NoError(t,
		(&spiceboxtoolspec.Reconciler{Client: mgr.GetClient(), Registry: toolkitReg}).SetupWithManager(mgr),
		"toolspec setup")
	require.NoError(t, (&toolcall.Reconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Runtimes:        runtimes,
		Store:           store,
		Registry:        registry,
		GatewayEndpoint: "test-gateway:8443",
		ToolkitRegistry: toolkitReg,
		APIReader:       mgr.GetAPIReader(),
	}).SetupWithManager(mgr), "toolcall setup")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()

	createReadyClassAndSession(t, env.Client, "cls-str", "sess-str")

	fakeExec.ProgramStream("default/sess-str-pod:sandbox", fake.StreamResponse{
		Stdout:   []byte("stream-data"),
		ExitCode: 0,
	})

	// The runner commits the stream token by hash: it generates the raw token,
	// keeps it in memory, and stamps only spec.streamTokenHash. Mirror that here.
	const streamRawToken = "stream-test-raw-token"
	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-str", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-str", Tool: "echo",
			Mode:            spiceboxv1alpha1.ToolCallModeStream,
			Timeout:         metav1.Duration{Duration: 10 * time.Second},
			StreamTokenHash: streamTokenHashHex(streamRawToken),
		},
	}
	mustCreate(t, env.Client, tc)

	// Wait for status.streaming to populate (reconciler registered stream + set Running).
	var streamingReady spiceboxv1alpha1.ToolCall
	eventually(t, 10*time.Second, func() bool {
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &streamingReady); err != nil {
			return false
		}
		return streamingReady.Status.Streaming != nil &&
			streamingReady.Status.Streaming.Available &&
			streamingReady.Status.Streaming.GatewayEndpoint == "test-gateway:8443"
	})

	// Act as a gateway client: claim the stream from the registry with the RAW
	// token (the gateway hashes it server-side). Close stdin to signal EOF so
	// the fake's producer goroutine can proceed past io.ReadAll, then drain
	// stdout/stderr so the pipe writers don't block.
	active, _, err := registry.Claim("default", tc.Name, streamRawToken)
	require.NoError(t, err, "registry.Claim")
	_ = active.Stream.Stdin.Close()
	go func() { _, _ = io.Copy(io.Discard, active.Stream.Stdout) }()
	go func() { _, _ = io.Copy(io.Discard, active.Stream.Stderr) }()

	// The watcher goroutine calls stream.Wait() and drives terminal status once
	// the producer completes (unblocked by the drain goroutines above).
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got); err != nil {
			return false
		}
		return hasTrueCondition(&got, spiceboxv1alpha1.ToolCallConditionSucceeded)
	})

	// Cleanup the stream so the fake's done channel closes cleanly.
	_ = active.Stream.Close()
}

func TestToolCall_MergesSessionDefaultEnv(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	createReadyClassAndSession(t, env.Client, "cls-de", "sess-de")

	// Stamp DefaultEnv on the SpiceboxSession after the helper readied it.
	ctx := context.Background()
	var sbox spiceboxv1alpha1.SpiceboxSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sess-de"}, &sbox))
	sbox.Spec.DefaultEnv = map[string]string{
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"GIT_TERMINAL_PROMPT": "0",
		"OVERRIDDEN":          "from-default-env",
	}
	require.NoError(t, env.Client.Update(ctx, &sbox), "set DefaultEnv on SpiceboxSession")

	fakeExec.Program("default/sess-de-pod:sandbox", fake.Response{ExitCode: 0})

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-defaultenv", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-de",
			Tool:    "echo",
			Timeout: metav1.Duration{Duration: 10 * time.Second},
			Env:     map[string]string{"OVERRIDDEN": "from-toolcall-env"},
		},
	}
	mustCreate(t, env.Client, tc)

	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(tc), &got); err != nil {
			return false
		}
		return hasTrueCondition(&got, spiceboxv1alpha1.ToolCallConditionSucceeded)
	})

	// Locate this specific call by pod target — Call carries Namespace/Pod/
	// Container alongside Request because a Request itself carries no sandbox
	// identity (the executor is already bound to one target). The exec
	// request's Env carries our merged env directly, so just find a call
	// against this pod and check Env.
	var got execpkg.Request
	var found bool
	for _, c := range fakeExec.Calls() {
		if c.Pod == "sess-de-pod" {
			got = c.Request
			found = true
			break
		}
	}
	require.True(t, found, "expected at least one exec call against sess-de-pod")
	assert.Equal(t, "/dev/null", got.Env["GIT_CONFIG_GLOBAL"],
		"session DefaultEnv applied (GIT_CONFIG_GLOBAL)")
	assert.Equal(t, "0", got.Env["GIT_TERMINAL_PROMPT"],
		"session DefaultEnv applied (GIT_TERMINAL_PROMPT)")
	assert.Equal(t, "from-toolcall-env", got.Env["OVERRIDDEN"],
		"ToolCall.Spec.Env overrides DefaultEnv for the same key")
}

// startInteractiveManager mirrors the Stream test's custom manager setup so
// the interactive-mode tests can hold a reference to the gateway.Registry.
func startInteractiveManager(t *testing.T, env *testenv.Env, fakeExec *fake.Binder, store artifactstore.Store, registry *gateway.Registry) {
	t.Helper()
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:     env.Scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	require.NoError(t, err, "NewManager")
	require.NoError(t,
		(&spiceboxclass.Reconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr),
		"class setup")
	runtimes := testenv.SandboxRuntimesWithExec(t, mgr.GetClient(), fakeExec.For)
	require.NoError(t,
		(&spiceboxsession.Reconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
			Runtimes: runtimes,
		}).SetupWithManager(mgr),
		"session setup")
	toolkitReg, err := toolspecregistry.NewWithBuiltins(mgr.GetClient())
	require.NoError(t, err, "toolspec registry")
	require.NoError(t,
		(&spiceboxtoolkit.Reconciler{Client: mgr.GetClient(), Registry: toolkitReg}).SetupWithManager(mgr),
		"toolkit setup")
	require.NoError(t,
		(&spiceboxtoolspec.Reconciler{Client: mgr.GetClient(), Registry: toolkitReg}).SetupWithManager(mgr),
		"toolspec setup")
	require.NoError(t, (&toolcall.Reconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Runtimes:        runtimes,
		Store:           store,
		Registry:        registry,
		GatewayEndpoint: "test-gateway:8443",
		ToolkitRegistry: toolkitReg,
		APIReader:       mgr.GetAPIReader(),
	}).SetupWithManager(mgr), "toolcall setup")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
}

func TestToolCall_InteractiveMode_RegistersStreamAndStatus(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	registry := gateway.NewRegistry()
	startInteractiveManager(t, env, fakeExec, store, registry)

	createReadyClassAndSession(t, env.Client, "cls-int", "sess-int")

	fakeExec.ProgramStream("default/sess-int-pod:sandbox", fake.StreamResponse{
		Stdout:   []byte("hello\n"),
		ExitCode: 0,
	})

	const interactiveRawToken = "interactive-test-raw-token"
	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-int", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session:         "sess-int",
			Tool:            "echo",
			Mode:            spiceboxv1alpha1.ToolCallModeInteractive,
			MaxDuration:     metav1.Duration{Duration: time.Hour},
			StreamTokenHash: streamTokenHashHex(interactiveRawToken),
		},
	}
	mustCreate(t, env.Client, tc)

	var ready spiceboxv1alpha1.ToolCall
	eventually(t, 10*time.Second, func() bool {
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &ready); err != nil {
			return false
		}
		return ready.Status.Streaming != nil && ready.Status.Streaming.Available
	})
	assert.Equal(t, "test-gateway:8443", ready.Status.Streaming.GatewayEndpoint)
	assert.True(t, hasTrueCondition(&ready, spiceboxv1alpha1.ToolCallConditionRunning))

	// Drain the fake stream so cleanup proceeds — claim with the RAW token.
	active, _, err := registry.Claim("default", tc.Name, interactiveRawToken)
	require.NoError(t, err, "claim")
	_ = active.Stream.Stdin.Close()
	go func() { _, _ = io.Copy(io.Discard, active.Stream.Stdout) }()
	go func() { _, _ = io.Copy(io.Discard, active.Stream.Stderr) }()
}

func TestToolCall_InteractiveDeletion_CancelsExec_AndStampsCanceled(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	registry := gateway.NewRegistry()
	startInteractiveManager(t, env, fakeExec, store, registry)

	createReadyClassAndSession(t, env.Client, "cls-del", "sess-del")

	// Program a streaming response that blocks until the exec context is
	// cancelled — fake.StreamResponse drains stdin in a goroutine before
	// emitting stdout, so the simplest "stuck" approximation is leaving
	// stdin open and never delivering output until ctx fires.
	fakeExec.ProgramStream("default/sess-del-pod:sandbox", fake.StreamResponse{
		Stdout:   []byte{}, // no output until stdin closes
		ExitCode: 0,
	})

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-del", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session:         "sess-del",
			Tool:            "echo",
			Mode:            spiceboxv1alpha1.ToolCallModeInteractive,
			MaxDuration:     metav1.Duration{Duration: time.Hour},
			StreamTokenHash: streamTokenHashHex("delete-test-raw-token"),
		},
	}
	mustCreate(t, env.Client, tc)

	// Wait until the stream is registered.
	var ready spiceboxv1alpha1.ToolCall
	eventually(t, 10*time.Second, func() bool {
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &ready); err != nil {
			return false
		}
		return ready.Status.Streaming != nil && ready.Status.Streaming.Available
	})

	// Claim first, as production always does: the sandbox client claims the
	// stream before the user can delete anything. Deleting an UNCLAIMED stream
	// exercises an ordering that cannot occur, and it is the ordering under
	// which the deletion path was accidentally correct — a claim used to remove
	// the entry outright, so teardown found nothing to cancel and the exec ran
	// on with no client. Claiming here is what makes this a regression test.
	_, _, claimErr := registry.Claim("default", tc.Name, "delete-test-raw-token")
	require.NoError(t, claimErr, "claim the stream before deleting, as the sandbox client does")

	// Delete the ToolCall. The controller's deletion branch should:
	//   - call Registry.CancelAndUnregister (cancels exec ctx → watchStreamCompletion finalizes)
	//   - stamp Canceled=True with ReasonUserDelete
	require.NoError(t, env.Client.Delete(context.Background(), tc), "Delete ToolCall")

	// Verify the ToolCall ends up either fully deleted or with Canceled=True.
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got); err != nil {
			return apierrors.IsNotFound(err) // gone after finalizer removed
		}
		return hasTrueCondition(&got, spiceboxv1alpha1.ToolCallConditionCanceled)
	})
}

// streamTokenHashHex mirrors the runner's stream-token commitment:
// hex(sha256(rawToken)). The operator registers the gateway stream with this
// hash; the gateway hashes the raw token a client presents and compares.
func streamTokenHashHex(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}

// TestToolCall_StreamingMode_RejectsMissingStreamTokenHash asserts a streaming
// ToolCall created WITHOUT spec.streamTokenHash is rejected: the controller
// stamps Failed=True with ReasonStreamTokenHashMissing rather than registering
// a gateway stream. Without the hash the gateway has nothing to authenticate a
// presented token against, so the call must not proceed.
func TestToolCall_StreamingMode_RejectsMissingStreamTokenHash(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	registry := gateway.NewRegistry()
	startInteractiveManager(t, env, fakeExec, store, registry)

	createReadyClassAndSession(t, env.Client, "cls-nohash", "sess-nohash")

	fakeExec.ProgramStream("default/sess-nohash-pod:sandbox", fake.StreamResponse{
		Stdout:   []byte("never-runs"),
		ExitCode: 0,
	})

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-nohash", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-nohash",
			Tool:    "echo",
			Mode:    spiceboxv1alpha1.ToolCallModeStream,
			Timeout: metav1.Duration{Duration: 10 * time.Second},
			// StreamTokenHash deliberately omitted.
		},
	}
	mustCreate(t, env.Client, tc)

	var got spiceboxv1alpha1.ToolCall
	eventually(t, 10*time.Second, func() bool {
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got); err != nil {
			return false
		}
		return hasTrueCondition(&got, spiceboxv1alpha1.ToolCallConditionFailed)
	})

	failed := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ToolCallConditionFailed)
	require.NotNil(t, failed, "Failed condition present")
	assert.Equal(t, spiceboxv1alpha1.ReasonStreamTokenHashMissing, failed.Reason,
		"rejection reason is StreamTokenHashMissing")
	assert.Nil(t, got.Status.Streaming,
		"no streaming endpoint written when the hash is missing")
}
