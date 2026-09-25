import { useCallback, useEffect, useRef, useState } from "react";
import { isActionPending, type ActionPhase, type ActionState, type Declaration } from "@ap/agentui";
import { sendSessionMessage } from "../../chat/ui/sessionMessage";
import { usePresenceHeartbeat } from "./usePresenceHeartbeat";

// LiveActionEntry mirrors live.go's liveActionEntry field-for-field. Note
// there is no `requester` field: live.go strips it deliberately (a canonical
// SpiceDB subject is an internal identifier a browser never needs, and never
// gets), and this mirror does not invent one.
//
// The mirror is pinned by ONE SHARED ARTIFACT: testdata/live.golden.json
// carries the REAL bytes GET .../live writes, a Go test asserts the route's
// output against that file (live_golden_internal_test.go), and liveGolden.test.tsx
// re-reads every field below BY NAME from the same file. A literal in each
// language's own test would NOT be a pin — a rename on either side would leave
// both suites green while `approvalAddressedToViewer` silently became
// undefined and chrome stopped auto-revealing for an approval the viewer must
// act on.
export interface LiveActionEntry {
  requestId: string;
  action: string;
  state: string;
  message?: string;
  approvalAddressedToViewer?: boolean;
  updatedAt: string;
}

// LiveActionMessage mirrors live.go's liveActionMessage. All three of its
// frame types are named here, "error" included: live.go authors browser-safe
// copy on that frame (Memory unconfigured, a snapshot read failure) and this
// hook surfaces it as `liveError` rather than logging it and leaving the
// viewer looking at controls that silently never update.
export interface LiveActionMessage {
  type: "snapshot" | "event" | "error";
  actions?: LiveActionEntry[];
  action?: LiveActionEntry;
  message?: string;
}

// LiveViewMessage mirrors live.go's liveViewMessage — the "view" arm of the
// SAME socket. Declaration is always webd's own re-resolved read; nothing on
// this frame is a per-hook patch (see channelevents.UIViewUpdatePayload's own
// doc comment for why the push is a trigger, not a document, and
// liveViewMessage's for why the WHOLE merged page tree travels).
// DeclaredAction is the half of pkg/web/uicomponents.Action this hook reads: which
// action it is, and whether it asks the agent rather than calling a tool. The
// tool, args template and inputs allowlist are deliberately not read here —
// they are the server's, applied server-side on the actions route, and a
// browser that consulted them would be re-deciding something already decided.
export interface DeclaredAction {
  name: string;
  prompt?: string;
}

// fillPrompt mirrors pkg/web/uicomponents.FillPrompt — {key} holes, filled from
// the same values map the args templates draw on, with an unsupplied hole left
// VISIBLE rather than emptied.
//
// Two implementations of one grammar is a real cost, accepted because the
// alternative is a server round-trip to compose a sentence the viewer is about
// to be shown as their own message. They are kept honest by their shared
// tests: the Go cases in prompt_test.go and the TS cases alongside this file
// assert the same table.
function fillPrompt(prompt: string, values: Record<string, string>): string {
  return prompt.replace(/\{([^{}]*)\}/g, (whole, key: string) =>
    key !== "" && Object.hasOwn(values, key) ? values[key] : whole,
  );
}

export interface LiveViewMessage {
  type: "view";
  // hook is the region update_view wrote, absent on the open-time frame that
  // every connect receives. It is what the page's per-hook updated cue is
  // keyed on, and it is the only evidence of a CLEAR: emptying an
  // already-empty hook changes no bytes, so the declaration alone cannot say
  // the agent acted.
  hook?: string;
  declaration: Declaration;
  // agentParams is what the agent set this view's controls to, already
  // filtered server-side to the parameters the declaration on THIS frame
  // declares. Absent on the ordinary page, where the viewer drives every
  // control. Applied by AgentUIView only to controls its viewer has not
  // touched — the precedence lives there, since only it knows what was.
  agentParams?: Record<string, string>;
}

// LiveFrame is every shape this hook's one socket can receive. Kept as a
// union rather than folding `declaration` onto LiveActionMessage: the two
// arms are genuinely different messages sharing only a `type` discriminator
// and a transport, exactly as live.go's own liveActionMessage/liveViewMessage
// are two Go types, not one with optional fields.
export type LiveFrame = LiveActionMessage | LiveViewMessage;

