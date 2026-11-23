import * as React from "react";
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent, waitFor, act } from "@testing-library/react";
import type { ChatFrame, ConnState, SessionDetail, TimelineItem } from "./types";
import { SessionSignalsContext, INITIAL_SESSION_SIGNALS } from "./sessionSignals";

// NS/NAME are the session this view is told to open. Fabricated names, never
// read from `examples/`. They are deliberately NOT "default"/anything the URL
// builder could produce on its own, so a hardcoded namespace in a request URL
// fails the addressing case below rather than passing by coincidence.
const NS = "demo-ns";
const NAME = "demo-session";
const API = `/sessions/api/${NS}/${NAME}`;

// This view SUBSCRIBES to the session socket the shell holds (SessionSocket.tsx)
// rather than opening one, so the provider is what these tests stand in for:
// capturing the registered handler is how a row drives a frame, and
// mockConnState is how one drives the connection state (e.g. a sustained
// outage) by flipping it and forcing a rerender. Which session the socket is
// opened for is the PROVIDER's fact, pinned in SessionSocket.test.tsx.
let capturedOnFrame: ((f: ChatFrame) => void) | null = null;
let mockConnState: ConnState = "connected";

vi.mock("./SessionSocket", () => ({
  useSessionFrames: (handler: (f: ChatFrame) => void) => {
    capturedOnFrame = handler;
  },
  useSessionConnState: () => mockConnState,
}));

import { ChatView, endedText } from "./ChatView";

afterEach(() => {
  cleanup();
  capturedOnFrame = null;
  mockConnState = "connected";
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

// view is the component under test at its ONE address. Every case renders this
// rather than a bare element so a prop rename cannot leave half the file
// rendering the old shape.
const view = (readOnly = false) => <ChatView ns={NS} name={NAME} readOnly={readOnly} />;

// S is the session ref the pushed frames carry. It is the view's OWN session,
// which is what a real socket delivers — the frames arrive on a connection
// opened for exactly this session.
const S = { namespace: NS, name: NAME };

const sampleDetail: SessionDetail = {
  sessionId: NAME,
  agentClass: "demo-agent",
  model: { provider: "anthropic", name: "claude-opus-4-8" },
  maxTokens: 120000,
  maxTurns: 40,
  maxDuration: "30m0s",
  owner: "user:me",
  createdAt: "2026-07-04T09:00:00Z",
  phase: "Running",
};

// stubFetch answers the five session-scoped endpoints this view addresses.
//
// Every response carries Content-Type: application/json, which is what
// writeJSON/writeError really set (pkg/web/webui/chat/handlers.go): the view reads
// an error body ONLY when the response declares itself JSON, because the
// route-level Authorize gate answers with an HTML page, so a stub that omitted
// the header would exercise a path the server never produces.
function stubFetch(opts: {
  messageStatus?: number;
  messageBody?: unknown;
  timeline?: TimelineItem[];
  messagesStatus?: number;
  detail?: SessionDetail;
  interruptStatus?: number;
  interruptBody?: unknown;
  decisionStatus?: number;
  decisionBody?: unknown;
}) {
  const {
    messageStatus = 200,
    messageBody = { sessionId: NAME, routed: true, ended: false },
    timeline = [],
    messagesStatus = 200,
    detail = sampleDetail,
    interruptStatus = 200,
    interruptBody = { ok: true },
    decisionStatus = 200,
    decisionBody = { ok: true },
  } = opts;
  const fn = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === `${API}/messages`) return json({ timeline }, messagesStatus);
    if (url === `${API}/detail`) return json(detail);
    if (url === `${API}/message`) return json(messageBody, messageStatus);
    if (url === `${API}/interrupt`) return json(interruptBody, interruptStatus);
    if (url === `${API}/decision`) return json(decisionBody, decisionStatus);
    void init;
    throw new Error("unexpected fetch: " + url);
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

// json builds a response in the shape the chat handlers really write.
function json(v: unknown, status = 200): Response {
  return new Response(JSON.stringify(v), { status, headers: { "Content-Type": "application/json" } });
}

// html builds the system-error page the route-level Authorize gate writes
// BEFORE any handler runs (pkg/web/webui/server.go's renderAuthorizeFailure). It is
// not JSON, which is the whole point: a view that parsed it would throw.
function html(status: number): Response {
  return new Response("<!doctype html><title>Access denied</title>", {
    status,
    headers: { "Content-Type": "text/html; charset=utf-8" },
  });
}

// fetchCalls reads the stubbed global back, for the helpers below that are
// called from cases holding no reference to the mock.
function fetchCalls(): unknown[][] {
  return (globalThis.fetch as unknown as { mock: { calls: unknown[][] } }).mock.calls;
}

// awaitSend waits for the composer's POST to have been issued. It replaces the
// old "wait until the socket learns the session id the POST returned": the view
// is told its session up front, so the send no longer establishes anything —
// this is purely the sync point the frame-driving cases need.
async function awaitSend(times = 1) {
  await waitFor(() =>
    expect(fetchCalls().filter(([u]) => u === `${API}/message`).length).toBeGreaterThanOrEqual(times),
  );
}

describe("endedText", () => {
  it("maps each session_ended reason to a friendly line", () => {
    expect(endedText({ session: { namespace: "n", name: "s" }, reason: "succeeded" })).toBe("Conversation ended.");
    expect(endedText({ session: { namespace: "n", name: "s" }, reason: "idle_timeout" })).toBe(
      "Conversation ended after being idle too long.",
    );
    expect(endedText({ session: { namespace: "n", name: "s" }, reason: "server_shutdown" })).toBe(
      "Conversation ended: the server is restarting.",
    );
    expect(
      endedText({ session: { namespace: "n", name: "s" }, reason: "failed", failureMessage: "ran out of budget" }),
    ).toBe("Conversation ended: ran out of budget.");
    expect(endedText({ session: { namespace: "n", name: "s" }, reason: "failed" })).toBe(
      "Conversation ended: the agent failed.",
    );
  });
});

// The view is told which session to open, and every request it makes must say
// so the same way: in the PATH. A body field naming a session would be a second
// answer to "which conversation is this", and the server's route gate only ever
// checked the first one.
describe("ChatView — the session it addresses", () => {
  it("renders an accepted reply once across bootstrap and repeated live events", async () => {
    stubFetch({ timeline: [{ kind: "message", role: "agent", text: "Stand up and stretch!", createdAt: "2026-10-03T08:00:00Z", operationID: "reply-1" }] });
    render(view());
    expect(await screen.findByText("Stand up and stretch!")).toBeTruthy();
    const frame: ChatFrame = {
      type: "user_message", session: S,
      payload: { session: S, text: "Stand up and stretch!", delivery: { id: "reply-1" } },
    };
    act(() => { capturedOnFrame!(frame); capturedOnFrame!(frame); });
    expect(screen.getAllByText("Stand up and stretch!")).toHaveLength(1);
    act(() => capturedOnFrame!({ ...frame, payload: { session: S, text: "Stand up and stretch!", delivery: { id: "reply-2" } } }));
    expect(screen.getAllByText("Stand up and stretch!")).toHaveLength(2);
  });
  it("renders an async opening summary with inspectable exact instructions beside ordinary replies", async () => {
    stubFetch({ timeline: [
      { kind: "opening", opening: { summary: "Session created to meet goal Stretch: Stand up and stretch.", instructions: "  Only respond_to_user may perform actions.\n<script>literal</script>" }, createdAt: "2026-10-03T05:57:00Z" },
      { kind: "message", role: "agent", text: "Stand up and stretch!", createdAt: "2026-10-03T05:57:20Z" },
    ] });
    render(view());
    const card = await screen.findByTestId("opening-card");
    expect(card.textContent).toContain("Session created to meet goal Stretch: Stand up and stretch.");
    const details = card.querySelector("details")!;
    expect(details.open).toBe(false);
    fireEvent.click(screen.getByText("View exact instructions"));
    expect(details.open).toBe(true);
    expect(card.querySelector("pre")!.textContent).toBe("  Only respond_to_user may perform actions.\n<script>literal</script>");
    expect(card.querySelector("script")).toBeNull();
    expect(await screen.findByText("Stand up and stretch!")).toBeTruthy();
    act(() => capturedOnFrame!({
      type: "session_opening",
      session: S,
      payload: {
        session: S,
        opening: {
          summary: "Session created to meet goal Stretch: Stand up and stretch.",
          instructions: "  Only respond_to_user may perform actions.\n<script>literal</script>",
        },
      },
    }));
    expect(screen.getAllByTestId("opening-card")).toHaveLength(1);
    expect(screen.getAllByText("Session created to meet goal Stretch: Stand up and stretch.")).toHaveLength(1);
    expect(details.open).toBe(true);
    // An ordinary assistant reply with identical wording is still an ordinary reply.
    act(() => capturedOnFrame!({
      type: "user_message",
      session: S,
      payload: { session: S, text: "Session created to meet goal Stretch: Stand up and stretch." },
    }));
    expect(screen.getAllByText("Session created to meet goal Stretch: Stand up and stretch.")).toHaveLength(2);
  });
  it("addresses the ns/name it was given — in every request URL and on the socket — and never a default", async () => {
    const fetchMock = stubFetch({});
    render(view());

    const textarea = await screen.findByPlaceholderText("Message the agent…");
    fireEvent.change(textarea, { target: { value: "hello there" } });
    fireEvent.click(screen.getByTitle("Send"));
    // Waited on the CALL COUNT, not on a URL: a view addressing the wrong
    // session still calls fetch, and this row's whole job is to inspect the
    // URL it called with. Waiting for the right URL would turn a wrong one
    // into a timeout that says nothing about the address.
    await waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThanOrEqual(2));

    const urls = fetchMock.mock.calls.map(([u]) => String(u));
    expect(urls.length).toBeGreaterThan(0);
    for (const u of urls) {
      expect(u).toContain(NS);
      expect(u).toContain(NAME);
      expect(u).not.toContain("default");
    }
  });
});

describe("ChatView — send flow", () => {
  it("POSTs the message to its own session's path, naming no session in the body, with an idempotency key", async () => {
    const fetchMock = stubFetch({});
    render(view());

    const textarea = await screen.findByPlaceholderText("Message the agent…");
    fireEvent.change(textarea, { target: { value: "hello there" } });
    fireEvent.click(screen.getByTitle("Send"));

    // Optimistic echo renders immediately, before the POST resolves.
    expect(screen.getByText("hello there")).toBeTruthy();

    await awaitSend();

    const messageCall = fetchMock.mock.calls.find(([url]) => url === `${API}/message`);
    expect(messageCall).toBeTruthy();
    const body = JSON.parse((messageCall![1] as RequestInit).body as string);
    expect(body).toMatchObject({ text: "hello there" });
    // The session rides in the path, never in the body: the server deleted the
    // field, and a body field the server ignores is a lie about what addresses
    // the conversation.
    expect(body.sessionId).toBeUndefined();
    expect(body.agentClass).toBeUndefined();
    // Every send carries a client-minted idempotency key (reused on Retry).
    expect(typeof body.requestId).toBe("string");
    expect(body.requestId.length).toBeGreaterThan(0);
  });

  it("sends every subsequent message the same way — same path, still no session in the body", async () => {
    const fetchMock = stubFetch({});
    render(view());

    const textarea = await screen.findByPlaceholderText("Message the agent…");
    fireEvent.change(textarea, { target: { value: "first" } });
    fireEvent.click(screen.getByTitle("Send"));
    await awaitSend();

    fireEvent.change(textarea, { target: { value: "second" } });
    fireEvent.click(screen.getByTitle("Send"));

    await waitFor(() => {
      const calls = fetchMock.mock.calls.filter(([url]) => url === `${API}/message`);
      expect(calls).toHaveLength(2);
    });
    const calls = fetchMock.mock.calls.filter(([url]) => url === `${API}/message`);
    const secondBody = JSON.parse((calls[1][1] as RequestInit).body as string);
    expect(secondBody).toMatchObject({ text: "second" });
    expect(secondBody.sessionId).toBeUndefined();
    expect(String(calls[1][0])).toBe(`${API}/message`);
  });

  it("marks a failed send red ON ITS OWN BUBBLE with the reason + a Retry, not a separate line", async () => {
    stubFetch({ messageStatus: 404, messageBody: { error: "chat session not found" } });
    render(view());

    const textarea = await screen.findByPlaceholderText("Message the agent…");
    fireEvent.change(textarea, { target: { value: "hi" } });
    fireEvent.click(screen.getByTitle("Send"));

    const failed = await screen.findByTestId("failed-message");
    // The failed message text AND the reason live on that one bubble…
    expect(failed.textContent).toContain("hi");
    expect(failed.textContent).toContain("chat session not found");
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    // …not duplicated as a separate bottom error line (the text renders once).
    expect(screen.getAllByText("hi")).toHaveLength(1);
  });

  it("keeps the composer enabled while a send is in flight (non-blocking)", async () => {
    // The second send's POST is held pending so we can assert the composer
    // never locks on it.
    let releaseSecond: (r: Response) => void = () => {};
    const secondPending = new Promise<Response>((res) => {
      releaseSecond = res;
    });
    let messageCalls = 0;
    const fn = vi.fn(async (url: string) => {
      if (url === `${API}/messages`) return json({ timeline: [] });
      if (url === `${API}/message`) {
        messageCalls += 1;
        if (messageCalls === 1) return json({ sessionId: NAME, routed: true, ended: false });
        return secondPending;
      }
      throw new Error("unexpected fetch: " + url);
    });
    vi.stubGlobal("fetch", fn);
    render(view());

    const textarea = (await screen.findByPlaceholderText("Message the agent…")) as HTMLTextAreaElement;
    fireEvent.change(textarea, { target: { value: "first" } });
    fireEvent.click(screen.getByTitle("Send"));
    await awaitSend();

    fireEvent.change(textarea, { target: { value: "second" } });
    fireEvent.click(screen.getByTitle("Send"));

    // The in-flight bubble is shown pending, but the composer is NOT disabled —
    // you can keep typing/sending immediately (the ~50ms interaction budget).
    expect(await screen.findByTestId("pending-message")).toBeTruthy();
    expect(textarea.disabled).toBe(false);

    act(() => releaseSecond(json({ sessionId: NAME, routed: true, ended: false })));
    await waitFor(() => expect(screen.queryByTestId("pending-message")).toBeNull());
  });

  it("Retry re-sends a failed message and clears the failure on success", async () => {
    let calls = 0;
    const fn = vi.fn(async (url: string) => {
      if (url === `${API}/messages`) return json({ timeline: [] });
      if (url === `${API}/message`) {
        calls += 1;
        if (calls === 1) return json({ error: "channelsd timeout" }, 503);
        return json({ sessionId: NAME, routed: true, ended: false });
      }
      throw new Error("unexpected fetch: " + url);
    });
    vi.stubGlobal("fetch", fn);
    render(view());

    const textarea = await screen.findByPlaceholderText("Message the agent…");
    fireEvent.change(textarea, { target: { value: "retry me" } });
    fireEvent.click(screen.getByTitle("Send"));
    await screen.findByTestId("failed-message");

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() => expect(screen.queryByTestId("failed-message")).toBeNull());
    await awaitSend(2);
    const msgCalls = fn.mock.calls.filter(([u]) => u === `${API}/message`);
    expect(msgCalls).toHaveLength(2);
    // The retry re-sends the SAME text (no re-typing needed)…
    const firstBody = JSON.parse((msgCalls[0][1] as RequestInit).body as string);
    const secondBody = JSON.parse((msgCalls[1][1] as RequestInit).body as string);
    expect(secondBody).toMatchObject({ text: "retry me" });
    // …AND the SAME idempotency key, so channelsd dedupes a Retry of a send
    // that actually landed (its reply merely lost the race) into a no-op that
    // returns the first delivery's decision — never a double-post.
    expect(firstBody.requestId).toBeTruthy();
    expect(secondBody.requestId).toBe(firstBody.requestId);
  });
});

