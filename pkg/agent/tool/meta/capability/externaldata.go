package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	websearchregistry "github.com/authzed/openagentprimitives/pkg/tools/websearch/registry"
)

func init() { Register(&externalDataCapability{}) }

// externalDataCapability is the DECLARATION half of web search/fetch. Its
// search/fetch tools ship with the ap-websearchd
// SidecarToolbox (pkg/tools/websearch/deploy/sidecartoolbox.yaml) —
// referenced directly in an AgentClass's spec.sidecarToolboxes, exactly like
// any other sidecar — NOT through this capability's Offer.
//
// That is not a shortcut; it is the only thing Offer CAN do, verified by
// reading Assemble (pkg/agent/tool/meta/capability/assemble.go) before
// writing this: AssembleDeps.NonMetaTools (sandbox/mcp/sidecar tools,
// sidecarTools among them) is appended UNCONDITIONALLY —
// `appendUnique(deps.NonMetaTools)` runs regardless of any capability's
// grant, config, or Offer result. A capability's Offer contributes or
// withholds META tools (the pkg/agent/tool/meta ones); it has no lever over
// a sidecar-sourced tool at all. This mirrors agentBuilderCapability
// exactly ("granting it sanctions nothing by itself" is the one true thing
// its Offer says), and for the same two reasons:
//
//  1. An AgentClass author states clear intent ("this class exposes
//     external data access") that ClusterAgentSettings/AgentSettings
//     capability ceilings and `oap agent capabilities` can read and gate,
//     independently of which SidecarToolbox happens to be referenced.
//  2. A granted class that forgot to also reference the sidecar (or whose
//     sidecar never became reachable) gets a logged, truthful reason
//     instead of silent nothing — Step 4's requirement.
//
// DefaultOn is false: even though spec R3 keeps the URL SPACE open by
// default once the tool is reachable, reachability itself is opt-in, the
// same posture as every other capability that can put new surface in front
// of an agent (workspace, agent_builder, knowledge).
//
// Considered and rejected: dispatching search/fetch in-runner from this
// capability's Offer, calling pkg/tools/websearch/registry directly the way
// a plain meta tool would. Rejected because it is the exact architecture
// design R1/R2 exist to supersede — internal/cmd/websearchd/main.go's own
// package doc states the reason plainly: an in-runner call is only
// automatically content-guarded/toolguard-budgeted when an admin opts a
// named meta tool INTO toolguard (pkg/agent/runner/toolguard_meta.go); a
// gated (MCP/sidecar) tool gets it unconditionally. It would also lose the
// SEP-1913 trust.returnMetadata declaration entirely — meta tools have no
// analogous per-datum-taint mechanism (see
// pkg/agent/runner/leakagewiring.LookupMCPToolUntrustedSource, which reads
// only from an MCPServer/SidecarToolbox CR's Tools[].Trust field).
type externalDataCapability struct{}

// externalDataConfig is the grant's narrowing config (the "judgment point":
// whether config can narrow anything, following the workspace precedent
// where the grant opts INTO the more dangerous half).
//
// Fetch, absent (nil) => true: spec R3 keeps the URL space OPEN by default,
// so the grant must EXPLICITLY narrow to search-only with `"fetch": false`.
// This is workspace's `{"apply": true}` precedent in the OPPOSITE polarity,
// and deliberately so: workspace's dangerous half (write-back) defaults
// OFF, because R3 has no equivalent ruling for workspace and a write is the
// more dangerous default to avoid; external_data's dangerous half (fetching
// an arbitrary URL) defaults ON, because R3 is an explicit ruling that
// keeping the URL space open is what makes this primitive useful for its
// actual purpose. Both grants share the same shape underneath: the config's
// only power is to move ONE particular half further from its default, never
// to loosen a default that already sits at "off".
//
// AllowedDomains, when non-empty, is meant to restrict fetch to those
// domains. Both fields are RECORDED and validated here; NEITHER is enforced
// by this capability's Offer, which — as the type doc above proves from
// Assemble's own code — never touches the sidecar's tool list at all.
// Enforcement is future work for whichever task wires a granted class's
// config into the sidecar's own dispatch/inspector chain. This
// mirrors the repo's own established precedent for exactly this situation:
// SidecarToolboxSpec.MCPUIAppTools is "ACCEPTED BUT NOT CONSUMED", and
// AgentSession.status.resolvedSidecarToolboxes' effectiveAllowedHosts is
// "recorded, not enforced" until a DNS-aware egress controller exists.
type externalDataConfig struct {
	Fetch          *bool    `json:"fetch,omitempty"`
	AllowedDomains []string `json:"allowedDomains,omitempty"`
}

// FetchEnabled reports whether cfg keeps fetch (the more dangerous, open-URL
// half) available. A nil Fetch is the R3 default: open. Exported-shaped
// (capitalized) so the future enforcement task this file's doc comments
// point at has a single, already-tested place to read the grant's decision
// from, rather than re-deriving the nil-pointer default at a second site.
func (cfg externalDataConfig) FetchEnabled() bool {
	return cfg.Fetch == nil || *cfg.Fetch
}

func (externalDataCapability) Name() string          { return "external_data" }
func (externalDataCapability) DefaultOn() bool       { return false }
func (externalDataCapability) Infrastructural() bool { return false }

func (externalDataCapability) ParseConfig(raw json.RawMessage) (Config, error) {
	if len(raw) == 0 {
		return externalDataConfig{}, nil
	}
	var cfg externalDataConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Offer never contributes a tool — see the type doc comment for why that is
// a structural fact about Assemble, not a limitation of this file. It still
// names the most SPECIFIC true reason it can, rather than one generic
// string, by consulting pkg/tools/websearch/registry: the same
// process-wide registry ap-websearchd's own main() consults
// (registry.Get(cfg.backendName) — internal/cmd/websearchd/main.go's run()).
//
// In every production runner build today that registry is empty: R2 keeps a
// websearch backend's code (and its credential) OUT of the runner process
// entirely, on purpose, so nothing here ever blank-imports
// pkg/tools/websearch/bravesearch. That is not this capability failing to
// find something it should — it is the fact the design turns on, and
// TestExternalData_RegistryEmptyIsTheDesignedRunnerState below pins it so a
// future change that DOES blank-import a backend into the runner is forced
// to re-examine why before this capability's oldest message goes quiet.
func (externalDataCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if !o.Granted {
		return nil, nil
	}
	if len(websearchregistry.Keys()) == 0 {
		return nil, &SkipReason{
			Capability: "external_data",
			Reason: "no websearch backend is registered in this process — search/fetch ship with the ap-websearchd " +
				"sidecar (reference it in this class's sidecarToolboxes), which has its own registry and its own " +
				"WEBSEARCH_API_KEY credential that this process cannot see; granting external_data alone attaches nothing",
		}
	}
	return nil, &SkipReason{
		Capability: "external_data",
		Reason: "search/fetch tools ship with the ap-websearchd sidecar (reference it in this class's " +
			"sidecarToolboxes), not through this capability; granting external_data alone attaches nothing",
	}
}
