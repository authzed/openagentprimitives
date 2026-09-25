// tools_inventory.go implements the `inventory` tool: a read-only snapshot of
// this cluster's REAL registered capabilities, channel kinds, and artifact
// renderers, plus the CR-backed SpiceboxClass catalog, sandbox backend
// registry, model catalog, cluster skills, this workshop's own MCP servers,
// and the cluster-tier governance ceilings. It exists so the builder agent
// never proposes a design the running cluster cannot actually serve, or that
// the cluster admin has forbidden — see the design spec's Assess step and
// Ruling C (ceilings come from ClusterAgentSettings).
//
// Every in-process registry this file reads from is populated by
// registries.go's blank imports, never by this file itself — a registry
// registries.go forgets to link reports an empty set here, not a compile
// error, which is exactly what tools_inventory_test.go's drift test exists to
// catch. The CR-backed fields (SandboxClasses, Models, ClusterSkills,
// MCPCatalog, Ceilings) instead depend on RBAC granted to the workshop SA —
// config/manager/workshop-reader.yaml for the three cluster-scoped kinds,
// the workshop namespace's own Role for MCPServer.
package workshopmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	assetregistry "github.com/authzed/openagentprimitives/pkg/channels/channelassets/registry"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	sandboxregistry "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/registry"
)

// toolInventory is the name `inventory` announces on the MCP surface.
// Exported as a const (mirroring pkg/web/uidemo/leadflow's ToolX convention)
// so a test spells it identically to what the server offers.
const toolInventory = "inventory"

// inventorySnapshot is the `inventory` tool's full read-only report. Every
// field is a flat list of names — the builder asks "does X exist", never
// "give me every field of X" — sorted where the source doesn't already
// guarantee order, so the reported JSON is stable across calls.
type inventorySnapshot struct {
	// Capabilities lists every registered meta-tool capability name
	// (pkg/agent/tool/meta/capability.Ordered) — what an AgentClass may grant,
	// whether or not any is active for a particular session.
	Capabilities []string `json:"capabilities"`
	// ChannelKinds lists every registered channel transport
	// (pkg/channels/channelkinds/registry.Names).
	ChannelKinds []string `json:"channelKinds"`
	// Renderers lists every registered artifact-renderer kind
	// (pkg/channels/channelassets/registry.All, by Renderer.Kind()).
	Renderers []string `json:"renderers"`
	// SandboxClasses lists every SpiceboxClass CR's name (cluster-scoped) —
	// the sandbox TEMPLATES a SidecarToolbox actually names via
	// spec.sandbox.class (pkg/apis/v1alpha1/sidecartoolbox_types.go). NOT the
	// backend kind set — see SandboxBackends for that.
	SandboxClasses []string `json:"sandboxClasses"`
	// SandboxBackends lists every registered sandbox BACKEND kind
	// (pkg/tools/sandboxkinds/registry.Keys) — "pod", "agent-sandbox", … — the
	// pluggable runtime a SpiceboxClass is built on, not a name a
	// SidecarToolbox can reference directly.
	SandboxBackends []string `json:"sandboxBackends"`
	// Models lists the cluster model catalog's permitted model names
	// (ClusterAgentSettings.Spec.ModelCatalog).
	Models []string `json:"models"`
	// ClusterSkills lists every ClusterSkill's canonical name, cluster-wide —
	// distinct from a namespaced Skill, which this SA token cannot list outside
	// its own workshop namespace and which inventory does not report.
	ClusterSkills []string `json:"clusterSkills"`
	// MCPCatalog lists the MCPServer CRs that currently exist in THIS
	// workshop's own namespace (Identity.Namespace) — never any other
	// namespace. inventory never enumerates another agent.
	MCPCatalog []string `json:"mcpCatalog"`
	// Ceilings is this cluster's governance ceiling
	// (ClusterAgentSettings.Spec.Limits, round-tripped through JSON) — budget
	// caps, denied models, allowed sandbox kinds, and so on — so the builder
	// never proposes what the cluster forbids (design spec Ruling C). Always
	// non-nil; an empty map when the cluster sets no ceiling.
	Ceilings map[string]any `json:"ceilings"`
	// SidecarImages are the first-party sidecar images THIS cluster admits in a
	// SidecarToolbox's spec.source.image (apimage.WorkshopSidecarImages — the
	// same set the admission webhook enforces). Ref is the reference to use
	// verbatim: registry-qualified when this cluster has a trusted registry,
	// the bare local tag otherwise. Purpose says which one to reach for.
	SidecarImages []sidecarImage `json:"sidecarImages"`
	// Providers are the sign-in flows this cluster ships (the embedded
	// /providers/ catalog, provider.All()) — the CLOSED set an authored
	// AgentIdentity's or MCP server's auth.provider may name. Shape says what
	// the person does to obtain it: "oauth" is a per-person sign-in (oauth-mcp
	// is the generic one that fits most services), "bearer" a pasted token
	// (github-pat, slack-bot-token, …). The builder reads this so it names a
	// real id instead of inventing a service name that does not exist here.
	Providers []providerInfo `json:"providers"`
}