describe("ChatView — websocket frame rendering", () => {
  async function sendFirstMessage() {
    stubFetch({});
    render(view());
    const textarea = await screen.findByPlaceholderText("Message the agent…");
    fireEvent.change(textarea, { target: { value: "hi" } });
    fireEvent.click(screen.getByTitle("Send"));
    await awaitSend();
    return textarea;
  }

  it("renders user_message frames as the agent's reply", async () => {
    await sendFirstMessage();
    capturedOnFrame!({
      type: "user_message",
      session: S,
      payload: { session: S, text: "hello human" },
    });
    await waitFor(() => expect(screen.getByText("hello human")).toBeTruthy());
  });

  it("renders a user_echo from another surface as a user message line", async () => {
    // A message typed in the artifact view (or another tab): its requestID
    // matches nothing this tab sent, so it renders as a user line here — the
    // "show view messages in the chat too" mirror.
    await sendFirstMessage();
    capturedOnFrame!({
      type: "user_echo",
      session: S,
      payload: {
        session: S,
        text: "please tighten the header",
        author: "a@example.com",
        requestID: "other-surface-1",
      },
    });
    await waitFor(() => expect(screen.getByText("please tighten the header")).toBeTruthy());
  });

  it("suppresses the echo of this tab's own send (matching requestID) but renders others", async () => {
    const fetchMock = stubFetch({});
    render(view());
    const textarea = await screen.findByPlaceholderText("Message the agent…");
    fireEvent.change(textarea, { target: { value: "my own message" } });
    fireEvent.click(screen.getByTitle("Send"));
    expect(screen.getByText("my own message")).toBeTruthy(); // optimistic bubble
    await awaitSend();
    const messageCall = fetchMock.mock.calls.find(([url]) => url === `${API}/message`);
    const requestId = JSON.parse((messageCall![1] as RequestInit).body as string).requestId as string;

    // The server mirrors the same message back with the SAME requestId — the
    // tab must not render a duplicate of what it already showed optimistically.
    capturedOnFrame!({
      type: "user_echo",
      session: S,
      payload: { session: S, text: "my own message", requestID: requestId },
    });
    // An echo from ANOTHER surface (different requestId) still renders — proving
    // frames were processed and only the self-echo was dropped.
    capturedOnFrame!({
      type: "user_echo",
      session: S,
      payload: { session: S, text: "from elsewhere", requestID: "other-1" },
    });
    await waitFor(() => expect(screen.getByText("from elsewhere")).toBeTruthy());
    expect(screen.getAllByText("my own message")).toHaveLength(1);
  });

  it("does not re-arm the working throbber from a late turn_progress after the turn ended", async () => {
    // The turn runs and completes; turn_activity(active:false) clears the
    // throbber. A turn_progress arriving AFTER that (published just before the
    // yield, delivered late) must NOT bring the throbber back — the session is
    // idle. This is the "session is idle but UI still shows working" bug.
    await sendFirstMessage();
    const S = { namespace: NS, name: NAME };
    capturedOnFrame!({ type: "turn_activity", session: S, payload: { session: S, active: true } });
    capturedOnFrame!({ type: "user_message", session: S, payload: { session: S, text: "Done." } });
    capturedOnFrame!({ type: "turn_activity", session: S, payload: { session: S, active: false, cause: "awaiting_user_message" } });
    await waitFor(() => expect(screen.queryByLabelText("Agent is working")).toBeNull());

    capturedOnFrame!({
      type: "turn_progress",
      session: S,
      payload: { session: S, payload: { inputTokens: 100, outputTokens: 200, elapsedSeconds: 5 } },
    });
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByLabelText("Agent is working")).toBeNull();
  });

  it("still shows progress for turn_progress DURING an active turn (gate doesn't over-suppress)", async () => {
    // Guard against over-gating: a turn_progress that arrives while a turn is
    // genuinely active (send → turn_activity(active:true)) must still drive the
    // working affordance.
    await sendFirstMessage();
    const S = { namespace: NS, name: NAME };
    capturedOnFrame!({ type: "turn_activity", session: S, payload: { session: S, active: true } });
    capturedOnFrame!({
      type: "turn_progress",
      session: S,
      payload: { session: S, payload: { inputTokens: 10, outputTokens: 20, elapsedSeconds: 1 } },
    });
    await waitFor(() => expect(screen.queryByLabelText("Agent is working")).toBeTruthy());
  });

  it("does not render an op tree from a late operation_activity after the turn ended", async () => {
    await sendFirstMessage();
    const S = { namespace: NS, name: NAME };
    capturedOnFrame!({ type: "turn_activity", session: S, payload: { session: S, active: true } });
    capturedOnFrame!({ type: "turn_activity", session: S, payload: { session: S, active: false, cause: "awaiting_user_message" } });
    // A late operation_activity (published pre-yield, delivered post-yield) must
    // not surface stale op nodes on the idle session.
    capturedOnFrame!({
      type: "operation_activity",
      session: S,
      payload: { session: S, payload: { operations: [{ id: "op-1", description: "querying Linear", active: true }], compactLine: "querying Linear" } },
    });
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByText("querying Linear")).toBeNull();
    expect(screen.queryByLabelText("Agent is working")).toBeNull();
  });

  it("does not re-arm working (nor resurrect a stream bubble) from a late stream_delta after the turn ended", async () => {
    // A stream_delta published just before the yield can arrive AFTER
    // turn_activity(active:false). Gated like the turn ticks, it must neither
    // re-arm the throbber nor grow a blinking-cursor bubble on the idle session.
    await sendFirstMessage();
    const S = { namespace: NS, name: NAME };
    capturedOnFrame!({ type: "turn_activity", session: S, payload: { session: S, active: true } });
    capturedOnFrame!({ type: "user_message", session: S, payload: { session: S, text: "All set." } });
    capturedOnFrame!({ type: "turn_activity", session: S, payload: { session: S, active: false, cause: "awaiting_user_message" } });
    await waitFor(() => expect(screen.queryByLabelText("Agent is working")).toBeNull());

    capturedOnFrame!({
      type: "stream_delta",
      session: S,
      payload: { session: S, payload: { eventType: "text_delta", text: "leftover token", blockIdx: 0 } },
    });
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByLabelText("Agent is working")).toBeNull();
    expect(screen.queryByText("leftover token")).toBeNull();
  });

  it("resyncs the transcript on reconnect (recovers frames the live socket missed)", async () => {
    // The live sink is push-only with no replay: a reply emitted while the ws
    // was briefly down is lost until reload. On reconnect the UI must refetch
    // the transcript and show it — the "stuck starting, refresh shows it" fix.
    const recovered: TimelineItem[] = [
      { kind: "message", role: "user", text: "hi", createdAt: "2026-07-15T00:00:00Z" },
      { kind: "message", role: "agent", text: "the reply you missed while offline", createdAt: "2026-07-15T00:00:01Z" },
    ];
    // The MOUNT load answers empty — the reply had not happened yet — and only
    // the RESYNC sees it. Without that the transcript would already carry the
    // line before the drop, and the case would pass whether or not a resync ran.
    let transcriptReads = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url === `${API}/messages`) {
          transcriptReads += 1;
          return json({ timeline: transcriptReads === 1 ? [] : recovered });
        }
        if (url === `${API}/detail`) return json({ ...sampleDetail, phase: "Idle" });
        if (url === `${API}/message`) return json({ sessionId: NAME, routed: true, ended: false });
        throw new Error("unexpected fetch: " + url);
      }),
    );
    const { rerender } = render(view());

    const textarea = await screen.findByPlaceholderText("Message the agent…");
    fireEvent.change(textarea, { target: { value: "hi" } });
    fireEvent.click(screen.getByTitle("Send"));
    await awaitSend();

    // Not shown yet: the reply only lives in the transcript (never pushed live).
    expect(screen.queryByText("the reply you missed while offline")).toBeNull();

    // Drop, then reconnect: the reconnect must refetch the transcript.
    mockConnState = "reconnecting";
    rerender(view());
    mockConnState = "connected";
    rerender(view());

    await waitFor(() => expect(screen.getByText("the reply you missed while offline")).toBeTruthy());
  });

  it("does NOT resync on the first connect (only on a genuine reconnect)", async () => {
    // A first connect must not clobber the just-loaded view with a transcript
    // refetch — the resync fires only after a drop. The mount load reads the
    // transcript exactly once, so a second read is an errant resync.
    //
    // Detail is read once on mount too (so a session already parked when the
    // view opens says so immediately), which is why the count below is 1 and
    // not 0: it used to be a proxy for "no resync happened" on the premise that
    // only the resync touched it. That premise changed; the property did not.
    // TWO detail reads still mean an errant resync.
    const fetchMock = stubFetch({ detail: { ...sampleDetail, phase: "Running" } });
    render(view());
    const textarea = await screen.findByPlaceholderText("Message the agent…");
    fireEvent.change(textarea, { target: { value: "hi" } });
    fireEvent.click(screen.getByTitle("Send"));
    await awaitSend();

    // Give any errant resync a chance to fire, then count.
    await new Promise((r) => setTimeout(r, 30));
    const transcriptReads = fetchMock.mock.calls.filter(([u]) => u === `${API}/messages`).length;
    expect(transcriptReads).toBe(1);
    const detailReads = fetchMock.mock.calls.filter(([u]) => u === `${API}/detail`).length;
    expect(detailReads).toBe(1);
  });

  it("renders an agent reply's artifact attachment as a same-origin download chip", async () => {
    await sendFirstMessage();
    capturedOnFrame!({
      type: "user_message",
      session: S,
      payload: {
        session: S,
        text: "here's your one-pager",
        attachments: [{ artifactId: "artifact-9", filename: "one-pager.html", mime: "text/html" }],
      },
    });
    await waitFor(() => expect(screen.getByText("here's your one-pager")).toBeTruthy());

    const link = screen.getByRole("link", { name: /one-pager\.html/ }) as HTMLAnchorElement;
    const href = link.getAttribute("href") ?? "";
    expect(href).toContain("/artifact-download?");
    expect(href).toContain("artifactId=artifact-9");
    expect(href).toContain(`sessionRef=${NS}%2F${NAME}`);
    expect(href).toContain("fn=one-pager.html");
  });

  it("renders a live_view_offer frame as a 'View live' link", async () => {
    await sendFirstMessage();
    capturedOnFrame!({
      type: "live_view_offer",
      session: S,
      payload: { session: S, url: "https://webd.example/artifact-view?d=AA&sig=BB" },
    });
    const link = (await screen.findByRole("link", { name: /view live/i })) as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("https://webd.example/artifact-view?d=AA&sig=BB");
  });

  it("ignores a live_view_offer with an empty url (viewer origin not configured)", async () => {
    await sendFirstMessage();
    capturedOnFrame!({
      type: "live_view_offer",
      session: S,
      payload: { session: S, url: "" },
    });
    expect(screen.queryByRole("link", { name: /view live/i })).toBeNull();
  });

  it("shows a notification as a transient pulsing notice, hidden once the turn completes", async () => {
    await sendFirstMessage();
    capturedOnFrame!({
      type: "notification",
      session: S,
      payload: { session: S, text: "thinking…" },
    });
    // Shown (in the working indicator) while the turn is active…
    await waitFor(() => expect(screen.getByText("thinking…")).toBeTruthy());
    // …and cleared when the turn ends — mid-flight status must NOT accumulate
    // in the timeline.
    capturedOnFrame!({
      type: "turn_activity",
      session: S,
      payload: { session: S, active: false, cause: "awaiting_user_message" },
    });
    await waitFor(() => expect(screen.queryByText("thinking…")).toBeNull());
  });

  it("keeps the update_status caption visible while the agent streams its reply", async () => {
    await sendFirstMessage();
    // An update_status caption arrives (a notification frame)…
    capturedOnFrame!({
      type: "notification",
      session: S,
      payload: { session: S, text: "Reading files…" },
    });
    await waitFor(() => expect(screen.getByText("Reading files…")).toBeTruthy());

    // …then the agent starts streaming tokens, which replaces the throbber. The
    // caption must NOT vanish — it lives on its own line (like Slack's
    // persistent setStatus), independent of the streaming bubble. This is the
    // regression: before the fix the caption was only inside the throbber, which
    // the streaming bubble suppresses.
    capturedOnFrame!({
      type: "stream_delta",
      session: S,
      payload: {
        session: S,
        payload: { eventType: "text_delta", text: "Here you go", blockIdx: 0 },
      },
    });
    await waitFor(() => expect(screen.getByText("Here you go")).toBeTruthy());
    expect(screen.getByText("Reading files…")).toBeTruthy();
  });

  it("decays an ephemeral notice to 'Working…' on the next frame, then a real caption replaces it", async () => {
    await sendFirstMessage();
    // A transient announcement (l.Notify → ephemeral), e.g. the runner picking
    // a queued message back up. It shows briefly…
    capturedOnFrame!({
      type: "notification",
      session: S,
      payload: {
        session: S,
        text: "Picking up 1 message you sent.",
        ephemeral: true,
      },
    });
    await waitFor(() => expect(screen.getByText("Picking up 1 message you sent.")).toBeTruthy());

    // …then the turn's first real activity frame supersedes it with a generic
    // "Working…" — the announcement must NOT linger as the caption once the
    // message has been picked up.
    capturedOnFrame!({
      type: "turn_progress",
      session: S,
      payload: {
        session: S,
        payload: { inputTokens: 10, outputTokens: 0, elapsedSeconds: 1, seq: 1 },
      },
    });
    await waitFor(() => expect(screen.getByText("Working…")).toBeTruthy());
    expect(screen.queryByText("Picking up 1 message you sent.")).toBeNull();

    // "Working…" holds only until the next real status update: a genuine
    // update_status caption (non-ephemeral) replaces it and then persists.
    capturedOnFrame!({
      type: "notification",
      session: S,
      payload: { session: S, text: "Reading files…" },
    });
    await waitFor(() => expect(screen.getByText("Reading files…")).toBeTruthy());
    expect(screen.queryByText("Working…")).toBeNull();
  });

  it("does not decay a persistent update_status caption to 'Working…' on later frames", async () => {
    await sendFirstMessage();
    // A real update_status caption (non-ephemeral) must survive subsequent
    // activity frames unchanged — it is not a one-shot announcement.
    capturedOnFrame!({
      type: "notification",
      session: S,
      payload: { session: S, text: "Compiling…" },
    });
    await waitFor(() => expect(screen.getByText("Compiling…")).toBeTruthy());
    capturedOnFrame!({
      type: "turn_progress",
      session: S,
      payload: {
        session: S,
        payload: { inputTokens: 5, outputTokens: 5, elapsedSeconds: 2, seq: 1 },
      },
    });
    // Still the real caption, never downgraded to the generic placeholder.
    await waitFor(() => expect(screen.getByText("Compiling…")).toBeTruthy());
    expect(screen.queryByText("Working…")).toBeNull();
  });

  it("renders plan_update frames as a checklist of steps, not just a count", async () => {
    await sendFirstMessage();
    capturedOnFrame!({
      type: "plan_update",
      session: S,
      payload: {
        session: S,
        payload: {
          planName: "main",
          items: [
            { id: "a", label: "Clone the repo", status: "done" },
            { id: "b", label: "Run the tests", status: "in_progress" },
          ],
          updatedAt: "2026-01-01T00:00:00Z",
        },
      },
    });
    // Each step label is rendered (the checklist), plus a header with the count.
    await waitFor(() => expect(screen.getByText("Clone the repo")).toBeTruthy());
    expect(screen.getByText("Run the tests")).toBeTruthy();
    expect(screen.getByText("Plan · 2 steps")).toBeTruthy();

    // Unlike a transient "thinking…" notice, a plan is a committed timeline
    // line: it stays visible after the turn completes.
    capturedOnFrame!({
      type: "turn_activity",
      session: S,
      payload: { session: S, active: false, cause: "awaiting_user_message" },
    });
    await waitFor(() => expect(screen.queryByRole("status")).toBeNull());
    expect(screen.getByText("Clone the repo")).toBeTruthy();
  });

  it("stops the plan's in-progress spinner when the session goes idle", async () => {
    // A plan whose last step is still "in_progress" (the agent yielded without
    // marking it done) must not keep spinning once the session is idle.
    await sendFirstMessage();
    const S = { namespace: NS, name: NAME };
    capturedOnFrame!({ type: "turn_activity", session: S, payload: { session: S, active: true } });
    capturedOnFrame!({
      type: "plan_update",
      session: S,
      payload: { session: S, payload: { planName: "main", items: [{ id: "b", label: "Deliver", status: "in_progress" }], updatedAt: "2026-01-01T00:00:00Z" } },
    });
    await waitFor(() => expect(screen.getByText("Deliver")).toBeTruthy());
    const planCard = screen.getByText("Deliver").closest("[data-testid='chat-line']") as HTMLElement;

    // While the turn is active the in-progress step spins…
    expect(planCard.querySelector(".animate-spin")).not.toBeNull();

    // …and stops once the session settles into idle.
    capturedOnFrame!({ type: "turn_activity", session: S, payload: { session: S, active: false, cause: "awaiting_user_message" } });
    await waitFor(() => expect(planCard.querySelector(".animate-spin")).toBeNull());
    expect(screen.getByText("Deliver")).toBeTruthy(); // the plan card itself stays
  });

  it("keeps one plan card per name, updating it in place across a message (not duplicating)", async () => {
    await sendFirstMessage();
    const planFrame = (status: string) => capturedOnFrame!({
      type: "plan_update",
      session: S,
      payload: { session: S, payload: {
        planName: "trip", items: [{ id: "a", label: "Pack bags", status }], updatedAt: "2026-01-01T00:00:00Z",
      } },
    });
    planFrame("in_progress");
    capturedOnFrame!({ type: "user_message", session: S,
      payload: { session: S, text: "a reply between" } });
    planFrame("done");
    // One card for "trip" (not two), showing the latest state; the message is present.
    await waitFor(() => expect(screen.getAllByText("Pack bags")).toHaveLength(1));
    expect(screen.getByText("a reply between")).toBeTruthy();
  });

  it("renders distinct plan names as separate cards", async () => {
    await sendFirstMessage();
    const planFrame = (name: string, label: string) => capturedOnFrame!({
      type: "plan_update", session: S,
      payload: { session: S, payload: {
        planName: name, items: [{ id: "a", label, status: "pending" }], updatedAt: "2026-01-01T00:00:00Z" } },
    });
    planFrame("alpha", "Do alpha");
    planFrame("beta", "Do beta");
    await waitFor(() => expect(screen.getByText("Do alpha")).toBeTruthy());
    expect(screen.getByText("Do beta")).toBeTruthy();
  });

  it("streams token deltas into a live bubble, then finalizes on the completed user_message", async () => {
    await sendFirstMessage();
    const delta = (text: string) =>
      capturedOnFrame!({
        type: "stream_delta",
        session: S,
        payload: { session: S, payload: { eventType: "text_delta", text, blockIdx: 0 } },
      });
    delta("Hel");
    delta("lo, wor");
    delta("ld");
    await waitFor(() => expect(screen.getByText("Hello, world")).toBeTruthy());

    // The completed reply is authoritative and replaces the streamed draft.
    capturedOnFrame!({
      type: "user_message",
      session: S,
      payload: { session: S, text: "Hello, world!" },
    });
    await waitFor(() => expect(screen.getByText("Hello, world!")).toBeTruthy());
    // The interim draft (without the trailing "!") is gone.
    expect(screen.queryByText("Hello, world")).toBeNull();
  });

  it("shows a working throbber after a send and clears it on turn_activity(active=false)", async () => {
    await sendFirstMessage();
    // The throbber (role=status) appears while the agent is working.
    await waitFor(() => expect(screen.getByRole("status", { name: "Agent is working" })).toBeTruthy());

    capturedOnFrame!({
      type: "turn_activity",
      session: S,
      payload: { session: S, active: false, cause: "awaiting_user_message" },
    });
    await waitFor(() => expect(screen.queryByRole("status", { name: "Agent is working" })).toBeNull());
  });

  it("renders a message sent while the agent is working ONCE — grayed while held, un-grayed on turn end", async () => {
    const fetchMock = stubFetch({});
    render(view());
    const textarea = await screen.findByPlaceholderText("Message the agent…");
    fireEvent.change(textarea, { target: { value: "hi" } });
    fireEvent.click(screen.getByTitle("Send"));
    await awaitSend();

    // Confirm the agent is actively working the turn.
    capturedOnFrame!({
      type: "turn_activity",
      session: S,
      payload: { session: S, active: true },
    });

    fireEvent.change(textarea, { target: { value: "second message" } });
    fireEvent.click(screen.getByTitle("Send"));

    // The message still POSTs immediately (the runner holds it server-side)…
    await waitFor(() => {
      const calls = fetchMock.mock.calls.filter(([url]) => url === `${API}/message`);
      expect(calls).toHaveLength(2);
    });
    // …AND renders EXACTLY ONCE, grayed, while the turn is still active — not
    // as a normal bubble PLUS a separate "Queued" duplicate.
    await waitFor(() => expect(screen.getAllByText("second message")).toHaveLength(1));
    const queuedEl = await screen.findByTestId("queued-message");
    expect(queuedEl.textContent).toContain("second message");
    expect(queuedEl.textContent).toContain("Queued");

    capturedOnFrame!({
      type: "turn_activity",
      session: S,
      payload: { session: S, active: false, cause: "awaiting_user_message" },
    });

    // The turn ended: the SAME single line un-grays (still exactly one render
    // of the text) — the "Queued" styling is gone, but the message itself
    // never disappeared or reappeared.
    await waitFor(() => expect(screen.queryByTestId("queued-message")).toBeNull());
    expect(screen.getAllByText("second message")).toHaveLength(1);
  });

  it("renders a turn_progress status line while the agent is working", async () => {
    await sendFirstMessage();
    capturedOnFrame!({
      type: "turn_progress",
      session: S,
      payload: {
        session: S,
        payload: { inputTokens: 1200, outputTokens: 6400, elapsedSeconds: 34, seq: 2 },
      },
    });
    await waitFor(() => expect(screen.getByText("working · 34s · 1.2K in · 6.4K out")).toBeTruthy());
  });

  it("renders send_error frames as a visible error line AND stops the working throbber", async () => {
    await sendFirstMessage();
    // The send left the throbber running; a surfaced failure must stop it,
    // not leave the user staring at a spinner that will never resolve.
    await waitFor(() => expect(screen.getByRole("status", { name: "Agent is working" })).toBeTruthy());
    capturedOnFrame!({
      type: "send_error",
      session: S,
      payload: { session: S, kind: "user_message", err: "boom", at: "2026-01-01T00:00:00Z" },
    });
    await waitFor(() => expect(screen.getByText("boom")).toBeTruthy());
    expect(screen.queryByRole("status", { name: "Agent is working" })).toBeNull();
  });

  it("send_error fully resets live-turn state: clears a mid-stream bubble AND a later tick can't re-arm", async () => {
    // A mid-stream error must (1) drop the partial streaming bubble rather than
    // leave an orphaned blinking cursor, and (2) drop turnActive so a late
    // turn_progress can't re-arm the throbber on the now-idle session.
    await sendFirstMessage();
    const S = { namespace: NS, name: NAME };
    capturedOnFrame!({
      type: "stream_delta",
      session: S,
      payload: { session: S, payload: { eventType: "text_delta", text: "partial draft", blockIdx: 0 } },
    });
    await waitFor(() => expect(screen.getByText("partial draft")).toBeTruthy());

    capturedOnFrame!({
      type: "send_error",
      session: S,
      payload: { session: S, kind: "user_message", err: "kaboom", at: "2026-01-01T00:00:00Z" },
    });
    await waitFor(() => expect(screen.getByText("kaboom")).toBeTruthy());
    // The throbber is gone AND the mid-stream draft bubble is cleared.
    expect(screen.queryByRole("status", { name: "Agent is working" })).toBeNull();
    expect(screen.queryByText("partial draft")).toBeNull();

    // A late turn_progress (published pre-error, delivered after) must NOT
    // re-arm the throbber — the bug the old partial clear left open.
    capturedOnFrame!({
      type: "turn_progress",
      session: S,
      payload: { session: S, payload: { inputTokens: 1, outputTokens: 1, elapsedSeconds: 1, seq: 1 } },
    });
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByRole("status", { name: "Agent is working" })).toBeNull();
  });

  it("renders interaction_decision_rejected as a visible error line — a refused click is never silently dropped", async () => {
    await sendFirstMessage();
    const S = { namespace: NS, name: NAME };
    capturedOnFrame!({
      type: "interaction_decision_rejected",
      session: S,
      payload: {
        session: S,
        payload: {
          agentSessionRef: S,
          category: "tool_approval",
          requestRef: "req-1",
          class: "handler_error",
          reason: "grant write failed: connection refused",
        },
      },
    });
    await waitFor(() => expect(screen.getByText("grant write failed: connection refused.")).toBeTruthy());
  });

  it("renders interaction_decision_rejected(already_resolved) naming the original decider, not raw JSON", async () => {
    await sendFirstMessage();
    const S = { namespace: NS, name: NAME };
    capturedOnFrame!({
      type: "interaction_decision_rejected",
      session: S,
      payload: {
        session: S,
        payload: {
          agentSessionRef: S,
          category: "tool_approval",
          requestRef: "req-1",
          class: "already_resolved",
          originalDecider: { kind: "slack", externalId: "U1", email: "approver@example.com" },
          originalOutcome: "approved",
        },
      },
    });
    await waitFor(() =>
      expect(screen.getByText("This request was already resolved by approver@example.com (approved).")).toBeTruthy(),
    );
  });

  it("surfaces 'Connection lost' and stops the throbber when the socket drops for good", async () => {
    stubFetch({});
    const { rerender } = render(view());
    const textarea = await screen.findByPlaceholderText("Message the agent…");
    fireEvent.change(textarea, { target: { value: "hi" } });
    fireEvent.click(screen.getByTitle("Send"));
    await awaitSend();
    await waitFor(() => expect(screen.getByRole("status", { name: "Agent is working" })).toBeTruthy());

    // A sustained outage means no further frames will arrive — the throbber
    // must stop with a clear note rather than spinning forever.
    mockConnState = "offline";
    rerender(view());

    await waitFor(() => expect(screen.getByText(/Connection lost/)).toBeTruthy());
    expect(screen.queryByRole("status", { name: "Agent is working" })).toBeNull();
  });

  it("marks the conversation ended and disables the input on session_ended", async () => {
    const textarea = await sendFirstMessage();
    capturedOnFrame!({
      type: "session_ended",
      session: S,
      payload: { session: S, reason: "succeeded" },
    });
    await waitFor(() => expect(screen.getByText("Conversation ended.")).toBeTruthy());
    await waitFor(() => {
      const ta = screen.getByPlaceholderText("This conversation has ended.") as HTMLTextAreaElement;
      expect(ta.disabled).toBe(true);
    });
    expect(textarea).toBeTruthy();
  });

  it("ignores frame types outside v1's rendered vocabulary without throwing", async () => {
    // NOTE: this deliberately uses a type the chat genuinely does not render.
    // It used to use tool_session_event, which IS rendered now (see the
    // tool-session tests below) — the guard is still worth keeping for the
    // frames that remain unhandled, but it has to name one of those.
    await sendFirstMessage();
    expect(() =>
      capturedOnFrame!({
        type: "permission_request",
        session: S,
        payload: { anything: "goes" },
      }),
    ).not.toThrow();
  });

  // b64 mirrors Go's []byte-as-base64 marshaling, so these frames carry the
  // shape the browser really receives.
  function b64(s: string): string {
    const bytes = new TextEncoder().encode(s);
    let bin = "";
    for (const byte of bytes) bin += String.fromCharCode(byte);
    return btoa(bin);
  }

  function toolDelta(ref: string, inner: Record<string, unknown>) {
    const session = S;
    return { type: "tool_session_delta", session, payload: { session, payload: { toolCallRef: ref, ...inner } } };
  }

  function toolEvent(ref: string, inner: Record<string, unknown>) {
    const session = S;
    return { type: "tool_session_event", session, payload: { session, payload: { toolCallRef: ref, ...inner } } };
  }

  it("renders a streaming tool session's raw output in the timeline", async () => {
    await sendFirstMessage();
    capturedOnFrame!({
      type: "tool_session_event",
      session: S,
      payload: {
        session: S,
        payload: { toolCallRef: "call-1", eventType: "tool_use_start", outerTool: "claude", reason: "running unit tests" },
      },
    });
    capturedOnFrame!(toolDelta("call-1", { stream: "stdout", data: b64("ok  demo/sample  0.42s\n") }));

    await waitFor(() => expect(screen.getByTestId("tool-session-body").textContent).toContain("ok  demo/sample"));
    expect(screen.getByText(/running unit tests/)).toBeTruthy();
  });

  // The whole frame→DOM path for a tool_use event, in the wire shape the runner
  // publishes (gloss on `summary`, ToolName only on the start). Every layer
  // below this carried `summary` correctly and the reducer dropped it, so the
  // block rendered a column of bare "Bash" tags with no command and blank rows
  // between them. Assert on rendered text, which is what that regression broke.
  it("renders an inner tool call with the command it ran and its outcome", async () => {
    await sendFirstMessage();
    capturedOnFrame!(
      toolEvent("call-1", {
        eventType: "tool_use_start",
        outerTool: "claude",
        reason: "running unit tests",
        toolName: "Bash",
        summary: "$ pnpm test",
      }),
    );
    capturedOnFrame!(toolEvent("call-1", { eventType: "tool_use_stop", ok: true, summary: "Test Files 3 passed" }));

    await waitFor(() => expect(screen.getByTestId("tool-session-body").textContent).toContain("$ pnpm test"));
    const body = screen.getByTestId("tool-session-body").textContent ?? "";
    expect(body).toContain("Bash");
    expect(body).toContain("✓");
    expect(body).toContain("Test Files 3 passed");
  });

  it("keeps one block per tool call rather than appending a line per chunk", async () => {
    await sendFirstMessage();
    capturedOnFrame!(toolDelta("call-1", { data: b64("first\n") }));
    capturedOnFrame!(toolDelta("call-1", { data: b64("second\n") }));
    capturedOnFrame!(toolDelta("call-1", { data: b64("third\n") }));

    await waitFor(() => expect(screen.getByTestId("tool-session-body").textContent).toContain("third"));
    expect(screen.getAllByTestId("tool-session-toggle")).toHaveLength(1);
    const body = screen.getByTestId("tool-session-body").textContent ?? "";
    expect(body).toContain("first");
    expect(body).toContain("second");
  });

  it("gives two concurrent tool calls their own blocks", async () => {
    await sendFirstMessage();
    capturedOnFrame!(toolDelta("call-1", { data: b64("from-one\n") }));
    capturedOnFrame!(toolDelta("call-2", { data: b64("from-two\n") }));

    await waitFor(() => expect(screen.getAllByTestId("tool-session-toggle")).toHaveLength(2));
    const bodies = screen.getAllByTestId("tool-session-body").map((b) => b.textContent ?? "");
    expect(bodies.some((t) => t.includes("from-one"))).toBe(true);
    expect(bodies.some((t) => t.includes("from-two"))).toBe(true);
  });

  it("closes out a failed tool session with its exit code", async () => {
    await sendFirstMessage();
    capturedOnFrame!(toolDelta("call-1", { data: b64("boom\n") }));
    capturedOnFrame!(toolDelta("call-1", { terminal: true, exitCode: 2, exitReason: "failed" }));

    await waitFor(() => expect(screen.getByTestId("tool-session-footer").textContent).toMatch(/exit 2/));
    // A failure stays expanded — it is the thing worth reading.
    expect(screen.getByTestId("tool-session-body").textContent).toContain("boom");
  });

  // NOTE: operation_activity mirrors turn_progress's wsFrame shape exactly
  // (pkg/web/webui/chat/sink.go's toFrame does `Payload: m` where m is
  // builtin.MsgOperationActivity{Session, Payload: channelevents.OperationActivityPayload}),
  // so the frame's payload double-nests: an outer {session, payload} envelope
  // wrapping the inner {operations, compactLine, cleared} — verified by
  // marshaling the real Go types, not assumed. See the turn_progress frames
  // above for the same shape.
  it("renders an operation_activity frame as a nested tree, cleared on turn end", async () => {
    await sendFirstMessage();
    capturedOnFrame!({
      type: "operation_activity",
      session: S,
      payload: {
        session: S,
        payload: {
          operations: [{ id: "op1", description: "fetch leadership goals", elapsedSeconds: 12, active: true,
            calls: [{ tool: "linear", reason: "querying Q3 goals", elapsedSeconds: 4, active: true }] }],
          compactLine: "fetch leadership goals ‣ querying Q3 goals",
        },
      },
    });
    await waitFor(() => expect(screen.getByText("fetch leadership goals")).toBeTruthy());
    expect(screen.getByText("querying Q3 goals")).toBeTruthy();
    // Cleared on turn end.
    capturedOnFrame!({
      type: "turn_activity",
      session: S,
      payload: { session: S, active: false, cause: "awaiting_user_message" },
    });
    await waitFor(() => expect(screen.queryByText("fetch leadership goals")).toBeNull());
  });

  it("clears the operation tree on a Cleared operation_activity tick", async () => {
    await sendFirstMessage();
    capturedOnFrame!({ type: "operation_activity", session: S,
      payload: { session: S,
        payload: { operations: [{ id: "op1", description: "compiling", elapsedSeconds: 3, active: true, calls: [] }] } } });
    await waitFor(() => expect(screen.getByText("compiling")).toBeTruthy());
    capturedOnFrame!({ type: "operation_activity", session: S,
      payload: { session: S, payload: { cleared: true } } });
    await waitFor(() => expect(screen.queryByText("compiling")).toBeNull());
  });

  // NOTE: interaction_request/applied mirror plan_update's wsFrame shape
  // exactly (pkg/web/webui/chat/sink.go's toFrame does `Payload: m` where m is
  // builtin.MsgInteractionRequest{Session, Payload: channelevents.
  // InteractionRequestPayload}), so the frame's payload double-nests — verified
  // against the Go wire shape, not assumed. See the plan_update frames above
  // for the same pattern.
  it("renders an interaction_request frame as an actionable card — the credential-prompt hang fix", async () => {
    await sendFirstMessage();
    const S = { namespace: NS, name: NAME };
    capturedOnFrame!({
      type: "interaction_request",
      session: S,
      payload: {
        session: S,
        payload: {
          agentSessionRef: S,
          category: "credential_link",
          requestRef: "req-1",
          lead: "Connect your account",
          actions: [{ id: "open", label: "Connect", kind: "link", url: "https://link.example.com/x" }],
          audience: { scope: "requester" },
        },
      },
    });
    await waitFor(() => expect(screen.getByText("Connect your account")).toBeTruthy());
    const link = screen.getByRole("link", { name: /Connect/ });
    expect(link.getAttribute("href")).toBe("https://link.example.com/x");
  });

  it("resolves the matching card in place on interaction_applied, keyed by requestRef", async () => {
    await sendFirstMessage();
    const S = { namespace: NS, name: NAME };
    const request = (requestRef: string, lead: string) => ({
      type: "interaction_request" as const,
      session: S,
      payload: {
        session: S,
        payload: {
          agentSessionRef: S, category: "credential_link", requestRef, lead,
          actions: [{ id: "open", label: "Connect", kind: "link" as const, url: "https://link.example.com/x" }],
          audience: { scope: "requester" },
        },
      },
    });
    // Two distinct requests…
    capturedOnFrame!(request("req-1", "Connect account A"));
    capturedOnFrame!(request("req-2", "Connect account B"));
    await waitFor(() => expect(screen.getByText("Connect account A")).toBeTruthy());
    expect(screen.getByText("Connect account B")).toBeTruthy();

    // …resolving req-1 only updates req-1's card; req-2 keeps its actions.
    capturedOnFrame!({
      type: "interaction_applied",
      session: S,
      payload: { session: S, payload: {
        agentSessionRef: S, category: "credential_link", requestRef: "req-1",
        outcome: "resolved", outcomeText: "Account A connected",
      } },
    });
    await waitFor(() => expect(screen.getByText("Account A connected")).toBeTruthy());
    // req-1's own card no longer shows a Connect link; req-2's still does.
    expect(screen.getAllByRole("link", { name: /Connect/ })).toHaveLength(1);
    expect(screen.getByText("Connect account B")).toBeTruthy();
  });

  it("interaction_applied for a requestRef with no matching card is a clean no-op — no crash, no phantom card", async () => {
    // This is the actual production path for credential_link today: the
    // out-of-band CredentialLinkedWatcher confirmation carries
    // RequestRef=credentialName, which never matches the mintRequestID()
    // the webchat card was keyed by (see the "interaction_applied" case in
    // ChatView.tsx and the KNOWN SLICE-1 GAP comment there and in
    // credential_linked.go). This test proves that mismatch degrades
    // gracefully rather than crashing or fabricating a card.
    await sendFirstMessage();
    const S = { namespace: NS, name: NAME };
    capturedOnFrame!({
      type: "interaction_applied",
      session: S,
      payload: {
        session: S,
        payload: {
          agentSessionRef: S, category: "credential_link", requestRef: "no-such-request",
          outcome: "resolved", outcomeText: "GitHub connected",
        },
      },
    });
    // No crash, and nothing resembling an interaction card materialized.
    expect(screen.queryAllByTestId("interaction-card")).toHaveLength(0);
    expect(screen.queryByText("GitHub connected")).toBeNull();

    // The no-op didn't corrupt any state: a subsequent, correctly-keyed
    // interaction_request still renders a card normally.
    capturedOnFrame!({
      type: "interaction_request",
      session: S,
      payload: {
        session: S,
        payload: {
          agentSessionRef: S, category: "credential_link", requestRef: "req-after",
          lead: "Connect your account",
          actions: [{ id: "open", label: "Connect", kind: "link", url: "https://link.example.com/x" }],
          audience: { scope: "requester" },
        },
      },
    });
    await waitFor(() => expect(screen.getByText("Connect your account")).toBeTruthy());
  });

  it("a decision-kind interaction action POSTs the session's decision path with category/requestRef/actionId — and no session in the body", async () => {
    const fetchMock = stubFetch({});
    render(view());
    const textarea = await screen.findByPlaceholderText("Message the agent…");
    fireEvent.change(textarea, { target: { value: "hi" } });
    fireEvent.click(screen.getByTitle("Send"));
    await awaitSend();

    const S = { namespace: NS, name: NAME };
    capturedOnFrame!({
      type: "interaction_request",
      session: S,
      payload: {
        session: S,
        payload: {
          agentSessionRef: S, category: "identity_choice", requestRef: "req-3",
          lead: "How should this agent authenticate?",
          actions: [{ id: "agent", label: "Let the agent continue", kind: "decision" }],
          audience: { scope: "requester" },
        },
      },
    });
    const btn = await screen.findByRole("button", { name: "Let the agent continue" });
    fireEvent.click(btn);

    await waitFor(() => {
      const calls = fetchMock.mock.calls.filter(([url]) => url === `${API}/decision`);
      expect(calls).toHaveLength(1);
    });
    const call = fetchMock.mock.calls.find(([url]) => url === `${API}/decision`)!;
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body).toEqual({ category: "identity_choice", requestRef: "req-3", actionId: "agent" });
    expect(String(call[0])).toBe(`${API}/decision`);
  });

  it("a failed decision POST surfaces a clear error line rather than hanging silently", async () => {
    stubFetch({ decisionStatus: 500 });
    render(view());
    const textarea = await screen.findByPlaceholderText("Message the agent…");
    fireEvent.change(textarea, { target: { value: "hi" } });
    fireEvent.click(screen.getByTitle("Send"));
    await awaitSend();

    const S = { namespace: NS, name: NAME };
    capturedOnFrame!({
      type: "interaction_request",
      session: S,
      payload: {
        session: S,
        payload: {
          agentSessionRef: S, category: "identity_choice", requestRef: "req-3",
          lead: "How should this agent authenticate?",
          actions: [{ id: "agent", label: "Let the agent continue", kind: "decision" }],
          audience: { scope: "requester" },
        },
      },
    });
    const btn = await screen.findByRole("button", { name: "Let the agent continue" });
    fireEvent.click(btn);
    // A non-OK response is surfaced as an explicit line rather than leaving the
    // now-disabled button hanging with no feedback (no-silent-errors, AGENTS.md).
    await waitFor(() => expect(screen.getByText("Failed to submit decision (500).")).toBeTruthy());
  });
});

