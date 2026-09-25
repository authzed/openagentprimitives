//go:build integration

package toolcall_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	"github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
)

// TestToolspec_Allow_AcceptedByPopulated covers the happy path: a class
// authorized to run echo, a ToolCall that runs echo, status surfaces acceptedBy.
func TestToolspec_Allow_AcceptedByPopulated(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	createReadyClassAndSession(t, env.Client, "cls-tsa", "sess-tsa")
	fakeExec.Program("default/sess-tsa-pod:sandbox", fake.Response{ExitCode: 0, Stdout: []byte("hi")})

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-tsa", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-tsa", Tool: "echo", Args: []string{"hi"},
			Timeout: metav1.Duration{Duration: 5 * time.Second},
		},
	}
	mustCreate(t, env.Client, tc)
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got); err != nil {
			return false
		}
		return hasTrueCondition(&got, spiceboxv1alpha1.ToolCallConditionSucceeded) &&
			got.Status.Toolspec != nil &&
			got.Status.Toolspec.AcceptedBy != ""
	})
}

// TestToolspec_FailClosed_NoSpecForTool — class has only cat coverage but the
// ToolCall asks for echo. Fails because no toolspec covers echo in this class.
func TestToolspec_FailClosed_NoSpecForTool(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	createReadyToolspec(t, env.Client, "cat-only", "cat", "2026-04-25")

	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-fc"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "python",
			Resources: makeResources(),
			Tools: []spiceboxv1alpha1.SpiceboxTool{
				{Name: "cat", Command: []string{"/bin/cat"}},
			},
			Toolspecs: []spiceboxv1alpha1.ToolspecRef{{Name: "cat-only"}},
		},
	}
	mustCreate(t, env.Client, cls)
	createReadySession(t, env.Client, "sess-fc", "cls-fc")

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-fc", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-fc", Tool: "echo", // not in this class
			Timeout: metav1.Duration{Duration: 5 * time.Second},
		},
	}
	mustCreate(t, env.Client, tc)
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got); err != nil {
			return false
		}
		return hasTrueCondition(&got, spiceboxv1alpha1.ToolCallConditionFailed)
	})
}

// TestToolspec_StreamMode_Denied — a stream-mode ToolCall denied by toolspec
// (parse failure from --bogus-flag) must set Failed=True with ReasonToolspecDenied
// and must NOT have status.streaming populated (validation gate runs before
// stream registration).
func TestToolspec_StreamMode_Denied(t *testing.T) {
	env := testenv.Shared(t)
	fakeExec := fake.New()
	store := blobstore.NewMem()
	startManagerWithExec(t, env, fakeExec, store)

	createReadyClassAndSession(t, env.Client, "cls-sm", "sess-sm")

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-sm", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-sm", Tool: "echo",
			Mode:    spiceboxv1alpha1.ToolCallModeStream,
			Args:    []string{"--bogus-flag"},
			Timeout: metav1.Duration{Duration: 5 * time.Second},
		},
	}
	mustCreate(t, env.Client, tc)
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.ToolCall
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tc), &got); err != nil {
			return false
		}
		return hasTrueConditionWithReason(&got, spiceboxv1alpha1.ToolCallConditionFailed,
			spiceboxv1alpha1.ReasonToolspecDenied) && got.Status.Streaming == nil
	})
}

// hasTrueConditionWithReason returns true when tc has the given condition type
// set to True with the given reason.
func hasTrueConditionWithReason(tc *spiceboxv1alpha1.ToolCall, condType, reason string) bool {
	for _, c := range tc.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionTrue && c.Reason == reason {
			return true
		}
	}
	return false
}

func makeResources() spiceboxv1alpha1.SpiceboxResources {
	return spiceboxv1alpha1.SpiceboxResources{
		CPU: resource.MustParse("100m"), Memory: resource.MustParse("128Mi"),
		EphemeralStorage: resource.MustParse("100Mi"), PidsLimit: 64,
	}
}

// createReadySession creates a SpiceboxSession bound to className and waits
// until the session is Ready=True.
func createReadySession(t *testing.T, cli client.Client, name, className string) {
	t.Helper()
	sess := newOwnedBundle(t, cli, name, className)
	mustCreate(t, cli, sess)

	// Wait until the session controller has created the pod and resolved the class.
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxSession
		if err := cli.Get(context.Background(), client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		return got.Status.ResolvedClass != nil
	})

	// envtest has no kubelet, mark the Pod ready manually so the session flips Ready=True.
	var pod corev1.Pod
	podKey := client.ObjectKey{Name: name + "-pod", Namespace: "default"}
	require.NoError(t, cli.Get(context.Background(), podKey, &pod), "get pod %s", podKey)
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
