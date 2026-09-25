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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/settingscmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

func TestInitHelpDescribesComposition(t *testing.T) {
	root := newRoot(t)
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"init", "--help"})
	require.NoError(t, root.Execute(), "init --help")

	out := stdout.String()
	assert.Contains(t, out, "build", "init help should mention build")
	assert.Contains(t, out, "install", "init help should mention install")
	assert.Contains(t, out, "check", "init help should mention check")
}

func TestInitFlagsCompose(t *testing.T) {
	root := newRoot(t)
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"init", "--skip-build", "--skip-install", "--help"})
	require.NoError(t, root.Execute(), "flags should parse")
}

func TestInitWizardDecision(t *testing.T) {
	assert.Equal(t, wizardDefaults, decideWizard(true /*defaults*/, false, false /*tty*/))
	assert.Equal(t, wizardInteractive, decideWizard(false, true /*wizard*/, false))
	assert.Equal(t, wizardSkip, decideWizard(false, false, false /*non-tty*/))
	assert.Equal(t, wizardOffer, decideWizard(false, false, true /*tty*/))
}

// TestRunInit_DispatchesToWizard pins the runInit → wizard dispatch: --wizard
// and --accept-existing each route the run through runInitWizardFn (the unified
// orchestrator), while neither flag keeps the linear build/install/status path.
//
// Everything downstream of the branch is a package-var seam (runInitWizardFn for
// the wizard, runBuildFn/runInstallFn/checksFn for the linear tail), so the test
// reaches the real dispatch code with only a fake, empty --local cluster and no
// images. The linear checks spy returns an error so the non-wizard case stops at
// the check step rather than continuing into the post-install offers, which read
// a live TTY — the branch, not the tail, is what this proves.
func TestRunInit_DispatchesToWizard(t *testing.T) {
	// A SPICEDB_ENDPOINT in the ambient env would send the linear path down a
	// live schema-apply; blank it so the branch is all this exercises.
	t.Setenv("SPICEDB_ENDPOINT", "")

	calledWizard := 0
	t.Cleanup(swap(&runInitWizardFn, func(context.Context, io.Writer, *apcmd.Globals, initWizardParams) error {
		calledWizard++
		return nil
	}))

	linearBuild, linearInstall := 0, 0
	t.Cleanup(swap(&runBuildFn, func(context.Context, io.Writer, string, string, bool, string, string, bool, bool, bool, *apcmd.Globals, progress.RailProvider) (map[string]string, error) {
		linearBuild++
		return nil, nil
	}))
	t.Cleanup(swap(&runInstallFn, func(_, _ context.Context, _ InstallConfig) error {
		linearInstall++
		return nil
	}))
	errLinearReached := errors.New("linear checks reached")
	t.Cleanup(swap(&checksFn, func(context.Context, io.Writer, *kube.Bundle, time.Duration, progress.RailProvider) error {
		return errLinearReached
	}))

	// A fake, empty cluster: resolveClusterKind(--local, allowNonLocal) validates
	// (no managed providerID, override on), and imagemode resolves to local :dev
	// images — so the run reaches the dispatch with no registry work.
	b := &kube.Bundle{Typed: k8sfake.NewSimpleClientset(), Namespace: "agentprimitives-system"}
	g := &apcmd.Globals{Namespace: b.Namespace, BundleFn: func() (*kube.Bundle, error) { return b, nil }}

	call := func(wizard, accept bool) error {
		return runInit(context.Background(), &bytes.Buffer{}, g,
			false /*skipBuild*/, false /*skipInstall*/, 2*time.Minute, manifests.Tags{},
			"" /*pinningMode*/, cloud.KeyLocal, true /*allowNonLocal*/, WebdRoutingOpts{},
			wizard, false /*defaults*/, "" /*imagePullSecret*/, false /*mirrorDeps*/, false /*createRegistry*/, false /*noIdp*/, false, /*noMonitoring*/
			settingscmd.DefaultModelOpts{}, "" /*artifactStoreURL*/, "" /*extEndpoint*/, "" /*extToken*/, false /*extInsecure*/, accept)
	}

	// --wizard dispatches to the orchestrator exactly once; the linear phases run not at all.
	require.NoError(t, call(true /*wizard*/, false /*accept*/))
	assert.Equal(t, 1, calledWizard, "--wizard dispatches to the unified wizard")
	assert.Equal(t, 0, linearInstall, "the wizard path must not run the linear install")

	// --accept-existing (no --wizard) also dispatches, since it needs detection + seeding.
	require.NoError(t, call(false /*wizard*/, true /*accept*/))
	assert.Equal(t, 2, calledWizard, "--accept-existing also dispatches to the unified wizard")

	// Neither flag: the linear build/install/check path runs and the wizard is not called.
	err := call(false /*wizard*/, false /*accept*/)
	require.ErrorIs(t, err, errLinearReached, "with neither flag, runInit takes the linear build/install/check route")
	assert.Equal(t, 2, calledWizard, "no flag → no wizard dispatch")
	assert.Equal(t, 1, linearBuild, "the linear path built")
	assert.Equal(t, 1, linearInstall, "the linear path installed")
}

