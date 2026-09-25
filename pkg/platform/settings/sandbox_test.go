package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func backend(kind, cfg string) *v1.SandboxBackend {
	b := &v1.SandboxBackend{Kind: kind}
	if cfg != "" {
		b.Config = &apiextensionsv1.JSON{Raw: []byte(cfg)}
	}
	return b
}

func specWithDefault(kind string) *v1.SettingsSpec {
	return &v1.SettingsSpec{Defaults: &v1.SettingsDefaults{Sandbox: backend(kind, "")}}
}

func specWithCeiling(kinds ...string) *v1.SettingsSpec {
	k := append([]string{}, kinds...)
	return &v1.SettingsSpec{Limits: &v1.SettingsLimits{AllowedSandboxKinds: &k}}
}

func specWithWarmPool(replicas int32) *v1.SettingsSpec {
	return &v1.SettingsSpec{Defaults: &v1.SettingsDefaults{
		Sandbox: &v1.SandboxBackend{WarmPool: &v1.WarmPoolConfig{Replicas: replicas}},
	}}
}

// Precedence runs most-specific-wins down the chain: the AgentClass (tier 3)
// overrides the SpiceboxClass (tier 4), because the SpiceboxClass states the
// bundle's default and the consuming agent may have a reason to run it
// elsewhere.
func TestResolveSandbox_Precedence(t *testing.T) {
	cases := []struct {
		name      string
		cluster   *v1.SettingsSpec
		namespace *v1.SettingsSpec
		bundle    BundleSandboxInputs
		wantKind  string
		wantProv  string
	}{
		{
			name:     "no tier sets anything: falls back to the documented default",
			wantKind: v1.DefaultSandboxKind,
			wantProv: "default",
		},
		{
			name:     "only the cluster sets a default: cluster wins",
			cluster:  specWithDefault("cluster-kind"),
			wantKind: "cluster-kind",
			wantProv: "cluster",
		},
		{
			name:      "namespace default overrides cluster default",
			cluster:   specWithDefault("cluster-kind"),
			namespace: specWithDefault("ns-kind"),
			wantKind:  "ns-kind",
			wantProv:  "namespace",
		},
		{
			name:      "SpiceboxClass overrides both tier defaults",
			cluster:   specWithDefault("cluster-kind"),
			namespace: specWithDefault("ns-kind"),
			bundle:    BundleSandboxInputs{FromSpiceboxClass: backend("class-kind", "")},
			wantKind:  "class-kind",
			wantProv:  "spiceboxclass",
		},
		{
			name:      "AgentClass overrides the SpiceboxClass",
			cluster:   specWithDefault("cluster-kind"),
			namespace: specWithDefault("ns-kind"),
			bundle: BundleSandboxInputs{
				FromAgentClass:    backend("agent-kind", ""),
				FromSpiceboxClass: backend("class-kind", ""),
			},
			wantKind: "agent-kind",
			wantProv: "agentclass",
		},
		{
			name:     "a tier naming an empty kind expresses no preference",
			cluster:  specWithDefault("cluster-kind"),
			bundle:   BundleSandboxInputs{FromAgentClass: backend("", "")},
			wantKind: "cluster-kind",
			wantProv: "cluster",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := map[string]string{}
			got, _, vs := resolveSandbox(Inputs{
				Cluster:       tc.cluster,
				Namespace:     tc.namespace,
				BundleSandbox: map[string]BundleSandboxInputs{"demo-bundle": tc.bundle},
			}, prov)

			require.Empty(t, vs, "no ceiling was set, so nothing may be rejected")
			require.Contains(t, got, "demo-bundle")
			assert.Equal(t, tc.wantKind, got["demo-bundle"].Kind)
			assert.Equal(t, tc.wantProv, prov["sandbox.demo-bundle.kind"],
				"provenance must name the tier that supplied the decision")
		})
	}
}

