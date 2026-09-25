package settingscmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/settingswizard"
	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"

	// The wizard's pinning-ceiling screen enumerates the pinning-kind
	// registry; oap registers every kind from main, so these assertions need
	// the same set.
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/cli"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/image"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/mcp"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/oap"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/skill"
	pinningregistry "github.com/authzed/openagentprimitives/pkg/authz/pinning/registry"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
)

// TestSettingsWizard_defaultsAppliesBaselineNoTUI verifies the --defaults path:
// it must compose BaselineSelections() and apply the resulting ClusterAgentSettings
// without asking anything.
func TestSettingsWizard_defaultsAppliesBaselineNoTUI(t *testing.T) {
	// A fake-backed Bundle (empty cluster) plus the dynamic client the apply
	// lands in: the --defaults path now reads existing settings through
	// g.Bundle().Controller before composing.
	g, dyn := settingsGlobals(t, nil)
	prev := DynamicFactory
	defer func() { DynamicFactory = prev }()
	DynamicFactory = func(_ *apcmd.Globals) (DynIface, error) {
		return dyn, nil
	}

	out, err := runSettingsG(t, g, "wizard", "--defaults")
	require.NoErrorf(t, err, "settings wizard --defaults; out=%s", out)

	// The wizard should print a preview mentioning ClusterAgentSettings.
	assert.Contains(t, out, "ClusterAgentSettings", "output should preview ClusterAgentSettings kind")
	assert.Contains(t, out, "cluster", "output should mention the singleton name")

	// The object should exist in the fake cluster.
	got, getErr := dyn.Resource(aptest.ClusterAgentSettingsGVR).Get(context.Background(), "cluster", metav1.GetOptions{})
	require.NoError(t, getErr, "ClusterAgentSettings 'cluster' should exist after --defaults apply")
	assert.Equal(t, "cluster", got.GetName())

	// Confirm the applied spec reflects Compose(BaselineSelections()).
	// BaselineSelections enables Breaker+RateLimit+DataLimit+Pinning, no content inspectors.
	spec, ok := got.Object["spec"].(map[string]any)
	require.True(t, ok, "spec should be a map")
	defaults, ok := spec["defaults"].(map[string]any)
	require.True(t, ok, "spec.defaults should be present")
	toolGuard, ok := defaults["toolGuard"].(map[string]any)
	require.True(t, ok, "spec.defaults.toolGuard should be present")
	rules, ok := toolGuard["rules"].([]any)
	require.True(t, ok, "spec.defaults.toolGuard.rules should be a slice")
	require.NotEmpty(t, rules, "should have at least one tool-guard rule")
}

// TestSettingsWizard_defaultsDryRunDoesNotApply verifies --defaults --dry-run
// prints a preview but does NOT apply the object to the cluster.
func TestSettingsWizard_defaultsDryRunDoesNotApply(t *testing.T) {
	g, dyn := settingsGlobals(t, nil)

	prev := DynamicFactory
	defer func() { DynamicFactory = prev }()
	DynamicFactory = func(_ *apcmd.Globals) (DynIface, error) {
		return dyn, nil
	}

	out, err := runSettingsG(t, g, "wizard", "--defaults", "--dry-run")
	require.NoErrorf(t, err, "settings wizard --defaults --dry-run; out=%s", out)

	// Preview should still be printed.
	assert.Contains(t, out, "ClusterAgentSettings", "dry-run should print preview")

	// The object must NOT exist in the fake cluster.
	_, getErr := dyn.Resource(aptest.ClusterAgentSettingsGVR).Get(context.Background(), "cluster", metav1.GetOptions{})
	assert.Error(t, getErr, "ClusterAgentSettings should NOT exist after dry-run")
}

// TestSettingsWizard_defaultsComposesBaselineSelections verifies that the
// --defaults output payload exactly matches Compose(BaselineSelections()).
func TestSettingsWizard_defaultsComposesBaselineSelections(t *testing.T) {
	g, dyn := settingsGlobals(t, nil)

	prev := DynamicFactory
	defer func() { DynamicFactory = prev }()
	DynamicFactory = func(_ *apcmd.Globals) (DynIface, error) {
		return dyn, nil
	}

	_, err := runSettingsG(t, g, "wizard", "--defaults")
	require.NoError(t, err)

	got, getErr := dyn.Resource(aptest.ClusterAgentSettingsGVR).Get(context.Background(), "cluster", metav1.GetOptions{})
	require.NoError(t, getErr)

	// Round-trip through JSON to get a plain map for comparison.
	gotJSON, err := json.Marshal(got.Object)
	require.NoError(t, err)

	// Confirm no content-inspector ceiling (baseline has none).
	spec, _ := got.Object["spec"].(map[string]any)
	limits, _ := spec["limits"].(map[string]any)
	// limits should exist (pinning) but contentInspectors should be absent.
	if limits != nil {
		_, hasCI := limits["contentInspectors"]
		assert.False(t, hasCI, "baseline should carry no content-inspector ceiling; got: %s", gotJSON)
	}
}

// TestDefaultDetectorImageIsLocal guards the fix for the prompt-injection
// detector to default to the locally-built image, not a registry path the
// cluster may not be able to pull.
func TestDefaultDetectorImageIsLocal(t *testing.T) {
	// The wizard must default the prompt-injection detector to the locally
	// built image, not a registry path the cluster may not be able to pull.
	assert.Equal(t, "pi-detector:dev", defaultDetectorImage)
	assert.NotContains(t, defaultDetectorImage, "ghcr.io",
		"detector default must be local; a registry is opt-in via --image-registry")
}

func TestDetectorImageDefault(t *testing.T) {
	// local: no registry → :dev
	assert.Equal(t, "pi-detector:dev", detectorImageDefault("", nil))
	// remote, no digest → registry tag
	assert.Equal(t, "myreg.io/ap/pi-detector:dev", detectorImageDefault("myreg.io/ap", nil))
	// remote, digest present → pinned
	got := detectorImageDefault("myreg.io/ap", map[string]string{apimage.Detector.Name: "sha256:abc"})
	assert.Equal(t, "myreg.io/ap/pi-detector:dev@sha256:abc", got)
}

// TestPrepopulateSelections_RoundTripsContentInspectors guards Task 7: the
// stored content-inspector typed config (threshold/action, allowlist
// domains) must round-trip through prepopulateSelections on re-run instead
// of resetting to sentinel defaults.
func TestPrepopulateSelections_RoundTripsContentInspectors(t *testing.T) {
	sel := settingswizard.Selections{
		PromptInjection: &settingswizard.PromptInjectionSel{
			DetectorImage: "reg.example.com/detector:1", Threshold: 0.9, Action: "block",
		},
		URLAllowlist: &settingswizard.URLAllowlistSel{
			DefaultAction: "approve",
			Rules: []settingswizard.URLRuleSel{
				{Domain: "github.com", Action: "allow"},
				{Domain: "api.example.com", Action: "allow"},
			},
		},
	}
	cas := settingswizard.Compose(sel)

	got, err := prepopulateSelections(cas, "reg.example.com", nil)
	require.NoError(t, err)

	require.NotNil(t, got.PromptInjection)
	assert.Equal(t, "reg.example.com/detector:1", got.PromptInjection.DetectorImage)
	assert.Equal(t, 0.9, got.PromptInjection.Threshold)
	assert.Equal(t, "block", got.PromptInjection.Action)

	require.NotNil(t, got.URLAllowlist)
	assert.Equal(t, "approve", got.URLAllowlist.DefaultAction)
	domains := make([]string, 0, len(got.URLAllowlist.Rules))
	for _, r := range got.URLAllowlist.Rules {
		domains = append(domains, r.Domain)
	}
	assert.Equal(t, []string{"github.com", "api.example.com"}, domains)
}

// TestPrepopulateSelections_RoundTripsNativeFileHandling guards the passive
// Tier-2 grant: an out-of-band NativeFileHandling=true on the CR must survive a
// wizard re-run (the wizard has no interactive toggle, so prepopulate is the
// only thing that keeps a re-run from silently dropping the grant).
func TestPrepopulateSelections_RoundTripsNativeFileHandling(t *testing.T) {
	grant := true
	cas := &v1alpha1.ClusterAgentSettings{
		Spec: v1alpha1.SettingsSpec{
			Limits: &v1alpha1.SettingsLimits{NativeFileHandling: &grant},
		},
	}

	got, err := prepopulateSelections(cas, "reg.example.com", nil)
	require.NoError(t, err)
	assert.True(t, got.NativeFileHandling, "out-of-band NativeFileHandling grant must round-trip on re-run")
}