// RECONNECT_MS is a flat retry interval, not exponential — the same choice
// pkg/web/webui/chat/ui/useChatSocket.ts and pkg/web/webui/artifactview/ui/
// useLiveSocket.ts already made for the identical "the server dropped us,
// keep trying" shape. A third, different retry policy here would be a second
// answer to a problem this codebase already has one for.
const RECONNECT_MS = 2000;

// INVOKE_FAILURE_MESSAGE is the copy a browser-side transport failure shows
// (a rejected fetch, or a non-2xx this handler cannot itself decode a real
// answer from). It never repeats the actual cause — that goes to
// console.error below — because the cause is a Go error string, a stack, or
// a bare status code, none of which a viewer can act on.
//
// It says "may not have completed", not "could not be sent": this same arm is
// reached for a non-2xx, which the server can only answer AFTER it has already
// dispatched the invocation, so the tool may well have run. Telling a viewer
// their write definitely did not happen, when it may have, is worse than
// telling them the truth is unknown.
const INVOKE_FAILURE_MESSAGE = "This action may not have completed. Check your connection, then check the result before retrying.";

// LIVE_ERROR_FALLBACK covers an "error" frame that carried no copy of its own.
// live.go always authors a sentence today; this exists so a future producer
// that forgets one still surfaces something a viewer can read, rather than an
// empty notice.
const LIVE_ERROR_FALLBACK = "Live updates for this view are unavailable right now.";

