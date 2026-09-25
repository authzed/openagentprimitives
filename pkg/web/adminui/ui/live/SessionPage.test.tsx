import { render, screen, waitFor, fireEvent, cleanup } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SessionPage } from "./SessionPage";

// routeFetch dispatches a stubbed response per URL suffix so the page's two
// fetches (session detail + full logs) each get the right body. A suffix mapped
// to a number is returned as that HTTP status with an {error} body.
function routeFetch(routes: Record<string, unknown>) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      for (const [suffix, body] of Object.entries(routes)) {
        if (url.endsWith(suffix)) {
          if (typeof body === "number") {
            return new Response(JSON.stringify({ error: "session default/s1 not tracked" }), { status: body });
          }
          return new Response(JSON.stringify(body), { status: 200 });
        }
      }
      return new Response(JSON.stringify({ error: `no stub for ${url}` }), { status: 404 });
    }),
  );
}

const DETAIL = {
  namespace: "default", name: "s1", class: "support-bot", channelKind: "slack",
  model: "claude-x", phase: "Running", startedAt: "2026-06-30T12:00:00Z",
  turnCount: 2, inputTokens: 12400, outputTokens: 3100, toolCallCount: 5,
  elapsedSeconds: 84, active: true, statusText: "running kubectl get pods",
  pendingToolGrants: 0, pendingLeakageApprovals: 0, lastEventAt: "2026-06-30T12:01:00Z",
  recentEvents: [
    { at: "2026-06-30T12:00:30Z", kind: "tool_activity", payload: { tool: "kubectl" } },
    { at: "2026-06-30T12:01:00Z", kind: "turn_completed" },
  ],
};

const LOGS = {
  entries: [
    { kind: "turn", id: "t1", index: 0, role: "user", createdAt: "2026-06-30T12:00:00Z", content: "deploy the service" },
    { kind: "turn", id: "t2", index: 1, role: "assistant", createdAt: "2026-06-30T12:00:10Z", content: "running kubectl apply" },
  ],
  truncated: false,
};

// TOOL_LOGS exercises the structured-block path: a tool_use (name + JSON input)
// and a failed tool_result.
const TOOL_LOGS = {
  entries: [
    {
      kind: "turn", id: "a1", index: 0, role: "assistant", createdAt: "2026-06-30T12:00:00Z",
      content: "→ kubectl(...)",
      blocks: [{ type: "tool_use", name: "kubectl", input: { verb: "get", resource: "pods" } }],
    },
    {
      kind: "turn", id: "a2", index: 1, role: "tool", createdAt: "2026-06-30T12:00:05Z",
      content: "← [error] boom",
      blocks: [{ type: "tool_result", content: "boom", isError: true }],
    },
  ],
  truncated: false,
};

beforeEach(() => window.history.pushState({}, "", "/admin/session/default/s1"));
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