describe("ChatView — interrupt & send now", () => {
  async function sendFirstMessageAndGoActive() {
    render(view());
    const textarea = await screen.findByPlaceholderText("Message the agent…");
    fireEvent.change(textarea, { target: { value: "hi" } });
    fireEvent.click(screen.getByTitle("Send"));
    await awaitSend();
    capturedOnFrame!({
      type: "turn_activity",
      session: S,
      payload: { session: S, active: true },
    });
    // Queue a follow-up WHILE the agent is working — the "Interrupt & Send Now"
    // button only appears when there is a queued message to send.
    fireEvent.change(textarea, { target: { value: "also do this" } });
    fireEvent.click(screen.getByTitle("Send"));
    await screen.findByTestId("queued-message");
    return textarea;
  }

  it("interrupt button appears while working, POSTs on click", async () => {
    vi.spyOn(crypto, "randomUUID").mockReturnValue("test-rid" as unknown as `${string}-${string}-${string}-${string}-${string}`);
    const fetchMock = stubFetch({});
    await sendFirstMessageAndGoActive();

    const interruptBtn = await screen.findByRole("button", { name: "Interrupt & Send Now" });
    fireEvent.click(interruptBtn);

    await waitFor(() => {
      const calls = fetchMock.mock.calls.filter(([url]) => url === `${API}/interrupt`);
      expect(calls).toHaveLength(1);
    });
    const call = fetchMock.mock.calls.find(([url]) => url === `${API}/interrupt`)!;
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body).toEqual({ requestId: "test-rid" });
    expect(String(call[0])).toBe(`${API}/interrupt`);

    // Disabled while the interrupt is pending, to prevent a double-fire.
    expect((screen.getByRole("button", { name: "Interrupt & Send Now" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("interrupt_applied interrupted → system line + un-gray", async () => {
    vi.spyOn(crypto, "randomUUID").mockReturnValue("test-rid" as unknown as `${string}-${string}-${string}-${string}-${string}`);
    const fetchMock = stubFetch({});
    await sendFirstMessageAndGoActive();

    fireEvent.click(screen.getByRole("button", { name: "Interrupt & Send Now" }));
    await waitFor(() => {
      const calls = fetchMock.mock.calls.filter(([url]) => url === `${API}/interrupt`);
      expect(calls).toHaveLength(1);
    });

    capturedOnFrame!({
      type: "interrupt_applied",
      session: S,
      payload: { session: S, requestID: "test-rid", outcome: "interrupted" },
    });

    await waitFor(() =>
      expect(screen.getByText("Interrupting — sending your message(s) now.")).toBeTruthy(),
    );
    // After "interrupted", the queue un-grays (the runner drains it now), so the
    // button — gated on a queued message — disappears rather than re-enabling.
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Interrupt & Send Now" })).toBeNull(),
    );
  });

  it("interrupt_applied rejected → reason shown", async () => {
    vi.spyOn(crypto, "randomUUID").mockReturnValue("test-rid" as unknown as `${string}-${string}-${string}-${string}-${string}`);
    const fetchMock = stubFetch({});
    await sendFirstMessageAndGoActive();

    fireEvent.click(screen.getByRole("button", { name: "Interrupt & Send Now" }));
    await waitFor(() => {
      const calls = fetchMock.mock.calls.filter(([url]) => url === `${API}/interrupt`);
      expect(calls).toHaveLength(1);
    });

    capturedOnFrame!({
      type: "interrupt_applied",
      session: S,
      payload: {
        session: S,
        requestID: "test-rid",
        outcome: "rejected",
        reason: "terraform_apply is changing state",
      },
    });

    await waitFor(() =>
      expect(screen.getByText("Couldn't interrupt — terraform_apply is changing state")).toBeTruthy(),
    );
  });
});

// The transcript is loaded from the session's own path the moment the view
// mounts — there is no resume click any more, because the shell already chose
// the session and this view opens that one and no other.
describe("ChatView — the transcript it replays", () => {
  it("replays the session's stored transcript on mount — messages and plan cards alike", async () => {
    const fetchMock = stubFetch({
      timeline: [
        { kind: "message", role: "user", text: "earlier question", createdAt: "2026-07-04T09:00:00Z" },
        {
          kind: "plan",
          plan: { planName: "trip", items: [{ id: "a", label: "Pack bags", status: "done" }], updatedAt: "2026-07-04T09:00:00Z" },
          createdAt: "2026-07-04T09:00:00Z",
        },
        { kind: "message", role: "agent", text: "earlier answer", createdAt: "2026-07-04T09:00:01Z" },
      ],
    });
    render(view());

    await waitFor(() => expect(screen.getByText("earlier question")).toBeTruthy());
    expect(screen.getByText("Pack bags")).toBeTruthy();
    expect(screen.getByText("earlier answer")).toBeTruthy();
    // Opening a conversation is a READ: nothing is sent on mount.
    expect(fetchMock.mock.calls.some(([url]) => url === `${API}/message`)).toBe(false);
  });

  // The route-level Authorize gate answers an unauthorized or unknown session
  // with an HTML system page BEFORE the handler runs, so this is what the
  // browser really receives — not the 404 JSON the handler tests describe. A
  // view that read it as JSON would throw inside its own fetch handler and
  // render an empty transcript with no explanation.
  it("says the viewer has no access when the route gate answers with its HTML page, rather than failing to parse", async () => {
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url === `${API}/messages`) return html(403);
        throw new Error("unexpected fetch: " + url);
      }),
    );
    render(view());

    await waitFor(() => expect(screen.getByText("You do not have access to this conversation.")).toBeTruthy());
    expect(errorSpy).toHaveBeenCalledWith("chat: load transcript failed", { ns: NS, name: NAME, status: 403 });
  });

  // The gate's other two HTML answers. 401 is an ordinary occurrence (the login
  // expired between page load and this read) and 503 is the fail-closed arm that
  // exists precisely so an indeterminate authorization check does not read as a
  // denial — so neither may collapse into the generic "could not be loaded".
  it.each([
    [401, "You are no longer signed in. Reload the page to continue."],
    [503, "That could not be checked right now. Try again in a moment."],
  ])("answers a %i from the route gate with its own copy, not the generic failure", async (status, copy) => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url === `${API}/messages`) return html(status as number);
        throw new Error("unexpected fetch: " + url);
      }),
    );
    render(view());

    await waitFor(() => expect(screen.getByText(copy as string)).toBeTruthy());
  });

  it("surfaces a send the route gate refused with its HTML page on the bubble, not as a parse failure", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url === `${API}/messages`) return json({ timeline: [] });
        if (url === `${API}/message`) return html(403);
        throw new Error("unexpected fetch: " + url);
      }),
    );
    render(view());

    const textarea = await screen.findByPlaceholderText("Message the agent…");
    fireEvent.change(textarea, { target: { value: "hi" } });
    fireEvent.click(screen.getByTitle("Send"));

    const failed = await screen.findByTestId("failed-message");
    expect(failed.textContent).toContain("You do not have access to this conversation.");
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });
});

