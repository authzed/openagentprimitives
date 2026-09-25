package installcmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// newReporter builds the non-TTY (streaming) progress reporter over a buffer, so
// preflight/confirmPlan output is captured as plain text for assertions.
func newReporter(t *testing.T, buf *bytes.Buffer) progress.Reporter {
	t.Helper()
	return progress.New(buf, strings.NewReader(""), true /*assumeYes*/)
}

// unreachableClientset returns a fake clientset whose Discovery().ServerVersion()
// fails — the stand-in for a cluster oap cannot reach.
func unreachableClientset(t *testing.T) kubernetes.Interface {
	t.Helper()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "version", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("dial tcp 127.0.0.1:6443: connect: connection refused")
	})
	return cs
}

// stubDockerLookPath swaps the package-level docker resolver for the duration of
// a test so preflight's build-tool check can be driven without a real docker
// install. found=false simulates a missing docker CLI.
func stubDockerLookPath(t *testing.T, found bool) {
	t.Helper()
	prev := dockerLookPath
	t.Cleanup(func() { dockerLookPath = prev })
	dockerLookPath = func(string) (string, error) {
		if found {
			return "/usr/local/bin/docker", nil
		}
		return "", errors.New("exec: \"docker\": executable file not found in $PATH")
	}
}

func TestPreflight(t *testing.T) {
	cases := []struct {
		name        string
		typed       kubernetes.Interface
		strat       cloud.Strategy
		building    bool
		dockerFound bool
		wantErr     bool
		wantText    []string
		wantNotText []string
	}{
		{
			name:        "reachable + local: cluster reachable, local-style note, no error",
			typed:       fake.NewSimpleClientset(),
			strat:       cloud.MustFor(cloud.KeyLocal),
			dockerFound: true,
			wantErr:     false,
			// "(local-style install)" pins the
			// strat.InstallProfile().UsesLocalDevImages() branch specifically — a
			// regression here (e.g. comparing strat.Key() against cloud.KeyDefault
			// again, as it did before the local/unmanaged split) would either never
			// fire for a real --local install, or fire for every on-prem/unmanaged
			// cluster instead. This case pins the former failure mode.
			wantText: []string{"cluster reachable", "local development cluster", "(local-style install)"},
		},
		{
			name:        "reachable + default (unmanaged): cluster reachable, cluster note, NO local-style note",
			typed:       fake.NewSimpleClientset(),
			strat:       cloud.MustFor(cloud.KeyDefault),
			dockerFound: true,
			wantErr:     false,
			// Pins the latter failure mode of the same regression: an on-prem/
			// unmanaged cluster (KeyDefault, the production profile) must never be
			// mislabeled a "local-style install".
			wantText:    []string{"cluster reachable", "unmanaged cluster (on-prem, bare-metal, or undetected) cluster"},
			wantNotText: []string{"local-style install"},
		},
		{
			name:        "reachable + gke: detected GKE cluster, no error",
			typed:       fake.NewSimpleClientset(),
			strat:       cloud.MustFor(cloud.KeyGKE),
			dockerFound: true,
			wantErr:     false,
			wantText:    []string{"cluster reachable", "detected: GKE cluster"},
		},
		{
			name:        "unreachable cluster: returns error + plain kubectl fix",
			typed:       unreachableClientset(t),
			strat:       cloud.MustFor(cloud.KeyDefault),
			dockerFound: true,
			wantErr:     true,
			wantText:    []string{"can't reach a Kubernetes cluster", "kubectl cluster-info"},
		},
		{
			name:        "building + docker present: docker check passes, no error",
			typed:       fake.NewSimpleClientset(),
			strat:       cloud.MustFor(cloud.KeyDefault),
			building:    true,
			dockerFound: true,
			wantErr:     false,
			wantText:    []string{"cluster reachable", "docker present"},
		},
		{
			name:        "building + docker missing: returns error + install-docker fix",
			typed:       fake.NewSimpleClientset(),
			strat:       cloud.MustFor(cloud.KeyDefault),
			building:    true,
			dockerFound: false,
			wantErr:     true,
			wantText:    []string{"Docker isn't installed", "get-docker"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NotNil(t, tc.strat, "cloud strategy must be registered for this test")
			stubDockerLookPath(t, tc.dockerFound)
			var buf bytes.Buffer
			err := preflight(context.Background(), &kube.Bundle{Typed: tc.typed}, tc.strat, newReporter(t, &buf), tc.building)
			if tc.wantErr {
				require.Error(t, err, "preflight must abort on a hard finding")
			} else {
				require.NoError(t, err, "preflight must pass when all checks are green")
			}
			out := buf.String()
			for _, w := range tc.wantText {
				assert.Containsf(t, out, w, "preflight output missing %q", w)
			}
			for _, w := range tc.wantNotText {
				assert.NotContainsf(t, out, w, "preflight output must not contain %q", w)
			}
		})
	}
}

