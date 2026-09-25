// Package cli adapts CLI toolkit binary assertions to the pinning.Kind
// contract. Syntactic-only: in-sandbox binary measurement (version probe +
// sha256) requires a sandbox exec primitive not yet available. This plan
// ships gate-1 declared-identity strength from the ToolkitTarget ref
// encoding; runtime verification is a follow-up.
package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning/registry"
)

// KindName is the registry key for CLI toolkit binary pinning.
const KindName = "cli"

// Kind implements pinning.Kind over CLI toolkit binary assertions. It is
// syntactic: the caller encodes the ref as "<toolkit-name>[@<version-or-hash>]"
// where the suffix is sourced from ToolkitTarget.PinnedBinaryHash (for a
// sha256 assertion) or ToolkitTarget.VersionRange. There is no live
// measurement in this kind.
type Kind struct{}

// StrengthFor classifies a toolkit's declared pin strength: frozen iff a
// binary hash is asserted, named iff a version range constrains it, else
// unpinned. The single source of truth for the rule the settings gate and
// the toolkit controller both apply.
func StrengthFor(pinnedBinaryHash, versionRange string) pinning.Strength {
	switch {
	case pinnedBinaryHash != "":
		return pinning.StrengthFrozen
	case versionRange != "":
		return pinning.StrengthNamed
	default:
		return pinning.StrengthUnpinned
	}
}

func init() { registry.Register(&Kind{}) }

func (k *Kind) Name() string { return KindName }

// ParseRef parses a CLI toolkit ref and classifies its pin strength.
// Format: "<toolkit-name>[@<version-range-or-hash>]".
//
// Classification:
//   - "@sha256:…" suffix → frozen (a pinnedBinaryHash assertion).
//   - "@<anything-else>" suffix → named (a VersionRange).
//   - bare name (no "@") → unpinned.
func (k *Kind) ParseRef(spec string) (pinning.Ref, error) {
	if spec == "" {
		return pinning.Ref{}, fmt.Errorf("cli ref: empty")
	}
	name, suffix := spec, ""
	if at := strings.LastIndex(spec, "@"); at >= 0 {
		name, suffix = spec[:at], spec[at+1:]
		if suffix == "" {
			return pinning.Ref{}, fmt.Errorf("cli ref: empty suffix after '@': %q", spec)
		}
	}
	if name == "" {
		return pinning.Ref{}, fmt.Errorf("cli ref: empty toolkit name: %q", spec)
	}
	var strength pinning.Strength
	switch {
	case strings.HasPrefix(suffix, "sha256:"):
		strength = pinning.StrengthFrozen
	case suffix != "":
		strength = pinning.StrengthNamed
	default:
		strength = pinning.StrengthUnpinned
	}
	return pinning.Ref{Kind: KindName, Spec: spec, Strength: strength}, nil
}

// Resolve returns the syntactic identity encoded in the ref. For a frozen ref
// (sha256 hash asserted), the hash is the digest; for a named ref (VersionRange),
// the suffix is the version; for an unpinned ref, the frozen record is empty.
func (k *Kind) Resolve(_ context.Context, r pinning.Ref) (pinning.Frozen, error) {
	at := strings.LastIndex(r.Spec, "@")
	if at < 0 {
		return pinning.Frozen{}, nil
	}
	suffix := r.Spec[at+1:]
	if strings.HasPrefix(suffix, "sha256:") {
		return pinning.Frozen{Digest: suffix}, nil
	}
	return pinning.Frozen{Version: suffix}, nil
}

// Verify compares the ref's asserted identity against the baseline.
// Digest is compared only when the current ref carries one — a named ref's
// syntactic Resolve has no digest, while the baseline's may be enriched with
// a measured binary hash; that enrichment is not drift. Version is compared
// for named refs to detect VersionRange rewrites.
func (k *Kind) Verify(ctx context.Context, r pinning.Ref, baseline pinning.Frozen) (pinning.DriftReport, error) {
	cur, err := k.Resolve(ctx, r)
	if err != nil {
		return pinning.DriftReport{}, err
	}
	rep := pinning.DriftReport{Old: baseline, New: cur}
	digestChanged := cur.Digest != "" && cur.Digest != baseline.Digest
	if digestChanged || cur.Version != baseline.Version {
		rep.Drifted = true
		rep.Summary = fmt.Sprintf("cli ref changed: %s -> %s",
			renderIdentity(baseline), renderIdentity(cur))
	}
	return rep, nil
}

// renderIdentity renders a Frozen for human-readable drift summaries.
func renderIdentity(f pinning.Frozen) string {
	switch {
	case f.Digest != "" && f.Version != "":
		return f.Digest + " (" + f.Version + ")"
	case f.Digest != "":
		return f.Digest
	case f.Version != "":
		return f.Version
	default:
		return "(unpinned)"
	}
}
