import { useState } from "react";
import {
  Button,
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  Label,
} from "@ap/design";
import type { StartableClass } from "./types";

// StartResponse mirrors pkg/web/webui/sessions' StartResponse and
// pkg/web/webui/agentui's startResponse — the SAME three keys, from two routes,
// pinned to ui/testdata/start.golden.json on both sides. This is the browser's
// ONE parse helper for both, which is why the two producers must not drift:
// a renamed key here yields `undefined` for one route and the navigation dead-
// ends with nothing thrown.
export interface StartResponse {
  ns: string;
  name: string;
  href: string;
}

// parseStartResponse reads a start route's answer. It refuses a body missing
// any of the three fields rather than navigating to `undefined`: a same-origin
// relative href is the whole contract, and a partial one is not a lesser
// version of it.
export function parseStartResponse(body: unknown): StartResponse {
  const b = body as Partial<StartResponse> | null;
  if (!b || typeof b.ns !== "string" || typeof b.name !== "string" || typeof b.href !== "string" || !b.href) {
    throw new Error("the server's answer did not address a session");
  }
  return { ns: b.ns, name: b.name, href: b.href };
}

// StartError distinguishes the ONE thing a caller must decide differently:
// whether retrying is safe.
//
// `refused` means the server ANSWERED — a status we read, so by construction
// no session was created and clicking again is safe. `indeterminate` means we
// never got an answer (a rejected fetch, a proxy that timed out, a body that
// did not parse as the contract): the request may well have succeeded
// server-side, and since neither start route carries an idempotency key, a
// retry would create a SECOND session. That is the only sequence in which a
// user who intended one session gets two without doing anything wrong.
export class StartError extends Error {
  readonly indeterminate: boolean;
  constructor(message: string, indeterminate: boolean) {
    super(message);
    this.name = "StartError";
    this.indeterminate = indeterminate;
  }
}

// startSession POSTs to one of the two start routes and returns where to go.
//
// It reads the refusal's own message when there is one — both routes answer a
// refusal as {"error": "..."} in fixed, browser-safe copy that names no CRD
// kind, no permission and no object — and falls back to a generic line when
// the body is not that shape (an HTML system page from a route-level gate, for
// instance, which would throw if parsed as JSON).
export async function startSession(url: string, body: Record<string, string>): Promise<StartResponse> {
  let res: Response;
  try {
    res = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  } catch (e) {
    // No answer at all. The server may have created the session anyway.
    console.error("sessions: start request did not complete", { url, err: e });
    throw new StartError(
      "We could not confirm whether that session started. Reload your sessions before trying again.",
      true,
    );
  }
  if (!res.ok) {
    let message = "";
    try {
      const parsed = (await res.json()) as { error?: string };
      message = typeof parsed?.error === "string" ? parsed.error : "";
    } catch {
      message = "";
    }
    console.error("sessions: start request failed", { url, status: res.status });
    // A status the server chose: nothing was created, so retrying is safe.
    throw new StartError(message || "That session could not be started right now.", false);
  }
  try {
    return parseStartResponse(await res.json());
  } catch {
    // A 200 whose body we cannot address. The session almost certainly EXISTS
    // — the server answered success — we simply cannot navigate to it, so this
    // is indeterminate, not a refusal.
    console.error("sessions: start succeeded but its answer could not be addressed", { url });
    throw new StartError(
      "That session started, but we could not open it. Reload your sessions to find it.",
      true,
    );
  }
}

// isIndeterminate reports whether a caught start failure leaves it unknown
// whether a session was created. Anything that is not a StartError is treated
// as indeterminate — an unexpected throw is exactly the case where we know
// least, and the safe answer is to stop offering the retry.
function isIndeterminate(e: unknown): boolean {
  return !(e instanceof StartError) || e.indeterminate;
}

