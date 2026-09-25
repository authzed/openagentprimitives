package settings

import (
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
)

// Resolve folds the 4-tier chain into EffectiveSettings plus any Violations.
// It is pure: no I/O, no clock, no client. Callers populate Inputs from CRDs.
func Resolve(in Inputs) (EffectiveSettings, []Violation) {
	out := EffectiveSettings{Provenance: map[string]string{}}
	var vs []Violation

	budget, budgetVs := resolveBudget(in, out.Provenance)
	out.Budget = budget
	vs = append(vs, budgetVs...)

	model, modelSrc, inPrice, outPrice, modelRouting, modelVs := resolveModel(in, out.Provenance)
	out.Model = model
	out.ModelTokenSource = modelSrc
	out.ModelInputPerMTok = inPrice
	out.ModelOutputPerMTok = outPrice
	out.ModelRouting = modelRouting
	vs = append(vs, modelVs...)

	out.AllowedToolkits = intersectAllowlist(limitToolkits(in.Cluster), limitToolkits(in.Namespace))
	vs = append(vs, checkToolkits(in.ClassToolkits, out.AllowedToolkits)...)

	out.AllowedMCP = foldMCP(in.Cluster, in.Namespace)
	vs = append(vs, checkMCP(in.ClassMCP, out.AllowedMCP)...)

	out.AllowedSkills, out.DeniedSkills, _ = resolveSkillSets(in)
	out.Pinning = effectivePinning(in)
	toolGuard, toolGuardVs := effectiveToolGuard(in)
	out.ToolGuard = toolGuard
	vs = append(vs, toolGuardVs...)
	vs = append(vs, checkSkills(in)...)
	vs = append(vs, checkPinning(in, out.Pinning)...)

	authz, authzVs := resolveAuthz(in, out.Provenance)
	out.Authz = authz
	vs = append(vs, authzVs...)

	// ContentInspectors: union across tiers (ceiling — lower tiers add, never
	// weaken). No dedup: duplicate instances are strictly stricter (AND).
	for _, spec := range []*v1.SettingsSpec{in.Cluster, in.Namespace} {
		if spec == nil || spec.Limits == nil || spec.Limits.ContentInspectors == nil {
			continue
		}
		out.ContentInspectors = append(out.ContentInspectors, (*spec.Limits.ContentInspectors)...)
	}

	sandbox, allowedSandbox, sandboxVs := resolveSandbox(in, out.Provenance)
	out.Sandbox = sandbox
	out.AllowedSandboxKinds = allowedSandbox
	vs = append(vs, sandboxVs...)

	out.ReportSessionCost = resolveReportSessionCost(in, out.Provenance)

	out.NativeFileHandling = resolveNativeFileHandling(in)

	out.RequireSubagentDigestPins = resolveRequireSubagentDigestPins(in)

	out.RequireStandingFor = resolveRequireStandingFor(in)

	return out, vs
}

// intersectAllowlist folds tri-state allowlists. A nil pointer contributes no
// constraint; a non-nil (possibly empty) list constrains. The result is the
// intersection of all non-nil tiers: nil if no tier constrained, otherwise a
// non-nil slice (empty = deny-all). Order follows the first constraining tier.
func intersectAllowlist(tiers ...*[]string) []string {
	var acc []string
	seeded := false
	for _, t := range tiers {
		if t == nil {
			continue
		}
		if !seeded {
			acc = append([]string{}, (*t)...)
			seeded = true
			continue
		}
		set := map[string]bool{}
		for _, s := range *t {
			set[s] = true
		}
		var next []string
		for _, s := range acc {
			if set[s] {
				next = append(next, s)
			}
		}
		acc = next
	}
	if !seeded {
		return nil
	}
	if acc == nil {
		acc = []string{}
	}
	return acc
}

// dimResult is one resolved budget dimension plus whether it was explicitly
// requested (vs. inherited as a default) and whether it got clamped.
type dimResult struct {
	value          int64
	explicit       bool
	clamped        bool
	provenanceTier string
}

