import { useEffect, useRef, useState } from "react";
import type { ChatFrame, ConnState } from "./types";

// parseChatFrame safely parses one server frame; returns null on bad JSON or a
// frame with no type (so the caller can ignore it without throwing).
export function parseChatFrame(raw: string): ChatFrame | null {
  try {
    const m = JSON.parse(raw) as ChatFrame;
    if (!m || typeof m.type !== "string") return null;
    return m;
  } catch {
    return null;
  }
}

// wsURLFor builds the ws(s):// URL for a session's chat socket from a
// location-like value plus the session's namespace and name. The session is
// addressed by PATH, which is what the route-level Authorize gate reads
// (pkg/web/webui/chat's Routes) — so the socket cannot ask for one session while
// the gate checked another. Both halves are percent-encoded: a namespace or
// name is a single path segment and must never be able to introduce one.
export function wsURLFor(loc: { protocol: string; host: string }, ns: string, name: string): string {
  const proto = loc.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${loc.host}/sessions/api/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/ws`;
}

// OFFLINE_AFTER is the consecutive-failure count past which the indicator stops
// claiming "reconnecting" and reports a sustained outage. Retries continue in
// the background regardless; this only governs what the user sees.
const OFFLINE_AFTER = 3;

// useChatSocket connects to a chat session's push-only WebSocket, calls
// onFrame for each parsed frame, and exposes a connection state. Mirrors
// artifactview's useLiveSocket (2s-backoff reconnect,
// connecting/connected/reconnecting/offline states). The session is named by
// (ns, name) and re-opens if either changes. onFrame is held in a ref so a
// changing callback identity doesn't churn the socket.
//
// An empty ns or name is a session this hook cannot address. Opening a socket
// anyway would ask the server for `/sessions/api///ws`, get a 404, and enter a
// reconnect loop that looks like a flaky network rather than the contract
// violation it is — so it opens nothing and says why, rather than failing
// silently in either direction.
export function useChatSocket(
  ns: string,
  name: string,
  onFrame: (f: ChatFrame) => void,
): { state: ConnState } {
  const [state, setState] = useState<ConnState>("connecting");
  const cbRef = useRef(onFrame);
  cbRef.current = onFrame;

  useEffect(() => {
    if (!ns || !name) {
      console.error("chat: no live socket opened; this view was given no session to address", { ns, name });
      setState("offline");
      return;
    }

    let ws: WebSocket | null = null;
    let timer: number | undefined;
    let closed = false;
    let failures = 0;

    // onDrop advances the failure counter and reflects it: a brief blip reads
    // as "reconnecting", a sustained one as "offline". Either way a retry is
    // queued.
    const onDrop = () => {
      if (closed) return;
      failures += 1;
      setState(failures >= OFFLINE_AFTER ? "offline" : "reconnecting");
      timer = window.setTimeout(connect, 2000);
    };

    const connect = () => {
      if (closed) return;
      setState(failures === 0 ? "connecting" : failures >= OFFLINE_AFTER ? "offline" : "reconnecting");
      try {
        ws = new WebSocket(wsURLFor(window.location, ns, name));
      } catch {
        onDrop();
        return;
      }
      ws.onopen = () => {
        failures = 0;
        setState("connected");
      };
      ws.onmessage = (ev) => {
        const f = parseChatFrame(ev.data as string);
        if (f) cbRef.current(f);
      };
      ws.onerror = () => {
        // onclose follows onerror; let onclose own the retry so we don't
        // double-schedule. Reflecting the drop here keeps the indicator honest.
        if (!closed) setState(failures >= OFFLINE_AFTER ? "offline" : "reconnecting");
      };
      ws.onclose = onDrop;
    };
    connect();

    return () => {
      closed = true;
      if (timer) window.clearTimeout(timer);
      if (ws) ws.close();
    };
  }, [ns, name]);

  return { state };
}
