import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { chatSrc } from "./chat";
import { renderNode } from "./renderNode";

afterEach(cleanup);

describe("ap:chat", () => {
  it("frames /chat-embed/{ns}/{name} same-origin, with no sandbox attribute", () => {
    render(renderNode({ component: "ap:chat", props: { sessionRef: "ws-c829e029cffc/demo-haiku-d44fc350" } }));
    const frame = screen.getByTestId("ap-chat") as HTMLIFrameElement;
    expect(frame.getAttribute("src")).toBe("/chat-embed/ws-c829e029cffc/demo-haiku-d44fc350");
    expect(frame).not.toHaveAttribute("sandbox");
    expect(frame).toHaveAttribute("title", "Test chat");
  });

  it("renders an inert notice, never an iframe, for a malformed ref", () => {
    render(renderNode({ component: "ap:chat", props: { sessionRef: "../etc" } }));
    expect(screen.queryByTestId("ap-chat")).toBeNull();
    expect(screen.getByText(/no valid session reference/i)).toBeInTheDocument();
  });

  it("chatSrc encodes each segment and refuses anything but two", () => {
    expect(chatSrc("a b/c")).toBe("/chat-embed/a%20b/c");
    expect(chatSrc("a")).toBeNull();
    expect(chatSrc("a/b/c")).toBeNull();
    expect(chatSrc(".."+"/x")).toBeNull();
  });
});