describe("ChatView — reload fidelity (live == reload)", () => {
  // timeline extracts the ordered user/agent/plan lines the user actually
  // sees, ignoring live-only affordances (streaming cursor, throbber).
  function timeline(container: HTMLElement): { role: string; text: string }[] {
    return Array.from(container.querySelectorAll('[data-testid="chat-line"]'))
      .map((el) => ({ role: el.getAttribute("data-role") ?? "", text: el.textContent ?? "" }))
      .filter((l) => l.role === "user" || l.role === "agent" || l.role === "plan");
  }

  // renderLive sends "hi", plays the given frames (ending settled), and returns
  // the resulting timeline.
  async function renderLive(frames: ChatFrame[]): Promise<{ role: string; text: string }[]> {
    stubFetch({});
    const { container } = render(view());
    const textarea = await screen.findByPlaceholderText("Message the agent…");
    fireEvent.change(textarea, { target: { value: "hi" } });
    fireEvent.click(screen.getByTitle("Send"));
    await awaitSend();
    for (const f of frames) act(() => capturedOnFrame!(f));
    // Settle the turn so live-only affordances clear.
    act(() =>
      capturedOnFrame!({
        type: "turn_activity",
        session: S,
        payload: { session: S, active: false, cause: "awaiting_user_message" },
      }),
    );
    return timeline(container);
  }

  // renderReload mounts the view fresh against the given persisted timeline —
  // which is exactly what a browser reload does now that the shell chooses the
  // session — and returns the resulting timeline.
  async function renderReload(items: TimelineItem[]): Promise<{ role: string; text: string }[]> {
    cleanup();
    stubFetch({ timeline: items });
    const { container } = render(view());
    await waitFor(() => expect(container.querySelectorAll('[data-testid="chat-line"]').length).toBeGreaterThan(0));
    return timeline(container);
  }

  const um = (text: string): ChatFrame => ({
    type: "user_message",
    session: S,
    payload: { session: S, text },
  });
  const delta = (text: string): ChatFrame => ({
    type: "stream_delta",
    session: S,
    payload: { session: S, payload: { eventType: "text_delta", text, blockIdx: 0 } },
  });
  const plan = (name: string, status: string, label: string): ChatFrame => ({
    type: "plan_update",
    session: S,
    payload: { session: S, payload: {
      planName: name, items: [{ id: "a", label, status }], updatedAt: "2026-01-01T00:00:00Z" } },
  });

  it("a plain reply renders identically live and on reload", async () => {
    const live = await renderLive([um("the answer is 42")]);
    const reload = await renderReload([
      { kind: "message", role: "user", text: "hi", createdAt: "2026-07-04T09:00:00Z" },
      { kind: "message", role: "agent", text: "the answer is 42", createdAt: "2026-07-04T09:00:01Z" },
    ]);
    expect(live).toEqual([
      { role: "user", text: "hi" },
      { role: "agent", text: "the answer is 42" },
    ]);
    expect(reload).toEqual(live);
  });

  it("streamed preamble is discarded live, so reload of the reply-only transcript matches", async () => {
    // The regression guard: live must finalize to the respond_to_user reply,
    // NOT the streamed preamble — mirroring the backend's authoritative
    // respond_to_user handling.
    const live = await renderLive([delta("let me "), delta("think… "), um("the answer is 42")]);
    const reload = await renderReload([
      { kind: "message", role: "user", text: "hi", createdAt: "2026-07-04T09:00:00Z" },
      { kind: "message", role: "agent", text: "the answer is 42", createdAt: "2026-07-04T09:00:01Z" },
    ]);
    expect(live).toEqual([
      { role: "user", text: "hi" },
      { role: "agent", text: "the answer is 42" },
    ]);
    expect(reload).toEqual(live);
  });

  it("a multi-line reply renders identically live and on reload", async () => {
    const body = "line one\nline two";
    const live = await renderLive([um(body)]);
    const reload = await renderReload([
      { kind: "message", role: "user", text: "hi", createdAt: "2026-07-04T09:00:00Z" },
      { kind: "message", role: "agent", text: body, createdAt: "2026-07-04T09:00:01Z" },
    ]);
    expect(reload).toEqual(live);
  });

  it("a plan renders identically live and on reload", async () => {
    const live = await renderLive([plan("trip", "done", "Pack bags"), um("done packing")]);
    const reload = await renderReload([
      { kind: "message", role: "user", text: "hi", createdAt: "2026-07-04T09:00:00Z" },
      { kind: "plan", plan: { planName: "trip", items: [{ id: "a", label: "Pack bags", status: "done" }], updatedAt: "2026-01-01T00:00:00Z" }, createdAt: "2026-07-04T09:00:00Z" },
      { kind: "message", role: "agent", text: "done packing", createdAt: "2026-07-04T09:00:01Z" },
    ]);
    expect(reload).toEqual(live);
  });

  it("a plan updated across a message stays ONE card, identical live and reload (multi-burst)", async () => {
    // Live: plan v1, a reply, plan v2 (same name, updated in place at first appearance).
    const live = await renderLive([plan("trip", "in_progress", "Pack bags"), um("halfway"), plan("trip", "done", "Pack bags")]);
    // Reload: the reconstructed final shape — one card (final state) at first appearance, then the message.
    const reload = await renderReload([
      { kind: "message", role: "user", text: "hi", createdAt: "2026-07-04T09:00:00Z" },
      { kind: "plan", plan: { planName: "trip", items: [{ id: "a", label: "Pack bags", status: "done" }], updatedAt: "2026-01-01T00:00:00Z" }, createdAt: "2026-07-04T09:00:00Z" },
      { kind: "message", role: "agent", text: "halfway", createdAt: "2026-07-04T09:00:01Z" },
    ]);
    expect(reload).toEqual(live);
  });

  it("a deleted plan renders as an identical cancelled stub live and on reload", async () => {
    // Delete = update_plan with empty items → the live "cancelled stub" card.
    // Reload reconstructs the same empty-items stub (matches live, not dropped).
    const planDelete = (name: string): ChatFrame => ({
      type: "plan_update",
      session: S,
      payload: { session: S, payload: {
        planName: name, items: [], updatedAt: "2026-01-01T00:00:00Z" } },
    });
    const live = await renderLive([plan("trip", "done", "Pack bags"), planDelete("trip"), um("scrapped it")]);
    const reload = await renderReload([
      { kind: "message", role: "user", text: "hi", createdAt: "2026-07-04T09:00:00Z" },
      { kind: "plan", plan: { planName: "trip", items: [], updatedAt: "2026-01-01T00:00:00Z" }, createdAt: "2026-07-04T09:00:00Z" },
      { kind: "message", role: "agent", text: "scrapped it", createdAt: "2026-07-04T09:00:01Z" },
    ]);
    expect(reload).toEqual(live);
  });
});

