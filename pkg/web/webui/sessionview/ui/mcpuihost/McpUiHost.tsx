import * as React from "react";
import { UIResourceRenderer, type UIActionResult, type UIActionResultToolCall } from "@mcp-ui/client";

export interface McpUiHostProps {
  /** ui://widget/<artifactID> — a stable per-widget identifier (see widgets.go's mcpUiHostBootstrap). */
  uri: string;
  /** The widget's raw, unsanitized HTML bytes (see pkg/channels/channelassets/mcpui's package doc). */
  html: string;
  title?: string;
  /**
   * The trusted SessionView shell's origin — threaded from
   * widgets.go's mcpUiHostBootstrap.trustedOrigin (server-sanitized, never
   * client-supplied). logUIAction uses this as postMessage's target origin;
   * NEVER pass/derive "*" here.
   */
  trustedOrigin: string;
}

// logUIAction is Plan 5's sandbox->trusted bridge: it logs the widget's UI
// action (tool/prompt/link/intent/notify — see @mcp-ui/client's
// UIActionResult) for local debugging, then relays it UP to the trusted
// SessionView shell via postMessage — mirrors artifactview/ui/host/boot.ts's
// `parent.postMessage({ap:"annot",...}, shellOrigin)` bridge exactly, down to
// never passing "*" as the target origin. It deliberately does NOT call a
// tool, does NOT fetch, and does NOT reach the agent runner directly — this
// frame has no network path under buildWidgetCSP's connect-src (and no
// business making one): the trusted shell is the only party that fetches
// /interact, after re-validating origin+source+tag itself (SessionView.tsx).
// Keeping this side-effect-limited-to-postMessage is the load-bearing
// contract this task's security guardrail requires: this iframe's own
// postMessage is the ONLY channel out, and the shell treats whatever arrives
// on it as fully untrusted (see SessionView.tsx's listener doc).
export async function logUIAction(result: UIActionResult, trustedOrigin: string): Promise<void> {
  console.log("[mcpui-host] UI action received, relaying to the trusted shell:", result);
  try {
    window.parent.postMessage({ ap: "mcpui", cmd: "action", result }, trustedOrigin);
  } catch (err) {
    // No network path exists under this frame's CSP to report this any other
    // way; a console entry at least leaves a local trace instead of silence.
    console.error("[mcpui-host] postMessage to the trusted shell failed", err);
  }
}

// --- appToolCall correlation protocol --------------------------------------
//
// `tool` UIActions are the one variant the widget expects a REPLY to (its
// readonly result rendered back into the widget) rather than fire-and-forget
// logging. Since this frame still has no network path of its own (same CSP
// as logUIAction above), the round-trip has to leave via the same single
// postMessage channel and come back the same way — a correlated request/reply
// pair layered on top of it, NOT a second channel. This is a SEPARATE
// correlation from @mcp-ui/client's own internal `messageId` (which relays
// onUIAction's resolved return value into the widget's iframe) — that one is
// handled entirely inside the library and never touched here; this one is
// the host-frame<->trusted-shell hop the plan's protocol block describes.
//
// The wire shapes (Task 3 / SessionView.tsx's shell listener MUST match
// verbatim):
//   host -> shell : { ap:"mcpui", cmd:"appToolCall", corrId, result }
//   shell -> host : { ap:"mcpui", cmd:"appToolCallReply", corrId, response }

// CallToolResultLike is the minimal MCP CallToolResult shape the shell's
// reply carries back — just enough to render into the widget via onUIAction's
// return value (@mcp-ui/client relays whatever onUIAction resolves to, back
// to the widget, by its own internal messageId).
export interface CallToolResultLike {
  content: Array<{ type: "text"; text: string }>;
  isError: boolean;
}

// A client-side backstop slightly ABOVE the server's own 30s
// /app-tool-call budget (pkg/web/webui/interact/apptoolcall.go's
// appToolCallTimeout) — if the shell's reply never arrives (network hiccup,
// a shell bug, the tab losing focus mid-flight), the widget still gets an
// answer instead of hanging on an unresolved onUIAction Promise forever.
const APP_TOOL_CALL_TIMEOUT_MS = 35_000;

// Fallback corrId source when crypto.randomUUID is unavailable (older
// embedded webviews). Deliberately NOT Math.random — a monotonic counter is
// unique for the lifetime of this module, which is all correlation needs.
let fallbackCorrIdCounter = 0;

