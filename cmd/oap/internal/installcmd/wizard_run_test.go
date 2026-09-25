package installcmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/settingscmd"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// installCall records the inputs runInstallFn was invoked with, so the test can
// prove the orchestrator folded the DETECTED settings into RunInstall's inputs
// without a real cluster.
type installCall struct {
	called             bool
	r                  WebdRoutingOpts
	wsOpts             WorkspaceResolveOptions
	ext                ExternalSpiceDB
	strat              cloud.Strategy
	tags               manifests.Tags
	railNonNil         bool
	installHasDeadline bool // ctx is timeout-bounded (R1)
	recheckHasDeadline bool // recheckCtx must NOT be (SIGINT only, no timeout)
}

// TestRunInitWizard_AcceptAll_SkipsConfigPrompts drives the unified init wizard
// against a fake cluster with a fully-detected existing install and
// --accept-existing. Accept-all seeds every detected key, so the Detected and
// Workspace config screens must short-circuit (no prompt text), and RunInstall
// must receive the detected workspace class + a preconfirmed WebdRoutingOpts.
func TestRunInitWizard_AcceptAll_SkipsConfigPrompts(t *testing.T) {
	// Fake cluster: issuer email + workspace marker + idp CR + monitoring channel.
	// The workspace marker ConfigMap and a matching RWX StorageClass sit in the
	// typed client so detection reports the class AND ListRWXClasses can offer it
	// (a Choice with no options is a fatal Prepare error) — the accept-all seed
	// then short-circuits that offered screen.
	marker := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: workspaceMarkerName, Namespace: workspaceMarkerNamespace},
		Data:       map[string]string{workspaceMarkerKey: "ap-workspace-rwx"},
	}
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "ap-workspace-rwx"},
		Provisioner: "cluster.local/ap-workspace-provisioner",
	}
	b := &kube.Bundle{
		Controller: fakeCtrlWith(t, idpCR("google"), monitoringChannel("demo-mon", "slack")),
		Dynamic:    fakeDynWithIssuer(t, "ops@example.com"),
		Typed:      k8sfake.NewSimpleClientset(marker, sc),
		Namespace:  "agentprimitives-system",
	}

	// Inject the fake bundle in place of a live cluster.
	restoreBundle := swap(&newInitWizardBundleFn, func(*apcmd.Globals) (*kube.Bundle, error) { return b, nil })
	t.Cleanup(restoreBundle)

	// Drive the whole run over a Plain driver with empty stdin: every presenting
	// screen (SpiceDB, hostname, Proceed) takes its default with no terminal.
	restoreDriver := swap(&driverForInitWizard, func(p tui.DriverParams) tui.Driver {
		return tui.Plain(strings.NewReader(""), p.Out, p.Theme)
	})
	t.Cleanup(restoreDriver)

	// Stub the three long-running phases so the test needs no images/cluster.
	// Both Build and Status also record whether they received a non-nil rail —
	// F5's core claim is that the wizard threads its OWN railState into these
	// two phases exactly as it already does for Install, instead of the gutter
	// vanishing for the Build/Status span.
	buildCalled, buildRailNonNil := false, false
	restoreBuild := swap(&runBuildFn, func(_ context.Context, _ io.Writer, _, _ string, _ bool, _, _ string, _, _, _ bool, _ *apcmd.Globals, rail progress.RailProvider) (map[string]string, error) {
		buildCalled = true
		buildRailNonNil = rail != nil
		return map[string]string{"agentprimitives-runner": "sha256:deadbeef"}, nil
	})
	t.Cleanup(restoreBuild)

	var got installCall
	restoreInstall := swap(&runInstallFn, func(ctx, recheckCtx context.Context, cfg InstallConfig) error {
		_, installHasDeadline := ctx.Deadline()
		_, recheckHasDeadline := recheckCtx.Deadline()
		got = installCall{called: true, r: cfg.Routing, wsOpts: cfg.Workspace, ext: cfg.Ext, strat: cfg.Strat, tags: cfg.Tags, railNonNil: cfg.Rail != nil, installHasDeadline: installHasDeadline, recheckHasDeadline: recheckHasDeadline}
		return nil
	})
	t.Cleanup(restoreInstall)

	checksCalled, checksRailNonNil := false, false
	restoreChecks := swap(&checksFn, func(_ context.Context, _ io.Writer, _ *kube.Bundle, _ time.Duration, rail progress.RailProvider) error {
		checksCalled = true
		checksRailNonNil = rail != nil
		return nil
	})
	t.Cleanup(restoreChecks)

	// The settings wizard is a real cluster-writing flow; stub it to a no-op so
	// the post-install phase does not do live kubeconfig discovery.
	settingsCalled := false
	restoreSettings := swap(&runSettingsWizardFn, func(context.Context, *cobra.Command, *apcmd.Globals, bool, bool, string, map[string]string, settingscmd.DefaultModelOpts, tui.Driver) error {
		settingsCalled = true
		return nil
	})
	t.Cleanup(restoreSettings)

	directoryCalled := false
	t.Cleanup(swap(&runDirectoryConfigureFn, func(context.Context, io.Reader, io.Writer, *apcmd.Globals) error {
		directoryCalled = true
		return nil
	}))

	// Force the settings phase to run deterministically (M6 gates it on
	// stdinInteractiveFn()/p.defaults, and this test asserts it ran). This
	// test is about accept-all's DETECTED-settings seeding, not the TTY gate
	// itself — see TestRunInitWizard_SettingsPhaseGatedByTTY for that.
	t.Cleanup(swap(&stdinInteractiveFn, func() bool { return true }))

	g := &apcmd.Globals{Namespace: b.Namespace, BundleFn: func() (*kube.Bundle, error) { return b, nil }}
	var out bytes.Buffer
	p := initWizardParams{
		strat:          cloud.MustFor(cloud.KeyLocal),
		registry:       "",
		platform:       "linux/amd64",
		tags:           manifests.Tags{},
		r:              WebdRoutingOpts{},
		timeout:        2 * time.Minute,
		acceptExisting: true,
	}

	require.NoError(t, runInitWizard(context.Background(), &out, g, p))

	s := out.String()
	// Accept-all short-circuits the seeded config screens: their prompt text
	// must never reach the user.
	assert.NotContains(t, s, "Review each setting", "Detected screen must short-circuit under accept-all")
	assert.NotContains(t, s, "Workspace storage class", "Workspace screen must short-circuit when the class is detected")
	// N5: this fake cluster's SpiceDB is bundled (no external ConfigMap), a
	// POSITIVELY detected value, not merely absent — so accept-all must short-
	// circuit the SpiceDB screen here too, exactly as it does for a detected
	// external backend, instead of asking a question with nothing left to answer.
	assert.NotContains(t, s, "Use an existing external SpiceDB", "SpiceDB screen must short-circuit when the (bundled) backend is detected")

	// The three phases ran, and RunInstall received the DETECTED inputs.
	assert.True(t, buildCalled, "build phase ran (skipBuild=false)")
	require.True(t, got.called, "install phase ran")
	assert.True(t, checksCalled, "status/check phase ran")
	assert.True(t, settingsCalled, "settings phase ran")
	assert.True(t, directoryCalled, "directory-sync configure offer ran (M5: dropped from the wizard)")

	assert.Equal(t, "", got.wsOpts.ExplicitClass, "keeping the detected workspace class must NOT force a re-probe: RunInstall reads the marker directly (WorkspaceUseDirectly), matching the non-wizard path")
	assert.Equal(t, "ops@example.com", got.r.acmeEmail, "detected ACME email folds into WebdRoutingOpts")
	assert.True(t, got.r.preconfirmed, "the Proceed step preconfirms so RunInstall does not re-ask")
	assert.Equal(t, ExternalSpiceDB{}, got.ext, "no external SpiceDB chosen → bundled (zero value)")
	assert.True(t, got.railNonNil, "the install checklist receives a non-nil rail")
	assert.Equal(t, map[string]string{"agentprimitives-runner": "sha256:deadbeef"}, got.tags.Digests, "build digests pin the install")

	// F5: the rail is not just an Install-phase thing — Build and Status must
	// receive the SAME non-nil rail so the gutter does not vanish for those
	// phases and reappear only for Install.
	assert.True(t, buildRailNonNil, "the build phase receives a non-nil rail")
	assert.True(t, checksRailNonNil, "the status/check phase receives a non-nil rail")

	// R1: the wizard wraps the install context like the linear runInit — installCtx
	// carries the timeout, recheckCtx carries only SIGINT so the keep-waiting Y is
	// never a no-op against an exhausted overall deadline.
	assert.True(t, got.installHasDeadline, "RunInstall's ctx is bounded by p.timeout, matching the linear path")
	assert.False(t, got.recheckHasDeadline, "the recheck ctx carries SIGINT but no timeout")
}

