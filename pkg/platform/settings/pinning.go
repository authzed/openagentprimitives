package settings

import (
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
)

// effectiveMode maps an absent rule Mode to the default.
func effectiveMode(m string) string {
	if m == "" {
		return v1.PinModeApprove
	}
	return m
}

// modeLevel orders modes for strongest-wins folding: block > approve > warn > off.
func modeLevel(m string) int {
	switch m {
	case v1.PinModeBlock:
		return 3
	case v1.PinModeApprove:
		return 2
	case v1.PinModeWarn:
		return 1
	default:
		return 0
	}
}

// strengthFloorLevel orders MinStrength floors ("" = no floor = 0).
func strengthFloorLevel(min string) int {
	return pinning.Strength(min).Level()
}

// tierPinningPolicy returns a copy of the tier's PinningPolicy, or nil when the
// tier declares none. The copy is what callers hand to PinRequirementFor, which
// must never mutate a caller's settings spec.
func tierPinningPolicy(s *v1.SettingsSpec) *v1.PinningPolicy {
	if s == nil || s.Limits == nil || s.Limits.Pinning == nil {
		return nil
	}
	return s.Limits.Pinning.DeepCopy()
}

// ruleFor returns the tier's rule for a kind, if any.
func ruleFor(p *v1.PinningPolicy, kind string) (v1.PinningRule, bool) {
	if p == nil {
		return v1.PinningRule{}, false
	}
	for _, r := range p.Rules {
		if r.Kind == kind {
			return r, true
		}
	}
	return v1.PinningRule{}, false
}

// bypassFor returns the tier's bypass matching (kind, name), if any. Name
// matching uses the same exact-or-trailing-wildcard syntax as AllowedSkills.
func bypassFor(p *v1.PinningPolicy, kind, name string) *v1.PinningBypass {
	if p == nil {
		return nil
	}
	for i, b := range p.Bypass {
		if b.Kind == kind && canonical.MatchPattern(b.Name, name) {
			return &p.Bypass[i]
		}
	}
	return nil
}

// PinRequirement is the effective pinning requirement for one declared
// dependency after tier-scoped folding.
type PinRequirement struct {
	// MinStrength is the pin-strength floor the declaration must meet; "" is no
	// floor, and it is informational (recorded, not enforced) while Mode is "off".
	MinStrength string
	// Mode is "" when NO rule applies (none configured, or every applicable rule
	// was bypassed) and callers do nothing; "off" when a rule exists but only
	// observes; otherwise the enforcing mode (warn/approve/block).
	Mode string
	// BypassReason is non-empty only when a bypass suppressed an otherwise-
	// applicable rule.
	BypassReason string
}

// HasRule reports whether any rule applies (including observe-only).
func (r PinRequirement) HasRule() bool { return r.Mode != "" }

// Enforced reports whether the requirement carries any enforcement teeth
// (block/approve/warn) as opposed to no rule or observe-only.
func (r PinRequirement) Enforced() bool { return r.Mode != "" && r.Mode != v1.PinModeOff }

// declaredPins normalizes every dependency declaration into DeclaredPin form:
// skills parsed from ClassSkills (malformed canonical names are skipped here —
// they are caught by the skill resolution path), other kinds pre-parsed by
// the caller in ClassPins.
func declaredPins(in Inputs) []DeclaredPin {
	out := make([]DeclaredPin, 0, len(in.ClassSkills)+len(in.ClassPins))
	for _, s := range in.ClassSkills {
		n, err := canonical.Parse(s)
		if err != nil {
			continue
		}
		out = append(out, DeclaredPin{Kind: "skill", Name: s, Strength: string(n.PinStrength())})
	}
	return append(out, in.ClassPins...)
}

// effectivePinning snapshots the per-tier policies for status visibility and
// runner-side per-item evaluation.
func effectivePinning(in Inputs) *v1.EffectivePinning {
	c := tierPinningPolicy(in.Cluster)
	n := tierPinningPolicy(in.Namespace)
	if c == nil && n == nil {
		return nil
	}
	return &v1.EffectivePinning{Cluster: c, Namespace: n}
}

