package runner

import (
	"context"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// maxNamedApprovers is the largest population a card renders BY NAME.
//
// Past it the card states a count instead. A long list of names is worse than
// useless on an approval card — it pushes the decision off the screen — and the
// enumeration itself costs a lookup. Eight is the point where a Slack card
// stops reading as a sentence.
const maxNamedApprovers = 8

// planGateApprovers renders WHO can approve this session's plan, for the card.
//
// This closes a seam that was built at both ends and joined at neither:
// PlanGateDeps.Approvers is read in three card-building sites and was set by no
// binary, while channelkinds.DescribeApprovers sat complete, tested and
// uncalled. Every plan-gate card therefore shipped with a blank approver line —
// and a card that does not say who can approve it is one the requester cannot
// chase.
//
// It degrades rather than fails, through the ladder DescribeApprovers owns:
// named approvers when few, a labelled population when the channel kind can
// name one, a bare count when it cannot, and the honest floor otherwise. The
// floor — "anyone with approve on this session" — is correct on its own terms,
// which is what makes every failure path here safe to swallow into it.
func (l *Loop) planGateApprovers(ctx context.Context) string {
	approveSet := "agentsession:" + l.SessionKey.Namespace + "/" + l.SessionKey.Name + "#approve"

	var names []string
	if l.SpiceDBLookupSubjects != nil {
		subs, err := l.SpiceDBLookupSubjects(ctx,
			"agentsession:"+l.SessionKey.Namespace+"/"+l.SessionKey.Name, "approve")
		if err != nil {
			// Logged, then floored. A lookup failure must not blank the line:
			// the floor still tells the requester what standing is needed, which
			// is strictly more than nothing.
			slog.Default().Info("plan_gate: could not resolve the approver population; "+
				"the card will state the permission rather than the people",
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
				"approveSet", approveSet, "err", err.Error())
		} else {
			// Presented, not raw. SpiceDBLookupSubjects returns the CANONICAL
			// subject, which for a user is base64 — a live card read "Who can
			// approve: YWRtaW5AYXAubG9jYWw". This line exists so a blocked
			// requester knows who to go ask, and an opaque id defeats exactly
			// that.
			//
			// Display only: the canonical form stays the identity everywhere it
			// is compared or written. DecodeForDisplay returns its input
			// unchanged when the subject is not a canonical id, so a
			// non-user subject renders as it always did.
			names = make([]string, 0, len(subs))
			for _, s := range subs {
				names = append(names, identity.DecodeForDisplay(s))
			}
		}
	}

	return channelkinds.DescribeApprovers(channelkinds.ApproverPopulation{
		// Kind is the channel kind, type-asserted for SubjectSetDescriber. Slack
		// does not implement it yet — it requests neither channels:read nor
		// groups:read — so a subject-set ref renders as a count today and as
		// "#eng (23 people)" once it does. Passing it now means that upgrade is
		// a change in one package rather than a change here.
		Kind:              l.ChannelKindImpl,
		Names:             names,
		Size:              len(names),
		MaxNamedApprovers: maxNamedApprovers,
	})
}
