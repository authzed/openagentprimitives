import { useEffect, useReducer, useRef, type ReactNode } from "react";
import {
  MessageCircle,
  ShieldCheck,
  History,
  Moon,
  Archive,
  CircleUserRound,
  PanelTopClose,
  PanelTopOpen,
  Sparkles,
} from "lucide-react";
import { Button } from "@ap/design";

// sessionOriginCopy turns the shell's selected-session SessionOrigin
// (agentui.Resolution.Branch, forwarded by pkg/web/webui/sessions' selectedView)
// into human disclosure copy. Per the design spec: "the mechanism is
// seamless; the fact is disclosed" — but disclosure must stay human, never
// echo the operational vocabulary (CRD kinds, phase strings, "AgentSession",
// "WakeEligible", the raw branch string itself, ...).
//
// All THREE ladder branches reach the browser: the shell selects a read-only
// transcript for an ended session rather than answering the whole page with an
// error, so "ended" is a disclosure this component makes, not a case the
// server absorbs before mount. The default branch below stays defensive
// against a value no case matches, and is deliberately generic rather than
// echoing whatever unrecognized string it was given.
//
// The "asleep" and "ended" copy both state the session's CONDITION and stop
// there; neither claims an action the platform has not taken. Opening this
// page is a read: it stamps no wake-requested-at annotation (the writers that
// do are channelsd's inbound Deliver path and the runner's own status writer,
// reached by SENDING a message), and it starts no replacement session — the
// chat escape hatch and the start control beside this line are how a viewer
// does either.
function sessionOriginCopy(origin: string, endedReason?: string): { text: string; Icon: typeof History } {
  switch (origin) {
    case "attached":
      return { text: "Resumed your session.", Icon: History };
    case "asleep":
      return { text: "Your session is asleep; it resumes when you send a message.", Icon: Moon };
    case "ended":
      // A session that never booted has no transcript to explain itself, so
      // the reason the platform recorded is spoken here or nowhere. The plain
      // sentence stands alone when there is no reason: a colon followed by
      // nothing is not a disclosure.
      //
      // When there IS a reason it gets the last word, and the generic offer of
      // a new session is dropped. The reason is written for the person and
      // already says what to do about it — a cap refusal says to close a
      // workshop BEFORE starting another — so appending "You can start a new
      // one." contradicts the sentence it follows. Nothing is taken away by
      // dropping it: an ended session's page renders its own start-a-
      // replacement control regardless (SessionShell's replacement region).
      return {
        text: endedReason
          ? `This session has ended: ${endedReason}`
          : "This session has ended. You can start a new one.",
        Icon: Archive,
      };
    default:
      return { text: "Connected to your session.", Icon: History };
  }
}

// isCollapseRequested is defensive the same way sessionOriginCopy's default
// branch is: AgentUIChrome.InitialState is CRD-enum-validated to
// "expanded"|"collapsed"|absent server-side, but this component treats any
// value other than the literal "collapsed" (including an unrecognized
// string) as the non-collapsing default rather than trusting the input.
function isCollapseRequested(state?: string): boolean {
  return state === "collapsed";
}

// TrustEventKind enumerates the categories Decision 3 names as always
// auto-revealing chrome: "an approval addressed to the viewer, a credential
// request, an identity question, a hard error."
export type TrustEventKind = "approval_addressed_to_viewer" | "credential_request" | "identity_question" | "hard_error";

// TrustEvent is the seam every event source drives chrome's auto-reveal
// through. The live sources today are the selected view's own: AgentUIView
// lifts a content-region render failure into a `hard_error` and an approval
// addressed to the viewer into an `approval_addressed_to_viewer`, and reports
// both UP to the shell (see its ChromeSignals). `id` is the event's identity:
// it disambiguates repeat occurrences of the same `kind`, so a second, later
// event of a kind the viewer already dismissed still re-triggers the reveal,
// and it is what Chrome remembers having revealed for (see
// ChromeProps.trustEvents).
export interface TrustEvent {
  kind: TrustEventKind;
  id: string;
}

