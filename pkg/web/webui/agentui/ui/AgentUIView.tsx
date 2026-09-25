import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Alert, AlertDescription, AlertTitle } from "@ap/design";
import { Loader2 } from "lucide-react";
import { cn } from "@ap/design/lib/utils";
import { ActionsProvider, applyBindings, BindingParamsProvider, collectDefaultParams, hooksContaining, HookStateProvider, pageLayoutOf, paramsFromSearch, questionOutsideHooks, reconcileParams, renderNode, searchWithParams, stepBindings, treeContains, ViewSessionProvider } from "@ap/agentui";
import type { ActionsContextValue, BindingState, Declaration, HookState } from "@ap/agentui";
import type { ApprovalNotice, TrustEvent } from "../../sessions/ui/Chrome";
import { InteractionCard } from "../../chat/ui/InteractionCard";
import { interactionRejectionText } from "../../chat/ui/interactionText";
import { sendSessionMessage } from "../../chat/ui/sessionMessage";
import { useSessionSignals } from "../../chat/ui/sessionSignals";
import { useSessionFrames } from "../../chat/ui/SessionSocket";
import type { ChatFrame, InteractionDecisionRejectedPayload } from "../../chat/ui/types";
import { useInteractionDecision } from "../../chat/ui/useInteractionDecision";
import { AgentReplyModal } from "./AgentReplyModal";
import { DefaultProgress } from "./DefaultProgress";
import { TRANSPORT_ERROR_KEY, useBindings } from "./useBindings";
import { useActionLifecycle } from "./useActionLifecycle";

// ChromeSignals is what the view reports UP to the shell's chrome: the
// approvals its live action lifecycle knows about, and the trust events that
// must auto-reveal chrome. The view renders no chrome of its own — chrome is
// the shell's — so a signal the view computes and does not report is a signal
// nobody acts on.
//
// trustEvents is a LIST and is forwarded unranked, exactly as Chrome consumes
// it. Reducing it to one entry here would reintroduce the precedence defect one
// layer earlier, where Chrome's own tests could not see it.
export interface ChromeSignals {
  approvals: readonly ApprovalNotice[];
  trustEvents: readonly (TrustEvent | null | undefined)[];
}

// EMPTY_CHROME_SIGNALS is the seed every host of this view starts from: a
// view that has reported nothing yet has no approvals and no trust events.
// A module-level constant rather than a fresh object literal per host, so
// "nothing reported" is referentially stable and cannot itself look like a
// change to a host that compares with sameChromeSignals.
export const EMPTY_CHROME_SIGNALS: ChromeSignals = { approvals: [], trustEvents: [] };

// HOOK_UPDATED_MS is how long a hook wears its "Updated" cue after a live view
// frame names it.
//
// The cue reports a FACT AFTER THE EVENT — this region just changed — rather
// than a prediction that it is about to. That is the whole difference from a
// spinner, and it is why a bound here is honest rather than a guess: nothing is
// pending when the cue is showing, so a deadline cannot cut short work that is
// still running. It only decides how long the change stays worth pointing at.
//
// Four seconds: long enough that a viewer whose eyes were elsewhere on the page
// still catches which region moved, short enough that a page an agent writes
// repeatedly does not end up permanently flagged, which would make the mark
// mean nothing.
export const HOOK_UPDATED_MS = 4000;

// sameChromeSignals reports whether two reports say the same thing. Hosts use
// it in their setState updater so a re-report of unchanged signals returns the
// SAME state object and React bails out of the re-render — which is what keeps
// the report loop (view effect -> host setState -> view re-render -> view
// effect) from running forever when a host's handler identity is not stable.
// Compared by VALUE, not by reference: the view rebuilds these arrays whenever
// its own memo inputs change, and equal contents must not read as a change.
export function sameChromeSignals(a: ChromeSignals, b: ChromeSignals): boolean {
  if (a === b) return true;
  if (a.approvals.length !== b.approvals.length || a.trustEvents.length !== b.trustEvents.length) return false;
  for (let i = 0; i < a.approvals.length; i++) {
    const x = a.approvals[i];
    const y = b.approvals[i];
    if (x.id !== y.id || x.label !== y.label || x.addressedToViewer !== y.addressedToViewer) return false;
  }
  for (let i = 0; i < a.trustEvents.length; i++) {
    const x = a.trustEvents[i];
    const y = b.trustEvents[i];
    if ((x?.id ?? null) !== (y?.id ?? null) || (x?.kind ?? null) !== (y?.kind ?? null)) return false;
  }
  return true;
}

