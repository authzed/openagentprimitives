import { captureElement } from "./capture";
import { AnnotationStore, type Bundle } from "./bundle";
import { resolveClickTarget, selectionTarget, elementLabel } from "./targets";
import { NotePopover } from "./popover";

// Annotator owns the interactive capture over the inner artifact document. Pins
// are injected INTO innerDoc (they scroll with content); the toolbar, list, and
// note popover live in hostDoc (clean CSS, no clash with artifact styles). The
// DOM-event wiring (pointermove → hover, click) is attached by bootAnnotator; the
// methods below are the testable seam. UI chrome (toolbar/list) reads state via
// the getters and re-renders on subscribe() notifications.
export class Annotator {
  private store = new AnnotationStore();
  private _armed = false;
  // one annotation can have several pins (a multi-component annotation), so map
  // its id to a list of pin elements.
  private pins = new Map<number, Element[]>();
  // target element → annotation id, so a second click on the same element reopens
  // its note instead of stacking a duplicate pin.
  private targets = new Map<Element, number>();
  private outline: HTMLElement | null = null;
  private popover: NotePopover;
  private listeners: Array<() => void> = [];

  constructor(
    hostDoc: Document,
    private innerDoc: Document,
    private artifactId: string,
    private onSend: (b: Bundle) => void,
  ) {
    this.popover = new NotePopover(hostDoc, {
      onComment: (i, v) => { this.store.setComment(i, v); this.notify(); },
      onChip: (i, k, v) => { this.store.setChip(i, k, v); this.notify(); },
      onDelete: (i) => this.remove(i),
    });
  }

  get armed(): boolean { return this._armed; }
  get count(): number { return this.store.count(); }
  filledCount(): number { return this.store.filled(); }
  list() { return this.store.list(); }

  // subscribe registers a listener fired whenever the annotation set or armed
  // state changes, so host-doc chrome (toolbar/list) can re-render itself.
  subscribe(fn: () => void): void { this.listeners.push(fn); }
  private notify(): void { for (const fn of this.listeners) fn(); }

  arm(): void { this._armed = true; this.notify(); }
  disarm(): void { this._armed = false; this.clearOutline(); this.notify(); }

  // hover paints the red highlight over the element under the cursor. Owned here
  // (not in the event wiring) so disarm() can tear it down — otherwise the last
  // highlight stays stuck on the document after leaving annotate mode.
  hover(el: Element): void {
    if (!this._armed || !el || el.nodeType !== 1) return;
    if (el.closest?.("[data-ap-pin]")) return; // don't outline our own pins
    const inner = this.innerDoc;
    if (!this.outline) {
      this.outline = inner.createElement("div");
      this.outline.setAttribute("data-ap-outline", "");
      this.outline.setAttribute("style", "position:absolute;z-index:2147483646;pointer-events:none;outline:2px solid #e11d48;background:rgba(225,29,72,.08)");
      (inner.body || inner.documentElement).appendChild(this.outline);
    }
    const r = el.getBoundingClientRect();
    const sx = inner.defaultView?.scrollX || 0, sy = inner.defaultView?.scrollY || 0;
    this.outline.style.left = (r.left + sx) + "px"; this.outline.style.top = (r.top + sy) + "px";
    this.outline.style.width = r.width + "px"; this.outline.style.height = r.height + "px";
    this.outline.title = elementLabel(el);
  }

  clearOutline(): void {
    if (this.outline) { this.outline.parentNode?.removeChild(this.outline); this.outline = null; }
  }

  handleClick(el: Element): void {
    if (!this._armed) return;
    // Click on an existing pin → reopen that note (don't annotate the pin itself).
    const pinEl = el.closest?.("[data-ap-pin]");
    if (pinEl) { this.openPopoverFor(Number(pinEl.getAttribute("data-ap-pin"))); return; }
    const { el: targetEl, target } = resolveClickTarget(el);
    // Already annotated this element → reopen instead of duplicating.
    const existing = this.targets.get(targetEl);
    if (existing != null) { this.openPopoverFor(existing); return; }
    const a = this.store.add(captureElement(targetEl, target, ""));
    this.targets.set(targetEl, a.index);
    this.injectPin(a.index, targetEl);
    this.openPopoverFor(a.index);
    this.notify();
  }

