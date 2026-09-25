package projectors

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// toolsDetailProjector renders a single tool CR. "tools" is a union slug over
// four CRDs; the detail dispatches by which one exists at (ns, name):
// namespaced MCPServer / SidecarToolbox (id = <ns>/<name>) and cluster-scoped
// SpiceboxToolkit / SpiceboxToolspec (id = <name>, ns == "").
type toolsDetailProjector struct{}

func (toolsDetailProjector) Resource() string { return "tools" }

func (toolsDetailProjector) Detail(ctx context.Context, c client.Client, ns, name string) (*config.ResourceDetail, error) {
	key := client.ObjectKey{Namespace: ns, Name: name}

	// A namespaced id only ever names an MCPServer or SidecarToolbox; a bare
	// (cluster) id only ever names a Toolkit or Toolspec. Probing only the
	// applicable pair avoids a spurious cross-scope Get.
	if ns != "" {
		var m spiceboxv1alpha1.MCPServer
		if found, err := getObj(ctx, c, key, &m); err != nil {
			return nil, err
		} else if found {
			return mcpServerDetail(&m), nil
		}
		var b spiceboxv1alpha1.SidecarToolbox
		if found, err := getObj(ctx, c, key, &b); err != nil {
			return nil, err
		} else if found {
			return sidecarToolboxDetail(&b), nil
		}
		return nil, nil
	}

	var k spiceboxv1alpha1.SpiceboxToolkit
	if found, err := getObj(ctx, c, key, &k); err != nil {
		return nil, err
	} else if found {
		return toolkitDetail(&k), nil
	}
	var s spiceboxv1alpha1.SpiceboxToolspec
	if found, err := getObj(ctx, c, key, &s); err != nil {
		return nil, err
	} else if found {
		return toolspecDetail(&s), nil
	}
	return nil, nil
}

func mcpServerDetail(m *spiceboxv1alpha1.MCPServer) *config.ResourceDetail {
	status, reason := projectStatus(m.Status.Conditions, spiceboxv1alpha1.MCPServerConditionReachable, "Reachable")
	d := &config.ResourceDetail{
		Name:         m.Name,
		Namespace:    m.Namespace,
		Scope:        "namespaced",
		Status:       status,
		StatusReason: reason,
		Description:  m.Spec.Intent,
		ManageCmd:    editCmd("mcpserver", m.Name, m.Namespace),
	}

	credential := m.Spec.Auth.Credential
	if credential == "" {
		credential = m.Name // runtime falls back to metadata.name
	}
	d.Sections = appendSection(d.Sections, fieldsSection("connection", "Connection",
		fld("Kind", "mcpserver"),
		fld("Endpoint", m.Spec.Server.URL),
		fld("Transport", m.Spec.Server.Transport),
		fld("Auth type", m.Spec.Auth.Type),
		fld("Credential", credential),
		fld("Call timeout", durationOrEmpty(m.Spec.CallTimeout.Duration)),
	))
	d.Sections = appendSection(d.Sections, listSection("tools", "Tools", mcpTools(m.Spec.Tools)))
	d.Sections = appendSection(d.Sections, observedToolsSection(m.Status.ObservedTools))
	d.Sections = appendSection(d.Sections, healthSection(m.Status.Conditions, spiceboxv1alpha1.MCPServerConditionReachable, "Reachable", status))
	return d
}

func sidecarToolboxDetail(b *spiceboxv1alpha1.SidecarToolbox) *config.ResourceDetail {
	status, reason := projectSidecarToolboxStatus(b.Status.Conditions)
	d := &config.ResourceDetail{
		Name:         b.Name,
		Namespace:    b.Namespace,
		Scope:        "namespaced",
		Status:       status,
		StatusReason: reason,
		Description:  b.Spec.Intent,
		ManageCmd:    editCmd("sidecartoolbox", b.Name, b.Namespace),
	}

	source := b.Spec.Source.Image
	if source == "" && b.Spec.Source.Inline != nil {
		source = "inline (" + b.Spec.Source.Inline.BaseImage + ")"
	}
	d.Sections = appendSection(d.Sections, fieldsSection("connection", "Connection",
		fld("Kind", "sidecartoolbox"),
		fld("Source", source),
		fld("Sandbox class", b.Spec.Sandbox.Class),
		fld("Transport port", fmt.Sprintf("%d", b.Spec.Transport.Port)),
		fld("Upstream provider", b.Spec.UpstreamAuth.Provider),
	))
	// Policy: this toolbox's OWN contribution to the effective egress allowlist —
	// it merges with (never replaces) the referenced SpiceboxClass's own
	// network.allowedHosts at session-resolve time (see
	// AgentSession.status.resolvedSidecarToolboxes[].effectiveAllowedHosts), which
	// this static CR view cannot show. Mirrors toolspecDetail's "Policy" section.
	d.Sections = appendSection(d.Sections, fieldsSection("policy", "Policy",
		fld("Allowed hosts", join(b.Spec.Sandbox.Network.AllowedHosts)),
	))
	d.Sections = appendSection(d.Sections, listSection("tools", "Tools", mcpTools(b.Spec.Tools)))
	d.Sections = appendSection(d.Sections, observedToolsSection(b.Status.ObservedTools))
	d.Sections = appendSection(d.Sections, healthSection(b.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionValid, "Valid", status))
	return d
}

