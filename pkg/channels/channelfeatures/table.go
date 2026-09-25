package channelfeatures

import (
	"fmt"
	"slices"

	"github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// entry binds one AgentClass capability to the channel features it needs.
//
// defaultOn MUST match the corresponding Capability.DefaultOn(). That is
// asserted by the mirror test in pkg/agent/tool/meta/capability, the only
// package that can see both sides.
type entry struct {
	capability string
	defaultOn  bool
	features   []Feature
}

// table is the capability→feature mapping. Capabilities with no channel-side
// requirement are deliberately ABSENT rather than listed with an empty feature
// set, which would only invite the mirror test to be loosened.
//
// agent_ui_handoff is the one to think twice about, since it does send a
// message: its offer rides MessageDelivery, which is in baseline, so it costs
// no permission beyond what every channel already needs to speak at all.
var table = []entry{
	{capability: "attachments", defaultOn: false, features: []Feature{AttachmentsOutbound, AttachmentsInbound}},
	{capability: "artifacts", defaultOn: false, features: []Feature{AttachmentsOutbound}},
	{capability: "thread_history", defaultOn: true, features: []Feature{ThreadHistory}},
	{capability: "channel_history", defaultOn: true, features: []Feature{ChannelHistory}},
	{capability: "mention_lookup", defaultOn: true, features: []Feature{UserLookup}},
	{capability: "credential_update", defaultOn: false, features: []Feature{CredentialPortal}},
	{capability: "session_views", defaultOn: false, features: []Feature{SessionViews}},
	{capability: "directory_sync", defaultOn: false, features: []Feature{DirectorySync}},
}

// baseline is what EVERY channel needs regardless of the bound agent — the
// ability to speak at all, and to show that it is working. A monitoring
// Channel has no AgentClass and gets exactly this.
var baseline = []Feature{MessageDelivery, StatusIndicator}

// Baseline returns the features required with no AgentClass in play.
func Baseline() []Feature { return append([]Feature(nil), baseline...) }

// Capabilities returns capability name → defaultOn for every entry in the
// table. Used by the mirror test to compare against the capability registry.
func Capabilities() map[string]bool {
	out := make(map[string]bool, len(table))
	for _, e := range table {
		out[e.capability] = e.defaultOn
	}
	return out
}

// ActiveFor resolves the features the class's active capabilities require,
// always including Baseline. A nil class yields Baseline alone (monitoring).
//
// Reads spec.capabilities, never status: status is eventually consistent, and
// a capability revocation must not leave a window in which a stale reader
// still grants.
//
// A malformed capability value fails CLOSED (the feature is omitted) AND
// yields a non-nil error callers must log rather than drop; the features
// resolved so far are still returned, so a caller that proceeds degrades
// rather than losing everything.
func ActiveFor(class *v1alpha1.AgentClass) ([]Feature, error) {
	seen := map[Feature]bool{}
	var out []Feature
	add := func(fs []Feature) {
		for _, f := range fs {
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	add(baseline)

	if class == nil {
		return out, nil
	}

	var firstErr error
	for _, e := range table {
		grant, err := agentcaps.GrantOf(class, e.capability)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("capability %q on AgentClass %s/%s has a malformed value: %w",
					e.capability, class.Namespace, class.Name, err)
			}
			continue // fail closed: the feature is not added
		}
		if agentcaps.Active(e.defaultOn, grant) {
			add(e.features)
		}
	}
	return out, firstErr
}

// FeaturesFor returns the features a capability implies, or nil if the
// capability has no channel-side requirement (or does not exist).
//
// This is the display direction: the setup wizard lists CAPABILITIES (what a
// user's answers get patched onto) and annotates each with the transport
// permissions its features cost. ActiveFor walks the same table to resolve.
func FeaturesFor(capability string) []Feature {
	for _, e := range table {
		if e.capability == capability {
			return slices.Clone(e.features)
		}
	}
	return nil
}
