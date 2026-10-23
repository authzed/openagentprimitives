import { describe, expect, it } from "vitest";
import { askPrompt, chatLinks, markdownPath } from "./copy-page";

const MD = "https://openap.org/docs/plan-gating.md";

describe("copy-page links", () => {
  it("points at the guide's Markdown rendition", () => {
    expect(markdownPath("plan-gating")).toBe("/docs/plan-gating.md");
  });

  it.each([
    ["chatgpt", "https://chatgpt.com/?hints=search&prompt="],
    ["claude", "https://claude.ai/new?q="],
  ])("%s: prefilled with a prompt naming the Markdown URL", (id, prefix) => {
    const link = chatLinks(MD).find((l) => l.id === id);
    expect(link?.href.startsWith(prefix)).toBe(true);
    const q = new URL(link!.href).searchParams;
    expect(q.get(id === "claude" ? "q" : "prompt")).toBe(askPrompt(MD));
  });

  it("encodes the prompt so the page URL survives as one parameter", () => {
    const { href } = chatLinks(`${MD}?a=1&b=2`)[1];
    expect(new URL(href).searchParams.get("q")).toContain("?a=1&b=2");
  });
});