// The ceiling narrows downward and is checked against the FINAL resolved kind,
// so a lower tier cannot escape it by naming a backend the cluster forbids.
func TestResolveSandbox_Ceiling(t *testing.T) {
	cases := []struct {
		name        string
		cluster     *v1.SettingsSpec
		namespace   *v1.SettingsSpec
		bundle      BundleSandboxInputs
		wantAllowed []string
		wantFatal   bool
	}{
		{
			name:        "no tier constrains: unconstrained, anything resolves",
			bundle:      BundleSandboxInputs{FromAgentClass: backend("anything", "")},
			wantAllowed: nil,
		},
		{
			name:        "cluster allows the resolved kind: accepted",
			cluster:     specWithCeiling("pod", "other"),
			bundle:      BundleSandboxInputs{FromAgentClass: backend("pod", "")},
			wantAllowed: []string{"pod", "other"},
		},
		{
			name:        "cluster forbids the resolved kind: fatal violation",
			cluster:     specWithCeiling("pod"),
			bundle:      BundleSandboxInputs{FromAgentClass: backend("off-cluster", "")},
			wantAllowed: []string{"pod"},
			wantFatal:   true,
		},
		{
			name:        "namespace narrows the cluster ceiling: intersection applies",
			cluster:     specWithCeiling("pod", "other"),
			namespace:   specWithCeiling("pod"),
			bundle:      BundleSandboxInputs{FromAgentClass: backend("other", "")},
			wantAllowed: []string{"pod"},
			wantFatal:   true,
		},
		{
			name:        "a namespace cannot widen what the cluster forbids",
			cluster:     specWithCeiling("pod"),
			namespace:   specWithCeiling("pod", "off-cluster"),
			bundle:      BundleSandboxInputs{FromAgentClass: backend("off-cluster", "")},
			wantAllowed: []string{"pod"},
			wantFatal:   true,
		},
		{
			name:        "an empty ceiling denies everything, including the default",
			cluster:     specWithCeiling(),
			wantAllowed: []string{},
			wantFatal:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, allowed, vs := resolveSandbox(Inputs{
				Cluster:       tc.cluster,
				Namespace:     tc.namespace,
				BundleSandbox: map[string]BundleSandboxInputs{"demo-bundle": tc.bundle},
			}, map[string]string{})

			assert.Equal(t, tc.wantAllowed, allowed)
			if !tc.wantFatal {
				assert.Empty(t, vs)
				return
			}
			require.Len(t, vs, 1)
			assert.True(t, vs[0].Fatal, "a class asking for a forbidden backend must not run")
			assert.Contains(t, vs[0].Message, "demo-bundle", "the message must name the bundle")
			assert.Equal(t, v1.ReasonSandboxKindNotPermitted, vs[0].Reason,
				"distinct from ReasonClassInvalidSandbox: the backend is registered, just not permitted here")
		})
	}
}

// Config merges shallowly, top-level key by top-level key, so a tier can
// override one knob without restating the rest. Deep-merging a blob AP does not
// interpret would be guesswork.
func TestResolveSandbox_ConfigMergesShallowly(t *testing.T) {
	got, _, vs := resolveSandbox(Inputs{
		Cluster: &v1.SettingsSpec{Defaults: &v1.SettingsDefaults{
			Sandbox: backend("demo-kind", `{"a":1,"b":{"keep":true}}`),
		}},
		BundleSandbox: map[string]BundleSandboxInputs{
			"demo-bundle": {FromAgentClass: backend("demo-kind", `{"b":{"replaced":true},"c":3}`)},
		},
	}, map[string]string{})
	require.Empty(t, vs)

	require.NotNil(t, got["demo-bundle"].Config)
	assert.JSONEq(t, `{"a":1,"b":{"replaced":true},"c":3}`, string(got["demo-bundle"].Config.Raw),
		"top-level keys merge; a replaced key is replaced wholesale, not deep-merged")
}

// Config only merges between tiers that agree on the kind. A tier that switches
// the backend brings its own config wholesale, because another backend's
// options are meaningless to it.
func TestResolveSandbox_ConfigNotInheritedAcrossDifferentKinds(t *testing.T) {
	got, _, vs := resolveSandbox(Inputs{
		Cluster: &v1.SettingsSpec{Defaults: &v1.SettingsDefaults{
			Sandbox: backend("kind-a", `{"only-meaningful-to-a":1}`),
		}},
		BundleSandbox: map[string]BundleSandboxInputs{
			"demo-bundle": {FromAgentClass: backend("kind-b", `{"for-b":2}`)},
		},
	}, map[string]string{})
	require.Empty(t, vs)

	assert.Equal(t, "kind-b", got["demo-bundle"].Kind)
	assert.JSONEq(t, `{"for-b":2}`, string(got["demo-bundle"].Config.Raw))
}

