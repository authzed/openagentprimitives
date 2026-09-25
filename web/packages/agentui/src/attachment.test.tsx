import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderNode } from "./renderNode";
import type { Node } from "./types";
import { ViewSessionProvider } from "./viewSession";

afterEach(cleanup);

const node = (props: Record<string, unknown>): Node => ({
  component: "ap:attachment",
  props,
});
const meta = {
  name: "demo-agent draft",
  filename: "demo-agent.oap",
  size: 4096,
  mime: "application/vnd.agentprimitives.authzed.com.agent.v1",
};

function stubFetch(status: number, body: unknown) {
  const fetchMock = vi.fn(
    async () =>
      new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
      }),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function withSession(n: Node) {
  render(
    <ViewSessionProvider value={{ ns: "workshop", name: "sess-live" }}>
      {renderNode(n)}
    </ViewSessionProvider>,
  );
}

beforeEach(() => vi.unstubAllGlobals());

describe("ap:attachment", () => {
  it("fetches the newest revision's facts for the view's session and renders a file row with a download link", async () => {
    const fetchMock = stubFetch(200, meta);
    withSession(node({ artifact: "artifact-0123456789abcdef" }));
    const row = await screen.findByTestId("ap-attachment");
    expect(fetchMock).toHaveBeenCalledWith(
      "/artifact-view/meta?artifactId=artifact-0123456789abcdef&sessionRef=workshop%2Fsess-live",
      expect.objectContaining({ credentials: "same-origin" }),
    );
    expect(row).toHaveTextContent("demo-agent.oap");
    expect(row).toHaveTextContent("Agent bundle");
    expect(row).toHaveTextContent("4"); // 4 kB, in whatever unit format the locale gives
    const link = screen.getByRole("link", { name: /download/i });
    expect(link).toHaveAttribute(
      "href",
      "/artifact-download?artifactId=artifact-0123456789abcdef&sessionRef=workshop%2Fsess-live&fn=demo-agent.oap",
    );
  });

  it("prefers the label over the filename for the row's title, and knows the common kinds", async () => {
    stubFetch(200, { ...meta, filename: "report.html", mime: "text/html" });
    withSession(
      node({ artifact: "artifact-0123456789abcdef", label: "Your draft" }),
    );
    const row = await screen.findByTestId("ap-attachment");
    expect(row).toHaveTextContent("Your draft");
    expect(row).toHaveTextContent("Web page");
  });

  it("renders the fail-visible card when the lookup is refused", async () => {
    // A non-2xx response is logged the same way useBindings.ts logs its own
    // fetch failures (unconditionally, whether the cause was the network or
    // an authored refusal) — spied here so the assertion is about the card,
    // not about whether this expected log line prints.
    const consoleError = vi
      .spyOn(console, "error")
      .mockImplementation(() => {});
    stubFetch(403, "you do not have access to this artifact");
    withSession(node({ artifact: "artifact-0123456789abcdef" }));
    expect(
      await screen.findByTestId("ap-attachment-unavailable"),
    ).toHaveTextContent("Attachment unavailable");
    expect(screen.queryByRole("link")).toBeNull();
    consoleError.mockRestore();
  });

  it("renders the fail-visible card outside any view session", () => {
    stubFetch(200, meta);
    render(renderNode(node({ artifact: "artifact-0123456789abcdef" })));
    expect(screen.getByTestId("ap-attachment-unavailable")).toBeInTheDocument();
  });

  it("logs and shows the card when the fetch itself fails", async () => {
    const consoleError = vi
      .spyOn(console, "error")
      .mockImplementation(() => {});
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new Error("down");
      }),
    );
    withSession(node({ artifact: "artifact-0123456789abcdef" }));
    expect(
      await screen.findByTestId("ap-attachment-unavailable"),
    ).toBeInTheDocument();
    await waitFor(() => expect(consoleError).toHaveBeenCalled());
    consoleError.mockRestore();
  });
});
