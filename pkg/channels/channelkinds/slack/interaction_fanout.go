// pkg/channels/channelkinds/slack/interaction_fanout.go
//
// capApproverFanout enforces the operator's --approver-fanout-limit on the
// interaction delivery path. Delivery-only — never an authorization bound:
// click-time DecideResourceOwners admits any eligible owner, delivered or not.
package slack

import (
	"sort"

	"github.com/go-logr/logr"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// capApproverFanout truncates approvers to limit (or
// channelkinds.DefaultApproverFanoutLimit when limit is unset/invalid),
// sorting by fanoutKey first so the truncation is deterministic rather than
// dependent on upstream ordering. Truncation is logged — never silent — so the
// "who got dropped" signal survives.
func capApproverFanout(logger logr.Logger, approvers []channelevents.ExternalIdentity, limit int, sessRef string) []channelevents.ExternalIdentity {
	if limit <= 0 {
		limit = channelkinds.DefaultApproverFanoutLimit
	}
	if len(approvers) <= limit {
		return approvers
	}
	sorted := make([]channelevents.ExternalIdentity, len(approvers))
	copy(sorted, approvers)
	sort.Slice(sorted, func(i, j int) bool { return fanoutKey(sorted[i]) < fanoutKey(sorted[j]) })
	logger.Info("approver fan-out truncated by limit; undelivered approvers can still approve",
		"eligible", len(approvers), "limit", limit, "session", sessRef)
	return sorted[:limit]
}

// fanoutKey is the deterministic sort key for one approver: Subject when
// set (a pre-formed SpiceDB subject, e.g. "user:<id>"), else the raw
// channel-native ExternalID. It is a sort key only — not a canonical
// identity comparison — so mixing Subject-set and ExternalID-only entries
// in one list is fine.
func fanoutKey(e channelevents.ExternalIdentity) string {
	if e.Subject != "" {
		return e.Subject.String()
	}
	return e.ExternalID.String()
}