describe("ChatView — a server notice on the send response", () => {
  // sampleNotice is the wire shape a refused/continued send comes back with:
  // messageResponse.Notice (pkg/web/webui/chat/handlers.go) carrying
  // channelevents.NoticeWire. Modelled on the pipeline's refusalNotice — an
  // untrusted `excerpt` reason, a trusted lead/body, and a nextStep, which is
  // mandatory at this tone (channelinteractions.Tone.RequiresNextStep).
  const sampleNotice = {
    category: "session_ended",
    tone: "critical",
    terminal: true,
    lead: "This session has ended and can't continue",
    body: "The agent hit an unrecoverable failure.",
    nextStep: "Start a new thread to begin again.",
    excerpt: { label: "Reason", content: "the runner pod was evicted" },
  };

  // stubTwoSends answers the FIRST send with a plain routed accept and the
  // SECOND with `second` — the two-send shape is kept because it is what a
  // real refusal looks like: the notice rides on a later, non-routed outcome
  // in a conversation that was already accepting messages.
  function stubTwoSends(second: unknown) {
    let messageCalls = 0;
    const fn = vi.fn(async (url: string) => {
      if (url === `${API}/messages`) return json({ timeline: [] });
      if (url === `${API}/message`) {
        messageCalls += 1;
        return json(messageCalls === 1 ? { sessionId: NAME, routed: true, ended: false } : second);
      }
      throw new Error("unexpected fetch: " + url);
    });
    vi.stubGlobal("fetch", fn);
    return fn;
  }

  async function sendTwice(second: unknown) {
    stubTwoSends(second);
    render(view());
    const textarea = await screen.findByPlaceholderText("Message the agent…");
    fireEvent.change(textarea, { target: { value: "first" } });
    fireEvent.click(screen.getByTitle("Send"));
    await awaitSend();
    fireEvent.change(textarea, { target: { value: "second" } });
    fireEvent.click(screen.getByTitle("Send"));
  }

  it("a non-routed send renders the notice's lead, body and next step — never silence", async () => {
    await sendTwice({ sessionId: NAME, routed: false, ended: false, notice: sampleNotice });

    // The lead is the headline the user must see; the next step is what stops
    // them being stuck (the tone's RequiresNextStep contract).
    await waitFor(() => expect(screen.getByText(sampleNotice.lead)).toBeTruthy());
    expect(screen.getByText(sampleNotice.body)).toBeTruthy();
    expect(screen.getByText(sampleNotice.nextStep)).toBeTruthy();
  });

  it("renders the notice's untrusted excerpt fenced, as literal text", async () => {
    await sendTwice({ sessionId: NAME, routed: false, ended: false, notice: sampleNotice });

    const card = await screen.findByTestId("notice-card");
    const pre = card.querySelector("pre");
    expect(pre).toBeTruthy();
    expect(pre!.textContent).toContain("the runner pod was evicted");
    expect(pre!.textContent).toContain("Reason");
  });

  it("a routed send carrying no notice renders no notice card", async () => {
    await sendTwice({ sessionId: NAME, routed: true, ended: false });

    await waitFor(() => expect(screen.getByText("second")).toBeTruthy());
    expect(screen.queryByTestId("notice-card")).toBeNull();
  });

  it("a notice with an empty lead renders nothing rather than an empty card", async () => {
    await sendTwice({ sessionId: NAME, routed: false, ended: false, notice: { tone: "routine", lead: "" } });

    await waitFor(() => expect(screen.getByText("second")).toBeTruthy());
    expect(screen.queryByTestId("notice-card")).toBeNull();
  });
});

