import * as React from "react";
import { getState, type AppState } from "./api";

export interface UseAppStateResult {
  state: AppState | null;
  error: string | null;
}

// useAppState fetches the desktop lifecycle snapshot once via GET /api/state,
// then keeps it live over GET /api/events — the settings server
// (handleEvents) streams plain unnamed `data: <json>` SSE frames, so a real
// EventSource delivers each one through onmessage (there is no named-event
// channel to subscribe to, unlike adminui's session stream).
//
// An EventSource error (the connection stalled; the browser is retrying
// underneath) is surfaced through `error` but is deliberately non-fatal: the
// last known `state` is left in place rather than cleared, because a stale
// snapshot is still a better answer than a blank screen while the browser's
// own auto-reconnect works in the background. A later message that succeeds
// clears the error again.
export function useAppState(apiBase: string): UseAppStateResult {
  const [state, setState] = React.useState<AppState | null>(null);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    let closed = false;

    getState(apiBase)
      .then((s) => {
        if (closed) return;
        setState(s);
        setError(null);
      })
      .catch((e: Error) => {
        if (!closed) setError(e.message);
      });

    const es = new EventSource(`${apiBase}/events`);
    es.onmessage = (ev) => {
      if (closed) return;
      try {
        setState(JSON.parse(ev.data) as AppState);
        setError(null);
      } catch {
        // Malformed frame — keep the last known state rather than clearing it.
      }
    };
    es.onerror = () => {
      if (!closed) setError("settings: live update stream stalled");
    };

    return () => {
      closed = true;
      es.close();
    };
  }, [apiBase]);

  return { state, error };
}