describe("SessionPage", () => {
  it("Overview shows the phase, counters, and a Kill session affordance", async () => {
    routeFetch({ "/sessions/default/s1/logs": LOGS, "/sessions/default/s1": DETAIL });
    render(<SessionPage apiBase="/admin/api" id="default/s1" />);

    await waitFor(() => expect(screen.getByText(/running kubectl get pods/)).toBeTruthy());
    // phase badge (header) + counter line
    expect(screen.getAllByText("Running").length).toBeGreaterThan(0);
    expect(screen.getByText(/12\.4K in/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Kill session" })).toBeTruthy();
  });

  it("Overview shows one row per bundle with its OWN sandbox kind, even when bundles of the same session differ", async () => {
    const withBundles = {
      ...DETAIL,
      bundles: [
        { name: "primary", spiceboxSessionName: "s1-primary", sandboxKind: "pod", sandboxRef: "default/pod-abc", phase: "Ready", reason: "PodReady", workspaceMode: "shared" },
        { name: "secondary", spiceboxSessionName: "s1-secondary", sandboxKind: "agent-sandbox", sandboxRef: "default/claim-xyz", prewarmed: true, phase: "Ready", workspaceMode: "isolated" },
      ],
    };
    routeFetch({ "/sessions/default/s1/logs": LOGS, "/sessions/default/s1": withBundles });
    render(<SessionPage apiBase="/admin/api" id="default/s1" />);

    await waitFor(() => expect(screen.getByText("primary")).toBeTruthy());
    expect(screen.getByText("pod")).toBeTruthy();
    expect(screen.getByText("default/pod-abc")).toBeTruthy();
    expect(screen.getByText("secondary")).toBeTruthy();
    expect(screen.getByText("agent-sandbox")).toBeTruthy();
    expect(screen.getByText("warm")).toBeTruthy();
    // Each bundle's OWN workspace mode — same "per-bundle, not collapsed"
    // rule as sandbox kind (bundles can share vs. isolate independently).
    expect(screen.getByText("shared workspace")).toBeTruthy();
    expect(screen.getByText("isolated workspace")).toBeTruthy();
  });

  it("Overview omits the Bundles section entirely when the session has no resolved bundles", async () => {
    routeFetch({ "/sessions/default/s1/logs": LOGS, "/sessions/default/s1": DETAIL });
    render(<SessionPage apiBase="/admin/api" id="default/s1" />);

    await waitFor(() => expect(screen.getByText(/running kubectl get pods/)).toBeTruthy());
    expect(screen.queryByText("Bundles")).toBeNull();
  });

  it("Full Logs renders the decoded transcript chronologically", async () => {
    routeFetch({ "/sessions/default/s1/logs": LOGS, "/sessions/default/s1": DETAIL });
    render(<SessionPage apiBase="/admin/api" id="default/s1" tab="logs" />);

    await waitFor(() => expect(screen.getByText("deploy the service")).toBeTruthy());
    expect(screen.getByText("running kubectl apply")).toBeTruthy();
    expect(screen.getByText("user")).toBeTruthy();
    expect(screen.getByText("assistant")).toBeTruthy();
  });

  it("switching to the Full Logs tab deep-links ?tab=logs", async () => {
    routeFetch({ "/sessions/default/s1/logs": LOGS, "/sessions/default/s1": DETAIL });
    render(<SessionPage apiBase="/admin/api" id="default/s1" />);

    await waitFor(() => expect(screen.getByText(/running kubectl get pods/)).toBeTruthy());
    // Radix tabs activate on mousedown/focus (not the synthetic click) — this
    // fires onTab, which navigates and stamps ?tab=logs onto the URL.
    fireEvent.mouseDown(screen.getByRole("tab", { name: "Full Logs" }));
    expect(window.location.search).toBe("?tab=logs");
  });

  it("the Recent Activity tab lists ring-buffer events newest-first", async () => {
    routeFetch({ "/sessions/default/s1/logs": LOGS, "/sessions/default/s1": DETAIL });
    render(<SessionPage apiBase="/admin/api" id="default/s1" tab="activity" />);

    await waitFor(() => expect(screen.getByText(/turn_completed/)).toBeTruthy());
    expect(screen.getByText(/tool_activity/)).toBeTruthy();
  });

  it("a 404 detail renders the not-tracked state, not an error", async () => {
    routeFetch({ "/sessions/default/s1/logs": LOGS, "/sessions/default/s1": 404 });
    render(<SessionPage apiBase="/admin/api" id="default/s1" />);

    await waitFor(() => expect(screen.getByText(/is not tracked/)).toBeTruthy());
  });

  it("Full Logs renders a tool_use block: the tool name + a JSONView of the input", async () => {
    routeFetch({ "/sessions/default/s1/logs": TOOL_LOGS, "/sessions/default/s1": DETAIL });
    render(<SessionPage apiBase="/admin/api" id="default/s1" tab="logs" />);

    await waitFor(() => expect(screen.getByText("→ kubectl")).toBeTruthy());
    // The JSONView of the tool input renders the keyed tree.
    expect(screen.getByText("verb:")).toBeTruthy();
    expect(screen.getByText('"get"')).toBeTruthy();
  });

  it("Full Logs renders a failed tool_result block with error styling", async () => {
    routeFetch({ "/sessions/default/s1/logs": TOOL_LOGS, "/sessions/default/s1": DETAIL });
    render(<SessionPage apiBase="/admin/api" id="default/s1" tab="logs" />);

    const label = await screen.findByText("← error");
    expect(label.className).toContain("destructive");
  });

  it("Full Logs grays out <untrusted-tool-output> wrapper tags in text", async () => {
    const untrusted = {
      entries: [{
        kind: "turn", id: "u1", index: 0, role: "user", createdAt: "2026-06-30T12:00:00Z",
        content: "…",
        blocks: [{ type: "text", text: 'context <untrusted-tool-output nonce="abc123">payload</untrusted-tool-output> end' }],
      }],
      truncated: false,
    };
    routeFetch({ "/sessions/default/s1/logs": untrusted, "/sessions/default/s1": DETAIL });
    render(<SessionPage apiBase="/admin/api" id="default/s1" tab="logs" />);

    const openTag = await screen.findByText('<untrusted-tool-output nonce="abc123">');
    expect(openTag.className).toContain("muted");
    expect(screen.getByText("</untrusted-tool-output>").className).toContain("muted");
    // The enclosed content survives (de-emphasized, not dropped).
    expect(screen.getByText("payload")).toBeTruthy();
  });

  it("Full Logs renders a string tool_result trust-aware: grayed wrapper tags + literal payload, not a JSON-escaped string", async () => {
    const untrustedResult = {
      entries: [{
        kind: "turn", id: "r1", index: 0, role: "tool", createdAt: "2026-06-30T12:00:05Z",
        content: "← result",
        blocks: [{
          type: "tool_result",
          // The runner stamps the nonce on BOTH the open and close markers.
          content: '<untrusted-tool-output nonce="70c41265bfc4687d">\nstatus updated\n</untrusted-tool-output nonce="70c41265bfc4687d">',
        }],
      }],
      truncated: false,
    };
    routeFetch({ "/sessions/default/s1/logs": untrustedResult, "/sessions/default/s1": DETAIL });
    render(<SessionPage apiBase="/admin/api" id="default/s1" tab="logs" />);

    // The wrapper markers render as their own muted spans (not one JSON-escaped blob).
    const openTag = await screen.findByText('<untrusted-tool-output nonce="70c41265bfc4687d">');
    expect(openTag.className).toContain("muted");
    expect(screen.getByText('</untrusted-tool-output nonce="70c41265bfc4687d">').className).toContain("muted");
    // The payload renders literally — real newlines, no \" or \n escapes.
    expect(screen.getByText(/^\s*status updated\s*$/)).toBeTruthy();
  });

  it("Full Logs unwraps a JSON-string-encoded tool_result (double quotes around the wrapper) before trust-aware rendering", async () => {
    // Some results arrive with the wrapped string JSON-encoded once more —
    // literal leading/trailing double quotes and \n / \" escape sequences.
    const doubleEncoded = {
      entries: [{
        kind: "turn", id: "r2", index: 0, role: "tool", createdAt: "2026-06-30T12:00:05Z",
        content: "← result",
        blocks: [{
          type: "tool_result",
          content: JSON.stringify('<untrusted-tool-output nonce="def456">\nrows deleted\n</untrusted-tool-output nonce="def456">'),
        }],
      }],
      truncated: false,
    };
    routeFetch({ "/sessions/default/s1/logs": doubleEncoded, "/sessions/default/s1": DETAIL });
    render(<SessionPage apiBase="/admin/api" id="default/s1" tab="logs" />);

    const openTag = await screen.findByText('<untrusted-tool-output nonce="def456">');
    expect(openTag.className).toContain("muted");
    expect(screen.getByText('</untrusted-tool-output nonce="def456">').className).toContain("muted");
    expect(screen.getByText(/^\s*rows deleted\s*$/)).toBeTruthy();
  });

  it("Full Logs renders a JSON payload inside the untrusted wrapper as a JSONView tree", async () => {
    const jsonInside = {
      entries: [{
        kind: "turn", id: "r3", index: 0, role: "tool", createdAt: "2026-06-30T12:00:05Z",
        content: "← result",
        blocks: [{
          type: "tool_result",
          content:
            '<untrusted-tool-output nonce="4ec82dba0835891e">\n' +
            '{"results":[{"id":56426022008,"displayName":"Allstate"}],"total":4}\n' +
            '</untrusted-tool-output nonce="4ec82dba0835891e">',
        }],
      }],
      truncated: false,
    };
    routeFetch({ "/sessions/default/s1/logs": jsonInside, "/sessions/default/s1": DETAIL });
    render(<SessionPage apiBase="/admin/api" id="default/s1" tab="logs" />);

    // The trust markers still render muted…
    const openTag = await screen.findByText('<untrusted-tool-output nonce="4ec82dba0835891e">');
    expect(openTag.className).toContain("muted");
    // …but the JSON payload renders as a keyed JSONView tree, not one text blob.
    expect(screen.getByText("total:")).toBeTruthy();
    expect(screen.getByText("4")).toBeTruthy();
    expect(screen.getByText('"Allstate"')).toBeTruthy();
  });

  it("Full Logs shows the decoded requester (session initiator) for a legacy inferred actor", async () => {
    const withActor = {
      entries: [{
        kind: "turn", id: "h1", index: 0, role: "user", createdAt: "2026-06-30T12:00:00Z",
        content: "deploy the service", actor: "user:YWxpY2VAZXhhbXBsZS5jb20", // base64url(alice@example.com)
        actorInferred: true,
        blocks: [{ type: "text", text: "deploy the service" }],
      }],
      truncated: false,
    };
    routeFetch({ "/sessions/default/s1/logs": withActor, "/sessions/default/s1": DETAIL });
    render(<SessionPage apiBase="/admin/api" id="default/s1" tab="logs" />);

    await waitFor(() => expect(screen.getByText(/requested by/)).toBeTruthy());
    expect(screen.getByText("alice@example.com")).toBeTruthy();
    expect(screen.getByText(/session initiator/)).toBeTruthy();
    const link = screen.getByText("alice@example.com").closest("a");
    expect(link?.getAttribute("href")).toBe("/admin/user/user:YWxpY2VAZXhhbXBsZS5jb20");
  });

  it("Full Logs shows the decoded actor without the session-initiator caption for a real per-turn author", async () => {
    const withActor = {
      entries: [{
        kind: "turn", id: "h2", index: 0, role: "user", createdAt: "2026-06-30T12:00:00Z",
        content: "deploy the service", actor: "user:YWxpY2VAZXhhbXBsZS5jb20", // base64url(alice@example.com)
        blocks: [{ type: "text", text: "deploy the service" }],
      }],
      truncated: false,
    };
    routeFetch({ "/sessions/default/s1/logs": withActor, "/sessions/default/s1": DETAIL });
    render(<SessionPage apiBase="/admin/api" id="default/s1" tab="logs" />);

    await waitFor(() => expect(screen.getByText(/^by$/)).toBeTruthy());
    expect(screen.getByText("alice@example.com")).toBeTruthy();
    expect(screen.queryByText(/session initiator/)).toBeNull();
  });

  it("Full Logs renders flat content that is a JSON document (e.g. a system_note) as a JSONView tree", async () => {
    const systemNote = {
      entries: [{
        kind: "system_note", id: "n1", index: 0, createdAt: "2026-07-02T14:56:46Z",
        content: '{"delivered":["toolu_01Ebbd7RJ2QUnuW8Q4BjMzeE"]}',
      }],
      truncated: false,
    };
    routeFetch({ "/sessions/default/s1/logs": systemNote, "/sessions/default/s1": DETAIL });
    render(<SessionPage apiBase="/admin/api" id="default/s1" tab="logs" />);

    await screen.findByText("delivered:");
    expect(screen.getByText('"toolu_01Ebbd7RJ2QUnuW8Q4BjMzeE"')).toBeTruthy();
  });

  it("Full Logs falls back to flat content when an entry has no blocks", async () => {
    const noBlocks = {
      entries: [{ kind: "turn", id: "c1", index: 0, role: "assistant", createdAt: "2026-06-30T12:00:00Z", content: "plain summary line" }],
      truncated: false,
    };
    routeFetch({ "/sessions/default/s1/logs": noBlocks, "/sessions/default/s1": DETAIL });
    render(<SessionPage apiBase="/admin/api" id="default/s1" tab="logs" />);

    await waitFor(() => expect(screen.getByText("plain summary line")).toBeTruthy());
  });
});