// TestRunSettingsWizard_DefaultModelNonInteractive verifies the --default-model*
// non-interactive flow: the resolved token ($ANTHROPIC_API_KEY) is written to the
// adoption-labelled central Secret, and the composed ClusterAgentSettings carries
// exactly one catalog entry marked default, pointing at that Secret.
func TestRunSettingsWizard_DefaultModelNonInteractive(t *testing.T) {
	// Fake-backed Bundle (empty cluster) + the dynamic client the applied
	// ClusterAgentSettings + Secret land in.
	g, dyn := settingsGlobals(t, nil)
	prev := DynamicFactory
	t.Cleanup(func() { DynamicFactory = prev })
	DynamicFactory = func(*apcmd.Globals) (DynIface, error) { return dyn, nil }

	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	opts := DefaultModelOpts{
		Model: "claude-sonnet-5", Provider: "anthropic", TokenEnv: "ANTHROPIC_API_KEY",
		SecretName: "model-default-token", SecretNamespace: "agentprimitives-system", SecretKey: "token",
	}
	err := RunWizard(context.Background(), &cobra.Command{}, g, true /*defaults*/, false /*dryRun*/, "", nil, opts)
	require.NoError(t, err)

	// Secret exists and is adoption-labelled.
	secretGVR := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	sec, err := dyn.Resource(secretGVR).Namespace("agentprimitives-system").Get(context.Background(), "model-default-token", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "true", sec.GetLabels()[adoptguard.AdoptedLabel])

	// ClusterAgentSettings has the sonnet-5 default catalog entry → the token.
	cas, err := dyn.Resource(aptest.ClusterAgentSettingsGVR).Get(context.Background(), "cluster", metav1.GetOptions{})
	require.NoError(t, err)
	cat, _, _ := unstructured.NestedSlice(cas.Object, "spec", "modelCatalog")
	require.Len(t, cat, 1)
	e := cat[0].(map[string]any)
	assert.Equal(t, "claude-sonnet-5", e["name"])
	assert.Equal(t, true, e["default"])
	assert.Equal(t, "model-default-token", e["tokenRef"].(map[string]any)["name"])
}

// TestRunSettingsWizard_DefaultModelDryRunSkipsSecretAndApply verifies --dry-run
// composes the catalog entry into the preview but creates neither the Secret nor
// the ClusterAgentSettings.
func TestRunSettingsWizard_DefaultModelDryRunSkipsSecretAndApply(t *testing.T) {
	g, dyn := settingsGlobals(t, nil)
	prev := DynamicFactory
	t.Cleanup(func() { DynamicFactory = prev })
	DynamicFactory = func(*apcmd.Globals) (DynIface, error) { return dyn, nil }

	// Deliberately do NOT set $ANTHROPIC_API_KEY — dry-run must never reach
	// token resolution, so a missing source must not fail the run.
	opts := DefaultModelOpts{
		Model: "claude-sonnet-5", Provider: "anthropic", TokenEnv: "ANTHROPIC_API_KEY",
		SecretName: "model-default-token", SecretNamespace: "agentprimitives-system", SecretKey: "token",
	}
	err := RunWizard(context.Background(), &cobra.Command{}, g, true /*defaults*/, true /*dryRun*/, "", nil, opts)
	require.NoError(t, err)

	secretGVR := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	_, err = dyn.Resource(secretGVR).Namespace("agentprimitives-system").Get(context.Background(), "model-default-token", metav1.GetOptions{})
	assert.Error(t, err, "dry-run must not create the central token Secret")

	_, err = dyn.Resource(aptest.ClusterAgentSettingsGVR).Get(context.Background(), "cluster", metav1.GetOptions{})
	assert.Error(t, err, "dry-run must not apply the ClusterAgentSettings")
}

// TestRunSettingsWizard_DefaultModelDryRunNeverBuildsDynamicClient guards the
// fix for the dry-run + --default-model early-build: applyDefaultModel never
// touches dyn when dryRun is set (it returns right after appending the
// catalog entry to sel), so RunWizard must not pay for
// DynamicFactory at all in that case. A factory that always errors
// proves the point — if RunWizard called it, the run would fail.
func TestRunSettingsWizard_DefaultModelDryRunNeverBuildsDynamicClient(t *testing.T) {
	// A fake-backed Bundle so the defaults path's existing-settings read (via
	// g.Bundle().Controller, NOT DynamicFactory) succeeds; DynamicFactory stays
	// erroring to prove the dry-run never reaches it.
	g, _ := settingsGlobals(t, nil)
	prev := DynamicFactory
	t.Cleanup(func() { DynamicFactory = prev })
	DynamicFactory = func(*apcmd.Globals) (DynIface, error) {
		return nil, fmt.Errorf("DynamicFactory must not be called under --dry-run")
	}

	opts := DefaultModelOpts{
		Model: "claude-sonnet-5", Provider: "anthropic", TokenEnv: "ANTHROPIC_API_KEY",
		SecretName: "model-default-token", SecretNamespace: "agentprimitives-system", SecretKey: "token",
	}
	err := RunWizard(context.Background(), &cobra.Command{}, g, true /*defaults*/, true /*dryRun*/, "", nil, opts)
	require.NoError(t, err, "dry-run + --default-model must not build a dynamic client")
}

// TestRunSettingsWizard_DefaultsPreservesExistingClusterEdits is the root-fix
// for the restart clobber: a --defaults re-run (a desktop VM bring-up, or an
// explicit re-invocation) must re-assert the baseline controls WITHOUT dropping
// the Cluster-tab edits the wizard has no screen for — the extra catalog
// entries (modelCatalog is atomic under a forced apply), the custom pinning
// ceiling, and the customized toolGuard thresholds.
func TestRunSettingsWizard_DefaultsPreservesExistingClusterEdits(t *testing.T) {
	tokenRef := func(name string) *v1alpha1.NamespacedSecretKeyRef {
		return &v1alpha1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: name, Key: "token"}
	}
	stored := &v1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ClusterAgentSettingsName},
		Spec: v1alpha1.SettingsSpec{
			Defaults: &v1alpha1.SettingsDefaults{
				ToolGuard: &v1alpha1.ToolGuardPolicy{
					Rules: []v1alpha1.ToolGuardRule{{
						Match:   v1alpha1.ToolGuardMatch{Tool: "*"},
						Breaker: &v1alpha1.BreakerSpec{FailureThreshold: 10, Action: v1alpha1.ToolGuardActionDeny},
					}},
				},
			},
			Limits: &v1alpha1.SettingsLimits{
				Pinning: &v1alpha1.PinningPolicy{Rules: []v1alpha1.PinningRule{
					{Kind: "image", MinStrength: "digest", Mode: v1alpha1.PinModeBlock},
				}},
			},
			ModelCatalog: &[]v1alpha1.ModelCatalogEntry{
				{Name: "old-default", Provider: "anthropic", Default: true, TokenRef: tokenRef("model-default-token")},
				{Name: "extra-1", Provider: "openai", TokenRef: tokenRef("model-default-token-extra-1")},
				{Name: "extra-2", Provider: "openrouter", TokenRef: tokenRef("model-default-token-extra-2")},
			},
		},
	}
	g, dyn := settingsGlobals(t, stored)
	prev := DynamicFactory
	t.Cleanup(func() { DynamicFactory = prev })
	DynamicFactory = func(*apcmd.Globals) (DynIface, error) { return dyn, nil }

	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	opts := DefaultModelOpts{
		Model: "claude-sonnet-5", Provider: "anthropic", TokenEnv: "ANTHROPIC_API_KEY",
		SecretName: "model-default-token", SecretNamespace: "agentprimitives-system", SecretKey: "token",
	}
	require.NoError(t, RunWizard(context.Background(), &cobra.Command{}, g,
		true /*defaults*/, false /*dryRun*/, "", nil, opts))

	applied, err := dyn.Resource(aptest.ClusterAgentSettingsGVR).Get(context.Background(), "cluster", metav1.GetOptions{})
	require.NoError(t, err)

	// Model catalog: the new default is written, old-default is demoted (kept as
	// a non-default entry), and both extras survive — nothing is dropped.
	cat, _, _ := unstructured.NestedSlice(applied.Object, "spec", "modelCatalog")
	names := map[string]bool{}
	defaults := []string{}
	for _, e := range cat {
		m := e.(map[string]any)
		name := m["name"].(string)
		names[name] = true
		if d, _ := m["default"].(bool); d {
			defaults = append(defaults, name)
		}
	}
	assert.True(t, names["claude-sonnet-5"], "the newly-registered default must be present")
	assert.True(t, names["old-default"], "the previous default must survive (demoted)")
	assert.True(t, names["extra-1"], "extra catalog entry 1 must survive the re-run")
	assert.True(t, names["extra-2"], "extra catalog entry 2 must survive the re-run")
	assert.Equal(t, []string{"claude-sonnet-5"}, defaults, "exactly one default, and it is the new one")

	// Custom pinning ceiling (image/digest/block) must survive verbatim.
	pins, _, _ := unstructured.NestedSlice(applied.Object, "spec", "limits", "pinning", "rules")
	var imageMode string
	for _, r := range pins {
		m := r.(map[string]any)
		if m["kind"] == "image" {
			imageMode, _ = m["mode"].(string)
		}
	}
	assert.Equal(t, v1alpha1.PinModeBlock, imageMode, "the operator's block-mode image pin must survive")

	// Customized toolGuard threshold (10, not the baseline 5) must survive.
	rules, _, _ := unstructured.NestedSlice(applied.Object, "spec", "defaults", "toolGuard", "rules")
	require.NotEmpty(t, rules)
	breaker, _, _ := unstructured.NestedMap(rules[0].(map[string]any), "breaker")
	require.NotNil(t, breaker)
	assert.EqualValues(t, 10, breaker["failureThreshold"], "custom breaker threshold must be preserved, not reset to baseline")
}

