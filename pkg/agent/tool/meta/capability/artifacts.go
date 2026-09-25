package capability

import (
	"encoding/json"
	"sort"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	assetregistry "github.com/authzed/openagentprimitives/pkg/channels/channelassets/registry"
)

func init() { Register(&artifactsCapability{}) }

// artifactsCapability is OPT-IN (default-off). When granted AND the session is
// channel-attached AND at least one renderer is registered, it injects the four
// artifact_* meta tools. The tools cover PRODUCTION + live-view, which are
// ungated by the channel's attach capability (see AvailableAssetKinds);
// attaching a produced artifact to a reply is the separate asset:* axis handled
// by respond_to_user. Ports main.go:595-633.
//
// An optional {renderers:[..]} allowlist narrows the injected kinds to a subset
// of the produceable renderer kinds; an allowlist disjoint from them is a skip
// (fail-closed, never silent).
type artifactsCapability struct{}

// artifactsConfig is the parsed per-capability config. Renderers is an optional
// allowlist intersected with the channel's available kinds. A plain Unmarshal
// tolerates the common {enabled} field (ignored here) but rejects a wrong-typed
// renderers value.
type artifactsConfig struct {
	Renderers []string `json:"renderers"`
}

func (artifactsCapability) Name() string          { return "artifacts" }
func (artifactsCapability) DefaultOn() bool       { return false }
func (artifactsCapability) Infrastructural() bool { return false }

func (artifactsCapability) ParseConfig(raw json.RawMessage) (Config, error) {
	if len(raw) == 0 {
		return artifactsConfig{}, nil
	}
	var cfg artifactsConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (artifactsCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if o.Binding == nil {
		return nil, nil // not channel-attached → inactive, not a skip
	}

	// The artifact service backs every artifact_* tool (FinalizeRevision,
	// history, offer-view). Without it the tools would nil-panic on first use.
	// Any registered renderer makes availableKinds non-empty for ANY
	// channel-attached session (production is ungated), so the renderer check
	// below does NOT catch a missing service — guard it explicitly and fail
	// closed (skip, never silent). Hardens both a misconfigured runner and any
	// e2e that grants artifacts without wiring RunnerEnv.Artifacts.
	if o.Env.Artifacts == nil {
		return nil, &SkipReason{Capability: "artifacts", Reason: "artifact service not available"}
	}

	availableKinds := AvailableAssetKinds(o.Binding)
	if len(availableKinds) == 0 {
		return nil, &SkipReason{Capability: "artifacts", Reason: "no renderer registered"}
	}

	// Apply the optional {renderers} allowlist by intersecting with the
	// produceable renderer kinds. Preserves availableKinds' sorted order.
	if cfg, ok := o.Config.(artifactsConfig); ok && len(cfg.Renderers) > 0 {
		availableKinds = intersectKinds(availableKinds, cfg.Renderers)
		if len(availableKinds) == 0 {
			return nil, &SkipReason{Capability: "artifacts", Reason: "no granted renderer available on channel"}
		}
	}

	// Per-kind and minimum input-size caps (main.go:601-610).
	perKind := map[string]int64{}
	minInput := int64(256 << 10)
	for _, k := range availableKinds {
		if r, ok := assetregistry.ByKind(k); ok {
			perKind[k] = r.MaxInputSize()
			if r.MaxInputSize() < minInput {
				minInput = r.MaxInputSize()
			}
		}
	}

	tools := []tool.Tool{
		meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
			Client:              o.Env.Client,
			Artifacts:           o.Env.Artifacts,
			AvailableKinds:      availableKinds,
			MaxInputBytes:       minInput,
			MaxInputBytesByKind: perKind,
			FileDownloader:      o.Env.FileDownloader,
		}),
		meta.NewArtifactAwait(meta.ArtifactAwaitConfig{Client: o.Env.Client, Artifacts: o.Env.Artifacts}),
		meta.NewArtifactHistory(meta.ArtifactHistoryConfig{Artifacts: o.Env.Artifacts}),
		meta.NewArtifactOfferView(meta.ArtifactOfferViewConfig{
			Artifacts:      o.Env.Artifacts,
			AvailableKinds: availableKinds,
			// The transport that will RENDER the offer, so the tool can ask
			// whether it has a live-view surface before publishing one — the
			// OUTBOUND binding, exactly as respond_to_user's ChannelKind is.
			// Reading the input binding here would suppress the offer on every
			// webhook-spawned session, whose input kind has no surface and whose
			// reader is on a transport that does.
			ChannelKind:       o.OutboundBinding().Kind,
			NATSPublish:       o.Env.NATSPublish,
			NATSSubjectPrefix: o.Env.SubjectPrefix,
			EnvelopeSigner:    o.Env.EnvelopeSigner,
			Client:            o.Env.Client,
			RenderFetch:       o.Env.RenderFetch,
			MarkupGen:         o.Env.MarkupGen,
		}),
	}

	// File tools ride this grant too (design doc §5.4) alongside the four
	// artifact_* tools above, via the shared modalityMetaTools helper — see
	// its doc for why attachmentsCapability also calls it, on its own,
	// without requiring this broader grant.
	tools = append(tools, modalityMetaTools(o.Env)...)

	return tools, nil
}

// AvailableAssetKinds returns the renderer-kind names an agent may PRODUCE and
// live-view on binding's channel, sorted by name; nil for a nil binding (not
// channel-attached ⇒ no artifact tools at all).
//
// It is UNGATED by the channel's attach capability: every registered renderer
// kind that answers AgentSelectable() true is produceable, and produced
// artifacts are live-viewed via a browser link. An operator-only kind (the
// workshop draft's "oap", the MCP-UI widget's "mcpui") is registered and
// rendered like any other but is never on this list. Whether a produceable
// kind can be ATTACHED to a reply is a separate axis, gated on the channel's
// asset:* capability.
//
// The runner calls this to gate the prompt's production instructions on the same
// grant. It returns only kind NAMES; the []runner.AssetKind computation stays in
// internal/cmd/runner so this package need not import pkg/agent/runner.
func AvailableAssetKinds(binding *spiceboxv1alpha1.ChannelBinding) []string {
	if binding == nil {
		return nil
	}
	var kinds []string
	for _, r := range assetregistry.All() {
		// An operator-only kind (the workshop draft, the MCP-UI widget) is
		// rendered and served like any other but never offered for production.
		if !r.AgentSelectable() {
			continue
		}
		kinds = append(kinds, r.Kind())
	}
	sort.Strings(kinds)
	return kinds
}

// intersectKinds returns the elements of available that also appear in allow,
// preserving available's order.
func intersectKinds(available, allow []string) []string {
	allowed := make(map[string]struct{}, len(allow))
	for _, a := range allow {
		allowed[a] = struct{}{}
	}
	var out []string
	for _, k := range available {
		if _, ok := allowed[k]; ok {
			out = append(out, k)
		}
	}
	return out
}