// UNSTARTABLE_REASON is what a viewer sees when the agent they picked is one
// this server cannot start a session for. It states the SERVER's limit and
// what they can still do, and names no namespace, no permission, no CRD kind
// and no API object — the operator's log carries which namespace it was.
//
// It deliberately does not say "you do not have access": the viewer does, which
// is the only reason the entry is in the picker at all.
export const UNSTARTABLE_REASON =
  "This server cannot start new sessions for that agent. You can still open the sessions you already have with it, or ask an administrator to enable starting here.";

// UNSTARTABLE_SUFFIX marks the picker entries the reason above applies to, so
// the limit is visible while choosing rather than only after choosing.
const UNSTARTABLE_SUFFIX = " — cannot start here";

// classOptionLabels renders one label per startable pair, disambiguating any
// title that appears more than once by appending its namespace, and marking
// any entry this server cannot actually start.
//
// startableClasses is deduped by (namespace, class), so a viewer with standing
// on the same-named agent in two namespaces gets two entries whose titles are
// identical — and the whole reason the authorization rule checks the PAIR is
// that those two may be entirely different agents with entirely different
// tools. Labelling both with the same string asks the viewer to pick blind.
//
// The namespace is a Kubernetes concept, so it is spelled as a plain qualifier
// ("Helper (in team-a)") rather than as control-plane vocabulary, and only
// where it is needed to tell two entries apart.
export function classOptionLabels(classes: readonly StartableClass[]): string[] {
  const seen = new Map<string, number>();
  for (const c of classes) seen.set(c.title, (seen.get(c.title) ?? 0) + 1);
  return classes.map((c) => {
    const base = (seen.get(c.title) ?? 0) > 1 ? `${c.title} (in ${c.ns})` : c.title;
    return c.startable ? base : base + UNSTARTABLE_SUFFIX;
  });
}

// navigateTo is the ONE place a start response's href is followed. Split out
// so a test can drive the whole submit path without jsdom's unimplemented
// navigation, and so there is exactly one line to look at when asking "where
// can this component send the browser".
export function navigateTo(href: string) {
  window.location.assign(href);
}

// EmptyDashboardExplanation is what a viewer with no startable classes sees
// instead of a control that would refuse every time. It answers "why can I
// start nothing" in the vocabulary of how access actually arrives — being
// added to a session, or a channel an agent is connected to — and names no
// CRD kind, no permission, no Kubernetes object and no SpiceDB relation.
//
// It lives beside the start control it stands in for, and is rendered by the
// shell in the no-selection region: that is where a viewer with an empty
// dashboard actually lands, and a header is the wrong place for a paragraph.
export function EmptyDashboardExplanation() {
  return (
    <p className="text-sm text-muted-foreground" data-testid="session-shell-no-startable-classes">
      You do not have access to any agents yet. Access arrives by being added to a session someone else started, or
      through a channel an agent is connected to. Ask your administrator to install at least one agent or give you
      access to an existing agent.
    </p>
  );
}

export function NoAgentsDialog() {
  const [open, setOpen] = useState(true);
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>No agents available</DialogTitle>
          <DialogDescription>
            You need an available agent to start a chat. Ask your administrator to install at least one agent or
            give you access to an existing agent.
          </DialogDescription>
        </DialogHeader>
        <Button type="button" onClick={() => setOpen(false)}>Got it</Button>
      </DialogContent>
    </Dialog>
  );
}

