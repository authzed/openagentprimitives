import { describe, it, expect } from "vitest";
import { render } from "@testing-library/react";
import { COMPONENTS } from "./registry";

function renderNode(props: Record<string, unknown>) {
  const El = COMPONENTS["ap:session_view"];
  // Renderer's second param (renderChild) is unused by this leaf renderer;
  // `undefined as never` satisfies the type's arity without a real one.
  return render(
    <>
      {El({ component: "ap:session_view", props } as never, undefined as never)}
    </>,
  );
}

describe("ap:session_view", () => {
  it("renders a same-origin iframe for a valid ns/name ref", () => {
    const { container } = renderNode({ sessionRef: "ns-x/weather-ai" });
    const iframe = container.querySelector("iframe");
    expect(iframe).not.toBeNull();
    expect(iframe!.getAttribute("src")).toBe("/session-view/ns-x/weather-ai");
  });

  it("url-encodes each segment", () => {
    const { container } = renderNode({ sessionRef: "ns x/a b" });
    expect(container.querySelector("iframe")!.getAttribute("src")).toBe(
      "/session-view/ns%20x/a%20b",
    );
  });

  for (const bad of [
    "",
    "a/b/c",
    "../x",
    "x/..",
    "/x",
    "x/",
    "http://evil/x",
  ]) {
    it(`renders a placeholder (no iframe) for malformed ref ${JSON.stringify(bad)}`, () => {
      const { container } = renderNode({ sessionRef: bad });
      expect(container.querySelector("iframe")).toBeNull();
    });
  }

  it("renders a placeholder on empty props (no throw)", () => {
    expect(() => renderNode({})).not.toThrow();
  });
});
