package settings

import (
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// mergeRouting merges an AgentClass's RoutingMetadata (overlay) over the
// catalog-resolved OpenRouterRouting (base) with narrowing-only semantics: an
// AgentClass may constrain admin policy, never loosen or escape it. Never
// mutates or aliases either input — the result is a fresh struct with fresh
// slices/pointers, safe to store independently of both.
//
// Per-field rules:
//   - Models/Only/AllowedModels: intersect, preserving overlay order; a disjoint
//     pair falls back to base rather than emitting empty — for Only, an empty
//     wire value means "all providers allowed", which would fail OPEN.
//   - Ignore: union (base order first, then overlay's new items).
//   - MaxPrice: tighten-only min per axis, with 0 meaning "no cap on this axis"
//     rather than "tightest cap" (plain min(0, x) would invert that).
//   - Sort/Order/CostQualityTradeoff: overlay wins when set. Order ranks
//     providers rather than allowing/denying them, so it is overlay-wins, not
//     intersect — Only/Ignore are the allow/deny controls.
//   - DataCollection: deny-sticky — "deny" on either side wins outright, else
//     overlay when set, else base. An agent may tighten privacy but can never
//     loosen an admin "deny".
//   - AllowFallbacks: logical AND — true is the PERMISSIVE pole, so the agent
//     may turn it off but can never force it back on.
//   - RequireParameters: logical OR — true is the RESTRICTIVE pole (fewer
//     eligible providers), so the agent may add it but can never drop it.
func mergeRouting(base, overlay *v1.OpenRouterRouting) *v1.OpenRouterRouting {
	if overlay == nil {
		return base.DeepCopy()
	}
	if base == nil {
		return overlay.DeepCopy()
	}

	return &v1.OpenRouterRouting{
		Models:              intersectNarrowing(base.Models, overlay.Models),
		Sort:                overlayWinsString(base.Sort, overlay.Sort),
		Order:               overlayWinsSlice(base.Order, overlay.Order),
		Only:                intersectNarrowing(base.Only, overlay.Only),
		Ignore:              union(base.Ignore, overlay.Ignore),
		AllowFallbacks:      andBoolPtr(base.AllowFallbacks, overlay.AllowFallbacks),
		RequireParameters:   orBoolPtr(base.RequireParameters, overlay.RequireParameters),
		DataCollection:      mergeDataCollection(base.DataCollection, overlay.DataCollection),
		MaxPrice:            mergeMaxPrice(base.MaxPrice, overlay.MaxPrice),
		AllowedModels:       intersectNarrowing(base.AllowedModels, overlay.AllowedModels),
		CostQualityTradeoff: overlayWinsFloatPtr(base.CostQualityTradeoff, overlay.CostQualityTradeoff),
	}
}

// intersectNarrowing intersects base and overlay, preserving overlay's order
// (the agent's preference, constrained to admin's set). An empty base means
// unconstrained ⇒ overlay stands; an empty overlay means the agent did not
// refine this field ⇒ base stands. Non-empty but disjoint falls back to base:
// emitting empty would, for an allow-list field like Only, mean "no
// restriction" and fail OPEN.
func intersectNarrowing(base, overlay []string) []string {
	if len(base) == 0 {
		return copyStrings(overlay)
	}
	if len(overlay) == 0 {
		return copyStrings(base)
	}
	inBase := make(map[string]struct{}, len(base))
	for _, v := range base {
		inBase[v] = struct{}{}
	}
	var out []string
	for _, v := range overlay {
		if _, ok := inBase[v]; ok {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return copyStrings(base)
	}
	return out
}

// union returns base ∪ overlay, base's items first (in base order) followed
// by overlay's items not already present.
func union(base, overlay []string) []string {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(base)+len(overlay))
	out := make([]string, 0, len(base)+len(overlay))
	for _, v := range base {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	for _, v := range overlay {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

// overlayWinsSlice replaces base's slice outright with overlay's when overlay
// is non-empty; used for fields that rank/order rather than allow/deny
// (Order), where narrowing-by-intersection would be the wrong semantics.
func overlayWinsSlice(base, overlay []string) []string {
	if len(overlay) > 0 {
		return copyStrings(overlay)
	}
	return copyStrings(base)
}

// copyStrings returns a fresh copy of in, or nil if in is empty — never
// aliases the caller's backing array.
func copyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// overlayWinsString returns overlay when non-empty, else base.
func overlayWinsString(base, overlay string) string {
	if overlay != "" {
		return overlay
	}
	return base
}

// overlayWinsFloatPtr returns a fresh pointer to overlay's value when set,
// else a fresh pointer to base's value, else nil.
func overlayWinsFloatPtr(base, overlay *float64) *float64 {
	if overlay != nil {
		v := *overlay
		return &v
	}
	if base != nil {
		v := *base
		return &v
	}
	return nil
}

// andBoolPtr logically ANDs base and overlay; a nil side defers to the other
// (copied, fresh pointer). AllowFallbacks' rule: true is the PERMISSIVE pole
// (fallbacks allowed), so the agent may turn it off but can never force a
// false admin setting back on.
func andBoolPtr(base, overlay *bool) *bool {
	if base == nil {
		return copyBoolPtr(overlay)
	}
	if overlay == nil {
		return copyBoolPtr(base)
	}
	v := *base && *overlay
	return &v
}

// orBoolPtr logically ORs base and overlay; a nil side defers to the other
// (copied, fresh pointer). RequireParameters' rule: true is the RESTRICTIVE
// pole (it narrows the eligible set to tool-calling providers), so the agent
// may add the requirement but can never drop one admin policy set.
func orBoolPtr(base, overlay *bool) *bool {
	if base == nil {
		return copyBoolPtr(overlay)
	}
	if overlay == nil {
		return copyBoolPtr(base)
	}
	v := *base || *overlay
	return &v
}

// copyBoolPtr returns a fresh pointer with the same value, or nil.
func copyBoolPtr(in *bool) *bool {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}

// mergeDataCollection is deny-sticky: "deny" on either side wins outright, so
// an agent may tighten privacy but can never loosen an admin "deny". Otherwise
// overlay wins when set, else base.
func mergeDataCollection(base, overlay string) string {
	if base == "deny" || overlay == "deny" {
		return "deny"
	}
	return overlayWinsString(base, overlay)
}

// mergeMaxPrice tightens per-axis price caps to the min, treating 0 on either
// side as "no cap on this axis" — a plain min(0, x) would turn an unset axis
// into a zero-price ceiling.
func mergeMaxPrice(base, overlay *v1.OpenRouterMaxPrice) *v1.OpenRouterMaxPrice {
	if base == nil {
		return overlay.DeepCopy()
	}
	if overlay == nil {
		return base.DeepCopy()
	}
	return &v1.OpenRouterMaxPrice{
		Prompt:     tightenPrice(base.Prompt, overlay.Prompt),
		Completion: tightenPrice(base.Completion, overlay.Completion),
	}
}

// tightenPrice returns the tighter (lower) of two per-axis price caps, where 0
// means unset on that axis — the other side wins rather than 0 winning as a
// false "tightest" cap.
func tightenPrice(base, overlay float64) float64 {
	switch {
	case base == 0:
		return overlay
	case overlay == 0:
		return base
	case base < overlay:
		return base
	default:
		return overlay
	}
}