// TestRunInitWizard_WorkspaceClassForwarding drives the unified init wizard
// against a fresh cluster (no workspace marker, so nothing is DETECTED) that
// offers RWX StorageClasses. With nothing seeded, the Workspace screen presents
// and its silent/default answer records a class as CHOSEN. Whether that chosen
// class becomes RunInstall's ExplicitClass depends on interactivity:
//
//   - interactive: a genuine, non-billable pick is a real override RunInstall
//     must probe before trusting (WorkspaceProbeBeforeUse), so it forwards.
//   - non-interactive: the Plain driver fabricated the pick on EOF
//     (pkg/cli/tui/driver.go), so it must NOT become an override — a billable
//     Filestore class must never be provisioned from an answer nobody gave (the
//     "never silently enable billable Filestore" contract, M3).
//
// Interactivity is forced through stdinInteractiveFn so the branch is exercised
// deterministically, independent of the test process's own stdin.
func TestRunInitWizard_WorkspaceClassForwarding(t *testing.T) {
	newBundle := func() *kube.Bundle {
		return &kube.Bundle{
			Controller: fakeCtrlWith(t), // no idp CR, no monitoring channel: fresh cluster
			Dynamic:    fakeDynNoIssuer(t),
			Typed: k8sfake.NewSimpleClientset(
				&storagev1.StorageClass{
					ObjectMeta:  metav1.ObjectMeta{Name: "ap-workspace-fresh"},
					Provisioner: "cluster.local/ap-workspace-provisioner",
				},
			), // no workspace marker: nothing detected
			Namespace: "agentprimitives-system",
		}
	}
	newParams := func() initWizardParams {
		return initWizardParams{
			strat:          cloud.MustFor(cloud.KeyLocal),
			platform:       "linux/amd64",
			timeout:        2 * time.Minute,
			acceptExisting: false, // nothing is detected; --accept-existing would fail loudly
			noIdp:          true,
			noMonitoring:   true,
			// Supplied so the routing screens short-circuit; this test is about
			// the workspace class, not routing.
			r: WebdRoutingOpts{trustedHostname: "webd.example.com", sandboxHostname: "sandbox.example.com"},
		}
	}

	t.Run("interactive genuine pick: forwarded as ExplicitClass so RunInstall probes it", func(t *testing.T) {
		t.Cleanup(swap(&stdinInteractiveFn, func() bool { return true }))
		_, got := driveInitWizardForRouting(t, newBundle(), newParams())
		require.True(t, got.called, "install phase ran")
		assert.Equal(t, "ap-workspace-fresh", got.wsOpts.ExplicitClass,
			"a non-billable class a human actually chose is a real override and must still be probed")
		assert.True(t, got.wsOpts.Preconfirmed, "the wizard presented the Workspace decision; RunInstall must not re-ask")
	})

	t.Run("non-TTY fabricated pick: NOT forwarded (never a silent billable override)", func(t *testing.T) {
		t.Cleanup(swap(&stdinInteractiveFn, func() bool { return false }))
		_, got := driveInitWizardForRouting(t, newBundle(), newParams())
		require.True(t, got.called, "install phase ran")
		assert.Equal(t, "", got.wsOpts.ExplicitClass,
			"a non-TTY EOF-fabricated pick must not become an explicit override; RunInstall resolves safely")
		assert.True(t, got.wsOpts.Preconfirmed, "the wizard still owns the Workspace decision; RunInstall must not re-ask")
	})
}

// TestWizardWorkspaceResolveOptions_IsolatedFoldsIntoNoRWX is N3's core unit
// contract: the isolated sentinel must fold into WorkspaceResolveOptions.NoRWX,
// never into ExplicitClass — resolveWorkspaceDecision would otherwise probe
// "isolated" as a literal (nonexistent) StorageClass name.
func TestWizardWorkspaceResolveOptions_IsolatedFoldsIntoNoRWX(t *testing.T) {
	classes := []cloud.RWXClassInfo{{Name: "ap-workspace-rwx", Bundled: true}}
	confirmBillable := func(string) bool {
		t.Fatal("the isolated pick must never reach the billable-class consent")
		return false
	}
	got := wizardWorkspaceResolveOptions(workspaceIsolatedValue, "ap-workspace-rwx", classes, true, confirmBillable)
	assert.Equal(t, WorkspaceResolveOptions{NoRWX: true, Preconfirmed: true}, got)
}

// TestWizardWorkspaceResolveOptions_DelegatesNonIsolatedChoices proves
// wizardWorkspaceResolveOptions is a thin wrapper for every OTHER answer: it
// defers to wizardWorkspaceExplicitClass unchanged (already covered in depth
// by TestRunInitWizard_WorkspaceClassForwarding / _NonTTYFilestoreNotFabricated),
// and always sets Preconfirmed.
func TestWizardWorkspaceResolveOptions_DelegatesNonIsolatedChoices(t *testing.T) {
	classes := []cloud.RWXClassInfo{{Name: "ap-workspace-rwx", Bundled: true}}
	confirmBillable := func(string) bool { return true }

	t.Run("kept/empty choice: ExplicitClass empty", func(t *testing.T) {
		got := wizardWorkspaceResolveOptions("", "ap-workspace-rwx", classes, true, confirmBillable)
		assert.Equal(t, WorkspaceResolveOptions{ExplicitClass: "", Preconfirmed: true}, got)
	})

	t.Run("genuine change, interactive, non-billable: forwarded as ExplicitClass", func(t *testing.T) {
		got := wizardWorkspaceResolveOptions("ap-workspace-rwx", "", classes, true, confirmBillable)
		assert.Equal(t, WorkspaceResolveOptions{ExplicitClass: "ap-workspace-rwx", Preconfirmed: true}, got)
	})
}

// TestRunInitWizard_NonTTYFilestoreNotFabricated is M3's core contract: a non-TTY
// wizard run whose Workspace key is unseeded must NOT pin a billable Filestore
// class the Plain driver fabricated on EOF. Even with a Filestore class on the
// cluster and no marker to compare against, the forwarded ExplicitClass stays
// empty so RunInstall never provisions billable storage nobody consented to.
func TestRunInitWizard_NonTTYFilestoreNotFabricated(t *testing.T) {
	t.Cleanup(swap(&stdinInteractiveFn, func() bool { return false }))
	b := &kube.Bundle{
		Controller: fakeCtrlWith(t),
		Dynamic:    fakeDynNoIssuer(t),
		Typed: k8sfake.NewSimpleClientset(
			&storagev1.StorageClass{
				ObjectMeta:  metav1.ObjectMeta{Name: "enterprise-multishare-rwx"},
				Provisioner: "filestore.csi.storage.gke.io",
			},
		),
		Namespace: "agentprimitives-system",
	}
	p := initWizardParams{
		strat:          cloud.MustFor(cloud.KeyLocal),
		platform:       "linux/amd64",
		timeout:        2 * time.Minute,
		acceptExisting: false,
		noIdp:          true,
		noMonitoring:   true,
		r:              WebdRoutingOpts{trustedHostname: "webd.example.com", sandboxHostname: "sandbox.example.com"},
	}
	_, got := driveInitWizardForRouting(t, b, p)
	require.True(t, got.called, "install phase ran")
	assert.Equal(t, "", got.wsOpts.ExplicitClass,
		"a non-TTY run must never fabricate a billable Filestore ExplicitClass")
}

// railPinSpy records, for every screen it presents, the rail step index it would
// render the screen under: its pinned step when the driver was reframed onto a
// rail slot, otherwise the screen's own ID. It is a tui.Reframer, so
// routingRunOptions' tui.Reframe hands back a child pinned to the Routing slot; a
// shared atIndex map and reframedTo pointer let the parent and its children write
// one record. This is what makes the -1 rail-blank observable: an un-reframed
// present of sandbox-hostname/acme-email records -1, a reframed one records the
// Routing slot's real index.
type railPinSpy struct {
	chrome     *tui.Chrome
	pinned     string         // "" until reframed onto a rail step
	reframedTo *string        // the stepID Reframe was last asked to pin (shared)
	atIndex    map[string]int // screenID -> rail index it presented under (shared)
}

func (d *railPinSpy) Present(_ context.Context, screenID string, _ *huh.Group) error {
	id := screenID
	if d.pinned != "" {
		id = d.pinned
	}
	d.atIndex[screenID] = d.chrome.StepIndex(id)
	return nil
}

func (d *railPinSpy) Reframe(ch *tui.Chrome, stepID string) tui.Driver {
	*d.reframedTo = stepID
	return &railPinSpy{chrome: ch, pinned: stepID, reframedTo: d.reframedTo, atIndex: d.atIndex}
}