// AgentUIViewProps mirrors pkg/web/webui/agentui/viewmodel.go's ViewProps
// field-for-field (json tags: ns, name, declaration) — that Go struct is the
// contract; this type does not invent fields it doesn't carry. `declaration`
// arrives as viewmodel.go's declarationWire{View, AgentComposed}, which is exactly
// @ap/agentui's `Declaration` shape (types.ts), so it's typed with that
// shared mirror rather than a second local copy.
//
// Identity, the session-origin disclosure and the chrome request are NOT
// here: chrome is the shell's (pkg/web/webui/sessions), and this component is
// one of the views its content region can hold.
//
// The shell reads these props out of its own bootstrap, an unchecked cast, so
// nothing at compile time or runtime notices when a key here and its Go json
// tag drift apart. The pin is testdata/props.golden.json: ViewFor's own
// output is asserted against that file (props_golden_internal_test.go) and
// AgentUIView.test.tsx renders it through this type, so a rename on either
// side fails the other side's suite.
export interface AgentUIViewProps {
  ns: string;
  name: string;
  declaration: Declaration;
  // agentParams is what the AGENT set this view's controls to
  // (set_view_params), already filtered server-side to the parameters this
  // declaration declares. Absent for the ordinary page, where the viewer
  // drives every control themselves.
  //
  // Carried apart from the declaration on purpose: an author's default and an
  // agent's choice are both "a value the viewer did not pick", but they are
  // not the same claim and they lose to a viewer's own selection at different
  // moments. Only this component knows what the viewer has touched, so the
  // precedence between the three lives here.
  agentParams?: Record<string, string>;
  // onChromeSignals is the ONLY route from this view to chrome. It is
  // optional so the view can be rendered and tested standalone, but a shell
  // that renders the view and never passes it leaves every approval and
  // every trust event this view computes unacted on — see ChromeSignals.
  // shown is whether THIS view is the one the viewer is looking at. The shell
  // owns that answer — it keeps this component mounted behind `hidden` while
  // the transcript is up, so the component cannot tell from its own DOM — and
  // the reply modal is a fallback for THIS view that must never cover its
  // sibling. A dialog renders through a portal to document.body, which an
  // ancestor's display:none cannot contain, so "hidden" alone does not hold it.
  //
  // Required rather than optional: a host that keeps this view mounted and
  // forgets to say would reproduce exactly the escape this exists to stop, and
  // there is no safe default for a question only the host can answer.
  //
  // It gates the modal and the "Needs your decision" region — two deliberate
  // consumers, for the same reason twice over: both are things the viewer
  // would act on, and the transcript already shows the reply and renders the
  // same InteractionCard inline, so a hidden view offering either would put a
  // second copy on the screen. The default progress region and the per-hook
  // cues live INSIDE the hidden subtree, where the browser already hides them
  // and they cost nothing.
  shown: boolean;
  onChromeSignals?: (signals: ChromeSignals) => void;
  // onSessionEnded reports that a route this view drives answered "this
  // session is over" (410). The view discloses nothing about it itself: the
  // ended-session disclosure and the start-a-replacement control are chrome's
  // and the shell's, and the shell derives both from one value.
  //
  // Without it, a session that ends under an agent-defined view leaves chrome
  // saying "Resumed your session." indefinitely — the transcript arm has had
  // this route since it was built, and this is the same report from the other
  // arm. See useBindings' onGone for exactly when it can and cannot fire.
  onSessionEnded?: () => void;
}

// ContentRegion is the agent-owned area: it renders the declaration's ONE page
// tree through @ap/agentui's renderNode — the platform's closed component
// vocabulary (ap:stack, ap:table, ap:markdown, ...) plus the single structural
// type, oap:generative. This is the only place in this view that interprets a
// node's component/props/bindings, and the shell's chrome never sees any of it.
//
// The agent's writable regions are the oap:generative HOOKS inside that tree,
// and they are rendered by the tree walk like any other node: this component
// does not enumerate them, style them, or know where they are. What it does own
// is the per-region state a node cannot say about ITSELF — which hooks the
// server marked composed, which just changed, which are re-resolving — and it
// hands that down as one HookState through HookStateProvider. That indirection
// is the security argument in @ap/agentui's hooks.tsx: a declared node has no
// route to a fact the page then trusts about it.
//
// Bindings ARE resolved here, but never by this component fetching or
// touching a capability itself: `states` (one BindingState per
// pkg/web/uicomponents.BindingPath) comes from AgentUIView's useBindings, which
// POSTs only the current parameter map to the server and gets back
// per-binding results resolved under the viewer's own subject. The tree is
// run through @ap/agentui's applyBindings(view, states) — laying resolved
// values, loading placeholders, or per-node error cards onto the declared
// tree — BEFORE renderNode ever sees it, so renderNode's job stays exactly
// "turn this Node tree into React" regardless of whether the tree came
// straight from the declaration or via applyBindings.
//
// onRenderError reports a tree that contained a node the renderer could
// not draw. That failure is the content region's only route to chrome (via
// AgentUIView's onChromeSignals): the design assigns error/degraded state to
// chrome, and a hard error is one of the trust events that must auto-reveal
// it, so a render failure cannot be left as a card inside this region alone.
// A binding error (per-node, or the page-level transport notice below) is
// deliberately NOT one of those events — it renders as data inside the
// content region, same as any other node.
//
// TWO PLATFORM FALLBACKS sit beside this region, and one rule decides both:
// what the DECLARED TREE is still asking. A page holding an unanswered
// ap:question anywhere already asks the viewer something, so AgentReplyModal —
// the floor beneath a question the agent asked in prose — does not open beside
// it; a page holding an ap:progress anywhere already says what the agent is
// doing, so DefaultProgress does not repeat it. Both are AgentUIView's to
// render (it owns the tree, the answered set and the session's signals); this
// region's only part in them is `waiting`, the hooks whose subtrees hold a
// still-unanswered ap:question, which it passes down as HookState so each such
// region can wear the cue. Deriving that from the
// tree rather than from a node's own claim is the same argument the rest of
// HookState makes: a declared node has no route to a fact the page then trusts
// about it.

// staleRegions returns the regions whose bindings are re-resolving right now.
//
// Keyed off the binding-path grammar (`<region>/<nodePath>#<prop>`, see
// @ap/agentui's bindingPath) rather than by walking the tree looking for
// Binding props: `states` is what the server actually answered for, so a region
// is treated as data-bearing exactly when data was resolved for it. Walking the
// tree instead would also count a binding that was declared but never
// evaluated.
//
// A region whose bindings did NOT re-resolve says nothing. A heading or a
// static paragraph is exactly as correct during a re-evaluation as after it,
// and dimming it would claim otherwise — turning one changed filter into a page
// that appears to reload wholesale.
//
// "" is a legal member and is the region outside every hook. No hook answers to
// it, so this component draws that one itself; every other member is a hook
// name, and GenerativeHook draws its own.
function staleRegions(states: Record<string, BindingState>, refreshing: boolean): Set<string> {
  const out = new Set<string>();
  if (!refreshing) return out;
  for (const path of Object.keys(states)) {
    // TRANSPORT_ERROR_KEY is useBindings' own reserved entry and carries no
    // region separator, so it never names a region.
    const sep = path.indexOf("/");
    if (sep < 0) continue;
    out.add(path.slice(0, sep));
  }
  return out;
}

