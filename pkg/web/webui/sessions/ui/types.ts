import type { Declaration } from "@ap/agentui";

// This file mirrors pkg/web/webui/sessions' own wire structs field-for-field —
// shellProps (page.go), sessionRow/listNotices/startableClass (list.go), and
// selectedView/viewBody/chatViewProps/unavailableMessage (view.go). Those Go
// structs are the contract; nothing here invents a field they do not carry,
// and every name below is the json tag, not the Go field name.
//
// The bootstrap is read through an unchecked `JSON.parse(...) as T`, so a
// rename on either side turns a prop into `undefined` at runtime with nothing
// failing anywhere. The pin is testdata/shell.golden.json: shellPageBuild's
// own output is asserted against that file (page_golden_internal_test.go) and
// SessionShell.test.tsx reads it — rendered where a component reads a field,
// and by key off the parsed JSON where one does not yet.

// SessionRow is one session the viewer holds interact on. `phase` is HUMAN
// copy mapped server-side, never the control-plane phase string.
export interface SessionRow {
  ns: string;
  name: string;
  class: string;
  title: string;
  phase: string;
  awaitingHuman: boolean;
  ended: boolean;
  // startedAt carries json:"startedAt,omitempty" — absent for a session whose
  // status has no start time yet, which is why it is optional here.
  startedAt?: string;
  uid?: string;
  goalCreated?: boolean;
  openingSummary?: string;
}

// ListNotices is every way the list is incomplete. Zero values mean complete.
export interface ListNotices {
  unavailable: number;
  truncated: boolean;
  // bootstrapUnavailable reports that the platform#start_session arm of the
  // start set could not be evaluated, so the agent picker may be missing
  // entries this viewer is entitled to. Distinct from the two above: those
  // narrow the SESSION LIST, this one narrows what can be STARTED.
  bootstrapUnavailable: boolean;
}

// StartableClass is one (namespace, class) the viewer may start a session
// for. `title` is the agent's display name — what the picker lists; `class`
// is what the start request names and the server re-authorizes.
//
// `startable` is a SECOND, independent fact: whether the server can actually
// create a session in that namespace. An entry with startable:false is one the
// viewer genuinely holds standing on and this server cannot start — it stays
// in the list, marked, with the reason shown, rather than being hidden (which
// would read as "you have no access") or offered (which would 500 on every
// press).
export interface StartableClass {
  ns: string;
  class: string;
  title: string;
  startable: boolean;
  // offersUI reports that this agent declares its own view. A THIRD
  // independent fact after standing and reachability, and the one that decides
  // what starting a session should even look like: an agent whose point is a
  // dashboard should open that dashboard, not hand the viewer a message box
  // and an empty transcript.
  //
  // Optional on the wire so a shell reading an older server's bootstrap simply
  // gets the transcript-shaped flow, which is correct for every agent.
  offersUI?: boolean;
}

// UnavailableMessage is the title/message a *webui.PageError produced,
// rendered INSIDE the content region rather than in place of the page.
export interface UnavailableMessage {
  title: string;
  message: string;
}

// ChatViewWireProps names which session's transcript to open. Deliberately
// minimal: the transcript itself is read through the chat data plane, gated
// on the same interact standing the page already confirmed.
//
// Named for the WIRE, like its sibling AgentUIViewWireProps below, because the
// component it feeds (pkg/web/webui/chat/ui's ChatView) has its own props type of
// the same shape-of-name and a different field set. Two exported ChatViewProps
// in adjacent modules is one import away from a confusing bug.
export interface ChatViewWireProps {
  ns: string;
  name: string;
}

// AgentUIViewWireProps is viewmodel.go's ViewProps — the agent-defined view's
// own bootstrap, nested under the discriminated `view` union.
export interface AgentUIViewWireProps {
  ns: string;
  name: string;
  declaration: Declaration;
  // agentParams is what the agent set the view's controls to, if anything.
  // Optional on the wire (Go omits it when empty), and absent for the ordinary
  // page where the viewer drives every control themselves.
  agentParams?: Record<string, string>;
}

// ViewBody is the discriminated payload: exactly one of ui/chat/unavailable
// is set, matched by `kind`. `notice` is DIFFERENT from `unavailable` — it is
// an additive disclosure alongside content that IS shown, never a substitute
// for it.
export interface ViewBody {
  kind: "agent-ui" | "chat" | "unavailable";
  ui?: AgentUIViewWireProps;
  chat?: ChatViewWireProps;
  unavailable?: UnavailableMessage;
  notice?: UnavailableMessage;
}

// SelectedView is the shell's per-session props, present exactly when the
// request carried a `?session=` naming a session the viewer may interact
// with. `chrome` is scoped here rather than to the whole page because it is
// the SELECTED session's UI's request — navigating to another session drops
// it.
// There is deliberately no `canStartReplacement` here: it would be exactly
// `sessionOrigin === "ended"`, and the shell must gate the start-a-replacement
// control on its own LIVE answer anyway (a session can end while the viewer is
// watching), never on a bootstrap-frozen copy. See SessionShell's
// `selectedEnded`.
export interface SelectedView {
  ns: string;
  name: string;
  sessionOrigin: string;
  chrome: { initialState?: string };
  // endedReason is why an ended session ended, when it ended in failure: the
  // server's `Failed` condition message, verbatim. Absent for every other
  // selection — a session that simply finished ended for no reason worth
  // disclosing. It is the ONLY explanation a session that never booted has:
  // there is no transcript, and its agent-UI page answers 410.
  endedReason?: string;
  offersAgentUI: boolean;
  view: ViewBody;
}

// ShellBootstrapProps is the ENTIRE server-rendered bootstrap for
// GET /sessions.
export interface ShellBootstrapProps {
  subject: string;
  sessions: SessionRow[];
  notices: ListNotices;
  selected?: SelectedView;
  startableClasses: StartableClass[];
  // canStartSessions reports whether this webd mounted the start route at all.
  // It is a SEPARATE fact from startableClasses being non-empty, and the start
  // control needs BOTH: a viewer can hold standing on several agents on a webd
  // that mounts no start route, and rendering the control from the standing
  // alone would offer a button whose every press 404s.
  canStartSessions: boolean;
}

// SessionsAPIResponse is what GET /sessions/api/sessions answers — the SAME
// buildSessionList output the page's first paint used, which is why the poll
// can replace those three fields wholesale without the two disagreeing about
// what "the list" is. It carries no `selected`: the selection is the URL's.
export interface SessionsAPIResponse {
  sessions: SessionRow[];
  notices: ListNotices;
  startableClasses: StartableClass[];
}
