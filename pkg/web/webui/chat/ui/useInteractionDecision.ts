import { useCallback } from "react";
import { failureText, sessionPath } from "./sessionMessage";

// useInteractionDecision is the ONE decision route every InteractionCard
// posts through — lifted verbatim out of ChatView's own submitDecision/
// handleDecision (see its git history) so a second view (the agent-defined
// UI's "Needs your decision" region) submits through the exact same fetch
// rather than growing its own copy. Two copies of this call is the bug the
// plan that introduced this hook refuses to ship.
//
// It POSTs to .../decision, which publishes a KindInteractionDecision
// envelope (see pkg/web/webui/chat/handlers.go's decisionHandler). The
// resolved outcome arrives asynchronously via the session socket's
// interaction_applied frame (folded into SessionSignals.pendingInteractions),
// which re-renders the card in place — this call only needs to surface a
// failure, mirroring every other fetch handler in this package: a non-OK
// response or a network error calls `onError` with a user-facing line rather
// than leaving a caller stuck on a silently-disabled button
// (no-silent-errors, AGENTS.md). Both failure paths are also console.error'd
// with enough context (ns/name/status or the raw error) to grep for.
//
// The returned function is a stable callback (memoized on ns/name/onError)
// that never awaits: a card's onClick handler stays synchronous, exactly like
// ChatView's original handleDecision — the async work and its error handling
// live entirely inside this hook.
export function useInteractionDecision(
  ns: string,
  name: string,
  onError: (text: string) => void,
): (requestRef: string, category: string, actionId: string) => void {
  const submitDecision = useCallback(
    async (requestRef: string, category: string, actionId: string) => {
      try {
        const resp = await fetch(`${sessionPath(ns, name)}/decision`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ category, requestRef, actionId }),
        });
        if (!resp.ok) {
          console.error("interaction decision: request failed", { ns, name, status: resp.status });
          const reason = await failureText(resp, `Failed to submit decision (${resp.status}).`);
          onError(reason);
        }
      } catch (err) {
        console.error("interaction decision: request errored", { ns, name, err });
        onError("Network error while trying to submit decision.");
      }
    },
    [ns, name, onError],
  );

  return useCallback(
    (requestRef: string, category: string, actionId: string) => {
      void submitDecision(requestRef, category, actionId);
    },
    [submitDecision],
  );
}
