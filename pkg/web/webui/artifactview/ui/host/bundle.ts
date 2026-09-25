import type { Captured } from "./capture";

export interface Annotation extends Captured {
  index: number;
  comment: string;
  intent?: string;
  severity?: string;
  // Additional captured targets for a multi-component annotation — the primary
  // element is the flattened Captured; these are the extra elements the note
  // also covers. Each renders its own pin sharing this annotation's number.
  extra?: Captured[];
}

export interface Bundle {
  artifactId: string;
  annotations: Annotation[];
}

export class AnnotationStore {
  private items: Annotation[] = [];
  // Monotonic id source. NOT items.length+1: pins/popovers key on `index`, so an
  // id must be stable for an annotation's whole life and never reused — otherwise
  // a remove()+add() could mint an id that collides with a surviving pin.
  private seq = 0;

  add(c: Captured): Annotation {
    const a: Annotation = { ...c, index: ++this.seq, comment: "" };
    this.items.push(a);
    return a;
  }
  private byIndex(i: number): Annotation | undefined { return this.items.find((x) => x.index === i); }
  setComment(i: number, s: string): void { const a = this.byIndex(i); if (a) a.comment = s; }
  setChip(i: number, k: "intent" | "severity", v: string): void { const a = this.byIndex(i); if (a) a[k] = v || undefined; }
  addExtra(i: number, c: Captured): void { const a = this.byIndex(i); if (a) (a.extra ??= []).push(c); }
  // remove drops the annotation and leaves the rest untouched — ids stay stable,
  // so a pin labelled "3" is still "3" after "2" is deleted (no relabel needed).
  remove(i: number): void {
    this.items = this.items.filter((x) => x.index !== i);
  }
  list(): Annotation[] { return this.items; }
  count(): number { return this.items.length; }
  // filled counts annotations that carry a real note — a whitespace-only comment
  // doesn't count. Send is gated on filled() > 0.
  filled(): number { return this.items.filter((x) => x.comment.trim() !== "").length; }
  bundle(artifactId: string): Bundle {
    return { artifactId, annotations: this.items.map((a) => ({ ...a })) };
  }
}