// NewSessionDialog is the shell's start control: a class picker over the
// (namespace, agent) pairs the SERVER derived for this viewer, a prompt, and a
// submit.
//
// The shell renders it only when the viewer has at least one startable class
// AND this webd mounted the start route (see SessionShell's `canStart`); the
// empty-classes guard below is a defensive floor, not that decision. A viewer
// with no interactable session can start nothing, and the shell explains that
// with EmptyDashboardExplanation rather than offering a button that always
// refuses — the honest failure mode of the derived authorization rule.
//
// A pair the server cannot CREATE in is a different case and gets a different
// answer: it stays in the picker, marked, and picking it shows the reason and
// refuses the submit (UNSTARTABLE_REASON). Dropping those entries instead would
// empty the list for the ordinary shared-cluster viewer — whose standing comes
// from a session in a channel's namespace — and send them to
// EmptyDashboardExplanation, which says access has not arrived yet. It has;
// this server just cannot start one there.
//
// The submit DISABLES ON CLICK and re-enables ONLY for a refusal the server
// actually answered. That is the only mitigation for a lost server-side
// double-submit guard — the start routes carry no idempotency key — and it is
// a client-side mitigation, not a guarantee: a duplicate session is visible
// rather than silent, which is why it was acceptable to ship without the
// server half.
//
// The re-enable rule is the load-bearing half. Re-enabling on ANY failure is
// itself a duplicate producer: the POST can block for as long as the adopt's
// readiness wait, an intermediary or the user's patience gives out, the fetch
// rejects — and the first request completes server-side anyway, so the retry
// creates a second session. See StartError.indeterminate.
export function NewSessionDialog({
  classes,
  onNavigate = navigateTo,
  openInitially = false,
  preselect,
  initialPrompt,
}: {
  classes: readonly StartableClass[];
  // onNavigate is injected only so a test can observe where a successful
  // start would send the browser; production always uses navigateTo.
  onNavigate?: (href: string) => void;
  // openInitially opens the picker on first paint, for a caller that arrived
  // asking to start something (the desktop menu bar's "New chat", which links
  // to /sessions?new=1). It seeds the INITIAL state only — the dialog stays
  // fully user-closable afterwards, and nothing re-opens it on re-render.
  openInitially?: boolean;
  // preselect names the (ns, class) pair a link asked to start with — the
  // builder page's "Try it yourself" link. It seeds the INITIAL selection
  // only, the same one-shot treatment as openInitially: read once by the
  // shell from the URL, dropped from every mirrored href afterward
  // (Chrome.sessionViewHref), so re-navigating within the session never
  // re-applies it.
  preselect?: { ns: string; agentClass: string };
  // initialPrompt seeds the message box for the same link, so it can also say
  // what the agent should be asked. Also one-shot; see preselect above.
  initialPrompt?: string;
}) {
  const [open, setOpen] = useState(openInitially);
  // preselectIndex looks the requested pair up among the classes THIS VIEWER
  // may start here — not among every class that exists, which this component
  // never sees. -1 means the link named a pair absent from that list: no
  // standing, no server support here, or simply a stale link.
  const preselectIndex = preselect
    ? classes.findIndex((c) => c.ns === preselect.ns && c.class === preselect.agentClass)
    : -1;
  // A requested pair that is not in the list is refused on screen, not
  // swapped for whichever class happens to be first — silently picking one
  // would start a session for an agent the link never named.
  //
  // The refusal explains a LINK, not a permanent state of the picker: it says
  // "that link pointed somewhere you can't start", and once the viewer has
  // deliberately picked something else themselves, the link is no longer what
  // is being submitted, so the refusal has nothing left to say. preselectDismissed
  // is set the moment the viewer changes the select (see onChange below) and,
  // unlike preselectIndex, never resets — reopening the SAME dialog instance
  // (preselect is one-shot; see the prop doc) must not resurrect a refusal the
  // viewer already moved past.
  const [preselectDismissed, setPreselectDismissed] = useState(false);
  const preselectRefused = preselect !== undefined && preselectIndex === -1 && !preselectDismissed;
  const [selected, setSelected] = useState(preselectIndex === -1 ? 0 : preselectIndex);
  const [prompt, setPrompt] = useState(initialPrompt ?? "");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (classes.length === 0) return null; // nothing to pick; see this component's doc

  const target = classes[Math.min(selected, classes.length - 1)];
  const labels = classOptionLabels(classes);
  // The picked agent is one this SERVER cannot create a session for. The entry
  // is still selectable — hiding agents the viewer demonstrably has access to
  // would read as "you have no access to that" — but the submit is refused
  // here, with the reason on screen, rather than sent to be refused by the
  // server. The server refuses it too (browserstart.Start): this is the
  // affordance, not the enforcement.
  const targetUnstartable = !target.startable;
  // An agent that declares its own view is opened, not conversed with. The
  // dialog says so: the primary action names the view, and the opening message
  // stops being the price of entry.
  //
  // The message is kept, behind a disclosure, rather than removed. It is how a
  // viewer says "last seven days" while starting — the same sentence they
  // would send from a channel — and an agent can use it to fill the view's own
  // controls in. Demanding it was the defect; discarding it would remove the
  // only way to start a view anywhere other than its defaults.
  const targetOffersUI = target.offersUI === true;

  async function submit() {
    // Latch BEFORE the await, so a second click during the request cannot
    // start a second session. It is never cleared on success: the tab is
    // navigating away, and re-enabling would open a window where the viewer
    // can submit again against a page that is already leaving.
    setSubmitting(true);
    setError(null);
    try {
      const res = await startSession("/sessions/api/start", {
        ns: target.ns,
        agentClass: target.class,
        prompt,
      });
      // `&view=ui` is asserted rather than left to the shell's default. That
      // default resolves to the agent view only while the view is currently
      // resolvable and falls back to the transcript otherwise — so a view that
      // is momentarily unresolvable would land the viewer somewhere they did
      // not ask to be, with nothing said about it. Asking for it by name means
      // an unavailable view is DISCLOSED as unavailable.
      onNavigate(targetOffersUI ? `${res.href}&view=ui` : res.href);
    } catch (e) {
      setError((e as Error).message);
      // Re-enable ONLY when the server answered a refusal, where nothing was
      // created by definition. An indeterminate failure keeps the button
      // latched: the session may already exist, and a retry would make a
      // second one that nothing server-side would collapse.
      if (!isIndeterminate(e)) setSubmitting(false);
    }
  }

  return (
    <>
      <Button
        type="button"
        size="sm"
        data-testid="session-shell-new-session"
        onClick={() => {
          setError(null);
          setOpen(true);
        }}
      >
        + New session
      </Button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Start a new session</DialogTitle>
            <DialogDescription>
              {targetOffersUI ? "Pick an agent. This one opens its own view." : "Pick an agent and say what you need."}
            </DialogDescription>
          </DialogHeader>
          <div className="flex flex-col gap-3">
            {preselectRefused && (
              // The link named a pair not in this viewer's list here — refused
              // rather than falling back to whatever the select shows at
              // index 0, which the viewer never asked for. See preselectIndex
              // above for what "not in the list" covers.
              <p className="text-sm text-destructive" data-testid="session-shell-preselect-refused">
                You can&apos;t start that agent from here.
              </p>
            )}
            <div className="flex flex-col gap-1">
              <Label htmlFor="new-session-class">Agent</Label>
              {/* A native select, not the design system's Select: this list is
                  server-derived and short, and a native control keeps the
                  option labels directly assertable and keyboard-reachable
                  without a portal. Each option shows the agent's TITLE — the
                  display name — never the class's own object name, and never
                  the session's. See classOptionLabels for the one case a
                  namespace is appended. */}
              <select
                id="new-session-class"
                className="rounded-md border border-input bg-background px-3 py-2 text-sm"
                data-testid="session-shell-new-session-class"
                value={selected}
                onChange={(e) => {
                  setSelected(Number(e.target.value));
                  // A deliberate manual pick clears the refusal: the viewer is
                  // no longer submitting the link's named pair, so a message
                  // about that pair no longer applies, and the select — which
                  // only ever lists classes this viewer can actually pick —
                  // must stay usable rather than locked behind it.
                  setPreselectDismissed(true);
                }}
              >
                {classes.map((c, i) => (
                  <option key={`${c.ns}/${c.class}`} value={i}>
                    {labels[i]}
                  </option>
                ))}
              </select>
            </div>
            {/* For a view-declaring agent the message is optional and folded
                away, so the dialog is one choice and one button. It stays
                reachable because it is how a viewer opens the view somewhere
                other than its defaults — "last seven days", the same sentence
                they would send from a channel. <details> rather than a state
                toggle: the open/closed state is the browser's, so typing into
                the textarea can never be interrupted by a re-render deciding
                to collapse it. */}
            {targetOffersUI ? (
              <details className="flex flex-col gap-1" data-testid="session-shell-new-session-prompt-disclosure">
                {/* Says what happens if they type nothing, rather than only
                    that they need not. A session always gets an opening turn —
                    the server supplies one saying the view was opened — and a
                    dialog that hid that would leave a line in the transcript
                    the viewer never saw the origin of. */}
                <summary className="cursor-pointer text-sm text-muted-foreground">
                  Add a message (optional — otherwise the agent is just told you opened the view)
                </summary>
                <textarea
                  id="new-session-prompt"
                  rows={3}
                  aria-label="Message"
                  className="mt-1 w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
                  data-testid="session-shell-new-session-prompt"
                  value={prompt}
                  onChange={(e) => setPrompt(e.target.value)}
                />
              </details>
            ) : (
              <div className="flex flex-col gap-1">
                <Label htmlFor="new-session-prompt">Message</Label>
                <textarea
                  id="new-session-prompt"
                  rows={4}
                  className="rounded-md border border-input bg-background px-3 py-2 text-sm"
                  data-testid="session-shell-new-session-prompt"
                  value={prompt}
                  onChange={(e) => setPrompt(e.target.value)}
                />
              </div>
            )}
            {targetUnstartable && (
              <p className="text-sm text-muted-foreground" data-testid="session-shell-new-session-unstartable">
                {UNSTARTABLE_REASON}
              </p>
            )}
            {error && (
              <p className="text-sm text-destructive" data-testid="session-shell-new-session-error">
                {error}
              </p>
            )}
            <div className="flex justify-end">
              <Button
                type="button"
                data-testid="session-shell-new-session-submit"
                // A message is required only where it is the ONLY input: a
                // conversation with nothing said is not a request. A
                // view-declaring agent already has one — the view's own
                // defaults — so demanding a sentence before it can be opened
                // asks the viewer to describe what they are about to be shown.
                disabled={
                  submitting || targetUnstartable || preselectRefused || (!targetOffersUI && prompt.trim() === "")
                }
                onClick={() => void submit()}
              >
                {submitting ? (targetOffersUI ? "Opening…" : "Starting…") : targetOffersUI ? "Open UI" : "Start session"}
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>
    </>
  );
}