  addSelection(): void {
    if (!this._armed) return;
    const s = selectionTarget(this.innerDoc);
    if (!s) return;
    const a = this.store.add(captureElement(s.el, "selection", s.selectedText));
    this.injectPin(a.index, s.el);
    this.openPopoverFor(a.index);
    this.notify();
  }

  // addComponent attaches the clicked element to the currently-open annotation as
  // an extra target (multi-component). It drops another pin sharing that
  // annotation's number under the same note. With nothing open it just starts a
  // new annotation, so a modifier-click is always meaningful.
  addComponent(el: Element): void {
    if (!this._armed) return;
    const active = this.popover.openIndex;
    if (active == null) { this.handleClick(el); return; }
    const { el: targetEl, target } = resolveClickTarget(el);
    if (this.targets.get(targetEl) != null) return; // already pinned somewhere
    this.store.addExtra(active, captureElement(targetEl, target, ""));
    this.targets.set(targetEl, active);
    this.injectPin(active, targetEl);
    this.notify();
  }

  openPopoverFor(index: number): void {
    const a = this.store.list().find((x) => x.index === index);
    if (a) this.popover.open(a);
  }

  // reopenPin handles a click on an existing pin — including while disarmed.
  // A pin is our own unambiguous UI (it never overlaps normal reading of the
  // static artifact), so clicking one re-enters annotate mode and reopens its
  // note rather than doing nothing.
  reopenPin(index: number): void {
    if (!this._armed) this.arm();
    this.openPopoverFor(index);
  }

  remove(index: number): void {
    this.store.remove(index);
    for (const pin of this.pins.get(index) ?? []) pin.parentNode?.removeChild(pin);
    this.pins.delete(index);
    for (const [el, i] of this.targets) if (i === index) this.targets.delete(el);
    if (this.popover.openIndex === index) this.popover.close();
    this.notify();
  }

  private injectPin(index: number, el: Element): void {
    const pin = this.innerDoc.createElement("div");
    pin.setAttribute("data-ap-pin", String(index));
    pin.textContent = String(index);
    // minimal inline styling — the artifact's CSS must not need to cooperate
    pin.setAttribute("style",
      "position:absolute;z-index:2147483647;min-width:18px;height:18px;line-height:18px;" +
      "padding:0 4px;border-radius:9px;background:#e11d48;color:#fff;font:12px/18px sans-serif;" +
      "text-align:center;box-shadow:0 1px 3px rgba(0,0,0,.4);cursor:pointer");
    // anchor at the element's top-left within the document
    const rect = el.getBoundingClientRect ? el.getBoundingClientRect() : ({ left: 0, top: 0 } as DOMRect);
    const sx = (this.innerDoc.defaultView?.scrollX) || 0;
    const sy = (this.innerDoc.defaultView?.scrollY) || 0;
    pin.style.left = (rect.left + sx) + "px";
    pin.style.top = (rect.top + sy) + "px";
    (this.innerDoc.body || this.innerDoc.documentElement).appendChild(pin);
    const arr = this.pins.get(index) ?? [];
    arr.push(pin);
    this.pins.set(index, arr);
  }

  setComment(index: number, s: string): void { this.store.setComment(index, s); this.notify(); }
  setChip(index: number, k: "intent" | "severity", v: string): void { this.store.setChip(index, k, v); this.notify(); }

  send(): void {
    // Only hand off when there's a real note — an empty capture isn't worth a turn.
    if (this.store.filled() === 0) return;
    this.onSend(this.store.bundle(this.artifactId));
    // clear pins + popover + store after a successful hand-off
    for (const arr of this.pins.values()) for (const pin of arr) pin.parentNode?.removeChild(pin);
    this.pins.clear();
    this.targets.clear();
    this.popover.close();
    this.store = new AnnotationStore();
    this.notify();
  }
}
