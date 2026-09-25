// Package skillpin provides pin-record construction helpers shared by
// controllers. Baseline serves the skill controllers (Skill, ClusterSkill);
// Declared serves controllers whose pin identity is expressed entirely in
// spec with no fetch/resolve step (SpiceboxToolkit).
package skillpin

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	skillkind "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/skill"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
)

// Baseline builds the skill PinRecord from a parsed canonical name. When the
// declared identity (strength + version, and digest for frozen refs) is
// unchanged from prev, the previous record's fetch-enriched digest, details,
// and ObservedAt are preserved: a syntactic rebuild has no digest for
// named/unpinned refs while the fetch path may have recorded the SHA the ref
// resolved to — that enrichment is not a change, and idle reconciles must not
// churn status.
func Baseline(n canonical.Name, prev *v1.PinRecord) *v1.PinRecord {
	strength := n.PinStrength()
	rec := &v1.PinRecord{Kind: skillkind.KindName, Strength: string(strength), Version: n.Ref}
	if strength == canonical.PinFrozen {
		rec.Digest = n.Ref
	}
	if prev != nil && prev.Strength == rec.Strength && prev.Version == rec.Version {
		if rec.Digest == "" {
			rec.Digest = prev.Digest // carry fetch enrichment forward
		}
		if rec.Digest == prev.Digest {
			rec.Details = prev.Details
			rec.ObservedAt = prev.ObservedAt
			return rec
		}
	}
	now := metav1.Now()
	rec.ObservedAt = &now
	return rec
}

// Declared builds a PinRecord for a dependency whose identity is declared
// entirely in spec (no fetch/resolve step), preserving ObservedAt when the
// identity is unchanged from prev. The identity is the triple (strength,
// digest, version): if all three match prev, ObservedAt and Details are
// carried forward from prev so that idle reconciles do not churn status.
//
// This is the counterpart to Baseline for controllers like SpiceboxToolkit
// (cli kind) where the pin strength comes directly from spec fields rather
// than a canonical name parse.
func Declared(kind, strength, digest, version string, prev *v1.PinRecord) *v1.PinRecord {
	rec := &v1.PinRecord{Kind: kind, Strength: strength, Digest: digest, Version: version}
	if prev != nil &&
		prev.Strength == rec.Strength &&
		prev.Version == rec.Version &&
		prev.Digest == rec.Digest {
		rec.Details = prev.Details
		rec.ObservedAt = prev.ObservedAt
		return rec
	}
	now := metav1.Now()
	rec.ObservedAt = &now
	return rec
}