// TestRunInit_WizardPathSkipsPreflightRoutingRefusal is N1's regression: on a
// managed cloud (gke, RequiresExternalHostname=true) the flag-only routing
// preflight (validateWebdRouting over the raw FLAG values) refused
// `oap init --accept-existing`/`--wizard` when no --trusted-hostname was
// re-passed — before detection could seed the cluster's installed HTTPRoute
// hostnames — so the wizard was never dispatched. The wizard path must now skip
// that pre-dispatch check and reach the dispatch; the LINEAR path (neither flag)
// must still refuse, unchanged.
func TestRunInit_WizardPathSkipsPreflightRoutingRefusal(t *testing.T) {
	// A registry set on the tags makes imagemode.Resolve return without a remote
	// registry round-trip, so the managed-cloud run reaches the dispatch/refusal
	// point with no image work.
	tags := manifests.Tags{Registry: "reg.example/oap"}

	calledWizard := 0
	t.Cleanup(swap(&runInitWizardFn, func(context.Context, io.Writer, *apcmd.Globals, initWizardParams) error {
		calledWizard++
		return nil
	}))
	// Stub the linear tail so a regression that let the linear path proceed past
	// the refusal cannot reach real image/cluster work.
	t.Cleanup(swap(&runBuildFn, func(context.Context, io.Writer, string, string, bool, string, string, bool, bool, bool, *apcmd.Globals, progress.RailProvider) (map[string]string, error) {
		return nil, nil
	}))
	t.Cleanup(swap(&runInstallFn, func(_, _ context.Context, _ InstallConfig) error { return nil }))
	t.Cleanup(swap(&checksFn, func(context.Context, io.Writer, *kube.Bundle, time.Duration, progress.RailProvider) error { return nil }))

	// A fake, empty gke cluster: no nodes report a providerID, so gke's
	// ValidateManagedPrefix passes (nothing contradicts --cluster-kind=gke).
	b := &kube.Bundle{Typed: k8sfake.NewSimpleClientset(), Namespace: "agentprimitives-system"}
	g := &apcmd.Globals{Namespace: b.Namespace, BundleFn: func() (*kube.Bundle, error) { return b, nil }}

	// No routing flags: an empty WebdRoutingOpts, the flag's advertised re-run case.
	call := func(wizard, accept bool) error {
		return runInit(context.Background(), &bytes.Buffer{}, g,
			false /*skipBuild*/, false /*skipInstall*/, 2*time.Minute, tags,
			"" /*pinningMode*/, cloud.KeyGKE, false /*allowNonLocal*/, WebdRoutingOpts{},
			wizard, false /*defaults*/, "" /*imagePullSecret*/, false /*mirrorDeps*/, false /*createRegistry*/, false /*noIdp*/, false, /*noMonitoring*/
			settingscmd.DefaultModelOpts{}, "" /*artifactStoreURL*/, "" /*extEndpoint*/, "" /*extToken*/, false /*extInsecure*/, accept)
	}

	// --accept-existing on gke with no routing flags reaches the wizard dispatch
	// (detection will supply the hostname) rather than refusing at the preflight.
	require.NoError(t, call(false /*wizard*/, true /*accept*/), "--accept-existing on a managed cloud must not be refused by the pre-dispatch routing preflight")
	assert.Equal(t, 1, calledWizard, "--accept-existing dispatches to the wizard despite the empty flag hostname")

	// --wizard alone (no --accept-existing) is likewise dispatched, not refused.
	require.NoError(t, call(true /*wizard*/, false /*accept*/), "--wizard on a managed cloud must not be refused by the pre-dispatch routing preflight")
	assert.Equal(t, 2, calledWizard, "--wizard also dispatches despite the empty flag hostname")

	// The LINEAR path (neither flag) still refuses: the flag-only preflight is
	// correct there, unchanged.
	err := call(false /*wizard*/, false /*accept*/)
	require.Error(t, err, "the linear path keeps the managed-cloud routing refusal")
	assert.Contains(t, err.Error(), "without an external hostname", "the refusal is the flag-only routing preflight")
	assert.Equal(t, 2, calledWizard, "the linear path never dispatches to the wizard")
}