// The server's answer about whether the conversation is over arrives as the
// `readOnly` prop — the ladder resolved it before this view mounted, so the
// composer comes back disabled without the view guessing from a phase string.
describe("ChatView — a conversation the server reports as ended", () => {
  it("disables the composer, and still renders the transcript, instead of letting the next send 404", async () => {
    stubFetch({
      timeline: [{ kind: "message", role: "agent", text: "earlier answer", createdAt: "2026-07-04T09:00:00Z" }],
    });
    render(view(true));

    // The transcript still replays — a terminal conversation is readable, just
    // not resumable (Registry.authorizeRead keeps admitting it). Asserted
    // alongside the disabled composer because a view that rendered NOTHING
    // would also have a disabled composer.
    await waitFor(() => expect(screen.getByText("earlier answer")).toBeTruthy());

    const ta = (await screen.findByPlaceholderText("This conversation has ended.")) as HTMLTextAreaElement;
    expect(ta.disabled).toBe(true);
  });

  it("leaves the composer enabled when the server reports the conversation as live", async () => {
    stubFetch({});
    render(view(false));

    const ta = (await screen.findByPlaceholderText("Message the agent…")) as HTMLTextAreaElement;
    expect(ta.disabled).toBe(false);
  });
});

// A session parked in AwaitingRetry is waiting for the USER, and webchat used
// to say nothing at all — the transcript simply stopped and the person sat
// looking at it.
//
// channelsd publishes a Retry prompt for this, but its sessionWatcher skips
// every client-hosted session (clientHostedHere) — and `browser` is exactly
// that: surfaced by webd, not channelsd. So the envelope this surface would
// have rendered is never sent for it, and webchat has to say so itself. The
// phase is already on the detail payload; nothing read it.
describe("ChatView parked-session surfacing", () => {
  it("tells the user a retry-parked session is waiting on them", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url === `${API}/messages`) return json({ timeline: [] });
        if (url === `${API}/detail`)
          return json({
            ...sampleDetail,
            phase: "AwaitingRetry",
            notices: [
              {
                kind: "awaiting_retry",
                tone: "degraded",
                lead: "This conversation is paused — the model connection dropped mid-turn.",
                nextStep: "Send a message to retry; it picks up where it left off.",
              },
            ],
          });
        throw new Error("unexpected fetch: " + url);
      }),
    );
    render(view());

    // The words matter less than that SOMETHING names the state and the way
    // out; silence is the bug.
    expect(await screen.findByText(/paused/i)).toBeTruthy();
    expect(screen.getByText(/send a message/i)).toBeTruthy();
  });

  it("says nothing for a normally running session", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url === `${API}/messages`) return json({ timeline: [] });
        if (url === `${API}/detail`) return json({ ...sampleDetail, phase: "Running" });
        throw new Error("unexpected fetch: " + url);
      }),
    );
    render(view());

    await screen.findByPlaceholderText("Message the agent…");
    expect(screen.queryByText(/paused/i)).toBeNull();
  });
});

