import { describe, it, expect } from "vitest";
import { resolveClickTarget, elementLabel } from "./targets";

function frag(html: string): Document {
  const d = document.implementation.createHTMLDocument("t");
  d.body.innerHTML = html;
  return d;
}

describe("elementLabel", () => {
  it("renders tag + first class or id", () => {
    const d = frag(`<button class="cta primary">Go</button>`);
    expect(elementLabel(d.querySelector("button")!)).toBe("button.cta");
    const d2 = frag(`<section id="hero"></section>`);
    expect(elementLabel(d2.querySelector("section")!)).toBe("section#hero");
  });
});

describe("resolveClickTarget", () => {
  it("returns the element itself for a normal element hit", () => {
    const d = frag(`<button class="cta">Go</button>`);
    const btn = d.querySelector("button")!;
    expect(resolveClickTarget(btn).el).toBe(btn);
    expect(resolveClickTarget(btn).target).toBe("element");
  });
  it("classifies a region container as a region target", () => {
    const d = frag(`<section class="card"><p>hi</p></section>`);
    const section = d.querySelector("section")!;
    const r = resolveClickTarget(section);
    expect(r.target).toBe("region");
    expect(r.el).toBe(section);
  });
});