func TestValidateInitFlags(t *testing.T) {
	cases := []struct {
		name           string
		localMode      bool
		clusterKind    string
		develop        bool
		extEP          string
		extToken       string
		acceptExisting bool
		skipInstall    bool
		wantErr        bool
		wantSubstr     string
	}{
		{name: "--local --develop: rejected (mixed content)", localMode: true, develop: true, wantErr: true, wantSubstr: "--develop cannot be combined with --local"},
		{name: "--local alone: allowed", localMode: true, develop: false, wantErr: false},
		{name: "--develop alone: allowed", localMode: false, develop: true, wantErr: false},
		{name: "neither: allowed", localMode: false, develop: false, wantErr: false},
		{name: "--local --cluster-kind=gke: rejected (conflicting)", localMode: true, clusterKind: cloud.KeyGKE, wantErr: true, wantSubstr: "--local sets --cluster-kind=local"},
		{name: "--local --cluster-kind=local: allowed (redundant, not conflicting)", localMode: true, clusterKind: cloud.KeyLocal, wantErr: false},
		{name: "--cluster-kind=gke alone (no --local): allowed", clusterKind: cloud.KeyGKE, wantErr: false},
		{name: "--cluster-kind=local --develop (no --local): rejected (mixed content)", localMode: false, clusterKind: cloud.KeyLocal, develop: true, wantErr: true, wantSubstr: "--develop cannot be combined with --local"},
		{name: "--external-spicedb-token without --external-spicedb-endpoint: rejected", extToken: "tok", wantErr: true, wantSubstr: "--external-spicedb-token requires --external-spicedb-endpoint"},
		{name: "--external-spicedb-endpoint alone: allowed", extEP: "spicedb.example:443", wantErr: false},
		{name: "--external-spicedb-endpoint + --external-spicedb-token: allowed", extEP: "spicedb.example:443", extToken: "tok", wantErr: false},
		{name: "--accept-existing --skip-install: rejected (m5, silently no-ops)", acceptExisting: true, skipInstall: true, wantErr: true, wantSubstr: "--accept-existing requires an install"},
		{name: "--accept-existing alone: allowed", acceptExisting: true, wantErr: false},
		{name: "--skip-install alone: allowed", skipInstall: true, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInitFlags(tc.localMode, tc.clusterKind, tc.develop, tc.extEP, tc.extToken, tc.acceptExisting, tc.skipInstall)
			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantSubstr)
		})
	}
}

func TestInitNoIdpFlagParses(t *testing.T) {
	root := newRoot(t)
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"init", "--no-idp", "--help"})
	require.NoError(t, root.Execute(), "--no-idp should be a valid init flag")
}

func TestInitNoMonitoringFlagParses(t *testing.T) {
	root := newRoot(t)
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"init", "--no-monitoring", "--help"})
	require.NoError(t, root.Execute(), "--no-monitoring should be a valid init flag")
}

// TestInitDefaultModelFlagsParse verifies `oap init` registers the same seven
// --default-model* flags as `oap settings wizard`, via the shared
// registerDefaultModelFlags helper (Task 3) — so init can drive the
// cluster-default-model flow non-interactively (--defaults --default-model=...)
// without a separate `oap settings wizard` invocation.
func TestInitDefaultModelFlagsParse(t *testing.T) {
	root := newRoot(t)
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"init", "--default-model", "claude-sonnet-5", "--default-model-provider", "anthropic", "--help"})
	require.NoError(t, root.Execute(), "--default-model* flags should be valid init flags")
}

