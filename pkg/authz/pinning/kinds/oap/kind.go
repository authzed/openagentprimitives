// Package oap adapts .oap agent-container OCI registry refs to the
// pinning.Kind contract. Unlike the other pinning kinds (image, skill, mcp,
// cli) — which are syntactic-only, deferring live identity resolution to a
// continuously-running reconciler — an installed .oap bundle has no
// reconciler behind it: `oap agent install` is a one-shot CLI operation with
// nothing else watching the ref afterward. So Resolve here actually
// HEAD-resolves the ref against the live OCI registry (via
// pkg/platform/oap/oci.Resolve — no blob content is pulled) to produce the frozen
// manifest digest recorded as install provenance.
//
// Ref grammar ("host[:port]/name[:tag|@digest]") is parsed via oras-go's
// registry.ParseReference — the same parser pkg/platform/oap/oci itself uses — kept
// in lockstep so any ref ParseRef accepts is one Resolve (and the
// underlying oci package) can actually act on.
package oap

import (
	"context"
	"fmt"

	ociregistry "oras.land/oras-go/v2/registry"

	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
)

// KindName is the registry key for .oap agent-container pinning.
const KindName = "oap"

// resolveOpts configures the live HEAD-resolve Resolve performs. Production
// always uses the zero value (TLS, no PlainHTTP) — real .oap registries are
// addressed over TLS, and the pinning.Kind interface has no room to thread a
// per-call Options through (Ref only carries Kind/Spec/Strength). It is
// package-private and overridden only by this package's own integration test
// (kind_integration_test.go) to point Resolve at an in-process PlainHTTP fake
// registry — the same test-seam-var shape as pkg/platform/identity/refresh's
// SetHTTPClient, rather than mutating global HTTP transport state.
var resolveOpts = oci.Options{}

// Kind implements pinning.Kind over .oap OCI registry refs.
type Kind struct{}

func init() { registry.Register(&Kind{}) }

func (k *Kind) Name() string { return KindName }

// ParseRef parses an .oap OCI registry ref and classifies its pin strength.
//
// Classification:
//   - "@sha256:…" suffix (a syntactically valid digest) → frozen (immutable
//     identity).
//   - ":<tag>" suffix → named, EXCEPT ":latest" which is a rolling alias and
//     is classified unpinned.
//   - bare ref (no tag, no digest; implicit latest) → unpinned.
func (k *Kind) ParseRef(spec string) (pinning.Ref, error) {
	if spec == "" {
		return pinning.Ref{}, fmt.Errorf("oap ref: empty")
	}
	parsed, err := ociregistry.ParseReference(spec)
	if err != nil {
		return pinning.Ref{}, fmt.Errorf("oap ref: %w", err)
	}
	var strength pinning.Strength
	switch {
	case isDigestRef(parsed):
		strength = pinning.StrengthFrozen
	case parsed.Reference != "" && parsed.Reference != "latest":
		strength = pinning.StrengthNamed
	default:
		strength = pinning.StrengthUnpinned
	}
	return pinning.Ref{Kind: KindName, Spec: spec, Strength: strength}, nil
}

// Resolve HEAD-resolves r's ref against the live OCI registry (via
// pkg/platform/oap/oci.Resolve; no blob content is pulled) and returns the manifest
// digest as Frozen.Digest — the install-time provenance record `oap agent
// install` writes for a registry-ref source. When the ref carries a tag,
// the tag is also recorded as Frozen.Version (the human-readable identity);
// a digest-only ref leaves Version empty.
func (k *Kind) Resolve(ctx context.Context, r pinning.Ref) (pinning.Frozen, error) {
	digest, err := oci.Resolve(ctx, r.Spec, resolveOpts)
	if err != nil {
		return pinning.Frozen{}, fmt.Errorf("oap: %w", err)
	}
	return FrozenFromDigest(r.Spec, digest)
}

// FrozenFromDigest builds the Frozen record Resolve would produce for ref,
// given a digest the caller already learned some other way — e.g. `oap agent
// install` calls oci.Pull to fetch the bundle, which resolves the manifest
// digest in the very same round trip. Recording that digest via this helper
// (rather than a fresh Resolve call) avoids a redundant registry request and
// guarantees the recorded pin matches byte-for-byte what was actually pulled
// and installed, not a possibly-different digest a later re-resolve of a
// moving tag could return.
func FrozenFromDigest(ref, digest string) (pinning.Frozen, error) {
	parsed, err := ociregistry.ParseReference(ref)
	if err != nil {
		return pinning.Frozen{}, fmt.Errorf("oap ref: %w", err)
	}
	f := pinning.Frozen{Digest: digest}
	if !isDigestRef(parsed) && parsed.Reference != "" {
		f.Version = parsed.Reference
	}
	return f, nil
}

// Verify re-resolves r's live manifest digest and compares it against
// baseline. Unlike the syntactic pinning kinds' Verify (which only detects a
// rewritten ref in spec), this IS a genuine live-drift check: a named ref's
// tag can move to point at different content between installs, and
// re-resolving against the registry catches that directly.
func (k *Kind) Verify(ctx context.Context, r pinning.Ref, baseline pinning.Frozen) (pinning.DriftReport, error) {
	cur, err := k.Resolve(ctx, r)
	if err != nil {
		return pinning.DriftReport{}, err
	}
	rep := pinning.DriftReport{Old: baseline, New: cur}
	if cur.Digest != baseline.Digest || cur.Version != baseline.Version {
		rep.Drifted = true
		rep.Summary = fmt.Sprintf("oap manifest changed: %s -> %s", renderIdentity(baseline), renderIdentity(cur))
	}
	return rep, nil
}

// isDigestRef reports whether parsed's Reference is a valid content digest
// (as opposed to a tag, or empty/bare).
func isDigestRef(parsed ociregistry.Reference) bool {
	_, err := parsed.Digest()
	return err == nil
}

// renderIdentity renders a Frozen for human-readable drift summaries, eliding
// empty components ("sha256:abc (v1.2.0)", "sha256:abc", or "(unresolved)").
func renderIdentity(f pinning.Frozen) string {
	switch {
	case f.Digest != "" && f.Version != "":
		return f.Digest + " (" + f.Version + ")"
	case f.Digest != "":
		return f.Digest
	default:
		return "(unresolved)"
	}
}