// ApprovalNotice is one pending approval the chrome-owned approval surface
// renders. It is presentation only: Chrome decides nothing about which
// approvals exist or who they are addressed to, exactly as it decides
// nothing about trust events — both arrive as props, derived by SessionShell
// from the selected view's reported signals and from the listed sessions'
// own awaiting-a-human flags. `id` is stable per underlying request (the
// server's requestId when known), so React's list reconciliation keys on the
// same fact Chrome's own trust-event latch does.
export interface ApprovalNotice {
  id: string;
  label: string;
  addressedToViewer: boolean;
}

// SiblingView names one of the two views the shell can show for a selected
// session, spelled as the `?view=` value that selects it. Chrome owns the
// sibling-view switch, so it owns exactly this vocabulary and no more — it
// learns nothing about what either view actually renders.
export type SiblingView = "chat" | "ui";

// sessionViewHref addresses one sibling view of the session the viewer is
// already on: the same `?session=` value, plus the `&view=` that selects the
// sibling. Built here and nowhere else, so the two switch controls cannot
// drift into two different address shapes, and taken as arguments (rather
// than read off possibly-absent props) so an address can only ever be built
// for a session that IS selected.
// viewTabClassName styles one half of the sibling-view switch. The shown view
// carries a filled background and foreground; the other reads as followable
// but quiet.
//
// The visual state is derived from the SAME `current` boolean that drives
// aria-current, in one expression, so the two cannot disagree — a filled tab
// that is not aria-current would tell a sighted viewer and a screen-reader
// viewer different things about where they are.
// followTab turns a tab click into a local view switch instead of a
// navigation — but only when the click is an ordinary one and a host actually
// supplied a handler.
//
// The tabs stay real anchors with real hrefs on purpose. Middle-click,
// ctrl/cmd-click and "open in new tab" must keep working, and they only do so
// if the browser sees a link it is allowed to follow; a <button> that calls
// pushState looks like a tab and cannot be opened in a new one. So the
// modifier keys and non-primary buttons are handed straight back to the
// browser, and preventDefault is called for nothing else.
//
// With no onSelectView the anchor is left entirely alone, which degrades to
// the old full-navigation behaviour rather than to a dead control.
function followTab(
  e: React.MouseEvent<HTMLAnchorElement>,
  view: SiblingView,
  href: string,
  onSelectView?: (view: SiblingView, href: string) => void,
): void {
  if (!onSelectView) return;
  if (e.defaultPrevented) return;
  if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  e.preventDefault();
  onSelectView(view, href);
}

// refreshTabHref rewrites a tab's href from the CURRENT address bar, on
// pointer-down, just before the browser acts on it.
//
// The href written at render goes stale the moment a viewer touches a filter:
// AgentUIView mirrors binding parameters into the query with replaceState,
// which by design re-renders nothing. A plain click is unaffected — its
// handler builds the address itself — but middle-click and ctrl-click never
// reach that handler, and would open the sibling view stripped of the filters
// the viewer is looking at. Since keeping those gestures working is the entire
// reason these tabs are anchors rather than buttons, they have to land on the
// same view a plain click does.
//
// mousedown precedes both the browser's navigation and the click handler for
// every button, which is what makes one line here cover all three paths.
function refreshTabHref(
  e: React.MouseEvent<HTMLAnchorElement>,
  ns: string,
  name: string,
  view: SiblingView,
): void {
  e.currentTarget.href = sessionViewHref(ns, name, view);
}

function viewTabClassName(current: boolean): string {
  const base =
    "inline-flex items-center gap-1.5 rounded px-2.5 py-1 text-[12px] font-medium transition-colors " +
    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring";
  return current
    ? `${base} bg-secondary text-secondary-foreground`
    : `${base} text-muted-foreground hover:bg-accent/50 hover:text-foreground`;
}

