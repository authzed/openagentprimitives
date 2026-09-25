import { useCallback, useEffect, useRef, useState } from "react";
import { Download, Loader2, Menu, X } from "lucide-react";
import { Alert, AlertDescription, AlertTitle, Button, TooltipProvider } from "@ap/design";
import type { ArtifactViewProps, LiveMessage, LiveRevision, MirrorMessage, StatusSnapshot } from "./types";
import type { Bundle } from "./host/bundle";
import { interactRequestFromBundle } from "./annotationSend";
import { useLiveSocket } from "./useLiveSocket";
import { useHostBridge } from "./useHostBridge";
import { StatusBar } from "./StatusBar";
import { ThreadLink } from "./ThreadLink";
import { ConnectionIndicator } from "./ConnectionIndicator";
import { RevisionList } from "./RevisionList";
import { ChatPanel } from "./ChatPanel";
import { appendMessage, type ChatMessage } from "./chatMessages";

// toChatMessage maps the server's MirrorMessage (role is a plain string over
// the wire) onto the panel's narrower ChatMessage — an unrecognized role
// degrades to "agent" (left-aligned) rather than throwing, the same
// fail-safe-default posture as the other best-effort mirrors on this page.
function toChatMessage(m: MirrorMessage): ChatMessage {
  return { role: m.role === "user" ? "user" : "agent", text: m.text, author: m.author, at: m.at };
}

// Seamless session-lapse recovery. The revision + live-ws endpoints require the
// webd OIDC session, which can expire while a long-lived artifact view stays
// open (the signed link outlives it). Rather than show an error, we transparently
// reload the shell — its AuthLoginIfNecessary re-establishes the session,
// silently when the IdP session is still valid — and restore the revision the
// user was opening. A short re-auth marker guards against a reload loop when
// re-auth itself can't recover (e.g. the IdP session is also gone → login page).
const REAUTH_AT_KEY = "ap_artifact_reauth_at";
const PENDING_REV_KEY = "ap_artifact_pending_rev";

function recentlyReauthed(): boolean {
  const at = Number(window.sessionStorage.getItem(REAUTH_AT_KEY) || "0");
  return Number.isFinite(at) && Date.now() - at < 15000;
}

// reauthReload reloads to re-establish the session, optionally pinning a revision
// to re-open afterwards. Returns false (no reload) if we just tried, so a failed
// re-auth can't loop.
function reauthReload(pendingRev?: string): boolean {
  if (recentlyReauthed()) return false;
  window.sessionStorage.setItem(REAUTH_AT_KEY, String(Date.now()));
  if (pendingRev) window.sessionStorage.setItem(PENDING_REV_KEY, pendingRev);
  window.location.reload();
  return true;
}