// TestRoutingScreens_PinnedToRoutingRailSlot proves Fix 1: the routing screens
// present under the single Routing rail slot, not each under its own ID. Only
// trusted-hostname is a rail step; sandbox-hostname and acme-email resolve to
// StepIndex == -1 under their own IDs, which blanks every rail step while they
// show. routingRunOptions reframes the shared driver onto flagTrustedHostname so
// all three keep the highlight on the Routing slot.
//
// It drives the REAL routing screens through the REAL routingRunOptions output,
// so it fails against the un-reframed code: with no reframe, Reframe is never
// called (reframedTo stays "") and sandbox-hostname/acme-email record -1.
func TestRoutingScreens_PinnedToRoutingRailSlot(t *testing.T) {
	theme := tui.NewTheme(tui.Caps{})
	chrome := tui.NewChrome("oap init", initRailSteps, theme)

	routingIdx := chrome.StepIndex(flagTrustedHostname)
	require.GreaterOrEqual(t, routingIdx, 0, "the Routing rail slot must exist in initRailSteps")

	reframedTo := ""
	spy := &railPinSpy{chrome: chrome, reframedTo: &reframedTo, atIndex: map[string]int{}}
	base := tui.Options{Theme: theme, In: strings.NewReader(""), Out: io.Discard, Driver: spy}

	// Detection reports a trusted hostname so the sandbox + acme screens do NOT
	// Skip and actually present — they are the two screens whose own IDs resolve
	// to -1, so the run must present them to exercise the property.
	d := DetectedSettings{TrustedHostname: "webd.example.com"}
	_, err := tui.RunWith(context.Background(), newRoutingScreens(cloud.DevProfile, WebdRoutingOpts{}, d), routingRunOptions(base, spy, chrome), tui.NewState())
	require.NoError(t, err)

	assert.Equal(t, flagTrustedHostname, reframedTo,
		"the routing screens' driver is reframed onto the Routing rail slot")
	// All three routing screens present under the ONE Routing slot, never -1.
	assert.Equal(t, routingIdx, spy.atIndex[flagTrustedHostname], "trusted-hostname pinned to the Routing slot")
	assert.Equal(t, routingIdx, spy.atIndex[flagSandboxHostname], "sandbox-hostname pinned to the Routing slot, not -1")
	assert.Equal(t, routingIdx, spy.atIndex[flagACMEEmail], "acme-email pinned to the Routing slot, not -1")
}

// TestInitRailSteps_AllImperativeLookupsResolve pins Fix 3: every rail step the
// orchestrator advances to via chrome.StepIndex must resolve to a valid (>= 0)
// index in initRailSteps. A future const rename that dropped one from the rail
// would silently make it resolve to -1 — the exact rail-blanking class Fix 1
// addresses — and this catches it before a user ever sees a blank rail.
func TestInitRailSteps_AllImperativeLookupsResolve(t *testing.T) {
	theme := tui.NewTheme(tui.Caps{})
	chrome := tui.NewChrome("", initRailSteps, theme)
	for _, id := range []string{
		// Config-screen IDs the config phase presents under.
		keyDetected, keyWorkspaceClass, keyExternalSpiceDB, flagTrustedHostname, keyProceed,
		// Imperative phases the orchestrator advances rail.active to.
		stepBuild, stepInstall, stepStatus, keySettings, stepDirectory, keyIdP, keyMonitoring,
	} {
		assert.GreaterOrEqualf(t, chrome.StepIndex(id), 0, "rail step %q must resolve to a valid index", id)
	}
}

// driveInitWizardForRouting runs the unified init wizard over a Plain driver
// (empty stdin, so every presenting screen takes its default) with the
// long-running Build/Install/Status phases and the settings phase stubbed to
// no-ops. It returns what reached the user (out) and the inputs RunInstall was
// called with. This is the reproduction harness for the routing-prompt bug: a
// caller varies only the fake cluster and the routing flags on p.
func driveInitWizardForRouting(t *testing.T, b *kube.Bundle, p initWizardParams) (string, installCall) {
	t.Helper()
	t.Cleanup(swap(&newInitWizardBundleFn, func(*apcmd.Globals) (*kube.Bundle, error) { return b, nil }))
	t.Cleanup(swap(&driverForInitWizard, func(dp tui.DriverParams) tui.Driver {
		return tui.Plain(strings.NewReader(""), dp.Out, dp.Theme)
	}))
	t.Cleanup(swap(&runBuildFn, func(context.Context, io.Writer, string, string, bool, string, string, bool, bool, bool, *apcmd.Globals, progress.RailProvider) (map[string]string, error) {
		return map[string]string{"agentprimitives-runner": "sha256:deadbeef"}, nil
	}))
	var got installCall
	t.Cleanup(swap(&runInstallFn, func(ctx, recheckCtx context.Context, cfg InstallConfig) error {
		got = installCall{called: true, r: cfg.Routing, wsOpts: cfg.Workspace, ext: cfg.Ext, strat: cfg.Strat, tags: cfg.Tags, railNonNil: cfg.Rail != nil}
		return nil
	}))
	t.Cleanup(swap(&checksFn, func(context.Context, io.Writer, *kube.Bundle, time.Duration, progress.RailProvider) error { return nil }))
	t.Cleanup(swap(&runSettingsWizardFn, func(context.Context, *cobra.Command, *apcmd.Globals, bool, bool, string, map[string]string, settingscmd.DefaultModelOpts, tui.Driver) error {
		return nil
	}))
	t.Cleanup(swap(&runDirectoryConfigureFn, func(context.Context, io.Reader, io.Writer, *apcmd.Globals) error { return nil }))

	g := &apcmd.Globals{Namespace: b.Namespace, BundleFn: func() (*kube.Bundle, error) { return b, nil }}
	var out bytes.Buffer
	require.NoError(t, runInitWizard(context.Background(), &out, g, p))
	return out.String(), got
}

// TestRunInitWizard_SkipsRoutingWhenHostnameKnown is the reproduction for the
// live bug: `oap init --wizard` re-prompted "Hostname for the web UI" even
// though the hostname was already known — supplied on the command line
// (--hostname-suffix, expanded into r before dispatch) OR present on the
// cluster. Neither reached the routing screen: nothing seeded the flag value
// into State, and detectSettings never read the cluster's hostname. With both
// fixes, the routing screen short-circuits and the known hostname flows into
// RunInstall.
func TestRunInitWizard_SkipsRoutingWhenHostnameKnown(t *testing.T) {
	// A fully-detected existing install (issuer email + workspace marker + idp +
	// monitoring channel) so --accept-existing resolves and the config screens
	// short-circuit. Only the routing hostname's source varies per case.
	marker := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: workspaceMarkerName, Namespace: workspaceMarkerNamespace},
		Data:       map[string]string{workspaceMarkerKey: "ap-workspace-rwx"},
	}
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "ap-workspace-rwx"},
		Provisioner: "cluster.local/ap-workspace-provisioner",
	}

	// Flag case: --hostname-suffix leaves the two hostnames on r before dispatch,
	// and no webd routes exist on the cluster, so only Fix (a) — seeding the flag
	// value into State — can keep the routing screen from prompting.
	t.Run("flag-supplied hostname: routing screen never prompts, flag reaches install", func(t *testing.T) {
		b := &kube.Bundle{
			Controller: fakeCtrlWith(t, idpCR("google"), monitoringChannel("demo-mon", "slack")),
			Dynamic:    fakeDynWithIssuer(t, "ops@example.com"),
			Typed:      k8sfake.NewSimpleClientset(marker, sc),
			Namespace:  "agentprimitives-system",
		}
		p := initWizardParams{
			strat:          cloud.MustFor(cloud.KeyLocal),
			platform:       "linux/amd64",
			timeout:        2 * time.Minute,
			acceptExisting: true,
			noIdp:          true,
			noMonitoring:   true,
			r:              WebdRoutingOpts{trustedHostname: "webd.example.com", sandboxHostname: "sandbox.example.com"},
		}
		out, got := driveInitWizardForRouting(t, b, p)

		assert.NotContains(t, out, "Hostname for the web UI",
			"a hostname supplied on the command line must not be re-prompted by the routing screen")
		require.True(t, got.called, "install phase ran")
		assert.Equal(t, "webd.example.com", got.r.trustedHostname, "the flag hostname reaches RunInstall")
		assert.Equal(t, "sandbox.example.com", got.r.sandboxHostname, "the flag sandbox hostname reaches RunInstall")
		assert.True(t, got.r.preconfirmed, "the Proceed step preconfirms so RunInstall does not re-ask")
	})

	// Detected case: no routing flags, but the cluster's webd HTTPRoutes carry the
	// hostnames, so only Fix (b) — detectSettings reading them, then the existing
	// seedStateFromDetected seeding State — can keep the routing screen quiet.
	t.Run("detected hostname: routing screen never prompts, cluster value reaches install", func(t *testing.T) {
		b := &kube.Bundle{
			Controller: fakeCtrlWith(t, idpCR("google"), monitoringChannel("demo-mon", "slack"),
				webdRoute(webdTrustedRouteName, "webd.example.com"),
				webdRoute(webdSandboxRouteName, "sandbox.example.com")),
			Dynamic:   fakeDynWithIssuer(t, "ops@example.com"),
			Typed:     k8sfake.NewSimpleClientset(marker, sc),
			Namespace: "agentprimitives-system",
		}
		p := initWizardParams{
			strat:          cloud.MustFor(cloud.KeyLocal),
			platform:       "linux/amd64",
			timeout:        2 * time.Minute,
			acceptExisting: true,
			noIdp:          true,
			noMonitoring:   true,
			r:              WebdRoutingOpts{},
		}
		out, got := driveInitWizardForRouting(t, b, p)

		assert.NotContains(t, out, "Hostname for the web UI",
			"a hostname already on the cluster must not be re-prompted by the routing screen")
		require.True(t, got.called, "install phase ran")
		assert.Equal(t, "webd.example.com", got.r.trustedHostname, "the detected hostname reaches RunInstall")
		assert.Equal(t, "sandbox.example.com", got.r.sandboxHostname, "the detected sandbox hostname reaches RunInstall")
	})
}

