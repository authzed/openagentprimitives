package runner

import (
	"github.com/authzed/openagentprimitives/pkg/agent/modality"
	modregistry "github.com/authzed/openagentprimitives/pkg/agent/modality/registry"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// ModalityInstructions returns the non-empty prompt guidance for each
// registered modality that has at least one of its own contributed meta
// tools present in mergedTools, in registry order. Pass the result to
// ComposeSystem's modalityInstructions parameter.
//
// Gating on "is this modality's tool actually in mergedTools" — rather than
// a specific capability's grant, e.g. the artifacts capability having
// injected artifact_prepare — is deliberate: a narrower capability grant can
// inject a modality's tool on its own (the attachments capability injects
// fetch_artifact, the files modality's Tier-1 tool, without the broader
// artifacts capability ever being granted — see
// pkg/agent/tool/meta/capability/attachments.go). An agent whose class grants
// only attachments would otherwise get fetch_artifact in its tool table with
// no prompt guidance on how to use it.
func ModalityInstructions(env modality.Env, mergedTools []tool.Tool) []string {
	var out []string
	for _, m := range modregistry.All() {
		if !modalityToolPresent(m, env, mergedTools) {
			continue
		}
		if s := m.Instructions(env); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// modalityToolPresent reports whether any of m's own contributed meta tools
// (as it would build them for env) already appears by name in mergedTools.
func modalityToolPresent(m modality.Modality, env modality.Env, mergedTools []tool.Tool) bool {
	for _, mt := range m.MetaTools(env) {
		for _, t := range mergedTools {
			if t.Name() == mt.Name() {
				return true
			}
		}
	}
	return false
}
