package cloud

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"
)

// fakeComponent returns a Component whose Present check uses the provided flag,
// and whose Manifests returns a single-document YAML Namespace if manifests is
// non-nil. ready is called after apply if provided.
func fakeComponent(name string, present bool, manifests func() ([][]byte, error), ready func(context.Context) error) Component {
	return Component{
		Name:      name,
		Why:       "test reason",
		Creates:   "test creates",
		ManualCmd: "kubectl apply -f test.yaml",
		Present:   func(context.Context) (bool, error) { return present, nil },
		Manifests: manifests,
		Ready:     ready,
	}
}

var namespaceYAML = []byte(`
apiVersion: v1
kind: Namespace
metadata:
  name: test-ns
`)

func TestEnsureComponent_AlreadyPresent(t *testing.T) {
	cl := Clients{
		Dynamic: fake.NewSimpleDynamicClient(scheme.Scheme),
	}
	comp := fakeComponent("test-comp", true, nil, nil)
	present, err := EnsureComponent(context.Background(), NopReporter{}, strings.NewReader(""), cl, comp, false)
	require.NoError(t, err)
	assert.True(t, present, "already-present component must return present=true")
}

func TestEnsureComponent_AssumeYes_Installs(t *testing.T) {
	cl := Clients{
		Dynamic: fake.NewSimpleDynamicClient(scheme.Scheme),
	}
	readyCalled := false
	comp := fakeComponent("test-comp", false,
		func() ([][]byte, error) { return [][]byte{namespaceYAML}, nil },
		func(context.Context) error { readyCalled = true; return nil },
	)
	present, err := EnsureComponent(context.Background(), NopReporter{}, strings.NewReader(""), cl, comp, true)
	require.NoError(t, err)
	assert.True(t, present, "assumeYes=true must install and return present=true")
	assert.True(t, readyCalled, "Ready must be called after apply")
}

func TestEnsureComponent_NonInteractive_Declines(t *testing.T) {
	cl := Clients{
		Dynamic: fake.NewSimpleDynamicClient(scheme.Scheme),
	}
	comp := fakeComponent("test-comp", false,
		func() ([][]byte, error) { return [][]byte{namespaceYAML}, nil },
		nil,
	)
	// NopReporter is non-interactive; assumeYes=false → should decline.
	present, err := EnsureComponent(context.Background(), NopReporter{}, strings.NewReader(""), cl, comp, false)
	require.NoError(t, err)
	assert.False(t, present, "non-interactive without assumeYes must decline and return present=false")
}

func TestEnsureComponent_PresentCheckError(t *testing.T) {
	cl := Clients{
		Dynamic: fake.NewSimpleDynamicClient(scheme.Scheme),
	}
	comp := Component{
		Name:    "bad-comp",
		Present: func(context.Context) (bool, error) { return false, errors.New("api error") },
	}
	_, err := EnsureComponent(context.Background(), NopReporter{}, strings.NewReader(""), cl, comp, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "check bad-comp")
}

func TestEnsureComponent_ManifestError(t *testing.T) {
	cl := Clients{
		Dynamic: fake.NewSimpleDynamicClient(scheme.Scheme),
	}
	comp := fakeComponent("bad-manifest", false,
		func() ([][]byte, error) { return nil, errors.New("embed fail") },
		nil,
	)
	_, err := EnsureComponent(context.Background(), NopReporter{}, strings.NewReader(""), cl, comp, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load bad-manifest manifests")
}

func TestEnsureComponent_ReadyError(t *testing.T) {
	cl := Clients{
		Dynamic: fake.NewSimpleDynamicClient(scheme.Scheme),
	}
	comp := fakeComponent("slow-comp", false,
		func() ([][]byte, error) { return [][]byte{namespaceYAML}, nil },
		func(context.Context) error { return errors.New("timeout") },
	)
	_, err := EnsureComponent(context.Background(), NopReporter{}, strings.NewReader(""), cl, comp, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "slow-comp did not become ready")
}

// TestComponentConfirm ports the confirm cases from cmd/oap/internal/installcmd/deps_test.go,
// adapted to the cloud.Confirm signature (uses Reporter.Interactive instead
// of a bare isTTY bool).
func TestComponentConfirm(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		assumeYes   bool
		interactive bool
		want        bool
	}{
		{"assume-yes short-circuits", "", true, false, true},
		{"non-interactive defaults no", "", false, false, false},
		{"interactive yes", "y\n", false, true, true},
		{"interactive Y", "Y\n", false, true, true},
		{"interactive no (empty)", "\n", false, true, false},
		{"interactive n", "n\n", false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := &stubReporter{interactive: tc.interactive}
			got := Confirm(strings.NewReader(tc.input), rep, "Install foo?", tc.assumeYes)
			assert.Equal(t, tc.want, got)
		})
	}
}