// TestRunInitWizard_ManualRoutingIgnoresDetectedHostnames is round-3 audit
// R1's reproduction: a cluster with detected webd routes seeds the trusted/
// sandbox hostname into State before the routing screens ever run
// (seedStateFromDetected), so under --manual-webd-routing — where
// newRoutingScreens presents NO screens at all (wizard_screens.go) because
// the operator is wiring routing themselves — the seeded value used to fold
// into WebdRoutingOpts anyway via applyRoutingAnswers reading the pre-seeded
// State. That manufactured RunInstall's post-build "--trusted-hostname and
// --manual-webd-routing are mutually exclusive" refusal (install.go:817),
// naming a flag the operator never passed. Linear never has this problem:
// askWebdRoutingInputs (routing_ask.go) skips entirely under manualRouting,
// so it never reads the cluster's routes to begin with.
func TestRunInitWizard_ManualRoutingIgnoresDetectedHostnames(t *testing.T) {
	marker := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: workspaceMarkerName, Namespace: workspaceMarkerNamespace},
		Data:       map[string]string{workspaceMarkerKey: "ap-workspace-rwx"},
	}
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "ap-workspace-rwx"},
		Provisioner: "cluster.local/ap-workspace-provisioner",
	}
	b := &kube.Bundle{
		Controller: fakeCtrlWith(t, idpCR("google"), monitoringChannel("demo-mon", "slack"),
			webdRoute(webdTrustedRouteName, "webd.example.com"),
			webdRoute(webdSandboxRouteName, "sandbox.example.com")),
		Dynamic:   fakeDynWithIssuer(t, "ops@example.com"),
		Typed:     k8sfake.NewSimpleClientset(marker, sc),
		Namespace: "agentprimitives-system",
	}
	p := initWizardParams{
		strat:          cloud.MustFor(cloud.KeyLocal),
		platform:       "linux/amd64",
		timeout:        2 * time.Minute,
		acceptExisting: true,
		noIdp:          true,
		noMonitoring:   true,
		r:              WebdRoutingOpts{manualWebdRouting: true},
	}
	_, got := driveInitWizardForRouting(t, b, p)

	require.True(t, got.called, "install phase ran")
	assert.Empty(t, got.r.trustedHostname, "a detected trusted hostname must not fold under --manual-webd-routing: RunInstall's post-build gate would refuse a flag the operator never passed")
	assert.Empty(t, got.r.sandboxHostname, "a detected sandbox hostname must not fold under --manual-webd-routing either")
}

// TestRunInitWizard_DisableViewerIgnoresDetectedSandboxHostname is R1's sibling
// case: --disable-artifact-viewer alone (routing not manual) must not fold a
// detected sandbox hostname either, since validateWebdRouting refuses
// sandbox+disableViewer unconditionally too — while the trusted hostname,
// which that flag says nothing against, still reaches RunInstall.
func TestRunInitWizard_DisableViewerIgnoresDetectedSandboxHostname(t *testing.T) {
	marker := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: workspaceMarkerName, Namespace: workspaceMarkerNamespace},
		Data:       map[string]string{workspaceMarkerKey: "ap-workspace-rwx"},
	}
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "ap-workspace-rwx"},
		Provisioner: "cluster.local/ap-workspace-provisioner",
	}
	b := &kube.Bundle{
		Controller: fakeCtrlWith(t, idpCR("google"), monitoringChannel("demo-mon", "slack"),
			webdRoute(webdTrustedRouteName, "webd.example.com"),
			webdRoute(webdSandboxRouteName, "sandbox.example.com")),
		Dynamic:   fakeDynWithIssuer(t, "ops@example.com"),
		Typed:     k8sfake.NewSimpleClientset(marker, sc),
		Namespace: "agentprimitives-system",
	}
	p := initWizardParams{
		strat:          cloud.MustFor(cloud.KeyLocal),
		platform:       "linux/amd64",
		timeout:        2 * time.Minute,
		acceptExisting: true,
		noIdp:          true,
		noMonitoring:   true,
		r:              WebdRoutingOpts{disableViewer: true},
	}
	_, got := driveInitWizardForRouting(t, b, p)

	require.True(t, got.called, "install phase ran")
	assert.Equal(t, "webd.example.com", got.r.trustedHostname, "the trusted hostname is unaffected by --disable-artifact-viewer")
	assert.Empty(t, got.r.sandboxHostname, "a detected sandbox hostname must not fold under --disable-artifact-viewer: RunInstall's post-build gate would refuse a flag the operator never passed")
}

// TestRunInitWizard_NilStratDoesNotPanicAtRoutingProfile pins the round-3
// audit N7 nit: p.strat.InstallProfile() at the routing-screens call site was
// unguarded, unlike the IdP/Monitoring gate's InstallProfile() read a few
// lines further down (which already falls back to cloud.ProductionProfile).
// p.strat is resolved on every real dispatch into the wizard, so this is
// unreachable in production — but a directly-constructed initWizardParams
// (a nil strat, the zero value of the interface field) must not panic here
// either, matching the sibling guard's defensive shape.
func TestRunInitWizard_NilStratDoesNotPanicAtRoutingProfile(t *testing.T) {
	b := &kube.Bundle{
		Controller: fakeCtrlWith(t), // no idp CR, no monitoring channel: fresh cluster
		Dynamic:    fakeDynNoIssuer(t),
		Typed:      k8sfake.NewSimpleClientset(), // no RWX StorageClass: the Workspace screen never offers, so p.strat's other (already-guarded) use sites are not in play
		Namespace:  "agentprimitives-system",
	}
	p := initWizardParams{
		// strat deliberately left unset (nil interface) — the case this guard covers.
		platform:       "linux/amd64",
		timeout:        2 * time.Minute,
		acceptExisting: false,
		noIdp:          true,
		noMonitoring:   true,
		r:              WebdRoutingOpts{trustedHostname: "webd.example.com", sandboxHostname: "sandbox.example.com"},
	}

	// driveInitWizardForRouting itself require.NoError's the run — a panic
	// here (the regression this pins) fails the test with its own trace, and a
	// clean run additionally proves the install phase was reached.
	_, got := driveInitWizardForRouting(t, b, p)
	assert.True(t, got.called, "install phase still ran with a nil strat")
}

// schemaApplyCall records the arguments applySchemaToEndpointFn was invoked
// with, so a test can prove the wizard runs the external-SpiceDB schema tail
// (Fix B2/A1) with the external endpoint/token — without dialing a real SpiceDB.
type schemaApplyCall struct {
	called   bool
	endpoint string
	token    string
	insecure bool
}

