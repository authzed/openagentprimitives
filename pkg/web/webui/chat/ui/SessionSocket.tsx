import { createContext, useCallback, useContext, useEffect, useMemo, useReducer, useRef, type ReactNode } from "react";
import { useChatSocket } from "./useChatSocket";
import { INITIAL_SESSION_SIGNALS, RECONNECT_RESET, reduceSessionSignals, SessionSignalsContext } from "./sessionSignals";
import type { ChatFrame, ConnState } from "./types";

type Handler = (f: ChatFrame) => void;

// SessionSocketValue is one session's live channel, shared. `state` is the
// connection's own state, for whichever surface discloses it; `subscribe`
// registers a frame handler for the caller's lifetime and returns the function
// that unregisters it. Every subscriber sees every frame, in arrival order —
// there is no filtering here, because which frames matter is each subscriber's
// question and not the socket's.
export interface SessionSocketValue {
  state: ConnState;
  subscribe: (handler: Handler) => () => void;
}

// The session socket belongs to the SHELL, not to any one view of the session.
//
// A socket owned by a view exists only while that view is mounted, and the
// shell shows one of two views at a time: a viewer reading the agent-defined
// page would have no session channel at all, so nothing could tell that page
// the agent had replied or that a card was waiting on a human. One socket per
// selected session, fanned to every subscriber, is what lets both views — and
// the shell's own tab notifications — derive the same facts from the same
// frames.
//
// The default value is what a subscriber outside a provider gets: no frames,
// said out loud. Silence here is indistinguishable from a session that has
// simply gone quiet, which is the failure this logs rather than sits on.
const SessionSocketContext = createContext<SessionSocketValue>({
  state: "offline",
  subscribe: () => {
    console.error("chat: useSessionFrames outside a SessionSocketProvider; no frames will arrive");
    return () => {};
  },
});

// SessionSocketProvider holds the one socket for (ns, name), fans its frames
// to everything mounted beneath it, and runs the one SessionSignals fold over
// them.
//
// The fold lives HERE, not in each consumer, for the same reason the socket
// does: subscription carries no replay. A consumer that folded for itself
// began at INITIAL_SESSION_SIGNALS whenever it happened to mount, so a view
// the shell mounts lazily — the agent-defined page, opened for the first time
// partway through a session — knew nothing of a prompt that had arrived while
// the viewer was reading the transcript. Folding from socket open makes the
// answer a property of the SESSION rather than of when a component asked.
export function SessionSocketProvider({ ns, name, children }: { ns: string; name: string; children: ReactNode }) {
  // A ref, not state: a subscriber arriving or leaving changes nothing anyone
  // renders, and putting the set in state would re-render the whole subtree —
  // both views of the session — every time one mounted.
  //
  // Built lazily rather than as `useRef(new Set())`, which constructs a Set on
  // EVERY render and discards all but the first.
  const subs = useRef<Set<Handler> | null>(null);
  if (subs.current === null) subs.current = new Set<Handler>();

  // The session's folded signals. reduceSessionSignals returns the SAME object
  // for a frame that carries no news, so React bails out of the re-render and
  // a chatty socket does not re-render every consumer beneath this provider.
  const [signals, dispatch] = useReducer(reduceSessionSignals, INITIAL_SESSION_SIGNALS);

  // fan is the socket's single onFrame: it folds first, then hands the frame
  // on untouched. Its identity is stable (dispatch's is, by React's contract),
  // so the socket is opened once per session rather than re-opened whenever
  // this re-renders.
  const fan = useCallback((f: ChatFrame) => {
    dispatch(f);
    for (const h of subs.current!) h(f);
  }, []);
  const { state } = useChatSocket(ns, name, fan);

  const subscribe = useCallback((h: Handler) => {
    subs.current!.add(h);
    return () => {
      subs.current!.delete(h);
    };
  }, []);

  // The reconnect EDGE, not the state: "connected" is where a healthy socket
  // sits, so dispatching on the value would reset the fold on the first render
  // of every mount. The previous value lives in a ref because nothing renders
  // it — it only decides whether this is an edge. See RECONNECT_RESET for what
  // the reset does and does not clear.
  const prevState = useRef(state);
  useEffect(() => {
    const was = prevState.current;
    prevState.current = state;
    if (state === "connected" && (was === "reconnecting" || was === "offline")) dispatch(RECONNECT_RESET);
  }, [state]);

  // Memoized on `state` alone (subscribe is stable): a new context value on
  // every render would re-run every subscriber's effect — unsubscribing and
  // resubscribing each one — for no change.
  const value = useMemo(() => ({ state, subscribe }), [state, subscribe]);

  return (
    <SessionSocketContext.Provider value={value}>
      <SessionSignalsContext.Provider value={signals}>{children}</SessionSignalsContext.Provider>
    </SessionSocketContext.Provider>
  );
}

// useSessionFrames calls `handler` for every frame on the session's socket,
// for as long as the calling component is mounted.
//
// The handler is held in a ref and the subscription is made once: a handler
// whose identity changes on every render — which a frame handler closing over
// component state does — would otherwise unsubscribe and resubscribe per
// render, and a frame arriving in that window would reach nobody.
export function useSessionFrames(handler: Handler): void {
  const { subscribe } = useContext(SessionSocketContext);
  const ref = useRef(handler);
  ref.current = handler;
  useEffect(() => subscribe((f) => ref.current(f)), [subscribe]);
}

// useSessionConnState is the session socket's connection state, for a surface
// that discloses it. "offline" outside a provider — there is no socket, which
// is exactly what a viewer would be told if one had dropped.
export function useSessionConnState(): ConnState {
  return useContext(SessionSocketContext).state;
}
