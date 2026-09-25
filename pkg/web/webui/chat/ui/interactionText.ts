import type { InteractionDecisionRejectedInner } from "./types";

// interactionRejectionText renders the one-line reason shown to this clicker
// when their decision on an interaction card was refused: the pipe never
// swallows a rejected click (no-silent-errors), so every surface that renders
// an InteractionCard must show SOMETHING even though the shared card itself is
// left intact (a rejected click never resolves it). Mirrors Slack's
// interactionRejectionText (pkg/channels/channelkinds/slack/interaction.go) minus the
// Slack-only <@mention> syntax: already_resolved names the original decider
// (email, falling back to their raw external id) and the outcome they
// applied; every other class renders the pipe-supplied reason.
//
// Shared by ChatView (a timeline note) and the agent-defined UI view's
// "Needs your decision" region (its own decisionError line) — both post
// through the same decision route (useInteractionDecision) and so must render
// the same rejection the same way, rather than each growing its own copy.
export function interactionRejectionText(p: InteractionDecisionRejectedInner): string {
  if (p.class === "already_resolved") {
    let who = "someone else";
    if (p.originalDecider) {
      who = p.originalDecider.email || p.originalDecider.externalId || who;
    }
    let msg = `This request was already resolved by ${who}`;
    if (p.originalOutcome) msg += ` (${p.originalOutcome})`;
    return msg + ".";
  }
  const reason = (p.reason || "").trim() || "your click could not be applied";
  return reason + ".";
}
