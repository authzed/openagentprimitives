package projectors

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// agentsDetailProjector renders one AgentClass into the tabbed detail view:
// Prompt (the system prompt), Tools (MCP/sidecar/toolkit refs), Skills, Settings
// (resolved model/budget), and Identity. The Sessions tab is frontend-only (it
// fetches /sessions), so it is deliberately not produced here.
type agentsDetailProjector struct{}

func (agentsDetailProjector) Resource() string { return "agents" }

func (agentsDetailProjector) Detail(ctx context.Context, c client.Client, ns, name string) (*config.ResourceDetail, error) {
	var ac spiceboxv1alpha1.AgentClass
	found, err := getObj(ctx, c, client.ObjectKey{Namespace: ns, Name: name}, &ac)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	status, reason := projectStatus(ac.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, "Valid")
	d := &config.ResourceDetail{
		Name:         ac.Name,
		Namespace:    ac.Namespace,
		Scope:        "namespaced",
		Status:       status,
		StatusReason: reason,
		Description:  agentDescription(&ac),
		ManageCmd:    editCmd("agentclass", ac.Name, ac.Namespace),
	}

	d.Sections = appendSection(d.Sections, textSection("prompt", "Prompt", agentPrompt(&ac.Spec.SystemPrompt)))
	d.Sections = appendSection(d.Sections, listSection("tools", "Tools", agentTools(&ac)))
	d.Sections = appendSection(d.Sections, listSection("skills", "Skills", agentSkills(&ac)))
	d.Sections = appendSection(d.Sections, agentSettings(&ac))
	d.Sections = appendSection(d.Sections, agentIdentity(&ac))
	d.Sections = appendSection(d.Sections, agentOapInstall(&ac))
	d.Sections = appendSection(d.Sections, healthSection(ac.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, "Valid", status))
	return d, nil
}

func agentDescription(ac *spiceboxv1alpha1.AgentClass) string {
	if ac.Spec.Description != "" {
		return ac.Spec.Description
	}
	return ac.Spec.DisplayName
}

// agentPrompt renders the AgentClass system prompt. An inline prompt is shown
// verbatim; a ConfigMap-sourced prompt shows a provenance note (admind does not
// fetch the ConfigMap content).
func agentPrompt(p *spiceboxv1alpha1.PromptSource) string {
	if p.Inline != "" {
		return p.Inline
	}
	if p.ConfigMapRef != nil {
		return fmt.Sprintf("Prompt sourced from ConfigMap %q key %q.", p.ConfigMapRef.Name, p.ConfigMapRef.Key)
	}
	return "(no system prompt)"
}

// agentTools lists every tool-source ref the class declares: MCPServer +
// SidecarToolbox refs (namespaced → link id = <ns>/<ref>) and each toolkit-
// backed ToolBundle's toolspecs (cluster-scoped → link id = <toolspec>).
func agentTools(ac *spiceboxv1alpha1.AgentClass) []config.ListItem {
	var items []config.ListItem
	for _, m := range ac.Spec.MCPServers {
		items = append(items, config.ListItem{
			Title:    m.Name,
			Subtitle: "mcpserver → " + m.Ref,
			Link:     &config.Link{Entity: "tool", ID: ac.Namespace + "/" + m.Ref},
			Badges:   []config.Badge{{Key: "kind", Value: "mcpserver"}},
		})
	}
	for _, s := range ac.Spec.SidecarToolboxes {
		items = append(items, config.ListItem{
			Title:    s.Name,
			Subtitle: "sidecartoolbox → " + s.Ref,
			Link:     &config.Link{Entity: "tool", ID: ac.Namespace + "/" + s.Ref},
			Badges:   []config.Badge{{Key: "kind", Value: "sidecartoolbox"}},
		})
	}
	for _, b := range ac.Spec.ToolBundles {
		for _, ts := range b.Toolspecs {
			items = append(items, config.ListItem{
				Title:    ts,
				Subtitle: fmt.Sprintf("bundle %s (class %s)", b.Name, b.Class),
				// SpiceboxToolspec is cluster-scoped: the tool detail id is the
				// bare toolspec name (no namespace prefix).
				Link:   &config.Link{Entity: "tool", ID: ts},
				Badges: []config.Badge{{Key: "kind", Value: "toolspec"}},
			})
		}
	}
	return items
}