// sessionViewHref is a sibling tab's address: the CURRENT query with only the
// session and view keys reasserted.
//
// It carries the rest of the query forward deliberately. Rebuilding the string
// from scratch dropped every other key the address bar held — in particular
// the `p.*` binding parameters AgentUIView mirrors there — so following a tab
// erased them, and switching back re-seeded the view from a URL that no longer
// described the filters the viewer had chosen. The dashboard came back
// unfiltered, and nothing about looking at the transcript for a moment
// suggests it should.
//
// `new`, `ns`, `agentClass` and `prompt` are the four keys dropped: together
// they are the picker's one-shot request — open the dialog, preselect an
// agent, prefill a message — which the shell consumes once at mount
// (SessionShell's `startRequest`). Carrying any of them would put that
// request into every tab href, so reloading from one, or simply following it,
// would reopen a dialog the viewer already dismissed or re-apply a
// preselection to a session they navigated away from.
//
// Stale parameters are not a hazard on the receiving side: reconcileParams
// filters an incoming map against the declaration that is actually rendering,
// so a key the current view does not declare is dropped rather than sent.
function sessionViewHref(ns: string, name: string, view: SiblingView): string {
  const q = new URLSearchParams(typeof window === "undefined" ? "" : window.location.search);
  q.delete("new");
  q.delete("ns");
  q.delete("agentClass");
  q.delete("prompt");
  q.set("session", `${ns}/${name}`);
  q.set("view", view);
  return `/sessions?${q.toString()}`;
}

// ChromeSelection is the session-scoped half of Chrome's props: EITHER a
// whole selection or none of it. A union rather than five independent
// optionals, because a HALF selection is not a state this component can
// render honestly — `ns` and `name` without `sessionOrigin` would fall
// through sessionOriginCopy's defensive default and disclose "Connected to
// your session." for a real session whose actual condition is asleep or
// ended. That is a wrong statement about a live session, and the cheapest
// place to rule it out is the type: neither caller can express it.
export type ChromeSelection =
  | {
      ns: string;
      name: string;
      sessionOrigin: string;
      // endedReason is why an ended session ended, when it ended in failure —
      // the server's own sentence, forwarded verbatim. OPTIONAL rather than
      // part of the whole-or-nothing rule above: a live session has no reason
      // to carry, and an ended one that simply finished has none worth
      // disclosing, so absent is the ordinary case rather than half a
      // selection.
      endedReason?: string;
      // currentView reports which sibling view the content region is already
      // showing, so that control can be marked aria-current="page". ONE value
      // rather than a boolean per control: two independent booleans can both
      // be true, and "both of these are the current view" is not a state the
      // shell can produce or this component can render honestly. Absent means
      // neither — the content region is showing something that is not a
      // sibling view (the inline unavailable card), so no control is "here".
      //
      // A control that IS current keeps its href and stays rendered: a control
      // that disappears when you reach its destination makes the region's
      // contents depend on which sibling view is showing, which is exactly the
      // shifting chrome the collapse discipline forbids.
      currentView?: SiblingView;
      // offersAgentUI reports that this session has an agent-defined view to
      // switch BACK to, so the switch's agent-view half renders. Without it
      // the chat control is a one-way trip: the viewer who follows it is left
      // on a page whose only marked control says "you are here", with no
      // affordance back to the view they came from and nothing but the URL bar
      // to recover with.
      offersAgentUI?: boolean;
      // onSelectView switches the shown view LOCALLY, instead of letting the
      // browser follow the tab's href. Optional: a host that omits it leaves
      // the tabs as ordinary links and gets the old full-navigation behaviour,
      // which is slow rather than broken.
      //
      // Chrome owns no view state itself — the shell does, because the content
      // region a tab selects is the shell's to render. Chrome reports the
      // click and is told what to mark current.
      onSelectView?: (view: SiblingView, href: string) => void;
    }
  | {
      ns?: never;
      name?: never;
      sessionOrigin?: never;
      endedReason?: never;
      currentView?: never;
      offersAgentUI?: never;
      onSelectView?: never;
    };

