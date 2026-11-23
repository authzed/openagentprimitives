import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { MoreHorizontal } from "lucide-react";
import { Alert, AlertDescription, AlertTitle, Button } from "@ap/design";
import {
  AgentUIView,
  EMPTY_CHROME_SIGNALS,
  sameChromeSignals,
  type ChromeSignals,
} from "../../agentui/ui/AgentUIView";
import { ChatView } from "../../chat/ui/ChatView";
import { SessionSocketProvider, useSessionFrames } from "../../chat/ui/SessionSocket";
import type { InteractionRequestPayload, UserMessagePayload } from "../../chat/ui/types";
import { Chrome, type ApprovalNotice, type ChromeSelection, type SiblingView } from "./Chrome";
import { EmptyDashboardExplanation, NewSessionDialog, NoAgentsDialog, StartReplacementButton } from "./NewSessionDialog";
import { SessionInfoPanel } from "./SessionInfoPanel";
import { SessionList } from "./SessionList";
import { StartupLine } from "./StartupLine";
import { GoalSessionAlerts } from "./GoalSessionAlerts";
import { formatTabTitle } from "./tabTitle";
import { useAwayNotifications } from "./useAwayNotifications";
import { useTabActivity } from "./useTabActivity";
import type { ListNotices, SelectedView, SessionRow, SessionsAPIResponse, ShellBootstrapProps } from "./types";

export type { ShellBootstrapProps } from "./types";

// SESSION_LIST_POLL_MS is how often an open tab re-reads the session list.
// The cost per interval per open tab is one SpiceDB LookupResources plus a
// bounded (listGetConcurrency) fan-out of AgentSession Gets — the exact work
// the page's own first paint already did — so it is a real, repeating server
// cost, not a free refresh. Visible tabs poll every ten seconds so a newly
// created goal session's short approval window is not missed. Hidden tabs
// retain the thirty-second cadence; returning to the tab refreshes immediately.
const SESSION_LIST_POLL_MS = 10_000;
const BACKGROUND_SESSION_LIST_POLL_MS = 30_000;

// crossSessionApprovalNotices turns the listed sessions' own awaiting-a-human
// flags into approval-surface lines for sessions the viewer is NOT currently
// looking at. The shell owns the approval surface for every listed session,
// not only the selected one — a session waiting on a human is worth seeing
// while the viewer is reading a different one.
//
// These carry addressedToViewer:false and are deliberately NOT fed to
// trustEvents: the phase says a human is needed, not WHICH human, and
// auto-revealing chrome for an approval that may belong to someone else is
// the cry-wolf failure Chrome's own ApprovalNotice doc warns about. Only the
// SELECTED session's live lifecycle supplies a trust event.
//
// The copy names the agent, never the session's control-plane identifiers:
// the row itself is where the viewer goes to act on it.
function crossSessionApprovalNotices(sessions: readonly SessionRow[], selected: SelectedView | undefined): ApprovalNotice[] {
  const out: ApprovalNotice[] = [];
  for (const s of sessions) {
    if (!s.awaitingHuman) continue;
    if (selected && s.ns === selected.ns && s.name === selected.name) continue;
    out.push({
      id: `session:${s.ns}/${s.name}`,
      label: `${s.title} is waiting on a person in another session.`,
      addressedToViewer: false,
    });
  }
  return out;
}

