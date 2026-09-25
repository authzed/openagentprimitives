// Package mcp adapts MCP server manifest assertions to the pinning.Kind
// contract. Syntactic-only: live manifest fetching and hashing happen in
// the MCPServer reconciler and the runner via CanonicalManifestHash — a
// live MCP server has no client-resolvable content beyond what those
// callers observe. There is no "named" rung: an MCP ref is frozen iff a
// hash is asserted, else unpinned.
package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning/registry"
)

// KindName is the registry key for MCP manifest pinning.
const KindName = "mcp"

// Kind implements pinning.Kind over MCPServer manifest assertions. It is
// syntactic: the caller encodes the ref as "<mcpserver-name>[@<hash>]" with
// the hash sourced from spec.pinnedManifestHash. Live manifest fetching and
// hashing happen in the MCPServer reconciler and the runner via
// CanonicalManifestHash — a live MCP server has no client-resolvable content
// beyond what those callers observe. There is no "named" rung: an MCP ref is
// frozen iff a hash is asserted, else unpinned.
type Kind struct{}

func init() { registry.Register(&Kind{}) }

func (k *Kind) Name() string { return KindName }

// ParseRef parses an MCP server ref and classifies its pin strength.
// Format: "<mcpserver-name>[@<hash>]". A hash suffix makes the ref frozen;
// the absence of a hash suffix makes it unpinned.
func (k *Kind) ParseRef(spec string) (pinning.Ref, error) {
	if spec == "" {
		return pinning.Ref{}, fmt.Errorf("mcp ref: empty")
	}
	name, hash := spec, ""
	if at := strings.LastIndex(spec, "@"); at >= 0 {
		name, hash = spec[:at], spec[at+1:]
		if hash == "" {
			return pinning.Ref{}, fmt.Errorf("mcp ref: empty hash after '@': %q", spec)
		}
	}
	if name == "" {
		return pinning.Ref{}, fmt.Errorf("mcp ref: empty server name: %q", spec)
	}
	strength := pinning.StrengthUnpinned
	if hash != "" {
		strength = pinning.StrengthFrozen
	}
	return pinning.Ref{Kind: KindName, Spec: spec, Strength: strength}, nil
}

// Resolve returns the syntactic identity encoded in the ref. For a frozen ref
// (hash asserted), the hash is both the digest and the full identity; for an
// unpinned ref, the frozen record is empty. Live manifest identity is computed
// by the MCPServer reconciler and runner via CanonicalManifestHash, not here.
func (k *Kind) Resolve(_ context.Context, r pinning.Ref) (pinning.Frozen, error) {
	if at := strings.LastIndex(r.Spec, "@"); at >= 0 {
		return pinning.Frozen{Digest: r.Spec[at+1:]}, nil
	}
	return pinning.Frozen{}, nil
}

// Verify compares the ref's asserted hash against the baseline digest.
// Drift is reported only when both the ref and baseline carry a digest and
// they differ; an unpinned ref never drifts syntactically.
func (k *Kind) Verify(ctx context.Context, r pinning.Ref, baseline pinning.Frozen) (pinning.DriftReport, error) {
	cur, err := k.Resolve(ctx, r)
	if err != nil {
		return pinning.DriftReport{}, err
	}
	rep := pinning.DriftReport{Old: baseline, New: cur}
	if cur.Digest != "" && cur.Digest != baseline.Digest {
		rep.Drifted = true
		rep.Summary = fmt.Sprintf("mcp asserted manifest hash changed: %s -> %s", baseline.Digest, cur.Digest)
	}
	return rep, nil
}
