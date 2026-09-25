package capability

import (
	modregistry "github.com/authzed/openagentprimitives/pkg/agent/modality/registry"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// modalityMetaTools returns every registered modality's contributed meta tools
// for env. Shared by artifactsCapability.Offer, which also injects the four
// production artifact_* tools, and attachmentsCapability.Offer, which injects
// ONLY these — see that capability's doc for why reading an ingested attachment
// must not require the broader artifacts grant. Both being active at once is
// safe: Assemble dedups by name, and both build from the same RunnerEnv.
func modalityMetaTools(env RunnerEnv) []tool.Tool {
	menv := env.ModalityEnv()
	var tools []tool.Tool
	for _, m := range modregistry.All() {
		tools = append(tools, m.MetaTools(menv)...)
	}
	return tools
}
