import { useEffect, useRef, useState } from "react";
import type { BindingState } from "@ap/agentui";

// BindingsResponse mirrors bindings.go's bindingsResponseBody. `value` is
// deliberately `unknown` — this hook never interprets it, only hands it to
// @ap/agentui's applyBindings, which lays it onto whatever prop the
// declaration bound it to.
export interface BindingsResponse {
  // Mirrors pkg/web/webui/agentui bindingResult.Status. Widened past ok/error when
  // the server gained needs_input — waiting on a viewer choice, which is not a
  // failure. Typed as a closed union so a new server status fails the switch
  // below at compile time rather than silently landing in the error arm.
  bindings: Record<string, { status: "ok" | "error" | "needs_input" | "waking"; value?: unknown; message?: string }>;
}

// TRANSPORT_ERROR_KEY is a reserved entry this hook writes into its returned
// map for a failure that never produced a per-binding response at all (a
// rejected fetch, or a non-2xx status) — as opposed to a real "error" entry
// the SERVER decided about one specific binding (bindings.go's per-binding
// resolveOneBinding). It can never collide with a real
// pkg/web/uicomponents.BindingPath: every real path contains both a "/" (the
// slot/node-index separator) and a "#" (the prop separator — see
// @ap/agentui's bindingPath), and this key contains neither.
//
// The distinction matters because the two failures call for different UI: a
// per-binding "error" degrades ONE slot (applyBindings turns just that node
// into an ap:error card — the rest of the page is unaffected data the server
// genuinely resolved). A transport failure means NOTHING was resolved, for
// reasons that have nothing to do with any one binding, so AgentUIView reads
// this key to show one page-level notice instead.
export const TRANSPORT_ERROR_KEY = "$transport";

const TRANSPORT_ERROR_MESSAGE = "This view's data could not be loaded. Check your connection and reload.";

function transportErrorState(message: string): BindingState {
  return { status: "error", message };
}

// BindingsRequestError carries a non-2xx the SERVER decided about, separating
// it from a fetch rejection (DNS, offline, CORS) that produced no response at
// all. `authored` is bindings.go's own {"error": …} copy — written for a human
// viewer for exactly these statuses: 401/403 (not authorized to interact),
// 400 (the tab's params or declaration no longer match the server's), 422 (the
// AgentUI is not Valid), 503 (the authorization check itself failed).
//
// It is safe to render because the server's own contract is that a
// browser-reachable message never carries an internal identifier — the same
// contract pkg/web/uibindings/tool's errorForStatus holds for the per-binding
// arm. Empty means the body was unreadable, and the caller falls back to
// TRANSPORT_ERROR_MESSAGE.
class BindingsRequestError extends Error {
  readonly authored: string;
  // status is kept as a FIELD, not only interpolated into the message: 410
  // means the session itself is over (the resolution ladder's terminal branch,
  // walkAgentUIDoors), which is a different fact from a failed request and
  // reaches a different consumer — the shell's ended-session disclosure. A
  // caller matching on the message text would be matching on prose.
  readonly status: number;
  constructor(status: number, authored: string) {
    super(`agent-ui bindings request failed (${status})`);
    this.name = "BindingsRequestError";
    this.authored = authored;
    this.status = status;
  }
}

// GONE is the status every agent-UI route answers for a session that has
// reached its terminal branch. It is the only status this hook reports UP
// rather than merely rendering: the others describe THIS request, while this
// one describes the session, and the shell owns what to disclose about that.
const GONE = 410;

// authoredError reads bindings.go's {"error": …} body off a non-2xx response,
// returning "" when there is nothing readable there.
//
// The parse failure is deliberately swallowed rather than rethrown: an
// unreadable body (a proxy's HTML 502, a truncated response) is not an
// additional fact a viewer or an operator can act on, and the response's
// status still reaches the console through the BindingsRequestError the
// caller throws either way. Returning "" is what selects the generic
// transport copy, so the failure is expressed as a value, not dropped.
async function authoredError(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as { error?: unknown };
    return typeof body?.error === "string" ? body.error : "";
  } catch {
    return "";
  }
}