// TestRunInitWizard_AppliesExternalSpiceDBSchema is B2/A1's regression: a wizard
// install pointed at an external SpiceDB must run the post-install schema apply
// (applySchemaToEndpoint) — the ONLY schema-write path for an external instance,
// which the bundled SpiceDB gets at startup and an external one does not. Before
// the fix the wizard went straight from install to checks, coming up schema-less
// with green checks. Here applySchemaToEndpointFn is stubbed to record its args,
// so a missing call (the bug) fails the test.
func TestRunInitWizard_AppliesExternalSpiceDBSchema(t *testing.T) {
	b := &kube.Bundle{
		Controller: fakeCtrlWith(t),
		Dynamic:    fakeDynNoIssuer(t),
		Typed:      k8sfake.NewSimpleClientset(),
		Namespace:  "agentprimitives-system",
	}
	t.Cleanup(swap(&newInitWizardBundleFn, func(*apcmd.Globals) (*kube.Bundle, error) { return b, nil }))
	t.Cleanup(swap(&driverForInitWizard, func(dp tui.DriverParams) tui.Driver {
		return tui.Plain(strings.NewReader(""), dp.Out, dp.Theme)
	}))
	t.Cleanup(swap(&runBuildFn, func(context.Context, io.Writer, string, string, bool, string, string, bool, bool, bool, *apcmd.Globals, progress.RailProvider) (map[string]string, error) {
		return map[string]string{}, nil
	}))
	var gotExt ExternalSpiceDB
	t.Cleanup(swap(&runInstallFn, func(_, _ context.Context, cfg InstallConfig) error {
		gotExt = cfg.Ext
		return nil
	}))
	t.Cleanup(swap(&checksFn, func(context.Context, io.Writer, *kube.Bundle, time.Duration, progress.RailProvider) error { return nil }))
	t.Cleanup(swap(&runSettingsWizardFn, func(context.Context, *cobra.Command, *apcmd.Globals, bool, bool, string, map[string]string, settingscmd.DefaultModelOpts, tui.Driver) error {
		return nil
	}))
	t.Cleanup(swap(&runDirectoryConfigureFn, func(context.Context, io.Reader, io.Writer, *apcmd.Globals) error { return nil }))

	var schema schemaApplyCall
	t.Cleanup(swap(&applySchemaToEndpointFn, func(_ context.Context, _ io.Writer, endpoint, token string, insecure bool) error {
		schema = schemaApplyCall{called: true, endpoint: endpoint, token: token, insecure: insecure}
		return nil
	}))

	g := &apcmd.Globals{Namespace: b.Namespace, BundleFn: func() (*kube.Bundle, error) { return b, nil }}
	p := initWizardParams{
		strat:                   cloud.MustFor(cloud.KeyLocal),
		platform:                "linux/amd64",
		timeout:                 2 * time.Minute,
		noIdp:                   true,
		noMonitoring:            true,
		r:                       WebdRoutingOpts{trustedHostname: "webd.example.com", sandboxHostname: "sandbox.example.com"},
		externalSpiceDBEndpoint: "spicedb.example.com:443",
		externalSpiceDBToken:    "extern-tok",
		externalSpiceDBInsecure: false,
	}
	require.NoError(t, runInitWizard(context.Background(), &bytes.Buffer{}, g, p))

	require.True(t, gotExt.enabled(), "the external SpiceDB flag must reach RunInstall as an enabled Ext")
	require.True(t, schema.called, "the wizard must run applySchemaToEndpoint for an external SpiceDB — it is the only schema-write path for one")
	assert.Equal(t, "spicedb.example.com:443", schema.endpoint, "the schema apply targets the external endpoint")
	assert.Equal(t, "extern-tok", schema.token, "the schema apply carries the external token")
	assert.False(t, schema.insecure, "the schema apply carries the external TLS setting")
}

// TestRunInitWizard_ExternalSpiceDBFlagSkipsScreen is F2/m2's regression: with
// --external-spicedb-endpoint supplied, the SpiceDB screen must NOT ask "use an
// existing external instance?" — the flag already answered it, and
// resolveExternalSpiceDBFromState gives the flag absolute precedence, so a screen
// answer could never matter. Seeding keyExternalSpiceDB from the flag
// short-circuits the screen and the flag flows to RunInstall.
func TestRunInitWizard_ExternalSpiceDBFlagSkipsScreen(t *testing.T) {
	b := &kube.Bundle{
		Controller: fakeCtrlWith(t),
		Dynamic:    fakeDynNoIssuer(t),
		Typed:      k8sfake.NewSimpleClientset(),
		Namespace:  "agentprimitives-system",
	}
	t.Cleanup(swap(&newInitWizardBundleFn, func(*apcmd.Globals) (*kube.Bundle, error) { return b, nil }))
	t.Cleanup(swap(&driverForInitWizard, func(dp tui.DriverParams) tui.Driver {
		return tui.Plain(strings.NewReader(""), dp.Out, dp.Theme)
	}))
	t.Cleanup(swap(&runBuildFn, func(context.Context, io.Writer, string, string, bool, string, string, bool, bool, bool, *apcmd.Globals, progress.RailProvider) (map[string]string, error) {
		return map[string]string{}, nil
	}))
	var gotExt ExternalSpiceDB
	t.Cleanup(swap(&runInstallFn, func(_, _ context.Context, cfg InstallConfig) error {
		gotExt = cfg.Ext
		return nil
	}))
	t.Cleanup(swap(&checksFn, func(context.Context, io.Writer, *kube.Bundle, time.Duration, progress.RailProvider) error { return nil }))
	t.Cleanup(swap(&runSettingsWizardFn, func(context.Context, *cobra.Command, *apcmd.Globals, bool, bool, string, map[string]string, settingscmd.DefaultModelOpts, tui.Driver) error {
		return nil
	}))
	t.Cleanup(swap(&applySchemaToEndpointFn, func(context.Context, io.Writer, string, string, bool) error { return nil }))
	t.Cleanup(swap(&runDirectoryConfigureFn, func(context.Context, io.Reader, io.Writer, *apcmd.Globals) error { return nil }))

	g := &apcmd.Globals{Namespace: b.Namespace, BundleFn: func() (*kube.Bundle, error) { return b, nil }}
	var out bytes.Buffer
	p := initWizardParams{
		strat:                   cloud.MustFor(cloud.KeyLocal),
		platform:                "linux/amd64",
		timeout:                 2 * time.Minute,
		noIdp:                   true,
		noMonitoring:            true,
		r:                       WebdRoutingOpts{trustedHostname: "webd.example.com", sandboxHostname: "sandbox.example.com"},
		externalSpiceDBEndpoint: "spicedb.example.com:443",
	}
	require.NoError(t, runInitWizard(context.Background(), &out, g, p))

	assert.NotContains(t, out.String(), "Use an existing external SpiceDB",
		"the SpiceDB screen must short-circuit when --external-spicedb-endpoint answered it")
	assert.Equal(t, "spicedb.example.com:443", gotExt.Endpoint, "the flag endpoint reaches RunInstall")
	assert.True(t, gotExt.enabled(), "the flag makes the install external")
}

// TestResolveExternalSpiceDBFromState covers the wizard's external-SpiceDB
// resolution (F3/M1 + F2). The flag wins outright; an unchosen external means
// bundled; and — the fail-closed contract — an external choice with no terminal
// to collect an endpoint on must error LOUDLY, never silently install bundled
// against the user's stated intent.
func TestResolveExternalSpiceDBFromState(t *testing.T) {
	t.Run("flag endpoint: returned outright, State ignored", func(t *testing.T) {
		st := tui.NewState()
		p := initWizardParams{externalSpiceDBEndpoint: "flag.example:443", externalSpiceDBToken: "flag-tok", externalSpiceDBInsecure: true}
		got, err := resolveExternalSpiceDBFromState(erroringReader{t}, st, &bytes.Buffer{}, p, DetectedSettings{})
		require.NoError(t, err)
		assert.Equal(t, ExternalSpiceDB{Endpoint: "flag.example:443", Token: "flag-tok", Insecure: true}, got)
	})

	t.Run("external not chosen: bundled (zero value), no error", func(t *testing.T) {
		got, err := resolveExternalSpiceDBFromState(erroringReader{t}, tui.NewState(), &bytes.Buffer{}, initWizardParams{}, DetectedSettings{})
		require.NoError(t, err)
		assert.Equal(t, ExternalSpiceDB{}, got)
	})

	t.Run("external chosen but non-interactive: fails loudly, never silently bundled", func(t *testing.T) {
		t.Cleanup(swap(&stdinInteractiveFn, func() bool { return false }))
		st := tui.NewState()
		st.SetBool(keyExternalSpiceDB, true)
		got, err := resolveExternalSpiceDBFromState(erroringReader{t}, st, &bytes.Buffer{}, initWizardParams{}, DetectedSettings{})
		require.Error(t, err, "an external choice that cannot be collected must fail, not fall back to bundled")
		assert.Contains(t, err.Error(), "no endpoint can be collected without a terminal")
		assert.Equal(t, ExternalSpiceDB{}, got)
	})

	// R1: keeping an already-external cluster must pre-fill the endpoint
	// prompt with the DETECTED endpoint, so a bare Enter keeps the cluster's
	// current connection instead of falling back to "no endpoint given" —
	// which refuseExternalToBundledFlip would then hard-abort.
	t.Run("external chosen, interactive, bare Enter on the endpoint: keeps the DETECTED endpoint, still prompts for the token", func(t *testing.T) {
		t.Cleanup(swap(&stdinInteractiveFn, func() bool { return true }))
		st := tui.NewState()
		st.SetBool(keyExternalSpiceDB, true)
		in := &stepReader{lines: []string{"\n", "prompted-tok\n", "\n"}}
		var out bytes.Buffer
		got, err := resolveExternalSpiceDBFromState(in, st, &out, initWizardParams{}, DetectedSettings{ExternalSpiceDB: true, ExternalSpiceDBEndpoint: "spicedb.example.com:443"})
		require.NoError(t, err)
		assert.Equal(t, ExternalSpiceDB{Endpoint: "spicedb.example.com:443", Token: "prompted-tok"}, got,
			"a bare Enter on the endpoint must keep the detected connection, not fall back to bundled")
		assert.Contains(t, out.String(), "spicedb.example.com:443", "the prompt must show the detected endpoint as its default")
	})

	// A genuinely typed endpoint still overrides the detected default — the
	// pre-fill is a convenience for the common "keep it" case, not a forced
	// value.
	t.Run("external chosen, interactive, a typed endpoint overrides the detected default", func(t *testing.T) {
		t.Cleanup(swap(&stdinInteractiveFn, func() bool { return true }))
		st := tui.NewState()
		st.SetBool(keyExternalSpiceDB, true)
		in := &stepReader{lines: []string{"typed.example:443\n", "prompted-tok\n", "\n"}}
		got, err := resolveExternalSpiceDBFromState(in, st, &bytes.Buffer{}, initWizardParams{}, DetectedSettings{ExternalSpiceDB: true, ExternalSpiceDBEndpoint: "spicedb.example.com:443"})
		require.NoError(t, err)
		assert.Equal(t, ExternalSpiceDB{Endpoint: "typed.example:443", Token: "prompted-tok"}, got)
	})
}

