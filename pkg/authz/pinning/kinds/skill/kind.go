// Package skill adapts pkg/tools/skills/canonical to the pinning.Kind contract.
// Syntactic-only: git resolution of a named ref to a concrete SHA happens in
// the SkillSource fetch path, and the Skill controller records the fetched
// SHA on the baseline PinRecord directly.
package skill

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning/registry"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
)

// KindName is the registry key for skill pinning.
const KindName = "skill"

// Kind implements pinning.Kind over canonical skill names.
type Kind struct{}

func init() { registry.Register(&Kind{}) }

func (k *Kind) Name() string { return KindName }

// ParseRef parses a canonical skill name and classifies its pin strength.
func (k *Kind) ParseRef(spec string) (pinning.Ref, error) {
	n, err := canonical.Parse(spec)
	if err != nil {
		return pinning.Ref{}, fmt.Errorf("skill ref: %w", err)
	}
	return pinning.Ref{Kind: KindName, Spec: n.String(), Strength: n.PinStrength()}, nil
}

// Resolve reports the ref's syntactic identity: for a frozen ref the sha is
// both digest and version; for a named ref only the version (the SHA a tag
// resolves to is recorded by the fetch path, not here).
func (k *Kind) Resolve(_ context.Context, r pinning.Ref) (pinning.Frozen, error) {
	n, err := canonical.Parse(r.Spec)
	if err != nil {
		return pinning.Frozen{}, fmt.Errorf("skill resolve: %w", err)
	}
	f := pinning.Frozen{Version: n.Ref}
	if n.PinStrength() == canonical.PinFrozen {
		f.Digest = n.Ref
	}
	return f, nil
}

// Verify compares the ref's current syntactic identity against the baseline.
// Digest is compared only for frozen refs — a syntactic Resolve of a named or
// unpinned ref has no digest, while the baseline's may be fetch-enriched with
// the SHA the ref resolved to; that enrichment is not drift. Content drift
// for named refs is detected by the fetch path (fetched SHA vs baseline
// digest); this syntactic check catches ref rewrites in spec.
func (k *Kind) Verify(ctx context.Context, r pinning.Ref, baseline pinning.Frozen) (pinning.DriftReport, error) {
	cur, err := k.Resolve(ctx, r)
	if err != nil {
		return pinning.DriftReport{}, err
	}
	rep := pinning.DriftReport{Old: baseline, New: cur}
	digestChanged := cur.Digest != "" && cur.Digest != baseline.Digest
	if digestChanged || cur.Version != baseline.Version {
		rep.Drifted = true
		rep.Summary = fmt.Sprintf("skill ref changed: %s -> %s",
			renderIdentity(baseline), renderIdentity(cur))
	}
	return rep, nil
}

// renderIdentity renders a Frozen for human-readable drift summaries, eliding
// empty components ("deadbee (v1.2.0)", "v1.2.0", "deadbee", or "(unpinned)").
func renderIdentity(f pinning.Frozen) string {
	switch {
	case f.Digest != "" && f.Version != "" && f.Digest != f.Version:
		return f.Digest + " (" + f.Version + ")"
	case f.Digest != "":
		return f.Digest
	case f.Version != "":
		return f.Version
	default:
		return "(unpinned)"
	}
}