function ContentRegion({
  declaration,
  states,
  refreshing,
  updatedHooks,
  waiting,
  onRenderError,
}: {
  declaration: Declaration;
  states: Record<string, BindingState>;
  refreshing: boolean;
  updatedHooks: ReadonlyMap<string, number>;
  // waiting names the hooks whose ap:question is still asking — the hooks
  // holding one, minus the ones the server marked answered. Derived from the
  // tree (and declaration.answered) by AgentUIView, which needs the same
  // answer to decide whether its own reply modal applies, so it is computed
  // once and shared rather than twice with two chances to disagree.
  waiting: ReadonlySet<string>;
  onRenderError: (component: string) => void;
}) {
  const stale = useMemo(() => staleRegions(states, refreshing), [states, refreshing]);

  // Which hooks the page has folded: every hook bound to a timeline step
  // whose state is done, titled by the hook's own title when the author gave
  // one, else that step's label. Derived from the declared tree — the agent
  // keeps the timeline's states current, and this is the whole of what a
  // repaint has to do to tidy the page.
  const collapsed = useMemo(() => {
    const out = new Map<string, string>();
    for (const [hookName, b] of stepBindings(declaration?.view)) {
      if (b.state === "done") out.set(hookName, b.title ?? b.label);
    }
    return out;
  }, [declaration?.view]);

  // The whole of what the page knows about its regions, assembled in one place
  // and read by every hook in the tree through context. composed comes from the
  // declaration's SERVER-COMPUTED agentComposed list — a top-level array of hook
  // names, never a node field — so a fragment cannot mark its own work.
  const hookState = useMemo<HookState>(
    () => ({
      composed: new Set(declaration?.agentComposed ?? []),
      updated: updatedHooks,
      stale,
      waiting,
      // answered is SERVER-COMPUTED (declaration.answered) the same way
      // agentComposed is: derived on every read/push from the fill's write
      // time and the transcript, never something the browser decides for
      // itself.
      answered: new Set(declaration?.answered ?? []),
      collapsed,
    }),
    [declaration?.agentComposed, declaration?.answered, updatedHooks, stale, waiting, collapsed],
  );

  // renderNode invokes the sink synchronously, DURING this render. Reporting
  // straight to the parent from there would be a setState-during-render of a
  // different component (a React invariant violation), so failures are
  // collected locally here and reported from the effect below, after commit.
  const failures: string[] = [];
  const tree = declaration?.view
    ? renderNode(applyBindings(declaration.view, states), undefined, (component) => failures.push(component))
    : null;

  // Only the first failing component is lifted: it names the trust event, and
  // a second bad node in the same declaration is the same hard error as far as
  // revealing chrome goes. Keying the effect on that name rather than on the
  // (freshly allocated every render) array is what keeps a re-render of the
  // same broken declaration from re-notifying.
  const firstFailure = failures.length > 0 ? failures[0] : null;
  useEffect(() => {
    if (firstFailure !== null) onRenderError(firstFailure);
  }, [firstFailure, onRenderError]);

  // The wire always carries a view — uicomponents rejects a declaration without
  // one, and the live frame's guard drops a frame missing it — so this is
  // defence in depth rather than an ordinary state. Kept because the
  // alternative to a sentence is a blank page with nothing saying why.
  if (!declaration?.view) {
    return <div className="p-4 text-sm text-muted-foreground">This agent has not declared any content for this session.</div>;
  }

  // transportFailure is useBindings.TRANSPORT_ERROR_KEY — set when the
  // /bindings POST itself failed (network down, non-2xx), as opposed to a
  // per-binding "error" the server resolved deliberately. Nothing resolved
  // in that case, so a single page-level notice is the honest description —
  // per-node cards would imply the server looked at each binding and
  // rejected it individually, which is not what happened.
  const transportFailure = states[TRANSPORT_ERROR_KEY];

  return (
    <HookStateProvider value={hookState}>
      <div
        data-testid="agent-ui-root"
        className={cn("flex flex-col gap-2 p-4 mx-auto", pageLayoutOf(declaration?.view) === "rail" ? "max-w-6xl" : "max-w-3xl")}
        aria-busy={stale.has("") || undefined}
      >
        {transportFailure?.status === "error" && (
          <Alert variant="destructive">
            <AlertTitle>Data unavailable</AlertTitle>
            <AlertDescription>{transportFailure.message}</AlertDescription>
          </Alert>
        )}
        <div className={cn("relative", stale.has("") && "opacity-60 transition-opacity")}>
          {tree}
          {stale.has("") && (
            // The region outside every hook, drawn here because no hook answers
            // to it. Corner-anchored and small: the content stays readable and
            // in place, which is the point — the previous answer is still the
            // best available one until the next arrives. pointer-events-none so
            // an indicator can never eat a click meant for the control
            // underneath.
            <div
              data-testid="agent-ui-region-refreshing"
              className="pointer-events-none absolute right-2 top-2 flex items-center gap-1.5 text-[11px] text-muted-foreground"
            >
              <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden="true" />
              <span>Updating…</span>
            </div>
          )}
        </div>
      </div>
    </HookStateProvider>
  );
}

