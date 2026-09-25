// actions.tsx is the seam every live action control (ap:button, ap:form —
// see registry.tsx's ActionButton/ActionForm) reaches the page's action
// lifecycle through, without registry.tsx or renderNode.tsx knowing anything
// about HOW an action is invoked, POSTed, or watched for a later update. The
// page (Task 11's page-level hook) owns the states map and the invoke
// function and supplies both via ActionsProvider; a control just reads/calls
// through useActions. This is deliberately the same shape as params.tsx's
// BindingParamsProvider/useBindingParams seam.
//
// This module never fetches anything itself and never names a tool, ref, or
// args template — a control only ever sends an action NAME plus a values map
// (ActionsContextValue.invoke), matching pkg/web/webui/agentui/actions.go's
// actionRequestBody, which is the ONLY thing a browser may POST. The tool and
// the args template live in the server-side Declaration.Actions table; the
// browser cannot name its own tool through this seam.
import * as React from "react";

// ActionPhase mirrors pkg/memory/kinds/uiaction's State, plus "idle" — which
// exists ONLY here, because a control nobody has clicked has no server-side
// fact to record. The Go side is the authority; this union is the mirror.
export type ActionPhase =
  | "idle"
  | "submitted"
  | "awaiting_approval"
  | "running"
  | "succeeded"
  | "failed"
  | "denied"
  | "expired"
  | "rate_limited";

export interface ActionState {
  phase: ActionPhase;
  requestId?: string;
  message?: string;
  approvalAddressedToViewer?: boolean;
}

// PENDING_PHASES is the ONE place "must the control stay disabled" is
// decided. A Set literal rather than `!isTerminal(phase)` on purpose: the two
// differ for "idle" (not pending, not terminal — it is the third state a
// naive negation would miscount) and, more importantly, for "expired" (settled
// AND re-enabling — pkg/memory/kinds/uiaction.IsTerminal calls it terminal,
// but "terminal" there means "stop watching for updates", not "keep the
// control disabled forever"). Conflating the two vocabularies is exactly the
// bug Task 4's mutation-failure message warns about: "without this,
// uiaction.StateFor can never return expired and the control never
// re-enables."
const PENDING_PHASES: ReadonlySet<ActionPhase> = new Set([
  "submitted",
  "awaiting_approval",
  "running",
]);

// isActionPending reports whether a control must stay disabled. Written as an
// explicit list of the THREE in-flight phases rather than as "not terminal",
// because the two differ for "idle": an idle control is not pending and must
// stay clickable, while a naive !isTerminal("idle") would disable every
// button on the page forever.
export function isActionPending(phase: ActionPhase): boolean {
  return PENDING_PHASES.has(phase);
}

// actionCaption is the status line a control renders alongside itself. It
// returns null for "idle" (nothing has happened) and "succeeded" (a settled
// happy control shows no clutter) — everything else gets a human sentence.
// A server-supplied message (denial reason, rate-limit hint) always wins over
// the canonical copy below, mirroring pkg/memory/kinds/uiaction.DisplayCopy's
// own phase-to-copy table but never the requestId or any other internal
// identifier: this function's return value is the ONLY thing this module
// puts in front of a viewer, and it is built from nothing but phase,
// approvalAddressedToViewer, and message.
export function actionCaption(state: ActionState): string | null {
  if (state.phase === "idle") return null;
  // A succeeded action is normally silent: a tool call's RESULT is its own
  // feedback, and a caption saying "done" beside visibly-changed data is
  // noise.
  //
  // Unless it carried an explicit message. An action that asks the agent has
  // no visible result of its own — the reply arrives as a turn in the
  // transcript, or in a slot the agent composes, seconds later and somewhere
  // else — so silence here is a button that appears to do nothing when
  // pressed. That was the complaint the ask actions exist to answer; leaving
  // the acknowledgement out would reproduce it one layer down.
  if (state.phase === "succeeded") return state.message ?? null;
  if (state.message) return state.message;
  switch (state.phase) {
    case "submitted":
      return "Request submitted.";
    case "awaiting_approval":
      return state.approvalAddressedToViewer
        ? "Your approval is needed to continue."
        : "Waiting on approval from someone else.";
    case "running":
      return "In progress.";
    case "failed":
      return "The request failed. You can try again.";
    case "denied":
      return "The request was denied.";
    case "expired":
      return "The approval window expired. You can try again.";
    case "rate_limited":
      return "Too many requests right now. Try again shortly.";
    default:
      return null;
  }
}

export interface ActionsContextValue {
  states: Record<string, ActionState>; // keyed by DECLARED action name
  invoke: (action: string, inputs?: Record<string, string>) => void;
  // answer sends free text to the session's transcript as the viewer — the
  // route a Prompt action's filled sentence already travels. Resolves when
  // channelsd accepted it; rejects on a transport failure.
  answer: (text: string) => Promise<void>;
  // busy is the PAGE-level fact "the agent is working": the view sets it
  // from the session's turn_activity frames. A control's own lifecycle
  // settles a Prompt action the moment the message is delivered — it cannot
  // know when the agent will answer — so without this a form re-enables
  // while the agent is still working on what it just sent, and a second
  // submit lands as a second message mid-turn. Every live control (button,
  // form, question) reads it as one more reason to be disabled. Optional so
  // a standalone render (no provider, or a test that says nothing about the
  // turn) keeps its controls live.
  busy?: boolean;
}

// The default context value — {} states, a no-op invoke — is what
// `@ap/agentui` renders when used standalone with no provider (Plan 1's
// fixture.test.tsx and renderNode.test.tsx render with no provider). A
// control reads this as "no action has been fired yet", not as an error: it
// renders live and interactive when it declares an action, and its click
// calls an invoke that quietly does nothing, rather than the control failing
// to render at all — the same choice params.tsx's defaultBindingParams
// already makes.
const defaultActionsValue: ActionsContextValue = {
  states: {},
  invoke: () => {},
  // Unlike invoke's quiet no-op, a card asking a question has nowhere silent
  // to put a viewer's typed answer: a resolved-and-discarded promise would
  // read as "sent" while doing nothing. So the standalone default fails LOUD
  // instead — a rejection a rendered-with-no-provider card surfaces as its
  // own inline error, rather than a false "sent" state.
  answer: () => Promise.reject(new Error("agentui: no ActionsProvider")),
};

const ActionsContext =
  React.createContext<ActionsContextValue>(defaultActionsValue);

export function ActionsProvider({
  value,
  children,
}: {
  value: ActionsContextValue;
  children: React.ReactNode;
}): React.ReactElement {
  return (
    <ActionsContext.Provider value={value}>{children}</ActionsContext.Provider>
  );
}

export function useActions(): ActionsContextValue {
  return React.useContext(ActionsContext);
}