func toolkitDetail(k *spiceboxv1alpha1.SpiceboxToolkit) *config.ResourceDetail {
	status, reason := projectStatus(k.Status.Conditions, spiceboxv1alpha1.SpiceboxToolkitConditionValid, "Valid")
	d := &config.ResourceDetail{
		Name:         k.Name,
		Scope:        "cluster",
		Status:       status,
		StatusReason: reason,
		Description:  k.Spec.Docs,
		ManageCmd:    editCmd("spiceboxtoolkit", k.Name, ""),
	}

	d.Sections = appendSection(d.Sections, fieldsSection("command", "Command",
		fld("Kind", "toolkit"),
		fld("Binary", k.Spec.Target.Binary),
		fld("Version range", k.Spec.Target.VersionRange),
		fld("Parser", parserString(k.Spec.Parser)),
		fld("Stream format", k.Spec.StreamFormat),
	))
	items := make([]config.ListItem, 0, len(k.Spec.Subcommands))
	for _, sc := range k.Spec.Subcommands {
		var badges []config.Badge
		if sc.Effects.Destructive {
			badges = append(badges, config.Badge{Key: "effect", Value: "destructive"})
		}
		items = append(items, config.ListItem{
			Title:    strings.Join(sc.Path, " "),
			Subtitle: sc.Description,
			Badges:   badges,
		})
	}
	d.Sections = appendSection(d.Sections, listSection("subcommands", "Subcommands", items))
	d.Sections = appendSection(d.Sections, healthSection(k.Status.Conditions, spiceboxv1alpha1.SpiceboxToolkitConditionValid, "Valid", status))
	return d
}

func toolspecDetail(s *spiceboxv1alpha1.SpiceboxToolspec) *config.ResourceDetail {
	status, reason := projectStatus(s.Status.Conditions, spiceboxv1alpha1.SpiceboxToolspecConditionValid, "Valid")
	d := &config.ResourceDetail{
		Name:         s.Name,
		Scope:        "cluster",
		Status:       status,
		StatusReason: reason,
		Description:  s.Spec.Intent,
		ManageCmd:    editCmd("spiceboxtoolspec", s.Name, ""),
	}

	toolkitRef := s.Spec.Toolkit.Name
	if toolkitRef != "" && s.Spec.Toolkit.Revision != "" {
		toolkitRef = toolkitRef + "@" + s.Spec.Toolkit.Revision
	}
	// spec.toolkit.name is the CANONICAL toolkit name (e.g. "git"); the actual
	// SpiceboxToolkit CR's metadata.name (e.g. "git-v1") is stamped on
	// status.resolvedToolkit. The tool detail resolves a cluster id by
	// metadata.name, so the link MUST target resolvedToolkit — a canonical-keyed
	// link 404s. "<builtin>" / unresolved has no CR to open, so no link.
	toolkitLinkID := s.Status.ResolvedToolkit
	if toolkitLinkID == "<builtin>" {
		toolkitLinkID = ""
	}
	d.Sections = appendSection(d.Sections, fieldsSection("command", "Command",
		fld("Kind", "toolspec"),
		fldLink("Toolkit", toolkitRef, "tool", toolkitLinkID),
		fld("Resolved toolkit", s.Status.ResolvedToolkit),
		fld("Allowed subcommands", join(s.Spec.AllowSubcommands)),
	))
	// Policy: the egress/filesystem/credential allowances the CLI wrapper grants.
	d.Sections = appendSection(d.Sections, fieldsSection("policy", "Policy",
		fld("Allowed network", join(s.Spec.Allow.Network.Destinations)),
		fld("Allowed paths", join(s.Spec.Allow.Filesystem.PathsUnder)),
		fld("Required credentials", join(s.Spec.Allow.Creds.Required)),
	))
	d.Sections = appendSection(d.Sections, healthSection(s.Status.Conditions, spiceboxv1alpha1.SpiceboxToolspecConditionValid, "Valid", status))
	return d
}

// mcpTools flattens declared MCPServerTools (shared by MCPServer and
// SidecarToolbox) into list items, badging read-only / destructive effects.
func mcpTools(tools []spiceboxv1alpha1.MCPServerTool) []config.ListItem {
	items := make([]config.ListItem, 0, len(tools))
	for _, t := range tools {
		var badges []config.Badge
		if t.Effects.ReadOnly {
			badges = append(badges, config.Badge{Key: "effect", Value: "readOnly"})
		}
		if t.Effects.Destructive {
			badges = append(badges, config.Badge{Key: "effect", Value: "destructive"})
		}
		items = append(items, config.ListItem{Title: t.Name, Subtitle: t.Intent, Badges: badges})
	}
	return items
}

// observedToolsSection lists the tool names the server's last successful
// tools/list probe returned. Empty → a zero Section the caller drops.
func observedToolsSection(observed []string) config.Section {
	items := make([]config.ListItem, 0, len(observed))
	for _, name := range observed {
		items = append(items, config.ListItem{Title: name})
	}
	return listSection("observed", "Observed tools", items)
}

func parserString(p spiceboxv1alpha1.ToolkitParserConfig) string {
	if p.Name != "" {
		return p.Kind + " (" + p.Name + ")"
	}
	return p.Kind
}

// durationOrEmpty renders a duration, or "" for the zero value so the field is
// dropped rather than showing "0s".
func durationOrEmpty(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}

func init() { config.RegisterDetail(&toolsDetailProjector{}) }
