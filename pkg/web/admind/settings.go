package admind

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// settingsRow is one labeled fact about the cluster settings, grouped under
// "Limits" (ceilings) or "Defaults" (fallbacks). Note carries a short
// clarification (deprecation, tri-state semantics) when useful.
type settingsRow struct {
	Group string `json:"group"` // "Limits" | "Defaults"
	Label string `json:"label"`
	Value string `json:"value"`
	Note  string `json:"note,omitempty"`
}

// settingsResponse is the GET /admin/v1/config/settings payload: a bespoke
// projection of the singleton ClusterAgentSettings, NOT a generic projector
// table. Note is non-empty only when there is no ClusterAgentSettings.
type settingsResponse struct {
	Rows      []settingsRow `json:"rows"`
	ManageCmd string        `json:"manageCmd"`
	Note      string        `json:"note,omitempty"`
}

const (
	groupLimits   = "Limits"
	groupDefaults = "Defaults"

	settingsManageCmd    = "oap settings wizard  ·  kubectl edit clusteragentsettings cluster"
	settingsNotFoundNote = "no ClusterAgentSettings configured — cluster governance uses built-in defaults"
)

// handleConfigSettings serves the bespoke cluster Settings panel: the resolved
// singleton ClusterAgentSettings named "cluster", projected into labeled rows.
// Registered at the exact path /admin/v1/config/settings, which net/http
// ServeMux dispatches ahead of the /config/{resource} projector wildcard.
// Not-found yields empty rows + an explanatory note, never a 404/500.
func (a *Admind) handleConfigSettings(w http.ResponseWriter, r *http.Request) {
	var cs spiceboxv1alpha1.ClusterAgentSettings
	err := a.cfg.K8s.Get(r.Context(),
		client.ObjectKey{Name: spiceboxv1alpha1.ClusterAgentSettingsName}, &cs)
	if apierrors.IsNotFound(err) {
		writeJSON(w, http.StatusOK, settingsResponse{
			Rows:      []settingsRow{},
			ManageCmd: settingsManageCmd,
			Note:      settingsNotFoundNote,
		})
		return
	}
	if err != nil {
		a.cfg.Logger.Info("admind: get cluster settings failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "get cluster settings failed: "+err.Error())
		return
	}

	b := &settingsRowBuilder{rows: []settingsRow{}}
	projectLimits(b, cs.Spec.Limits)
	projectDefaults(b, cs.Spec.Defaults)
	projectModelCatalog(b, cs.Spec.ModelCatalog)
	writeJSON(w, http.StatusOK, settingsResponse{Rows: b.rows, ManageCmd: settingsManageCmd})
}

type settingsRowBuilder struct{ rows []settingsRow }

func (b *settingsRowBuilder) add(group, label, value, note string) {
	b.rows = append(b.rows, settingsRow{Group: group, Label: label, Value: value, Note: note})
}

// addAllowlist renders a tri-state allowlist pointer: nil → omit (no
// constraint), non-nil empty → "deny-all", non-empty → the joined entries.
func (b *settingsRowBuilder) addAllowlist(group, label string, p *[]string) {
	if p == nil {
		return
	}
	if len(*p) == 0 {
		b.add(group, label, "deny-all", "empty allowlist — no entries permitted")
		return
	}
	b.add(group, label, strings.Join(*p, ", "), "")
}

func projectLimits(b *settingsRowBuilder, lim *spiceboxv1alpha1.SettingsLimits) {
	if lim == nil {
		return
	}
	if bg := lim.Budget; bg != nil {
		if bg.MaxTurns > 0 {
			b.add(groupLimits, "Budget · Max Turns", strconv.FormatInt(int64(bg.MaxTurns), 10), "")
		}
		if bg.MaxTokens > 0 {
			b.add(groupLimits, "Budget · Max Tokens", strconv.FormatInt(bg.MaxTokens, 10), "")
		}
		if bg.MaxDuration.Duration > 0 {
			b.add(groupLimits, "Budget · Max Duration", bg.MaxDuration.Duration.String(), "")
		}
	}
	if len(lim.DeniedModels) > 0 {
		b.add(groupLimits, "Denied Models", strings.Join(lim.DeniedModels, ", "), "union across tiers")
	}
	if lim.AllowModelOverride != nil {
		v := "false (catalog-only)"
		if *lim.AllowModelOverride {
			v = "true (bring-your-own model allowed)"
		}
		b.add(groupLimits, "Allow Model Override", v, "top-down: cluster grants, lower tiers restrict")
	}
	b.addAllowlist(groupLimits, "Allowed Toolkits", lim.AllowedToolkits)
	b.addAllowlist(groupLimits, "Allowed Skills", lim.AllowedSkills)
	if lim.AllowedMCPServers != nil {
		if len(*lim.AllowedMCPServers) == 0 {
			b.add(groupLimits, "Allowed MCP Servers", "deny-all", "empty allowlist — no entries permitted")
		} else {
			names := make([]string, 0, len(*lim.AllowedMCPServers))
			for _, s := range *lim.AllowedMCPServers {
				names = append(names, s.Name)
			}
			b.add(groupLimits, "Allowed MCP Servers", strings.Join(names, ", "), "")
		}
	}
	if len(lim.DeniedSkills) > 0 {
		b.add(groupLimits, "Denied Skills", strings.Join(lim.DeniedSkills, ", "), "union across tiers")
	}
	if lim.Pinning != nil {
		b.add(groupLimits, "Pinning", pinningValue(lim.Pinning), "")
	}
	if lim.ToolGuard != nil {
		b.add(groupLimits, "Tool Guard Ceiling", toolGuardCeilingValue(lim.ToolGuard), "")
	}
	if lim.ContentInspectors != nil {
		ids := make([]string, 0, len(*lim.ContentInspectors))
		for _, ci := range *lim.ContentInspectors {
			ids = append(ids, ci.ID)
		}
		val := "(none)"
		if len(ids) > 0 {
			val = strings.Join(ids, ", ")
		}
		b.add(groupLimits, "Content Inspectors", val, "")
	}
}

func projectDefaults(b *settingsRowBuilder, def *spiceboxv1alpha1.SettingsDefaults) {
	if def == nil {
		return
	}
	if m := def.Model; m != nil {
		b.add(groupDefaults, "Model", m.Provider+"/"+m.Name, "")
	}
	if bg := def.Budget; bg != nil {
		// Guard zero-value fields: a zero default budget means "no default set",
		// not "0 turns/tokens max" — emitting "0" rows would mislead operators
		// (mirrors the projectLimits guards).
		if bg.MaxTurns > 0 {
			b.add(groupDefaults, "Budget · Max Turns", strconv.FormatInt(int64(bg.MaxTurns), 10), "")
		}
		if bg.MaxTokens > 0 {
			b.add(groupDefaults, "Budget · Max Tokens", strconv.FormatInt(bg.MaxTokens, 10), "")
		}
		if bg.MaxDuration.Duration > 0 {
			b.add(groupDefaults, "Budget · Max Duration", bg.MaxDuration.Duration.String(), "")
		}
	}
	if az := def.Authz; az != nil {
		if az.ApprovalTimeout != nil {
			b.add(groupDefaults, "Authz · Approval Timeout", az.ApprovalTimeout.Duration.String(), "")
		}
		if az.InformationLeakageApprovalTTL != nil {
			b.add(groupDefaults, "Authz · Leakage Approval TTL",
				az.InformationLeakageApprovalTTL.Duration.String(), "")
		}
		if az.ScopeMaxLLMLatencyMs != nil {
			b.add(groupDefaults, "Authz · Max LLM Latency",
				strconv.FormatInt(int64(*az.ScopeMaxLLMLatencyMs), 10)+"ms", "")
		}
	}
	if tg := def.ToolGuard; tg != nil {
		b.add(groupDefaults, "Tool Guard Policy",
			fmt.Sprintf("%d rule(s)", len(tg.Rules)), "")
	}
}

// projectModelCatalog renders the model catalog: one row per entry, showing
// provider, the estimated per-MTok price (or "no price"), and a default marker.
func projectModelCatalog(b *settingsRowBuilder, catalog *[]spiceboxv1alpha1.ModelCatalogEntry) {
	if catalog == nil || len(*catalog) == 0 {
		return
	}
	for _, e := range *catalog {
		var parts []string
		if e.Provider != "" {
			parts = append(parts, e.Provider)
		}
		if e.HasPrice() {
			parts = append(parts, fmt.Sprintf("$%.2f in / $%.2f out per MTok (est.)", e.InputPerMTok, e.OutputPerMTok))
		} else {
			parts = append(parts, "no price")
		}
		note := ""
		if e.Default {
			note = "default"
		}
		b.add(groupLimits, "Model Catalog · "+e.Name, strings.Join(parts, " · "), note)
	}
}

// pinningValue summarizes a PinningPolicy: each rule as kind(min,mode) plus a
// bypass count. An empty policy renders "—".
func pinningValue(p *spiceboxv1alpha1.PinningPolicy) string {
	if len(p.Rules) == 0 && len(p.Bypass) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(p.Rules))
	for _, r := range p.Rules {
		mode := r.Mode
		if mode == "" {
			mode = spiceboxv1alpha1.PinModeApprove
		}
		min := r.MinStrength
		if min == "" {
			min = "any"
		}
		parts = append(parts, fmt.Sprintf("%s(min=%s,mode=%s)", r.Kind, min, mode))
	}
	out := strings.Join(parts, ", ")
	if len(p.Bypass) > 0 {
		out += fmt.Sprintf(" · %d bypass", len(p.Bypass))
	}
	return out
}