func resolveBudget(in Inputs, prov map[string]string) (v1.BudgetConfig, []Violation) {
	var vs []Violation

	turns := resolveDim(
		pick(in.SessionBudget, func(b *v1.BudgetConfig) int64 { return int64(b.MaxTurns) }),
		pick(in.ClassBudget, func(b *v1.BudgetConfig) int64 { return int64(b.MaxTurns) }),
		defDim(in.Namespace, func(d *v1.SettingsDefaults) int64 { return budgetTurns(d.Budget) }),
		defDim(in.Cluster, func(d *v1.SettingsDefaults) int64 { return budgetTurns(d.Budget) }),
		ceilDim(in.ClassBudget, func(b *v1.BudgetConfig) int64 { return int64(b.MaxTurns) }),
		ceilDim(in.RootBudget, func(b *v1.BudgetConfig) int64 { return int64(b.MaxTurns) }),
		ceilLimit(in.Namespace, func(c *v1.SettingsBudgetCeiling) int64 { return int64(c.MaxTurns) }),
		ceilLimit(in.Cluster, func(c *v1.SettingsBudgetCeiling) int64 { return int64(c.MaxTurns) }),
	)
	tokens := resolveDim(
		pick(in.SessionBudget, func(b *v1.BudgetConfig) int64 { return b.MaxTokens }),
		pick(in.ClassBudget, func(b *v1.BudgetConfig) int64 { return b.MaxTokens }),
		defDim(in.Namespace, func(d *v1.SettingsDefaults) int64 { return budgetTokens(d.Budget) }),
		defDim(in.Cluster, func(d *v1.SettingsDefaults) int64 { return budgetTokens(d.Budget) }),
		ceilDim(in.ClassBudget, func(b *v1.BudgetConfig) int64 { return b.MaxTokens }),
		ceilDim(in.RootBudget, func(b *v1.BudgetConfig) int64 { return b.MaxTokens }),
		ceilLimit(in.Namespace, func(c *v1.SettingsBudgetCeiling) int64 { return c.MaxTokens }),
		ceilLimit(in.Cluster, func(c *v1.SettingsBudgetCeiling) int64 { return c.MaxTokens }),
	)
	durNs := resolveDim(
		pick(in.SessionBudget, func(b *v1.BudgetConfig) int64 { return int64(b.MaxDuration.Duration) }),
		pick(in.ClassBudget, func(b *v1.BudgetConfig) int64 { return int64(b.MaxDuration.Duration) }),
		defDim(in.Namespace, func(d *v1.SettingsDefaults) int64 { return budgetDur(d.Budget) }),
		defDim(in.Cluster, func(d *v1.SettingsDefaults) int64 { return budgetDur(d.Budget) }),
		ceilDim(in.ClassBudget, func(b *v1.BudgetConfig) int64 { return int64(b.MaxDuration.Duration) }),
		ceilDim(in.RootBudget, func(b *v1.BudgetConfig) int64 { return int64(b.MaxDuration.Duration) }),
		ceilLimit(in.Namespace, func(c *v1.SettingsBudgetCeiling) int64 { return int64(c.MaxDuration.Duration) }),
		ceilLimit(in.Cluster, func(c *v1.SettingsBudgetCeiling) int64 { return int64(c.MaxDuration.Duration) }),
	)

	sessionExp := resolveDim(
		pick(in.SessionBudget, func(b *v1.BudgetConfig) int64 { return int64(b.SessionExpiration.Duration) }),
		pick(in.ClassBudget, func(b *v1.BudgetConfig) int64 { return int64(b.SessionExpiration.Duration) }),
		defDim(in.Namespace, func(d *v1.SettingsDefaults) int64 { return budgetSessionExp(d.Budget) }),
		defDim(in.Cluster, func(d *v1.SettingsDefaults) int64 { return budgetSessionExp(d.Budget) }),
		ceilDim(in.ClassBudget, func(b *v1.BudgetConfig) int64 { return int64(b.SessionExpiration.Duration) }),
		ceilDim(in.RootBudget, func(b *v1.BudgetConfig) int64 { return int64(b.SessionExpiration.Duration) }),
		ceilLimit(in.Namespace, func(c *v1.SettingsBudgetCeiling) int64 { return int64(c.SessionExpiration.Duration) }),
		ceilLimit(in.Cluster, func(c *v1.SettingsBudgetCeiling) int64 { return int64(c.SessionExpiration.Duration) }),
	)

	// agents (MaxDelegatedAgents) is a property of the delegation TREE, read
	// only from the root — unlike every sibling dimension above, it deliberately
	// carries no ceilDim(in.RootBudget, ...) arm. Capping a child's own copy of
	// this field by the root's copy would treat it as a per-session value, which
	// it is not: Task 7 reads it off the root's effective settings alone.
	agents := resolveDim(
		pick(in.SessionBudget, func(b *v1.BudgetConfig) int64 { return int64(b.MaxDelegatedAgents) }),
		pick(in.ClassBudget, func(b *v1.BudgetConfig) int64 { return int64(b.MaxDelegatedAgents) }),
		defDim(in.Namespace, func(d *v1.SettingsDefaults) int64 { return budgetAgents(d.Budget) }),
		defDim(in.Cluster, func(d *v1.SettingsDefaults) int64 { return budgetAgents(d.Budget) }),
		ceilDim(in.ClassBudget, func(b *v1.BudgetConfig) int64 { return int64(b.MaxDelegatedAgents) }),
		ceilLimit(in.Namespace, func(c *v1.SettingsBudgetCeiling) int64 { return int64(c.MaxDelegatedAgents) }),
		ceilLimit(in.Cluster, func(c *v1.SettingsBudgetCeiling) int64 { return int64(c.MaxDelegatedAgents) }),
	)

	// One aggregate BudgetClamped warning listing every explicitly-requested
	// dimension that got capped (clamping an inherited default is silent).
	dims := []struct {
		name string
		d    dimResult
	}{{"maxTurns", turns}, {"maxTokens", tokens}, {"maxDuration", durNs}, {"sessionExpiration", sessionExp}, {"maxDelegatedAgents", agents}}
	var clampedDims []string
	for _, dd := range dims {
		if dd.d.value > 0 && dd.d.provenanceTier != "" {
			prov["budget."+dd.name] = dd.d.provenanceTier
		}
		if dd.d.clamped && dd.d.explicit {
			clampedDims = append(clampedDims, dd.name)
		}
	}
	if len(clampedDims) > 0 {
		vs = append(vs, Violation{
			Reason:  ReasonBudgetClamped,
			Message: "budget dimensions capped by a ceiling: " + strings.Join(clampedDims, ", "),
			Fatal:   false,
		})
	}
	// Single top-level provenance hint for the budget origin (nearest explicit).
	switch {
	case in.SessionBudget != nil:
		prov["budget"] = "session"
	case in.ClassBudget != nil:
		prov["budget"] = "class"
	case in.Namespace != nil && in.Namespace.Defaults != nil && in.Namespace.Defaults.Budget != nil:
		prov["budget"] = "namespace"
	case in.Cluster != nil && in.Cluster.Defaults != nil && in.Cluster.Defaults.Budget != nil:
		prov["budget"] = "cluster"
	}

	return v1.BudgetConfig{
		MaxTurns:           int32(turns.value),
		MaxTokens:          tokens.value,
		MaxDuration:        metav1.Duration{Duration: time.Duration(durNs.value)},
		SessionExpiration:  metav1.Duration{Duration: time.Duration(sessionExp.value)},
		MaxDelegatedAgents: int32(agents.value),
	}, vs
}

// requested is a value the lower tiers supply (explicit picks then defaults).
type requested struct {
	value    int64
	set      bool
	explicit bool // true for session/class picks, false for inherited defaults
	tier     string
}

// ceiling is a value a tier imposes as an upper bound (0 = no ceiling).
type ceiling struct {
	value int64
	set   bool
}

func pick(b *v1.BudgetConfig, get func(*v1.BudgetConfig) int64) requested {
	if b == nil {
		return requested{}
	}
	v := get(b)
	if v <= 0 {
		return requested{}
	}
	return requested{value: v, set: true, explicit: true}
}

func defDim(s *v1.SettingsSpec, get func(*v1.SettingsDefaults) int64) requested {
	if s == nil || s.Defaults == nil {
		return requested{}
	}
	v := get(s.Defaults)
	if v <= 0 {
		return requested{}
	}
	return requested{value: v, set: true, explicit: false}
}

func ceilDim(b *v1.BudgetConfig, get func(*v1.BudgetConfig) int64) ceiling {
	if b == nil {
		return ceiling{}
	}
	v := get(b)
	if v <= 0 {
		return ceiling{}
	}
	return ceiling{value: v, set: true}
}

func ceilLimit(s *v1.SettingsSpec, get func(*v1.SettingsBudgetCeiling) int64) ceiling {
	if s == nil || s.Limits == nil || s.Limits.Budget == nil {
		return ceiling{}
	}
	v := get(s.Limits.Budget)
	if v <= 0 {
		return ceiling{}
	}
	return ceiling{value: v, set: true}
}

// resolveDim takes the requested picks (session, class) then the default picks
// (namespace, cluster) — first set wins — then applies the tightest ceiling.
// Two distinct outcomes come out of that last step:
//
//   - Something was requested and exceeds the ceiling ⇒ value clamped, clamped
//     true, provenanceTier "clamped".
//   - NOTHING was requested but a ceiling exists ⇒ the ceiling silently BECOMES
//     the value. Not a clamp: nothing was reduced, so clamped stays false,
//     provenanceTier stays empty, no warning is raised. This is a pinned
//     contract — a ceiling with no request anywhere is how an admin sets a
//     default budget.
func resolveDim(session, class, nsDef, clDef requested, ceilings ...ceiling) dimResult {
	var req requested
	for _, r := range []requested{session, class, nsDef, clDef} {
		if r.set {
			req = r
			break
		}
	}
	// tier mirrors the selection loop above (same priority order); kept as a
	// separate switch so the pick/defDim helpers stay tier-agnostic.
	switch {
	case session.set:
		req.tier = "session"
	case class.set:
		req.tier = "class"
	case nsDef.set:
		req.tier = "namespace"
	case clDef.set:
		req.tier = "cluster"
	}

	limit := int64(0)
	for _, c := range ceilings {
		if c.set && (limit == 0 || c.value < limit) {
			limit = c.value
		}
	}

	res := dimResult{value: req.value, explicit: req.explicit, provenanceTier: req.tier}
	if limit > 0 && (req.value == 0 || limit < req.value) {
		if req.value > limit {
			res.clamped = true
			res.provenanceTier = "clamped"
		}
		res.value = limit
	}
	return res
}