func TestConfirmPlanProceedSemantics(t *testing.T) {
	comps := buildCoreComponents(&kube.Bundle{}, manifests.Tags{}, "", cloud.MustFor(cloud.KeyDefault) /*not local*/, ExternalSpiceDB{} /*not external*/)

	cases := []struct {
		name        string
		assumeYes   bool
		isTTY       bool
		stdin       string
		r           WebdRoutingOpts
		wantProceed bool
		wantPrompt  bool // whether the "Proceed with the install?" prompt is printed
		wantText    []string
	}{
		{
			name:        "interactive decline: proceed=false, summary + prompt shown",
			isTTY:       true,
			stdin:       "n\n",
			wantProceed: false,
			wantPrompt:  true,
			wantText:    []string{"PostgreSQL — agent memory store", "Estimated time: ~2–5 minutes."},
		},
		{
			name:        "interactive accept: proceed=true, prompt shown",
			isTTY:       true,
			stdin:       "y\n",
			wantProceed: true,
			wantPrompt:  true,
		},
		{
			name:        "--yes: proceed=true, no prompt (automation not blocked)",
			assumeYes:   true,
			isTTY:       true,
			wantProceed: true,
			wantPrompt:  false,
		},
		{
			name:        "non-TTY: proceed=true, no prompt (automation not blocked)",
			isTTY:       false,
			wantProceed: true,
			wantPrompt:  false,
		},
		{
			name:        "external access: HTTPS origin + load-balancer time estimate",
			assumeYes:   true,
			isTTY:       true,
			r:           WebdRoutingOpts{trustedHostname: "webd.example.com"},
			wantProceed: true,
			wantText: []string{
				"Secure HTTPS at https://webd.example.com",
				"the cloud load balancer is the slow part",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			rep := newReporter(t, &buf)
			proceed, err := confirmPlan(comps, tc.r, rep, strings.NewReader(tc.stdin), &buf, tc.assumeYes, tc.isTTY)
			require.NoError(t, err, "confirmPlan does no cluster work and must not error")
			assert.Equal(t, tc.wantProceed, proceed)

			out := buf.String()
			assert.Contains(t, out, "Here's what oap will set up:", "the plan summary is always rendered")
			assert.Contains(t, out, "Estimated time:", "a time estimate is always rendered")
			for _, w := range tc.wantText {
				assert.Containsf(t, out, w, "plan summary missing %q", w)
			}
			if tc.wantPrompt {
				assert.Contains(t, out, "Proceed with the install?", "an interactive run prompts")
			} else {
				assert.NotContains(t, out, "Proceed with the install?", "--yes / non-TTY must not prompt")
			}
		})
	}
}

// blockingReader fails the test if it is ever read. confirmPlan under
// preconfirmed must reach its proceed decision without consulting stdin at all —
// the go/no-go was already answered by the wizard's own Proceed step.
type blockingReader struct{ t *testing.T }

func (b blockingReader) Read([]byte) (int, error) {
	b.t.Error("confirmPlan read stdin despite preconfirmed — the go/no-go prompt was not suppressed")
	return 0, io.EOF
}

// TestConfirmPlanPreconfirmedSkipsPrompt pins 11a: when the caller already got a
// go/no-go (the init wizard's Proceed step sets r.preconfirmed), confirmPlan
// proceeds WITHOUT prompting, even on an interactive TTY with --yes unset — the
// one combination that would otherwise ask. It must neither print the prompt nor
// touch stdin (driven here by a reader that errors the test if read).
func TestConfirmPlanPreconfirmedSkipsPrompt(t *testing.T) {
	comps := buildCoreComponents(&kube.Bundle{}, manifests.Tags{}, "", cloud.MustFor(cloud.KeyDefault), ExternalSpiceDB{})

	var buf bytes.Buffer
	rep := newReporter(t, &buf)
	proceed, err := confirmPlan(comps, WebdRoutingOpts{preconfirmed: true}, rep, blockingReader{t: t}, &buf, false /*assumeYes*/, true /*isTTY*/)
	require.NoError(t, err, "confirmPlan does no cluster work and must not error")
	assert.True(t, proceed, "a preconfirmed plan proceeds")
	assert.NotContains(t, buf.String(), "Proceed with the install?",
		"preconfirmed must not re-ask the go/no-go the wizard already answered")
}