// EffectivePinningFor snapshots two raw settings-tier specs into the per-tier
// pinning form PinRequirementFor consumes, so a caller outside the resolver
// (`oap pin status`) can evaluate pin requirements without building full
// resolver Inputs. nil when neither tier sets any pinning.
func EffectivePinningFor(cluster, namespace *v1.SettingsSpec) *v1.EffectivePinning {
	return effectivePinning(Inputs{Cluster: cluster, Namespace: namespace})
}

// skillStrengthWord renders a MinStrength in the skill-native vocabulary
// ("sha"/"tag"): a skill ref literally is an @tag or an @sha, so a message
// saying "frozen" would not tell an author what to type. Unrecognized
// strengths fall back to the raw value.
func skillStrengthWord(minStrength string) string {
	switch minStrength {
	case "frozen":
		return "sha"
	case "named":
		return "tag"
	default:
		return minStrength
	}
}

// checkPinning enforces the pin-strength floors statically (no I/O). Mode
// governs fatality: block floors are fatal, approve/warn floors warn, off (and
// absent rules) only observe. The skill kind reports through its own Reasons
// and message wording. eff is the already-computed effectivePinning snapshot,
// passed in to avoid re-deriving the per-tier policies.
func checkPinning(in Inputs, eff *v1.EffectivePinning) []Violation {
	var cPol, nPol *v1.PinningPolicy
	if eff != nil {
		cPol, nPol = eff.Cluster, eff.Namespace
	}
	var vs []Violation
	for _, d := range declaredPins(in) {
		if d.Kind == "skill" && d.Strength == string(pinning.StrengthUnpinned) {
			vs = append(vs, Violation{Reason: ReasonSkillRolling, Fatal: false,
				Message: "skill " + d.Name + " tracks a mutable ref; pin to a SHA for reproducibility/security"})
		}
		req := PinRequirementFor(cPol, nPol, d.Kind, d.Name)
		if !req.Enforced() || req.MinStrength == "" {
			continue
		}
		if pinning.Strength(d.Strength).Level() >= strengthFloorLevel(req.MinStrength) {
			continue
		}
		fatal := req.Mode == v1.PinModeBlock
		if d.Kind == "skill" {
			vs = append(vs, Violation{Reason: ReasonSkillPinningRequired, Fatal: fatal,
				Message: "skill " + d.Name + " must be pinned to at least " + skillStrengthWord(req.MinStrength)})
			continue
		}
		vs = append(vs, Violation{Reason: ReasonPinningRequired, Fatal: fatal,
			Message: d.Kind + " " + d.Name + " must be pinned to at least " + req.MinStrength})
	}
	return vs
}

// PinRequirementFor computes the effective pin requirement for one declared
// dependency. Bypasses are tier-scoped: a cluster bypass shields the rules of
// both tiers; a namespace bypass shields only the namespace rule.
func PinRequirementFor(cluster, namespace *v1.PinningPolicy, kind, name string) PinRequirement {
	cRule, cHas := ruleFor(cluster, kind)
	nRule, nHas := ruleFor(namespace, kind)
	cBypass := bypassFor(cluster, kind, name)
	nBypass := bypassFor(namespace, kind, name)

	var req PinRequirement

	apply := func(r v1.PinningRule) {
		if strengthFloorLevel(r.MinStrength) > strengthFloorLevel(req.MinStrength) {
			req.MinStrength = r.MinStrength
		}
		if m := effectiveMode(r.Mode); req.Mode == "" || modeLevel(m) > modeLevel(req.Mode) {
			req.Mode = m
		}
	}
	shieldedBy := func(b *v1.PinningBypass) {
		if req.BypassReason == "" && b != nil {
			req.BypassReason = b.Reason
		}
	}
	if cHas {
		if cBypass != nil {
			shieldedBy(cBypass)
		} else {
			apply(cRule)
		}
	}
	if nHas {
		switch {
		case cBypass != nil:
			shieldedBy(cBypass)
		case nBypass != nil:
			shieldedBy(nBypass)
		default:
			apply(nRule)
		}
	}
	return req
}
