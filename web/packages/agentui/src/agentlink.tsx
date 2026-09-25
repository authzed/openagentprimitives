// agentlink.tsx renders ap:agentlink — a button-styled control for ONE agent.
// It takes no href on purpose: the URL is built here from
// namespace/agentClass/prompt — the only address this control can ever point
// at — so an agent-composed fill can never send a person anywhere else.
//
// Two modes, and what a click does is the whole difference. In LINK mode it is
// read-only, like progress.tsx: an anchor that opens the browser chat's
// new-session dialog in a new tab, firing no action and posting nothing;
// ap:button/ap:form are the only controls in this vocabulary that do. In
// `embed` mode the click STARTS a session as the viewer — POST
// /sessions/api/start, the same route and the same starter check the
// new-session dialog itself goes through — and mounts that session's chat in
// the control's place. Embed grants nothing extra: the route authorizes the
// person who clicked, never this declaration.
import * as React from "react";
import { buttonVariants } from "@ap/design";
import { ChatFrame } from "./chat";
import { p, s } from "./props";
import { sessionRefSegments } from "./sessionRef";
import type { Node } from "./types";

// dns1123LabelRe mirrors the server's own check (checkAgentLink,
// pkg/web/uicomponents/components.go, via k8s.io/apimachinery/pkg/util/validation.IsDNS1123Label):
// lowercase alphanumerics and "-", 1-63 chars, no leading/trailing "-".
const dns1123LabelRe = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;

function isDNS1123Label(v: string): boolean {
  return v.length > 0 && v.length <= 63 && dns1123LabelRe.test(v);
}

// agentLinkHref builds the one URL this control can ever produce. `new=1`
// tells /sessions to open the new-session dialog rather than the list;
// ns/agentClass pick which agent it is for; prompt (when non-empty)
// prefills the dialog's message. URLSearchParams both encodes and orders the
// query deterministically, which is what makes the well-formed-case
// assertion in agentlink.test.tsx pin an exact string rather than a regex.
//
// The server's own Check (checkAgentLink) is the only real gate — this is
// the SAME rule enforced a second time here, because namespace/agentClass
// are spliced into a same-origin URL this control opens in a new tab: a
// value that slipped past the server (an older bundle, a future author
// bypassing Check) must not be trusted to be address-shaped just because it
// decoded as a string. Throwing here — rather than building a malformed
// href — is what lets renderNode.tsx's eager-invocation catch turn a bad
// declaration into the visible FailedComponent card instead of a link to
// wherever the string happened to point.
export function agentLinkHref(n: Node): string {
  const namespace = s(n, "namespace");
  const agentClass = s(n, "agentClass");
  const label = s(n, "label");
  if (!isDNS1123Label(namespace)) {
    throw new Error(`ap:agentlink: namespace ${JSON.stringify(namespace)} is not a DNS-1123 label`);
  }
  if (!isDNS1123Label(agentClass)) {
    throw new Error(`ap:agentlink: agentClass ${JSON.stringify(agentClass)} is not a DNS-1123 label`);
  }
  if (label === "") {
    throw new Error("ap:agentlink: label is required");
  }
  const q = new URLSearchParams({ new: "1", ns: namespace, agentClass });
  const prompt = s(n, "prompt");
  if (prompt !== "") q.set("prompt", prompt);
  return `/sessions?${q.toString()}`;
}

// EmbeddedAgentLink is the embed mode: the click starts the session AS THE
// VIEWER — the same POST /sessions/api/start the new-session dialog uses,
// same body, same starter check — and mounts the chat for the returned session
// in this control's place. The ref is remembered per (namespace, agentClass)
// in sessionStorage so a reload shows the same chat and a second render can
// never start a second session; the agent's own repaint with an explicit
// ap:chat is the durable path this is the immediate shadow of.
function storageKey(namespace: string, agentClass: string): string {
  return `ap-agentlink:${namespace}/${agentClass}`;
}

// rememberedRef is the one read of the memory. A refused read degrades to
// "nothing remembered" — the Start button — which is safe; it is logged for
// the same reason the two writes below are.
function rememberedRef(key: string): string | null {
  try {
    return window.sessionStorage.getItem(key);
  } catch (err) {
    console.error("agentui: ap:agentlink could not read the remembered test session", err);
    return null;
  }
}

