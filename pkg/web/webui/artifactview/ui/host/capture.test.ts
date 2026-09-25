import { describe, it, expect } from "vitest";
import { captureElement, cssPath } from "./capture";

function frag(html: string): Element {
  const d = document.implementation.createHTMLDocument("t");
  d.body.innerHTML = html;
  return d.body;
}

describe("cssPath", () => {
  it("builds a short nth-of-type path to the element", () => {
    const body = frag(`<main><section><button id="cta">Go</button></section></main>`);
    const btn = body.querySelector("#cta")!;
    // id short-circuits to a stable selector
    expect(cssPath(btn)).toBe("#cta");
  });
  it("falls back to nth-of-type when no id", () => {
    const body = frag(`<ul><li>a</li><li>b</li></ul>`);
    const li2 = body.querySelectorAll("li")[1];
    expect(cssPath(li2)).toContain("li:nth-of-type(2)");
  });
});

describe("captureElement", () => {
  it("captures identity, text, classes, and a11y for an element target", () => {
    const body = frag(`<button class="cta primary" aria-label="Sign up">Join now</button>`);
    const btn = body.querySelector("button")!;
    const c = captureElement(btn, "element", "");
    expect(c.target).toBe("element");
    expect(c.tagName).toBe("button");
    expect(c.cssClasses).toEqual(["cta", "primary"]);
    expect(c.elementText).toBe("Join now");
    expect(c.accessibility).toEqual({ role: "", label: "Sign up" });
  });
  it("records the highlighted phrase for a selection target", () => {
    const body = frag(`<p>the CTA padding is wrong here</p>`);
    const p = body.querySelector("p")!;
    const c = captureElement(p, "selection", "padding is wrong");
    expect(c.target).toBe("selection");
    expect(c.selectedText).toBe("padding is wrong");
  });
});
