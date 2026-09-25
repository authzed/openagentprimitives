import type { Annotation } from "./bundle";

// NotePopover is the per-annotation note editor, rendered in the HOST document
// (clean CSS, no clash with artifact styles). Class-driven only — the host page's
// CSP is style-src 'nonce' with no unsafe-inline, so inline style="" is stripped.
// It owns no store; it drives the annotator through the callbacks below.
export interface PopoverCallbacks {
  onComment: (index: number, value: string) => void;
  onChip: (index: number, kind: "intent" | "severity", value: string) => void;
  onDelete: (index: number) => void;
}

const INTENTS = ["change", "question", "bug", "praise"] as const;
const SEVERITIES = ["low", "med", "high"] as const;

// annotationLabel is a short human label: prefer the selected text, then the
// element's own text, then its path — trimmed so it stays one line. Shared with
// the annotations list in the toolbar.
export function annotationLabel(a: Annotation, cap = 44): string {
  const raw = (a.selectedText || a.elementText || a.elementPath || a.tagName).replace(/\s+/g, " ").trim();
  const short = raw.length > cap ? raw.slice(0, cap) + "…" : raw;
  return short && short.toLowerCase() !== a.tagName ? `${a.tagName} · ${short}` : a.tagName;
}

export class NotePopover {
  private el: HTMLElement | null = null;
  private idx: number | null = null;

  constructor(private hostDoc: Document, private cb: PopoverCallbacks) {}

  get isOpen(): boolean { return this.el !== null; }
  get openIndex(): number | null { return this.idx; }

  open(a: Annotation): void {
    this.close(); // only one open at a time
    this.idx = a.index;
    const d = this.hostDoc;

    const pop = d.createElement("div");
    pop.className = "ap-annot-pop";
    pop.setAttribute("data-ap-pop", String(a.index));

    const head = d.createElement("div");
    head.className = "ap-annot-pop-head";
    head.textContent = `#${a.index} · ${annotationLabel(a)}`;

    const hint = d.createElement("div");
    hint.className = "ap-annot-pop-hint";
    hint.textContent = "⌥-click more elements to add them to this note";

    const note = d.createElement("textarea");
    note.placeholder = "Add a note…";
    note.value = a.comment;
    note.addEventListener("input", () => this.cb.onComment(a.index, note.value));

    const actions = d.createElement("div");
    actions.className = "ap-annot-pop-actions";
    const del = d.createElement("button");
    del.className = "ap-annot-btn ap-annot-del";
    del.textContent = "Delete";
    del.onclick = () => this.cb.onDelete(a.index);
    const done = d.createElement("button");
    done.className = "ap-annot-btn ap-annot-done";
    done.textContent = "Done";
    done.onclick = () => this.close();
    actions.append(del, done);

    pop.append(head, hint, note, this.chips(a, "intent", INTENTS, a.intent), this.chips(a, "severity", SEVERITIES, a.severity), actions);
    (d.body || d.documentElement).appendChild(pop);
    this.el = pop;
    try { note.focus(); } catch { /* no browsing context (tests) — ignore */ }
  }

  // chips renders a single-select chip group. Clicking the pressed chip clears the
  // value (onChip with ""); clicking another switches to it.
  private chips(a: Annotation, kind: "intent" | "severity", opts: readonly string[], current?: string): HTMLElement {
    const d = this.hostDoc;
    const row = d.createElement("div");
    row.className = "ap-annot-chips";
    row.setAttribute("data-kind", kind);
    const lbl = d.createElement("span");
    lbl.className = "ap-annot-chips-label";
    lbl.textContent = kind;
    row.appendChild(lbl);
    for (const val of opts) {
      const chip = d.createElement("button");
      chip.className = "ap-annot-chip";
      chip.setAttribute("data-val", val);
      chip.textContent = val;
      chip.setAttribute("aria-pressed", current === val ? "true" : "false");
      chip.onclick = () => {
        const wasPressed = chip.getAttribute("aria-pressed") === "true";
        for (const c of row.querySelectorAll(".ap-annot-chip")) c.setAttribute("aria-pressed", "false");
        if (!wasPressed) chip.setAttribute("aria-pressed", "true");
        this.cb.onChip(a.index, kind, wasPressed ? "" : val);
      };
      row.appendChild(chip);
    }
    return row;
  }

  close(): void {
    if (this.el) {
      this.el.parentNode?.removeChild(this.el);
      this.el = null;
      this.idx = null;
    }
  }
}
