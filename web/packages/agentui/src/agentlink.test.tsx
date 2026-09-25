import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AgentLink } from "./agentlink";
import { renderNode } from "./renderNode";
import type { Node } from "./types";

afterEach(cleanup);

const agentLinkNode = (props: Record<string, unknown>): Node => ({ component: "ap:agentlink", props });

describe("ap:agentlink", () => {
  it("opens the new-session dialog for exactly that agent, in a new tab", () => {
    render(
      <AgentLink
        n={{
          component: "ap:agentlink",
          props: { namespace: "ws-demo", agentClass: "demo-agent", label: "Try it as yourself", prompt: "linear issues for PR #1234" },
        }}
      />,
    );
    const a = screen.getByTestId("ap-agentlink") as HTMLAnchorElement;
    expect(a.getAttribute("href")).toBe("/sessions?new=1&ns=ws-demo&agentClass=demo-agent&prompt=linear+issues+for+PR+%231234");
    expect(a.getAttribute("target")).toBe("_blank");
    expect(a.getAttribute("rel")).toContain("noopener");
    expect(a.textContent).toBe("Try it as yourself");
  });

  // href is deliberately not a prop this component reads (components.go
  // registers ap:agentlink with no href field at all) — an agent-supplied
  // "href" in props must never leak through to the anchor it renders.
  it("omits an empty prompt and never reads an href prop", () => {
    render(
      <AgentLink
        n={{ component: "ap:agentlink", props: { namespace: "ws-demo", agentClass: "demo-agent", label: "Try it", href: "/admin" } }}
      />,
    );
    expect((screen.getByTestId("ap-agentlink") as HTMLAnchorElement).getAttribute("href")).toBe(
      "/sessions?new=1&ns=ws-demo&agentClass=demo-agent",
    );
  });
});

// A namespace/agentClass that is not DNS-1123-label-shaped, or an empty
// label, must never reach the anchor this control opens in a new tab — it
// fails the WHOLE ap:agentlink node visibly (renderNode's FailedComponent),
// rather than building a malformed href. checkAgentLink
// (pkg/web/uicomponents/components.go) is the real gate; this is the second
// enforcement of the identical rule, for a value that reaches the browser
// some other way (an older bundle, a future author bypassing Check).
describe("ap:agentlink fails closed on an address-unsafe value", () => {
  let consoleError: typeof console.error;
  beforeEach(() => {
    consoleError = console.error;
    console.error = () => {};
  });
  afterEach(() => {
    console.error = consoleError;
  });

  const cases: { name: string; props: Record<string, unknown> }[] = [
    { name: "namespace is not a DNS-1123 label", props: { namespace: "Not_Valid!", agentClass: "demo-agent", label: "Try it" } },
    { name: "agentClass is not a DNS-1123 label", props: { namespace: "ws-demo", agentClass: "Not_Valid!", label: "Try it" } },
    { name: "label is empty", props: { namespace: "ws-demo", agentClass: "demo-agent", label: "" } },
  ];
  for (const tc of cases) {
    it(`renders the fail-visible card when ${tc.name}`, () => {
      render(renderNode(agentLinkNode(tc.props)));
      expect(screen.getByText(/could not be displayed/)).toBeInTheDocument();
      expect(screen.queryByTestId("ap-agentlink")).not.toBeInTheDocument();
    });
  }

  // The good case, through the same renderNode path, is unaffected.
  it("still renders normally through renderNode when every value is valid", () => {
    render(renderNode(agentLinkNode({ namespace: "ws-demo", agentClass: "demo-agent", label: "Try it" })));
    expect(screen.getByTestId("ap-agentlink")).toBeInTheDocument();
  });
});

// The one session every embed case is about, and the two addresses it is
// reachable at: the storage key the control remembers it under, and the detail
// route the mount-time check reads (the same one the framed chat itself uses).
const embedRef = "ws-abc123456789/demo-haiku-1a2b3c4d";
const embedKey = "ap-agentlink:ws-abc123456789/demo-haiku";
const embedDetailURL = "/sessions/api/ws-abc123456789/demo-haiku-1a2b3c4d/detail";