// ChromeBase is everything that does not depend on a session being selected.
// Every region it describes renders unconditionally, because the
// never-suppressible rule is about COLLAPSE, not about whether a session is
// open.
interface ChromeBase {
  // subject is ALREADY display-form (identity.DecodeForDisplay, server-side)
  // — this component never sees, and never needs to understand, the raw
  // canonical "user:<base64...>" SpiceDB subject.
  subject: string;
  // initialState is the UI's REQUESTED chrome visibility
  // (AgentUISpec.Chrome.InitialState, forwarded verbatim by the shell's
  // selected-session props) — a REQUEST only, honored at first paint and any
  // time the viewer has not yet made their own explicit choice. Per
  // Decision 3's precedence (user's explicit toggle > UI's requested initial
  // state > platform default, shown), the viewer's own toggle click
  // permanently outranks this prop for the rest of the mount.
  initialState?: string;
  // trustEvents carries the currently-active trust events from EVERY source
  // feeding this chrome — the platform's own (approvals addressed to the
  // viewer, credential requests, identity questions) and the page's own
  // derived ones (a content-region render failure). Any event whose `id` this
  // component has not already revealed for forces chrome visible, regardless
  // of the current collapse state — including a state the viewer explicitly
  // chose.
  //
  // A LIST, not a single "current event", because the sources are independent
  // and the design ranks none of them: "any trust event ... auto-reveals it".
  // Reducing them to one slot before dispatch — picking a winner with `??` —
  // makes whichever source loses that pick unable to reveal at all for as long
  // as the winner is present, which is the same suppression in a narrower
  // form. Sources may hold `null`/`undefined` while they have nothing active;
  // those are skipped, and a source CLEARING never reveals anything.
  trustEvents?: readonly (TrustEvent | null | undefined)[];
  // approvals carries the currently pending approvals this chrome should
  // list, already filtered and human-labeled by the caller. Chrome renders
  // them and makes no authorization or filtering decision of its own — the
  // same posture it already has for trustEvents. Absent or empty means "none
  // active right now", not "there are none": see the approval-surface
  // region's own comment below for why the empty-state copy does not assert
  // a checked state.
  approvals?: readonly ApprovalNotice[];
  // headerActions is a chrome region whose CONTENTS this component does not
  // understand, exactly as `sidebar` is: the host supplies session-scoped
  // controls (the session-details panel today) and Chrome only places them. It
  // sits in the header's right-hand group, beside the escape hatch, because
  // that is where a control that acts on the CURRENT session belongs.
  //
  // A slot rather than a fixed set of buttons, because Chrome must not grow a
  // vocabulary of session actions: it would then have to decide which ones
  // apply, which is a capability question this component never asks.
  //
  // The collapse discipline applies to the slot's own CONTAINER — it carries
  // `hidden` like every other region and stays mounted. It does NOT reach
  // content the host renders through a portal (a Radix Dialog's own body, for
  // instance), which lands on document.body outside this subtree entirely. That
  // escape costs nothing structurally — portaled content is still the host's,
  // still unreachable by the content region — but a host that opens a modal
  // from here owns closing it, because collapsing chrome will not.
  headerActions?: ReactNode;
  // sidebar is a chrome region whose CONTENTS this component does not
  // understand — the session list is passed in, so Chrome learns nothing
  // about what a session is. It renders as a DOM sibling of `children`'s
  // container under the same `hidden={collapsed}` discipline as every other
  // region, which is what makes the list unreachable by the content region:
  // an agent's declared node tree cannot cover or remove a sibling it does
  // not own the subtree of.
  sidebar?: ReactNode;
  children: ReactNode;
}

export type ChromeProps = ChromeBase & ChromeSelection;

// ChromeVisibility is the whole of Chrome's visibility state. The two latches
// are what make the precedence table hold over time rather than only at first
// paint:
//
//   - userToggled: once the viewer makes their own explicit choice, the UI's
//     requested state must never win again for this mount. Precedence is
//     "user toggle > UI request", not "whichever was set most recently".
//   - trustRevealed: once a trust event has revealed chrome, the UI's requested
//     state must not un-reveal it either. Without this latch a UI that keeps
//     requesting `collapsed` re-hides chrome under a live trust event for any
//     viewer who has not toggled — which is exactly the "hidden means absent"
//     outcome the design's chrome-authority decision calls non-negotiable.
//
// A trust reveal is NOT the viewer's own choice, so it does not set
// userToggled: the viewer's next manual toggle after a reveal is still
// authoritative, and re-collapsing after handling the event is legitimate.
interface ChromeVisibility {
  collapsed: boolean;
  userToggled: boolean;
  trustRevealed: boolean;
}

// ChromeEvent enumerates the three things that can change chrome visibility.
// Modelling them as events (rather than as two effects racing to write one
// useState) is what puts the precedence table in a single switch: whether a UI
// request still has standing is decided in one place, against the latches,
// instead of being an ordering property of the effects that fire.
type ChromeEvent = { type: "ui_requested"; collapsed: boolean } | { type: "user_toggled" } | { type: "trust_event" };