// DEBOUNCE_MS coalesces a burst of parameter changes into one request.
//
// Cancellation alone does not bound the cost. Aborting the fetch closes the
// browser's side of the connection, but the server-side fan-out an already-sent
// request started keeps running: bindings.go resolves every bound prop
// concurrently, and the tool resolver reaches the runner over NATS under its
// own timeout rather than the HTTP request's context — so a superseded
// request's tool executions run to completion regardless. The only way to not
// pay for work nobody will look at is to not ask for it.
//
// The volume is real: an ap:daterange fires setParam once per field, and every
// POST re-resolves EVERY binding in the declaration — including bindings that
// reference no parameter at all — so one range adjustment can dispatch several
// times maxBindingsPerRequest tool executions, most of them already garbage.
// The per-origin rate limit is the only other backstop, and tripping it turns a
// viewer's own scrubbing into rate_limited cards.
//
// 250ms is under the threshold at which a control stops feeling live, and the
// FIRST evaluation of a mounted page is not delayed at all (see firstRunRef):
// a Tier-0 UI still paints as fast as the server can answer.
const DEBOUNCE_MS = 250;

// WAKE_RETRY_MS / WAKE_MAX_ATTEMPTS bound the poll that runs while a reaped
// session is being woken.
//
// Two seconds because a wake is a pod spawn — the reconciler has to notice the
// annotation, create the pod, and the runner has to reach its subscribers.
// Polling faster only multiplies the binding fan-out against a session that
// provably cannot answer yet.
//
// Fifteen attempts, so roughly thirty seconds. The bound exists because a wake
// can fail for reasons this browser will never learn: the pod cannot schedule,
// the image will not pull, the class went invalid. A skeleton with no deadline
// claims progress that is not happening, which is a worse failure than an
// honest error — the viewer waits indefinitely for something nobody is doing.
const WAKE_RETRY_MS = 2_000;
const WAKE_MAX_ATTEMPTS = 15;


