package settings

import (
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ToStatus converts the resolver's EffectiveSettings into the CRD-serializable
// v1alpha1.EffectiveSettings stamped onto AgentClass/AgentSession status.
func (e EffectiveSettings) ToStatus() v1.EffectiveSettings {
	out := v1.EffectiveSettings{
		Model:              e.Model,
		ModelTokenSource:   e.ModelTokenSource,
		ModelInputPerMTok:  e.ModelInputPerMTok,
		ModelOutputPerMTok: e.ModelOutputPerMTok,
		ModelRouting:       e.ModelRouting,
		Budget:             e.Budget,
		Authz: v1.EffectiveAuthz{
			ApprovalTimeout:               metav1.Duration{Duration: e.Authz.ApprovalTimeout},
			InformationLeakageApprovalTTL: metav1.Duration{Duration: e.Authz.InformationLeakageApprovalTTL},
			ScopeMaxLLMLatencyMs:          e.Authz.ScopeMaxLLMLatencyMs,
			PlanGate:                      e.Authz.PlanGate,
			Metaagent:                     e.Authz.Metaagent,
		},
		AllowedToolkits:           e.AllowedToolkits,
		AllowedMCP:                e.AllowedMCP,
		AllowedSkills:             e.AllowedSkills,
		DeniedSkills:              e.DeniedSkills,
		Pinning:                   e.Pinning,
		ToolGuard:                 e.ToolGuard,
		ContentInspectors:         e.ContentInspectors,
		Provenance:                e.Provenance,
		ReportSessionCost:         e.ReportSessionCost,
		NativeFileHandling:        e.NativeFileHandling,
		RequireSubagentDigestPins: e.RequireSubagentDigestPins,
		RequireStandingFor:        e.RequireStandingFor,
	}

	if len(e.Sandbox) > 0 {
		out.Sandbox = make(map[string]v1.SandboxBackend, len(e.Sandbox))
		for name, b := range e.Sandbox {
			out.Sandbox[name] = b
		}
	}
	out.AllowedSandboxKinds = e.AllowedSandboxKinds

	return out
}