// The OTHER notices channelsd would have published are equally invisible to a
// client-hosted surface, so webchat must render whatever the derivation
// returns — not just the retry case that happened to be found first.
describe("ChatView renders every derived session notice", () => {
  const withNotices = (notices: unknown[]) =>
    vi.fn(async (url: string) => {
      if (url === `${API}/messages`) return json({ timeline: [] });
      if (url === `${API}/detail`) return json({ ...sampleDetail, notices });
      throw new Error("unexpected fetch: " + url);
    });

  it("shows a routine startup wait without inventing an action", async () => {
    vi.stubGlobal("fetch", withNotices([
      { kind: "startup", tone: "routine", lead: "Starting the security scanner — your request will run once it's ready…" },
    ]));
    render(view());
    expect(await screen.findByTestId("session-notice-startup")).toBeTruthy();
    expect(screen.getByText(/security scanner/i)).toBeTruthy();
  });

  it("shows a scheduling wait with the underlying cause", async () => {
    vi.stubGlobal("fetch", withNotices([
      { kind: "scheduling", tone: "routine", lead: "Waiting for capacity to run your request…", body: "0/1 nodes available" },
    ]));
    render(view());
    expect(await screen.findByTestId("session-notice-scheduling")).toBeTruthy();
    expect(screen.getByText("0/1 nodes available")).toBeTruthy();
  });

  it("renders several at once, most actionable first", async () => {
    vi.stubGlobal("fetch", withNotices([
      { kind: "awaiting_retry", tone: "degraded", lead: "This conversation is paused.", nextStep: "Send a message to retry." },
      { kind: "startup", tone: "routine", lead: "Starting up…" },
    ]));
    const { container } = render(view());
    await screen.findByTestId("session-notice-awaiting_retry");
    const rendered = Array.from(container.querySelectorAll("[data-testid^='session-notice-']")).map((el) =>
      el.getAttribute("data-testid"),
    );
    expect(rendered).toEqual(["session-notice-awaiting_retry", "session-notice-startup"]);
  });

  it("shows nothing for a healthy session", async () => {
    vi.stubGlobal("fetch", withNotices([]));
    const { container } = render(view());
    await screen.findByPlaceholderText("Message the agent…");
    expect(container.querySelector("[data-testid^='session-notice-']")).toBeNull();
  });
});