// useBindings POSTs the current parameter map to POST /agent-ui/{ns}/{name}/
// bindings and returns one BindingState per binding path the server answers
// with. It re-POSTs whenever the parameter map changes — that is the spec's
// "re-evaluated on parameter change: a time-span ap:select re-runs its bound
// tool with new args, sub-second, with no LLM in the loop". Nothing here
// involves the agent: the request carries only `{params}` (bindingsRequestBody
// on the Go side) — never a source, ref, or args template, which the server
// reads fresh from its own validated declaration under the viewer's subject.
//
// A burst of changes is coalesced (DEBOUNCE_MS) and a superseded request is
// aborted, so the number of server-side fan-outs a viewer can create by
// scrubbing a control is bounded rather than one per keystroke per field.
//
// Re-evaluation now has TWO triggers, not one: a parameter change (above),
// and a settled action (`generation`, bumped by AgentUIView whenever
// useActionLifecycle's onSettled fires). The second exists because an
// action's own result deliberately does not ride the live push — see
// useActionLifecycle's own doc comment — so a control that just wrote
// through an action (advance a stage, approve a step) has no other route to
// making the page's declared bindings (a table, a metric) reflect that
// write. `generation` carries no data of its own; it only asks this hook to
// re-run with the CURRENT params, same request shape as any other trigger.
// onGone, when supplied, is called once per request that the server answered
// with 410 — the session's own terminal branch, not a failure of this request.
// It is the ONLY route by which this view learns a session ended while it was
// open: the live socket carries no terminal frame (it stays connected, serving
// snapshots, for an ended session), and a websocket handshake refusal is not
// readable as a status from the browser at all. The consequence is that a view
// nobody touches will not notice — this fires on the next binding evaluation
// (a control changed, an action settled), not on a timer.
export function useBindings(
  ns: string,
  name: string,
  params: Record<string, string>,
  generation: number,
  onGone?: () => void,
): { states: Record<string, BindingState>; refreshing: boolean } {
  const [states, setStates] = useState<Record<string, BindingState>>({});
  // refreshing is true while a RE-evaluation is in flight over content that is
  // already on screen — never during the first load, which has its own
  // vocabulary (the skeleton stand-ins applyBindings substitutes for
  // unresolved paths).
  //
  // The two cases need different treatment because they answer different
  // questions. On first load the viewer is asking "is anything coming?", and a
  // skeleton the shape of the eventual content answers it. On a re-evaluation
  // the answer is already on screen, and replacing it with skeletons would
  // throw away a correct view to announce that a slightly newer one is coming
  // — a flash of nothing in exchange for no information.
  //
  // What the viewer needs there is smaller: that the thing they just changed
  // was received, and that what they are looking at is a moment out of date.
  // Fast resolution made the absence conspicuous — change a date, and the page
  // sat still until it silently became correct, with nothing to distinguish
  // "working on it" from "your change did nothing".
  const [refreshing, setRefreshing] = useState(false);
  // Held in a ref rather than listed as an effect dependency: a caller whose
  // handler identity changes every render would otherwise re-issue the whole
  // binding fan-out on every render — the exact cost DEBOUNCE_MS exists to
  // bound. The effect always calls the LATEST handler.
  const onGoneRef = useRef(onGone);
  onGoneRef.current = onGone;
  // generation disambiguates in-flight requests. Two requests can be in
  // flight at once whenever a parameter changes before the previous
  // response has arrived (a viewer clicking through a date-range control
  // quickly is the expected case, not an edge case) — without this guard, a
  // slow response to an EARLIER parameter value arriving after a fast
  // response to a LATER one would silently overwrite the newer, correct
  // result with stale data. Only the response whose generation still
  // matches the ref (i.e., no newer request has since been issued) is
  // allowed to write state.
  const generationRef = useRef(0);
  // firstRunRef keeps the debounce off the FIRST evaluation. A freshly mounted
  // page has no burst to coalesce — it has one parameter map, the
  // declaration's own defaults — and delaying its first paint would buy nothing
  // while costing the "a Tier-0 UI is functional with no interaction" promise a
  // quarter second on every load.
  const firstRunRef = useRef(true);
  // wakeGeneration re-runs the effect for a retry without pretending the
  // parameters changed — it is the same request, asked again.
  const [wakeGeneration, setWakeGeneration] = useState(0);
  const wakeAttemptsRef = useRef(0);
  const wakeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    let mounted = true;
    generationRef.current += 1;
    const generation = generationRef.current;
    const isCurrent = () => mounted && generation === generationRef.current;
    // One controller per evaluation. The cleanup below aborts it, so a request
    // whose parameters have been superseded — or whose component has unmounted
    // — is CANCELLED rather than left in flight to be discarded on arrival.
    // The generation guard stays regardless: it protects against out-of-order
    // ARRIVAL among requests that were all legitimately issued, which
    // cancellation does not address.
    const controller = new AbortController();

    const delay = firstRunRef.current ? 0 : DEBOUNCE_MS;
    firstRunRef.current = false;

    const timer = setTimeout(() => {
      // Raised as the request goes out, not when the effect fires: the debounce
      // window is where a viewer's keystrokes are still being coalesced, and
      // announcing work during it would blink the indicator once per keystroke
      // for requests that were never issued.
      //
      // firstRunRef is already false by here (it is consumed above to pick the
      // delay), so this reads the ref's meaning off `delay` instead. A first
      // load leaves refreshing false and lets the skeletons speak.
      if (delay > 0) setRefreshing(true);
      fetch(`/agent-ui/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/bindings`, {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ params }),
        signal: controller.signal,
      })
      .then(async (res) => {
        if (!res.ok) throw new BindingsRequestError(res.status, await authoredError(res));
        return (await res.json()) as BindingsResponse;
      })
      .then((data) => {
        if (!isCurrent()) return;
        const next: Record<string, BindingState> = {};
        let waking = false;
        for (const [path, r] of Object.entries(data.bindings ?? {})) {
          if (r.status === "ok") {
            next[path] = { status: "ok", value: r.value };
            continue;
          }
          // needs_input is carried through as itself. Folding it into the
          // error arm — which a two-way ok/else map does silently — puts a
          // destructive card over a filter the viewer simply has not set yet,
          // which is the whole thing the server-side status exists to avoid.
          // Any OTHER unrecognized status stays an error: an unknown state
          // from a newer server must fail visibly, not read as fine.
          if (r.status === "needs_input") {
            next[path] = { status: "needs_input", message: r.message ?? "Choose a value above to load this data." };
            continue;
          }
          if (r.status === "waking") {
            next[path] = { status: "waking", message: r.message ?? "Waking this agent…" };
            waking = true;
            continue;
          }
          next[path] = { status: "error", message: r.message ?? "This view's data is not available." };
        }
        setStates(next);
        // A wake is in progress, so this answer is provisional: retry until
        // the runner is back, or until the budget runs out.
        //
        // BOUNDED, and that bound is the difference between a recovery and a
        // page that animates forever. A wake can fail for reasons this browser
        // will never learn — the pod cannot schedule, the image will not pull,
        // the class became invalid — and a skeleton with no deadline claims
        // progress that is not happening. When the budget is spent the page
        // says so plainly instead.
        if (waking && wakeAttemptsRef.current < WAKE_MAX_ATTEMPTS) {
          wakeAttemptsRef.current += 1;
          wakeTimerRef.current = setTimeout(() => {
            if (!mounted) return;
            setStates((prev) => prev); // keep the skeletons up
            setWakeGeneration((g) => g + 1);
          }, WAKE_RETRY_MS);
        } else if (waking) {
          setStates((prev) => {
            const out: Record<string, BindingState> = {};
            for (const [path, s] of Object.entries(prev)) {
              out[path] = s.status === "waking"
                ? { status: "error", message: "the agent for this session isn't responding" }
                : s;
            }
            return out;
          });
        } else {
          // Any successful non-waking answer means the runner is back; the
          // next cold open starts from a full budget rather than inheriting
          // the attempts this one spent.
          wakeAttemptsRef.current = 0;
        }
        setRefreshing(false);
      })
      .catch((err: unknown) => {
        // An abort is this hook cancelling its OWN superseded request in the
        // cleanup below — a deliberate action, with a newer request already
        // taking its place, not a failure with a cause to report. Checked
        // explicitly rather than left to isCurrent() so the intent is legible
        // and an abort can never surface as a console error.
        // An abort means a NEWER request is already in flight and will clear
        // the flag when it settles; returning early here (rather than clearing
        // first) is what keeps the indicator continuous across a burst of
        // changes instead of flickering off between them.
        if (controller.signal.aborted) return;
        if (!isCurrent()) return;
        setRefreshing(false);
        // Never silently drop: this is the one place the real cause is
        // available, and it is logged whether or not the viewer sees a
        // version of it.
        console.error("agent-ui bindings: request failed", err);
        // Reported BEFORE the per-binding state is rewritten, and independently
        // of it: the host's disclosure of an ended session must not depend on
        // this hook's rendering decisions. The error cards below still render —
        // the bindings genuinely could not resolve — and the host adds what
        // only it can say.
        if (err instanceof BindingsRequestError && err.status === GONE) onGoneRef.current?.();
        // A status the SERVER decided about comes with copy the server wrote
        // for this viewer; showing the generic network message instead is how
        // a revoked grant reads as "check your connection". A fetch rejection
        // (offline, DNS, CORS) has no server copy and no viewer-actionable
        // detail, so it keeps the generic line.
        const message = err instanceof BindingsRequestError && err.authored !== "" ? err.authored : TRANSPORT_ERROR_MESSAGE;
        setStates((prev) => {
          // Any path a PRIOR successful response resolved must not keep
          // showing that now-unconfirmed value forever — flip it to error
          // too, not just add the transport-level notice, so a slot that
          // was genuinely rendering real data doesn't silently go stale.
          const next: Record<string, BindingState> = {};
          for (const path of Object.keys(prev)) next[path] = transportErrorState(message);
          next[TRANSPORT_ERROR_KEY] = transportErrorState(message);
          return next;
        });
      });
    }, delay);

    return () => {
      mounted = false;
      // Two distinct cases, both needed: clearTimeout stops a request still
      // waiting out the debounce from ever being issued, and abort() cancels
      // one already in flight.
      clearTimeout(timer);
      controller.abort();
      // A pending wake retry belongs to the request that scheduled it. Left
      // running it would fire against an unmounted component, or — worse — go
      // on polling after the viewer navigated away, keeping a woken session
      // awake for nobody.
      if (wakeTimerRef.current !== null) {
        clearTimeout(wakeTimerRef.current);
        wakeTimerRef.current = null;
      }
    };
    // Keyed on JSON.stringify(params) rather than params itself: a caller
    // that recomputes an equal-by-value params object every render (as
    // AgentUIView's setParam does not, but a future caller might) must not
    // re-fetch when nothing actually changed. `generation` is appended
    // alongside it as the second, independent trigger described above.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ns, name, JSON.stringify(params), generation, wakeGeneration]);

  return { states, refreshing };
}