function wsURLFor(loc: { protocol: string; host: string }, ns: string, name: string): string {
  const proto = loc.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${loc.host}/agent-ui/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/live`;
}

function toActionState(entry: LiveActionEntry): ActionState {
  return {
    phase: entry.state as ActionPhase,
    requestId: entry.requestId,
    message: entry.message,
    approvalAddressedToViewer: entry.approvalAddressedToViewer,
  };
}

// isSettled reuses isActionPending's own PENDING_PHASES table — the ONE
// pending/not-pending split @ap/agentui ships — rather than a second
// terminal/non-terminal switch living in this file too. Reading "not
// pending" as "settled" is safe specifically because every phase this hook
// ever writes into `states` came from the server (uiaction.State) or is this
// hook's own "submitted"/"failed" — never "idle", the one phase where "not
// pending" and "terminal" disagree (see @ap/agentui's actions.tsx doc
// comment on isActionPending for why "idle" is carved out there).
function isSettled(phase: ActionPhase): boolean {
  return !isActionPending(phase);
}

// sameActionState compares the four fields an ActionState carries. Used only
// to suppress a no-op re-render when a reconnect re-delivers a snapshot the
// hook already holds; it is never a correctness gate.
function sameActionState(a: ActionState | undefined, b: ActionState): boolean {
  return (
    a !== undefined &&
    a.phase === b.phase &&
    a.requestId === b.requestId &&
    a.message === b.message &&
    Boolean(a.approvalAddressedToViewer) === Boolean(b.approvalAddressedToViewer)
  );
}

// useActionLifecycle owns the whole network side of one page's actions: the
// POST that fires an action (POST /agent-ui/{ns}/{name}/actions) and the
// websocket that carries its outcome back (GET .../live).
//
// It is named for what it owns, NOT `useActions` — that name belongs to
// @ap/agentui's context hook, which reads state a provider (this hook's
// caller) supplied. Two hooks with one name across two packages is how a
// call site ends up reaching for the wrong one with no type error.
//
// # Two producers, one map
//
// The same lifecycle fact reaches this hook from TWO sources: the synchronous
// POST answer, and the live socket (both a per-transition event and the full
// snapshot every connect begins with). Memory is authoritative and the socket
// is the low-latency copy of it, so the socket is ALWAYS at least as fresh as
// the POST answer that raced it. Two consequences this hook implements:
//
//   - A POST answer is applied only if no socket frame for that action landed
//     after the POST was issued (lifecycleSeqRef / socketSeqRef below). The
//     normal ordering for an approval-gated action makes this the common case,
//     not an exotic race: the runner records+publishes `awaiting_approval`
//     while webd is still relaying the synchronous `submitted` reply, and the
//     POST answer carries no approvalAddressedToViewer field at all — so
//     applying it unconditionally would un-tell the viewer that THEY are the
//     approver, and no further frame is due until the approver acts.
//   - A snapshot RECONCILES; it does not merely backfill. A settle that
//     happens while the socket is down is carried only by the next snapshot,
//     and this hook's `states` survives a reconnect (the socket effect is keyed
//     [ns, name] and scheduleReconnect reconnects inside the same mount), so a
//     snapshot that skipped already-known actions would leave that control
//     disabled forever.
//
// onSettled fires whenever any action reaches a phase isActionPending calls
// not-pending — from a socket event, from a snapshot that newly learns of a
// settle, or from a 2xx POST answer. AgentUIView uses it to bump a
// bindings-refresh generation, so a page whose declared bindings reflect the
// action's effect (a table the action just changed a row in) repaints
// without the viewer reloading — see UIActionUpdatePayload's own doc for why
// the action's result does not ride the push directly, making this
// re-evaluation (rather than the push payload itself) the mechanism. It
// deliberately does NOT fire for a transport failure: that would re-POST
// /bindings over the same dead connection and replace every resolved binding
// on the page with a transport-error card, turning one failed click into a
// blanked dashboard.
export function useActionLifecycle(
  ns: string,
  name: string,
  params: Record<string, string>,
  onSettled: () => void,
  // onView receives the frame's whole page tree, its agent parameter choices,
  // and the HOOK it named. The hook is passed through rather than derived from
  // the declaration because it cannot be derived: a clear leaves bytes a
  // previous state already had, so only the frame says which region changed.
  onView: (declaration: Declaration, agentParams?: Record<string, string>, hook?: string) => void,
  // actions is the CURRENT declaration's action table — the live one, since a
  // view update can add or remove an action mid-session. It is read only to
  // tell an ask-the-agent action from a tool-calling one; everything else
  // about an action stays server-side.
  actions: readonly DeclaredAction[] = [],
): {
  states: Record<string, ActionState>;
  invoke: (action: string, inputs?: Record<string, string>) => void;
  liveError: string | null;
} {
  const [states, setStates] = useState<Record<string, ActionState>>({});
  const [liveError, setLiveError] = useState<string | null>(null);

  const mountedRef = useRef(true);
  const paramsRef = useRef(params);
  paramsRef.current = params;
  const onSettledRef = useRef(onSettled);
  onSettledRef.current = onSettled;
  const onViewRef = useRef(onView);
  onViewRef.current = onView;
  // Held in a ref for the same reason paramsRef is: invoke's identity must
  // stay stable across renders (every control depends on it), while the table
  // it reads has to be the CURRENT one at click time, not the one captured
  // when the callback was created.
  const actionsRef = useRef(actions);
  actionsRef.current = actions;
  // statesRef mirrors the committed `states` so the socket handlers can decide
  // whether a frame teaches them something NEW (a settle they had not already
  // recorded) without doing that comparison inside a setStates updater, which
  // must stay a pure function of prev.
  const statesRef = useRef(states);
  statesRef.current = states;
  // inFlight is keyed by action NAME — the same key `states` uses — and
  // holds exactly the actions with a POST currently in flight. This is the
  // double-submit guard: checked and set synchronously at the top of
  // `invoke`, before any async work, so two clicks handled before the first
  // request has even returned cannot both pass it. The rendered `disabled`
  // attribute is not a substitute — it only engages on the next commit, and
  // two invocations dispatched within one event handler never reach a commit
  // in between.
  const inFlightRef = useRef<Set<string>>(new Set());
  // The CURRENT live socket, or null between reconnects. Held in a ref rather
  // than state because writing it must not re-render: it changes on every
  // reconnect, and nothing renders differently for it.
  const liveSocketRef = useRef<WebSocket | null>(null);
  // lifecycleSeqRef is a monotonic tick shared by both producers; socketSeqRef
  // records, per action, the tick at which the socket last wrote that action.
  // A POST answer compares its own issue tick against it — see the "Two
  // producers, one map" note above.
  const lifecycleSeqRef = useRef(0);
  const socketSeqRef = useRef<Record<string, number>>({});

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  // The live socket. Opens once per (ns, name) — a params change or a click
  // must not tear it down, which is why both are read through refs above
  // instead of appearing in this effect's own dependency array.
  useEffect(() => {
    let ws: WebSocket | null = null;
    let timer: number | undefined;
    let closed = false;
    let attempt = 0;

    // markSocketApplied stamps the shared tick onto an action the socket just
    // wrote, so a POST answer issued BEFORE this frame can recognize itself as
    // superseded.
    const markSocketApplied = (action: string) => {
      lifecycleSeqRef.current += 1;
      socketSeqRef.current[action] = lifecycleSeqRef.current;
    };

    // learnsOfASettle reports whether `entry` tells this hook something new:
    // the action is settled and the state currently held for it is not. Used
    // to decide whether onSettled must fire, without firing it again for a
    // settle already applied (a snapshot repeats every record on every
    // connect, and each reconnect would otherwise re-POST /bindings).
    const learnsOfASettle = (entry: LiveActionEntry): boolean => {
      if (!isSettled(entry.state as ActionPhase)) return false;
      const held = statesRef.current[entry.action];
      return held === undefined || !isSettled(held.phase);
    };

    const applySnapshot = (entries: LiveActionEntry[]) => {
      // uiaction.List — and so this frame's Actions slice — is newest first,
      // so the FIRST entry for a given action name IS the newest. Dedup is
      // frame-LOCAL: an older entry later in this array must not win, but an
      // entry must still replace whatever `states` already holds, because a
      // snapshot is memory's authoritative answer and this map survives the
      // reconnect that requested it.
      const seen = new Set<string>();
      const next: Record<string, ActionState> = {};
      let settledSomething = false;
      let changedSomething = false;
      for (const entry of entries) {
        if (seen.has(entry.action)) continue;
        seen.add(entry.action);
        // A POST for this action is in flight: its answer is about to arrive
        // and is newer than any record that existed when this snapshot was
        // read. Leaving the optimistic "submitted" in place is what keeps a
        // reconnect landing mid-click from re-enabling a control whose write
        // has not answered yet.
        if (inFlightRef.current.has(entry.action)) continue;
        if (learnsOfASettle(entry)) settledSomething = true;
        const state = toActionState(entry);
        if (!sameActionState(statesRef.current[entry.action], state)) changedSomething = true;
        next[entry.action] = state;
        markSocketApplied(entry.action);
      }
      // Every connect re-sends the full snapshot, and a flapping server can
      // reconnect on a 2s cadence, so a snapshot that teaches this hook
      // nothing must not allocate a new states object and re-render the page.
      if (!changedSomething) return;
      setStates((prev) => ({ ...prev, ...next }));
      if (settledSomething) onSettledRef.current();
    };

    const applyEvent = (entry: LiveActionEntry) => {
      const settled = learnsOfASettle(entry);
      markSocketApplied(entry.action);
      setStates((prev) => ({ ...prev, [entry.action]: toActionState(entry) }));
      if (settled) onSettledRef.current();
    };

    const handleFrame = (raw: string) => {
      let msg: LiveFrame;
      try {
        msg = JSON.parse(raw) as LiveFrame;
      } catch (err) {
        console.error("agent-ui live: malformed frame", { ns, name, err });
        return;
      }
      if (!mountedRef.current) return;
      if (msg.type === "snapshot") {
        // A snapshot IS the proof that live updates work again, so it — not a
        // bare socket open — is what clears a previous "error" frame's notice.
        // Clearing on open instead would make the notice flicker on and off
        // every RECONNECT_MS against a server that keeps answering with an
        // error frame and closing.
        setLiveError(null);
        applySnapshot(msg.actions ?? []);
        return;
      }
      if (msg.type === "event" && msg.action) {
        applyEvent(msg.action);
        return;
      }
      if (msg.type === "error") {
        // live.go authored browser-safe copy for exactly this frame (a nil
        // Memory, a failed snapshot read). Surfacing it is the
        // no-silent-errors rule's "tell the person who is waiting" arm: the
        // alternative is controls that never update with nothing on screen
        // saying why. The console still gets the frame for the operator.
        console.error("agent-ui live: server reported live updates unavailable", { ns, name, msg });
        setLiveError(msg.message || LIVE_ERROR_FALLBACK);
        return;
      }
      if (msg.type === "view") {
        // The frame is server-built (live.go always marshals a `view` tree,
        // and uicomponents rejects a page without one), so this shape check is
        // defence in depth, not the primary guard — but a thrown TypeError from
        // a missing `view` deep inside collectDefaultParams/reconcileParams or
        // applyBindings would surface during the NEXT render, not here, and an
        // uncaught render error unmounts the whole tree with no error boundary
        // above it. Skipping a malformed frame (logged) leaves the viewer on a
        // stale-but-visible page, which is the far smaller failure.
        //
        // `component` is checked too, not just the object-ness: every consumer
        // downstream walks the tree from its root component name, and a `view`
        // that is an object without one fails exactly as far in.
        if (
          !msg.declaration ||
          typeof msg.declaration.view !== "object" ||
          msg.declaration.view === null ||
          typeof msg.declaration.view.component !== "string"
        ) {
          console.error("agent-ui live: malformed view frame; keeping the previous declaration", { ns, name, msg });
          return;
        }
        // agentParams and the written hook ride the SAME frame as the page tree
        // they belong with, so a viewer never sees a new page against the old
        // controls, and the cue never names a region from a different frame.
        onViewRef.current(msg.declaration, msg.agentParams, msg.hook);
        return;
      }
      // Any other shape this union does not name. Never silently dropped: the
      // POST path still works with no live updates, exactly as a viewer
      // sees it, but the operator-diagnosable cause goes to the console.
      console.error("agent-ui live: unhandled frame", { ns, name, msg });
    };

    const scheduleReconnect = () => {
      if (closed) return;
      attempt += 1;
      // Logged on EVERY attempt, not just the first: a socket that keeps
      // failing silently is a page whose buttons never resolve, with
      // nothing in the console pointing at why.
      console.error("agent-ui live: socket closed; reconnecting", { ns, name, attempt });
      timer = window.setTimeout(connect, RECONNECT_MS);
    };

    const connect = () => {
      if (closed) return;
      try {
        ws = new WebSocket(wsURLFor(window.location, ns, name));
      } catch (err) {
        console.error("agent-ui live: failed to open socket", { ns, name, err });
        scheduleReconnect();
        return;
      }
      ws.onopen = () => {
        attempt = 0;
      };
      // Exposed so the presence heartbeat can write to whichever socket is
      // CURRENT. Reconnects replace `ws`, and a heartbeat holding the old one
      // would silently stop reporting the moment the socket flapped — the
      // exact conditions (a slow network, a laptop waking) under which a
      // viewer most needs their runner kept alive.
      liveSocketRef.current = ws;
      ws.onmessage = (ev) => handleFrame(ev.data as string);
      ws.onerror = () => {
        // onclose follows onerror on a real drop; onclose owns the retry so
        // this does not double-schedule (mirrors useChatSocket.ts).
      };
      ws.onclose = scheduleReconnect;
    };
    connect();

    return () => {
      closed = true;
      if (timer) window.clearTimeout(timer);
      if (ws) ws.close();
      liveSocketRef.current = null;
    };
  }, [ns, name]);

  // The presence heartbeat rides this socket because it is already the one
  // thing that exists per open agent-UI, already authorized (the route runs
  // its own agentsession#interact check before the upgrade), and already
  // reconnecting on its own. A second channel would have to re-earn all three.
  usePresenceHeartbeat({
    enabled: true,
    send: useCallback(() => {
      const sock = liveSocketRef.current;
      if (!sock || sock.readyState !== WebSocket.OPEN) return;
      try {
        sock.send(JSON.stringify({ type: "presence" }));
      } catch (err) {
        // Logged, never thrown: a dropped heartbeat degrades to the runner's
        // base idle TTL, which is exactly the behaviour that existed before
        // this signal. Failing the view over one would be the larger harm.
        console.error("agent-ui live: presence heartbeat send failed", { ns, name, err });
      }
    }, [ns, name]),
  });

  const invoke = useCallback(
    (action: string, inputs?: Record<string, string>) => {
      // Closes the double-submit window: see inFlightRef's own doc comment
      // above.
      if (inFlightRef.current.has(action)) return;
      inFlightRef.current.add(action);
      lifecycleSeqRef.current += 1;
      const issuedAt = lifecycleSeqRef.current;
      setStates((prev) => ({ ...prev, [action]: { phase: "submitted" } }));

      // An action that declares a PROMPT asks the agent instead of calling a
      // tool, and asking the agent something is sending it a message. So it
      // goes down the transcript's own route rather than the actions route:
      // same authorization, same attribution, and the request lands in the
      // transcript where the viewer can see what was asked and read the reply.
      //
      // Routing on the DECLARATION, never on anything the control supplies —
      // the action table is the server's document, and the browser is reading
      // it, not deciding it.
      const declared = actionsRef.current.find((a) => a.name === action);
      if (declared?.prompt) {
        sendSessionMessage(ns, name, fillPrompt(declared.prompt, { ...paramsRef.current, ...(inputs ?? {}) }))
          .then(() => {
            if (!mountedRef.current) return;
            // Terminal on delivery, not on an answer. The agent's reply is a
            // turn in the transcript and, if it composes one, a view update —
            // both of which arrive on their own channels. Holding this control
            // "pending" until then would leave it stuck for any question the
            // agent answers in prose alone.
            setStates((prev) => ({ ...prev, [action]: { phase: "succeeded", message: "Asked the agent." } }));
            onSettledRef.current?.();
          })
          .catch((err: unknown) => {
            console.error("agent-ui actions: asking the agent failed", { ns, name, action, err });
            if (!mountedRef.current) return;
            setStates((prev) => ({ ...prev, [action]: { phase: "failed", message: "Could not reach the agent." } }));
          })
          .finally(() => { inFlightRef.current.delete(action); });
        return;
      }

      fetch(`/agent-ui/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/actions`, {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ action, params: paramsRef.current, inputs }),
      })
        .then(async (res) => {
          if (!res.ok) throw new Error(`agent-ui actions: request failed (${res.status})`);
          return (await res.json()) as { requestId: string; state: string; message?: string };
        })
        .then((data) => {
          if (!mountedRef.current) return;
          // A socket frame for this action landed after this POST was issued.
          // Memory is authoritative and the socket carries it, so that frame
          // is newer than this answer — and this answer's body has no
          // approvalAddressedToViewer field to carry forward. Dropping it is
          // the only correct move; the socket has already recorded the state
          // and already fired onSettled if it was terminal.
          if ((socketSeqRef.current[action] ?? 0) > issuedAt) return;
          const phase = data.state as ActionPhase;
          setStates((prev) => ({
            ...prev,
            [action]: {
              phase,
              requestId: data.requestId,
              message: data.message,
              // Never CLEARED by an answer that has no opinion on it: the
              // response envelope carries no such field, so anything already
              // known about who must approve survives this write.
              approvalAddressedToViewer: prev[action]?.approvalAddressedToViewer,
            },
          }));
          if (isSettled(phase)) onSettledRef.current();
        })
        .catch((err: unknown) => {
          // Never silently drop: the real cause (a network failure, a
          // non-2xx this call could not even decode a body for) is logged
          // here; the control gets only the generic copy, same posture as
          // useBindings.ts's own transport-failure branch.
          console.error("agent-ui actions: invoke failed", { ns, name, action, err });
          if (!mountedRef.current) return;
          setStates((prev) => ({
            ...prev,
            [action]: { phase: "failed", message: INVOKE_FAILURE_MESSAGE, approvalAddressedToViewer: prev[action]?.approvalAddressedToViewer },
          }));
          // Deliberately no onSettled here — see the hook's doc comment. A
          // transport failure re-POSTing /bindings would fail for the same
          // reason and blank every binding on the page.
        })
        .finally(() => {
          // Once THIS request has settled — success or failure — a fresh
          // click is a new, legitimate invocation, not a duplicate of one
          // still in flight.
          inFlightRef.current.delete(action);
        });
    },
    [ns, name],
  );

  return { states, invoke, liveError };
}
