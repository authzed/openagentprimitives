package toolguard

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// credentialHalt ends the session after a streak of unbilled failures — calls
// the provider turned away without metering anything, which is what a
// credential it refuses looks like from here.
//
// It is a HALT rather than the breaker's deny because the breaker's answer is
// a cool-off, and a cool-off is an answer to a transient outage. Told "this
// tool is temporarily unavailable, try again in ~30s", a model reasonably keeps
// working: it retries later, tries another tool, re-plans. Against a key the
// provider will never accept, every one of those is another turn spent, and
// the run ends at maxTurns or maxDuration with the operator none the wiser.
// Stopping loudly is the only outcome that is both honest and cheap.
func (h *GuardRecord) credentialHalt(ctx context.Context, in pipeline.Input, origin string, rule ResolvedRule) pipeline.Decision {
	key := OriginKey(origin)
	h.d.Logger.Info("toolguard: credential halt",
		"session", in.Session.String(), "tool", in.Tool.Name, "origin", origin,
		"key", key, "threshold", rule.AuthHaltThreshold, "rule", rule.Provenance)
	if h.d.RecordAudit != nil {
		h.d.RecordAudit(ctx, Event{
			Event: "credential_halt", Tool: in.Tool.Name, Origin: origin, Key: key,
			UseID: in.Tool.UseID, Action: ActionHalt.String(), Provenance: rule.Provenance,
		})
	}
	// The "tool_guard:" prefix routes runnerHost.Halt to
	// ReasonAgentSessionToolGuardHalt, so the terminal AgentSession status says
	// a policy stopped this run rather than that a hook crashed.
	return pipeline.Decision{
		Verdict: pipeline.Halt,
		Reason:  "tool_guard: " + credentialHaltReason(in.Tool.Name, rule.AuthHaltThreshold, origin),
		Notices: []pipeline.Notice{{
			Notice: notice.New(categories.SessionHalted, notice.Args{
				Lead: "This session was stopped",
				Body: credentialHaltNoticeBody(in.Tool.Name, rule.AuthHaltThreshold),
				// Terminal: this run is over, so the next step is a fresh
				// attempt once the sign-in is fixed, never a retry of this one.
				NextStep: "Update this agent's sign-in for that tool, then start a new session.",
				Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester},
			}),
			ToRequester: true,
		}},
	}
}

// credentialHaltReason is the operator-facing text: it lands on the terminal
// AgentSession status, so it names the origin and the threshold an operator
// reading `status.conditions` needs to locate what fired and which credential
// to go look at.
func credentialHaltReason(toolName string, threshold int32, origin string) string {
	return fmt.Sprintf(
		"tool %q: %d consecutive calls to origin %s failed with nothing billed by the provider behind it, which is the shape of a rejected credential rather than of work that ran and failed. The session was stopped instead of spending its remaining turns on a credential that cannot succeed.",
		toolName, threshold, origin)
}

// credentialHaltNoticeBody is the copy a PERSON reads in the channel. It names
// the tool — which they have already watched the agent call — and nothing
// else: no breaker key, no threshold vocabulary, no resource kind. What the
// reader needs is which sign-in to go fix and why waiting will not help.
func credentialHaltNoticeBody(toolName string, threshold int32) string {
	return fmt.Sprintf(
		"%d calls to %q in a row were turned away before the service behind it did any work — the sign that its sign-in is no longer being accepted. Waiting would not have helped, so the session stopped rather than spend the rest of its time on calls that cannot succeed.",
		threshold, toolName)
}