// remember/forget are the two writes to that memory. Storage can refuse — a
// private window, a quota — and neither failure is worth interrupting the
// person for: a failed remember costs only the reload memory (the chat is
// already mounted for this render), and a failed forget only means the dead
// ref is read again on the next reload, where this same check drops it. Both
// are logged, because a silently lost write is how the next reader of this
// file loses an hour.
function remember(key: string, ref: string): void {
  try {
    window.sessionStorage.setItem(key, ref);
  } catch (err) {
    console.error("agentui: ap:agentlink could not remember the test session", err);
  }
}

function forget(key: string): void {
  try {
    window.sessionStorage.removeItem(key);
  } catch (err) {
    console.error("agentui: ap:agentlink could not forget the remembered test session", err);
  }
}

// sessionsTabHref addresses one session on the sessions page. It is the
// FALLBACK for "Open in a tab": the start response carries the server's own
// href and that is preferred, but a remembered ref survives the reload the
// response did not.
function sessionsTabHref(ref: string): string {
  return `/sessions?${new URLSearchParams({ session: ref }).toString()}`;
}

// sameOriginPath keeps the start response's href inside this control's one
// promise — that it can send a person exactly one place. StartResponse.Href
// (pkg/web/webui/sessions/start.go) is documented as a same-origin RELATIVE
// path; anything else is refused here and the rebuilt address is used
// instead. Two second characters leave the origin: "//host" is
// protocol-relative, and "/\host" is the backslash spelling browsers fold
// into it when they parse an href. An absent href is the ordinary case for a
// remembered ref and is not logged; a present-but-refused one is.
function sameOriginPath(href: unknown): string | null {
  if (typeof href !== "string") return null;
  if (!href.startsWith("/") || href.startsWith("//") || href.startsWith("/\\")) {
    console.error("agentui: ap:agentlink refused a start href that is not a same-origin path; using the rebuilt address", href);
    return null;
  }
  return href;
}

// startFailure is what the person reads after a start that produced no
// session, plus whether pressing Start again is safe. It is NOT safe when the
// server never answered: it may have created the session anyway, and a retry
// would then start a second one.
type startFailure = { text: string; retryable: boolean };

// The two fixed sentences. The refusal fallback is only for a body that is not
// the {error} shape the start route answers with (an HTML system page from a
// route-level gate, say); the indeterminate one mirrors
// pkg/web/webui/sessions/ui/NewSessionDialog.tsx's own wording for the same
// case, in this control's words.
const startRefusedFallback = "The test could not start.";
const startIndeterminate = "We could not confirm whether that session started. Reload before trying again.";

