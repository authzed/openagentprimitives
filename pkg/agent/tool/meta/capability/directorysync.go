package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

func init() { Register(&directorySyncCapability{}) }

// directorySyncCapability is OPT-IN (default-off), and — unlike every other
// registered Capability — grants nothing to the AGENT it is checked on. It
// exists purely so pkg/channels/channelfeatures' capability→feature table
// (table.go) has a real, registered capability to mirror
// (channelfeatures_mirror_test.go's TestChannelFeaturesTableMirrorsTheRegistry),
// which is what makes the Slack wizard able to render
// channelfeatures.DirectorySync's channels:read/groups:read at all — see
// pkg/channels/channelkinds/slack/features.go.
//
// Checking it says "this Slack app's bot token is also meant to back a
// RelationshipSource" (pkg/platform/relsync, pkg/controllers/relationshipsource),
// a workspace-level directory sync — not a behavior of the agent this
// AgentClass describes. Offer returns no tools for exactly that reason: there
// is nothing for THIS agent to do differently once its app can enumerate the
// workspace, only a scope grant on the token underneath it.
type directorySyncCapability struct{}

func (directorySyncCapability) Name() string          { return "directory_sync" }
func (directorySyncCapability) DefaultOn() bool       { return false }
func (directorySyncCapability) Infrastructural() bool { return false }

func (directorySyncCapability) ParseConfig(raw json.RawMessage) (Config, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// Tolerate (and ignore) the common {enabled} shape every capability
	// accepts; reject anything else so a typo'd sub-config is not silently
	// dropped. There is no per-capability config to parse — see the type doc.
	var cfg struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return nil, nil
}

// Offer returns no tools — see the type doc on why this capability has
// nothing for the agent to do.
func (directorySyncCapability) Offer(OfferContext) ([]tool.Tool, *SkipReason) { return nil, nil }