// TestRunInitWizardOfferThreadsDefaultModel verifies the init→wizard seam
// (runInitWizardOffer) forwards a populated settingscmd.DefaultModelOpts into
// runSettingsWizard: the resolved token lands in the adoption-labelled central
// Secret, and the composed ClusterAgentSettings carries the default catalog
// entry pointing at it. Mirrors settings_wizard_test.go's
// TestRunSettingsWizard_DefaultModelNonInteractive, but driven through the
// `oap init` seam rather than `oap settings wizard` directly.
func TestRunInitWizardOfferThreadsDefaultModel(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	prev := settingscmd.DynamicFactory
	t.Cleanup(func() { settingscmd.DynamicFactory = prev })
	settingscmd.DynamicFactory = func(*apcmd.Globals) (settingscmd.DynIface, error) { return dyn, nil }

	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	dm := settingscmd.DefaultModelOpts{
		Model: "claude-sonnet-5", Provider: "anthropic", TokenEnv: "ANTHROPIC_API_KEY",
		SecretName: "model-default-token", SecretNamespace: "agentprimitives-system", SecretKey: "token",
	}

	// The wizard's defaults path reads the existing ClusterAgentSettings
	// through g.Bundle().Controller before composing, so the Globals needs a
	// fake-backed Bundle (empty cluster) — a bare &apcmd.Globals{} would do
	// LIVE kubeconfig discovery against the ambient context from a unit test.
	// Mirrors settingscmd's settingsGlobals(t, nil) construction (unexported,
	// test-only there — duplicated rather than exported for one caller).
	ctrlScheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(ctrlScheme))
	require.NoError(t, corev1.AddToScheme(ctrlScheme))
	b := &kube.Bundle{
		Controller: fake.NewClientBuilder().WithScheme(ctrlScheme).Build(),
		Dynamic:    dyn,
		Namespace:  "default",
	}
	g := &apcmd.Globals{Namespace: b.Namespace, BundleFn: func() (*kube.Bundle, error) { return b, nil }}

	var out bytes.Buffer
	err := runInitWizardOffer(context.Background(), &out, g, wizardDefaults, "" /*registry*/, nil /*digests*/, dm)
	require.NoError(t, err)

	secretGVR := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	sec, err := dyn.Resource(secretGVR).Namespace("agentprimitives-system").Get(context.Background(), "model-default-token", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "true", sec.GetLabels()[adoptguard.AdoptedLabel])

	cas, err := dyn.Resource(aptest.ClusterAgentSettingsGVR).Get(context.Background(), "cluster", metav1.GetOptions{})
	require.NoError(t, err)
	cat, _, _ := unstructured.NestedSlice(cas.Object, "spec", "modelCatalog")
	require.Len(t, cat, 1)
	e := cat[0].(map[string]any)
	assert.Equal(t, "claude-sonnet-5", e["name"])
	assert.Equal(t, true, e["default"])
	assert.Equal(t, "model-default-token", e["tokenRef"].(map[string]any)["name"])
}

func TestIdpOfferOptions_DefaultsToCurrentKind(t *testing.T) {
	labels, defIdx := idpOfferOptions([]string{"google", "oidc"}, "oidc")
	// options are: google, oidc, none  → oidc is index 2 (1-based).
	require.Len(t, labels, 3)
	assert.Contains(t, labels[1], "oidc")
	assert.Contains(t, labels[1], "(current)")
	assert.Equal(t, 2, defIdx, "default points at the current kind")
}

func TestIdpOfferOptions_NoCurrentDefaultsToNone(t *testing.T) {
	labels, defIdx := idpOfferOptions([]string{"google", "oidc"}, "")
	require.Len(t, labels, 3)
	assert.Equal(t, 3, defIdx, "default points at none when nothing configured")
	assert.NotContains(t, labels[0], "(current)")
}

func monOfferChan(name, kind string) spiceboxv1alpha1.Channel {
	return spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spiceboxv1alpha1.ChannelSpec{Role: spiceboxv1alpha1.ChannelRoleMonitoring, Kind: kind},
	}
}

func TestMonitoringOfferOptions_NoneExistingDefaultsToSkip(t *testing.T) {
	labels, defIdx := monitoringOfferOptions(nil)
	require.Len(t, labels, 2)
	assert.Contains(t, labels[0], "set up")
	assert.Contains(t, labels[1], "skip")
	assert.Equal(t, 2, defIdx, "default is skip when nothing exists")
}