// TestDefaultModelOpts_Enabled documents the enabled() gate: an empty Model
// means "no default model" and the whole flow is skipped.
func TestDefaultModelOpts_Enabled(t *testing.T) {
	assert.False(t, DefaultModelOpts{}.enabled())
	assert.False(t, DefaultModelOpts{Model: "   "}.enabled())
	assert.True(t, DefaultModelOpts{Model: "claude-sonnet-5"}.enabled())
}

// TestRegisterDefaultModelFlags_NamesAndDefaults locks the shared flag helper's
// flag names and default values so `oap settings wizard` and `oap init`
// cannot drift from each other or from the documented defaults.
func TestRegisterDefaultModelFlags_NamesAndDefaults(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	dm := RegisterDefaultModelFlags(cmd)
	require.NotNil(t, dm)

	for _, tc := range []struct {
		flag    string
		wantDef string
	}{
		{"default-model", ""},
		{"default-model-provider", "anthropic"},
		{"default-model-token-env", "ANTHROPIC_API_KEY"},
		{"default-model-token-file", ""},
		{"default-model-secret-name", "model-default-token"},
		{"default-model-secret-namespace", "agentprimitives-system"},
		{"default-model-secret-key", "token"},
	} {
		f := cmd.Flags().Lookup(tc.flag)
		require.NotNilf(t, f, "flag %q must be registered", tc.flag)
		assert.Equalf(t, tc.wantDef, f.DefValue, "flag %q default", tc.flag)
	}

	assert.Equal(t, "anthropic", dm.Provider)
	assert.Equal(t, "ANTHROPIC_API_KEY", dm.TokenEnv)
	assert.Equal(t, "model-default-token", dm.SecretName)
	assert.Equal(t, "agentprimitives-system", dm.SecretNamespace)
	assert.Equal(t, "token", dm.SecretKey)
}

// TestBuildCatalogStepOpts_MapsFieldsAndAppliesDefaults verifies the pure
// mapping from the interactive "Model catalog" answers to a
// *catalogStepResult: blank secret fields fall back to the same defaults as
// RegisterDefaultModelFlags (so an interactive run and a --default-model run
// converge on the same central Secret), non-blank fields are trimmed and kept
// as-is, prices pass through untouched, and a blank model name yields nil.
func TestBuildCatalogStepOpts_MapsFieldsAndAppliesDefaults(t *testing.T) {
	cases := []struct {
		name                                               string
		secretName, secretNamespace, secretKey             string
		wantSecretName, wantSecretNamespace, wantSecretKey string
	}{
		{
			name:       "blank secret fields fall back to the flag defaults",
			secretName: "", secretNamespace: "", secretKey: "",
			wantSecretName: "model-default-token", wantSecretNamespace: "agentprimitives-system", wantSecretKey: "token",
		},
		{
			name:       "non-blank secret fields are trimmed and kept as-is",
			secretName: "  my-secret  ", secretNamespace: " my-ns ", secretKey: " my-key ",
			wantSecretName: "my-secret", wantSecretNamespace: "my-ns", wantSecretKey: "my-key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildCatalogStepOpts("claude-sonnet-5", "anthropic", tc.secretName, tc.secretNamespace, tc.secretKey, 0, 0)
			require.NotNil(t, got)
			assert.Equal(t, "claude-sonnet-5", got.opts.Model)
			assert.Equal(t, "anthropic", got.opts.Provider)
			assert.Equal(t, "ANTHROPIC_API_KEY", got.opts.TokenEnv, "interactive step should also honor $ANTHROPIC_API_KEY before asking")
			assert.Equal(t, tc.wantSecretName, got.opts.SecretName)
			assert.Equal(t, tc.wantSecretNamespace, got.opts.SecretNamespace)
			assert.Equal(t, tc.wantSecretKey, got.opts.SecretKey)
		})
	}

	t.Run("blank model name returns nil (defensive)", func(t *testing.T) {
		assert.Nil(t, buildCatalogStepOpts("   ", "anthropic", "", "", "", 0, 0))
	})

	t.Run("prices pass through untouched", func(t *testing.T) {
		got := buildCatalogStepOpts("claude-opus-4-8", "anthropic", "s", "ns", "k", 15, 75)
		require.NotNil(t, got)
		assert.Equal(t, 15.0, got.inputPerMTok)
		assert.Equal(t, 75.0, got.outputPerMTok)
	})
}

// TestResolveDefaultModelOpts covers the mutual-exclusion/dedup guard between
// the --default-model* flags and the interactive "Model catalog" step:
// at most one may ever feed applyDefaultModel (the sole appender to
// sel.ModelCatalog), so exactly one of {neither, flag-only, form-only} yields
// ok=true, and both-set is a fail-closed error rather than silently picking
// one path.
func TestResolveDefaultModelOpts(t *testing.T) {
	flagOpts := DefaultModelOpts{Model: "claude-sonnet-5", Provider: "anthropic", SecretName: "s1", SecretNamespace: "ns1", SecretKey: "k1"}
	formStep := &catalogStepResult{opts: DefaultModelOpts{Model: "claude-opus-4-8", Provider: "anthropic", SecretName: "s2", SecretNamespace: "ns2", SecretKey: "k2"}}

	cases := []struct {
		name      string
		flag      DefaultModelOpts
		form      *catalogStepResult
		wantOK    bool
		wantErr   bool
		wantModel string
	}{
		{name: "neither opted in: ok=false, no error", flag: DefaultModelOpts{}, form: nil, wantOK: false, wantErr: false},
		{name: "flag only: returns the flag opts", flag: flagOpts, form: nil, wantOK: true, wantModel: "claude-sonnet-5"},
		{name: "form only: returns the form step's opts", flag: DefaultModelOpts{}, form: formStep, wantOK: true, wantModel: "claude-opus-4-8"},
		{name: "both set: ambiguous, fails closed with an error", flag: flagOpts, form: formStep, wantOK: false, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := resolveDefaultModelOpts(tc.flag, tc.form)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			if ok {
				assert.Equal(t, tc.wantModel, got.Model)
			}
		})
	}
}

