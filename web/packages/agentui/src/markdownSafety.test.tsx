import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";
// No global jest-dom setupFile is wired for this package (see
// renderNode.test.tsx) — import the vitest adapter locally.
import "@testing-library/jest-dom/vitest";
import { renderNode } from "./renderNode";

// vitest.config.ts sets globals: false, so @testing-library/react never finds
// a global afterEach to register its cleanup with. Without this, a render()
// from one test can still be in document.body when the next test's
// querySelector runs — see renderNode.test.tsx for the same repo pattern.
afterEach(cleanup);

const md = (body: string) => renderNode({ component: "ap:markdown", props: { body } });

// This suite asserts a guarantee @ap/design's Markdown already provides (see
// markdown.tsx: rehype-raw is deliberately absent, urlTransform strips
// dangerous protocols). It is pinned HERE, from the agent-UI side, because
// ap:markdown is the intended home for every agent-authored prose body —
// Tier-1 markdown bodies are LLM output, so a future edit adding rehype-raw
// for an unrelated chat feature would silently turn every agent UI into an
// XSS surface. If any assertion below fails, that is a real finding: check
// whether rehype-raw has been added to markdown.tsx before touching this file.
describe("ap:markdown never renders markup from an agent-authored body", () => {
  it("renders a script tag as inert text, not as a script element", () => {
    const { container } = render(md("<script>window.__pwned = true</script>"));
    expect(container.querySelector("script")).toBeNull();
    expect((window as unknown as { __pwned?: boolean }).__pwned).toBeUndefined();
  });

  it("renders an img onerror payload as inert text, not as an element", () => {
    const { container } = render(md('<img src="x" onerror="window.__pwned = true">'));
    expect(container.querySelector("img")).toBeNull();
  });

  it("strips a javascript: URL from a markdown link", () => {
    const { container } = render(md("[click me](javascript:window.__pwned=true)"));
    const anchor = container.querySelector("a");
    expect(anchor?.getAttribute("href") ?? "").not.toContain("javascript:");
  });

  it("still renders legitimate GFM", () => {
    const { container } = render(md("# Heading\n\n- one\n- two\n\n`code`"));
    expect(container.querySelector("h1")?.textContent).toBe("Heading");
    expect(container.querySelectorAll("li")).toHaveLength(2);
    expect(container.querySelector("code")?.textContent).toBe("code");
  });
});