// useUpdatedHooks owns the per-hook "this region just changed" cue: which hooks
// are currently wearing it, and the timer that takes each one off again.
//
// A MAP of hook name to tick, not a set, because a SECOND write to a region
// already showing the cue is still a change — a consumer keying an effect on
// presence alone could not see it — and because the timer is re-armed per hook,
// so two regions written seconds apart each get their full window rather than
// sharing one.
function useUpdatedHooks(): {
  updatedHooks: ReadonlyMap<string, number>;
  markUpdated: (hook: string) => void;
} {
  const [updatedHooks, setUpdatedHooks] = useState<Map<string, number>>(() => new Map());
  const timersRef = useRef<Map<string, ReturnType<typeof setTimeout>>>(new Map());

  const markUpdated = useCallback((hook: string) => {
    setUpdatedHooks((prev) => new Map(prev).set(hook, Date.now()));
    const existing = timersRef.current.get(hook);
    if (existing !== undefined) clearTimeout(existing);
    timersRef.current.set(
      hook,
      setTimeout(() => {
        timersRef.current.delete(hook);
        setUpdatedHooks((prev) => {
          if (!prev.has(hook)) return prev;
          const next = new Map(prev);
          next.delete(hook);
          return next;
        });
      }, HOOK_UPDATED_MS),
    );
  }, []);

  // Every pending timer outlives a fast unmount otherwise, and its setState
  // would land on a component that is gone.
  useEffect(() => {
    const timers = timersRef.current;
    return () => {
      for (const t of timers.values()) clearTimeout(t);
      timers.clear();
    };
  }, []);

  return { updatedHooks, markUpdated };
}