// TestRefuseExternalToBundledFlip covers F4's fail-closed guard: a detected
// external SpiceDB backend paired with a resolved decision to install the
// bundled one must be refused, so the endpoint config is never silently
// overwritten and the external instance never silently orphaned.
func TestRefuseExternalToBundledFlip(t *testing.T) {
	cases := []struct {
		name    string
		d       DetectedSettings
		ext     ExternalSpiceDB
		wantErr bool
	}{
		{
			name:    "detected external + resolved bundled: refuse (no silent flip)",
			d:       DetectedSettings{ExternalSpiceDB: true, ExternalSpiceDBEndpoint: "spicedb.example.com:443"},
			ext:     ExternalSpiceDB{},
			wantErr: true,
		},
		{
			name: "detected external + resolved external: allowed (kept)",
			d:    DetectedSettings{ExternalSpiceDB: true, ExternalSpiceDBEndpoint: "spicedb.example.com:443"},
			ext:  ExternalSpiceDB{Endpoint: "spicedb.example.com:443"},
		},
		{
			name: "detected bundled + resolved bundled: allowed (no external to orphan)",
			d:    DetectedSettings{ExternalSpiceDB: false},
			ext:  ExternalSpiceDB{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseExternalToBundledFlip(tc.d, tc.ext)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "spicedb.example.com:443", "the refusal must name the external endpoint")
				assert.Contains(t, err.Error(), "orphan")
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestRunInitWizard_NonTTYExternalSpiceDBNotFlipped is F4's non-negotiable
// end-to-end contract: a non-TTY accept-all run against a cluster whose SpiceDB
// endpoint points off-cluster must NOT flip the backend to the bundled
// in-cluster one from a fabricated default. Detection sees the external config,
// the seed marks the SpiceDB decision external, and the run fails closed BEFORE
// any install (which is what would overwrite the endpoint ConfigMap) rather than
// silently orphaning the external instance.
func TestRunInitWizard_NonTTYExternalSpiceDBNotFlipped(t *testing.T) {
	t.Cleanup(swap(&stdinInteractiveFn, func() bool { return false }))
	extCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "spicebox-spicedb-config", Namespace: cloud.WebdServiceNamespace},
		Data:       map[string]string{"endpoint": "spicedb.example.com:443"},
	}
	b := &kube.Bundle{
		Controller: fakeCtrlWith(t),
		Dynamic:    fakeDynNoIssuer(t),
		Typed:      k8sfake.NewSimpleClientset(extCM),
		Namespace:  "agentprimitives-system",
	}
	t.Cleanup(swap(&newInitWizardBundleFn, func(*apcmd.Globals) (*kube.Bundle, error) { return b, nil }))
	t.Cleanup(swap(&driverForInitWizard, func(dp tui.DriverParams) tui.Driver {
		return tui.Plain(strings.NewReader(""), dp.Out, dp.Theme)
	}))
	buildCalled := false
	t.Cleanup(swap(&runBuildFn, func(context.Context, io.Writer, string, string, bool, string, string, bool, bool, bool, *apcmd.Globals, progress.RailProvider) (map[string]string, error) {
		buildCalled = true
		return map[string]string{}, nil
	}))
	installCalled := false
	t.Cleanup(swap(&runInstallFn, func(context.Context, context.Context, InstallConfig) error {
		installCalled = true
		return nil
	}))
	t.Cleanup(swap(&checksFn, func(context.Context, io.Writer, *kube.Bundle, time.Duration, progress.RailProvider) error { return nil }))
	t.Cleanup(swap(&runSettingsWizardFn, func(context.Context, *cobra.Command, *apcmd.Globals, bool, bool, string, map[string]string, settingscmd.DefaultModelOpts, tui.Driver) error {
		return nil
	}))
	t.Cleanup(swap(&applySchemaToEndpointFn, func(context.Context, io.Writer, string, string, bool) error { return nil }))

	g := &apcmd.Globals{Namespace: b.Namespace, BundleFn: func() (*kube.Bundle, error) { return b, nil }}
	p := initWizardParams{
		strat:          cloud.MustFor(cloud.KeyLocal),
		platform:       "linux/amd64",
		timeout:        2 * time.Minute,
		acceptExisting: true,
		noIdp:          true,
		noMonitoring:   true,
		r:              WebdRoutingOpts{trustedHostname: "webd.example.com", sandboxHostname: "sandbox.example.com"},
	}
	err := runInitWizard(context.Background(), &bytes.Buffer{}, g, p)
	require.Error(t, err, "a non-TTY accept-all against an external SpiceDB must fail closed, never flip to bundled")
	assert.False(t, installCalled, "no install may run — that is what would overwrite the endpoint ConfigMap and orphan the external instance")
	assert.False(t, buildCalled, "the run fails before the build phase too")
}

// swap sets *p to v and returns a restore func for its previous value.
func swap[T any](p *T, v T) func() {
	prev := *p
	*p = v
	return func() { *p = prev }
}

// TestRunInitWizard_DriverIsInline is F6's regression: the wizard's single
// shared driver must be built with Inline: true, so its TTY driver never takes
// the alternate screen (pkg/cli/tui/run.go's Options.Inline rule) — the install
// checklist's build/status output streams between the wizard's screens on the
// same terminal buffer, including the post-install IdP/Monitoring screens that
// read the checklist output printed just above them. This wraps the REAL
// driverForInitWizard (tui.DriverFor) in a spy that records the DriverParams it
// was called with, so it fails against the un-fixed code (Inline defaulting to
// false) rather than only proving the spy itself is wired.
func TestRunInitWizard_DriverIsInline(t *testing.T) {
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "ap-workspace-fresh"},
		Provisioner: "cluster.local/ap-workspace-provisioner",
	}
	b := &kube.Bundle{
		Controller: fakeCtrlWith(t),
		Dynamic:    fakeDynNoIssuer(t),
		Typed:      k8sfake.NewSimpleClientset(sc),
		Namespace:  "agentprimitives-system",
	}
	p := initWizardParams{
		strat:          cloud.MustFor(cloud.KeyLocal),
		platform:       "linux/amd64",
		timeout:        2 * time.Minute,
		acceptExisting: false,
		noIdp:          true,
		noMonitoring:   true,
		r:              WebdRoutingOpts{trustedHostname: "webd.example.com", sandboxHostname: "sandbox.example.com"},
	}

	t.Cleanup(swap(&newInitWizardBundleFn, func(*apcmd.Globals) (*kube.Bundle, error) { return b, nil }))

	var gotParams []tui.DriverParams
	t.Cleanup(swap(&driverForInitWizard, func(dp tui.DriverParams) tui.Driver {
		gotParams = append(gotParams, dp)
		return tui.Plain(strings.NewReader(""), dp.Out, dp.Theme)
	}))
	t.Cleanup(swap(&runBuildFn, func(context.Context, io.Writer, string, string, bool, string, string, bool, bool, bool, *apcmd.Globals, progress.RailProvider) (map[string]string, error) {
		return map[string]string{}, nil
	}))
	t.Cleanup(swap(&runInstallFn, func(context.Context, context.Context, InstallConfig) error { return nil }))
	t.Cleanup(swap(&checksFn, func(context.Context, io.Writer, *kube.Bundle, time.Duration, progress.RailProvider) error { return nil }))
	t.Cleanup(swap(&runSettingsWizardFn, func(context.Context, *cobra.Command, *apcmd.Globals, bool, bool, string, map[string]string, settingscmd.DefaultModelOpts, tui.Driver) error {
		return nil
	}))
	t.Cleanup(swap(&runDirectoryConfigureFn, func(context.Context, io.Reader, io.Writer, *apcmd.Globals) error { return nil }))

	g := &apcmd.Globals{Namespace: b.Namespace, BundleFn: func() (*kube.Bundle, error) { return b, nil }}
	require.NoError(t, runInitWizard(context.Background(), &bytes.Buffer{}, g, p))

	require.NotEmpty(t, gotParams, "driverForInitWizard must be called at least once to build the wizard's shared driver")
	for i, dp := range gotParams {
		assert.Truef(t, dp.Inline, "driverForInitWizard call %d must carry Inline: true — the wizard's driver is shared with output streamed between screens", i)
	}
}

