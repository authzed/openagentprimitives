import { describe, it, expect } from "vitest";
import { buildToolbar } from "./toolbar";
import { Annotator } from "./annotator";

function fixture() {
  const host = document.implementation.createHTMLDocument("host");
  const inner = document.implementation.createHTMLDocument("inner");
  inner.body.innerHTML = `<button id="cta">Go</button><button id="cta2">Stop</button>`;
  const a = new Annotator(host, inner, "", () => {});
  const bar = buildToolbar(host, a);
  return { host, inner, a, bar };
}

describe("buildToolbar", () => {
  it("styles the toolbar via CSS classes, never an inline style attribute", () => {
    // The host page's style-src is 'nonce-X' with no 'unsafe-inline'. A CSP
    // nonce authorizes <style> elements but NOT inline style="" attributes, so
    // the chrome MUST be class-driven — an inline style attr would be stripped.
    const { host, inner, a, bar } = fixture();
    expect(bar.className).toBe("ap-annot-toolbar");
    expect(bar.getAttribute("style")).toBeNull();
    // Populate a list row too, so we check inline styles on dynamic content.
    a.arm();
    a.handleClick(inner.querySelector("#cta")!);
    for (const el of bar.querySelectorAll("*")) {
      expect(el.getAttribute("style")).toBeNull();
    }
    expect(host.body.querySelector(".ap-annot-toolbar")).toBe(bar);
  });

  it("the toggle arms/disarms and reflects the armed state via aria-pressed + label", () => {
    const { a, bar } = fixture();
    const toggle = bar.querySelector<HTMLButtonElement>(".ap-annot-toggle")!;
    expect(a.armed).toBe(false);
    expect(toggle.getAttribute("aria-pressed")).toBe("false");
    toggle.click();
    expect(a.armed).toBe(true);
    expect(toggle.getAttribute("aria-pressed")).toBe("true");
    expect(toggle.textContent).toContain("Annotating");
    toggle.click();
    expect(a.armed).toBe(false);
    expect(toggle.getAttribute("aria-pressed")).toBe("false");
    expect(toggle.textContent).toContain("Annotate");
  });

  it("Send stays disabled until at least one annotation carries a note", () => {
    const { inner, a, bar } = fixture();
    const send = bar.querySelector<HTMLButtonElement>(".ap-annot-send")!;
    expect(send.disabled).toBe(true);
    a.arm();
    a.handleClick(inner.querySelector("#cta")!);
    expect(send.disabled).toBe(true); // pin exists, but no note yet
    a.setComment(1, "reword this");
    expect(send.disabled).toBe(false);
  });

  it("renders one list row per annotation; edit opens its note, remove drops it", () => {
    const { host, inner, a, bar } = fixture();
    a.arm();
    a.handleClick(inner.querySelector("#cta")!);
    a.handleClick(inner.querySelector("#cta2")!);
    expect(bar.querySelectorAll(".ap-annot-item").length).toBe(2);

    bar.querySelector<HTMLButtonElement>('.ap-annot-item[data-idx="1"] .ap-annot-edit')!.click();
    expect(host.querySelector<HTMLElement>(".ap-annot-pop")!.getAttribute("data-ap-pop")).toBe("1");

    bar.querySelector<HTMLButtonElement>('.ap-annot-item[data-idx="1"] .ap-annot-remove')!.click();
    expect(bar.querySelectorAll(".ap-annot-item").length).toBe(1);
    expect(a.count).toBe(1);
    expect(bar.querySelector(".ap-annot-item")!.getAttribute("data-idx")).toBe("2"); // stable id survives
  });

  it("starts collapsed to a trigger; clicking it opens the panel and it stays open until closed", () => {
    const { inner, a, bar } = fixture();
    const fab = bar.querySelector<HTMLButtonElement>(".ap-annot-fab")!;
    expect(fab).not.toBeNull();
    expect(bar.querySelector(".ap-annot-panel")).not.toBeNull();
    // collapsed by default — even after capturing an annotation (no auto-open)
    expect(bar.classList.contains("ap-annot-open")).toBe(false);
    a.arm();
    a.handleClick(inner.querySelector("#cta")!);
    expect(bar.classList.contains("ap-annot-open")).toBe(false);

    // the trigger opens it
    fab.click();
    expect(bar.classList.contains("ap-annot-open")).toBe(true);
    // and it STAYS open across further changes
    a.handleClick(inner.querySelector("#cta2")!);
    a.setComment(1, "note");
    expect(bar.classList.contains("ap-annot-open")).toBe(true);
    // clicking the trigger again closes it
    fab.click();
    expect(bar.classList.contains("ap-annot-open")).toBe(false);
  });

  it("the trigger shows the pending count as a badge while collapsed", () => {
    const { inner, a, bar } = fixture();
    const badge = bar.querySelector<HTMLElement>(".ap-annot-fab-count")!;
    expect(badge.hidden).toBe(true); // nothing pending
    a.arm();
    a.handleClick(inner.querySelector("#cta")!);
    a.handleClick(inner.querySelector("#cta2")!);
    expect(badge.hidden).toBe(false);
    expect(badge.textContent).toBe("2");
  });
});
