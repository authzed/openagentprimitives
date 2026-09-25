// usePresenceHeartbeat tells the platform that a person is actually looking at
// this agent-defined UI, so the runner answering its data bindings does not
// idle out underneath them.
//
// A runner's lifetime used to be shaped by CONVERSATION, but a declared data
// binding is answered by that runner with no conversation at all. So a viewer
// could be driving a dashboard — changing a date range, watching a table
// re-resolve — while the session, from the loop's point of view, sat silent,
// and the runner exited. Observed on a live cluster: two turns in six seconds,
// then exit at 5m19s, after which every binding timed out for want of a
// subscriber.
//
// The conditions are evaluated HERE and nowhere else. Visibility, focus and
// "did this person touch anything recently" are facts only the tab holds; webd
// deliberately does not re-derive them, because the only thing it could
// observe is "the socket is open", which would keep a pod alive for a window
// somebody forgot about.
import { useEffect, useRef } from "react";

// HEARTBEAT_MS is how often a watched, focused, recently-used tab reports in.
// webd floors republication at its own interval, so sending faster buys
// nothing; this is chosen to sit comfortably inside that floor's multiple
// while still being far shorter than any idle TTL.
const HEARTBEAT_MS = 20_000;

// INTERACTION_WINDOW_MS is how long an interaction keeps counting. A tab that
// is visible and focused but untouched for longer than this stops reporting:
// "focused" alone is satisfied by a window left open on a second monitor, and
// holding a runner for that is exactly the cost this signal has to bound.
const INTERACTION_WINDOW_MS = 5 * 60_000;

// INTERACTION_EVENTS are what counts as touching the page. Deliberately
// includes scroll and keydown, not only clicks: reading a long table is
// use, and a viewer who is scrolling one is precisely the person who would
// notice the data going stale.
const INTERACTION_EVENTS = ["pointerdown", "keydown", "scroll", "wheel"] as const;

export interface PresenceHeartbeatOptions {
  // send delivers one heartbeat. Separate from the socket itself so this hook
  // owns WHEN to report and the caller owns HOW — the caller already manages
  // the socket's reconnect lifecycle, and a hook that reached into it would
  // have to duplicate that.
  send: () => void;
  // enabled gates the whole hook. False for a view with no live socket, so a
  // caller never has to conditionally call a hook.
  enabled?: boolean;
}

// usePresenceHeartbeat reports while the tab is visible, focused, and has been
// interacted with inside INTERACTION_WINDOW_MS.
//
// It reports on a timer rather than on each interaction, deliberately: a
// per-event send would turn a scroll into a burst, and the signal is
// idempotent — "still watching" gains nothing from being said twice quickly.
export function usePresenceHeartbeat({ send, enabled = true }: PresenceHeartbeatOptions): void {
  // Held in a ref so a caller whose `send` identity changes every render does
  // not tear down and rebuild the timer and every listener on each render.
  const sendRef = useRef(send);
  sendRef.current = send;

  useEffect(() => {
    if (!enabled) return;
    if (typeof window === "undefined" || typeof document === "undefined") return;

    // Seeded to now: opening the page IS an interaction. Without this a viewer
    // who opens a dashboard and reads it without touching anything would send
    // nothing at all, which is the one case the whole signal exists for.
    let lastInteraction = Date.now();
    const mark = () => {
      lastInteraction = Date.now();
    };
    for (const ev of INTERACTION_EVENTS) {
      window.addEventListener(ev, mark, { passive: true });
    }

    const watching = (): boolean => {
      if (document.visibilityState !== "visible") return false;
      if (typeof document.hasFocus === "function" && !document.hasFocus()) return false;
      return Date.now() - lastInteraction < INTERACTION_WINDOW_MS;
    };

    // Report immediately as well as on the interval, so a freshly-opened
    // dashboard holds its runner from the first moment rather than after one
    // whole interval of silence.
    if (watching()) sendRef.current();
    const timer = window.setInterval(() => {
      if (watching()) sendRef.current();
    }, HEARTBEAT_MS);

    return () => {
      window.clearInterval(timer);
      for (const ev of INTERACTION_EVENTS) {
        window.removeEventListener(ev, mark);
      }
    };
  }, [enabled]);
}

// Exported for tests and for the caller that needs to reason about cadence.
export const PRESENCE_HEARTBEAT_MS = HEARTBEAT_MS;
export const PRESENCE_INTERACTION_WINDOW_MS = INTERACTION_WINDOW_MS;