// The shell's StartupLine (SessionShell) reads the same signals.startup this
// view does, over the same session socket, and renders the pre-start blocker
// in its own words. While it is showing, the sessionnotice cards that say the
// same thing must not repeat it — but only those two; a degraded notice
// (awaiting_retry) is not a startup caption and stays regardless.
describe("the shell's startup line owns pre-start", () => {
  const withSchedulingAndRetry = () =>
    vi.fn(async (url: string) => {
      if (url === `${API}/messages`) return json({ timeline: [] });
      if (url === `${API}/detail`)
        return json({
          ...sampleDetail,
          notices: [
            { kind: "awaiting_retry", tone: "degraded", lead: "This conversation is paused.", nextStep: "Send a message to retry." },
            { kind: "scheduling", tone: "routine", lead: "Waiting for capacity to run your request…", body: "0/1 nodes available" },
          ],
        });
      throw new Error("unexpected fetch: " + url);
    });

  it("hides the scheduling card while the shell's startup line is showing, keeps the degraded one", async () => {
    vi.stubGlobal("fetch", withSchedulingAndRetry());
    render(
      <SessionSignalsContext.Provider
        value={{ ...INITIAL_SESSION_SIGNALS, startup: { text: "Can't start yet — waiting for capacity", short: null, stillTrying: false } }}
      >
        {view()}
      </SessionSignalsContext.Provider>,
    );
    await screen.findByTestId("session-notice-awaiting_retry");
    expect(screen.queryByTestId("session-notice-scheduling")).toBeNull();
  });

  it("shows the scheduling card when there is no startup line above it (no provider)", async () => {
    vi.stubGlobal("fetch", withSchedulingAndRetry());
    render(view());
    expect(await screen.findByTestId("session-notice-scheduling")).toBeTruthy();
  });
});


describe("durable approval replay", () => {
 const request = {
  agentSessionRef: S, category: "plan_phase", requestRef: "durable-plan-review",
  lead: "Approve the private reminder", body: "Original review instructions",
  fields: [{label:"Why",value:"Deliver one private reminder"}],
  actions: [{id:"approve",label:"Approve",kind:"decision" as const}],
  audience: {scope:"approvers"},
 };
 const applied = {agentSessionRef:S,category:request.category,requestRef:request.requestRef,outcome:"approved"};
 const timeline: TimelineItem[] = [
  {kind:"plan",plan:{planName:"reminder",items:[{id:"send",label:"Deliver private reminder",status:"done"}]},createdAt:"2026-10-03T16:54:08Z"},
  {kind:"interaction",interactionRequest:request,interactionApplied:applied,createdAt:"2026-10-03T16:54:11Z"},
 ];
 it("restores the same approved plan card after remount and repeated live requests", async () => {
  stubFetch({timeline});
  const first=render(view());
  await waitFor(()=>expect(screen.getByText("Approved")).toBeTruthy());
  expect(screen.getByText("Original review instructions")).toBeTruthy();
  expect(screen.getAllByText("Deliver private reminder")).toHaveLength(1);
  expect(screen.queryByRole("button",{name:"Approve"})).toBeNull();
  first.unmount();
  render(view());
  await waitFor(()=>expect(screen.getByText("Approved")).toBeTruthy());
  act(()=>capturedOnFrame!({type:"interaction_request",session:S,payload:{session:S,payload:request}}));
  expect(screen.getAllByText("Approved")).toHaveLength(1);
  expect(screen.getAllByText("Deliver private reminder")).toHaveLength(1);
  expect(screen.queryByRole("button",{name:"Approve"})).toBeNull();
 });
 it("retains a live approval that arrives before transcript bootstrap completes", async () => {
  let finish!: (value:Response)=>void;
  const waiting=new Promise<Response>((resolve)=>{finish=resolve;});
  vi.stubGlobal("fetch",vi.fn((url:string)=>url===`${API}/messages` ? waiting : Promise.resolve(json(sampleDetail))));
  render(view());
  act(()=>capturedOnFrame!({type:"interaction_applied",session:S,payload:{session:S,payload:applied}}));
  await act(async()=>finish(json({timeline:[{kind:"interaction",interactionRequest:request,createdAt:"2026-10-03T16:54:11Z"}]})));
  await waitFor(()=>expect(screen.getByText("Approved")).toBeTruthy());
  expect(screen.queryByRole("button",{name:"Approve"})).toBeNull();
 });
});
