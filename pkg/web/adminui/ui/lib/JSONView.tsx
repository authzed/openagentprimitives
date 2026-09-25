import * as React from "react";
import { cn } from "@ap/design";

// JSONView is a reusable, dependency-light JSON viewer. It pretty-prints a JSON
// value (object / array / primitive) as a readable, indented, collapsible tree,
// and offers a "raw" toggle that swaps the tree for the raw
// JSON.stringify(value, null, 2) text. Monospace + theme-tokened, compact. Used
// for tool input + output in the Full Logs viewer — `value` may be an object,
// an array, or a plain string; every shape renders.
export function JSONView({ value, className }: { value: unknown; className?: string }) {
  const [raw, setRaw] = React.useState(false);
  return (
    <div className={cn("rounded border border-border bg-background/40 text-xs", className)}>
      <div className="flex justify-end border-b border-border/50 px-1.5 py-0.5">
        <button
          type="button"
          onClick={() => setRaw((r) => !r)}
          className="font-mono text-[10px] uppercase tracking-wide text-muted-foreground hover:text-foreground"
          aria-pressed={raw}
        >
          {raw ? "tree" : "raw"}
        </button>
      </div>
      <div className="overflow-x-auto p-2 font-mono leading-relaxed">
        {raw ? (
          <pre className="whitespace-pre-wrap break-words text-foreground">{stringify(value)}</pre>
        ) : (
          <JSONNode value={value} />
        )}
      </div>
    </div>
  );
}

// stringify is JSON.stringify with a fallback for values it drops (undefined, a
// function) so the raw view never renders the literal string "undefined" from a
// missing serialization.
function stringify(value: unknown): string {
  const s = JSON.stringify(value, null, 2);
  return s === undefined ? String(value) : s;
}

// tryParseJSONContainer parses s as JSON when it is an object or array
// document (after trimming), returning undefined otherwise — the shared "is
// this renderable as a JSON tree?" gate for callers that receive free-form
// strings (tool output, transcript notes).
export function tryParseJSONContainer(s: string): unknown | undefined {
  const trimmed = s.trim();
  if (!trimmed.startsWith("{") && !trimmed.startsWith("[")) {
    return undefined;
  }
  try {
    return JSON.parse(trimmed);
  } catch {
    return undefined; // looked like JSON but isn't — caller renders as text
  }
}

// JSONNode recursively renders one value as its own row: primitives as
// "label: value", objects/arrays as a collapsible branch whose label lives in
// the toggle line.
function JSONNode({ value, label }: { value: unknown; label?: string }) {
  if (value === null || typeof value !== "object") {
    return (
      <div className="flex flex-wrap items-start gap-1.5">
        {label !== undefined && <span className="shrink-0 text-foreground/70">{label}:</span>}
        <Primitive value={value} />
      </div>
    );
  }
  return <Branch value={value as object} label={label} />;
}

// primitiveClass color-codes a primitive by JS type using theme tokens so the
// tree reads at a glance (strings vs numbers vs booleans vs null).
function primitiveClass(v: unknown): string {
  switch (typeof v) {
    case "string":
      return "text-success";
    case "number":
      return "text-primary";
    case "boolean":
      return "text-state";
    default:
      return "text-muted-foreground"; // null / undefined
  }
}

function Primitive({ value }: { value: unknown }) {
  const text = typeof value === "string" ? JSON.stringify(value) : String(value);
  return <span className={primitiveClass(value)}>{text}</span>;
}

// Branch renders an object or array as a collapsible block: a toggle line
// carrying the key label (when any) + the open bracket, the shallow-indented
// children (guttered), and the close bracket. Keeping the label inside the
// toggle line means children indent a fixed ~2 characters from the row start
// instead of hanging off the key's right edge. Empty containers collapse to a
// single "{}" / "[]".
function Branch({ value, label }: { value: object; label?: string }) {
  const [open, setOpen] = React.useState(true);
  const isArray = Array.isArray(value);
  const entries: [string, unknown][] = isArray
    ? (value as unknown[]).map((v, i) => [String(i), v])
    : Object.entries(value as Record<string, unknown>);
  const [ob, cb] = isArray ? ["[", "]"] : ["{", "}"];
  const keySpan = label !== undefined ? <span className="text-foreground/70">{label}: </span> : null;

  if (entries.length === 0) {
    return (
      <div>
        {keySpan}
        <span className="text-muted-foreground">
          {ob}
          {cb}
        </span>
      </div>
    );
  }

  return (
    <div>
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        className="text-muted-foreground hover:text-foreground"
      >
        {keySpan}
        <span aria-hidden="true">{open ? "▾" : "▸"}</span> {ob}
        {!open && (
          <span className="text-muted-foreground">
            {" "}
            …{entries.length}
            {" "}
            {cb}
          </span>
        )}
      </button>
      {open && (
        <>
          <div className="ml-2 border-l border-border/40 pl-2">
            {entries.map(([key, v]) => (
              <JSONNode key={key} value={v} label={isArray ? undefined : key} />
            ))}
          </div>
          <div className="text-muted-foreground">{cb}</div>
        </>
      )}
    </div>
  );
}