// agentSkills lists the class's opted-in skills: the local Name an operator
// recognises as the item label, and the canonical Ref as its value. No entity
// link is attached: a Skill CR's detail id is its metadata.name (a hash of
// the canonical), so a Ref-keyed link would not resolve.
func agentSkills(ac *spiceboxv1alpha1.AgentClass) []config.ListItem {
	items := make([]config.ListItem, 0, len(ac.Spec.Skills))
	for _, s := range ac.Spec.Skills {
		items = append(items, config.ListItem{Title: s.Name, Subtitle: s.Ref})
	}
	return items
}

// agentSettings renders the resolved model/budget from status.EffectiveSettings
// when stamped, falling back to the class spec (which may be nil = inherited).
func agentSettings(ac *spiceboxv1alpha1.AgentClass) config.Section {
	model := ""
	var budget *spiceboxv1alpha1.BudgetConfig
	if es := ac.Status.EffectiveSettings; es != nil {
		if es.Model.Name != "" {
			model = modelString(es.Model.Provider, es.Model.Name)
		}
		b := es.Budget
		budget = &b
	}
	if model == "" && ac.Spec.Model != nil {
		model = modelString(ac.Spec.Model.Provider, ac.Spec.Model.Name)
	}
	if budget == nil {
		budget = ac.Spec.Budget
	}
	if model == "" {
		model = "(inherited)"
	}

	fields := []config.Field{fld("Model", model)}
	if budget != nil {
		fields = append(fields,
			fld("Max turns", fmt.Sprintf("%d", budget.MaxTurns)),
			fld("Max tokens", fmt.Sprintf("%d", budget.MaxTokens)),
			fld("Max duration", budget.MaxDuration.Duration.String()),
		)
	}
	fields = append(fields, fld("Tool session log", ac.Spec.ToolSessionLog))
	return fieldsSection("settings", "Settings", fields...)
}

func modelString(provider, name string) string {
	if provider == "" {
		return name
	}
	return provider + "/" + name
}

// agentIdentity renders the identity mode + the bound AgentIdentity ref (linked
// to its detail page when set).
func agentIdentity(ac *spiceboxv1alpha1.AgentClass) config.Section {
	mode := ac.Spec.IdentityMode
	if mode == "" {
		mode = "agent"
	}
	identityID := ""
	if ac.Spec.AgentIdentity != "" {
		identityID = ac.Namespace + "/" + ac.Spec.AgentIdentity
	}
	return fieldsSection("identity", "Identity",
		fld("Identity mode", mode),
		fldLink("Agent identity", ac.Spec.AgentIdentity, "identity", identityID),
	)
}

// agentOapInstall renders the provenance of an AgentClass installed from a
// `.oap` bundle (status.oapInstall, stamped by the AgentClass reconciler from
// the oap-source annotation). Absent for classes created by other means
// (kubectl apply, wizard, …) — appendSection drops the zero Section it
// returns in that case. For a registry-sourced install it also derives a
// copy-pasteable `oap agent pull` command the React UI renders as a
// copy-to-clipboard field; a file-sourced install has no OCI ref to pull from,
// so that field is omitted.
func agentOapInstall(ac *spiceboxv1alpha1.AgentClass) config.Section {
	oi := ac.Status.OapInstall
	if oi == nil {
		return config.Section{}
	}

	source := oi.SourceRef
	if oi.SourceKind == "file" {
		source = "local file"
	}

	fields := []config.Field{
		fld("Source", source),
		fld("Digest", oi.Digest),
		fld("Version", oi.Version),
		fld("Installed", timeOrEmpty(&oi.InstalledAt)),
	}
	if oi.SourceKind == "registry" && oi.SourceRef != "" && oi.Digest != "" {
		fields = append(fields, fld("Pull command",
			fmt.Sprintf("oap agent pull %s@%s -o %s.oap", oi.SourceRef, oi.Digest, ac.Name)))
	}
	return fieldsSection("oap", "OAP Bundle", fields...)
}

// join renders a string slice as a comma-separated value for a field.
func join(xs []string) string { return strings.Join(xs, ", ") }

func init() { config.RegisterDetail(&agentsDetailProjector{}) }
