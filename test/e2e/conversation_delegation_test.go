//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// delegationTestScheme is the minimal scheme singleSession/FindOnlyAgentSession
// need: just the AgentSession CRD. No Channel/Secret machinery, because
// neither helper under test reads a Channel — they read AgentSession.Spec
// directly.
func delegationTestScheme(t *testing.T) *apiruntime.Scheme {
	t.Helper()
	s := apiruntime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "spiceboxv1alpha1.AddToScheme")
	return s
}

// rootSession is a channel-attached, non-delegated AgentSession — the shape
// SessionRef/checkGoldenTrace/checkAuthz actually want.
func rootSession(ns, name string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Kind: "fake", Key: "default-thread", Name: name + "-channel",
			},
		},
	}
}

// attendedChild is shaped exactly the way
// pkg/controllers/subagentrequest's buildChild + attendedChildBinding build
// one: Spec.Parent set to the root, and an InputChannel binding that copies
// the root's own Kind/Key verbatim (attended's whole point is that the child
// answers on the SAME surface the person is already reading). By kind and key
// alone it is indistinguishable from a second, independent top-level session
// bound to that root's own channel — only Spec.Parent tells them apart.
func attendedChild(ns, childName, rootName string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: childName},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Parent: &spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: rootName},
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Kind: "fake", Key: "default-thread", Name: rootName + "-channel",
			},
		},
	}
}

// TestSingleSession_SkipsAttendedChild proves requirement 1 of Task 3: an
// attended-shaped child (Spec.Parent set, root's own binding copied) must be
// SKIPPED by singleSession, resolving to the root alone rather than tripping
// the ">1 conversational session" fatal. Before the Spec.Parent-based filter,
// this fatal'd — see the report's captured failure output for the exact
// message with the old kind-based reachesAnotherSession check restored.
func TestSingleSession_SkipsAttendedChild(t *testing.T) {
	scheme := delegationTestScheme(t)
	root := rootSession("default", "root-session")
	child := attendedChild("default", "root-session-attended-child", "root-session")
	fakeCli := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(root, child).Build()

	h := &Harness{t: t, K8s: fakeCli}
	ns, name := h.singleSession("TestSingleSession_SkipsAttendedChild")
	assert.Equal(t, "default", ns)
	assert.Equal(t, "root-session", name, "must resolve to the root, not fatal on the attended child")
}

// singleSessionFatalHelperEnv gates TestSingleSessionFatalHelperProcess into
// actually driving singleSession. Mirrors
// internal/cmd/claudeshim/main_test.go's GO_WANT_HELPER_PROCESS convention
// (itself the standard library's own os/exec_test.go pattern): a real
// t.Fatalf call cannot be observed from INSIDE the same test binary run
// without failing it — Go's testing package marks every ancestor of a failed
// subtest failed too, there is no "catch and swallow" — so the call under
// test is driven in a freshly re-exec'd process scoped to just that one test
// function, and the parent process below inspects only that subprocess's
// exit code and output.
const singleSessionFatalHelperEnv = "E2E_SINGLESESSION_FATAL_HELPER"

// TestSingleSessionFatalHelperProcess is not a real test under a normal `go
// test` run: singleSessionFatalHelperEnv is unset, so it returns immediately
// and contributes nothing. runSingleSessionFatalHelper re-invokes the
// compiled test binary with that env var set and -test.run scoped to just
// this function, turning it into the actual
// singleSession-on-two-independent-sessions call under test for that one
// subprocess.
func TestSingleSessionFatalHelperProcess(t *testing.T) {
	if os.Getenv(singleSessionFatalHelperEnv) != "1" {
		return
	}
	scheme := delegationTestScheme(t)
	first := rootSession("default", "first-session")
	second := rootSession("default", "second-session")
	fakeCli := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(first, second).Build()
	h := &Harness{t: t, K8s: fakeCli}
	h.singleSession("TestSingleSessionFatalHelperProcess")
}

// runSingleSessionFatalHelper spawns a fresh process re-invoking this test
// binary as TestSingleSessionFatalHelperProcess and returns its exit code and
// combined output.
func runSingleSessionFatalHelper(t *testing.T) (exitCode int, output string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSingleSessionFatalHelperProcess$")
	cmd.Env = append(os.Environ(), singleSessionFatalHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	require.ErrorAsf(t, err, &exitErr,
		"helper process must exit with a normal (possibly non-zero) exit code, not fail to start: %v (output=%s)", err, out)
	return exitErr.ExitCode(), string(out)
}

// TestSingleSession_StillFatalsOnGenuineSecondSession proves requirement 2 of
// Task 3: the fix must NOT become permissive. Two independent, non-delegated,
// channel-attached AgentSessions (neither carries Spec.Parent) is exactly the
// ambiguity the ">1" guard was built to catch, and it must still catch it —
// see runSingleSessionFatalHelper's doc for why the actual Fatalf call is
// driven from a subprocess rather than in-process.
func TestSingleSession_StillFatalsOnGenuineSecondSession(t *testing.T) {
	code, out := runSingleSessionFatalHelper(t)
	assert.NotEqual(t, 0, code,
		"singleSession must still fatal (nonzero exit) when two genuinely independent top-level sessions exist; output=%s", out)
	assert.Contains(t, out, "multiple channel-attached AgentSession CRs found",
		"fatal message must name the ambiguity singleSession's guard exists to catch")
}

// TestFindOnlyAgentSession_SkipsAttendedChild is
// TestSingleSession_SkipsAttendedChild's sibling for the other Step-2 site,
// FindOnlyAgentSession (test/e2e/testhelpers.go) — SessionRef's blast radius
// note calls out that a mistake in one of these helpers and not the other
// would leave a gap, so both get the direct positive proof. It resolves on
// the FIRST poll (exactly one non-delegated session present immediately), so
// this does not pay FindOnlyAgentSession's 10s deadline.
func TestFindOnlyAgentSession_SkipsAttendedChild(t *testing.T) {
	scheme := delegationTestScheme(t)
	root := rootSession("default", "root-session")
	child := attendedChild("default", "root-session-attended-child", "root-session")
	fakeCli := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(root, child).Build()

	h := &Harness{t: t, K8s: fakeCli}
	got := FindOnlyAgentSession(t, h, "default")
	require.NotNil(t, got)
	assert.Equal(t, "root-session", got.Name, "must resolve to the root, not fatal on the attended child")
}