func budgetTurns(b *v1.BudgetConfig) int64 {
	if b == nil {
		return 0
	}
	return int64(b.MaxTurns)
}
func budgetTokens(b *v1.BudgetConfig) int64 {
	if b == nil {
		return 0
	}
	return b.MaxTokens
}
func budgetDur(b *v1.BudgetConfig) int64 {
	if b == nil {
		return 0
	}
	return int64(b.MaxDuration.Duration)
}
func budgetSessionExp(b *v1.BudgetConfig) int64 {
	if b == nil {
		return 0
	}
	return int64(b.SessionExpiration.Duration)
}
func budgetAgents(b *v1.BudgetConfig) int64 {
	if b == nil {
		return 0
	}
	return int64(b.MaxDelegatedAgents)
}

// clusterCatalog returns the cluster-tier registry (entries carry tokens), or nil.
func clusterCatalog(in Inputs) []v1.ModelCatalogEntry {
	if in.Cluster == nil || in.Cluster.ModelCatalog == nil {
		return nil
	}
	return *in.Cluster.ModelCatalog
}

// effectiveCatalog narrows the cluster catalog to names also listed by the
// namespace tier (token/provider always come from the cluster entry). nil when
// the cluster set no catalog (legacy path).
func effectiveCatalog(in Inputs) []v1.ModelCatalogEntry {
	cl := clusterCatalog(in)
	if cl == nil {
		return nil
	}
	if in.Namespace == nil || in.Namespace.ModelCatalog == nil {
		return cl
	}
	ns := map[string]bool{}
	for _, e := range *in.Namespace.ModelCatalog {
		ns[e.Name] = true
	}
	out := make([]v1.ModelCatalogEntry, 0)
	for _, e := range cl {
		if ns[e.Name] {
			out = append(out, e)
		}
	}
	return out
}

func deniedModelSet(in Inputs) map[string]bool {
	set := map[string]bool{}
	for _, s := range []*v1.SettingsSpec{in.Cluster, in.Namespace} {
		if s == nil || s.Limits == nil {
			continue
		}
		for _, n := range s.Limits.DeniedModels {
			set[n] = true
		}
	}
	return set
}

// allowModelOverride folds top-down: the cluster must grant it; the namespace
// can only further restrict. Default false.
func allowModelOverride(in Inputs) bool {
	grant := false
	if in.Cluster != nil && in.Cluster.Limits != nil && in.Cluster.Limits.AllowModelOverride != nil {
		grant = *in.Cluster.Limits.AllowModelOverride
	}
	if !grant {
		return false
	}
	if in.Namespace != nil && in.Namespace.Limits != nil && in.Namespace.Limits.AllowModelOverride != nil {
		return *in.Namespace.Limits.AllowModelOverride
	}
	return true
}

// resolveNativeFileHandling folds top-down like allowModelOverride: the
// cluster must grant native (Tier-2, provider-native sandbox) file handling;
// the namespace can only further restrict. Default false.
func resolveNativeFileHandling(in Inputs) bool {
	grant := false
	if in.Cluster != nil && in.Cluster.Limits != nil && in.Cluster.Limits.NativeFileHandling != nil {
		grant = *in.Cluster.Limits.NativeFileHandling
	}
	if !grant {
		return false
	}
	if in.Namespace != nil && in.Namespace.Limits != nil && in.Namespace.Limits.NativeFileHandling != nil {
		return *in.Namespace.Limits.NativeFileHandling
	}
	return true
}

// resolveRequireSubagentDigestPins folds the requirement top-down: a tier
// that sets true wins over everything below it (a requirement, once made,
// cannot be relaxed by a narrower scope); with the cluster silent or
// explicitly off, the namespace may still add the requirement for itself.
func resolveRequireSubagentDigestPins(in Inputs) bool {
	if in.Cluster != nil && in.Cluster.Limits != nil &&
		in.Cluster.Limits.RequireSubagentDigestPins != nil && *in.Cluster.Limits.RequireSubagentDigestPins {
		return true
	}
	if in.Namespace != nil && in.Namespace.Limits != nil &&
		in.Namespace.Limits.RequireSubagentDigestPins != nil {
		return *in.Namespace.Limits.RequireSubagentDigestPins
	}
	return false
}