// StartReplacementButton is the ended-session control: start another session of
// the SAME agent, from the session the viewer is looking at.
//
// It POSTs to the session-scoped route (POST /agent-ui/{ns}/{name}/start),
// whose authorization is interact on the ended session itself — a strictly
// narrower gate than the dashboard control's, and the right one here because
// the class is read from the server's own copy of that session rather than
// named by the browser.
//
// It gates on the SHELL's live answer for whether the session is over, not on
// the frozen bootstrap prop: a session that ends while the viewer is watching
// must surface this control without a reload.
export function StartReplacementButton({
  ns,
  name,
  onNavigate = navigateTo,
}: {
  ns: string;
  name: string;
  onNavigate?: (href: string) => void;
}) {
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function submit() {
    setSubmitting(true);
    setError(null);
    try {
      const res = await startSession(
        `/agent-ui/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/start`,
        { prompt: "Continue where we left off." },
      );
      onNavigate(res.href);
    } catch (e) {
      setError((e as Error).message);
      // Re-enable ONLY when the server answered a refusal, where nothing was
      // created by definition. An indeterminate failure keeps the button
      // latched: the session may already exist, and a retry would make a
      // second one that nothing server-side would collapse.
      if (!isIndeterminate(e)) setSubmitting(false);
    }
  }

  return (
    <div className="flex items-center gap-2">
      <Button
        type="button"
        size="sm"
        variant="outline"
        data-testid="session-shell-start-replacement"
        disabled={submitting}
        onClick={() => void submit()}
      >
        {submitting ? "Starting…" : "Start a new session"}
      </Button>
      {error && (
        <span className="text-sm text-destructive" data-testid="session-shell-start-replacement-error">
          {error}
        </span>
      )}
    </div>
  );
}