// SelectedContent renders whichever view the SERVER chose for the selected
// session. The browser dispatches on `view.kind` and asks no capability
// question of its own — viewFor (view.go) already answered it, under the
// viewer's own subject.
//
// It also holds the session's one live socket, around BOTH views rather than
// inside either — see the provider at the end of this component.
function SelectedContent({
  selected,
  shown,
  ended,
  onChromeSignals,
  onUserSend,
  onAgentReply,
  onAwaitingDecision,
  onSessionEnded,
  uiEverShownRef,
}: {
  selected: SelectedView;
  // shown is which sibling the viewer is looking at NOW — the shell's live
  // answer, not selected.view.kind, which only seeds it. The two diverge the
  // moment a tab is followed: switching is local state plus a history entry,
  // not a navigation, so the bootstrap prop still describes the view the
  // SERVER rendered while this says what is on screen.
  shown: SiblingView;
  // ended is the SHELL's live answer for the selected session, not
  // selected.sessionOrigin: the bootstrap prop is frozen at page load and a
  // session that ends while the viewer is watching must not leave this view
  // writable — see SessionShell's own `ended`.
  ended: boolean;
  onChromeSignals: (signals: ChromeSignals) => void;
  onUserSend: () => void;
  onAgentReply: (text: string) => void;
  onAwaitingDecision: (lead: string) => void;
  onSessionEnded: () => void;
  // uiEverShownRef records whether the agent view has been opened at least
  // once for the CURRENT selection. Owned by the shell rather than this
  // component so it resets with the selection: a ref declared here would
  // survive a session change (this component is not remounted for one) and
  // would keep the previous session's view mounted alongside the new one.
  uiEverShownRef: React.MutableRefObject<boolean>;
}) {
  // notice is ADDITIVE — it sits beside content that IS shown, unlike
  // `unavailable`, which replaces it. Rendered above whichever view follows
  // so the answer to "you asked for something this session cannot offer"
  // arrives before the substitute content, not after it.
  const notice = selected.view.notice ? (
    <div className="p-4 max-w-3xl mx-auto" data-testid="session-shell-view-notice">
      <Alert>
        <AlertTitle>{selected.view.notice.title}</AlertTitle>
        <AlertDescription>{selected.view.notice.message}</AlertDescription>
      </Alert>
    </div>
  ) : null;

  // The agent view is MOUNTED ONCE and then KEPT — hidden when the viewer
  // looks at the transcript, never torn down.
  //
  // Unmounting discarded everything it holds: the resolved bindings, the live
  // socket, the declaration the agent had composed, the parameters in flight,
  // and the working state of any question already asked. Coming back re-opened
  // the socket and re-ran every binding — a full server-side fan-out — only to
  // arrive at the page that had just been thrown away. A glance at the
  // transcript and back cost a reload each way.
  //
  // Kept LAZILY: uiEverShownRef means a viewer who never opens the agent view
  // never pays for one. Once opened it stays, because holding it costs a
  // hidden subtree and dropping it costs that round trip.
  //
  // Hidden with a class rather than by returning null, and that distinction is
  // the entire point: `hidden` keeps the component mounted with its state and
  // its socket intact, where null is the teardown this replaces.
  if (shown === "ui") uiEverShownRef.current = true;
  const ui = selected.view.ui;
  const agentView =
    ui && uiEverShownRef.current ? (
      <div className={shown === "ui" ? "h-full" : "hidden"} data-testid="session-shell-ui-region">
        {/* The notice belongs to whichever view is ON SCREEN, so it renders
            inside this subtree only while the agent view is the one shown —
            otherwise a hidden region would hold the page's only copy of it. */}
        {shown === "ui" && notice}
        {/* `shown` is told, not inferred: this subtree stays MOUNTED behind
            `hidden` while the transcript is up, and the view's reply modal
            renders through a portal to document.body, where no ancestor's
            display:none reaches it. Without this the hidden view would drop a
            focus-trapping dialog over the transcript. */}
        <AgentUIView
          ns={ui.ns}
          name={ui.name}
          declaration={ui.declaration}
          agentParams={ui.agentParams}
          shown={shown === "ui"}
          onChromeSignals={onChromeSignals}
          onSessionEnded={onSessionEnded}
        />
      </div>
    ) : null;

  // Dispatch on what the viewer is LOOKING at, not on what the server
  // rendered. `unavailable` is not one of the cases: it is not a place a tab
  // can send you, it is what the agent-view arm falls back to when its payload
  // could not be resolved — folded into that arm below so the switch has
  // exactly the two states the tabs have.
  //
  // A function rather than the component's own return, because what it returns
  // is mounted INSIDE the session socket below — which must span both views,
  // not live inside whichever arm this picks.
  function shownContent() {
    switch (shown) {
      case "ui":
        // agentView is NOT returned here — it is rendered at a fixed position
        // outside this switch (see the provider below). Nothing but the
        // fallback for a missing payload belongs to this arm.
        return ui ? null : (
          // No agent-view payload. Two different situations land here and the
          // server already told them apart: `unavailable` carries the disclosure
          // for a UI that is DECLARED but broken (missing object, unreconciled,
          // Valid=False), which selecting the tab is supposed to reveal rather
          // than hide. Only when there is no such message is this the wire the
          // server cannot produce — logged as well as rendered, because the card
          // tells the viewer what to do and the console is the only place the
          // cause survives.
          <>
            {notice}
            {!selected.view.unavailable && logMalformedView("agent view shown but no ui or unavailable payload", selected)}
            <UnavailableCard
              title={selected.view.unavailable?.title ?? "View unavailable"}
              message={selected.view.unavailable?.message ?? "This agent's view could not be loaded."}
            />
          </>
        );
      case "chat":
        // The transcript. It is the DEFAULT view — most agents declare no UI —
        // and an ended session renders here read-only, from `ended`, which the
        // shell derives once for BOTH this prop and chrome's disclosure. The
        // start-a-replacement control beside this view is the shell's, not the
        // transcript's.
        //
        // The address comes from the `chat` payload, not from `selected` — it is
        // the discriminated union's own answer to which session's transcript
        // this is, and reading it is what makes a rename of its json tag fail
        // here instead of silently falling back to a value that happens to
        // match. An absent payload gets the same inline card the agent-ui arm's
        // does: a wire the server cannot produce (viewFor sets Chat at every
        // site that sets kind=chat), said out loud rather than papered over.
        return selected.view.chat ? (
          <>
            {notice}
            {/* The start-a-replacement control gates on `ended` — the SHELL's
                live answer, which is why the wire carries no bootstrap-frozen
                "may I start a replacement" field for it to read instead. A
                session that ends while the viewer is watching must surface this
                without a reload, and any frozen copy would leave it unreachable
                until one.

                It addresses the session's OWN start route, whose authorization
                is interact on this session: the class comes from the server's
                copy of it, never from the browser. */}
            {ended && (
              <div className="p-4 max-w-3xl mx-auto" data-testid="session-shell-replacement-region">
                <StartReplacementButton ns={selected.view.chat.ns} name={selected.view.chat.name} />
              </div>
            )}
            <ChatView
              ns={selected.view.chat.ns}
              name={selected.view.chat.name}
              readOnly={ended}
              onUserSend={onUserSend}
              onSessionEnded={onSessionEnded}
            />
          </>
        ) : (
          <>
            {notice}
            {logMalformedView("kind is chat but the view payload is absent", selected)}
            <UnavailableCard title="View unavailable" message="This conversation could not be opened." />
          </>
        );
      default:
        // `shown` is the shell's own two-value state, so this is unreachable
        // from the wire — it guards a future third tab added without a matching
        // arm. Falling through to chat would open a live transcript for a view
        // that asked to be something else, silently; this says so instead, and
        // logs the value so an operator can tell WHICH view was asked for.
        return (
          <>
            {notice}
            {logMalformedView("unrecognized shown view", selected)}
            <UnavailableCard title="View unavailable" message="This view could not be shown." />
          </>
        );
    }
  }

  // ONE socket for the selected session, wrapping BOTH views' region rather
  // than sitting inside whichever one is on screen. The views take turns — the
  // transcript unmounts while the agent-defined page is shown — and a socket
  // held by a view would go with it, leaving the session with no live channel
  // at all for as long as the other view is up.
  //
  // Addressed from the SAME value the transcript is, so the frames a viewer
  // sees and the history they are threaded into can never be two different
  // sessions. The selection is the fallback for a view body carrying no chat
  // payload: the socket must exist for the agent-defined view too, and the
  // page has no second answer to which session is on screen.
  //
  // Keyed on that address, so a change of selection retires the previous
  // session's subscribers along with its socket rather than handing them the
  // new session's frames.
  const liveSession = selected.view.chat ?? { ns: selected.ns, name: selected.name };
  return (
    <SessionSocketProvider key={`${liveSession.ns}/${liveSession.name}`} ns={liveSession.ns} name={liveSession.name}>
      <SessionNotifications onAgentReply={onAgentReply} onAwaitingDecision={onAwaitingDecision} onSessionEnded={onSessionEnded} />
      {/* The startup line sits above BOTH views for the same reason the socket wraps both: a person switching tabs must not lose the one line saying why the agent has not started. */}
      <StartupLine />
      {/* agentView sits HERE, at a fixed position, rather than inside either
          arm of shownContent(). That is what actually keeps it mounted, and the
          distinction is not cosmetic: shownContent() returns a bare <div> for
          the agent view and a <Fragment> for the transcript, so React sees a
          different element type at that child position on every switch and
          tears the whole subtree down — discarding the resolved bindings, the
          socket subscription and the folded session signals that the `hidden`
          class was chosen to preserve. Rendered from one place, its position
          never changes, so a switch is a class change and nothing more. */}
      {agentView}
      {shownContent()}
    </SessionSocketProvider>
  );
}