// toolGuardCeilingValue summarizes the set ceiling sub-fields. All-nil → "—".
func toolGuardCeilingValue(tg *spiceboxv1alpha1.ToolGuardCeiling) string {
	var parts []string
	if tg.MaxFailureThreshold != nil {
		parts = append(parts, "maxFailureThreshold="+strconv.FormatInt(int64(*tg.MaxFailureThreshold), 10))
	}
	if tg.MinInitialCoolOff != nil {
		parts = append(parts, "minInitialCoolOff="+tg.MinInitialCoolOff.Duration.String())
	}
	if tg.MinAction != nil {
		parts = append(parts, "minAction="+*tg.MinAction)
	}
	if tg.MaxCallsPerTurn != nil {
		parts = append(parts, "maxCallsPerTurn="+strconv.FormatInt(int64(*tg.MaxCallsPerTurn), 10))
	}
	if tg.MaxCalls != nil && tg.Window != nil {
		parts = append(parts, "maxCalls="+strconv.FormatInt(int64(*tg.MaxCalls), 10)+"/"+tg.Window.Duration.String())
	}
	if tg.MaxEgressBytes != nil {
		parts = append(parts, "maxEgressBytes="+strconv.FormatInt(*tg.MaxEgressBytes, 10))
	}
	if tg.MaxIngressBytes != nil {
		parts = append(parts, "maxIngressBytes="+strconv.FormatInt(*tg.MaxIngressBytes, 10))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, ", ")
}