// parseModelFlags binds the real --default-model* flags and parses args
// through cobra, so the cases below exercise the flag DEFAULTS a user
// actually gets — which is the whole point: substituting a non-Anthropic
// model into the documented invocation is what silently pairs it with
// provider=anthropic and $ANTHROPIC_API_KEY.
func parseModelFlags(t *testing.T, args ...string) DefaultModelOpts {
	t.Helper()
	cmd := &cobra.Command{}
	dm := RegisterDefaultModelFlags(cmd)
	require.NoError(t, cmd.ParseFlags(args))
	return *dm
}

// TestResolveDefaultModelOpts_RefusesAnIncoherentModelProviderPair covers both
// halves of DefaultModelOpts.checkCoherent. A catalog entry marked
// `default: true` is inherited by every session that names no model of its
// own, so a model the provider cannot serve — or a token read from the wrong
// provider's env var — must fail here, not at the first turn.
func TestResolveDefaultModelOpts_RefusesAnIncoherentModelProviderPair(t *testing.T) {
	cases := []struct {
		name string
		args []string
		// wantErrSubstrs are all required in the message; empty means the
		// combination must be accepted.
		wantErrSubstrs []string
	}{
		{
			name: "documented anthropic invocation: accepted unchanged",
			args: []string{"--default-model", "claude-sonnet-5"},
		},
		{
			name:           "openai model substituted into it: refused, naming the model and its real provider",
			args:           []string{"--default-model", "gpt-5.5"},
			wantErrSubstrs: []string{"gpt-5.5", `"openai"`, `"anthropic"`},
		},
		{
			name:           "anthropic model under --default-model-provider=openai: refused",
			args:           []string{"--default-model", "claude-sonnet-5", "--default-model-provider", "openai", "--default-model-token-env", "OPENAI_API_KEY"},
			wantErrSubstrs: []string{"claude-sonnet-5", `"anthropic"`, `"openai"`},
		},
		{
			name:           "openrouter's virtual model under anthropic: refused",
			args:           []string{"--default-model", "openrouter/auto"},
			wantErrSubstrs: []string{"openrouter/auto", `"openrouter"`},
		},
		{
			name: "matching openai pair with its own token env: accepted",
			args: []string{"--default-model", "gpt-5.5", "--default-model-provider", "openai", "--default-model-token-env", "OPENAI_API_KEY"},
		},
		{
			name: "namespaced openrouter model under openrouter: accepted",
			args: []string{"--default-model", "anthropic/claude-3.5-sonnet", "--default-model-provider", "openrouter", "--default-model-token-env", "OPENROUTER_API_KEY"},
		},
		{
			name: "model no built-in table lists: accepted, the tables trail releases",
			args: []string{"--default-model", "gpt-6-not-yet-tabled", "--default-model-provider", "openai", "--default-model-token-env", "OPENAI_API_KEY"},
		},
		{
			name:           "non-anthropic provider left on the default token env: refused, naming the Anthropic var",
			args:           []string{"--default-model", "gpt-6-not-yet-tabled", "--default-model-provider", "openai"},
			wantErrSubstrs: []string{"ANTHROPIC_API_KEY", "openai", "--default-model-token-env"},
		},
		{
			name: "non-anthropic provider with a token FILE: accepted, the file wins over any env",
			args: []string{"--default-model", "gpt-6-not-yet-tabled", "--default-model-provider", "openai", "--default-model-token-file", "/tmp/token"},
		},
		{
			// The interactive catalog screen offers this provider; it fronts
			// no upstream, so it must not be made to name a key var.
			name: "test harness provider on the default token env: accepted, it needs no real key",
			args: []string{"--default-model", "scripted", "--default-model-provider", "test"},
		},
		{
			name: "surrounding whitespace does not defeat attribution of a matching pair",
			args: []string{"--default-model", "  gpt-5.5  ", "--default-model-provider", "openai", "--default-model-token-env", "OPENAI_API_KEY"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := resolveDefaultModelOpts(parseModelFlags(t, tc.args...), nil)
			if len(tc.wantErrSubstrs) == 0 {
				require.NoError(t, err)
				assert.True(t, ok, "an accepted combination must still opt in")
				return
			}
			require.Error(t, err)
			assert.False(t, ok, "a refused combination must not opt in")
			assert.Empty(t, got.Model, "a refused combination must yield no opts")
			for _, want := range tc.wantErrSubstrs {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

// TestResolveDefaultModelOpts_RefusesAnIncoherentPairFromTheFormToo proves the
// check sits at the choke point both paths pass through, not on the flag path
// alone. The interactive "Model catalog" step takes the model as free text
// beside a provider Select, so typing an OpenAI id under the offered
// "anthropic" option is exactly the mispairing a user can produce there.
func TestResolveDefaultModelOpts_RefusesAnIncoherentPairFromTheFormToo(t *testing.T) {
	step := buildCatalogStepOpts("gpt-5.5", defaultModelProvider, "", "", "", 0, 0)
	require.NotNil(t, step, "fixture must produce a catalog step")

	got, ok, err := resolveDefaultModelOpts(DefaultModelOpts{}, step)
	require.Error(t, err)
	assert.False(t, ok)
	assert.Empty(t, got.Model)
	assert.Contains(t, err.Error(), "gpt-5.5")
	assert.Contains(t, err.Error(), `"openai"`)
}

// TestResolveDefaultModelOpts_FormStepProducesExactlyOneDefaultEntry drives
// the form-only path end to end through applyDefaultModel (dry-run, so no
// dynamic client or token question is needed) and Compose, and asserts the
// composed ClusterAgentSettings carries exactly one catalog entry marked
// default:true — the guarantee this path exists to provide.
func TestResolveDefaultModelOpts_FormStepProducesExactlyOneDefaultEntry(t *testing.T) {
	step := buildCatalogStepOpts("claude-sonnet-5", "anthropic", "", "", "", 3, 15)
	require.NotNil(t, step)

	opts, ok, err := resolveDefaultModelOpts(DefaultModelOpts{}, step)
	require.NoError(t, err)
	require.True(t, ok)

	var sel settingswizard.Selections
	require.NoError(t, applyDefaultModel(context.Background(), nil, &sel, opts, nil /*never asks*/, true /*dryRun*/, false /*existingDefault*/))
	require.Len(t, sel.ModelCatalog, 1, "exactly one catalog entry")
	assert.True(t, sel.ModelCatalog[0].Default)
	assert.Equal(t, "claude-sonnet-5", sel.ModelCatalog[0].Name)

	cas := settingswizard.Compose(sel)
	require.NotNil(t, cas.Spec.ModelCatalog)
	got := *cas.Spec.ModelCatalog
	require.Len(t, got, 1, "exactly one default:true catalog entry in the composed ClusterAgentSettings")
	assert.True(t, got[0].Default)
}

// TestCatalogStepResult_NeverCarriesTokenValue guards against a future
// "simplification" that adds a raw token-value field to DefaultModelOpts or
// catalogStepResult. Both structs flow from the interactive "Model catalog"
// answers (and, for DefaultModelOpts, from cobra flags) into applyDefaultModel;
// the token VALUE must only ever live in the token screen's own bound field,
// never a struct field that could be logged, marshalled into the YAML preview,
// or otherwise persisted.
func TestCatalogStepResult_NeverCarriesTokenValue(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeOf(DefaultModelOpts{}),
		reflect.TypeOf(catalogStepResult{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			name := typ.Field(i).Name
			lower := strings.ToLower(name)
			assert.NotEqualf(t, "token", lower, "%s.%s stores a raw token value", typ.Name(), name)
			assert.Falsef(t, strings.Contains(lower, "tokenvalue"), "%s.%s stores a raw token value", typ.Name(), name)
		}
	}
}

// TestDecideCatalogTokenAction covers all four (existing, entered) cells of
// the interactive re-run fix: an interactive wizard re-run of an
// ALREADY-registered default must let the user leave the token question blank
// to keep the existing Secret (no silent $ANTHROPIC_API_KEY rotation, no
// dropped wizard selections from a hard error), while a brand-new default
// still requires a token.
func TestDecideCatalogTokenAction(t *testing.T) {
	cases := []struct {
		name      string
		existing  bool
		entered   string
		wantMint  bool
		wantValue string
		wantErr   bool
	}{
		{
			name:     "new default, blank entry: error (a new default requires a token)",
			existing: false, entered: "",
			wantErr: true,
		},
		{
			name:     "new default, non-blank entry: mint with the entered value",
			existing: false, entered: "sk-new",
			wantMint: true, wantValue: "sk-new",
		},
		{
			name:     "existing default, blank entry: keep the existing Secret (no mint, no error)",
			existing: true, entered: "  ", // whitespace-only should trim to blank too
			wantMint: false, wantValue: "",
		},
		{
			name:     "existing default, non-blank entry: explicit rotation, mint with the entered value",
			existing: true, entered: "sk-rotated",
			wantMint: true, wantValue: "sk-rotated",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mint, value, err := decideCatalogTokenAction(tc.existing, tc.entered)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantMint, mint)
			assert.Equal(t, tc.wantValue, value)
		})
	}
}

// TestApplyDefaultModel_NilAskerNeverTakesTheOptionalTokenBranch guards the
// gate on applyDefaultModel's optional-token branch: existingDefault=true alone
// must NOT change behavior on a run that cannot ask (a nil asker). This keeps
// --defaults runs resolving the token exactly the same way even if
// existingDefault were ever (incorrectly) threaded through.
func TestApplyDefaultModel_NilAskerNeverTakesTheOptionalTokenBranch(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)

	t.Setenv("ANTHROPIC_API_KEY", "sk-env-value")
	opts := DefaultModelOpts{
		Model: "claude-sonnet-5", Provider: "anthropic", TokenEnv: "ANTHROPIC_API_KEY",
		SecretName: "model-default-token", SecretNamespace: "agentprimitives-system", SecretKey: "token",
	}
	var sel settingswizard.Selections
	// ask=nil, existingDefault=true: must still resolve via modeltoken.Resolve's
	// file/env/prompt precedence (env wins here), NOT the optional-token branch
	// (which would need a terminal).
	err := applyDefaultModel(context.Background(), dyn, &sel, opts, nil /*never asks*/, false /*dryRun*/, true /*existingDefault*/)
	require.NoError(t, err)

	secretGVR := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	sec, err := dyn.Resource(secretGVR).Namespace("agentprimitives-system").Get(context.Background(), "model-default-token", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "sk-env-value", sec.Object["stringData"].(map[string]any)["token"])
}

// TestApplyDefaultModel_OptionalTokenBranchIsTakenOnlyOnAnInteractiveRerun
// pairs an asker with each (existingDefault) case and asserts which question it
// was handed. The `optional` argument is the whole difference between "leaving
// it blank keeps your token" and "leaving it blank is an error", so it is
// asserted rather than assumed.
func TestApplyDefaultModel_OptionalTokenBranchIsTakenOnlyOnAnInteractiveRerun(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	opts := DefaultModelOpts{
		Model: "claude-sonnet-5", Provider: "anthropic", TokenEnv: "AP_UNSET_TOKEN_ENV",
		SecretName: "model-default-token", SecretNamespace: "agentprimitives-system", SecretKey: "token",
	}

	cases := []struct {
		name            string
		existingDefault bool
		answer          string
		wantOptional    bool
		wantSecretValue string // "" means no Secret should have been written
	}{
		{
			name:            "brand-new default: the token is required, and the answer is minted",
			existingDefault: false, answer: "sk-fresh",
			wantOptional: false, wantSecretValue: "sk-fresh",
		},
		{
			name:            "re-run, blank answer: optional, and the stored token is left alone",
			existingDefault: true, answer: "",
			wantOptional: true, wantSecretValue: "",
		},
		{
			name:            "re-run, typed answer: optional, and the token is rotated to it",
			existingDefault: true, answer: "sk-rotated",
			wantOptional: true, wantSecretValue: "sk-rotated",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dyn := dynamicfake.NewSimpleDynamicClient(scheme)
			var gotOptional, asked bool
			ask := func(_ context.Context, _ DefaultModelOpts, optional bool) (string, error) {
				asked, gotOptional = true, optional
				return tc.answer, nil
			}

			var sel settingswizard.Selections
			require.NoError(t, applyDefaultModel(context.Background(), dyn, &sel, opts, ask, false /*dryRun*/, tc.existingDefault))

			// Asserted separately from gotOptional: `false` is also what a
			// question that was never asked would leave behind.
			require.True(t, asked, "the token question must have been put to the operator")
			assert.Equal(t, tc.wantOptional, gotOptional)

			secretGVR := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
			sec, err := dyn.Resource(secretGVR).Namespace("agentprimitives-system").Get(context.Background(), "model-default-token", metav1.GetOptions{})
			if tc.wantSecretValue == "" {
				assert.Error(t, err, "a blank answer on a re-run must leave the stored token untouched")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantSecretValue, sec.Object["stringData"].(map[string]any)["token"])
		})
	}
}

// A Ctrl+C at the token question must stay recognisable as one all the way back
// to RunWizard, which is what lets it report "no changes applied"
// instead of a non-zero exit — nothing has been written at that point.
//
// The chain crosses two layers that could quietly flatten it (the asker's
// tui.UserFacing, and modeltoken.Resolve returning its prompt's error), so the
// sentinel is asserted rather than assumed.
func TestApplyDefaultModel_CancellationStaysMatchableThroughBothLayers(t *testing.T) {
	opts := DefaultModelOpts{
		Model: "claude-sonnet-5", Provider: "anthropic", TokenEnv: "AP_UNSET_TOKEN_ENV",
		SecretName: "model-default-token", SecretNamespace: "agentprimitives-system", SecretKey: "token",
	}
	canceled := fmt.Errorf("tui: screen %q canceled: %w", "token", huh.ErrUserAborted)
	ask := func(context.Context, DefaultModelOpts, bool) (string, error) {
		return "", tui.UserFacing(canceled)
	}

	for _, existingDefault := range []bool{false, true} {
		t.Run(fmt.Sprintf("existingDefault=%v: the abort is still huh.ErrUserAborted", existingDefault), func(t *testing.T) {
			var sel settingswizard.Selections
			err := applyDefaultModel(context.Background(), nil, &sel, opts, ask, false /*dryRun*/, existingDefault)

			require.Error(t, err)
			assert.ErrorIs(t, err, huh.ErrUserAborted, "the sentinel must survive both layers")
			assert.NotContains(t, err.Error(), "tui: ", "and the wording must still be the operator's")
		})
	}
}

// ── the wizard's screens ────────────────────────────────────────────────────

// wizardPres presents a run over the line-oriented driver, reading a script.
// Nothing here may reach the process's own stdin — a test that let it would be
// exercising whatever terminal the test runner happened to have.
func wizardPres(t *testing.T, script string, out io.Writer) *Presentation {
	t.Helper()
	return &Presentation{
		in:    strings.NewReader(script),
		out:   out,
		theme: tui.NewTheme(tui.Caps{}),
	}
}

// The accessible renderer drives a multi-select by number — each line toggles
// one row, 0 confirms — and an input by line, where a bare Enter keeps whatever
// the field was pre-filled with. Every case below answers to completion and
// asserts on the SELECTIONS the run produced, never on err: that renderer has
// no error channel, so a short script yields a fully-defaulted result and a nil
// error.
//
// wantAsked is what keeps that honest. Several rows expect a value that is ALSO
// the field's default, which is exactly what a question that never rendered
// would leave behind, so each case names the question titles that must have
// reached the operator — and, for a skipped screen, the ones that must not.
func TestRunWizardFormOverThePlainDriver(t *testing.T) {
	cases := []struct {
		name    string
		initial settingswizard.Selections
		script  string

		wantSel      settingswizard.Selections
		wantStep     *catalogStepResult
		wantAsked    []string
		wantNotAsked []string
	}{
		{
			name:    "baseline confirmed unchanged: the four baseline controls, no detail screen runs",
			initial: settingswizard.BaselineSelections(),
			script:  "0\n",
			wantSel: settingswizard.Selections{Breaker: true, RateLimit: true, DataLimit: true, Pinning: true},
			wantAsked: []string{
				"Security controls to enable",
			},
			wantNotAsked: []string{
				"Prompt-injection detector image",
				"Allowed domains",
				"Model name",
			},
		},
		{
			name:    "every control unticked: all off, and no detail screen runs",
			initial: settingswizard.BaselineSelections(),
			script:  "1\n2\n3\n4\n0\n",
			wantSel: settingswizard.Selections{},
			wantAsked: []string{
				"Security controls to enable",
			},
			wantNotAsked: []string{
				"Prompt-injection detector image",
				"Allowed domains",
				"Model name",
			},
		},
		{
			name:    "prompt-injection ticked: its three questions are asked and answered",
			initial: settingswizard.Selections{},
			script:  "5\n0\n" + "reg.example.com/detect:2\n" + "0.9\n" + "2\n",
			wantSel: settingswizard.Selections{
				PromptInjection: &settingswizard.PromptInjectionSel{
					DetectorImage: "reg.example.com/detect:2", Threshold: 0.9, Action: "block",
				},
			},
			wantAsked: []string{
				"Prompt-injection detector image",
				"Detection threshold, between 0 and 1",
				"Action on a detected injection",
			},
			wantNotAsked: []string{"Allowed domains", "Model name"},
		},
		{
			name:    "URL allow-list ticked: the domains split, and the unlisted action is chosen",
			initial: settingswizard.Selections{},
			script:  "6\n0\n" + "github.com, api.example.com\n" + "2\n",
			wantSel: settingswizard.Selections{
				URLAllowlist: &settingswizard.URLAllowlistSel{
					Rules: []settingswizard.URLRuleSel{
						{Domain: "github.com", Action: "allow"},
						{Domain: "api.example.com", Action: "allow"},
					},
					DefaultAction: "approve",
				},
			},
			wantAsked: []string{
				"Allowed domains, separated by commas",
				"Action for an unlisted URL",
			},
			wantNotAsked: []string{"Prompt-injection detector image", "Model name"},
		},
		{
			name:    "model catalog ticked: carried out separately, never written into Selections",
			initial: settingswizard.Selections{},
			// name, provider, then a bare Enter for each of the three pre-filled
			// token fields, then both prices.
			script: "7\n0\n" + "claude-opus-4-8\n" + "1\n" + "\n" + "\n" + "\n" + "15\n" + "75\n",
			// ModelCatalog stays nil: applyDefaultModel is the sole appender.
			wantSel: settingswizard.Selections{},
			wantStep: &catalogStepResult{
				opts: DefaultModelOpts{
					Model: "claude-opus-4-8", Provider: "anthropic", TokenEnv: "ANTHROPIC_API_KEY",
					SecretName: "model-default-token", SecretNamespace: "agentprimitives-system", SecretKey: "token",
				},
				inputPerMTok: 15, outputPerMTok: 75,
			},
			wantAsked: []string{
				"Model name",
				"Name for the stored API token",
				"Input price per million tokens",
			},
			wantNotAsked: []string{"Prompt-injection detector image", "Allowed domains"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			sel, step, err := runWizardForm(context.Background(), wizardPres(t, tc.script, &out), tc.initial, "", nil)

			require.NoError(t, err)
			assert.Equal(t, tc.wantSel, sel)
			assert.Equal(t, tc.wantStep, step)

			for _, title := range tc.wantAsked {
				assert.Containsf(t, out.String(), title, "the operator must actually have been shown %q", title)
			}
			for _, title := range tc.wantNotAsked {
				assert.NotContainsf(t, out.String(), title, "an unticked control must not ask %q", title)
			}
		})
	}
}

// The summary is what stays in scrollback after the form tears down, so it has
// to name the decision in the operator's own words. (That no CREDENTIAL reaches
// it is a different guarantee, covered where the token is collected — see
// TestModelTokenScreenConsultsStateFirst.)
func TestRunWizardFormSummaryNamesTheControlsThatWereTurnedOn(t *testing.T) {
	var out bytes.Buffer
	_, _, err := runWizardForm(context.Background(), wizardPres(t, "0\n", &out), settingswizard.BaselineSelections(), "", nil)
	require.NoError(t, err)

	assert.Contains(t, out.String(), "circuit breaker", "the summary names what was turned on")
	assert.Contains(t, out.String(), "dependency pinning")
}

// A price is optional, so clearing one has to be able to REMOVE it. The
// keep-the-pre-fill fallback that makes a bare Enter work on a required field
// would make these two one-way: an operator could add a stale list price and
// never take it off again.
func TestCatalogScreenPricesCanBeCleared(t *testing.T) {
	stored := &settingswizard.ModelEntrySel{
		Name: "demo-model", Provider: "anthropic",
		TokenSecretName: "demo-token", TokenSecretNamespace: "demo-ns", TokenSecretKey: "key",
		InputPerMTok: 15, OutputPerMTok: 75,
	}
	st := tui.NewState()
	st.SetAll(keyControls, []string{guardCatalog})

	s := &catalogScreen{initial: stored}
	g, err := s.Prepare(context.Background(), st)
	require.NoError(t, err)
	require.NotNil(t, g, "a re-run still asks; the stored values only pre-fill the fields")

	// What an operator blanking both price fields on a terminal leaves behind.
	// The name and the token fields keep their pre-fill, so this isolates the
	// price behaviour from the required-field one.
	s.inputPrice.value, s.outputPrice.value = "", ""
	require.NoError(t, s.Apply(context.Background(), st))

	assert.Empty(t, st.Get(keyModelInputPrice), "a cleared price must stay cleared")
	assert.Empty(t, st.Get(keyModelOutputPrice), "a cleared price must stay cleared")
	assert.Equal(t, "demo-model", st.Get(keyModelName), "a required field still keeps its pre-fill")

	_, step, err := selectionsFromState(settingswizard.Selections{}, st)
	require.NoError(t, err)
	require.NotNil(t, step)
	assert.Zero(t, step.inputPerMTok, "no price reaches the catalog entry")
	assert.Zero(t, step.outputPerMTok)
}

// A refusal must read as the sentence a screen wrote. `tui: apply screen
// "model":` in front of it is this command's plumbing showing through, and the
// screen ID names nothing an operator can act on.
func TestRunWizardFormRefusalCarriesNoFramingAndNoScreenID(t *testing.T) {
	var out bytes.Buffer
	// Tick the model catalog, then let the script run out: the model name has no
	// default to fall back on, so the screen refuses.
	_, _, err := runWizardForm(context.Background(), wizardPres(t, "7\n0\n", &out), settingswizard.Selections{}, "", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "model name")
	assert.NotContains(t, err.Error(), "tui: ", "the sequencer's framing is ours, not the operator's")
	assert.NotContains(t, err.Error(), "screen", "a screen ID is our vocabulary, not the operator's")
}

// The State-first rule for every screen: an answer already recorded is not
// asked for again. Driven through the fail-closed driver, which turns any
// screen that DOES build a group into an error — the only way to prove "asked
// nothing" rather than "asked and happened to be handed the same answer".
func TestSettingsScreensConsultStateFirst(t *testing.T) {
	st := tui.NewState()
	st.SetAll(keyControls, []string{guardBreaker, guardInjection, guardURLList, guardCatalog})
	st.Set(keyDetectorImage, "reg.example.com/detect:3")
	st.Set(keyThreshold, "0.5")
	st.Set(keyDetectorAction, "block")
	st.Set(keyAllowedDomains, "example.test")
	st.Set(keyUnlistedAction, "approve")
	st.Set(keyModelName, "demo-model")
	st.Set(keyModelProvider, "anthropic")
	st.Set(keyModelSecretName, "demo-token")
	st.Set(keyModelSecretNS, "demo-ns")
	st.Set(keyModelSecretKey, "key")
	st.Set(keyModelInputPrice, "1")
	st.Set(keyModelOutputPrice, "2")

	answered, err := tui.RunWith(context.Background(),
		settingsScreens(settingswizard.Selections{}, "", nil),
		tui.Options{Theme: tui.NewTheme(tui.Caps{}), NonInteractive: true},
		st)
	require.NoError(t, err, "a fully-seeded run must reach the end without presenting anything")

	sel, step, err := selectionsFromState(settingswizard.Selections{}, answered)
	require.NoError(t, err)

	assert.True(t, sel.Breaker)
	assert.False(t, sel.RateLimit, "a control the answer did not name stays off")
	require.NotNil(t, sel.PromptInjection)
	assert.Equal(t, "reg.example.com/detect:3", sel.PromptInjection.DetectorImage)
	assert.Equal(t, 0.5, sel.PromptInjection.Threshold)
	assert.Equal(t, "block", sel.PromptInjection.Action)
	require.NotNil(t, sel.URLAllowlist)
	assert.Equal(t, "approve", sel.URLAllowlist.DefaultAction)
	require.NotNil(t, step)
	assert.Equal(t, "demo-model", step.opts.Model)
	assert.Equal(t, "demo-token", step.opts.SecretName)
	assert.Equal(t, 1.0, step.inputPerMTok)
}

// The token screen obeys the same rule, and is checked on its own because it is
// presented as its own run rather than as part of the wizard.
func TestModelTokenScreenConsultsStateFirst(t *testing.T) {
	s := &modelTokenScreen{model: "demo-model", provider: "anthropic"}
	st := tui.NewState()
	st.Set(keyModelToken, "sk-seeded")

	g, err := s.Prepare(context.Background(), st)

	require.NoError(t, err)
	// Compared rather than assert.Nil-ed: a huh.Group renders as several
	// thousand lines of bubbletea state, which buries the one fact the failure
	// is about.
	assert.True(t, g == nil, "an answered question must not be asked")
	require.NoError(t, s.Apply(context.Background(), st))
	assert.Equal(t, "sk-seeded", st.Get(keyModelToken))
	assert.Empty(t, st.Notes(), "a credential must never reach the summary")
}

func TestNewModelTokenAsker(t *testing.T) {
	opts := DefaultModelOpts{Model: "claude-sonnet-5", Provider: "anthropic"}

	t.Run("required: the typed token is returned and the model is named in the question", func(t *testing.T) {
		var out bytes.Buffer
		got, err := newModelTokenAsker(wizardPres(t, "sk-typed\n", &out))(context.Background(), opts, false)

		require.NoError(t, err)
		assert.Equal(t, "sk-typed", got)
		assert.Contains(t, out.String(), "API token for claude-sonnet-5")
	})

	t.Run("optional: a blank line is an answer, and says so before asking", func(t *testing.T) {
		var out bytes.Buffer
		got, err := newModelTokenAsker(wizardPres(t, "\n", &out))(context.Background(), opts, true)

		require.NoError(t, err)
		assert.Empty(t, got, "a blank answer is what keeps the stored token")
		assert.Contains(t, out.String(), "already stored", "the operator must be told what blank means")
	})

	// A defaulted-empty token would be minted into the cluster as an empty
	// credential, and the failure would surface much later as a provider
	// rejecting every request.
	t.Run("required, input exhausted: refuses, and the refusal carries no framing, no screen ID and no value", func(t *testing.T) {
		var out bytes.Buffer
		got, err := newModelTokenAsker(wizardPres(t, "", &out))(context.Background(), opts, false)

		require.Error(t, err)
		assert.Empty(t, got)
		assert.Contains(t, err.Error(), "claude-sonnet-5")
		assert.NotContains(t, err.Error(), "tui: ", "the sequencer's framing is ours, not the operator's")
		assert.NotContains(t, err.Error(), "screen", "a screen ID is our vocabulary, not the operator's")
	})
}

// An answer that names nothing on offer branches nothing and applies nothing,
// so it would silently leave a control off with nothing reported — the same
// failure the checklist's own rows exist to prevent, arriving by the other
// door.
func TestControlsScreenRefusesAnUnofferedControl(t *testing.T) {
	s := &controlsScreen{}
	st := tui.NewState()
	st.SetAll(keyControls, []string{"not-a-control"})

	err := s.Apply(context.Background(), st)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-a-control")
	// The short label, not the internal value: this refusal is read by a person,
	// and "ratelimit" / "urllist" / "pi" are our keys, not their vocabulary.
	assert.Contains(t, err.Error(), "circuit breaker", "the refusal must say what WAS on offer")
	assert.Contains(t, err.Error(), "URL allow-list")
	assert.NotContains(t, err.Error(), "urllist", "the refusal must not list our internal keys")
}

// The checklist's pre-selection is derived from the Selections a run starts
// from, so a re-run opens with exactly what is already configured ticked.
func TestPreselectedGuards(t *testing.T) {
	cases := []struct {
		name    string
		initial settingswizard.Selections
		want    []string
	}{
		{name: "nothing configured: nothing ticked", initial: settingswizard.Selections{}, want: []string{}},
		{
			name:    "the recommended baseline: its four controls, in offer order",
			initial: settingswizard.BaselineSelections(),
			want:    []string{guardBreaker, guardRateLimit, guardDataLimit, guardPinning},
		},
		{
			name: "the opt-in controls: each ticked from the field that configures it",
			initial: settingswizard.Selections{
				PromptInjection: &settingswizard.PromptInjectionSel{},
				URLAllowlist:    &settingswizard.URLAllowlistSel{},
				ModelCatalog:    []settingswizard.ModelEntrySel{{Name: "demo-model"}},
			},
			want: []string{guardInjection, guardURLList, guardCatalog},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, preselectedGuards(tc.initial))
		})
	}
}