// SessionNotifications is the shell's own ear on the session socket: the events
// that belong to the SHELL rather than to either view are read here, so a
// viewer on the agent-defined page is told exactly what a viewer on the
// transcript is told. Two of them reach the browser TAB (a reply landed; a card
// needs a human); the third is the session ending, which chrome discloses.
//
// The end must be read here because the transcript — the only other reader of
// that frame — is unmounted while the agent-defined view is shown, and that
// view has no route to the frame at all: it learns a session is over only when
// a route it drives next answers 410, which for a page nobody is touching may
// be never.
//
// A component rather than a hook call in the shell, because the provider is
// mounted by SelectedContent, below the shell: this subscribes from inside it
// and calls back out to the handlers that own the tab.
function SessionNotifications({
  onAgentReply,
  onAwaitingDecision,
  onSessionEnded,
}: {
  onAgentReply: (text: string) => void;
  onAwaitingDecision: (lead: string) => void;
  onSessionEnded: () => void;
}) {
  // requestRefs already reported, so a republished prompt — a restart
  // resurfacing a parked card — refreshes that card without pinging the viewer
  // a second time for one pending decision. A ref, not state: this runs inside
  // the frame handler and must not schedule a render just to remember it.
  const notifiedDecisionsRef = useRef<Set<string>>(new Set());

  useSessionFrames((f) => {
    if (f.type === "user_message") {
      // Despite the name this frame carries the AGENT's reply text — see
      // UserMessagePayload's own note on the builtin render-event vocabulary.
      onAgentReply((f.payload as UserMessagePayload).text);
      return;
    }
    if (f.type === "interaction_request") {
      // The prompt's body is nested under a second `payload` — the wsFrame
      // double-nesting types.ts documents.
      const inner = (f.payload as InteractionRequestPayload).payload;
      if (notifiedDecisionsRef.current.has(inner.requestRef)) return;
      notifiedDecisionsRef.current.add(inner.requestRef);
      onAwaitingDecision(inner.lead);
      return;
    }
    if (f.type === "session_ended") {
      // No payload is read: the frame's arrival IS the fact, and the handler
      // it reaches is idempotent, so a second one costs nothing.
      onSessionEnded();
    }
  });

  return null;
}