// stubReporter is a minimal Reporter for tests that need Interactive() control.
// It records whether Suspend was used and captures what was written through it.
type stubReporter struct {
	interactive bool
	steps       []string
	suspended   bool
	suspendOut  bytes.Buffer
	suspendIn   io.Reader // lent to fn as stdin; nil unless a test sets it
}

func (r *stubReporter) Step(format string, args ...any) {}
func (r *stubReporter) OK(format string, args ...any)   {}
func (r *stubReporter) Info(format string, args ...any) {}
func (r *stubReporter) Warn(format string, args ...any) {}
func (r *stubReporter) Interactive() bool               { return r.interactive }
func (r *stubReporter) Suspend(fn func(io.Writer, io.Reader)) {
	r.suspended = true
	fn(&r.suspendOut, r.suspendIn)
}

// TestConfirm_PromptOwnsTerminalViaSuspend is the regression for the garbled GKE
// consent prompt: Confirm must emit its prompt through Suspend (so a live
// checklist quiesces) and must NOT append a trailing newline — the cursor stays
// on the prompt line where the user types their answer.
func TestConfirm_PromptOwnsTerminalViaSuspend(t *testing.T) {
	rep := &stubReporter{interactive: true}
	got := Confirm(strings.NewReader("y\n"), rep, "Enable the GKE Gateway API now?", false)
	assert.True(t, got, "y answer must confirm")
	assert.True(t, rep.suspended, "Confirm must own the terminal via Suspend, not print over the checklist")
	prompt := rep.suspendOut.String()
	assert.Contains(t, prompt, "[y/N]:", "the y/N prompt is written through Suspend")
	assert.NotContains(t, prompt, "\n", "the prompt must not append a newline (cursor stays after [y/N]:)")
}

// TestDisableLeaderElectionDoc is the pkg/platform/cloud equivalent of
// TestDisableLeaderElection in cmd/oap/internal/installcmd/deps_test.go.
func TestDisableLeaderElectionDoc(t *testing.T) {
	deployWithArgs := func(args ...string) *unstructured.Unstructured {
		a := make([]any, len(args))
		for i, s := range args {
			a[i] = s
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"kind": "Deployment",
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"containers": []any{map[string]any{"name": "c", "args": a}},
			}}},
		}}
	}
	containerArgs := func(t *testing.T, d *unstructured.Unstructured) []string {
		t.Helper()
		cs, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
		raw, _ := cs[0].(map[string]any)["args"].([]any)
		out := make([]string, len(raw))
		for i, v := range raw {
			out[i], _ = v.(string)
		}
		return out
	}

	// a controller using --leader-election-namespace gets --leader-elect=false
	d := deployWithArgs("--v=2", "--leader-election-namespace=kube-system")
	require.NoError(t, disableLeaderElectionDoc(d))
	assert.Contains(t, containerArgs(t, d), "--leader-elect=false")

	// idempotent — re-running doesn't duplicate
	require.NoError(t, disableLeaderElectionDoc(d))
	n := 0
	for _, a := range containerArgs(t, d) {
		if a == "--leader-elect=false" {
			n++
		}
	}
	assert.Equal(t, 1, n, "no duplicate --leader-elect=false")

	// a container without leader-election is untouched
	d2 := deployWithArgs("--v=2")
	require.NoError(t, disableLeaderElectionDoc(d2))
	assert.NotContains(t, containerArgs(t, d2), "--leader-elect=false")

	// non-Deployment is a safe no-op
	require.NoError(t, disableLeaderElectionDoc(&unstructured.Unstructured{Object: map[string]any{"kind": "ConfigMap"}}))
}