// TestRunInitWizard_SettingsPhaseGatedByTTY is M6's regression: a non-TTY
// wizard run (piped stdin, no --defaults) must not run the interactive
// settings wizard at all. huh's accessible renderer turns an EOF into each
// field's FABRICATED default with a nil error (pkg/cli/tui/driver.go), and
// RunWizardWithDriver composes and applies the result with no confirm — so an
// unguarded run would write a ClusterAgentSettings baseline nobody chose. This
// mirrors the linear path's own decideWizard gate (init.go): non-TTY without
// --defaults never runs the settings form either. --defaults instead takes an
// explicit non-interactive merge-with-existing path and must still run.
func TestRunInitWizard_SettingsPhaseGatedByTTY(t *testing.T) {
	newBundle := func() *kube.Bundle {
		return &kube.Bundle{
			Controller: fakeCtrlWith(t),
			Dynamic:    fakeDynNoIssuer(t),
			Typed:      k8sfake.NewSimpleClientset(),
			Namespace:  "agentprimitives-system",
		}
	}
	newParams := func(defaults bool) initWizardParams {
		return initWizardParams{
			strat:        cloud.MustFor(cloud.KeyLocal),
			platform:     "linux/amd64",
			timeout:      2 * time.Minute,
			noIdp:        true,
			noMonitoring: true,
			defaults:     defaults,
			r:            WebdRoutingOpts{trustedHostname: "webd.example.com", sandboxHostname: "sandbox.example.com"},
		}
	}
	driveWithSettingsSpy := func(t *testing.T, p initWizardParams) (bool, error) {
		t.Helper()
		b := newBundle()
		t.Cleanup(swap(&newInitWizardBundleFn, func(*apcmd.Globals) (*kube.Bundle, error) { return b, nil }))
		t.Cleanup(swap(&driverForInitWizard, func(dp tui.DriverParams) tui.Driver {
			return tui.Plain(strings.NewReader(""), dp.Out, dp.Theme)
		}))
		t.Cleanup(swap(&runBuildFn, func(context.Context, io.Writer, string, string, bool, string, string, bool, bool, bool, *apcmd.Globals, progress.RailProvider) (map[string]string, error) {
			return map[string]string{}, nil
		}))
		t.Cleanup(swap(&runInstallFn, func(context.Context, context.Context, InstallConfig) error { return nil }))
		t.Cleanup(swap(&checksFn, func(context.Context, io.Writer, *kube.Bundle, time.Duration, progress.RailProvider) error { return nil }))
		t.Cleanup(swap(&runDirectoryConfigureFn, func(context.Context, io.Reader, io.Writer, *apcmd.Globals) error { return nil }))
		called := false
		t.Cleanup(swap(&runSettingsWizardFn, func(context.Context, *cobra.Command, *apcmd.Globals, bool, bool, string, map[string]string, settingscmd.DefaultModelOpts, tui.Driver) error {
			called = true
			return nil
		}))

		g := &apcmd.Globals{Namespace: b.Namespace, BundleFn: func() (*kube.Bundle, error) { return b, nil }}
		err := runInitWizard(context.Background(), &bytes.Buffer{}, g, p)
		return called, err
	}

	t.Run("non-TTY, no --defaults: settings phase skipped", func(t *testing.T) {
		t.Cleanup(swap(&stdinInteractiveFn, func() bool { return false }))
		called, err := driveWithSettingsSpy(t, newParams(false))
		require.NoError(t, err)
		assert.False(t, called, "a non-TTY run with no --defaults must not run the interactive settings wizard")
	})

	t.Run("non-TTY with --defaults: settings phase still runs", func(t *testing.T) {
		t.Cleanup(swap(&stdinInteractiveFn, func() bool { return false }))
		called, err := driveWithSettingsSpy(t, newParams(true))
		require.NoError(t, err)
		assert.True(t, called, "--defaults must still apply the recommended baseline non-interactively")
	})

	t.Run("interactive TTY, no --defaults: settings phase runs", func(t *testing.T) {
		t.Cleanup(swap(&stdinInteractiveFn, func() bool { return true }))
		called, err := driveWithSettingsSpy(t, newParams(false))
		require.NoError(t, err)
		assert.True(t, called, "a genuinely interactive run must still offer the settings wizard")
	})
}

// TestRunInitWizard_IdPMonitoringGatedByInstallProfile is m1/F5's regression:
// the wizard's IdP/Monitoring phases were gated only on !p.noIdp/!p.noMonitoring,
// ignoring the resolved cluster's InstallProfile — so `oap init --local --wizard`
// presented both screens even though the linear local path
// (shouldPromptIdp/shouldPromptMonitoring, init.go) deliberately never offers
// them for the dev profile. Fresh cluster (nothing detected), noIdp/noMonitoring
// left false, so ONLY the profile gate can suppress the screens.
func TestRunInitWizard_IdPMonitoringGatedByInstallProfile(t *testing.T) {
	// Force interactivity: the IdP/Monitoring gates ALSO require a TTY (N4), and
	// this test isolates the PROFILE half of the gate — see
	// TestRunInitWizard_IdPMonitoringGatedByTTY for the non-TTY half.
	t.Cleanup(swap(&stdinInteractiveFn, func() bool { return true }))
	newBundle := func() *kube.Bundle {
		return &kube.Bundle{
			Controller: fakeCtrlWith(t), // fresh: no idp CR, no monitoring channel
			Dynamic:    fakeDynNoIssuer(t),
			Typed:      k8sfake.NewSimpleClientset(),
			Namespace:  "agentprimitives-system",
		}
	}
	newParams := func(strat cloud.Strategy) initWizardParams {
		return initWizardParams{
			strat:    strat,
			platform: "linux/amd64",
			timeout:  2 * time.Minute,
			r:        WebdRoutingOpts{trustedHostname: "webd.example.com", sandboxHostname: "sandbox.example.com"},
		}
	}

	t.Run("dev profile (--local): IdP/Monitoring screens never presented", func(t *testing.T) {
		out, got := driveInitWizardForRouting(t, newBundle(), newParams(cloud.MustFor(cloud.KeyLocal)))
		require.True(t, got.called, "install phase ran")
		assert.NotContains(t, out, "Connect an identity provider?", "a dev-profile cluster must not prompt for an identity provider")
		assert.NotContains(t, out, "Monitoring channel?", "a dev-profile cluster must not prompt for a monitoring channel")
	})

	t.Run("production profile (default kind): IdP/Monitoring screens presented", func(t *testing.T) {
		out, got := driveInitWizardForRouting(t, newBundle(), newParams(cloud.MustFor(cloud.KeyDefault)))
		require.True(t, got.called, "install phase ran")
		assert.Contains(t, out, "Connect an identity provider?", "a production-profile cluster must still offer identity-provider setup")
		assert.Contains(t, out, "Monitoring channel?", "a production-profile cluster must still offer monitoring setup")
	})
}

// TestRunInitWizard_IdPMonitoringGatedByTTY is N4's regression: the wizard's
// IdP/Monitoring phases were gated on the install profile only, dropping the
// linear shouldPromptIdp/shouldPromptMonitoring's `isTTY` conjunct — so a non-TTY
// `--accept-existing` run on a production-profile cluster still PRESENTED both
// screens over the Plain driver, where huh's accessible renderer fabricates an
// answer on EOF and a stray piped line could select a real kind. A production
// profile alone must no longer present the screens without a TTY.
func TestRunInitWizard_IdPMonitoringGatedByTTY(t *testing.T) {
	t.Cleanup(swap(&stdinInteractiveFn, func() bool { return false }))
	b := &kube.Bundle{
		Controller: fakeCtrlWith(t), // fresh: nothing detected, only the profile+TTY gate decides
		Dynamic:    fakeDynNoIssuer(t),
		Typed:      k8sfake.NewSimpleClientset(),
		Namespace:  "agentprimitives-system",
	}
	p := initWizardParams{
		strat:    cloud.MustFor(cloud.KeyDefault), // production profile: PromptsFor* true
		platform: "linux/amd64",
		timeout:  2 * time.Minute,
		r:        WebdRoutingOpts{trustedHostname: "webd.example.com", sandboxHostname: "sandbox.example.com"},
	}
	out, got := driveInitWizardForRouting(t, b, p)
	require.True(t, got.called, "install phase ran")
	assert.NotContains(t, out, "Connect an identity provider?", "a non-TTY run must not present the identity-provider screen even on a production profile")
	assert.NotContains(t, out, "Monitoring channel?", "a non-TTY run must not present the monitoring screen even on a production profile")
}