const embedNode = (extra: Record<string, unknown> = {}): Node => ({
  component: "ap:agentlink",
  props: { namespace: "ws-abc123456789", agentClass: "demo-haiku", label: "Start the test", embed: true, ...extra },
});

describe("embed mode", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    window.sessionStorage.clear();
    cleanup();
  });

  it("embed: the click starts the session as the viewer, mounts the chat in place, and opens the tab at the server's own address", async () => {
    const fetchSpy = vi.fn().mockResolvedValue(new Response(JSON.stringify({ ns: "ws-abc123456789", name: "demo-haiku-1a2b3c4d", href: "/sessions?session=ws-abc123456789%2Fdemo-haiku-1a2b3c4d&view=chat" }), { status: 200, headers: { "content-type": "application/json" } }));
    vi.stubGlobal("fetch", fetchSpy);
    render(renderNode(embedNode({ prompt: "cherry blossoms" })));
    fireEvent.click(screen.getByTestId("ap-agentlink-embed"));
    await waitFor(() => expect(screen.getByTestId("ap-chat")).toBeInTheDocument());
    expect(fetchSpy).toHaveBeenCalledWith("/sessions/api/start", expect.objectContaining({ method: "POST" }));
    const body = JSON.parse((fetchSpy.mock.calls[0][1] as RequestInit).body as string);
    expect(body).toEqual({ ns: "ws-abc123456789", agentClass: "demo-haiku", prompt: "cherry blossoms" });
    expect(screen.getByTestId("ap-chat").getAttribute("src")).toBe("/chat-embed/ws-abc123456789/demo-haiku-1a2b3c4d");
    // The server said where this session lives; the rebuilt string is only the
    // fallback for a ref that arrived with no start response behind it.
    expect(screen.getByTestId("ap-agentlink-open-tab")).toHaveAttribute("href", "/sessions?session=ws-abc123456789%2Fdemo-haiku-1a2b3c4d&view=chat");
    expect(screen.queryByTestId("ap-agentlink-embed")).toBeNull();
    expect(window.sessionStorage.getItem(embedKey)).toBe(embedRef);
  });

  it.each([
    ["protocol-relative", "//evil.example/sessions"],
    ["backslash-spelled", "/\\evil.example/sessions"],
    ["absolute", "https://evil.example/sessions"],
  ])("embed: a start href that would leave the origin (%s) is refused, logged, and the tab opens at the rebuilt address", async (_kind, href) => {
    const err = vi.spyOn(console, "error").mockImplementation(() => {});
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ ns: "ws-abc123456789", name: "demo-haiku-1a2b3c4d", href }), { status: 200, headers: { "content-type": "application/json" } })));
    render(renderNode(embedNode()));
    fireEvent.click(screen.getByTestId("ap-agentlink-embed"));
    await waitFor(() => expect(screen.getByTestId("ap-chat")).toBeInTheDocument());
    expect(screen.getByTestId("ap-agentlink-open-tab")).toHaveAttribute("href", "/sessions?session=ws-abc123456789%2Fdemo-haiku-1a2b3c4d");
    expect(err).toHaveBeenCalledTimes(1);
  });

  it("embed: a remembered session still reachable keeps its chat, offers no second Start, and opens the tab at the rebuilt address", async () => {
    const fetchSpy = vi.fn().mockResolvedValue(new Response(JSON.stringify({ phase: "Running" }), { status: 200, headers: { "content-type": "application/json" } }));
    vi.stubGlobal("fetch", fetchSpy);
    window.sessionStorage.setItem(embedKey, embedRef);
    render(renderNode(embedNode()));
    await waitFor(() => expect(fetchSpy).toHaveBeenCalledWith(embedDetailURL, { credentials: "same-origin" }));
    expect(screen.getByTestId("ap-chat")).toBeInTheDocument();
    expect(screen.queryByTestId("ap-agentlink-embed")).toBeNull();
    expect(screen.getByTestId("ap-agentlink-open-tab")).toHaveAttribute("href", "/sessions?session=ws-abc123456789%2Fdemo-haiku-1a2b3c4d");
    expect(window.sessionStorage.getItem(embedKey)).toBe(embedRef);
  });

  // The check is a background confirmation, not a gate: blanking the frame
  // until it answers would flicker the chat on every reload of a session that
  // is fine, which is the common case.
  it("embed: a remembered session mounts its chat immediately, before the check has answered", () => {
    vi.stubGlobal("fetch", vi.fn(() => new Promise<Response>(() => {})));
    window.sessionStorage.setItem(embedKey, embedRef);
    render(renderNode(embedNode()));
    expect(screen.getByTestId("ap-chat")).toBeInTheDocument();
    expect(screen.queryByTestId("ap-agentlink-embed")).toBeNull();
  });

  // "Start a fresh test" deletes the session the link remembers. Without this,
  // the repainted link mounts the dead session behind an access-denied frame
  // and never offers Start again.
  it("embed: a remembered session the server no longer serves is forgotten, and Start comes back", async () => {
    const err = vi.spyOn(console, "error").mockImplementation(() => {});
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("gone", { status: 404 })));
    window.sessionStorage.setItem(embedKey, embedRef);
    render(renderNode(embedNode()));
    await waitFor(() => expect(screen.getByTestId("ap-agentlink-embed")).toBeInTheDocument());
    expect(screen.queryByTestId("ap-chat")).toBeNull();
    expect(window.sessionStorage.getItem(embedKey)).toBeNull();
    // A 404/403 IS the answer to "is it still there" — expected, not an error.
    expect(err).not.toHaveBeenCalled();
  });

  it("embed: a check that never completed falls back to Start and is logged", async () => {
    const err = vi.spyOn(console, "error").mockImplementation(() => {});
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("offline")));
    window.sessionStorage.setItem(embedKey, embedRef);
    render(renderNode(embedNode()));
    await waitFor(() => expect(screen.getByTestId("ap-agentlink-embed")).toBeInTheDocument());
    expect(window.sessionStorage.getItem(embedKey)).toBeNull();
    expect(err).toHaveBeenCalled();
  });

  it("embed: a failed start says so and keeps the button", async () => {
    const err = vi.spyOn(console, "error").mockImplementation(() => {});
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("nope", { status: 403 })));
    render(renderNode(embedNode()));
    fireEvent.click(screen.getByTestId("ap-agentlink-embed"));
    await waitFor(() => expect(screen.getByText(/could not start/i)).toBeInTheDocument());
    expect(screen.getByTestId("ap-agentlink-embed")).toBeEnabled();
    expect(err).toHaveBeenCalled();
  });

  // A status the server chose means nothing was created, so a retry is safe —
  // and the server's own sentence says WHICH refusal it was, which the generic
  // line cannot.
  it("embed: a refused start shows the server's own reason and leaves Start pressable", async () => {
    const err = vi.spyOn(console, "error").mockImplementation(() => {});
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: "That agent is not available to start a session with." }), { status: 403, headers: { "content-type": "application/json" } })));
    render(renderNode(embedNode()));
    fireEvent.click(screen.getByTestId("ap-agentlink-embed"));
    await waitFor(() => expect(screen.getByText(/That agent is not available to start a session with\./)).toBeInTheDocument());
    expect(screen.getByTestId("ap-agentlink-embed")).toBeEnabled();
    expect(err).toHaveBeenCalled();
  });

  // No answer at all is not a refusal: the server may have created the session
  // anyway, so pressing Start again could start a second one.
  it("embed: a start that never answered says so and leaves Start disabled", async () => {
    const err = vi.spyOn(console, "error").mockImplementation(() => {});
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("offline")));
    render(renderNode(embedNode()));
    fireEvent.click(screen.getByTestId("ap-agentlink-embed"));
    await waitFor(() => expect(screen.getByText(/could not confirm whether that session started/i)).toBeInTheDocument());
    expect(screen.getByTestId("ap-agentlink-embed")).toBeDisabled();
    expect(screen.queryByText(/Try again/)).toBeNull();
    expect(err).toHaveBeenCalled();
  });
});