// Config that is not a JSON object at all is detectable here, so it must be
// reported rather than silently dropped — but it is not fatal, because the
// resolved backend is still usable.
func TestResolveSandbox_MalformedConfigIsReportedNotDropped(t *testing.T) {
	got, _, vs := resolveSandbox(Inputs{
		BundleSandbox: map[string]BundleSandboxInputs{
			"demo-bundle": {FromAgentClass: backend("demo-kind", `["not","an","object"]`)},
		},
	}, map[string]string{})

	require.Len(t, vs, 1, "a skipped config value must be surfaced")
	assert.False(t, vs[0].Fatal, "the resolved backend is still usable")
	assert.Contains(t, vs[0].Message, "demo-bundle")
	assert.Equal(t, "demo-kind", got["demo-bundle"].Kind, "the kind still resolves")
	assert.Equal(t, v1.ReasonSandboxConfigIgnored, vs[0].Reason,
		"distinct from ReasonSandboxKindNotPermitted: the backend is usable, only its config was ignored")
}

// The per-bundle fold must NEVER report WarmPool as effective, whichever tier
// sets it. Its result lands on AgentSession.status.effectiveSettings.sandbox and
// NOTHING reads WarmPool from there — pool sizing runs entirely through
// ResolveClassWarmPool (class + cluster only, tested below). Resolving it here
// would confirm on status a value never honored: an admin setting warmPool on
// the AgentClass's per-bundle override — the MOST specific tier — would get an
// affirmative report and no pool, with no error and no log.
//
// Every tier is populated here, so the assertion cannot pass merely because the
// fixture forgot to set one.
func TestResolveSandbox_NeverReportsWarmPoolAsEffective(t *testing.T) {
	got, _, vs := resolveSandbox(Inputs{
		Cluster:   specWithWarmPool(3),
		Namespace: specWithWarmPool(5),
		BundleSandbox: map[string]BundleSandboxInputs{
			"demo-bundle": {
				FromAgentClass: &v1.SandboxBackend{
					WarmPool: &v1.WarmPoolConfig{Replicas: 9, Namespaces: []string{"ns-a"}},
				},
				FromSpiceboxClass: &v1.SandboxBackend{
					Kind:     "class-kind",
					WarmPool: &v1.WarmPoolConfig{Replicas: 7},
				},
			},
		},
	}, map[string]string{})

	require.Empty(t, vs)
	require.Contains(t, got, "demo-bundle")
	assert.Equal(t, "class-kind", got["demo-bundle"].Kind,
		"Kind still resolves normally; only WarmPool is out of this fold's scope")
	assert.Nil(t, got["demo-bundle"].WarmPool,
		"warmPool must not be reported as effective on a path that never sizes a pool")
}

// ResolveClassWarmPool is the SpiceboxClass controller's entry point: only
// two tiers (class, cluster) — see the function's own doc comment for why a
// namespace tier does not apply to a cluster-scoped class.
func TestResolveClassWarmPool_ClassThenClusterPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		class   v1.SandboxBackend
		cluster *v1.SettingsSpec
		want    *v1.WarmPoolConfig
	}{
		{
			name:  "no tier sets WarmPool: nil, pre-warming stays off",
			class: v1.SandboxBackend{Kind: "agent-sandbox"},
		},
		{
			name:    "only the cluster sets a default: cluster wins",
			class:   v1.SandboxBackend{Kind: "agent-sandbox"},
			cluster: specWithWarmPool(3),
			want:    &v1.WarmPoolConfig{Replicas: 3},
		},
		{
			name: "the class's own warmPool overrides the cluster default",
			class: v1.SandboxBackend{Kind: "agent-sandbox",
				WarmPool: &v1.WarmPoolConfig{Replicas: 7, Namespaces: []string{"ns-a"}}},
			cluster: specWithWarmPool(3),
			want:    &v1.WarmPoolConfig{Replicas: 7, Namespaces: []string{"ns-a"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveClassWarmPool(tc.class, tc.cluster)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestResolveSandbox_EveryBundleAppears(t *testing.T) {
	got, _, vs := resolveSandbox(Inputs{
		BundleSandbox: map[string]BundleSandboxInputs{
			"alpha": {},
			"bravo": {FromSpiceboxClass: backend("other", "")},
		},
	}, map[string]string{})
	require.Empty(t, vs)

	require.Len(t, got, 2)
	assert.Equal(t, v1.DefaultSandboxKind, got["alpha"].Kind)
	assert.Equal(t, "other", got["bravo"].Kind)
}