// A note's lines are hard-wrapped at the form's body width with no indication
// that the second half belongs to the first, so guidance that overflows reads
// as two unrelated fragments. Measured against the same Chrome the TTY driver
// sizes its forms with, rather than against an assumed column count.
func TestWizardNotesFitTheNoteWidth(t *testing.T) {
	th := tui.NewTheme(tui.Caps{TTY: true, Color: true, Width: 80})

	cases := []struct {
		name    string
		block   string
		screens []tui.Screen
	}{
		{
			name:    "the model catalog's guidance, measured against the wizard's own rail",
			block:   catalogGuidance,
			screens: settingsScreens(settingswizard.Selections{}, "", nil),
		},
		{
			name:    "the kept-token guidance, measured against its own single-screen run",
			block:   tokenKeptGuidance,
			screens: []tui.Screen{&modelTokenScreen{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limit := tui.NewChrome(settingsWizardTitle, tui.Steps(tc.screens), th).BodyWidth()
			for _, line := range strings.Split(tc.block, "\n") {
				assert.LessOrEqual(t, len([]rune(line)), limit, "line is too wide for the note: %q", line)
			}
		})
	}
}

// settingsGlobals wires a apcmd.Globals whose Bundle() is a fake holding the given
// stored ClusterAgentSettings (none, when nil), plus the fake dynamic client
// its applies land in, so a whole interactive run happens with no cluster.
func settingsGlobals(t *testing.T, stored *v1alpha1.ClusterAgentSettings) (*apcmd.Globals, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	// Registered rather than the cas-only fake, because a run that rotates the
	// model token writes a Secret through this same client.
	dynScheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(dynScheme))
	dyn := dynamicfake.NewSimpleDynamicClient(dynScheme)

	builder := fake.NewClientBuilder().WithScheme(s)
	if stored != nil {
		builder = builder.WithObjects(stored)
	}
	b := &kube.Bundle{
		Controller: builder.Build(),
		Dynamic:    dyn,
		Namespace:  "default",
	}
	return &apcmd.Globals{Namespace: b.Namespace, BundleFn: func() (*kube.Bundle, error) { return b, nil }}, dyn
}