function initialVisibility(requested?: string): ChromeVisibility {
  return { collapsed: isCollapseRequested(requested), userToggled: false, trustRevealed: false };
}

function visibilityReducer(state: ChromeVisibility, event: ChromeEvent): ChromeVisibility {
  switch (event.type) {
    case "ui_requested":
      // Returning the SAME object (not an equal one) is load-bearing: this
      // event is re-dispatched on mount with the value the initializer already
      // used, and React bails out of a re-render only on referential equality.
      if (state.userToggled || state.trustRevealed || state.collapsed === event.collapsed) {
        return state;
      }
      return { ...state, collapsed: event.collapsed };
    case "user_toggled":
      // Clearing trustRevealed keeps the latch scoped to "undo by a UI
      // request": the viewer has now seen and acted on the reveal, so it is
      // their own toggle — recorded here — that outranks everything after.
      return { collapsed: !state.collapsed, userToggled: true, trustRevealed: false };
    case "trust_event":
      return { ...state, collapsed: false, trustRevealed: true };
  }
}

// Chrome is the platform-owned wrapper around one content region
// (`children`) whose contents the shell chose server-side. Every region
// below — identity, the session-origin disclosure, the session list
// (`sidebar`), the sibling-view switch, the approval surface, the collapse
// toggle — is a DOM sibling of `children`'s container, never a descendant of
// it: the content region has no way to remove, cover, or reach into these
// elements because it does not own the subtree they live in. That structural
// separation is what "the content region can neither draw over nor suppress"
// these regions means in practice, and it holds whether the content region
// is an agent's declared node tree or a chat transcript.
//
// Collapse ("hideable, never suppressible") reduces those regions with the
// native `hidden` attribute rather than conditional rendering: every testid
// below stays mounted in every visibility state, so a collapsed chrome is
// still a chrome — just a visually reduced one — never an absent one.
export function Chrome({
  ns,
  name,
  subject,
  sessionOrigin,
  endedReason,
  currentView,
  offersAgentUI,
  onSelectView,
  initialState,
  trustEvents,
  approvals,
  headerActions,
  sidebar,
  children,
}: ChromeProps) {
  // ChromeSelection makes a half-selection unrepresentable, so testing one
  // field answers for all of them. Carried as the PAIR rather than as a
  // boolean so every address below is built from values the type has already
  // proven present — a `${ns}/${name}` template evaluated outside this guard
  // renders the literal "undefined/undefined" into an href nobody intended.
  const selection = ns !== undefined && name !== undefined ? { ns, name } : null;
  const origin = sessionOriginCopy(sessionOrigin ?? "", endedReason);
  const [visibility, dispatch] = useReducer(visibilityReducer, initialState, initialVisibility);
  const collapsed = visibility.collapsed;

  // The UI's request is re-offered whenever it changes value; the reducer
  // decides whether it still has standing (see visibilityReducer). On mount it
  // re-offers the value the initializer already used, which the reducer
  // answers with the identical state object, so React bails out of the
  // re-render.
  useEffect(() => {
    dispatch({ type: "ui_requested", collapsed: isCollapseRequested(initialState) });
  }, [initialState]);

  // revealedFor remembers which trust-event ids have already triggered a
  // reveal. It is what lets every source act independently: a re-render
  // carrying the same events is inert, while a genuinely new event reveals no
  // matter which source produced it, no matter which other sources are
  // currently holding an event, and no matter that the viewer collapsed chrome
  // again in between. Identity is the event's `id`, not the object — a second,
  // later event of a kind the viewer already dismissed carries a new id and so
  // reveals again.
  const revealedFor = useRef<Set<string>>(new Set());
  const activeIds = (trustEvents ?? []).flatMap((e) => (e ? [e.id] : []));

  useEffect(() => {
    const fresh = activeIds.filter((id) => !revealedFor.current.has(id));
    if (fresh.length === 0) return;
    for (const id of fresh) revealedFor.current.add(id);
    // One dispatch however many arrived together: the reducer's answer to "a
    // trust event happened" does not depend on how many did.
    dispatch({ type: "trust_event" });
    // Depending on the joined ids rather than on activeIds (a fresh array every
    // render) keeps this from re-running when nothing about the event set
    // changed. The body reads activeIds, which is derived from exactly that.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeIds.join("|")]);

  function handleToggle() {
    dispatch({ type: "user_toggled" });
  }

  return (
    <div className="h-screen flex flex-col bg-background text-foreground">
      <header className="flex items-center gap-2.5 px-3 py-2 min-h-[52px] bg-card border-b border-border">
        {/* The collapse toggle leads the header, directly above the session
            list it sits over — the region whose column it shares. It is the
            one control NOT under `hidden`, because it is the way back from
            collapsed: putting it behind its own effect would strand a viewer
            in a chrome-less shell. */}
        <Button
          type="button"
          variant="ghost"
          size="icon"
          aria-expanded={!collapsed}
          aria-label={collapsed ? "Show chrome" : "Hide chrome"}
          data-testid="session-shell-chrome-toggle"
          onClick={handleToggle}
        >
          {collapsed ? (
            <PanelTopOpen className="h-4 w-4" aria-hidden="true" />
          ) : (
            <PanelTopClose className="h-4 w-4" aria-hidden="true" />
          )}
        </Button>
        {/* The session's own status keeps the reading position: it is about
            what the viewer is looking at, so it stays beside the content, not
            out at the identity's edge. */}
        {selection && (
          <div className="flex items-center gap-2 min-w-0" hidden={collapsed}>
            <origin.Icon className="h-3.5 w-3.5 text-muted-foreground shrink-0" aria-hidden="true" />
            {/* title carries the same sentence the span truncates: an ended
                session's reason can be a full sentence, and a disclosure the
                viewer cannot finish reading is not one. */}
            <span
              className="text-[13px] text-muted-foreground truncate"
              title={origin.text}
              data-testid="session-shell-session-origin"
            >
              {origin.text}
            </span>
          </div>
        )}
        <div className="ml-auto flex items-center gap-2 shrink-0">
          {/* Host-supplied session controls. The container is under `hidden`
              like every other region, and mounted whenever the host passes
              anything — Chrome makes no judgement about which controls apply.
              See headerActions for what the discipline does and does not cover. */}
          {headerActions && (
            <div className="flex items-center gap-1" data-testid="session-shell-header-actions" hidden={collapsed}>
              {headerActions}
            </div>
          )}
          {/* Identity sits at the far edge, away from the session's own status
              and controls: it answers "who am I acting as", which is true of
              the whole shell rather than of the session in front of you, and
              it is the one thing here a viewer checks rarely and deliberately
              rather than while working. */}
          <div className="flex items-center gap-2 min-w-0" hidden={collapsed}>
            <CircleUserRound className="h-4 w-4 text-muted-foreground shrink-0" aria-hidden="true" />
            <span className="text-[13px] font-medium truncate" data-testid="session-shell-identity">
              Acting as {subject}
            </span>
          </div>
        </div>
      </header>

      {/* Approval surface: a persistent, chrome-owned region the content
          region cannot cover or remove. Its RESERVATION is the guarantee — the
          element is always in the DOM at a fixed minimum height, so an arriving
          approval never reflows the page out from under a click, and nothing
          the content region renders can cover or remove it.
          Chrome makes no decision about which approvals exist or who they are
          addressed to (see ApprovalNotice's own doc comment).

          When empty it now renders NOTHING rather than "Approval requests
          appear here". That placeholder was answering a question nobody asks
          on a healthy page, in the most persistent spot on screen, and it read
          as state ("something is expected of me") when it was only naming a
          region. Note what did NOT change: the region still asserts no checked
          state. Saying "No pending approvals" would claim a verification this
          component has not made — it renders what its caller derived, and an
          empty prop on one render says nothing about the next. Silence claims
          nothing, which is the honest answer.

          The bar also carries the sibling-view switch at its trailing edge:
          both are per-session chrome that must survive whatever the content
          region does, and the switch reads better next to the session's own
          surfaces than beside the shell-wide identity and start controls. */}
      <div
        className="flex items-center gap-2 px-3 py-1 min-h-[36px] border-b border-border bg-card/60 text-[12px] text-muted-foreground"
        data-testid="session-shell-approval-surface"
        hidden={collapsed}
      >
        {approvals && approvals.length > 0 && (
          <>
            <ShieldCheck className="h-3.5 w-3.5 shrink-0" aria-hidden="true" />
            <ul className="flex flex-col gap-0.5">
              {approvals.map((a) => (
                <li key={a.id}>{a.label}</li>
              ))}
            </ul>
          </>
        )}

        {/* The sibling-view switch, as a segmented control. Both halves switch
            the VIEW; neither leaves the shell — following one re-renders a
            single region rather than tearing down chrome, the session list and
            this bar to rebuild them on another page.

            It is a SWITCH, not a one-way hatch: the agent-view half renders for
            every session whose agent offers one, INCLUDING while the transcript
            is shown, so a viewer who follows the chat control always has the way
            back in the same place they left it. The agent-view half is the
            conditional one, because a session whose agent declares no view has
            nothing to switch to; the chat half is unconditional, because every
            session has a transcript.

            Rendered as tabs rather than two buttons because they are one choice
            with two states, not two independent actions: the shown view carries
            a filled background and aria-current, so "where am I" is answered by
            the control itself instead of by remembering which one was pressed.
            A lone Chat tab (no agent UI declared) still renders current — a
            one-tab group reads as a label for what is shown, which is true. */}
        {selection && (
          <div
            className="ml-auto flex items-center gap-0.5 rounded-md border border-border bg-background/60 p-0.5 shrink-0"
            data-testid="session-shell-view-tabs"
          >
            {offersAgentUI && (
              <a
                href={sessionViewHref(selection.ns, selection.name, "ui")}
                aria-current={currentView === "ui" ? "page" : undefined}
                data-testid="session-shell-agent-view-switch"
                className={viewTabClassName(currentView === "ui")}
                onMouseDown={(e) => refreshTabHref(e, selection.ns, selection.name, "ui")}
                onClick={(e) => followTab(e, "ui", sessionViewHref(selection.ns, selection.name, "ui"), onSelectView)}
              >
                <Sparkles className="h-3.5 w-3.5" aria-hidden="true" />
                Agent view
              </a>
            )}
            <a
              href={sessionViewHref(selection.ns, selection.name, "chat")}
              aria-current={currentView === "chat" ? "page" : undefined}
              data-testid="session-shell-chat-escape-hatch"
              className={viewTabClassName(currentView === "chat")}
              onMouseDown={(e) => refreshTabHref(e, selection.ns, selection.name, "chat")}
              onClick={(e) => followTab(e, "chat", sessionViewHref(selection.ns, selection.name, "chat"), onSelectView)}
            >
              <MessageCircle className="h-3.5 w-3.5" aria-hidden="true" />
              Chat
            </a>
          </div>
        )}
      </div>

      <div className="flex-1 min-h-0 flex">
        {/* The session list is a chrome region, not content: it is a SIBLING
            of the content region below, under the same collapse discipline,
            so whatever the content region renders can neither cover it nor
            take it away. Chrome does not know what it holds — see `sidebar`.

            A host that passes no sidebar gets the region at zero width, NOT
            an unmounted one: the never-suppressible rule is about the element
            staying in the DOM, and a bordered 16rem column with nothing in it
            is a layout defect, not a chrome guarantee. Width is the only
            thing that varies. */}
        <aside
          className={
            sidebar
              ? "w-64 shrink-0 min-h-0 overflow-y-auto border-r border-border bg-card/40"
              : "w-0 shrink-0 min-h-0 overflow-hidden"
          }
          data-testid="session-shell-sidebar"
          hidden={collapsed}
        >
          {sidebar}
        </aside>

        {/* A flex COLUMN, so a sibling rendered above the chat (the ended-session
            "start a new session" strip) takes its own height and the chat view
            takes the rest via flex-1. With `h-full` on the chat and a plain
            block here, the two summed to more than the box and the whole
            region scrolled by exactly the strip's height. */}
        <main className="flex min-h-0 flex-1 flex-col overflow-y-auto" data-testid="session-shell-content-region">
          {children}
        </main>
      </div>
    </div>
  );
}
