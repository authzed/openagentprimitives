// Package image adapts OCI image refs to the pinning.Kind contract.
// Syntactic-only: kubelet-resolved digests are recorded by the
// SidecarToolbox controller via the probe pod's containerStatuses imageID —
// live registry lookups happen there, not here. ParseRef classifies refs
// from spec.source.image (or spec.source.inline.baseImage).
package image

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning/registry"
)

// KindName is the registry key for OCI image pinning.
const KindName = "image"

// Kind implements pinning.Kind over OCI image refs. It is syntactic: digest
// resolution happens in the SidecarToolbox reconciler via kubelet's
// containerStatuses, not here.
type Kind struct{}

func init() { registry.Register(&Kind{}) }

func (k *Kind) Name() string { return KindName }

// ParseRef parses an OCI image ref and classifies its pin strength.
//
// Classification:
//   - "@sha256:…" suffix → frozen (immutable digest).
//   - ":<tag>" suffix → named, EXCEPT ":latest" which is a rolling alias and
//     is classified unpinned.
//   - bare ref (no tag, no digest; implicit latest) → unpinned.
func (k *Kind) ParseRef(spec string) (pinning.Ref, error) {
	if spec == "" {
		return pinning.Ref{}, fmt.Errorf("image ref: empty")
	}
	_, tag, digest := SplitRef(spec)
	var strength pinning.Strength
	switch {
	case digest != "":
		strength = pinning.StrengthFrozen
	case tag != "" && tag != "latest":
		strength = pinning.StrengthNamed
	default:
		// bare ref or ":latest" — rolling alias, unpinned in spirit
		strength = pinning.StrengthUnpinned
	}
	return pinning.Ref{Kind: KindName, Spec: spec, Strength: strength}, nil
}

// Resolve returns the syntactic identity encoded in the ref. For a frozen
// ref (digest present), the digest is recorded; for a named ref (tag), the
// version is the tag; for unpinned, the frozen record is empty. Live digest
// resolution is performed by the SidecarToolbox reconciler, not here.
func (k *Kind) Resolve(_ context.Context, r pinning.Ref) (pinning.Frozen, error) {
	_, tag, digest := SplitRef(r.Spec)
	switch {
	case digest != "":
		return pinning.Frozen{Digest: digest}, nil
	case tag != "" && tag != "latest":
		return pinning.Frozen{Version: tag}, nil
	default:
		return pinning.Frozen{}, nil
	}
}

// Verify compares the ref's current syntactic identity against the baseline.
// Digest is compared only when the current ref carries one (i.e., for frozen
// refs) — a named ref's syntactic Resolve has no digest, while the baseline's
// may be fetch-enriched with the kubelet-resolved digest; that enrichment is
// not drift. Content drift for named refs (tag now resolves to a different
// digest) is detected by the SidecarToolbox reconciler comparing the newly
// probed digest against the baseline; this syntactic check catches ref
// rewrites in spec (tag changed, or frozen digest changed).
func (k *Kind) Verify(ctx context.Context, r pinning.Ref, baseline pinning.Frozen) (pinning.DriftReport, error) {
	cur, err := k.Resolve(ctx, r)
	if err != nil {
		return pinning.DriftReport{}, err
	}
	rep := pinning.DriftReport{Old: baseline, New: cur}
	digestChanged := cur.Digest != "" && cur.Digest != baseline.Digest
	if digestChanged || cur.Version != baseline.Version {
		rep.Drifted = true
		rep.Summary = fmt.Sprintf("image ref changed: %s -> %s",
			renderIdentity(baseline), renderIdentity(cur))
	}
	return rep, nil
}

// renderIdentity renders a Frozen for human-readable drift summaries, eliding
// empty components ("sha256:abc (v1)", "sha256:abc", "v1", or "(unpinned)").
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