// The wizard's screens and the token question are two separate runs over ONE
// stdin, and this is the only test that spans the join between them.
//
// The line-oriented driver buffers what it reads, so a second driver built over
// the same stream starts a second buffer over bytes the first already pulled
// in — and those bytes are then unreachable. The token question sees EOF, and
// because a re-run's question is optional, an empty answer is a legitimate one:
// the rotation this script asked for would be skipped in silence, with the
// command exiting 0 and reporting success.
//
// Asserted on the STORED TOKEN, never on err, and never on the wizard merely
// completing: every one of those is identical between a run that rotated the
// token and a run that quietly declined to.
func TestRunSettingsWizardRotatesTheTokenFromTheSameStdinAsTheWizard(t *testing.T) {
	// A stored default entry is what makes this a re-run: the model-catalog
	// control opens pre-ticked, and the token question opens optional.
	stored := settingswizard.Compose(settingswizard.Selections{
		ModelCatalog: []settingswizard.ModelEntrySel{{
			Name: "demo-model", Provider: "anthropic", Default: true,
			TokenSecretName: "model-default-token", TokenSecretNamespace: "agentprimitives-system", TokenSecretKey: "token",
		}},
	})
	g, dyn := settingsGlobals(t, stored)
	prev := DynamicFactory
	t.Cleanup(func() { DynamicFactory = prev })
	DynamicFactory = func(*apcmd.Globals) (DynIface, error) { return dyn, nil }

	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	// One script, two runs: confirm the pre-ticked catalog control, accept all
	// seven pre-filled catalog answers, then rotate the token.
	cmd.SetIn(strings.NewReader("0\n" + strings.Repeat("\n", 7) + "sk-rotated-from-the-script\n"))

	require.NoError(t, RunWizard(context.Background(), cmd, g,
		false /*defaults*/, false /*dryRun*/, "", nil, DefaultModelOpts{}))

	// The question must have been reached at all — "no Secret written" is also
	// what a run that never asked produces.
	assert.Contains(t, errOut.String(), "API token for demo-model",
		"the token question must have been put to the operator")

	secretGVR := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	sec, err := dyn.Resource(secretGVR).Namespace("agentprimitives-system").Get(
		context.Background(), "model-default-token", metav1.GetOptions{})
	require.NoError(t, err, "the token the script supplied must have been stored")
	assert.Equal(t, "sk-rotated-from-the-script", sec.Object["stringData"].(map[string]any)["token"])
}