// TestConfirmPlanFriendlyNames asserts every core component is rendered with its
// plain-language description (the Name→friendly mapping is complete for the real
// buildCoreComponents output).
func TestConfirmPlanFriendlyNames(t *testing.T) {
	comps := buildCoreComponents(&kube.Bundle{}, manifests.Tags{}, "", cloud.MustFor(cloud.KeyDefault) /*not local*/, ExternalSpiceDB{} /*not external*/)

	var buf bytes.Buffer
	rep := newReporter(t, &buf)
	proceed, err := confirmPlan(comps, WebdRoutingOpts{}, rep, strings.NewReader(""), &buf, true /*assumeYes*/, true /*isTTY*/)
	require.NoError(t, err, "confirmPlan does no cluster work and must not error")
	require.True(t, proceed, "--yes must proceed without prompting")

	out := buf.String()
	for name, desc := range componentPlanDescriptions {
		assert.Containsf(t, out, desc, "plan summary must describe %q as %q", name, desc)
	}
	assert.NotContains(t, out, "Proceed with the install?", "--yes must not print the prompt")
}

func TestUnwedgeLeftoverNamespace(t *testing.T) {
	const ns = "agentprimitives-system"

	t.Run("namespace absent: nil (nothing to do)", func(t *testing.T) {
		var buf bytes.Buffer
		b := &kube.Bundle{Typed: fake.NewSimpleClientset(), Dynamic: dynfake.NewSimpleDynamicClient(scheme.Scheme)}
		err := unwedgeLeftoverNamespace(context.Background(), b, fakeUnwedgeStrategy{}, newReporter(t, &buf), ns)
		require.NoError(t, err)
	})

	t.Run("namespace active (no deletionTimestamp): nil", func(t *testing.T) {
		var buf bytes.Buffer
		b := &kube.Bundle{
			Typed:   fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}),
			Dynamic: dynfake.NewSimpleDynamicClient(scheme.Scheme),
		}
		err := unwedgeLeftoverNamespace(context.Background(), b, fakeUnwedgeStrategy{}, newReporter(t, &buf), ns)
		require.NoError(t, err)
	})

	t.Run("namespace terminating, unwedge clears it: nil", func(t *testing.T) {
		var buf bytes.Buffer
		term := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		now := metav1.Now()
		term.DeletionTimestamp = &now
		term.Finalizers = []string{"kubernetes"}
		typed := fake.NewSimpleClientset(term)
		// After unwedge "clears" it, the namespace should be reported gone. A
		// counter-based reactor lets the first Get (the initial existence check)
		// return the terminating namespace normally; all subsequent Gets (the
		// bounded-wait poll loop) return NotFound, simulating the namespace
		// disappearing after the unwedge call.
		var gets int
		typed.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
			gets++
			if gets == 1 {
				return false, nil, nil // let tracker return the terminating namespace
			}
			return true, nil, apierrors.NewNotFound(corev1.Resource("namespaces"), ns)
		})
		b := &kube.Bundle{Typed: typed, Dynamic: dynfake.NewSimpleDynamicClient(scheme.Scheme)}
		err := unwedgeLeftoverNamespace(context.Background(), b, fakeUnwedgeStrategy{report: cloud.UnwedgeReport{FinalizersCleared: []string{"neg-obj"}}}, newReporter(t, &buf), ns)
		require.NoError(t, err)
	})

	t.Run("namespace terminating, cannot clear: error mentions the namespace", func(t *testing.T) {
		// Shorten the poll window so this test does not wait the full 30 s.
		prev := unwedgeNamespaceWait
		unwedgeNamespaceWait = 50 * time.Millisecond
		t.Cleanup(func() { unwedgeNamespaceWait = prev })

		var buf bytes.Buffer
		term := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		now := metav1.Now()
		term.DeletionTimestamp = &now
		b := &kube.Bundle{Typed: fake.NewSimpleClientset(term), Dynamic: dynfake.NewSimpleDynamicClient(scheme.Scheme)}
		// Strategy returns Blocked + no cleared finalizers; the namespace Get keeps
		// returning the terminating namespace, so the bounded wait fails.
		const manualCmd = `kubectl patch svcneg neg-obj -n agentprimitives-system --type merge -p '{"metadata":{"finalizers":[]}}'`
		err := unwedgeLeftoverNamespace(context.Background(), b,
			fakeUnwedgeStrategy{report: cloud.UnwedgeReport{
				StuckFinalizers: []string{"networking.gke.io/neg-finalizer"},
				Blocked:         []string{"NEG still referenced"},
				ManualCommands:  []string{manualCmd},
			}},
			newReporter(t, &buf), ns)
		require.Error(t, err)
		assert.Contains(t, err.Error(), ns)
		assert.Contains(t, err.Error(), manualCmd, "error must surface the manual fix command")
	})
}
