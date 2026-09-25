import { useEffect, useRef, useState } from "react";
import type { ConnState, LiveMessage } from "./types";

// parseLiveMessage safely parses one server frame; returns null on bad JSON or a
// frame with no type (so the caller can ignore it without throwing).
export function parseLiveMessage(raw: string): LiveMessage | null {
  try {
    const m = JSON.parse(raw) as LiveMessage;
    if (!m || typeof m.type !== "string") return null;
    return m;
  } catch {
    return null;
  }
}

// wsURLFrom builds the ws(s):// URL for the live socket from a location-like value,
// preserving the d+sig query the link carries.
export function wsURLFrom(loc: { protocol: string; host: string; search: string }): string {
  const proto = loc.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${loc.host}/artifact-view/ws${loc.search}`;
}

// OFFLINE_AFTER is the consecutive-failure count past which the indicator stops
// claiming "reconnecting" and reports a sustained outage. Retries continue in the
// background regardless; this only governs what the user sees.
const OFFLINE_AFTER = 3;

// useLiveSocket connects to the live-view WebSocket, calls onMessage for each
// parsed frame, and exposes a richer connection state. It reconnects with a 2s
// backoff when the socket closes (mirrors the original shell): the first attempt
// shows "connecting", a successful open shows "connected" and resets the failure
// counter, and each failed open/close shows "reconnecting" until the failures
// reach OFFLINE_AFTER, after which it shows "offline" (still retrying every 2s).
// onMessage is held in a ref so a changing callback identity doesn't churn the
// socket.
export function useLiveSocket(
  onMessage: (m: LiveMessage) => void,
  // onOffline fires once when a transient drop becomes a sustained outage. The
  // socket can't see the HTTP status of a failed upgrade, so the caller uses
  // this to probe whether the cause is a lapsed session (recoverable) or a real
  // network outage (the socket keeps retrying either way).
  onOffline?: () => void,
): { state: ConnState } {
  const [state, setState] = useState<ConnState>("connecting");
  const cbRef = useRef(onMessage);
  cbRef.current = onMessage;
  const offlineRef = useRef(onOffline);
  offlineRef.current = onOffline;

  useEffect(() => {
    let ws: WebSocket | null = null;
    let timer: number | undefined;
    let closed = false;
    let failures = 0;

    // onDrop advances the failure counter and reflects it: a brief blip reads as
    // "reconnecting", a sustained one as "offline". Either way a retry is queued.
    const onDrop = () => {
      if (closed) return;
      failures += 1;
      setState(failures >= OFFLINE_AFTER ? "offline" : "reconnecting");
      // Exactly when a blip crosses into a sustained outage, give the caller one
      // chance to detect a lapsed session (vs a real drop). Fires once per
      // episode; a successful reconnect resets the counter so a later outage
      // fires it again.
      if (failures === OFFLINE_AFTER) offlineRef.current?.();
      timer = window.setTimeout(connect, 2000);
    };

    const connect = () => {
      if (closed) return;
      // A fresh attempt after a drop keeps the user-visible state (offline stays
      // offline, otherwise reconnecting); the very first attempt is "connecting".
      setState(failures === 0 ? "connecting" : failures >= OFFLINE_AFTER ? "offline" : "reconnecting");
      try {
        ws = new WebSocket(wsURLFrom(window.location));
      } catch {
        onDrop();
        return;
      }
      ws.onopen = () => {
        failures = 0;
        setState("connected");
      };
      ws.onmessage = (ev) => {
        const m = parseLiveMessage(ev.data as string);
        if (m) cbRef.current(m);
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
  }, []);

  return { state };
}