// The preview is machine-readable output an operator may be redirecting to a
// file, so it must not share a stream with the questions: `oap settings wizard >
// cas.yaml` has to leave the questions visible AND the file parseable.
func TestRunSettingsWizardInteractiveSplitsTheQuestionsFromThePreview(t *testing.T) {
	g, dyn := settingsGlobals(t, nil)
	prev := DynamicFactory
	t.Cleanup(func() { DynamicFactory = prev })
	DynamicFactory = func(*apcmd.Globals) (DynIface, error) { return dyn, nil }

	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	// Confirm the pre-selected baseline and answer nothing else.
	cmd.SetIn(strings.NewReader("0\n"))

	require.NoError(t, RunWizard(context.Background(), cmd, g,
		false /*defaults*/, false /*dryRun*/, "", nil, DefaultModelOpts{}))

	assert.Contains(t, errOut.String(), "Security controls to enable", "the questions belong off the preview's stream")
	assert.NotContains(t, out.String(), "Security controls to enable")
	assert.Contains(t, out.String(), "kind: ClusterAgentSettings", "the preview belongs on the command's output stream")
	assert.NotContains(t, errOut.String(), "kind: ClusterAgentSettings", "the manifest must not be interleaved with the questions")

	applied, err := dyn.Resource(aptest.ClusterAgentSettingsGVR).Get(context.Background(), "cluster", metav1.GetOptions{})
	require.NoError(t, err, "confirming the baseline must apply it")
	assert.Equal(t, "cluster", applied.GetName())
}