function mintCorrId(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  fallbackCorrIdCounter += 1;
  return `mcpui-corr-${fallbackCorrIdCounter}`;
}

function errorResult(text: string): CallToolResultLike {
  return { content: [{ type: "text", text }], isError: true };
}

function isCallToolResultLike(value: unknown): value is CallToolResultLike {
  if (!value || typeof value !== "object") return false;
  const v = value as { content?: unknown; isError?: unknown };
  if (typeof v.isError !== "boolean") return false;
  if (!Array.isArray(v.content)) return false;
  return v.content.every(
    (item): item is { type: "text"; text: string } =>
      !!item && typeof item === "object" && (item as { type?: unknown }).type === "text" && typeof (item as { text?: unknown }).text === "string",
  );
}

// pendingAppToolCalls is module-level (not component-instance state) because
// the reply listener below is installed exactly once per module load and
// must be able to resolve whichever pending corrId a reply names, regardless
// of which McpUiHost render (there is normally exactly one per frame) issued
// it. Each entry's function both resolves the caller's Promise AND clears
// its timeout + removes itself from the map — see sendAppToolCall's `settle`.
const pendingAppToolCalls = new Map<string, (response: CallToolResultLike) => void>();

// trustedOrigin is a per-host constant, threaded once via McpUiHostProps from
// the server-sanitized bootstrap (widgets.go's mcpUiHostBootstrap) — never
// client-supplied and never changes across this frame's lifetime. Stashing
// the latest value here lets the single module-level reply listener validate
// incoming replies against it without needing per-listener closures.
let replyListenerTrustedOrigin: string | null = null;
let replyListenerInstalled = false;

// handleAppToolCallReply is the reply-hop half of the confused-deputy
// defense the plan's Global Constraints call for: ALL THREE checks must pass
// before a pending corrId is ever resolved from message data. Anything that
// fails validation is silently ignored (not an error — most `message` events
// on `window` are unrelated to this protocol entirely).
function handleAppToolCallReply(e: MessageEvent): void {
  if (e.origin !== replyListenerTrustedOrigin) return;
  if (e.source !== window.parent) return;
  const data = e.data as { ap?: string; cmd?: string; corrId?: unknown; response?: unknown } | null | undefined;
  if (!data || data.ap !== "mcpui" || data.cmd !== "appToolCallReply" || typeof data.corrId !== "string") return;

  const settle = pendingAppToolCalls.get(data.corrId);
  if (!settle) return; // no longer pending (already timed out, or a stray/duplicate reply)

  settle(isCallToolResultLike(data.response) ? data.response : errorResult("The action returned a malformed response."));
}

function ensureReplyListenerInstalled(): void {
  if (replyListenerInstalled) return;
  replyListenerInstalled = true;
  window.addEventListener("message", handleAppToolCallReply);
}

// sendAppToolCall is the correlated counterpart to logUIAction, used ONLY
// for `tool` UIActions: it posts an `appToolCall` message tagged with a
// fresh corrId, and returns a Promise that resolves once the matching
// `appToolCallReply` arrives (or the client-side timeout above fires) — the
// resolved CallToolResultLike is what onUIAction returns, which
// @mcp-ui/client then relays into the widget via its own messageId
// mechanism.
export function sendAppToolCall(result: UIActionResultToolCall, trustedOrigin: string): Promise<CallToolResultLike> {
  replyListenerTrustedOrigin = trustedOrigin;
  ensureReplyListenerInstalled();

  const corrId = mintCorrId();

  return new Promise<CallToolResultLike>((resolve) => {
    let settled = false;
    const settle = (response: CallToolResultLike) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      pendingAppToolCalls.delete(corrId);
      resolve(response);
    };

    const timer = setTimeout(() => settle(errorResult("The action timed out.")), APP_TOOL_CALL_TIMEOUT_MS);

    pendingAppToolCalls.set(corrId, settle);

    try {
      window.parent.postMessage({ ap: "mcpui", cmd: "appToolCall", corrId, result }, trustedOrigin);
    } catch (err) {
      // Mirrors logUIAction's try/catch: no other channel exists to report
      // this, and the widget must never be left hanging on a postMessage
      // throw — resolve an isError result instead of a dangling Promise.
      console.error("[mcpui-host] postMessage to the trusted shell failed", err);
      settle(errorResult("Failed to send the action to the trusted shell."));
    }
  });
}