func TestMonitoringOfferOptions_OneExistingDefaultsToKeepNamed(t *testing.T) {
	labels, defIdx := monitoringOfferOptions([]spiceboxv1alpha1.Channel{monOfferChan("team-alerts", "slack")})
	require.Len(t, labels, 3)
	assert.Contains(t, labels[0], "keep")
	assert.Contains(t, labels[0], "team-alerts")
	assert.Contains(t, labels[0], "slack")
	assert.Contains(t, labels[2], "skip")
	assert.Equal(t, 1, defIdx, "default is keep when one exists")
}

func TestMonitoringOfferOptions_MultipleExistingUsesPluralLabel(t *testing.T) {
	labels, defIdx := monitoringOfferOptions([]spiceboxv1alpha1.Channel{
		monOfferChan("team-alerts", "slack"),
		monOfferChan("ops", "slack"),
	})
	require.Len(t, labels, 3)
	assert.Contains(t, labels[0], "keep")
	assert.Contains(t, labels[0], "channel(s)")
	assert.NotContains(t, labels[0], "team-alerts", "no single name when multiple exist")
	assert.Equal(t, 1, defIdx)
}

func newMonitoringOfferScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

func TestRunInitMonitoringOffer_KeepExistingIsDefaultNoop(t *testing.T) {
	existing := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "team-alerts", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Role: spiceboxv1alpha1.ChannelRoleMonitoring, Kind: "slack"},
	}
	c := fake.NewClientBuilder().WithScheme(newMonitoringOfferScheme(t)).WithObjects(existing).Build()

	var out bytes.Buffer
	// Empty stdin → EOF → default selection, which is "keep" when one exists.
	err := runInitMonitoringOffer(context.Background(), strings.NewReader(""), &out, &apcmd.Globals{Namespace: "default"}, c, "default")
	require.NoError(t, err)

	s := out.String()
	assert.Contains(t, s, "keep using existing", "menu shows the keep option")
	assert.Contains(t, s, `Keeping existing monitoring channel "team-alerts"`, "keep prints a confirmation")
	assert.NotContains(t, s, "--- Channel ---", "keep must NOT run the wizard / apply anything")
	assert.NotContains(t, s, "Skipping monitoring channel setup", "keep is not skip")
}

func TestRunInitMonitoringOffer_NoExistingDefaultsToSkip(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newMonitoringOfferScheme(t)).Build() // no channels

	var out bytes.Buffer
	err := runInitMonitoringOffer(context.Background(), strings.NewReader(""), &out, &apcmd.Globals{Namespace: "default"}, c, "default")
	require.NoError(t, err)

	s := out.String()
	assert.Contains(t, s, "1) set up a monitoring channel", "menu shown even when none exist")
	assert.Contains(t, s, "Skipping monitoring channel setup", "default is skip when none exist")
	assert.NotContains(t, s, "keep using existing", "no keep option when none exist")
}

func TestRunInitMonitoringOffer_ExplicitSkipWithExisting(t *testing.T) {
	existing := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "team-alerts", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Role: spiceboxv1alpha1.ChannelRoleMonitoring, Kind: "slack"},
	}
	c := fake.NewClientBuilder().WithScheme(newMonitoringOfferScheme(t)).WithObjects(existing).Build()

	var out bytes.Buffer
	// One existing → options are 1=keep, 2=set up, 3=skip. Choose 3.
	err := runInitMonitoringOffer(context.Background(), strings.NewReader("3\n"), &out, &apcmd.Globals{Namespace: "default"}, c, "default")
	require.NoError(t, err)

	s := out.String()
	assert.Contains(t, s, "Skipping monitoring channel setup")
	assert.NotContains(t, s, "Keeping existing monitoring channel")
}

// runInitDirectoryOffer drives runInitDirectoryScreenOffer in mode with no
// input (empty stdin — the wizardSkip/wizardDefaults cases never read it, and
// wizardOffer with no input scans to EOF, same as a closed pipe would),
// substituting a spy for runDirectoryConfigureFn so the test can observe
// whether the directory-configure flow was invoked without driving the full
// interactive TUI or a real cluster.
func runInitDirectoryOffer(t *testing.T, mode wizardMode, applied func()) {
	t.Helper()
	runInitDirectoryOfferWithInput(t, mode, "", applied)
}