// AgentUIView is the agent-defined view: one ContentRegion (agent-owned)
// rendering the declared page tree, with the platform's own fallbacks stacked
// around it. It renders NO chrome — identity, the session list, the
// sibling-view switch, the approval surface and the collapse toggle are the
// session shell's (pkg/web/webui/sessions), which renders this view inside its
// content region.
//
// This component still SOURCES the trust events its own content can raise —
// a render failure, and an approval its live lifecycle reports as addressed
// to the viewer — and reports them up through onChromeSignals. Chrome
// implements the auto-reveal, but a component cannot reveal on an event
// nobody hands it: a UI requesting collapsed chrome plus a declaration the
// browser's bundle cannot render (version skew between a running tab and a
// redeployed server) would otherwise leave the viewer looking at "Cannot
// render this section" with identity, the chat escape hatch, and the approval
// surface all hidden.
export function AgentUIView({ ns, name, declaration: initialDeclaration, agentParams, shown, onChromeSignals, onSessionEnded }: AgentUIViewProps) {
  const [renderFailure, setRenderFailure] = useState<TrustEvent | null>(null);

  const handleRenderError = useCallback((component: string) => {
    const id = `render-error:${component}`;
    // The same failing node must keep the same event identity across
    // re-renders: Chrome re-reveals whenever the id changes, so returning a
    // fresh object every time would both re-reveal over the viewer's own later
    // toggle and re-render forever.
    setRenderFailure((prev) => (prev?.id === id ? prev : { kind: "hard_error", id }));
  }, []);

  // declaration is STATE, not a fixed prop: a live `view` frame
  // (useActionLifecycle's onView, below) rewrites it after mount, when the
  // agent fills or clears a hook while this page is already open (J4
  // — an update_view landing on an open page). Seeded once from the
  // bootstrap prop; every later value is webd's OWN re-resolved read (see
  // live.go's liveViewMessage doc comment) — never anything carried on the
  // triggering envelope, so this component only ever renders a declaration
  // the platform itself validated.
  const [declaration, setDeclaration] = useState<Declaration>(initialDeclaration);

  // The session's own live facts — whether a turn is running, what it is doing,
  // whether it ended waiting on the viewer — folded from the shell's one
  // session socket. Called unconditionally and above everything that reads it,
  // like every other hook here.
  //
  // This is what makes the view's fallbacks possible at all: they are derived
  // from frames the runner already publishes to every surface, so an agent that
  // declared nothing for a situation still cannot leave the viewer with a page
  // that says nothing is happening while something is.
  const signals = useSessionSignals();

  // What the DECLARED TREE still ASKS — the one rule that decides both
  // fallbacks, computed in a single memo because every answer comes from the
  // same tree and the same answered set, and they must never disagree.
  //
  // `open` is the hooks whose question is still asking: every hook holding an
  // ap:question, MINUS the ones the server marked answered. answered is
  // server-computed (declaration.answered — derived from the fill's write time
  // against the transcript's visible user entries, never a browser guess), and
  // subtracting it is what keeps an already-sent card from suppressing the
  // floor beneath it: the viewer who answered the brief and got the agent's
  // next question in prose must get the reply modal, not a page showing a sent
  // card and nothing asking anything.
  //
  // questionShowing asks about the WHOLE page — a question on screen is a
  // question on screen whoever wrote it, and a platform fallback beside one
  // would ask the same thing twice in different words — so it is `open` plus
  // the author's own questions, which live outside every hook and have no hook
  // name anything could mark answered. progressShowing is the same whole-page
  // question for the progress floor, where nothing is ever "answered".
  // `waiting` is the narrower answer — which HOOKS are still asking — and it is
  // what each such region wears as its own cue.
  //
  // An ap:notice anywhere counts the same way: the agent told the person
  // something and is waiting for an event, and a modal demanding typed words
  // beside it would turn a notice back into a question.
  const { questionShowing, progressShowing, waiting } = useMemo(() => {
    const questionHooks = hooksContaining(declaration?.view, "ap:question");
    const answered = new Set(declaration?.answered ?? []);
    const open = new Set([...questionHooks].filter((h) => !answered.has(h)));
    return {
      questionShowing: open.size > 0 || questionOutsideHooks(declaration?.view) || treeContains(declaration?.view, "ap:notice"),
      progressShowing: treeContains(declaration?.view, "ap:progress"),
      waiting: open,
    };
  }, [declaration?.view, declaration?.answered]);

  // dismissedSeq is the seq of the LAST reply whose modal the viewer settled —
  // by answering it or by choosing Later. Compared against the current reply's
  // seq rather than kept as a bare "dismissed" flag, because the seq is
  // session-monotonic (see sessionSignals' replySeq): a latch would silence
  // every later question in the session, while this silences exactly the one
  // that was settled and reopens for the next.
  const [dismissedSeq, setDismissedSeq] = useState(0);

  // params is the CURRENT binding-parameter map — seeded once from the
  // declaration's own declared control values (collectDefaultParams) so a
  // Tier-0 UI resolves its bindings on first paint with no interaction, then
  // updated by whichever ap:select/ap:daterange the viewer drives (see
  // params.tsx's BindingParamsProvider — that seam is how a control reaches
  // this state without AgentUIView naming any control type). Changing it is
  // what makes useBindings re-POST: the effect there is keyed on the map's
  // JSON, not on identity.
  // Seeded from the declaration's literal defaults, then OVERLAID with whatever
  // the address bar already carries, so an arriving link decides the view it
  // describes. The overlay is filtered through reconcileParams for the same
  // reason a live `view` frame is: a URL can name a parameter this declaration
  // does not declare (an old link, a hand-edited address), and such a key must
  // not ride along into a binding request it can no longer affect.
  // Three sources, in strictly increasing authority:
  //
  //   author default  <  agent choice  <  the viewer's own selection
  //
  // The author's default is what the page means with nobody having said
  // anything. The agent's choice is somebody having said something — "the last
  // seven days", in a channel, possibly before this page was open — which is
  // more specific than a default and less specific than a hand on the control.
  // The address bar is the viewer's own, either because they picked it here or
  // because they were sent a link describing a particular view; either way it
  // is the most direct statement of intent available and it wins.
  const [params, setParams] = useState<Record<string, string>>(() => {
    const defaults = collectDefaultParams(declaration);
    const seeded = { ...defaults, ...(agentParams ?? {}) };
    if (typeof window === "undefined") return seeded;
    return reconcileParams(declaration, { ...seeded, ...paramsFromSearch(window.location.search) });
  });

  // touchedRef records every key the VIEWER has set with their own hand. It is
  // what a later agent write is measured against: an agent may fill a control
  // nobody has touched, and may never move one out from under a reader.
  //
  // A ref, not state, because nothing renders from it — and because it must be
  // updated during the same event that changes the parameter, not a render
  // later, or a push arriving in between would be judged against a stale set.
  //
  // Keys already in the address bar at mount count as touched: a viewer who
  // followed a link to a filtered view has stated that filter as surely as one
  // who picked it, and the agent must not overwrite it on the next push.
  const touchedRef = useRef<Set<string>>(
    new Set(typeof window === "undefined" ? [] : Object.keys(paramsFromSearch(window.location.search))),
  );
  const setParam = useCallback((paramName: string, value: string) => {
    touchedRef.current.add(paramName);
    setParams((prev) => (prev[paramName] === value ? prev : { ...prev, [paramName]: value }));
  }, []);

  // A later agent write reaches the page through the live socket, which
  // re-delivers the whole view. Applied ONLY to untouched keys: the agent
  // filling in a blank control is the feature, and the agent silently
  // re-filtering a page someone is reading is the thing that would make this
  // feature unsafe to ship. Identity-stable when nothing applies, so an
  // unrelated view push cannot churn the parameter map and re-trigger every
  // binding.
  const applyAgentParams = useCallback((incoming: Record<string, string>, decl: Declaration) => {
    setParams((prev) => {
      const next = { ...prev };
      let changed = false;
      for (const [k, v] of Object.entries(reconcileParams(decl, incoming))) {
        if (touchedRef.current.has(k) || next[k] === v) continue;
        next[k] = v;
        changed = true;
      }
      return changed ? next : prev;
    });
  }, []);

  // The parameter map is mirrored into the query string on every change, so a
  // filter survives a reload, survives switching to the transcript and back,
  // and can be sent to someone else as the view it actually describes.
  //
  // replaceState, not pushState: dragging a date picker would otherwise stack
  // one history entry per keystroke and turn Back into an undo log for
  // individual digits. The trade is that Back leaves the filtered view rather
  // than stepping through filters, which is the behaviour a viewer expects
  // from a control that looks like a form field.
  //
  // Guarded on an exact string compare so an unchanged map never touches
  // history at all — searchWithParams sorts its keys precisely so this
  // comparison is stable rather than order-dependent.
  useEffect(() => {
    if (typeof window === "undefined") return;
    const next = searchWithParams(window.location.search, params);
    if (next === window.location.search) return;
    window.history.replaceState(window.history.state, "", `${window.location.pathname}${next}${window.location.hash}`);
  }, [params]);
  const bindingParams = useMemo(() => ({ params, setParam }), [params, setParam]);

  // viewSession is this view's own (ns, name), provided to the declared page
  // through ViewSessionProvider — the HOST's session, never anything a node
  // says. ap:attachment reads it to address the artifact-view meta and
  // download routes for the session the viewer is actually looking at.
  const viewSession = useMemo(() => ({ ns, name }), [ns, name]);

  // bindingsGeneration is the second trigger useBindings' own doc comment
  // describes: bumped whenever an action settles OR a live view update
  // arrives, so a control that just wrote through an action (advance a
  // stage, approve a step), or a hook the agent just wrote, makes the
  // page's own declared bindings catch up without the viewer reloading.
  const [bindingsGeneration, setBindingsGeneration] = useState(0);
  const handleActionSettled = useCallback(() => {
    setBindingsGeneration((g) => g + 1);
  }, []);

  // The per-hook updated cue. Owned here rather than in ContentRegion because
  // it is driven by a LIVE FRAME (handleView, below) and not by anything the
  // rendered tree knows: ContentRegion receives the current map and passes it
  // into the HookState every hook reads.
  const { updatedHooks, markUpdated } = useUpdatedHooks();

  // declarationJSONRef holds the serialized declaration currently applied, so
  // handleView can tell a real change from the open-time ECHO. live.go sends a
  // `view` frame unconditionally on every connect (correct — a browser that
  // reconnects must catch up on a declaration that changed while it was away),
  // and on a plain page load that frame is byte-identical to the bootstrap
  // prop: both are the same declarationWire marshalled by the same server-side
  // resolveView, so a string compare is exact rather than approximate.
  //
  // Applying it anyway is not merely a wasted render. It bumps
  // bindingsGeneration, whose effect cleanup ABORTS useBindings' in-flight
  // mount fetch and re-issues it after the debounce — and aborting does not
  // stop the server-side work, which bindings.go has already fanned out
  // concurrently with each tool resolver running under its own timeout, not
  // the request's context (see DEBOUNCE_MS's own doc comment). So every page
  // load, and every reconnect of the flat-interval retry loop, used to cost a
  // second full binding fan-out that nothing would ever read.
  const declarationJSONRef = useRef(JSON.stringify(initialDeclaration));

  // handleView applies one live `view` frame: swap in webd's re-resolved
  // page tree, reconcile the parameter map against it — params.tsx's
  // reconcileParams, the SAME coalesced scrub that already handles a
  // parameter burst, reused here rather than a second hand-rolled merge —
  // and bump bindingsGeneration through the SAME trigger handleActionSettled
  // uses, not a second re-evaluation path.
  //
  // The early return is the echo guard above; the ref is written before any
  // setState so two frames delivered in one tick cannot both pass it.
  // The agent's parameter choices are applied BEFORE the echo guard, and
  // deliberately outside it. set_view_params changes no hook, so the
  // declaration in its frame is byte-identical to the one already applied —
  // the guard would return early and the values would never land. That is the
  // whole feature silently doing nothing on the one frame that carries it.
  //
  // Applying unconditionally is safe because applyAgentParams is idempotent
  // and identity-stable: an echo frame re-offering values already in effect
  // returns the same state object, React bails out, and no binding re-runs.
  //
  // The updated cue is marked BEFORE the echo guard for the same reason, and it
  // is the stronger case: a CLEAR of an already-empty hook produces bytes the
  // page already has, so the guard returns early and the declaration never says
  // the agent acted. The frame's `hook` is the only evidence there is, and the
  // cue is the floor — a region the agent wrote must say so even when what it
  // wrote was nothing.
  const handleView = useCallback((next: Declaration, incomingAgentParams?: Record<string, string>, hook?: string) => {
    if (incomingAgentParams && Object.keys(incomingAgentParams).length > 0) {
      applyAgentParams(incomingAgentParams, next);
    }
    if (hook) markUpdated(hook);
    const nextJSON = JSON.stringify(next);
    if (nextJSON === declarationJSONRef.current) return;
    declarationJSONRef.current = nextJSON;
    setDeclaration(next);
    setParams((prev) => reconcileParams(next, prev));
    setBindingsGeneration((g) => g + 1);
  }, [applyAgentParams, markUpdated]);

  const { states: actionStates, invoke, liveError } = useActionLifecycle(ns, name, params, handleActionSettled, handleView, declaration?.actions ?? []);
  // answer is the seam an ap:question sends through: free text to this
  // session's transcript, as the viewer, over the same route the chat composer
  // uses. Nothing about it is action-shaped — there is no tool, no args
  // template and nothing to authorize beyond what the viewer could have typed
  // themselves — which is why it sits beside invoke rather than inside it.
  const answer = useCallback((text: string) => sendSessionMessage(ns, name, text), [ns, name]);
  // busy carries the session's turn into every control on the page: while
  // the agent is working, a form, button or question is disabled, whatever
  // its own lifecycle says (a Prompt action settles on delivery, long before
  // the agent answers). It clears when the turn ends — awaiting the viewer,
  // complete, or idle — which is exactly when a control may fire again.
  const actionsValue: ActionsContextValue = useMemo(
    () => ({ states: actionStates, invoke, answer, busy: signals.turnActive }),
    [actionStates, invoke, answer, signals.turnActive],
  );

  const { states, refreshing } = useBindings(ns, name, params, bindingsGeneration, onSessionEnded);

  // approvalSignature is a stable string built from exactly the fields that
  // change what approvalNotices/approvalTrustEvents below produce, so a
  // states update that leaves the awaiting_approval subset unchanged (a
  // settle of some OTHER action, say) does not allocate new arrays —
  // actionStates itself gets a new object reference on every settle
  // regardless of which action changed.
  const approvalSignature = Object.entries(actionStates)
    .filter(([, s]) => s.phase === "awaiting_approval")
    .map(([action, s]) => `${action}:${s.requestId ?? ""}:${s.approvalAddressedToViewer ? "1" : "0"}`)
    .sort()
    .join("|");

  // approvalNotices feeds the shell's approval-surface region;
  // approvalTrustEvents feeds the trustEvents list below. Only an approval
  // ADDRESSED TO THIS
  // VIEWER is a trust event — Decision 3's approval table auto-reveals chrome
  // for the approver, never for the requester of someone else's pending
  // approval (that case is disclosed on the control itself; auto-revealing
  // chrome for every approval regardless of who must act would make chrome
  // cry wolf in a shared session).
  const { approvalNotices, approvalTrustEvents } = useMemo(() => {
    const notices: ApprovalNotice[] = [];
    const events: TrustEvent[] = [];
    for (const [action, state] of Object.entries(actionStates)) {
      if (state.phase !== "awaiting_approval") continue;
      const id = state.requestId ?? action;
      notices.push({
        id,
        label: state.approvalAddressedToViewer
          ? "Your approval is needed to continue."
          : "Waiting on approval from someone else.",
        addressedToViewer: Boolean(state.approvalAddressedToViewer),
      });
      if (state.approvalAddressedToViewer) events.push({ kind: "approval_addressed_to_viewer", id });
    }
    return { approvalNotices: notices, approvalTrustEvents: events };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [approvalSignature]);

  // chromeSignals is the whole of what this view tells chrome. Every source is
  // carried, unranked. Picking one with `??` would mean the loser can never
  // reveal while the winner holds an event: with one source winning, a viewer
  // who collapses chrome after handling it is then unreachable by every later
  // event from a DIFFERENT source. The design ranks no trust event above
  // another, so neither does this list — it only ever grows by appending a
  // new source, never by reducing to one entry. A binding failure — per-node
  // or the page-level transport notice — is deliberately absent: the spec
  // names approvals, credential requests, identity questions, and hard render
  // errors as what auto-reveals chrome, and a binding error is none of those.
  //
  // Memoized on its own sources so the object identity changes only when the
  // signals do; the reporting effect below depends on it, and a fresh object
  // every render would re-notify the shell on every keystroke's worth of
  // unrelated state.
  const chromeSignals = useMemo<ChromeSignals>(
    () => ({ approvals: approvalNotices, trustEvents: [renderFailure, ...approvalTrustEvents] }),
    [approvalNotices, approvalTrustEvents, renderFailure],
  );

  // The report itself. onChromeSignals is in the deps deliberately: the shell
  // supplies a useCallback'd handler with an empty dep array (see
  // SessionShell), so its identity is stable and this effect runs only when
  // the signals change. A handler whose identity changed every shell render
  // would make this effect fire every render — the same hazard
  // handleRenderError's own comment describes for the render-failure id.
  useEffect(() => {
    onChromeSignals?.(chromeSignals);
  }, [chromeSignals, onChromeSignals]);

  // showModal is the whole of when the reply fallback applies. Every clause is
  // load-bearing: this view must be the one on screen, the turn must have ended
  // either WAITING on the viewer or on the runner's idle exit (not merely
  // ended for some other reason), there must be a reply to show, it must be
  // one the viewer has not already settled, the page must not already be
  // asking, and the session must not be over — an ended session has nobody
  // left to answer.
  //
  // The idle-exit clause (signals.pausedIdle) is what makes a pause visible
  // here, not only an ask: the runner's idle exit means the agent replied and
  // stopped WITHOUT asking anything, and with no declared card and no ask,
  // this view rendered nothing at all for it — the reply landed only in the
  // transcript, and the page looked dead to a viewer who cannot see that. A
  // pause must be visible on this view, with a way to continue from it, even
  // though nobody is owed an answer the way awaitingReply's ask is.
  //
  // currentReplySeq is the seq of the reply IN HAND, which is not
  // signals.replySeq: that one is the session's running count and survives a
  // turn start, while lastAgentReply is nulled by one, so the two disagree for
  // the whole of a turn before its first reply lands. Comparing dismissedSeq
  // against the session counter would reopen the modal for a question that no
  // longer exists.
  const currentReplySeq = signals.lastAgentReply?.seq ?? 0;
  const showModal =
    shown &&
    (signals.awaitingReply || signals.pausedIdle) &&
    signals.lastAgentReply !== null &&
    currentReplySeq > dismissedSeq &&
    !questionShowing &&
    !signals.ended;

  // Through `answer`, the SAME seam an ap:question sends through: the modal is
  // the fallback for that card, and a reply typed here must be indistinguishable
  // from one typed into it.
  //
  // Settled ONLY once the send resolved: a rejected reply leaves dismissedSeq
  // where it was, so the modal stays open holding the typed text rather than
  // closing over a message that never arrived.
  const handleModalReply = useCallback(
    async (text: string) => {
      await answer(text);
      setDismissedSeq(currentReplySeq);
    },
    [answer, currentReplySeq],
  );

  // Later settles the same reply without sending anything. The question is not
  // gone — it is still in the transcript, and the agent is still waiting — but
  // this view has stopped interrupting the viewer about it.
  const handleModalDismiss = useCallback(() => setDismissedSeq(currentReplySeq), [currentReplySeq]);

  // decisionError is the "Needs your decision" region's own failure line — a
  // decision-kind InteractionCard action failed or errored (see
  // useInteractionDecision's contract), OR a rejection the pipe reported
  // asynchronously on the session socket (see handleSessionFrame below).
  // Cleared on the next click so a stale failure from a previous attempt
  // doesn't linger under a fresh one still in flight.
  const [decisionError, setDecisionError] = useState<string | null>(null);
  const submitDecision = useInteractionDecision(ns, name, setDecisionError);
  const decide = useCallback(
    (requestRef: string, category: string, actionId: string) => {
      setDecisionError(null);
      submitDecision(requestRef, category, actionId);
    },
    [submitDecision],
  );

  // cardAttempts is a per-requestRef retry counter, bumped every time THIS
  // page's decision on that card is REJECTED (see handleSessionFrame below).
  // Folded into the card's own React key (`${requestRef}:${attempt}`) because
  // InteractionCard latches its own `pending` state the instant any action is
  // clicked and clears it only when `applied` arrives (see its doc comment) —
  // a rejection is neither a click nor a resolution, so without a key change
  // the card would sit disabled with every button unusable and no way to
  // decide again. Chat's own inline card doesn't have this problem: a
  // rejection there lands as a NEW timeline line beside the (still-disabled)
  // card, because the transcript is an append-only log and re-deciding from
  // the same card was never the point. This region has no timeline — the
  // card IS the only place to decide — so it has to come back to life
  // instead.
  const [cardAttempts, setCardAttempts] = useState<Record<string, number>>({});

  // handleSessionFrame is this region's only frame handler: an
  // interaction_decision_rejected naming a requestRef this page ISN'T
  // currently showing (already resolved, or a card rendered somewhere else)
  // is a no-op — there is nothing here to correct. For one that is showing,
  // it surfaces the SAME rejection text ChatView renders for its own inline
  // card (interactionRejectionText — shared rather than re-derived) and bumps
  // that card's attempt counter so its buttons come back.
  const handleSessionFrame = useCallback(
    (f: ChatFrame) => {
      if (f.type !== "interaction_decision_rejected") return;
      const inner = (f.payload as InteractionDecisionRejectedPayload).payload;
      if (!signals.pendingInteractions.some((r) => r.requestRef === inner.requestRef)) return;
      setDecisionError(interactionRejectionText(inner));
      setCardAttempts((prev) => ({ ...prev, [inner.requestRef]: (prev[inner.requestRef] ?? 0) + 1 }));
    },
    [signals.pendingInteractions],
  );
  useSessionFrames(handleSessionFrame);

  return (
    <ViewSessionProvider value={viewSession}>
      <BindingParamsProvider value={bindingParams}>
        <ActionsProvider value={actionsValue}>
          {/* A column that fills the host's content region. Both providers above
              are context-only and emit no DOM, so this element is what gives the
              decisions region, the progress strip, and the declared page a
              shared block to stack in. min-h-full (not h-full) so a tall
              declaration still grows and scrolls in the host's overflow
              container instead of being clipped to one viewport. */}
          <div className="flex min-h-full flex-col">
          {/* The default progress region, rendered ONLY when the declaration
              holds no ap:progress of its own. At the top of the column because
              it is a status strip for the page beneath it, not a card within it
              — and so its position does not move as the page's own content
              changes height. */}
          {!progressShowing && <DefaultProgress signals={signals} />}
          {/* liveError is the copy live.go authored on its "error" frame (Memory
              unconfigured, a failed snapshot read). Rendering it is the
              no-silent-errors rule's "surface a user-visible cause" arm: without
              it a viewer sees controls that never update and nothing saying why,
              while the server had already written the sentence. It is NOT a
              trust event — chrome auto-reveals for approvals, credential
              requests, identity questions, and hard render errors, and a
              degraded live channel is none of those. */}
          {liveError !== null && (
            <div className="px-4 pt-4 max-w-3xl mx-auto" data-testid="agent-ui-live-error">
              <Alert variant="destructive">
                <AlertTitle>Live updates unavailable</AlertTitle>
                <AlertDescription>{liveError}</AlertDescription>
              </Alert>
            </div>
          )}
          {/* "Needs your decision": a pending interaction_request (folded into
              signals.pendingInteractions by sessionSignals.ts) is the agent
              BLOCKED on the viewer, exactly the fact the chat's own inline
              InteractionCard exists to surface. This view has no timeline to
              render one into, so the SAME card renders here, above the
              declared page — never a second "approval component" — and posts
              through the SAME decision route every InteractionCard uses (the
              shared useInteractionDecision hook). Rendered only while shown:
              a hidden view answering nothing here is exactly the point of
              hidden, not shown. */}
          {shown && signals.pendingInteractions.length > 0 && (
            <section
              data-testid="agent-ui-decisions"
              aria-label="Needs your decision"
              className="flex flex-col gap-2 border-b border-border bg-card/60 px-3 py-2"
            >
              <p className="text-xs font-medium text-muted-foreground">Needs your decision</p>
              {signals.pendingInteractions.map((req) => (
                // The attempt count in the key is what makes a rejected click
                // remountable — see cardAttempts' own doc comment above.
                <InteractionCard key={`${req.requestRef}:${cardAttempts[req.requestRef] ?? 0}`} request={req} onDecision={decide} />
              ))}
              {decisionError && (
                <p role="alert" className="text-xs text-destructive">
                  {decisionError}
                </p>
              )}
            </section>
          )}

          <ContentRegion declaration={declaration} states={states} refreshing={refreshing} updatedHooks={updatedHooks} waiting={waiting} onRenderError={handleRenderError} />

          {/* The reply fallback is the platform's, not the agent's, so no
              declared node can reach or suppress it. It renders in a portal, so
              its position in this column decides nothing about where it
              appears. */}
          <AgentReplyModal
            open={showModal}
            variant={signals.awaitingReply ? "asked" : "paused"}
            text={signals.lastAgentReply?.text ?? ""}
            replySeq={currentReplySeq}
            onReply={handleModalReply}
            onDismiss={handleModalDismiss}
          />
          </div>
        </ActionsProvider>
      </BindingParamsProvider>
    </ViewSessionProvider>
  );
}