// pinningModesByKind indexes a composed policy's modes for assertion.
func pinningModesByKind(t *testing.T, cas *v1alpha1.ClusterAgentSettings) map[string]string {
	t.Helper()
	require.NotNil(t, cas.Spec.Limits, "composed CAS must carry limits")
	require.NotNil(t, cas.Spec.Limits.Pinning, "composed CAS must carry a pinning ceiling")
	out := map[string]string{}
	for _, r := range cas.Spec.Limits.Pinning.Rules {
		out[r.Kind] = r.Mode
	}
	return out
}

// TestPrepopulateSelections_PreservesStoredPinningCeiling pins the re-run
// contract for the pinning ceiling. `oap install --pinning-mode=block` seeds a
// {kind, mode: block} rule for every registered pinning kind
// (ensureClusterPinningMode). PinningPolicy.Rules is +listType=map
// +listMapKey=kind and the wizard applies with Force, so any rule the wizard
// re-emits takes ownership of that kind's `mode` — re-emitting a hardcoded
// baseline silently downgrades the operator's block ceiling to warn.
//
// It also downgraded only SOME kinds: the composer transcribed four of the
// five registered kinds, so `oap` kept block and the resulting policy was
// incoherent rather than merely weaker. Deriving the baseline kind list from
// the registry — the same source ensureClusterPinningMode and the settings
// webhook already use — is what makes that unrepresentable.
func TestPrepopulateSelections_PreservesStoredPinningCeiling(t *testing.T) {
	kinds := pinningregistry.All()
	require.NotEmpty(t, kinds, "cmd/oap registers the pinning kinds in main.go")

	stored := make([]v1alpha1.PinningRule, 0, len(kinds))
	for _, k := range kinds {
		stored = append(stored, v1alpha1.PinningRule{Kind: k.Name(), MinStrength: "frozen", Mode: v1alpha1.PinModeBlock})
	}
	cas := &v1alpha1.ClusterAgentSettings{
		Spec: v1alpha1.SettingsSpec{
			Limits: &v1alpha1.SettingsLimits{Pinning: &v1alpha1.PinningPolicy{Rules: stored}},
		},
	}

	sel, err := prepopulateSelections(cas, "reg.example.com", nil)
	require.NoError(t, err)
	require.True(t, sel.Pinning, "a stored ceiling pre-checks the pinning control")

	modes := pinningModesByKind(t, settingswizard.Compose(sel))
	for _, k := range kinds {
		assert.Equal(t, v1alpha1.PinModeBlock, modes[k.Name()],
			"re-running the wizard must not downgrade the stored ceiling for kind %q", k.Name())
	}
}

// TestCompose_BaselinePinningCoversEveryRegisteredKind guards the fresh-run
// half: with no stored rules the baseline must cover every kind the registry
// knows, so a newly registered kind cannot be left out of the ceiling by a
// transcribed literal that nobody remembers to update.
func TestCompose_BaselinePinningCoversEveryRegisteredKind(t *testing.T) {
	modes := pinningModesByKind(t, settingswizard.Compose(settingswizard.BaselineSelections()))

	for _, k := range pinningregistry.All() {
		assert.Contains(t, modes, k.Name(), "baseline ceiling must cover registered kind %q", k.Name())
	}
}

// TestPrepopulateSelections_PreservesNonDefaultCatalogEntries pins the other
// half of the same re-run problem. ModelCatalog carries no +listType marker,
// so it is ATOMIC: a forced apply replaces the whole list regardless of who
// owns the individual entries. The wizard only ever edits the single default
// entry, so unless the rest are carried through, re-running it deletes every
// non-default model from the cluster catalog.
func TestPrepopulateSelections_PreservesNonDefaultCatalogEntries(t *testing.T) {
	ref := func(name string) *v1alpha1.NamespacedSecretKeyRef {
		return &v1alpha1.NamespacedSecretKeyRef{Namespace: "ap", Name: name, Key: "token"}
	}
	catalog := []v1alpha1.ModelCatalogEntry{
		{Name: "model-a", Provider: "anthropic", Default: true, TokenRef: ref("tok-a")},
		{Name: "model-b", Provider: "openai", TokenRef: ref("tok-b")},
		{Name: "model-c", Provider: "openrouter", TokenRef: ref("tok-c")},
	}
	cas := &v1alpha1.ClusterAgentSettings{
		Spec: v1alpha1.SettingsSpec{ModelCatalog: &catalog},
	}

	sel, err := prepopulateSelections(cas, "reg.example.com", nil)
	require.NoError(t, err)

	got := settingswizard.Compose(sel)
	require.NotNil(t, got.Spec.ModelCatalog)
	names := make([]string, 0, len(*got.Spec.ModelCatalog))
	var defaults []string
	for _, e := range *got.Spec.ModelCatalog {
		names = append(names, e.Name)
		if e.Default {
			defaults = append(defaults, e.Name)
		}
	}
	assert.ElementsMatch(t, []string{"model-a", "model-b", "model-c"}, names,
		"a re-run must not delete catalog entries the wizard does not edit")
	assert.Equal(t, []string{"model-a"}, defaults, "exactly one entry stays the default")
}
