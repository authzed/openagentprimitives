import { describe, expect, it } from "vitest";
import golden from "./testdata/frames.golden.json";
import { parseChatFrame } from "./useChatSocket";
import { INITIAL_SESSION_SIGNALS, RECONNECT_RESET, reduceSessionSignals, type SessionSignals } from "./sessionSignals";
import type { ChatFrame } from "./types";

// The frames are the Go side's REAL toFrame output (frames_golden_internal_test.go);
// this test folds them and asserts the derived signals at each checkpoint.
function fold(n: number): SessionSignals {
  let s = INITIAL_SESSION_SIGNALS;
  for (const raw of golden.frames.slice(0, n)) { const f = parseChatFrame(JSON.stringify(raw)); if (f) s = reduceSessionSignals(s, f); }
  return s;
}

describe("reduceSessionSignals over the shared frames golden", () => {
  it("turn start", () => { const s = fold(1); expect(s.turnActive).toBe(true); expect(s.awaitingReply).toBe(false); });
  it("composing a page update while update_view streams", () => { expect(fold(2).composingUpdate).toBe(true); expect(fold(5).composingUpdate).toBe(false); });
  it("activity and plan", () => { const s = fold(4); expect(s.activity?.compactLine).toBe("Connecting GitHub"); expect(s.plan?.items?.map((i) => i.status)).toEqual(["done", "in_progress", "pending"]); });
  it("awaiting the person after respond_to_user + await_user_message", () => { const s = fold(7); expect(s.turnActive).toBe(false); expect(s.awaitingReply).toBe(true); expect(s.lastAgentReply?.text).toBe("Which repository should it watch?"); expect(s.lastAgentReply?.seq).toBe(1); });
  it("a reply from another surface clears the ask", () => { const s = fold(8); expect(s.awaitingReply).toBe(false); expect(s.lastAgentReply).toBeNull(); });
  // The last frame is the turn loop's IDLE EXIT (cause "idle"), the paused
  // signal a channel-attached session really ends a turn on. Only
  // "awaiting_reply" — the agent asking and blocking on the answer — is an ask,
  // so this must leave awaitingReply false: it is what keeps the reply modal
  // off the end of every turn. It is still a pause the person must be able to
  // see, though — pausedIdle is what a channel-less surface (the agent-UI
  // view) reads instead, so the session doesn't look dead after the reply.
  it("the next turn clears activity when told; the idle exit is not an ask, but it is a visible pause", () => { const s = fold(11); expect(s.activity).toBeNull(); expect(s.turnActive).toBe(false); expect(s.awaitingReply).toBe(false); expect(s.pausedIdle).toBe(true); });
  // The session is over; nobody is left to answer. The reply fallback reads
  // exactly this, so the frame that sets it belongs in the golden rather than
  // in a literal only this test believes in.
  //
  // fold(14) rather than fold(12): the golden now carries a plan-amendment
  // interaction_request/applied pair (indices 11-12) between the idle exit and
  // session_ended (index 13) — see the pendingInteractions block below.
  it("session_ended is terminal, and an ended session is not an ask", () => { const s = fold(14); expect(s.ended).toBe(true); expect(s.awaitingReply).toBe(false); expect(s.turnActive).toBe(false); });

  // The startup line: the health watcher's caption folds into `startup`, with
  // "still trying" carried as a flag rather than as text, and the one frame
  // that says started clears it.
  it("session_startup folds into startup, and started clears it", () => {
    const s = fold(15);
    expect(s.startup).toEqual({ text: "Starting up — your request will run once everything's ready…", short: null, stillTrying: true });
    const started = reduceSessionSignals(s, {
      type: "session_startup",
      session: { namespace: "workshop", name: "sess-live" },
      payload: { session: { namespace: "workshop", name: "sess-live" }, text: "", started: true },
    } as ChatFrame);
    expect(started.startup).toBeNull();
  });

  // An empty caption means "say nothing", never a wordless spinner: a
  // session_startup frame with no text must not set startup, whether folded
  // from nothing (INITIAL_SESSION_SIGNALS) or over a line already showing.
  it("session_startup with an empty caption leaves startup null, and clears a previously set one", () => {
    const emptyCaption = {
      type: "session_startup",
      session: { namespace: "workshop", name: "sess-live" },
      payload: { session: { namespace: "workshop", name: "sess-live" }, text: "", started: false },
    } as ChatFrame;
    expect(reduceSessionSignals(INITIAL_SESSION_SIGNALS, emptyCaption).startup).toBeNull();

    const withStartup = fold(15);
    expect(withStartup.startup).not.toBeNull();
    expect(reduceSessionSignals(withStartup, emptyCaption).startup).toBeNull();
  });

  // pendingInteractions: the golden's plan-amendment request/applied pair
  // (indices 11-12, between the idle exit and session_ended) folds into the
  // live set of prompts the agent is blocked on — what the agent-defined
  // view's "Needs your decision" region reads.
  describe("pendingInteractions", () => {
    it("an interaction_request adds one entry keyed by requestRef", () => {
      const s = fold(12); // through the request frame, index 11
      expect(s.pendingInteractions).toHaveLength(1);
      expect(s.pendingInteractions[0].requestRef).toBe("req-1");
      expect(s.pendingInteractions[0].lead).toBe("The agent is asking to add workshop_apply to its approved plan.");
    });

    it("a second request frame with the same requestRef replaces, not duplicates", () => {
      const s = fold(12);
      const f = parseChatFrame(
        JSON.stringify({
          type: "interaction_request",
          session: {},
          payload: {
            session: {},
            payload: { agentSessionRef: {}, category: "plan_amendment", requestRef: "req-1", lead: "Updated ask.", audience: { scope: "" } },
          },
        }),
      )!;
      const next = reduceSessionSignals(s, f);
      expect(next.pendingInteractions).toHaveLength(1);
      expect(next.pendingInteractions[0].lead).toBe("Updated ask.");
    });

    it("the matching interaction_applied removes it", () => {
      const s = fold(13); // through the applied frame, index 12
      expect(s.pendingInteractions).toEqual([]);
    });

    // The list is what the "Needs your decision" region renders, top to
    // bottom: two prompts outstanding at once must read in the order they
    // arrived, not whichever the last frame happened to name.
    it("two distinct requests both stand, in arrival order", () => {
      const second = reduceSessionSignals(
        fold(12), // through the req-1 request frame
        parseChatFrame(
          JSON.stringify({
            type: "interaction_request",
            session: {},
            payload: { session: {}, payload: { agentSessionRef: {}, category: "credential_request", requestRef: "req-2", lead: "Another ask.", audience: { scope: "" } } },
          }),
        )!,
      );
      expect(second.pendingInteractions.map((r) => r.requestRef)).toEqual(["req-1", "req-2"]);
      expect(second.pendingInteractions.map((r) => r.lead)).toEqual(["The agent is asking to add workshop_apply to its approved plan.", "Another ask."]);
    });

    // An applied frame for a prompt this fold never saw is news about nothing
    // — it lands after a reconnect reset cleared the list, or on a surface
    // that came up mid-session. Returning the SAME object (not an equal one)
    // is what keeps it from re-rendering every consumer under the provider.
    it("an interaction_applied for an unknown requestRef changes nothing at all", () => {
      const s = fold(12);
      const f = parseChatFrame(
        JSON.stringify({
          type: "interaction_applied",
          session: {},
          payload: { session: {}, payload: { agentSessionRef: {}, category: "plan_amendment", requestRef: "req-never-seen", outcome: "approved" } },
        }),
      )!;
      expect(reduceSessionSignals(s, f)).toBe(s);
    });

    it("session_ended clears any still-pending interaction", () => {
      // Built directly rather than via fold(14): the golden's own request is
      // already resolved by session_ended, which would prove nothing about
      // session_ended's OWN clearing behavior versus the applied frame having
      // already emptied it.
      const requested = reduceSessionSignals(
        fold(11),
        parseChatFrame(
          JSON.stringify({
            type: "interaction_request",
            session: {},
            payload: { session: {}, payload: { agentSessionRef: {}, category: "plan_amendment", requestRef: "req-2", lead: "Another ask.", audience: { scope: "" } } },
          }),
        )!,
      );
      expect(requested.pendingInteractions).toHaveLength(1);
      const ended = reduceSessionSignals(
        requested,
        parseChatFrame(JSON.stringify({ type: "session_ended", session: {}, payload: { session: {}, reason: "succeeded" } }))!,
      );
      expect(ended.pendingInteractions).toEqual([]);
    });

    it("the reconnect reset clears any still-pending interaction", () => {
      const requested = reduceSessionSignals(
        fold(11),
        parseChatFrame(
          JSON.stringify({
            type: "interaction_request",
            session: {},
            payload: { session: {}, payload: { agentSessionRef: {}, category: "plan_amendment", requestRef: "req-2", lead: "Another ask.", audience: { scope: "" } } },
          }),
        )!,
      );
      expect(requested.pendingInteractions).toHaveLength(1);
      const reset = reduceSessionSignals(requested, RECONNECT_RESET);
      expect(reset.pendingInteractions).toEqual([]);
    });
  });

  // turn_progress is the transcript's token/elapsed line. No fallback renders
  // it, so folding it would be state nobody reads — and a new signals object
  // on every heartbeat, re-rendering both views for nothing.
  it("ignores turn_progress, whose line belongs to the transcript", () => {
    const s = fold(4);
    const f = parseChatFrame(JSON.stringify({ type: "turn_progress", session: {}, payload: { session: {}, payload: { inputTokens: 10, outputTokens: 2, elapsedSeconds: 3, seq: 1 } } }))!;
    expect(reduceSessionSignals(s, f)).toBe(s);
  });

  // The socket has no replay: a turn_activity(active:false) that landed while
  // the connection was away is simply gone, and nothing else ever clears
  // turnActive — the default progress region would spin "Working…" forever.
  // The reconnect fails toward "not working", and leaves alone what a gap
  // cannot falsify: an ask is still outstanding until someone answers it.
  it("a reconnect clears the working signals and keeps the ones a gap cannot falsify", () => {
    const mid = fold(4);
    expect(mid.turnActive).toBe(true);
    expect(mid.composingUpdate).toBe(true);
    expect(mid.activity).not.toBeNull();

    const s = reduceSessionSignals(mid, RECONNECT_RESET);
    expect(s.turnActive).toBe(false);
    expect(s.composingUpdate).toBe(false);
    expect(s.activity).toBeNull();
    expect(s.plan).toEqual(mid.plan);

    const asked = reduceSessionSignals(fold(7), RECONNECT_RESET);
    expect(asked.awaitingReply).toBe(true);
    expect(asked.lastAgentReply?.text).toBe("Which repository should it watch?");
    expect(asked.replySeq).toBe(1);
  });

  it("returns the same object for a frame that changes nothing", () => { const s = fold(1); const f = parseChatFrame(JSON.stringify({ type: "live_view_offer", session: {}, payload: {} }))!; expect(reduceSessionSignals(s, f)).toBe(s); });

  // pausedIdle: a synthetic sequence rather than the golden, so the case is
  // legible on its own — a reply followed by an idle exit, no question asked.
  describe("pausedIdle", () => {
    function foldFrames(frames: unknown[]): SessionSignals {
      let s = INITIAL_SESSION_SIGNALS;
      for (const raw of frames) { const f = parseChatFrame(JSON.stringify(raw)); if (f) s = reduceSessionSignals(s, f); }
      return s;
    }

    it("a reply followed by an idle exit is a paused turn, not an ask", () => {
      const s = foldFrames([
        { type: "turn_activity", session: {}, payload: { session: {}, active: true } },
        { type: "user_message", session: {}, payload: { session: {}, text: "I'll move on to the tools now." } },
        { type: "turn_activity", session: {}, payload: { session: {}, active: false, cause: "idle" } },
      ]);
      expect(s.pausedIdle).toBe(true);
      expect(s.awaitingReply).toBe(false);
      expect(s.lastAgentReply?.text).toBe("I'll move on to the tools now.");
    });

    it("the next turn starting clears the paused signal", () => {
      const paused = foldFrames([
        { type: "turn_activity", session: {}, payload: { session: {}, active: true } },
        { type: "user_message", session: {}, payload: { session: {}, text: "I'll move on to the tools now." } },
        { type: "turn_activity", session: {}, payload: { session: {}, active: false, cause: "idle" } },
      ]);
      expect(paused.pausedIdle).toBe(true);
      const next = reduceSessionSignals(paused, parseChatFrame(JSON.stringify({ type: "turn_activity", session: {}, payload: { session: {}, active: true } }))!);
      expect(next.pausedIdle).toBe(false);
    });
  });
  it("seq is session-monotonic: a reply in a later turn gets a higher seq", () => {
    let s = fold(11);
    for (const raw of [
      { type: "turn_activity", session: {}, payload: { session: {}, active: true } },
      { type: "user_message", session: {}, payload: { session: {}, text: "And which channel?" } },
    ]) { const f = parseChatFrame(JSON.stringify(raw)); if (f) s = reduceSessionSignals(s, f); }
    expect(s.lastAgentReply?.seq).toBe(2);
    expect(s.replySeq).toBe(2);
  });
});
