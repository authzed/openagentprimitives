package toolguard

import (
	"fmt"
	"path"
	"time"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Builtin is the hardcoded bottom-tier rule: breaker on, rate limits off.
// Default-on posture per the spec; override via any ToolGuardPolicy tier.
var Builtin = ResolvedRule{
	FailureThreshold:       5,
	OriginFailureThreshold: 10,
	InitialCoolOff:         30 * time.Second,
	MaxCoolOff:             10 * time.Minute,
	Action:                 ActionDeny,
	// Two consecutive unbilled failures end the session. Deliberately far below
	// the breaker's threshold, and answered by a halt rather than a deny,
	// because a refused credential is not a flaky upstream: no cool-off makes
	// it work, and the breaker's "try again in ~30s" reply just spends the rest
	// of the session's turn and duration budget on a call that cannot succeed.
	//
	// Two rather than one: a toolkit's own retry cap keeps a refused call to
	// about a second, so two is immediate in wall-clock terms (seconds, against
	// a maxDuration measured in hours) while still surviving a single-attempt
	// blip. That blip is real — an operator rotating a credential mid-flight
	// makes a healthy running session take exactly one resolution failure
	// inside the rotation window — and halting on the first would make routine
	// rotation more dangerous than the defect this closes.
	AuthHaltThreshold: 2,
	RateAction:        ActionDeny,
	ByteAction:        ActionDeny,
	Provenance:        "builtin",
}

// Tiers carries the per-tier policies + the pre-folded strictest ceiling
// (folding across cluster/namespace Limits happens in pkg/platform/settings).
type Tiers struct {
	Class     *v1.ToolGuardPolicy
	Namespace *v1.ToolGuardPolicy
	Cluster   *v1.ToolGuardPolicy
	Ceiling   *v1.ToolGuardCeiling
}

type tierRules struct {
	name  string // "class" | "namespace" | "cluster"
	rules []v1.ToolGuardRule
}

// ungatedFallback is the no-rule-matched rule for tools that bypass the
// containment pipeline (see ForUngatedTools). It carries no breaker at any
// severity, so only a ceiling's rate/byte bound can make it do anything —
// which is exactly the split ForUngatedTools exists to draw.
//
// It carries no credential halt either, for the same reason and one more of
// its own: these are in-process calls with no provider behind them, so there
// is no credential for one to have refused.
var ungatedFallback = ResolvedRule{
	RateAction: ActionDeny,
	ByteAction: ActionDeny,
	Provenance: "ceiling",
}

// ResolvedPolicy is the immutable session-start product the hooks consult.
type ResolvedPolicy struct {
	tiers   []tierRules
	ceiling *v1.ToolGuardCeiling
	// ceilingRates is every sliding-window bound the folded ceiling carries —
	// its leading MaxCalls/Window pair plus ToolGuardCeiling.RateBounds, which
	// is how pkg/platform/settings keeps both the cluster's and the namespace's window
	// when they differ. Flattened once here so the per-call clamp neither
	// re-reads the CRD pointers nor re-decides what is enforceable.
	ceilingRates []WindowLimit
	// breakerless swaps RuleFor's no-rule-matched fallback from Builtin to
	// ungatedFallback. Set only on the sibling view ForUngatedTools returns.
	breakerless bool
	// ungated is that sibling view, built once at resolve time so consulting it
	// per tool call allocates nothing. nil on the view itself.
	ungated *ResolvedPolicy
}

// ceilingWindowLimits flattens a folded ceiling's sliding-window bounds,
// dropping any that cannot enforce (a half-authored pair, a non-positive count
// or window). Dropping is safe to do silently HERE because pkg/platform/settings' fold
// already reported each one as a ToolGuardRateBoundUnenforceable violation; a
// ceiling reaching this far with such a bound came from an object written
// before the resolver reported it.
func ceilingWindowLimits(c *v1.ToolGuardCeiling) []WindowLimit {
	if c == nil {
		return nil
	}
	var out []WindowLimit
	if c.MaxCalls != nil && c.Window != nil {
		if w := (WindowLimit{MaxCalls: *c.MaxCalls, Window: c.Window.Duration}); w.Enforced() {
			out = append(out, w)
		}
	}
	for _, b := range c.RateBounds {
		if w := (WindowLimit{MaxCalls: b.MaxCalls, Window: b.Window.Duration}); w.Enforced() {
			out = append(out, w)
		}
	}
	return out
}

// ResolvePolicy validates globs and freezes the tier walk. An error means
// invalid config — callers fail the session start (fail closed).
func ResolvePolicy(t Tiers) (*ResolvedPolicy, error) {
	p := &ResolvedPolicy{ceiling: t.Ceiling, ceilingRates: ceilingWindowLimits(t.Ceiling)}
	for _, tier := range []struct {
		name string
		pol  *v1.ToolGuardPolicy
	}{{"class", t.Class}, {"namespace", t.Namespace}, {"cluster", t.Cluster}} {
		if tier.pol == nil {
			continue
		}
		for i, rule := range tier.pol.Rules {
			for _, pat := range []string{rule.Match.Tool, rule.Match.Origin} {
				if pat == "" {
					continue
				}
				if _, err := path.Match(pat, "probe"); err != nil {
					return nil, fmt.Errorf("toolguard: %s rule %d: invalid glob %q: %w", tier.name, i, pat, err)
				}
			}
		}
		p.tiers = append(p.tiers, tierRules{name: tier.name, rules: tier.pol.Rules})
	}
	p.ungated = &ResolvedPolicy{tiers: p.tiers, ceiling: p.ceiling, ceilingRates: p.ceilingRates, breakerless: true}
	return p, nil
}

// ForUngatedTools returns the view of this policy to use for tools that bypass
// the containment pipeline — today the Stateless/Passthrough meta tools the
// dispatcher runs outside executeToolContained (see
// pkg/agent/runner/toolguard_meta.go). Authored rules resolve identically; what
// differs is what "no rule matched" means.
//
// Both halves of that split are deliberate:
//
//   - A ToolGuardCeiling MUST reach these tools. It is a separate admin surface
//     from the rules, and its CRD promises the rate and byte bounds apply "even
//     when no lower-tier rule configures" one. An admin capping maxIngressBytes
//     to bound how much third-party content enters model context means it most
//     of all for read_channel_history / query_memory / search_memory, which
//     exist to fetch exactly that.
//   - Builtin MUST NOT. Its consecutive-failure breaker is calibrated for
//     external dependencies, where repeated failure means the upstream is down
//     and backing off helps. Meta tools are in-process calls and several are the
//     session's own control surface: denying agent_work_complete or
//     respond_to_user after five bad-argument errors wedges the session rather
//     than protecting anything.
//
// So the fallback is ungatedFallback clamped by the ceiling's rate/byte bounds
// only. Breaker clamps are skipped, including the MinAction severity floor —
// MinAction floors the action of a RULE, and here there is no rule, so applying
// it would re-arm Builtin's breaker on a tool that has none.
func (p *ResolvedPolicy) ForUngatedTools() *ResolvedPolicy {
	if p == nil || p.breakerless {
		return p
	}
	return p.ungated
}

// RuleFor walks the tiers most-specific-first; the first matching rule wins
// and fully replaces lower tiers, then the ceiling clamps it. kind is the
// runtime tool.Kind string; origin is "<kind>/<name>" or "" for origin-less
// tools.
func (p *ResolvedPolicy) RuleFor(kind, toolName, origin string) ResolvedRule {
	for _, tier := range p.tiers {
		for i, rule := range tier.rules {
			if !matches(rule.Match, kind, toolName, origin) {
				continue
			}
			return p.clamp(fromCRD(rule, fmt.Sprintf("%s[%d]", tier.name, i)))
		}
	}
	if p.breakerless {
		// No rule matched, and Builtin is out of reach for this view: the
		// ceiling's volume bounds are all that can apply. See ForUngatedTools.
		return p.clampVolume(ungatedFallback)
	}
	return p.clamp(Builtin)
}

func matches(m v1.ToolGuardMatch, kind, toolName, origin string) bool {
	if m.Kind != "" && m.Kind != kind {
		return false
	}
	if m.Tool != "" {
		if ok, _ := path.Match(m.Tool, toolName); !ok {
			return false
		}
	}
	if m.Origin != "" {
		if ok, _ := path.Match(m.Origin, origin); !ok {
			return false
		}
	}
	return true
}

// fromCRD converts a matched CRD rule: nil Breaker = no breaker; set Breaker
// inherits unset fields from Builtin. nil RateLimit = no rate limit.
func fromCRD(rule v1.ToolGuardRule, provenance string) ResolvedRule {
	out := ResolvedRule{Action: ActionOff, RateAction: ActionDeny, ByteAction: ActionDeny, Provenance: provenance}
	// The credential halt is inherited UNCONDITIONALLY, unlike every breaker
	// parameter below. No CRD field configures it, so there is nothing for an
	// author to have meant by omitting it — and the alternative silently
	// disables it: a rule written to add a rate limit to a toolkit tool would
	// take the credential halt away from the very tool most likely to need it.
	out.AuthHaltThreshold = Builtin.AuthHaltThreshold
	if b := rule.Breaker; b != nil {
		out.Action = ParseAction(b.Action, Builtin.Action)
		// Always carry explicit CRD thresholds and cool-offs; only backfill
		// from Builtin when the breaker is active (Action != off). A disabled
		// breaker must not accumulate state, so we never default unset fields
		// to Builtin values — leaving them zero means GuardRecord skips
		// recording entirely. clamp backfills Builtin params itself when a
		// MinAction ceiling floor re-enables the breaker.
		out.FailureThreshold = b.FailureThreshold
		if out.Action != ActionOff && out.FailureThreshold == 0 {
			out.FailureThreshold = Builtin.FailureThreshold
		}
		out.OriginFailureThreshold = b.OriginFailureThreshold
		if out.Action != ActionOff && out.OriginFailureThreshold == 0 {
			out.OriginFailureThreshold = Builtin.OriginFailureThreshold
		}
		if b.InitialCoolOff != nil {
			out.InitialCoolOff = b.InitialCoolOff.Duration
		} else if out.Action != ActionOff {
			out.InitialCoolOff = Builtin.InitialCoolOff
		}
		if b.MaxCoolOff != nil {
			out.MaxCoolOff = b.MaxCoolOff.Duration
		} else if out.Action != ActionOff {
			out.MaxCoolOff = Builtin.MaxCoolOff
		}
	}
	if rl := rule.RateLimit; rl != nil {
		out.RateMaxPerTurn = rl.MaxCallsPerTurn
		out.RateMaxCalls = rl.MaxCalls
		if rl.Window != nil {
			out.RateWindow = rl.Window.Duration
		}
		out.RateAction = ParseAction(rl.Action, ActionDeny)
	}
	if dl := rule.DataLimit; dl != nil {
		out.MaxEgressBytes = dl.MaxEgressBytes
		out.MaxIngressBytes = dl.MaxIngressBytes
		out.MaxUIIngressBytes = dl.MaxUIIngressBytes
		out.ByteAction = ParseAction(dl.Action, ActionDeny)
	}
	return out
}

// clamp applies the whole folded ceiling: thresholds min-clamped, cool-off
// floor raised, action severity floored, rate and byte ceilings imposed (even
// when the rule configured none — an unset limit is "unlimited", so the ceiling
// wins). Both halves are no-ops when no ceiling was configured.
func (p *ResolvedPolicy) clamp(r ResolvedRule) ResolvedRule {
	return p.clampVolume(p.clampBreaker(r))
}

// clampBreaker applies the breaker half of the ceiling: the action-severity
// floor and the threshold/cool-off bounds. Split from clampVolume because the
// ungated-tool view applies the volume half alone (see ForUngatedTools).
func (p *ResolvedPolicy) clampBreaker(r ResolvedRule) ResolvedRule {
	c := p.ceiling
	if c == nil {
		return r
	}
	if c.MinAction != nil {
		if floor := ParseAction(*c.MinAction, ActionOff); r.Action < floor {
			r.Action = floor
			// A floored-on breaker needs working parameters.
			if r.FailureThreshold == 0 {
				r.FailureThreshold = Builtin.FailureThreshold
			}
			if r.OriginFailureThreshold == 0 {
				r.OriginFailureThreshold = Builtin.OriginFailureThreshold
			}
			if r.InitialCoolOff == 0 {
				r.InitialCoolOff = Builtin.InitialCoolOff
			}
			if r.MaxCoolOff == 0 {
				r.MaxCoolOff = Builtin.MaxCoolOff
			}
		}
	}
	if c.MaxFailureThreshold != nil && r.FailureThreshold > 0 && r.FailureThreshold > *c.MaxFailureThreshold {
		r.FailureThreshold = *c.MaxFailureThreshold
	}
	if c.MinInitialCoolOff != nil && r.FailureThreshold > 0 && r.InitialCoolOff < c.MinInitialCoolOff.Duration {
		r.InitialCoolOff = c.MinInitialCoolOff.Duration
		if r.MaxCoolOff < r.InitialCoolOff {
			r.MaxCoolOff = r.InitialCoolOff
		}
	}
	return r
}

// clampVolume applies the call-volume half of the ceiling: rate and byte
// ceilings, imposed even when the rule configured none — an unset rate or byte
// budget is "unlimited", so the ceiling wins — and the ACTION floor that makes
// those bounds bind (see floorVolumeActions).
func (p *ResolvedPolicy) clampVolume(r ResolvedRule) ResolvedRule {
	c := p.ceiling
	if c == nil {
		return r
	}
	r = p.floorVolumeActions(r)
	if c.MaxCallsPerTurn != nil && (r.RateMaxPerTurn == 0 || r.RateMaxPerTurn > *c.MaxCallsPerTurn) {
		r.RateMaxPerTurn = *c.MaxCallsPerTurn
	}
	// maxCalls and window mean nothing apart, so a ceiling bound is never split
	// across the authored one. The ceiling may name SEVERAL windows — the fold
	// keeps every distinct one the cluster and namespace tiers wrote — and each
	// lands by its own shape:
	//
	//   - the rule enforces no sliding-window bound (either half missing, so
	//     v1.CallRate reads it as unlimited): the first ceiling bound becomes
	//     the rule's, exactly as an unset byte budget takes the ceiling's;
	//   - the same window as the rule's: the stricter COUNT wins, which is
	//     exact — one bound still expresses both;
	//   - a different window: BOTH bind, carried in RateCeilings and swept
	//     together by Admit.
	//
	// Never collapse two different windows to whichever wins a calls/second
	// comparison: that discards a bound its author wrote, whichever side you
	// keep. The looser scalar lets {100, 1m} (=144,000/day) overwrite an authored
	// {200, 24h}; the stricter one lets a {200, 24h} rule permit all 200 inside
	// one second, which the admin's {100, 1m} exists to prevent. The conjunction
	// is what both parties wrote, and what ToolGuardCeiling promises: a lower
	// tier may tighten, never relax.
	for _, ceil := range p.ceilingRates {
		authored := WindowLimit{MaxCalls: r.RateMaxCalls, Window: r.RateWindow}
		switch {
		case !authored.Enforced():
			r.RateMaxCalls, r.RateWindow = ceil.MaxCalls, ceil.Window
		case authored.Window == ceil.Window:
			if ceil.MaxCalls < authored.MaxCalls {
				r.RateMaxCalls = ceil.MaxCalls
			}
		default:
			r.RateCeilings = tightenBound(r.RateCeilings, ceil)
		}
	}
	if c.MaxEgressBytes != nil && (r.MaxEgressBytes == 0 || r.MaxEgressBytes > *c.MaxEgressBytes) {
		r.MaxEgressBytes = *c.MaxEgressBytes
	}
	if c.MaxIngressBytes != nil && (r.MaxIngressBytes == 0 || r.MaxIngressBytes > *c.MaxIngressBytes) {
		r.MaxIngressBytes = *c.MaxIngressBytes
	}
	if c.MaxUIIngressBytes != nil && (r.MaxUIIngressBytes == 0 || r.MaxUIIngressBytes > *c.MaxUIIngressBytes) {
		r.MaxUIIngressBytes = *c.MaxUIIngressBytes
	}
	return r
}

// tightenBound merges w into dst, keeping one entry per window at the strictest
// count — a ceiling that names the same window twice (hand-authored, since the
// fold dedupes) must not cost two passes in Admit's per-call sweep.
//
// dst is nil on entry from clampVolume: no rule source populates RateCeilings,
// so the append always allocates a slice this ResolvedRule alone owns and can
// never write through into Builtin's or ungatedFallback's shared value. That
// costs one small allocation per tool call whose rule is bounded by a ceiling
// naming a different window — the case that previously dropped a bound outright.
func tightenBound(dst []WindowLimit, w WindowLimit) []WindowLimit {
	for i, have := range dst {
		if have.Window == w.Window {
			if w.MaxCalls < have.MaxCalls {
				dst[i].MaxCalls = w.MaxCalls
			}
			return dst
		}
	}
	return append(dst, w)
}

// floorVolumeActions raises RateAction / ByteAction to at least ActionDeny in
// whichever dimension the ceiling bounds — the half of the clamp that makes the
// magnitudes above mean anything.
//
// Clamping only the magnitudes left the ceiling escapable from a
// namespace-writable surface. AgentClass.spec.toolGuard is authored per
// namespace; Limits.ToolGuard is the cluster admin's "hard bound lower tiers
// cannot escape". A class rule writing `action: warn` kept warn straight
// through the clamp, and warn returns pipeline.Decision{} in both hooks
// (hook_guard.Eval, events.byteLimitDecision) — the over-budget call dispatches
// and the oversized result reaches the model, which is exactly what an admin
// capping maxIngressBytes on read_channel_history / query_memory is buying.
//
// The floor keys off the ceiling CONFIGURING a dimension, not off it having
// tightened the number: a rule whose own limit is stricter but whose action is
// warn passes an arbitrarily large payload just the same, so a ceiling in that
// dimension binds nothing while a warn action stands.
//
// deny is the floor because it is the action a ceiling already carries wherever
// it stands alone — Builtin, ungatedFallback and fromCRD's no-action default
// all use deny. It is a floor, never an assignment: a rule that authored halt
// stays halt, since the ceiling exists to bound a rule, not to soften one.
//
// ToolGuardCeiling.MinAction is deliberately NOT read here. Its CRD doc scopes
// it to the breaker's action, and clampBreaker applies it there; promoting a
// byte overrun into a session halt is a larger change than this bound needs.
func (p *ResolvedPolicy) floorVolumeActions(r ResolvedRule) ResolvedRule {
	c := p.ceiling
	// ceilingRates rather than the raw MaxCalls/Window pointers: the ceiling can
	// carry its bounds in RateBounds alone, and a bound that cannot enforce is
	// already filtered out — flooring the action for one of those would tighten
	// a rule on the strength of a ceiling that bounds nothing.
	if c.MaxCallsPerTurn != nil || len(p.ceilingRates) > 0 {
		if r.RateAction < ActionDeny {
			r.RateAction = ActionDeny
		}
	}
	// One ByteAction covers both byte dimensions (DataLimitSpec has a single
	// Action), so a ceiling on either one floors it.
	if c.MaxEgressBytes != nil || c.MaxIngressBytes != nil {
		if r.ByteAction < ActionDeny {
			r.ByteAction = ActionDeny
		}
	}
	return r
}