export function EmbeddedAgentLink({ n }: { n: Node }): JSX.Element {
  // Builds the "open it in a tab" address the failure line falls back to, by
  // the same validation the tab flow runs. It cannot fail visibly from here:
  // the registry entry for ap:agentlink (registry.tsx) already called
  // agentLinkHref EAGERLY, so a bad address became renderNode's failed card
  // before this component was ever constructed. It stays ahead of the hooks so
  // a future caller rendering this component directly still throws before any
  // hook has run, rather than part-way through a render.
  const tabHref = agentLinkHref(n);
  const namespace = s(n, "namespace");
  const agentClass = s(n, "agentClass");
  const key = storageKey(namespace, agentClass);
  const [ref, setRef] = React.useState<string | null>(() => rememberedRef(key));
  const [openHref, setOpenHref] = React.useState<string | null>(null);
  const [starting, setStarting] = React.useState(false);
  const [failure, setFailure] = React.useState<startFailure | null>(null);

  // A remembered ref outlives the session it names: "Start a fresh test"
  // DELETES the old session, and a repainted link would otherwise mount that
  // dead session's chat behind an access-denied frame and never offer Start
  // again. So a remembered ref is verified once, on mount, against the same
  // detail route the framed chat reads — authorized on the viewer's own
  // interact, exactly like the page. The frame mounts immediately and stays
  // until the check answers: the reachable case is the common one, and
  // blanking the chat on every reload while a request is in flight would
  // flicker it for no reason.
  React.useEffect(() => {
    const remembered = rememberedRef(key);
    if (remembered === null) return;
    let live = true;
    const drop = () => {
      if (!live) return;
      forget(key);
      setRef(null);
    };
    const segs = sessionRefSegments(remembered);
    if (segs === null) {
      // Nothing can address it, so nothing can check it either.
      drop();
      return;
    }
    void (async () => {
      try {
        const res = await fetch(`/sessions/api/${encodeURIComponent(segs[0])}/${encodeURIComponent(segs[1])}/detail`, {
          credentials: "same-origin",
        });
        // A 404 or a 403 IS the answer to "is it still mine to open" — the
        // expected shape of a session that was stopped, so it is not logged.
        if (!res.ok) drop();
      } catch (err) {
        console.error("agentui: ap:agentlink could not check the remembered test session", err);
        drop();
      }
    })();
    return () => {
      live = false;
    };
  }, [key]);

  if (ref !== null) {
    return (
      <div className="flex flex-col gap-2">
        <ChatFrame sessionRef={ref} />
        <a data-testid="ap-agentlink-open-tab" href={openHref ?? sessionsTabHref(ref)} target="_blank" rel="noopener noreferrer" className="self-end text-xs text-[hsl(var(--primary))]">
          Open in a tab
        </a>
      </div>
    );
  }

  async function start() {
    setStarting(true);
    setFailure(null);
    let res: Response;
    try {
      res = await fetch("/sessions/api/start", {
        method: "POST",
        credentials: "same-origin",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ ns: namespace, agentClass, prompt: s(n, "prompt") }),
      });
    } catch (err) {
      console.error("agentui: ap:agentlink embed start did not complete", err);
      setFailure({ text: startIndeterminate, retryable: false });
      setStarting(false);
      return;
    }
    if (!res.ok) {
      // A status the server CHOSE: nothing was created, so a retry is safe.
      // Its own {error} sentence says which refusal this was, which the
      // generic line cannot.
      let message = "";
      try {
        const parsed = (await res.json()) as { error?: string };
        message = typeof parsed?.error === "string" ? parsed.error : "";
      } catch {
        message = "";
      }
      console.error("agentui: ap:agentlink embed start was refused", { status: res.status });
      setFailure({ text: message || startRefusedFallback, retryable: true });
      setStarting(false);
      return;
    }
    try {
      const body = (await res.json()) as { ns?: string; name?: string; href?: string };
      if (!body.ns || !body.name) throw new Error("start answered without a session");
      const next = `${body.ns}/${body.name}`;
      remember(key, next);
      setOpenHref(sameOriginPath(body.href));
      setRef(next);
    } catch (err) {
      // A success whose body cannot be addressed. The session almost certainly
      // EXISTS — the server said so — so this is indeterminate, not a refusal,
      // and the button stays down for the same reason.
      console.error("agentui: ap:agentlink embed start succeeded but its answer could not be addressed", err);
      setFailure({ text: startIndeterminate, retryable: false });
    } finally {
      setStarting(false);
    }
  }

  return (
    <div className="flex flex-col gap-2">
      <button
        type="button"
        data-testid="ap-agentlink-embed"
        onClick={() => void start()}
        disabled={starting || failure?.retryable === false}
        className={buttonVariants({ variant: "default" })}
      >
        {s(n, "label")}
      </button>
      {failure !== null && (
        <p role="alert" className="text-sm text-[hsl(var(--destructive))]">
          {failure.text}
          {failure.retryable && (
            <>
              {" "}
              Try again, or <a href={tabHref} target="_blank" rel="noopener noreferrer" className="underline">open it in a tab</a>.
            </>
          )}
        </p>
      )}
    </div>
  );
}

export function AgentLink({ n }: { n: Node }): JSX.Element {
  if (Boolean(p(n).embed)) return <EmbeddedAgentLink n={n} />;
  return (
    <a
      data-testid="ap-agentlink"
      href={agentLinkHref(n)}
      target="_blank"
      rel="noopener noreferrer"
      className={buttonVariants({ variant: "default" })}
    >
      {s(n, "label")}
    </a>
  );
}