// resolveRequireStandingFor unions the admin veto across tiers.
//
// UNION rather than override, and that is the security property. A namespace
// listing one type must not silently drop the cluster's other entries — the
// same ratchet-up-only rule MinPlanGateMode follows, expressed for a set.
func resolveRequireStandingFor(in Inputs) []string {
	seen := map[string]struct{}{}
	for _, s := range []*v1.SettingsSpec{in.Cluster, in.Namespace} {
		if s == nil || s.Limits == nil {
			continue
		}
		for _, t := range s.Limits.RequireStandingFor {
			if t != "" {
				seen[t] = struct{}{}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func defModel(s *v1.SettingsSpec) *v1.DefaultModel {
	if s == nil || s.Defaults == nil {
		return nil
	}
	return s.Defaults.Model
}

// pickModelName chooses the model name + provenance tier: class (FromCatalog or
// Name) → namespace default → cluster default → the effective catalog's Default.
func pickModelName(in Inputs, catalog []v1.ModelCatalogEntry) (string, string) {
	if in.ClassModel != nil {
		if in.ClassModel.FromCatalog != "" {
			return in.ClassModel.FromCatalog, "class"
		}
		if in.ClassModel.Name != "" {
			return in.ClassModel.Name, "class"
		}
	}
	if dm := defModel(in.Namespace); dm != nil {
		if dm.FromCatalog != "" {
			return dm.FromCatalog, "namespace"
		}
		if dm.Name != "" {
			return dm.Name, "namespace"
		}
	}
	if dm := defModel(in.Cluster); dm != nil {
		if dm.FromCatalog != "" {
			return dm.FromCatalog, "cluster"
		}
		if dm.Name != "" {
			return dm.Name, "cluster"
		}
	}
	for _, e := range catalog {
		if e.Default {
			return e.Name, "cluster"
		}
	}
	return "", ""
}

func pickProvider(in Inputs) string {
	if in.ClassModel != nil && in.ClassModel.Provider != "" {
		return in.ClassModel.Provider
	}
	if dm := defModel(in.Namespace); dm != nil && dm.Provider != "" {
		return dm.Provider
	}
	if dm := defModel(in.Cluster); dm != nil && dm.Provider != "" {
		return dm.Provider
	}
	return ""
}

func classAPIKey(in Inputs) *v1.SecretKeyRef {
	if in.ClassModel != nil && in.ClassModel.APIKey.Name != "" {
		k := in.ClassModel.APIKey
		return &k
	}
	return nil
}

func defaultModelAPIKey(in Inputs) *v1.SecretKeyRef {
	if dm := defModel(in.Namespace); dm != nil && dm.APIKey != nil {
		return dm.APIKey
	}
	if dm := defModel(in.Cluster); dm != nil && dm.APIKey != nil {
		return dm.APIKey
	}
	return nil
}

// resolveModel returns the effective model, the catalog token source (nil on
// the bring-your-own and no-catalog paths), the catalog entry's input/output
// price (0, 0 on every non-catalog-success path), the OpenRouter routing
// preferences (nil unless the entry's Provider is "openrouter"), and any
// violations.
func resolveModel(in Inputs, prov map[string]string) (v1.ModelConfig, *v1.NamespacedSecretKeyRef, float64, float64, *v1.OpenRouterRouting, []Violation) {
	var vs []Violation
	var m v1.ModelConfig

	catalog := effectiveCatalog(in)
	byName := map[string]v1.ModelCatalogEntry{}
	for _, e := range catalog {
		byName[e.Name] = e
	}

	name, tier := pickModelName(in, catalog)
	if tier != "" {
		prov["model"] = tier
	}
	if name == "" {
		if in.ForSession {
			vs = append(vs, Violation{Reason: ReasonModelMissingModel, Message: "no model resolved from any tier", Fatal: true})
		}
		return m, nil, 0, 0, nil, vs
	}
	m.Name = name
	byo := classAPIKey(in)

	// Legacy path: no catalog set anywhere — behave as before, no catalog gate.
	if catalog == nil {
		m.Provider = pickProvider(in)
		switch {
		case byo != nil:
			m.APIKey = *byo
		default:
			if k := defaultModelAPIKey(in); k != nil {
				m.APIKey = *k
			}
		}
		if m.APIKey.Name == "" {
			vs = append(vs, Violation{Reason: ReasonModelMissingCredential, Message: "resolved model " + name + " has no reachable apiKey", Fatal: in.ForSession})
		}
		return m, nil, 0, 0, nil, vs
	}

	// Catalog path.
	if deniedModelSet(in)[name] {
		vs = append(vs, Violation{Reason: ReasonModelForbidden, Message: "model " + name + " is denied by a deniedModels rule", Fatal: true})
		return m, nil, 0, 0, nil, vs
	}
	if byo != nil {
		if !allowModelOverride(in) {
			vs = append(vs, Violation{Reason: ReasonModelOverrideNotAllowed, Message: "AgentClass supplies its own model apiKey but allowModelOverride is not granted", Fatal: true})
		}
		m.Provider = pickProvider(in)
		m.APIKey = *byo
		return m, nil, 0, 0, nil, vs
	}
	entry, ok := byName[name]
	if !ok {
		vs = append(vs, Violation{Reason: ReasonModelNotInCatalog, Message: "model " + name + " is not in the effective model catalog", Fatal: true})
		return m, nil, 0, 0, nil, vs
	}
	m.Provider = entry.Provider
	if entry.TokenRef == nil {
		vs = append(vs, Violation{Reason: ReasonModelMissingCredential, Message: "catalog model " + name + " has no tokenRef", Fatal: in.ForSession})
		return m, nil, 0, 0, nil, vs
	}
	src := *entry.TokenRef
	// Routing is meaningful only for the openrouter provider; any other entry's
	// Routing is dropped rather than surfaced. For openrouter, the AgentClass's
	// RoutingMetadata is merged over the catalog's with narrowing-only semantics
	// (mergeRouting): the agent may constrain admin policy, never loosen it.
	var routing *v1.OpenRouterRouting
	if entry.Provider == "openrouter" {
		var metadata *v1.OpenRouterRouting
		if in.ClassModel != nil {
			metadata = in.ClassModel.RoutingMetadata
		}
		routing = mergeRouting(entry.Routing, metadata)
	}
	return m, &src, entry.InputPerMTok, entry.OutputPerMTok, routing, vs
}

// allowedContains reports whether name is permitted. A nil allowed slice means
// unconstrained (everything permitted); an empty non-nil slice is deny-all.
func allowedContains(allowed []string, name string) bool {
	if allowed == nil {
		return true
	}
	for _, a := range allowed {
		if a == name {
			return true
		}
	}
	return false
}

func limitToolkits(s *v1.SettingsSpec) *[]string {
	if s == nil || s.Limits == nil {
		return nil
	}
	return s.Limits.AllowedToolkits
}

func checkToolkits(requested, allowed []string) []Violation {
	if allowed == nil {
		return nil
	}
	var vs []Violation
	for _, tk := range requested {
		if !allowedContains(allowed, tk) {
			vs = append(vs, Violation{Reason: ReasonToolkitNotAllowed, Message: "toolkit " + tk + " is not in the effective allowed-toolkits set", Fatal: true})
		}
	}
	return vs
}

// foldMCP intersects the per-tier AllowedMCPServers by server name, intersecting
// each server's tool list. Returns nil if no tier constrained MCP servers.
func foldMCP(tiers ...*v1.SettingsSpec) []v1.AllowedMCPServer {
	var acc []v1.AllowedMCPServer
	seeded := false
	for _, s := range tiers {
		if s == nil || s.Limits == nil || s.Limits.AllowedMCPServers == nil {
			continue
		}
		cur := *s.Limits.AllowedMCPServers
		if !seeded {
			acc = append([]v1.AllowedMCPServer{}, cur...)
			seeded = true
			continue
		}
		byName := map[string]v1.AllowedMCPServer{}
		for _, m := range cur {
			byName[m.Name] = m
		}
		var next []v1.AllowedMCPServer
		for _, a := range acc {
			if m, ok := byName[a.Name]; ok {
				next = append(next, v1.AllowedMCPServer{Name: a.Name, Tools: intersectTools(a.Tools, m.Tools)})
			}
		}
		acc = next
		if acc == nil {
			acc = []v1.AllowedMCPServer{}
		}
	}
	if !seeded {
		return nil
	}
	return acc
}

// intersectTools intersects two per-server tool lists. A nil/empty list or one
// containing "*" means "all tools" for that tier.
func intersectTools(a, b []string) []string {
	if isAllTools(a) {
		return b
	}
	if isAllTools(b) {
		return a
	}
	set := map[string]bool{}
	for _, t := range b {
		set[t] = true
	}
	var out []string
	for _, t := range a {
		if set[t] {
			out = append(out, t)
		}
	}
	return out
}

func isAllTools(ts []string) bool {
	if len(ts) == 0 {
		return true
	}
	for _, t := range ts {
		if t == "*" {
			return true
		}
	}
	return false
}

func checkMCP(requested []MCPRequest, allowed []v1.AllowedMCPServer) []Violation {
	if allowed == nil {
		return nil
	}
	byName := map[string]v1.AllowedMCPServer{}
	for _, a := range allowed {
		byName[a.Name] = a
	}
	var vs []Violation
	for _, req := range requested {
		a, ok := byName[req.Server]
		if !ok {
			vs = append(vs, Violation{Reason: ReasonMCPServerNotAllowed, Message: "MCP server " + req.Server + " is not in the effective allowed-MCP-servers set", Fatal: true})
			continue
		}
		if isAllTools(a.Tools) {
			continue
		}
		// Server is restricted to specific tools. A request that enumerates no
		// tools means "all tools", which a restricted subset can't satisfy.
		if len(req.Tools) == 0 {
			vs = append(vs, Violation{Reason: ReasonMCPToolNotAllowed, Message: "MCP server " + req.Server + " allows only specific tools; the class must enumerate them", Fatal: true})
			continue
		}
		for _, tool := range req.Tools {
			if !allowedContains(a.Tools, tool) {
				vs = append(vs, Violation{Reason: ReasonMCPToolNotAllowed, Message: "MCP tool " + req.Server + "/" + tool + " is not permitted", Fatal: true})
			}
		}
	}
	return vs
}

func limitSkills(s *v1.SettingsSpec) *[]string {
	if s == nil || s.Limits == nil {
		return nil
	}
	return s.Limits.AllowedSkills
}

func deniedSkillsOf(s *v1.SettingsSpec) []string {
	if s == nil || s.Limits == nil {
		return nil
	}
	return s.Limits.DeniedSkills
}

// resolveSkillSets returns the union of allow patterns (nil if no tier
// constrained; empty non-nil if a tier denies all) and the union of deny
// patterns, for status visibility.
func resolveSkillSets(in Inputs) (allowed, denied []string, constrained bool) {
	tiers := []*[]string{limitSkills(in.Cluster), limitSkills(in.Namespace)}
	seen := map[string]bool{}
	for _, t := range tiers {
		if t == nil {
			continue
		}
		constrained = true
		for _, p := range *t {
			if !seen[p] {
				seen[p] = true
				allowed = append(allowed, p)
			}
		}
	}
	if constrained && allowed == nil {
		allowed = []string{}
	}
	denied = unionStrings(deniedSkillsOf(in.Cluster), deniedSkillsOf(in.Namespace))
	return allowed, denied, constrained
}

func unionStrings(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, s := range l {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

func anySkillMatch(patterns []string, name string) bool {
	for _, p := range patterns {
		if canonical.MatchPattern(p, name) {
			return true
		}
	}
	return false
}

// checkSkills validates the class's opted-in skills against the allow/deny
// ceilings: each must match >=1 pattern in EVERY constraining allow tier, and
// must NOT match any deny pattern. Pin-strength floors live in checkPinning.
func checkSkills(in Inputs) []Violation {
	allowTiers := []*[]string{limitSkills(in.Cluster), limitSkills(in.Namespace)}
	denied := unionStrings(deniedSkillsOf(in.Cluster), deniedSkillsOf(in.Namespace))
	var vs []Violation
	for _, skill := range in.ClassSkills {
		for _, t := range allowTiers {
			if t == nil {
				continue
			}
			if !anySkillMatch(*t, skill) {
				vs = append(vs, Violation{Reason: ReasonSkillNotAllowed, Fatal: true,
					Message: "skill " + skill + " is not in the effective allowed-skills set"})
				break
			}
		}
		if anySkillMatch(denied, skill) {
			vs = append(vs, Violation{Reason: ReasonSkillDenied, Fatal: true,
				Message: "skill " + skill + " matches a denied-skills pattern"})
		}
	}
	return vs
}

const (
	defaultApprovalTimeout = 10 * time.Minute
	defaultInfoLeakTTL     = 10 * time.Minute
	defaultScopeLatencyMs  = int32(5000)
)

// resolveAuthz folds the authz windows the runner gates on, by the same two
// rules as budget:
//
//   - Defaults inherit downward — class, then namespace, then cluster, then the
//     built-in constants above.
//   - Limits.Authz only tightens — the shortest ceiling any tier set caps the
//     inherited value. A window the CLASS asked to be longer is clamped, its
//     provenance becomes "clamped", and one non-fatal AuthzClamped violation
//     names every capped window. Clamping a merely-inherited value is silent.
//
// Deliberately NOT built on resolveDim: that helper lets a ceiling BECOME the
// value when nothing was requested, which is right for budgets (no built-in
// default) and wrong here — a 30m ceiling would WIDEN the built-in 10m window.
// These knobs always have a value, so a ceiling may only ever reduce it.
func resolveAuthz(in Inputs, prov map[string]string) (EffectiveAuthz, []Violation) {
	// ApprovalTimeout: class wins, then tier defaults, then hard default.
	approval := authzWindow{value: defaultApprovalTimeout}
	if in.ClassAuthz != nil && in.ClassAuthz.ApprovalTimeout != nil {
		approval = authzWindow{value: in.ClassAuthz.ApprovalTimeout.Duration, tier: "class", explicit: true}
	} else if d := defAuthz(in.Namespace); d != nil && d.ApprovalTimeout != nil {
		approval = authzWindow{value: d.ApprovalTimeout.Duration, tier: "namespace"}
	} else if d := defAuthz(in.Cluster); d != nil && d.ApprovalTimeout != nil {
		approval = authzWindow{value: d.ApprovalTimeout.Duration, tier: "cluster"}
	}
	approval = approval.cappedBy(authzCeilings(in, func(c *v1.SettingsAuthzCeiling) time.Duration {
		return c.MaxApprovalTimeout.Duration
	})...)

	// InformationLeakageApprovalTTL: class authz block, then tier defaults.
	leakTTL := authzWindow{value: defaultInfoLeakTTL}
	if in.ClassAuthz != nil && in.ClassAuthz.InformationLeakage != nil && in.ClassAuthz.InformationLeakage.ApprovalTTL != nil {
		leakTTL = authzWindow{value: in.ClassAuthz.InformationLeakage.ApprovalTTL.Duration, tier: "class", explicit: true}
	} else if d := defAuthz(in.Namespace); d != nil && d.InformationLeakageApprovalTTL != nil {
		leakTTL = authzWindow{value: d.InformationLeakageApprovalTTL.Duration, tier: "namespace"}
	} else if d := defAuthz(in.Cluster); d != nil && d.InformationLeakageApprovalTTL != nil {
		leakTTL = authzWindow{value: d.InformationLeakageApprovalTTL.Duration, tier: "cluster"}
	}
	leakTTL = leakTTL.cappedBy(authzCeilings(in, func(c *v1.SettingsAuthzCeiling) time.Duration {
		return c.MaxInformationLeakageApprovalTTL.Duration
	})...)

	// ScopeMaxLLMLatencyMs: class scope, then tier defaults. Default-only — see
	// SettingsAuthzCeiling for why it carries no ceiling.
	out := EffectiveAuthz{
		ApprovalTimeout:               approval.value,
		InformationLeakageApprovalTTL: leakTTL.value,
		ScopeMaxLLMLatencyMs:          defaultScopeLatencyMs,
	}
	scopeTier := ""
	if in.ClassAuthz != nil && in.ClassAuthz.Scope != nil && in.ClassAuthz.Scope.MaxLLMLatencyMs > 0 {
		out.ScopeMaxLLMLatencyMs, scopeTier = in.ClassAuthz.Scope.MaxLLMLatencyMs, "class"
	} else if d := defAuthz(in.Namespace); d != nil && d.ScopeMaxLLMLatencyMs != nil {
		out.ScopeMaxLLMLatencyMs, scopeTier = *d.ScopeMaxLLMLatencyMs, "namespace"
	} else if d := defAuthz(in.Cluster); d != nil && d.ScopeMaxLLMLatencyMs != nil {
		out.ScopeMaxLLMLatencyMs, scopeTier = *d.ScopeMaxLLMLatencyMs, "cluster"
	}

	out.PlanGate = resolvePlanGate(in)
	out.Metaagent = resolveMetaagent(in)

	windows := []struct {
		name string
		w    authzWindow
	}{{"approvalTimeout", approval}, {"informationLeakageApprovalTTL", leakTTL}}
	var clamped []string
	for _, ww := range windows {
		if ww.w.tier != "" {
			prov["authz."+ww.name] = ww.w.tier
		}
		if ww.w.clamped && ww.w.explicit {
			clamped = append(clamped, ww.name)
		}
	}
	if scopeTier != "" {
		prov["authz.scopeMaxLlmLatencyMs"] = scopeTier
	}

	var vs []Violation
	if len(clamped) > 0 {
		vs = append(vs, Violation{
			Reason:  ReasonAuthzClamped,
			Message: "authz windows capped by a ceiling: " + strings.Join(clamped, ", "),
			Fatal:   false,
		})
	}
	return out, vs
}

// authzWindow is one resolved authz duration plus where it came from and
// whether a ceiling reduced it. explicit distinguishes a value the AgentClass
// asked for (worth warning about when clamped) from one it merely inherited.
type authzWindow struct {
	value    time.Duration
	tier     string
	explicit bool
	clamped  bool
}

// cappedBy reduces the window to the shortest ceiling any tier set. A zero
// ceiling contributes nothing. The tier that supplied the value keeps its
// provenance unless the ceiling actually reduced it, in which case provenance
// becomes "clamped" — same vocabulary resolveDim uses.
func (w authzWindow) cappedBy(ceilings ...time.Duration) authzWindow {
	limit := time.Duration(0)
	for _, c := range ceilings {
		if c > 0 && (limit == 0 || c < limit) {
			limit = c
		}
	}
	if limit > 0 && w.value > limit {
		w.value = limit
		w.clamped = true
		w.tier = "clamped"
	}
	return w
}

// authzCeilings reads one window's ceiling from every tier that can impose one.
// A tier that set none contributes a zero, which cappedBy ignores.
func authzCeilings(in Inputs, get func(*v1.SettingsAuthzCeiling) time.Duration) []time.Duration {
	out := make([]time.Duration, 0, 2)
	for _, s := range []*v1.SettingsSpec{in.Cluster, in.Namespace} {
		if s == nil || s.Limits == nil || s.Limits.Authz == nil {
			out = append(out, 0)
			continue
		}
		out = append(out, get(s.Limits.Authz))
	}
	return out
}

// planGateRank orders the modes by strictness. An UNRECOGNIZED value ranks at
// the strict end deliberately: an unparseable mode is a config error, and
// reading it as "disabled" would turn a typo into a silent security downgrade.
// An operator whose gate is too tight notices; one whose gate is silently off
// does not.
func planGateRank(mode string) int {
	switch mode {
	case "", "disabled":
		return 0
	case "logging":
		return 1
	case "enforcing":
		return 2
	default:
		return 3
	}
}

// strictestMode returns whichever of a and b is stricter. Empty means "unset"
// and loses to any real value; two unset values resolve to disabled.
func strictestMode(a, b string) string {
	if planGateRank(a) >= planGateRank(b) {
		if a == "" {
			return "disabled"
		}
		return a
	}
	if b == "" {
		return "disabled"
	}
	return b
}

// resolvePlanGate folds the plan gate in two steps, unlike the class-wins folds
// above it. Step 1 picks the requested mode: the class if it declared one, else
// the nearest tier default. Step 2 clamps that UP to the strictest floor any
// tier declares.
//
// The two steps read different fields on purpose. A tier DEFAULT says what a
// class gets when it declares nothing, and a class may still declare something
// laxer — that is what makes the gate adoptable. A tier FLOOR
// (SettingsLimits.MinPlanGateMode) is the separate, deliberate act that forbids
// it. Collapsing them into one field would silently turn every operator's
// default into a mandate.
func resolvePlanGate(in Inputs) *v1.PlanGateConfig {
	out := &v1.PlanGateConfig{Mode: "disabled"}

	// Step 1 — requested mode: class, else namespace default, else cluster
	// default. Carry the rest of the class's config through unchanged; only
	// Mode participates in the clamp.
	if in.ClassAuthz != nil && in.ClassAuthz.PlanGate != nil {
		cp := *in.ClassAuthz.PlanGate
		out = &cp
		if out.Mode == "" {
			out.Mode = "disabled"
		}
	} else if d := defAuthz(in.Namespace); d != nil && d.PlanGate != nil {
		cp := *d.PlanGate
		out = &cp
	} else if d := defAuthz(in.Cluster); d != nil && d.PlanGate != nil {
		cp := *d.PlanGate
		out = &cp
	}
	if out.Mode == "" {
		out.Mode = "disabled"
	}

	// Step 2 — clamp up to the strictest floor. Tiers ratchet UP only: a
	// namespace declaring a laxer floor than the cluster's does not loosen it,
	// which is why both are folded through strictestMode rather than the
	// namespace simply overriding.
	floor := ""
	if in.Cluster != nil && in.Cluster.Limits != nil {
		floor = in.Cluster.Limits.MinPlanGateMode
	}
	if in.Namespace != nil && in.Namespace.Limits != nil {
		floor = strictestMode(floor, in.Namespace.Limits.MinPlanGateMode)
	}
	out.Mode = strictestMode(out.Mode, floor)

	return out
}

func defAuthz(s *v1.SettingsSpec) *v1.DefaultAuthz {
	if s == nil || s.Defaults == nil {
		return nil
	}
	return s.Defaults.Authz
}

// effectiveToolGuard carries the per-tier Defaults policies through and folds
// the Limits ceilings strictest-per-field: thresholds/rate caps min, cool-off
// floor max, action-severity floor max. nil when no tier configured anything.
// Violations report bounds that were authored but cannot enforce — on both the
// ceilings and the carried-through policies.
func effectiveToolGuard(in Inputs) (*v1.EffectiveToolGuard, []Violation) {
	var out v1.EffectiveToolGuard
	var vs []Violation
	if d := defToolGuard(in.Cluster); d != nil {
		p, pvs := admissibleToolGuardPolicy(d, "cluster")
		out.Cluster = p
		vs = append(vs, pvs...)
	}
	if d := defToolGuard(in.Namespace); d != nil {
		p, pvs := admissibleToolGuardPolicy(d, "namespace")
		out.Namespace = p
		vs = append(vs, pvs...)
	}
	ceiling, cvs := foldToolGuardCeiling(limitToolGuard(in.Cluster), limitToolGuard(in.Namespace))
	vs = append(vs, cvs...)
	out.Ceiling = ceiling
	if out.Cluster == nil && out.Namespace == nil && out.Ceiling == nil {
		return nil, vs
	}
	return &out, vs
}

// admissibleToolGuardPolicy strips every rate pair that cannot enforce from a
// tier's policy on its way into status.effectiveSettings, reporting each one.
//
// The Defaults policies are the one part of EffectiveToolGuard that is MIRRORED
// rather than computed — rules cannot be pre-folded (first match in the highest
// tier wins per tool). That mirror puts RateLimitSpec on a status path, so every
// validation rule on the AUTHORED surface also governs the status write, for
// objects stored before the rule existed and never re-validated. A half-authored
// pair carried through therefore makes the whole status write inadmissible, and
// a rejected status write wedges the reconcile with an error naming a field on a
// different object.
//
// So the mirror is narrowed to what the CRD accepts, by the same predicate the
// ceiling side uses (classifyRatePair). Enforcement is unaffected — such a pair
// already capped nothing (v1.CallRate, ResolvedRule.HasRateLimit) — and the drop
// is reported, so status records the field as inert rather than showing a limit
// that never applied.
//
// p is returned unchanged when every pair is well-formed; a drop deep-copies,
// because Resolve is pure and the pointer aliases the caller's SettingsSpec.
func admissibleToolGuardPolicy(p *v1.ToolGuardPolicy, tier string) (*v1.ToolGuardPolicy, []Violation) {
	if p == nil {
		return nil, nil
	}
	out := p
	var vs []Violation
	for i := range p.Rules {
		rl := p.Rules[i].RateLimit
		if rl == nil {
			continue
		}
		var maxCalls *int32
		if rl.MaxCalls != 0 {
			v := rl.MaxCalls
			maxCalls = &v
		}
		state, why := classifyRatePair(maxCalls, rl.Window)
		if state != ratePairBroken {
			continue
		}
		if out == p {
			out = p.DeepCopy()
		}
		out.Rules[i].RateLimit.MaxCalls = 0
		out.Rules[i].RateLimit.Window = nil
		vs = append(vs, Violation{
			Reason: ReasonToolGuardRateBoundUnenforceable,
			Message: tier + " toolGuard rule " + strconv.Itoa(i) + " rateLimit " + why +
				"; it caps nothing and was not carried into effective settings",
		})
	}
	return out, vs
}

func defToolGuard(s *v1.SettingsSpec) *v1.ToolGuardPolicy {
	if s == nil || s.Defaults == nil {
		return nil
	}
	return s.Defaults.ToolGuard
}

func limitToolGuard(s *v1.SettingsSpec) *v1.ToolGuardCeiling {
	if s == nil || s.Limits == nil {
		return nil
	}
	return s.Limits.ToolGuard
}

// actionSeverity orders the ceiling MinAction floor: warn < deny < halt.
func actionSeverity(s string) int {
	switch s {
	case "warn":
		return 1
	case "deny":
		return 2
	case "halt":
		return 3
	default:
		return 0
	}
}

// ratePairState is what classifyRatePair concluded about one sliding-window
// bound.
type ratePairState int

const (
	// ratePairAbsent: neither half authored. Nothing to enforce and nothing to
	// report — declining to set a bound is not a mistake.
	ratePairAbsent ratePairState = iota
	// ratePairEnforceable: both halves present and positive.
	ratePairEnforceable
	// ratePairBroken: authored, but caps nothing.
	ratePairBroken
)

// classifyRatePair decides whether one maxCalls/window pair can enforce, and
// says why not when it cannot. It is the single predicate behind both places a
// half-authored pair must be caught: the ceiling fold (tierRateBounds) and the
// tier-policy mirror (admissibleToolGuardPolicy).
//
// A bound is a pair (see v1.CallRate): half of one, or a non-positive count or
// window, caps nothing. The returned reason is phrased to follow the name of
// the offending field ("maxCalls/window is missing window").
func classifyRatePair(maxCalls *int32, window *metav1.Duration) (ratePairState, string) {
	switch {
	case maxCalls == nil && window == nil:
		return ratePairAbsent, ""
	case window == nil:
		return ratePairBroken, "is missing window"
	case maxCalls == nil:
		return ratePairBroken, "is missing maxCalls"
	case *maxCalls <= 0 || window.Duration <= 0:
		return ratePairBroken, "has a non-positive maxCalls or window"
	default:
		return ratePairEnforceable, ""
	}
}

// tierRateBounds reads every sliding-window bound one tier's ceiling expresses
// — the leading MaxCalls/Window pair plus RateBounds — splitting the ones that
// can enforce from the ones that only look like they do. The ones that cannot
// are named for the caller to report rather than dropped in silence.
func tierRateBounds(c *v1.ToolGuardCeiling) (enforceable []v1.CallRateBound, unenforceable []string) {
	if c == nil {
		return nil, nil
	}
	keep := func(where string, maxCalls *int32, window *metav1.Duration) {
		switch state, why := classifyRatePair(maxCalls, window); state {
		case ratePairEnforceable:
			enforceable = append(enforceable, v1.CallRateBound{MaxCalls: *maxCalls, Window: *window})
		case ratePairBroken:
			unenforceable = append(unenforceable, where+" "+why)
		}
	}
	keep("maxCalls/window", c.MaxCalls, c.Window)
	for i := range c.RateBounds {
		b := c.RateBounds[i]
		keep("rateBounds["+strconv.Itoa(i)+"]", &b.MaxCalls, &b.Window)
	}
	return enforceable, unenforceable
}

// foldRateBounds merges every tier's sliding-window bounds into the folded
// ceiling as a CONJUNCTION: one bound per distinct window, keeping the
// strictest count, all of them enforced.
//
// Choosing between windows is what must not happen. Neither pair implies the
// other at every horizon — {5 calls, 1s} permits 432,000/day, {10 calls, 24h}
// permits all 10 inside one second — so keeping only one always discards a bound
// an admin wrote, on the surface the CRD calls the hard bound lower tiers cannot
// escape. pkg/authz/toolguard reaches the same conclusion one layer down,
// carrying the ceiling's pair alongside a rule's (ResolvedRule.RateCeilings).
//
// The strictest-by-rate pair leads in MaxCalls/Window so a consumer that reads
// only the pair still sees a real bound; the rest land in RateBounds ordered by
// window. The folded ceiling is re-stamped onto status, so a stable order is
// what keeps that write idempotent.
func foldRateBounds(out *v1.ToolGuardCeiling, bounds []v1.CallRateBound) {
	if len(bounds) == 0 {
		return
	}
	strictestPerWindow := map[time.Duration]int32{}
	for _, b := range bounds {
		if cur, seen := strictestPerWindow[b.Window.Duration]; !seen || b.MaxCalls < cur {
			strictestPerWindow[b.Window.Duration] = b.MaxCalls
		}
	}
	merged := make([]v1.CallRateBound, 0, len(strictestPerWindow))
	for window, maxCalls := range strictestPerWindow {
		merged = append(merged, v1.CallRateBound{MaxCalls: maxCalls, Window: metav1.Duration{Duration: window}})
	}
	// Sort by rate, then by window, so the leading pair is the strictest on
	// average and the remainder is deterministic whatever order the map ranged.
	sort.Slice(merged, func(i, j int) bool {
		ri := v1.CallRate(merged[i].MaxCalls, merged[i].Window.Duration)
		rj := v1.CallRate(merged[j].MaxCalls, merged[j].Window.Duration)
		if ri != rj {
			return ri < rj
		}
		return merged[i].Window.Duration < merged[j].Window.Duration
	})
	lead := merged[0]
	maxCalls, window := lead.MaxCalls, lead.Window
	out.MaxCalls, out.Window = &maxCalls, &window
	rest := merged[1:]
	sort.Slice(rest, func(i, j int) bool { return rest[i].Window.Duration < rest[j].Window.Duration })
	if len(rest) > 0 {
		out.RateBounds = rest
	}
}

func foldToolGuardCeiling(tiers ...*v1.ToolGuardCeiling) (*v1.ToolGuardCeiling, []Violation) {
	var out *v1.ToolGuardCeiling
	var vs []Violation
	var rateBounds []v1.CallRateBound
	ensure := func() *v1.ToolGuardCeiling {
		if out == nil {
			out = &v1.ToolGuardCeiling{}
		}
		return out
	}
	for _, c := range tiers {
		if c == nil {
			continue
		}
		enforceable, unenforceable := tierRateBounds(c)
		rateBounds = append(rateBounds, enforceable...)
		for _, u := range unenforceable {
			vs = append(vs, Violation{
				Reason:  ReasonToolGuardRateBoundUnenforceable,
				Message: "toolGuard ceiling bound " + u + "; it caps nothing and was not folded in",
			})
		}
		if c.MaxFailureThreshold != nil {
			o := ensure()
			if o.MaxFailureThreshold == nil || *c.MaxFailureThreshold < *o.MaxFailureThreshold {
				v := *c.MaxFailureThreshold
				o.MaxFailureThreshold = &v
			}
		}
		if c.MinInitialCoolOff != nil {
			o := ensure()
			if o.MinInitialCoolOff == nil || c.MinInitialCoolOff.Duration > o.MinInitialCoolOff.Duration {
				v := *c.MinInitialCoolOff
				o.MinInitialCoolOff = &v
			}
		}
		if c.MinAction != nil {
			o := ensure()
			if o.MinAction == nil || actionSeverity(*c.MinAction) > actionSeverity(*o.MinAction) {
				v := *c.MinAction
				o.MinAction = &v
			}
		}
		if c.MaxCallsPerTurn != nil {
			o := ensure()
			if o.MaxCallsPerTurn == nil || *c.MaxCallsPerTurn < *o.MaxCallsPerTurn {
				v := *c.MaxCallsPerTurn
				o.MaxCallsPerTurn = &v
			}
		}
		if c.MaxEgressBytes != nil {
			o := ensure()
			if o.MaxEgressBytes == nil || *c.MaxEgressBytes < *o.MaxEgressBytes {
				v := *c.MaxEgressBytes
				o.MaxEgressBytes = &v
			}
		}
		if c.MaxIngressBytes != nil {
			o := ensure()
			if o.MaxIngressBytes == nil || *c.MaxIngressBytes < *o.MaxIngressBytes {
				v := *c.MaxIngressBytes
				o.MaxIngressBytes = &v
			}
		}
		if c.MaxUIIngressBytes != nil {
			o := ensure()
			if o.MaxUIIngressBytes == nil || *c.MaxUIIngressBytes < *o.MaxUIIngressBytes {
				v := *c.MaxUIIngressBytes
				o.MaxUIIngressBytes = &v
			}
		}
	}
	if len(rateBounds) > 0 {
		foldRateBounds(ensure(), rateBounds)
	}
	return out, vs
}

func reportCostDefault(s *v1.SettingsSpec) *bool {
	if s == nil || s.Defaults == nil {
		return nil
	}
	return s.Defaults.ReportSessionCost
}

// resolveReportSessionCost cascades namespace default over cluster default; nil
// at both ⇒ true (on by default).
func resolveReportSessionCost(in Inputs, prov map[string]string) bool {
	if v := reportCostDefault(in.Namespace); v != nil {
		prov["reportSessionCost"] = "namespace"
		return *v
	}
	if v := reportCostDefault(in.Cluster); v != nil {
		prov["reportSessionCost"] = "cluster"
		return *v
	}
	return true
}

// resolveMetaagent folds the metaagent trigger: the class if it declared one,
// else the nearest tier default, else mention.
//
// Class-wins with no floor, unlike resolvePlanGate's two-step clamp. There is
// deliberately no SettingsLimits.MinMetaagentTrigger yet: a floor forbidding a
// class from going laxer only earns its complexity once someone needs it, and
// the direction that would matter — an operator forcing shadow or mention
// cluster-wide — is expressible today by simply not letting classes set it.
//
// The default is mention, which is the narrowest of the three. An absent
// setting anywhere in the chain means the ambient path stays off.
func resolveMetaagent(in Inputs) *v1.MetaagentConfig {
	pick := func(m *v1.MetaagentConfig) *v1.MetaagentConfig {
		cp := *m
		// Normalize through the accessor so a value that predates the enum, or
		// arrives from an older tier, resolves fail-safe to mention rather than
		// travelling on as an unrecognized string nothing downstream matches.
		cp.Trigger = m.GetMetaagentTrigger()
		return &cp
	}

	if in.ClassAuthz != nil && in.ClassAuthz.Metaagent != nil {
		return pick(in.ClassAuthz.Metaagent)
	}
	if d := defAuthz(in.Namespace); d != nil && d.Metaagent != nil {
		return pick(d.Metaagent)
	}
	if d := defAuthz(in.Cluster); d != nil && d.Metaagent != nil {
		return pick(d.Metaagent)
	}
	return &v1.MetaagentConfig{Trigger: v1.MetaagentTriggerMention}
}
