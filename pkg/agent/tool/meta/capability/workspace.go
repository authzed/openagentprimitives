package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds"
	wsregistry "github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/registry"
)

func init() { Register(&workspaceCapability{}) }

// workspaceCapability is OPT-IN (default-off). When granted AND the session
// has a bound + overlaid workspace source (RunnerEnv.WorkspaceSource
// non-nil), it injects sync_workspace. apply_workspace — the write-back half
// — is injected ONLY when the {"apply":true} config additionally opts in AND
// the bound source's driver implements workspacekinds.Applier; a driver with
// no write-back support (or a grant that never asked for apply) never sees
// the tool, so there is nothing for the runner's authz hook to gate in that
// case.
type workspaceCapability struct{}

// workspaceConfig is the parsed per-capability config. Apply is ABSENT ⇒
// false (mirrors agentcaps.SessionViewsConfig's inversion): a bare
// `workspace: {}` grant is read-only sync, never silently gaining write-back.
type workspaceConfig struct {
	Apply bool `json:"apply"`
}

func (workspaceCapability) Name() string          { return "workspace" }
func (workspaceCapability) DefaultOn() bool       { return false }
func (workspaceCapability) Infrastructural() bool { return false }

func (workspaceCapability) ParseConfig(raw json.RawMessage) (Config, error) {
	if len(raw) == 0 {
		return workspaceConfig{}, nil
	}
	var cfg workspaceConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (workspaceCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	rt := o.Env.WorkspaceSource
	if rt == nil {
		return nil, &SkipReason{Capability: "workspace", Reason: "no workspace source bound or overlay not cut"}
	}
	tools := []tool.Tool{meta.NewSyncWorkspace(rt.Kind, rt.Locator, rt.Revision, rt.WorkDir, rt.OverlayPVC, rt.ReconcileImage, rt.ReconcileServiceAccount)}

	if cfg, ok := o.Config.(workspaceConfig); ok && cfg.Apply {
		if drv, ok := wsregistry.Get(rt.Kind); ok {
			if _, isApplier := drv.(workspacekinds.Applier); isApplier {
				tools = append(tools, meta.NewApplyWorkspace(rt.Kind, rt.Locator, rt.Revision, rt.WorkDir, rt.OverlayPVC, rt.CredSecret, rt.ReconcileImage, rt.ReconcileServiceAccount))
			}
		}
	}
	return tools, nil
}
