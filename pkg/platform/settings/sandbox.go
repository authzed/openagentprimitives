package settings

import (
	"encoding/json"
	"fmt"
	"sort"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// sandboxTier is one candidate in the precedence chain, most specific first.
type sandboxTier struct {
	name    string // recorded as provenance
	backend *v1.SandboxBackend
}

// resolveSandbox folds the four sandbox tiers per bundle and checks each
// resolved kind against the ceiling.
//
// Precedence, most specific first: the AgentClass's per-bundle override, the
// referenced SpiceboxClass's own setting, the namespace default, the cluster
// default, then v1.DefaultSandboxKind. A tier naming an empty kind expresses no
// preference and is skipped rather than treated as a choice.
//
// Pure: every tier arrives through Inputs, including the SpiceboxClass one the
// caller had to read.
func resolveSandbox(in Inputs, prov map[string]string) (map[string]v1.SandboxBackend, []string, []Violation) {
	allowed := intersectAllowlist(limitSandboxKinds(in.Cluster), limitSandboxKinds(in.Namespace))

	if len(in.BundleSandbox) == 0 {
		return nil, allowed, nil
	}

	out := make(map[string]v1.SandboxBackend, len(in.BundleSandbox))
	var vs []Violation

	// Sorted so violations and provenance are deterministic across runs.
	names := make([]string, 0, len(in.BundleSandbox))
	for name := range in.BundleSandbox {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		b := in.BundleSandbox[name]
		tiers := []sandboxTier{
			{"agentclass", b.FromAgentClass},
			{"spiceboxclass", b.FromSpiceboxClass},
			{"namespace", defaultSandbox(in.Namespace)},
			{"cluster", defaultSandbox(in.Cluster)},
		}

		resolved, source, skipped := foldSandboxTiers(tiers)
		prov[fmt.Sprintf("sandbox.%s.kind", name)] = source
		out[name] = resolved

		if allowed != nil && !contains(allowed, resolved.Kind) {
			vs = append(vs, Violation{
				Reason: v1.ReasonSandboxKindNotPermitted,
				Message: fmt.Sprintf(
					"bundle %q resolves to sandbox kind %q, which is not permitted here (allowed: %v)",
					name, resolved.Kind, allowed),
				Fatal: true,
			})
		}

		if skipped > 0 {
			vs = append(vs, Violation{
				Reason: v1.ReasonSandboxConfigIgnored,
				Message: fmt.Sprintf(
					"bundle %q: %d sandbox config value(s) were not JSON objects and were ignored",
					name, skipped),
				Fatal: false,
			})
		}
	}
	return out, allowed, vs
}

// foldSandboxTiers walks tiers most-specific-first, returning the resolved
// backend, the name of the tier that decided its kind, and the count of
// config entries mergeSandboxConfig had to skip because they were not JSON
// objects.
//
// WarmPool is DELIBERATELY NOT RESOLVED HERE; the returned SandboxBackend
// always leaves it nil. This fold's result is reported as the EFFECTIVE
// per-bundle sandbox settings (AgentSession.status.effectiveSettings.sandbox),
// and nothing reads WarmPool from there: pool sizing runs through
// settings.ResolveClassWarmPool, once per class reconcile by the SpiceboxClass
// controller, over the class and cluster tiers alone.
//
// Folding it here would report a value as effective on two tiers that never size
// a pool: an admin setting warmPool on AgentClass.spec.toolBundles[].sandbox —
// the MOST specific tier — would see status confirm it while no pool was made,
// with no error and no log. One resolution path for this field, and it is the
// one that is honored.
func foldSandboxTiers(tiers []sandboxTier) (v1.SandboxBackend, string, int) {
	winner := -1
	for i, t := range tiers {
		if t.backend != nil && t.backend.Kind != "" {
			winner = i
			break
		}
	}
	if winner < 0 {
		// No tier expressed a preference. Config from a tier that named no kind
		// is not inherited: it would be configuration for an unstated backend.
		return v1.SandboxBackend{Kind: v1.DefaultSandboxKind}, "default", 0
	}

	kind := tiers[winner].backend.Kind
	// Merge config from the winner outward through the less-specific tiers that
	// AGREE on the kind. A tier naming a different backend contributes nothing:
	// its options are meaningless to the backend that won.
	var configs []*apiextensionsv1.JSON
	for i := len(tiers) - 1; i >= winner; i-- {
		t := tiers[i]
		if t.backend == nil || t.backend.Config == nil {
			continue
		}
		if t.backend.Kind != "" && t.backend.Kind != kind {
			continue
		}
		configs = append(configs, t.backend.Config)
	}

	merged, skipped := mergeSandboxConfig(configs)
	return v1.SandboxBackend{Kind: kind, Config: merged}, tiers[winner].name, skipped
}

// resolveWarmPool scans tiers most-specific-first for the first that sets
// WarmPool. nil when none does — distinct from a zero-value WarmPoolConfig:
// nil is "no preference", Replicas: 0 is an explicit "pre-warming is off".
//
// ResolveClassWarmPool is its ONLY caller, deliberately — see foldSandboxTiers
// for why the per-bundle fold does not resolve this field at all.
func resolveWarmPool(tiers []sandboxTier) *v1.WarmPoolConfig {
	for _, t := range tiers {
		if t.backend != nil && t.backend.WarmPool != nil {
			return t.backend.WarmPool
		}
	}
	return nil
}

// ResolveClassWarmPool is the entry point the SpiceboxClass controller calls
// directly, once per class reconcile, to size the class's warm pool. Only TWO
// tiers — the class's own spec.sandbox.warmPool, then the cluster default —
// unlike Kind's four-tier chain (agentclass > spiceboxclass > namespace >
// cluster, folded by resolveSandbox for a specific bundle).
//
// There is deliberately no namespace tier. AgentSettings is namespaced, but
// SpiceboxClass is cluster-scoped and can be referenced by bundles across many
// namespaces at once, so no single AgentSettings is "the" namespace tier for a
// fact this object owns. Resolution stops at the cluster tier; the namespace
// tier stays exclusive to resolveSandbox's per-bundle chain, where a concrete
// namespace genuinely exists.
func ResolveClassWarmPool(class v1.SandboxBackend, cluster *v1.SettingsSpec) *v1.WarmPoolConfig {
	return resolveWarmPool([]sandboxTier{
		{"spiceboxclass", &class},
		{"cluster", defaultSandbox(cluster)},
	})
}

// mergeSandboxConfig shallow-merges JSON objects left to right: later entries
// win per top-level key. Shallow by design — AP does not interpret the blob, so
// deep-merging it would be guessing at a backend's schema.
//
// Returns the merged object plus the count of entries that were not JSON objects
// at all. Those are skipped rather than fatal (the resolved backend is still
// usable), but the caller reports them: an override silently vanishing is worse
// than one that runs with a warning.
func mergeSandboxConfig(configs []*apiextensionsv1.JSON) (*apiextensionsv1.JSON, int) {
	if len(configs) == 0 {
		return nil, 0
	}
	merged := map[string]json.RawMessage{}
	any := false
	skipped := 0
	for _, c := range configs {
		if c == nil || len(c.Raw) == 0 {
			skipped++
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(c.Raw, &m); err != nil {
			// Not a JSON object at all (array, string, number, bool) — a key AP
			// doesn't recognize is legitimate passthrough, but a structurally wrong
			// shape is detectable here and must be reported, not silently dropped.
			skipped++
			continue
		}
		for k, v := range m {
			merged[k] = v
		}
		any = true
	}
	if !any {
		return nil, skipped
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return nil, skipped
	}
	return &apiextensionsv1.JSON{Raw: raw}, skipped
}

func defaultSandbox(s *v1.SettingsSpec) *v1.SandboxBackend {
	if s == nil || s.Defaults == nil {
		return nil
	}
	return s.Defaults.Sandbox
}

func limitSandboxKinds(s *v1.SettingsSpec) *[]string {
	if s == nil || s.Limits == nil {
		return nil
	}
	return s.Limits.AllowedSandboxKinds
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}