export function ArtifactView(props: ArtifactViewProps) {
  const iframeRef = useRef<HTMLIFrameElement>(null);
  const showRevision = useHostBridge(iframeRef, props.sandboxOrigin);

  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [pinned, setPinned] = useState(false);
  const pinnedRef = useRef(false);
  pinnedRef.current = pinned;

  const [revisions, setRevisions] = useState<LiveRevision[]>([]);
  const [curRev, setCurRev] = useState<string | null>(null);
  const [latest, setLatest] = useState<LiveMessage | null>(null);
  const [status, setStatus] = useState<StatusSnapshot | null>(null);
  const [hasContent, setHasContent] = useState<boolean>(!!props.hostUrl);
  const [pendingRev, setPendingRev] = useState<string | null>(null);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [annotError, setAnnotError] = useState<string | null>(null);
  const latestRef = useRef<LiveMessage | null>(null);
  latestRef.current = latest;

  const onMessage = useCallback(
    (msg: LiveMessage) => {
      if (msg.type === "status") {
        if (msg.status) setStatus(msg.status);
        return;
      }
      if (msg.type === "message") {
        if (msg.message) setMessages((prev) => appendMessage(prev, toChatMessage(msg.message as MirrorMessage)));
        return;
      }
      setLatest(msg);
      if (Array.isArray(msg.revisions)) setRevisions(msg.revisions);
      if (!pinnedRef.current && (msg.current?.hostUrl || msg.current?.contentUrl)) {
        showRevision({ hostUrl: msg.current.hostUrl, contentUrl: msg.current.contentUrl });
        setCurRev(msg.current.revisionId || null);
        setHasContent(true);
      }
    },
    [showRevision],
  );

  // On a sustained socket outage, probe one authenticated request: a 401 means
  // the session lapsed (server reachable, unauthenticated) → re-auth seamlessly;
  // any other result is a genuine outage the socket keeps retrying.
  const onOffline = useCallback(() => {
    const rev = latestRef.current?.current?.revisionId;
    if (!rev) return;
    fetch(`/artifact-view/revision${window.location.search}&rev=${encodeURIComponent(rev)}`)
      .then((resp) => {
        if (resp.status === 401) reauthReload();
      })
      .catch(() => {
        /* genuine network outage — leave the socket to retry */
      });
  }, []);

  const { state: connState } = useLiveSocket(onMessage, onOffline);

  // "Working" drives the flowing bottom border + the pulsing state dot: the
  // session has a live status and isn't paused (awaiting approval / reply /
  // retry / failed all read as paused-ish and suppress the animation).
  const working = !!status && !status.paused;

  const selectRevision = useCallback(
    (revisionId: string) => {
      setPinned(true);
      fetch(`/artifact-view/revision${window.location.search}&rev=${encodeURIComponent(revisionId)}`)
        .then((resp) => {
          if (resp.status === 401) {
            // Session lapsed while the page was open: re-authenticate seamlessly
            // (the reload re-runs the shell's login) and restore this selection.
            reauthReload(revisionId);
            return null;
          }
          if (!resp.ok) throw new Error(`revision fetch failed: ${resp.status}`);
          return resp.json();
        })
        .then((data: { hostUrl?: string; contentUrl?: string } | null) => {
          if (!data) return;
          window.sessionStorage.removeItem(REAUTH_AT_KEY); // a success clears the loop-guard
          if (data.hostUrl || data.contentUrl) {
            showRevision({ hostUrl: data.hostUrl, contentUrl: data.contentUrl });
            setCurRev(revisionId);
            setHasContent(true);
          }
        })
        .catch((e) => {
          if (window.console) console.warn("view-older failed", e);
        });
    },
    [showRevision],
  );

  // downloadURL builds the same-origin /artifact-download link for this
  // artifact (optionally a specific revision). It works because the viewer and
  // the endpoint share the webd origin and the session; the endpoint responds
  // with Content-Disposition: attachment so the click is a download.
  const downloadURL = useCallback(
    (rev?: string) => {
      const params = new URLSearchParams({ artifactId: props.artifactId, sessionRef: `${props.ns}/${props.name}` });
      if (rev) params.set("rev", rev);
      return `/artifact-download?${params.toString()}`;
    },
    [props.artifactId, props.ns, props.name],
  );

  const goLive = useCallback(() => {
    setPinned(false);
    if (latest?.current?.hostUrl || latest?.current?.contentUrl) {
      showRevision({ hostUrl: latest?.current?.hostUrl, contentUrl: latest?.current?.contentUrl });
      setCurRev(latest?.current?.revisionId || null);
      setHasContent(true);
    }
  }, [latest, showRevision]);

  // After a re-auth reload, pick up the revision the user had been opening.
  useEffect(() => {
    const pending = window.sessionStorage.getItem(PENDING_REV_KEY);
    if (pending) {
      window.sessionStorage.removeItem(PENDING_REV_KEY);
      setPendingRev(pending);
    }
  }, []);

  // Re-open the pending revision once it appears in the (re-fetched) list.
  useEffect(() => {
    if (pendingRev && revisions.some((r) => r.revisionId === pendingRev)) {
      const rev = pendingRev;
      setPendingRev(null);
      selectRevision(rev);
    }
  }, [pendingRev, revisions, selectRevision]);

  // Relay the annotation bundle from the host doc (sandbox origin) to the
  // server. The host builds the bundle and posts it up via postMessage; the
  // shell is the only party trusted to reach the network, so it stamps
  // artifactId (held in props, re-checked server-side via CheckArtifactView)
  // and POSTs to /interact with the same credentials + Origin as the chat
  // compose path. A failure — non-2xx OR a network-level reject — is surfaced
  // as a dismissible banner (annotError) rather than swallowed: the host doc
  // has no visibility into the outcome, so the shell is the only place that can
  // tell the user their notes didn't reach the agent. A success clears any
  // prior banner.
  useEffect(() => {
    const onMsg = (e: MessageEvent) => {
      if (e.origin !== props.sandboxOrigin) return;
      if (e.source !== iframeRef.current?.contentWindow) return;
      const d = e.data as { ap?: string; cmd?: string; bundle?: Bundle };
      if (d?.ap !== "annot" || d.cmd !== "send" || !d.bundle) return;
      fetch(`/session/${props.ns}/${props.name}/interact`, {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(interactRequestFromBundle(d.bundle, props.artifactId)),
      })
        .then(async (r) => {
          if (!r.ok) {
            const body = (await r.json().catch(() => ({}))) as { error?: string };
            console.error("annotation send failed", r.status, body);
            setAnnotError(body.error ? body.error : `The server returned ${r.status}.`);
            return;
          }
          setAnnotError(null);
        })
        .catch((err) => {
          console.error("annotation send failed (network)", err);
          setAnnotError("The server was unreachable — check your connection and try again.");
        });
    };
    window.addEventListener("message", onMsg);
    return () => window.removeEventListener("message", onMsg);
  }, [props.sandboxOrigin, props.ns, props.name, props.artifactId]);

  // Compose-box send: POST a free-text user_message to /interact. On a non-2xx
  // this throws so ChatPanel keeps the draft (no clear-on-failure); the sent
  // text is never appended optimistically — it renders once its user_echo
  // round-trips on the live stream (see toChatMessage above), same as any
  // other outbound line.
  const onSend = useCallback(
    async (text: string) => {
      const r = await fetch(`/session/${props.ns}/${props.name}/interact`, {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ kind: "user_message", artifactId: props.artifactId, payload: { text } }),
      });
      if (!r.ok) {
        const e = await r.json().catch(() => ({}));
        console.error("send failed", r.status, e);
        throw new Error(e?.error || "send failed"); // ChatPanel keeps the draft on throw
      }
    },
    [props.ns, props.name, props.artifactId],
  );

  return (
    <TooltipProvider delayDuration={200}>
      <div className="h-screen flex flex-col bg-background text-foreground">
        <div className="relative flex items-center gap-3 px-3 py-2 min-h-[52px] bg-card border-b border-border">
          <Button
            variant="ghost"
            size="icon"
            className="h-7 w-7 text-muted-foreground"
            title="Toggle revisions"
            onClick={() => setSidebarOpen((o) => !o)}
          >
            <Menu className="h-4 w-4" />
          </Button>
          <div className="flex flex-col min-w-0 max-w-[46%] shrink">
            <span className="text-[13px] font-semibold leading-[1.25] truncate" title={props.artifactName}>
              {props.artifactName}
            </span>
            {props.artifactDescription && (
              <span className="text-[11px] text-muted-foreground leading-[1.3] truncate" title={props.artifactDescription}>
                {props.artifactDescription}
              </span>
            )}
          </div>
          <div className="w-px h-[26px] bg-border shrink-0" />
          <StatusBar status={status} />
          <div className="flex items-center gap-2 ml-auto shrink-0">
            <a
              href={downloadURL(pinned && curRev ? curRev : undefined)}
              download
              aria-label="Download"
              title="Download"
              className="flex h-7 w-7 items-center justify-center rounded-md text-muted-foreground hover:bg-accent hover:text-accent-foreground"
            >
              <Download className="h-4 w-4" aria-hidden="true" />
            </a>
            <ThreadLink href={props.backLink} channelKind={props.channelKind} />
            {pinned && (
              <Button variant="secondary" size="sm" className="h-7 gap-1.5 px-2.5" title="Follow latest" onClick={goLive}>
                <span className="inline-block h-[7px] w-[7px] rounded-full bg-success" />
                Live
              </Button>
            )}
            <ConnectionIndicator state={connState} />
          </div>
          {working && (
            <div
              aria-hidden="true"
              className="absolute left-0 right-0 bottom-0 h-[2px] bg-[linear-gradient(90deg,transparent,hsl(var(--success)),transparent)] bg-[length:50%_100%] animate-ap-flow"
            />
          )}
        </div>
        <div className="flex flex-1 min-h-0">
          <aside className={`bg-card border-r border-border overflow-hidden transition-[width] ${sidebarOpen ? "w-[300px] overflow-y-auto" : "w-0"}`}>
            <RevisionList revisions={revisions} currentRevId={curRev} onSelect={selectRevision} downloadHref={downloadURL} />
          </aside>
          <div className="relative flex-1 h-full">
            <iframe
              ref={iframeRef}
              className="border-0 w-full h-full block bg-white"
              sandbox="allow-scripts allow-same-origin"
              src={props.hostUrl || undefined}
              referrerPolicy="no-referrer"
              title="artifact content"
            />
            {!hasContent && (
              <div className="absolute inset-0 flex flex-col items-center justify-center gap-2 bg-background text-muted-foreground">
                <Loader2 className="h-5 w-5 animate-spin" aria-hidden="true" />
                <span className="text-sm">Generating preview…</span>
              </div>
            )}
            {annotError && (
              <div className="absolute inset-x-3 top-3 z-10">
                <Alert variant="destructive" className="relative bg-card pr-9 shadow-lg">
                  <AlertTitle>Annotation not sent</AlertTitle>
                  <AlertDescription>{annotError}</AlertDescription>
                  <button
                    type="button"
                    aria-label="Dismiss"
                    onClick={() => setAnnotError(null)}
                    className="absolute right-2 top-2 rounded p-1 text-muted-foreground hover:bg-accent hover:text-accent-foreground"
                  >
                    <X className="h-4 w-4" aria-hidden="true" />
                  </button>
                </Alert>
              </div>
            )}
          </div>
          {props.interactions.length > 0 && (
            <div className="w-[320px] shrink-0 h-full">
              <ChatPanel messages={messages} canCompose={props.interactions.includes("user_message")} onSend={onSend} />
            </div>
          )}
        </div>
      </div>
    </TooltipProvider>
  );
}
