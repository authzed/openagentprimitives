package capability

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

func init() { Register(&fineGrainedInfoLeakageCapability{}) }

// FineGrainedInfoLeakageName is the capability key gating per-datum
// information-flow tracking (the pt_tag provenance lattice).
//
// Exported because the consumers live outside this package — the PostToolCall
// mint hook and the egress audience check — and both must ask the SAME
// question. A second string literal somewhere is how one of them ends up
// minting tags the other never reads.
const FineGrainedInfoLeakageName = "fine_grained_info_leakage"

// fineGrainedInfoLeakageCapability gates the per-datum half of info-leakage
// tracking: minting a pt_tag per datum a tool call touched, and checking a
// response against the INTERSECTION of its tags' audiences rather than the
// session's whole taint set.
//
// Opt-in, default-off, and deliberately so. It is not a strictly-better
// upgrade an existing agent should receive silently:
//
//   - it costs a LookupSubjects per tool call and a durable record per datum,
//     which every class would start paying whether or not anyone wanted it;
//   - it changes what the disclosure gate PERMITS. Per-datum is usually
//     narrower, but "usually" is not a property to hand an operator without
//     asking — a payload the coarse check blocked can become deliverable once
//     the audience is computed from the data actually in it.
//
// With the key absent, behaviour is byte-for-byte what it is today: nothing is
// minted, no tag is read, and the session-wide taint set decides. That is also
// why it composes with the coverage fallback rather than competing with it —
// a session that enables this mid-flight has partially-tagged payloads, which
// are not fully tag-covered and therefore already degrade to the coarse check.
// No transition case is needed.
type fineGrainedInfoLeakageCapability struct{}

// FineGrainedInfoLeakageConfig is the parsed per-capability config.
//
// Empty today: the capability is a switch, and the axes worth configuring
// (which resource types to tag, how long redacted content is retained) are not
// yet built and would be guesses. It exists as a named type so those can be
// added without changing any consumer's call shape.
type FineGrainedInfoLeakageConfig struct{}

// fineGrainedInfoLeakageWire is the on-the-wire shape. Enabled is declared so
// the common {enabled} flag survives DisallowUnknownFields — an empty struct
// would reject a perfectly legal {"enabled": false}.
type fineGrainedInfoLeakageWire struct {
	// Enabled is the common capability flag; nil means absent, hence enabled.
	Enabled *bool `json:"enabled,omitempty"`
}

func (fineGrainedInfoLeakageCapability) Name() string          { return FineGrainedInfoLeakageName }
func (fineGrainedInfoLeakageCapability) DefaultOn() bool       { return false }
func (fineGrainedInfoLeakageCapability) Infrastructural() bool { return false }

// ParseConfig rejects anything it does not recognize rather than ignoring it.
//
// A typo'd key here is worse than usual: the operator believes they have
// configured per-datum provenance, the class runs coarse, and every disclosure
// is decided by a wider audience than they think they asked for. The
// AgentClass controller surfaces the error as CapabilitiesValid.
func (fineGrainedInfoLeakageCapability) ParseConfig(raw json.RawMessage) (Config, error) {
	if len(raw) == 0 {
		return FineGrainedInfoLeakageConfig{}, nil
	}
	var wire fineGrainedInfoLeakageWire
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return nil, fmt.Errorf("%s: parsing capability config: %w", FineGrainedInfoLeakageName, err)
	}
	return FineGrainedInfoLeakageConfig{}, nil
}

// Offer contributes one tool: derive_tag, the model's way to carry provenance
// through content it SYNTHESIZES from several tagged sources (a fact fused from
// two documents), which no tool boundary sees. The rest of this capability gates
// hook behaviour (the mint, the emitter, the egress checks), not the tool
// surface.
//
// The tool is offered only when the runner wired MintDerivedTag; without it a
// bare OfferContext (a test, a future caller) would yield a tool that refuses
// every call, so offer nothing instead — returning (nil, nil) rather than a
// SkipReason, which would log a warning on every session that enables it.
//
// derive_tag is sound by construction: it mints only DERIVED tags (readers =
// the intersection of real inputs, never a widening or a fabricated reader), and
// this Offer authorizes every claimed input against pt_tag#access first, so a
// model cannot derive from a tag it was never granted or guess an id.
func (fineGrainedInfoLeakageCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	// derive_tag requires BOTH the mint backing AND a derivation validator. A nil
	// validator means the platform cannot check that a derivation introduces no
	// information beyond its sources — so the tool is not offered (fail-closed),
	// and content that would have carried a synthesized tag falls to the coarse
	// floor instead of laundering under a too-wide id.
	if o.Env.MintDerivedTag == nil || o.Env.DeriveValidator == nil {
		return nil, nil
	}
	mint := func(ctx context.Context, derivedFrom []string, content string) (string, error) {
		// 1. Access: the session must hold every source it derives from.
		if o.Env.TagAccessCheck != nil {
			for _, id := range derivedFrom {
				ok, err := o.Env.TagAccessCheck(ctx, id)
				if err != nil {
					return "", fmt.Errorf("checking access to %s: %w", id, err)
				}
				if !ok {
					return "", fmt.Errorf("this session may not derive from %s — it was never granted that tag", id)
				}
			}
		}
		// 2. Resolve the sources' stored bytes for the validator to judge against.
		if o.Env.ResolveTagContents == nil {
			return "", fmt.Errorf("cannot resolve source contents to validate the derivation")
		}
		sources, err := o.Env.ResolveTagContents(ctx, derivedFrom)
		if err != nil {
			return "", fmt.Errorf("resolving source contents: %w", err)
		}
		// 3. Validate with the dedicated model (clean context). Reject/fail-closed.
		valid, reason, err := o.Env.DeriveValidator.ValidateDerivation(ctx, sources, content)
		if err != nil {
			return "", fmt.Errorf("could not validate the derivation: %w", err)
		}
		if !valid {
			return "", fmt.Errorf("the derivation was rejected — it introduces information not in its named sources: %s", reason)
		}
		// 4. Mint, storing content so egress content-binding accepts the pasted region.
		return o.Env.MintDerivedTag(ctx, derivedFrom, content)
	}
	return []tool.Tool{meta.NewDeriveTag(meta.DeriveTagConfig{Mint: mint})}, nil
}