// routeUIAction is onUIAction's branch, factored out so it's directly
// testable without rendering @mcp-ui/client's UIResourceRenderer: `tool`
// actions get the correlated sendAppToolCall round-trip; every other
// UIActionType (prompt/link/intent/notify) keeps the existing
// fire-and-forget logUIAction.
export function routeUIAction(result: UIActionResult, trustedOrigin: string): Promise<unknown> {
  return result.type === "tool" ? sendAppToolCall(result, trustedOrigin) : logUIAction(result, trustedOrigin);
}

// usePreferredTheme reads the OS/browser color-scheme preference — the
// "theme from prefers-color-scheme" host-context signal this task's brief
// calls for. Deliberately NOT @ap/runtime's ThemeProvider, which hardcodes
// the trusted shell's dark design theme and pulls in the full @ap/design
// stylesheet — inappropriate for this tightly CSP-locked sandbox-origin
// bundle (see index.tsx's doc comment for the full reasoning).
function usePreferredTheme(): "dark" | "light" {
  const query = "(prefers-color-scheme: dark)";
  const [theme, setTheme] = React.useState<"dark" | "light">(() =>
    typeof window !== "undefined" && typeof window.matchMedia === "function" && window.matchMedia(query).matches
      ? "dark"
      : "light",
  );
  React.useEffect(() => {
    if (typeof window === "undefined" || typeof window.matchMedia !== "function") return;
    const mq = window.matchMedia(query);
    const onChange = (e: MediaQueryListEvent) => setTheme(e.matches ? "dark" : "light");
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);
  return theme;
}

// useContainerSize reports this component's own rendered box — the
// "container dims" half of the host contract — via ResizeObserver, so the
// widget can be told how much room it actually has.
function useContainerSize(ref: React.RefObject<HTMLElement>): { width: number; height: number } {
  const [size, setSize] = React.useState({ width: 0, height: 0 });
  React.useEffect(() => {
    const el = ref.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (!entry) return;
      setSize({ width: entry.contentRect.width, height: entry.contentRect.height });
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [ref]);
  return size;
}

// McpUiHost renders the widget via @mcp-ui/client's UIResourceRenderer.
//
// SECURITY (do not change without re-reading this task's guardrail): resource
// carries mimeType "text/html" with NO `proxy` htmlProp set. That combination
// makes @mcp-ui/client@6.0.0's HTMLResourceRenderer pick its "srcDoc" render
// path, whose iframe sandbox attribute is EXACTLY "allow-scripts" — verified
// against the library's own source (dist/index.mjs): srcDoc mode ->
// "allow-scripts"; its OTHER mode ("src" — used only when a `proxy` string or
// an externalUrl-typed resource is supplied) unconditionally ADDS
// "allow-same-origin", which pkg/web/webui/sessionview's sandbox-origin isolation
// model (widgets.go's package doc) forbids: /mcpui-host and /mcpui-content
// share an origin, so "allow-same-origin" there would make the widget's
// script same-origin with (and able to read/write) the host document itself.
// NEVER pass `htmlProps.proxy` or an externalUrl resource here. Passing
// supportedContentTypes={["rawHtml"]} is belt-and-suspenders: even a resource
// that somehow looked like externalUrl/remoteDom is refused outright before
// reaching either render path. remote-dom is out of scope for this task and
// is never enabled (no remoteDomProps passed).
export function McpUiHost({ uri, html, title, trustedOrigin }: McpUiHostProps) {
  const containerRef = React.useRef<HTMLDivElement>(null);
  const theme = usePreferredTheme();
  const { width, height } = useContainerSize(containerRef);

  const resource = React.useMemo(() => ({ uri, mimeType: "text/html" as const, text: html }), [uri, html]);
  const onUIAction = React.useCallback((result: UIActionResult) => routeUIAction(result, trustedOrigin), [trustedOrigin]);

  return (
    <div ref={containerRef} style={{ width: "100%", height: "100%" }}>
      <UIResourceRenderer
        resource={resource}
        onUIAction={onUIAction}
        supportedContentTypes={["rawHtml"]}
        htmlProps={{
          style: { width: "100%", height: "100%", border: "0" },
          // Host-context: theme + current container size, delivered to the
          // widget via the library's own postMessage handshake
          // (UI_LIFECYCLE_IFRAME_RENDER_DATA) — never a direct DOM/window
          // reference, since the widget frame is deliberately not
          // same-origin with this document.
          iframeRenderData: { theme, containerWidth: width, containerHeight: height },
          iframeProps: { title: title || "interactive widget" },
        }}
      />
    </div>
  );
}