// runInitDirectoryOfferWithInput is runInitDirectoryOffer, but over canned
// stdin content — for exercising wizardOffer's own Y/n prompt.
func runInitDirectoryOfferWithInput(t *testing.T, mode wizardMode, input string, applied func()) {
	t.Helper()
	prev := runDirectoryConfigureFn
	t.Cleanup(func() { runDirectoryConfigureFn = prev })
	runDirectoryConfigureFn = func(context.Context, io.Reader, io.Writer, *apcmd.Globals) error {
		applied()
		return nil
	}

	var out bytes.Buffer
	err := runInitDirectoryScreenOffer(context.Background(), strings.NewReader(input), &out, &apcmd.Globals{}, mode)
	require.NoError(t, err)
}

// A CI install must never block on a prompt.
func TestInitDirectoryScreen_SkippedWhenNotATTY(t *testing.T) {
	called := false
	runInitDirectoryOffer(t, wizardSkip, func() { called = true })
	assert.False(t, called)
}

// --defaults must configure NO directories: there is no safe default for
// which orgs to sync, and it writes authorization data.
func TestInitDirectoryScreen_DefaultsConfiguresNothing(t *testing.T) {
	called := false
	runInitDirectoryOffer(t, wizardDefaults, func() { called = true })
	assert.False(t, called, "--defaults must not silently start a directory sync")
}

// On a TTY the operator is offered the screen and may decline.
func TestInitDirectoryScreen_OfferedOnATTYAndDeclinable(t *testing.T) {
	called := false
	runInitDirectoryOfferWithInput(t, wizardOffer, "n\n", func() { called = true })
	assert.False(t, called)
}

func TestInitDirectoryScreen_RunsWhenAccepted(t *testing.T) {
	called := false
	runInitDirectoryOfferWithInput(t, wizardOffer, "y\n", func() { called = true })
	assert.True(t, called)
}

// checksFailingBundle is a fake kube.Bundle with none of runCheck's required
// CRDs/services, so every poll fails deterministically and fast — no real
// cluster call ever succeeds against it.
func checksFailingBundle(t *testing.T) *kube.Bundle {
	t.Helper()
	return &kube.Bundle{
		Controller: fakeCtrlWith(t),
		Typed:      k8sfake.NewSimpleClientset(),
		Namespace:  "agentprimitives-system",
	}
}

// TestWaitUntilChecksPass_RailPath is F5's plumbing proof for the Status
// phase: with a non-nil rail, waitUntilChecksPass drives the wait through a
// rail-aware Phase (AwaitOptional) instead of the raw fmt.Fprintln the
// nil-rail path uses below, and it must still surface a persistently failing
// check as an error once the deadline elapses — AwaitOptional never consults
// the interactive keep-waiting prompt, so this can't hang on stdin. The short
// timeout keeps the test fast: pollUntilDeadline's own context carries it, so
// the failure surfaces at the timeout rather than at Await's 2s backoff.
func TestWaitUntilChecksPass_RailPath(t *testing.T) {
	b := checksFailingBundle(t)
	rail := &fakeRail{steps: []tui.Step{{ID: "checks", Label: "Status"}}, active: 0}

	var out bytes.Buffer
	err := waitUntilChecksPass(context.Background(), &out, b, 100*time.Millisecond, rail)
	require.Error(t, err, "an always-failing check must surface an error once the deadline elapses")
	assert.NotContains(t, out.String(), "all checks passed", "a failing run must not print the success line")
}

// TestWaitUntilChecksPass_NilRailUnchanged pins the byte-identical contract: a
// nil rail keeps waitUntilChecksPass on the original wait.Until path, returning
// wait.Until's own timeout sentinel — it would fail if that branch were ever
// routed through the rail-aware Phase (which returns a differently-shaped
// error) by mistake.
func TestWaitUntilChecksPass_NilRailUnchanged(t *testing.T) {
	b := checksFailingBundle(t)

	var out bytes.Buffer
	err := waitUntilChecksPass(context.Background(), &out, b, 100*time.Millisecond, nil)
	require.Error(t, err, "an always-failing check must surface an error once the timeout elapses")
	assert.ErrorIs(t, err, wait.ErrTimeout, "the nil-rail path is exactly wait.Until, unchanged")
}