// TestRunInitWizard_DirectoryPhaseGatedByTTY is N2's regression: the restored
// directory-sync offer hardcoded wizardInteractive, dropping the linear
// non-TTY→skip and --defaults→skip mapping that runInitDirectoryScreenOffer's own
// doc comment declares a deliberate safety decision. The offer must now run the
// configure only on a genuine TTY without --defaults; a non-TTY (piped/CI) run
// and a --defaults run must both skip it (no fabricated directory-sync answer,
// no credential picked on the operator's behalf).
func TestRunInitWizard_DirectoryPhaseGatedByTTY(t *testing.T) {
	newBundle := func() *kube.Bundle {
		return &kube.Bundle{
			Controller: fakeCtrlWith(t),
			Dynamic:    fakeDynNoIssuer(t),
			Typed:      k8sfake.NewSimpleClientset(),
			Namespace:  "agentprimitives-system",
		}
	}
	newParams := func(defaults bool) initWizardParams {
		return initWizardParams{
			strat:        cloud.MustFor(cloud.KeyLocal),
			platform:     "linux/amd64",
			timeout:      2 * time.Minute,
			noIdp:        true,
			noMonitoring: true,
			defaults:     defaults,
			r:            WebdRoutingOpts{trustedHostname: "webd.example.com", sandboxHostname: "sandbox.example.com"},
		}
	}
	driveWithDirectorySpy := func(t *testing.T, p initWizardParams) (bool, error) {
		t.Helper()
		b := newBundle()
		t.Cleanup(swap(&newInitWizardBundleFn, func(*apcmd.Globals) (*kube.Bundle, error) { return b, nil }))
		t.Cleanup(swap(&driverForInitWizard, func(dp tui.DriverParams) tui.Driver {
			return tui.Plain(strings.NewReader(""), dp.Out, dp.Theme)
		}))
		t.Cleanup(swap(&runBuildFn, func(context.Context, io.Writer, string, string, bool, string, string, bool, bool, bool, *apcmd.Globals, progress.RailProvider) (map[string]string, error) {
			return map[string]string{}, nil
		}))
		t.Cleanup(swap(&runInstallFn, func(context.Context, context.Context, InstallConfig) error { return nil }))
		t.Cleanup(swap(&checksFn, func(context.Context, io.Writer, *kube.Bundle, time.Duration, progress.RailProvider) error { return nil }))
		// The settings phase runs on --defaults or a TTY too; stub it to a no-op so
		// only the directory spy is observed.
		t.Cleanup(swap(&runSettingsWizardFn, func(context.Context, *cobra.Command, *apcmd.Globals, bool, bool, string, map[string]string, settingscmd.DefaultModelOpts, tui.Driver) error {
			return nil
		}))
		called := false
		t.Cleanup(swap(&runDirectoryConfigureFn, func(context.Context, io.Reader, io.Writer, *apcmd.Globals) error {
			called = true
			return nil
		}))

		g := &apcmd.Globals{Namespace: b.Namespace, BundleFn: func() (*kube.Bundle, error) { return b, nil }}
		err := runInitWizard(context.Background(), &bytes.Buffer{}, g, p)
		return called, err
	}

	t.Run("non-TTY, no --defaults: directory offer skipped", func(t *testing.T) {
		t.Cleanup(swap(&stdinInteractiveFn, func() bool { return false }))
		called, err := driveWithDirectorySpy(t, newParams(false))
		require.NoError(t, err)
		assert.False(t, called, "a non-TTY run with no --defaults must not run the directory-sync configure (no fabricated answer)")
	})

	t.Run("--defaults on a TTY: directory offer still skipped", func(t *testing.T) {
		t.Cleanup(swap(&stdinInteractiveFn, func() bool { return true }))
		called, err := driveWithDirectorySpy(t, newParams(true))
		require.NoError(t, err)
		assert.False(t, called, "--defaults has no recommended directories to sync; the offer must skip, matching the linear wizardDefaults arm")
	})

	t.Run("interactive TTY, no --defaults: directory offer runs", func(t *testing.T) {
		t.Cleanup(swap(&stdinInteractiveFn, func() bool { return true }))
		called, err := driveWithDirectorySpy(t, newParams(false))
		require.NoError(t, err)
		assert.True(t, called, "a genuinely interactive run must still offer directory-sync configure (M5 parity)")
	})
}

// TestRunInitWizard_ProceedDeclinePrintsAbortedMessage is m4's regression: a
// user who declines the final Proceed confirm saw NOTHING — `oap init` exited
// 0 with no statement that nothing was applied, violating the no-silent-
// outcomes rule. Uses the same fully-detected fixture as
// TestRunInitWizard_AcceptAll_SkipsConfigPrompts (marker + idp + monitoring,
// --accept-existing) so every config screen up through SpiceDB short-circuits;
// only the (unseeded) SpiceDB confirm and the Proceed confirm need scripted
// answers.
func TestRunInitWizard_ProceedDeclinePrintsAbortedMessage(t *testing.T) {
	marker := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: workspaceMarkerName, Namespace: workspaceMarkerNamespace},
		Data:       map[string]string{workspaceMarkerKey: "ap-workspace-rwx"},
	}
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "ap-workspace-rwx"},
		Provisioner: "cluster.local/ap-workspace-provisioner",
	}
	b := &kube.Bundle{
		Controller: fakeCtrlWith(t, idpCR("google"), monitoringChannel("demo-mon", "slack")),
		Dynamic:    fakeDynWithIssuer(t, "ops@example.com"),
		Typed:      k8sfake.NewSimpleClientset(marker, sc),
		Namespace:  "agentprimitives-system",
	}
	t.Cleanup(swap(&newInitWizardBundleFn, func(*apcmd.Globals) (*kube.Bundle, error) { return b, nil }))
	// "n" declines the (unseeded) SpiceDB confirm — matching its own default —
	// then "n" declines Proceed, which is what this test is about.
	t.Cleanup(swap(&driverForInitWizard, func(dp tui.DriverParams) tui.Driver {
		return tui.Plain(strings.NewReader("n\nn\n"), dp.Out, dp.Theme)
	}))
	buildCalled := false
	t.Cleanup(swap(&runBuildFn, func(context.Context, io.Writer, string, string, bool, string, string, bool, bool, bool, *apcmd.Globals, progress.RailProvider) (map[string]string, error) {
		buildCalled = true
		return map[string]string{}, nil
	}))
	installCalled := false
	t.Cleanup(swap(&runInstallFn, func(context.Context, context.Context, InstallConfig) error {
		installCalled = true
		return nil
	}))

	g := &apcmd.Globals{Namespace: b.Namespace, BundleFn: func() (*kube.Bundle, error) { return b, nil }}
	var out bytes.Buffer
	p := initWizardParams{
		strat:          cloud.MustFor(cloud.KeyLocal),
		platform:       "linux/amd64",
		timeout:        2 * time.Minute,
		acceptExisting: true,
	}
	require.NoError(t, runInitWizard(context.Background(), &out, g, p))

	assert.Contains(t, out.String(), "aborted; nothing was changed.", "a Proceed decline must print something, not exit silently")
	assert.False(t, buildCalled, "a decline must never reach the build phase")
	assert.False(t, installCalled, "a decline must never reach the install phase")
}

// TestRunInitWizard_RWXClassListErrorWarnsAndFallsThrough is F8's regression:
// `classes, _ := cloud.ListRWXClasses(...)` silently dropped a list failure.
// It must now warn (no-silent-errors) while still falling through to
// RunInstall's own workspace detection, exactly as an empty result already did
// before this fix — a list error must not make the wizard itself fail.
func TestRunInitWizard_RWXClassListErrorWarnsAndFallsThrough(t *testing.T) {
	b := &kube.Bundle{
		Controller: fakeCtrlWith(t),
		Dynamic:    fakeDynNoIssuer(t),
		Typed:      k8sfake.NewSimpleClientset(),
		Namespace:  "agentprimitives-system",
	}
	listErr := errors.New("storageclasses list boom")
	b.Typed.(*k8sfake.Clientset).PrependReactor("list", "storageclasses", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, listErr
	})

	out, got := driveInitWizardForRouting(t, b, initWizardParams{
		strat:        cloud.MustFor(cloud.KeyLocal),
		platform:     "linux/amd64",
		timeout:      2 * time.Minute,
		noIdp:        true,
		noMonitoring: true,
		r:            WebdRoutingOpts{trustedHostname: "webd.example.com", sandboxHostname: "sandbox.example.com"},
	})

	require.True(t, got.called, "install phase ran despite the list error")
	assert.Contains(t, out, "list RWX-capable storage classes", "the dropped error must be warned, not silently swallowed")
	assert.Equal(t, "", got.wsOpts.ExplicitClass, "with no classes listed, the workspace decision still falls through to RunInstall's own detection")
}