// logMalformedView reports a `view` payload the browser cannot dispatch on.
// It returns null so it can be dropped straight into JSX beside the card the
// viewer sees: the card is the user-visible arm of the no-silent-errors rule,
// this is the operator-visible one, and neither substitutes for the other.
function logMalformedView(reason: string, selected: SelectedView): null {
  console.error("sessions: cannot render the selected view", {
    reason,
    ns: selected.ns,
    name: selected.name,
    kind: selected.view.kind,
  });
  return null;
}

// UnavailableCard is the inline answer for a view that was asked for and
// cannot be shown. It costs the CONTENT REGION only: identity, the session
// list, the approval surface and the toggle all still render, so a broken
// agent-defined view never takes the page with it.
function UnavailableCard({ title, message }: { title: string; message: string }) {
  return (
    <div className="p-4 max-w-3xl mx-auto" data-testid="session-shell-view-unavailable">
      <Alert variant="destructive">
        <AlertTitle>{title}</AlertTitle>
        <AlertDescription>{message}</AlertDescription>
      </Alert>
    </div>
  );
}

// SessionShell is the platform-owned shell: Chrome (identity, the session
// list, the sibling-view switch, the approval surface, the session-details
// panel, the collapse toggle) wrapping one content region whose contents are
// chosen server-side by viewFor. Every chrome region is a DOM sibling of the
// content region's container, never an ancestor or descendant of it, so
// nothing the content region renders — an agent's declared node tree, a
// transcript — can draw over, cover, or suppress them.
//
// The browser TAB is the shell's too: it owns document.title and the away
// notification, both keyed on the SELECTED session's agent title, and it reads
// the events those turn on off the session's own socket (SessionNotifications).
// A view supplies only what a view alone can see — the viewer typed into its
// composer — so which view is on screen changes nothing about what the tab
// knows.
//
// The shell owns the approval surface for EVERY listed session, not only the
// selected one: a session waiting on a human is visible while the viewer is
// looking at a different one. Those cross-session notices carry
// addressedToViewer:false and are NOT trust events — the phase says a human is
// needed, not WHICH human, and auto-revealing chrome for an approval that may
// belong to someone else is the cry-wolf failure Chrome's own ApprovalNotice
// doc warns about. Only the SELECTED session's live lifecycle supplies a
// trust event, exactly as before the hoist.
export function SessionShell(props: ShellBootstrapProps) {
  const { selected } = props;

  // sessions/notices are STATE because the poll below replaces them; the
  // selection is NOT, because it is the URL's. A poll that moved `selected`
  // would change what the viewer is reading out from under them.
  const [sessions, setSessions] = useState<SessionRow[]>(props.sessions ?? []);
  const [notices, setNotices] = useState<ListNotices | undefined>(props.notices);

  // signals is what the selected view reported up. Seeded empty: a view that
  // has reported nothing has nothing for chrome to act on, which is different
  // from "there is nothing" — see Chrome's approval-surface copy.
  const [signals, setSignals] = useState<ChromeSignals>(EMPTY_CHROME_SIGNALS);

  // infoOpen drives the chrome-owned session-details panel. It is the SHELL's
  // because what it shows — model, budget, owner, phase — describes the
  // session, not the transcript, so it must be reachable from the
  // agent-defined view too.
  const [infoOpen, setInfoOpen] = useState(false);

  // endedLive latches that the SELECTED session's own live channel reported the
  // conversation over. It is the ONE thing on this page allowed to override the
  // server's frozen `selected.sessionOrigin`, and only in that direction: a
  // session cannot un-end.
  const [endedLive, setEndedLive] = useState(false);

  // Tab activity + away notifications are the PAGE's, not any one view's: the
  // browser tab is the shell, and the unread badge names the session the
  // viewer has open. The session socket is only the source of the event (a
  // reply landed); what to do about it is decided here, where the agent's
  // display title and document.title are known.
  const tabActive = useTabActivity();
  const { unread, notify, requestPermission } = useAwayNotifications(tabActive);

  // useCallback with an EMPTY dep array: this handler is listed in the view's
  // reporting effect deps, so a handler whose identity changed every shell
  // render would run that effect on every render. The updater form is what
  // lets the equality check bail out without reading `signals` from this
  // closure — returning the SAME object makes React skip the re-render, which
  // is what closes the loop for good rather than merely making it cheap.
  const handleChromeSignals = useCallback((next: ChromeSignals) => {
    setSignals((prev) => (sameChromeSignals(prev, next) ? prev : next));
  }, []);

  // listAbort cancels any in-flight list read on unmount. It lives in a ref
  // rather than inside the poll effect because refreshList is also called
  // ON DEMAND (see handleSessionEnded), from outside that effect's scope.
  const listAbort = useRef<AbortController | null>(null);
  if (listAbort.current === null) listAbort.current = new AbortController();

  // refreshList re-reads the viewer's session list. Lifted out of the interval
  // so a known list-changing event can ask for it immediately instead of
  // waiting up to SESSION_LIST_POLL_MS for the next tick.
  const refreshList = useCallback(async (signal: AbortSignal) => {
    try {
      const res = await fetch("/sessions/api/sessions", { signal });
      if (!res.ok) {
        // A failed refresh is NOT allowed to blank the list: the props
        // already on screen stay, and the cause is logged rather than
        // dropped. Blanking would be indistinguishable from "you have no
        // sessions", which is the silent-failure shape this repo forbids.
        console.error("sessions: session-list refresh failed", { status: res.status });
        return;
      }
      const body = (await res.json()) as SessionsAPIResponse;
      if (signal.aborted) return;
      // A 200 whose body is not the shape this endpoint promises is NOT
      // "you have no sessions": `sessions ?? []` cannot tell the two apart,
      // and taking the second for the first produces exactly the blank list
      // the comment above forbids. Keep what is on screen and say so.
      if (!Array.isArray(body?.sessions)) {
        console.error("sessions: session-list refresh returned an unexpected body; keeping the current list");
        return;
      }
      // Only the list is reconciled. `selected` is never touched here.
      setSessions(body.sessions);
      setNotices(body.notices);
    } catch (err) {
      if (signal.aborted) return;
      console.error("sessions: session-list refresh errored", err);
    }
  }, []);

  useEffect(() => {
    const controller = listAbort.current!;
    let lastRefresh = Date.now();
    const timer = setInterval(() => {
      if (document.visibilityState !== "visible" && Date.now() - lastRefresh < BACKGROUND_SESSION_LIST_POLL_MS) return;
      lastRefresh = Date.now();
      void refreshList(controller.signal);
    }, SESSION_LIST_POLL_MS);
    const refreshOnReturn = () => {
      if (document.visibilityState === "visible") {
        lastRefresh = Date.now();
        void refreshList(controller.signal);
      }
    };
    window.addEventListener("focus", refreshOnReturn);
    document.addEventListener("visibilitychange", refreshOnReturn);
    return () => {
      controller.abort();
      clearInterval(timer);
      window.removeEventListener("focus", refreshOnReturn);
      document.removeEventListener("visibilitychange", refreshOnReturn);
    };
  }, [refreshList]);

  // handleSessionEnded is where every report that the selected session is over
  // arrives, from whichever surface noticed first. The shell's own ear on the
  // session socket reports the `session_ended` frame, whichever view is showing
  // (see SessionNotifications); the transcript also reports a send the server
  // answered as no-longer-routable, and the agent-defined view reports a route
  // of its own answering 410 (see AgentUIView's onSessionEnded for what that
  // arm can and cannot notice).
  //
  // It exists because `selected` is a frozen bootstrap prop and nothing else on
  // this page ever re-resolves it. Without this, a session that ends while the
  // viewer is watching it leaves chrome disclosing "Resumed your session."
  // beside a view that knows better, and leaves every control gated on the
  // session being over — the start-a-replacement one most of all — unreachable
  // until the viewer reloads. From the agent-UI arm that control is one click
  // away rather than present, because the sibling-view switch's chat half stays
  // rendered and the transcript is where a replacement is started.
  //
  // The ref latch, not `endedLive`, is what makes this idempotent: the callback
  // can fire more than once (a frame, then a later send), and the list refresh
  // is a real server read that must not repeat per event.
  const endedHandledRef = useRef(false);
  const handleSessionEnded = useCallback(() => {
    if (endedHandledRef.current) return;
    endedHandledRef.current = true;
    setEndedLive(true);
    // The row for this session is now wrong in the list too — it still reads as
    // live. Ask for the list immediately rather than leaving it stale until the
    // next poll tick.
    void refreshList(listAbort.current!.signal);
  }, [refreshList]);

  const approvals = useMemo(
    () => [...crossSessionApprovalNotices(sessions, selected), ...signals.approvals],
    [sessions, selected, signals.approvals],
  );

  // The selected session's AGENT display name, for the tab title and the
  // away-notification headline. Read off the list rather than the selection
  // because the list already carries it and shellPageBuild unions the selected
  // session into the list; undefined only if that union ever stops holding, in
  // which case both surfaces fall back to the bare product name rather than to
  // the session's Kubernetes object name, which must never reach a browser
  // surface — an OS notification least of all.
  const selectedTitle = useMemo(
    () => sessions.find((s) => selected && s.ns === selected.ns && s.name === selected.name)?.title,
    [sessions, selected],
  );

  // notifyTitleRef holds the headline the away-notification uses, read at
  // frame-arrival time. A ref rather than a dep so a list poll that changes
  // nothing about the selection cannot churn the callback the socket's frame
  // handler closes over.
  const notifyTitleRef = useRef("oap sessions");
  notifyTitleRef.current = selectedTitle || "oap sessions";

  useEffect(() => {
    document.title = formatTabTitle(selectedTitle ?? null, unread);
  }, [selectedTitle, unread]);

  // handleAgentReply is what SessionNotifications calls when the agent's reply
  // lands on the session socket. The shell decides whether that is worth
  // interrupting the viewer for — useAwayNotifications drops it outright while
  // the tab is active.
  const handleAgentReply = useCallback((text: string) => {
    notify(notifyTitleRef.current, text);
  }, [notify]);

  // handleAwaitingDecision is the same road as handleAgentReply, for the other
  // kind of waiting. A reply can sit unread indefinitely; a card is BLOCKING
  // the agent until someone clicks, so it says so rather than reusing the
  // session title as its headline.
  const handleAwaitingDecision = useCallback((lead: string) => {
    notify(`${notifyTitleRef.current} needs your decision`, lead);
  }, [notify]);

  // sessionOrigin is what this page currently knows about the selected
  // session's ladder branch: the server's answer at page load, overridden by
  // what the live channel saw. It is derived, not stored, so the two can never
  // drift; `selectedEnded` is derived from it in turn so chrome's disclosure
  // and every control gated on the session being over read ONE value.
  //
  // That is why the wire carries no separate "this session has ended" or "may
  // I start a replacement" boolean: either would be a bootstrap-frozen second
  // copy of this same fact, and the stale one.
  const sessionOrigin = selected ? (endedLive ? "ended" : selected.sessionOrigin) : undefined;
  const selectedEnded = sessionOrigin === "ended";

  // canStart needs BOTH facts, and neither implies the other: the classes are
  // the viewer's own standing, canStartSessions is whether this webd mounted
  // the start route at all. See the headerActions comment below.
  const canStart = props.canStartSessions && (props.startableClasses?.length ?? 0) > 0;

  // shownView is which sibling is on screen. Seeded from what the server
  // rendered, then owned by the tabs: following one is a state change plus a
  // history entry, NOT a navigation.
  //
  // It used to be a navigation, and the cost was not only latency. A full load
  // tore down the shell, the session list and the approval surface to change
  // one region, and took every piece of unsaved view state with it — which is
  // how a viewer lost the filters they had just picked by looking at the
  // transcript for a moment.
  //
  // `unavailable` seeds to "ui": that kind means the agent's view was asked
  // for and could not be resolved, so the agent-view arm is the one whose
  // fallback carries the disclosure.
  const [shownView, setShownView] = useState<SiblingView>(() =>
    props.selected?.view.kind === "chat" ? "chat" : "ui",
  );

  // A session change re-seeds the view. Without this, opening a session from
  // the list while sitting on the transcript would carry "chat" across to an
  // agent whose UI is the point of opening it.
  const selectedKey = props.selected ? `${props.selected.ns}/${props.selected.name}` : "";
  const seededFor = useRef(selectedKey);
  // uiEverShownRef gates keeping the agent view mounted (see SelectedContent).
  // Reset on a session change in the SAME effect that re-seeds the view: the
  // flag means "this session's agent view has been opened", and carrying it
  // across a selection would mount the new session's view before the viewer
  // asked for it — running its bindings and opening its socket for a page they
  // may never look at.
  const uiEverShownRef = useRef(false);
  useEffect(() => {
    if (seededFor.current === selectedKey) return;
    seededFor.current = selectedKey;
    uiEverShownRef.current = false;
    setShownView(props.selected?.view.kind === "chat" ? "chat" : "ui");
  }, [selectedKey, props.selected?.view.kind]);

  // handleSelectView switches locally and records the switch in history, so
  // the address bar keeps describing what is on screen and Back returns to the
  // sibling rather than to the previous page.
  //
  // pushState (unlike the parameter mirror, which replaces): a view switch is
  // a deliberate, discrete act a viewer reasonably expects Back to undo, and
  // there is one entry per switch rather than one per keystroke.
  const handleSelectView = useCallback((next: SiblingView, href: string) => {
    setShownView(next);
    if (typeof window !== "undefined") {
      window.history.pushState(window.history.state, "", href);
    }
  }, []);

  // The Back/Forward buttons move through those entries, and the browser does
  // not re-render for them, so the view has to follow `?view=` itself. Without
  // this, Back would change the URL and leave the content region on whatever
  // was already there — the address and the page disagreeing, which is worse
  // than not supporting Back at all.
  useEffect(() => {
    if (typeof window === "undefined") return;
    const onPop = () => {
      const v = new URLSearchParams(window.location.search).get("view");
      setShownView(v === "chat" ? "chat" : "ui");
    };
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

  // ?new=1 asks for the picker already open — the desktop menu bar's "New
  // chat" links here, and landing on a list with the control merely available
  // is not what that item promises. `ns` and `agentClass` ask for one of the
  // picker's entries already chosen — the builder page's "Try it yourself"
  // link, which knows which agent it means to send someone to. `prompt` seeds
  // the message box the same way. All four are read ONCE, into the dialog's
  // initial state: each is a one-shot REQUEST, not a mode, so closing the
  // picker must stick and a re-render must not reopen or re-seed it. useMemo
  // rather than a bare read because window.location is not a React input and
  // re-reading it per render would resurrect the dialog (or the preselection)
  // after the viewer dismissed it. Chrome.sessionViewHref drops all four from
  // every mirrored href for the same reason.
  const startRequest = useMemo((): {
    open: boolean;
    preselect?: { ns: string; agentClass: string };
    initialPrompt?: string;
  } => {
    if (typeof window === "undefined") return { open: false };
    const q = new URLSearchParams(window.location.search);
    const ns = q.get("ns") ?? "";
    const agentClass = q.get("agentClass") ?? "";
    return {
      open: q.get("new") === "1",
      preselect: ns && agentClass ? { ns, agentClass } : undefined,
      initialPrompt: q.get("prompt") ?? undefined,
    };
  }, []);

  // ChromeSelection is a whole-or-nothing union (Chrome.tsx), so the
  // session-scoped props are built as one object: passing them individually
  // as `selected?.ns` would offer Chrome a half-selection the type forbids and
  // the component cannot disclose honestly.
  //
  // offersAgentUI is ANDed with the shell's own live `selectedEnded`, not
  // forwarded bare: the server answers false for a session that had already
  // ended at page load (viewFor, view.go — an ended session's
  // bindings/actions/live routes all answer 410, so there is nothing to switch
  // to), and a session that ends while the viewer is watching must reach the
  // same state without a reload. Reading the frozen bootstrap value alone
  // would leave a switch on screen whose destination stopped existing.
  const chromeSelection: ChromeSelection = selected
    ? {
        ns: selected.ns,
        name: selected.name,
        sessionOrigin: sessionOrigin ?? selected.sessionOrigin,
        currentView: shownView,
        onSelectView: handleSelectView,
        // Forwarded only for a session the shell currently shows as ended:
        // a session that ends while the viewer watches (endedLive) never had
        // a bootstrap reason to forward, and one that failed BEFORE the page
        // loaded is exactly the case the server's copy answers.
        endedReason: selectedEnded ? selected.endedReason : undefined,
        offersAgentUI: selected.offersAgentUI && !selectedEnded,
      }
    : {};

  return (
    <Chrome
      {...chromeSelection}
      subject={props.subject}
      initialState={selected?.chrome?.initialState}
      approvals={approvals}
      // Forwarded UNRANKED, exactly as the view reported them. Reducing this
      // to one slot with `??` would make whichever source lost the pick
      // unable to reveal at all for as long as the winner is present — the
      // same suppression in a narrower form, one layer above the list Chrome
      // itself consumes.
      trustEvents={signals.trustEvents}
      // undefined, not an empty fragment, when neither child would render:
      // Chrome mounts the header-actions container on truthiness alone, and an
      // always-truthy slot would leave an empty container (and its testid) in
      // the DOM on a shell with no selection and no start control — quietly
      // making that guard meaningless for every future caller.
      headerActions={
        canStart || selected ? (
          <>
            {/* The start control is chrome, not content: it is reachable with
                NO session selected (the dashboard case the session-scoped
                start route cannot serve) and it must stay reachable while an
                agent's declared view owns the content region.

                Both conditions are required. startableClasses non-empty is the
                viewer's standing; canStartSessions is whether this webd
                mounted the route at all. Rendering from the first alone offers
                a button whose every press 404s on a shared cluster. */}
            {canStart && (
              <NewSessionDialog
                classes={props.startableClasses}
                openInitially={startRequest.open}
                preselect={startRequest.preselect}
                initialPrompt={startRequest.initialPrompt}
              />
            )}
            {selected && (
              <>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  aria-label="Session details"
                  data-testid="session-shell-session-info"
                  onClick={() => setInfoOpen(true)}
                >
                  <MoreHorizontal className="h-4 w-4" aria-hidden="true" />
                </Button>
                {/* The panel lives beside its button, in the chrome region —
                    not inside the content region, which an agent's declared
                    node tree owns. It fetches only while open, so a shell whose
                    viewer never asks for details never reads the detail
                    endpoint. */}
                <SessionInfoPanel ns={selected.ns} name={selected.name} open={infoOpen} onOpenChange={setInfoOpen} />
              </>
            )}
          </>
        ) : undefined
      }
      sidebar={
        <SessionList
          sessions={sessions}
          notices={notices}
          selectedNs={selected?.ns}
          selectedName={selected?.name}
        />
      }
    >
      <GoalSessionAlerts key={props.subject} sessions={sessions} selected={selected} subject={props.subject} notify={notify} />
      {props.canStartSessions && (props.startableClasses?.length ?? 0) === 0 && startRequest.open && <NoAgentsDialog />}
      {selected ? (
        <SelectedContent
          selected={selected}
          shown={shownView}
          ended={selectedEnded}
          onChromeSignals={handleChromeSignals}
          onUserSend={requestPermission}
          onAgentReply={handleAgentReply}
          onAwaitingDecision={handleAwaitingDecision}
          onSessionEnded={handleSessionEnded}
          uiEverShownRef={uiEverShownRef}
        />
      ) : (
        <div className="p-6 flex flex-col gap-3" data-testid="session-shell-no-selection">
          <span className="text-sm text-muted-foreground">Pick a session to open it.</span>
          {/* The empty dashboard explains itself. Gated on the viewer's own
              standing, not on canStart: a viewer who holds standing on agents
              but whose webd mounts no start route is not being told they have
              no access — they have it, this server just cannot start one for
              them, which is an operator's problem and not copy for them. */}
          {(props.startableClasses?.length ?? 0) === 0 && <EmptyDashboardExplanation />}
        </div>
      )}
    </Chrome>
  );
}