// providerInfo is one entry of Providers: a sign-in flow this cluster ships,
// as the id an auth.provider names it by, plus what it is for and its shape.
type providerInfo struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Shape       string `json:"shape"`
}

// sidecarImage is one entry of SidecarImages: a first-party image THIS
// cluster admits, the reference to name it by, and what it's for.
type sidecarImage struct {
	Name    string `json:"name"`
	Ref     string `json:"ref"`
	Purpose string `json:"purpose"`
}

// registerInventory wires the `inventory` tool onto mcpSrv. inventory is
// read-only and auto (no human approval) — that policy is declared in Task
// 8's SidecarToolbox; this file only implements the handler, it does not
// decide its own approval requirement.
func (s *Server) registerInventory(mcpSrv *mcp.Server) {
	mcpSrv.AddTool(&mcp.Tool{
		Name: toolInventory,
		Description: "Read-only snapshot of this cluster's real capabilities, channel kinds, " +
			"artifact renderers, sandbox classes and backends, permitted models, cluster skills, " +
			"this workshop's own MCP servers, and the cluster's governance ceilings, the " +
			"first-party sidecar images this cluster admits (with the reference to use), and the " +
			"sign-in flows (providers) an account can be connected with. Call this " +
			"before proposing any agent design — it reports what the running cluster can actually " +
			"do and what it forbids, never what another agent has.",
		InputSchema: map[string]any{"type": "object"},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleInventory)
}

// handleInventory answers the `inventory` tool call. It never mutates
// anything and never enumerates another agent — no AgentSession or
// AgentClass listing outside this workshop's own namespace.
func (s *Server) handleInventory(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	snap := inventorySnapshot{
		Capabilities:    capabilityNames(),
		ChannelKinds:    chregistry.Names(),
		Renderers:       rendererNames(),
		SandboxBackends: sandboxregistry.Keys(),
		Ceilings:        map[string]any{},
		SidecarImages:   sidecarImages(),
		Providers:       providerCatalog(),
	}

	var classes spiceboxv1alpha1.SpiceboxClassList
	if err := s.K8s.List(ctx, &classes); err != nil {
		return s.inventoryReadErr("listing SpiceboxClasses", err), nil
	}
	snap.SandboxClasses = spiceboxClassNames(classes.Items)

	var cas spiceboxv1alpha1.ClusterAgentSettings
	if err := s.K8s.Get(ctx, client.ObjectKey{Name: spiceboxv1alpha1.ClusterAgentSettingsName}, &cas); err != nil {
		return s.inventoryReadErr("reading ClusterAgentSettings", err), nil
	}
	ceilings, err := ceilingsFromLimits(cas.Spec.Limits)
	if err != nil {
		// Never silently drop: SettingsLimits is a plain struct, so this cannot
		// fail in practice, but the caller still gets a structured tool error
		// instead of an empty/partial snapshot claiming success.
		return s.toolErr("building ceilings: %v", err), nil
	}
	snap.Ceilings = ceilings
	snap.Models = modelCatalogNames(cas.Spec.ModelCatalog)

	var skills spiceboxv1alpha1.ClusterSkillList
	if err := s.K8s.List(ctx, &skills); err != nil {
		return s.inventoryReadErr("listing ClusterSkills", err), nil
	}
	snap.ClusterSkills = clusterSkillNames(skills.Items)

	var mcpServers spiceboxv1alpha1.MCPServerList
	if err := s.K8s.List(ctx, &mcpServers, client.InNamespace(s.Identity.Namespace)); err != nil {
		return s.inventoryReadErr("listing MCPServers", err), nil
	}
	snap.MCPCatalog = mcpServerNames(mcpServers.Items)

	body, err := json.Marshal(snap)
	if err != nil {
		// Every field here is a plain slice/map of strings — json.Marshal
		// cannot fail in practice — but per repo convention no error path is
		// silently dropped.
		return s.toolErr("marshaling inventory: %v", err), nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil
}

// inventoryReadErr distinguishes an apiserver DENIAL (surfaced verbatim, per
// Server.deniedResult's contract — never re-worded) from any other read
// failure (a structured tool error carrying what step failed).
func (s *Server) inventoryReadErr(step string, err error) *mcp.CallToolResult {
	if apierrors.IsForbidden(err) {
		return s.deniedResult(err)
	}
	return s.toolErr("%s: %v", step, err)
}

// capabilityNames lists every registered meta-tool capability's name, sorted.
// capability.Ordered's own order is assembly order (infra first,
// introspection last), not alphabetical — inventory reports a set, so the two
// orders don't need to agree.
func capabilityNames() []string {
	ordered := capability.Ordered()
	names := make([]string, 0, len(ordered))
	for _, c := range ordered {
		names = append(names, c.Name())
	}
	sort.Strings(names)
	return names
}

// rendererNames lists every registered artifact-renderer kind, sorted.
// channelassets/registry has no Names()-style helper of its own (unlike
// channelkinds/registry), so this extracts Renderer.Kind() from All() itself.
func rendererNames() []string {
	all := assetregistry.All()
	names := make([]string, 0, len(all))
	for _, r := range all {
		names = append(names, r.Kind())
	}
	sort.Strings(names)
	return names
}

// sidecarImages reports the admissible sidecar image set — ranged from
// apimage.WorkshopSidecarImages (Task 1's single source of truth for what
// the workshop admission webhook allows in a SidecarToolbox's
// spec.source.image), never transcribed as a name list here — with Ref
// resolved against this cluster's trusted registry, WORKSHOP_TRUSTED_IMAGE_REGISTRY
// (set only on the workshop-identity branch of BuildSidecarPod, empty on a
// local/desktop install with no trusted registry).
func sidecarImages() []sidecarImage {
	reg := os.Getenv("WORKSHOP_TRUSTED_IMAGE_REGISTRY")
	out := make([]sidecarImage, 0, len(apimage.WorkshopSidecarImages))
	for _, img := range apimage.WorkshopSidecarImages {
		ref := img.LocalRef()
		if reg != "" {
			ref = img.RegistryRef(reg)
		}
		out = append(out, sidecarImage{
			Name:    img.Name,
			Ref:     ref,
			Purpose: img.WorkshopPurpose,
		})
	}
	return out
}

// providerCatalog reports the sign-in flows this cluster ships — ranged from
// provider.All() (the embedded /providers/ library, already sorted by id),
// never transcribed here — so a provider added to the embed is reported with
// no second place to keep in sync. This is the closed set an auth.provider may
// name; a builder reading it reaches for a real id (oauth-mcp for a per-person
// OAuth service, github-pat for a GitHub token) instead of inventing one.
func providerCatalog() []providerInfo {
	all := provider.All()
	out := make([]providerInfo, 0, len(all))
	for _, p := range all {
		out = append(out, providerInfo{
			Name:        p.ID,
			Title:       p.Title,
			Description: p.Description,
			Shape:       p.Shape,
		})
	}
	return out
}

// modelCatalogNames lists the cluster model catalog's permitted model names,
// sorted. A nil catalog (no ModelCatalog entries at all) reports an empty,
// non-nil slice.
func modelCatalogNames(catalog *[]spiceboxv1alpha1.ModelCatalogEntry) []string {
	names := []string{}
	if catalog == nil {
		return names
	}
	for _, m := range *catalog {
		names = append(names, m.Name)
	}
	sort.Strings(names)
	return names
}

// clusterSkillNames lists every ClusterSkill's canonical name, sorted.
func clusterSkillNames(items []spiceboxv1alpha1.ClusterSkill) []string {
	names := make([]string, 0, len(items))
	for _, sk := range items {
		names = append(names, sk.Spec.CanonicalName)
	}
	sort.Strings(names)
	return names
}

// spiceboxClassNames lists every SpiceboxClass CR's name, sorted —
// cluster-scoped, so no namespace filter applies. This is what
// SidecarToolbox.Spec.Sandbox.Class actually references
// (pkg/apis/v1alpha1/sidecartoolbox_types.go); it is a different set from
// SandboxBackends, the pluggable runtime KIND registry a SpiceboxClass is
// built on.
func spiceboxClassNames(items []spiceboxv1alpha1.SpiceboxClass) []string {
	names := make([]string, 0, len(items))
	for _, c := range items {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return names
}

// mcpServerNames lists the MCPServer CRs present in this workshop's own
// namespace, sorted — by CR name only, never a field naming another agent.
func mcpServerNames(items []spiceboxv1alpha1.MCPServer) []string {
	names := make([]string, 0, len(items))
	for _, m := range items {
		names = append(names, m.Name)
	}
	sort.Strings(names)
	return names
}

// ceilingsFromLimits round-trips SettingsLimits through JSON into a
// map[string]any instead of hand-listing its fields: a new ceiling field
// added to SettingsLimits is reported automatically, with no second place to
// keep in sync (CLAUDE.md's "no documenting absent config" applies just as
// much to a stale hand-copied field list as to a stale doc). A nil limits
// (the cluster set no ceiling at all) reports an empty, non-nil map.
func ceilingsFromLimits(limits *spiceboxv1alpha1.SettingsLimits) (map[string]any, error) {
	out := map[string]any{}
	if limits == nil {
		return out, nil
	}
	b, err := json.Marshal(limits)
	if err != nil {
		return nil, fmt.Errorf("marshal settings limits: %w", err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("unmarshal settings limits into map: %w", err)
	}
	return out, nil
}
