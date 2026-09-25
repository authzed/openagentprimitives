import * as React from "react";
import { Area, AreaChart, Bar, BarChart, CartesianGrid, Line, LineChart, XAxis, YAxis } from "recharts";
import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
  ChartContainer,
  ChartTooltip,
  Input,
  Label,
  Markdown,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
  seriesColor,
} from "@ap/design";
// House style: design-package components import cn via the package-qualified
// "@ap/design/lib/utils" subpath rather than a relative path (see button.tsx,
// badge.tsx, …) — kept here for the same reason: it resolves through the same
// tsconfig/vite path alias without going through the barrel export.
import { cn } from "@ap/design/lib/utils";
import { actionCaption, isActionPending, useActions, type ActionState } from "./actions";
import { agentLinkHref, AgentLink } from "./agentlink";
import { AttachmentCard } from "./attachment";
import { ChatNode } from "./chat";
import { Disclosure } from "./disclosure";
import { GenerativeHook } from "./hooks";
import { NoticeCard } from "./notice";
import { PageNode } from "./page";
import { usePageLayout } from "./pageLayout";
import { useBindingParams } from "./params";
import { paramKey } from "./paramSpecs";
import { ProgressCard } from "./progress";
import { p, s } from "./props";
import { QuestionCard } from "./question";
import { sessionRefSegments } from "./sessionRef";
import { StatusLine } from "./status";
import type { Node } from "./types";

// Renderer resolves ONE vocabulary type. renderChild renders a single child
// node given an explicit key, and is passed in rather than imported, so
// registry.tsx and renderNode.tsx do not import each other — the recursion
// (walking n.children) lives in exactly one place: renderNode.tsx.
export type Renderer = (n: Node, renderChild: (n: Node, key: React.Key) => React.ReactElement) => React.ReactElement;

// p/s (the prop readers every entry below uses) live in props.ts, not here —
// question.tsx and progress.tsx need the identical string-coercion behavior
// (a Bindable prop like ap:progress's `now` can arrive as a number from a live
// source) and cannot import THIS module without a cycle, since this module is
// what imports them to register their renderers.

// pickVariant narrows an agent-declared string prop to one of a component's
// real cva variants, falling back rather than passing the raw string through.
// A cva `variant` prop is not a free-form className: an unrecognized value
// matches none of cva's `variants.variant` cases AND suppresses
// `defaultVariants` (because the prop is present, just unmatched), so an
// agent typo or an unsupported value silently renders an uncolored,
// unstyled component instead of falling back to "default".
function pickVariant<T extends string>(value: string, allowed: readonly T[], fallback: T): T {
  return (allowed as readonly string[]).includes(value) ? (value as T) : fallback;
}

const BADGE_VARIANTS = ["default", "secondary", "destructive", "outline"] as const;
const BUTTON_VARIANTS = ["default", "secondary", "destructive", "outline", "ghost", "link"] as const;

const GAP: Record<string, string> = { none: "gap-0", sm: "gap-2", md: "gap-4", lg: "gap-6" };
const ALIGN: Record<string, string> = {
  start: "items-start",
  center: "items-center",
  end: "items-end",
  stretch: "items-stretch",
};

// fieldID namespaces an ap:form field's DOM id. FormField.Name is
// agent-authored and lands in a document-global id space shared with the
// platform chrome the renderer is mounted inside — a field named "search" or
// "session" would otherwise mint a duplicate id, and a duplicate id makes
// label-for targeting ambiguous: clicking the shell's label can focus the
// agent's input, or vice versa. `name` (the form-data key) stays raw; only the
// id is prefixed.
const fieldID = (name: string): string => `apui-${name}`;

// headAlign/cellAlign map TableColumn.Align to a Tailwind class. Split in two
// because a right-aligned data cell also wants tabular-nums (it holds a
// number), but a right-aligned header cell — plain label text — does not.
const headAlign = (align?: string): string | undefined =>
  align === "right" ? "text-right" : align === "center" ? "text-center" : undefined;
const cellAlign = (align?: string): string | undefined =>
  align === "right" ? "text-right tabular-nums" : align === "center" ? "text-center" : undefined;

// ParamSelect is ap:select's live renderer: a control is "live" exactly when
// the declaration gave it a `param` name to drive (SelectProps.Param,
// pkg/web/uicomponents' registered ParamProp for this type) — a select with no
// param prop stays disabled below, same as the old blanket-disabled
// rendering, because a control that LOOKS live but drives nothing is worse
// than one that reads as not-yet-wired. It is its own component (rather than
// inlined in the COMPONENTS map like every other entry) because it is the
// first renderer in this file that needs a hook: useBindingParams reads
// context, and COMPONENTS' functions are called directly by renderNode, not
// by React, so calling a hook from one of them would break the rules of
// hooks. Wrapping it in a real component element is what makes the hook call
// legal.
function ParamSelect({ n }: { n: Node }) {
  const { params, setParam } = useBindingParams();
  const param = s(n, "param");
  const live = param !== "";
  const declaredValue = s(n, "value");
  const value = live ? (params[param] ?? declaredValue) : declaredValue;
  const placeholder = s(n, "placeholder");
  return (
    <div className="flex flex-col gap-1">
      <select
        disabled={!live}
        className={cn(
          "h-9 rounded-md border border-input bg-transparent px-3 text-sm",
          !live && "text-muted-foreground",
        )}
        value={value}
        onChange={live ? (e) => setParam(param, e.target.value) : undefined}
      >
        {placeholder && (
          <option value="" disabled hidden>
            {placeholder}
          </option>
        )}
        {((p(n).options as { value: string; label?: string }[]) ?? []).map((o) => (
          <option key={o.value} value={o.value}>
            {o.label ?? o.value}
          </option>
        ))}
      </select>
    </div>
  );
}

// dateParamValue converts a native <input type="date"> value into the RFC3339
// instant this parameter is CONTRACTED to carry.
//
// DateRangeProps.From/To are documented RFC3339 (pkg/web/uicomponents), and the
// tool args they feed are consumed as instants — an MCPServer's CEL trust
// constraint calls timestamp(f.value) on them. But a date input's value is
// date-only ("2026-08-03"), and CEL's timestamp() rejects that outright.
//
// The result was a type-conversion ERROR inside the validator, which fails
// closed, which surfaces to the viewer as "you do not have access to this
// data" — an authorization sentence for a format mismatch. Every date the
// picker produced was refused, and nothing on the page hinted at the cause.
//
// Midnight UTC, and only for a bare date: the picker has no time or zone to
// offer, so any instant chosen here is a convention rather than information,
// and midnight is the one a reader of "2026-08-03" already assumes. A value
// that already carries a time is passed through untouched — a declaration is
// free to seed a real instant, and re-normalizing one would silently move it.
function dateParamValue(v: string): string {
  if (v === "" || v.includes("T")) return v;
  return `${v}T00:00:00Z`;
}

// dateInputValue is the inverse, for display only: <input type="date"> shows
// nothing at all unless its value is exactly YYYY-MM-DD, so an RFC3339
// parameter has to be trimmed back before it reaches the control. Without
// this, a viewer returning to a page with a date in its URL would see an
// EMPTY picker over data that was in fact filtered — the two halves of the
// screen disagreeing about what is being shown.
function dateInputValue(v: string): string {
  const t = v.indexOf("T");
  return t === -1 ? v : v.slice(0, t);
}

// ParamDateRange is ap:daterange's live renderer — same live/inert split as
// ParamSelect, but DateRangeProps.Param drives TWO parameter keys
// (`<param>.from`/`<param>.to`, params.tsx's collectDefaultParams mirrors the
// same split) rather than one, because a date range is two independent
// values a viewer can each change. Inert stays the pre-existing disabled
// button rendering — a real date picker is more UI investment than an inert
// placeholder warrants, and native date inputs are enough to prove the
// parameter actually drives a re-evaluation.
function ParamDateRange({ n }: { n: Node }) {
  const { params, setParam } = useBindingParams();
  const param = s(n, "param");
  const live = param !== "";
  if (!live) {
    return (
      <Button variant="secondary" disabled>
        {s(n, "from") || "start"} → {s(n, "to") || "end"}
      </Button>
    );
  }
  const fromKey = paramKey(param, "from");
  const toKey = paramKey(param, "to");
  const from = params[fromKey] ?? s(n, "from");
  const to = params[toKey] ?? s(n, "to");
  return (
    <div className="flex items-center gap-2">
      <Input
        type="date"
        aria-label="From date"
        value={dateInputValue(from)}
        onChange={(e) => setParam(fromKey, dateParamValue(e.target.value))}
        className="h-9 w-auto"
      />
      <span className="text-muted-foreground" aria-hidden="true">
        →
      </span>
      <Input
        type="date"
        aria-label="To date"
        value={dateInputValue(to)}
        onChange={(e) => setParam(toKey, dateParamValue(e.target.value))}
        className="h-9 w-auto"
      />
    </div>
  );
}

// ActionStatus is the status line ActionButton/ActionForm render alongside
// themselves. Split out as its own tiny component rather than inlined twice
// because both controls need the identical caption-or-nothing rendering, and
// data-testid="agent-ui-action-status" is the one place a caller (this
// package's own tests, and Task 11's later live-approval surface) can find
// it without coupling to either control's markup.
function ActionStatus({ text }: { text: string | null }) {
  if (!text) return null;
  return (
    <span data-testid="agent-ui-action-status" className="text-xs text-muted-foreground">
      {text}
    </span>
  );
}

// ActionButton is ap:button's live renderer: a control is "live" exactly
// when the declaration gave it an `action` name to fire — a button with no
// action prop stays disabled, the same argument ParamSelect makes for `param`
// just scoped to actions. It is its own component (not inlined in COMPONENTS)
// because it calls useActions(), a hook — see ParamSelect's comment above for
// why COMPONENTS' entries, called directly by renderNode rather than by
// React, cannot do that themselves.
//
// Each control looks up its OWN action's state by name (states[action]) —
// never any other entry in the map — so a failed or pending action degrades
// exactly the one control that fired it. A sibling control bound to a
// different action name is untouched, the same "one failed section, never
// the page" rule bindings.ts's applyBindings already follows for the read
// half (see its own doc comment).
// DataTable is ap:table's renderer. It is its own component rather than an
// inline COMPONENTS entry for the same reason ActionButton and ParamSelect
// are: a table with a per-row action calls useActions(), a hook, and
// COMPONENTS' entries are called directly by renderNode rather than by React.
//
// rowAction turns each row into something a viewer can act on — the thing a
// table of records almost always wants and had no way to express. The row's
// own values become the action's inputs, so a control can say "tell me about
// this one" without the declaration having to name which one in advance.
//
// Values are stringified on the way out because that is what an action's
// inputs are (a flat string map, filtered server-side against the action's own
// Inputs allowlist). A column value that is an object is dropped rather than
// sent as "[object Object]": it is a selector pointed at the wrong place, and
// shipping the mistake into a sentence the agent reads makes it harder to see,
// not easier.
function DataTable({
  n,
  columns,
  rows,
}: {
  n: Node;
  columns: { key: string; header?: string; align?: string }[];
  rows: Record<string, unknown>[];
}) {
  const { states, invoke } = useActions();
  const actionName = s(n, "rowAction");
  const live = actionName !== "";
  const state: ActionState = live ? (states[actionName] ?? { phase: "idle" }) : { phase: "idle" };
  const pending = isActionPending(state.phase);

  if (rows.length === 0) {
    return <p className="py-6 text-center text-sm text-muted-foreground">{s(n, "empty", "Nothing to show.")}</p>;
  }
  return (
    <div className="overflow-x-auto">
      <Table>
        <TableHeader>
          <TableRow>
            {columns.map((c) => (
              <TableHead key={c.key} className={headAlign(c.align)}>
                {c.header ?? c.key}
              </TableHead>
            ))}
            {/* An unlabelled trailing header, not one reading "Actions": the
                buttons below say what they do, and a column title repeating it
                is noise in a header row that is otherwise all data. */}
            {live && <TableHead className="w-px" />}
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((row, i) => (
            <TableRow key={i}>
              {columns.map((c) => (
                <TableCell key={c.key} className={cellAlign(c.align)}>
                  {String(row[c.key] ?? "")}
                </TableCell>
              ))}
              {live && (
                <TableCell className="text-right">
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={pending}
                    data-testid="agent-ui-row-action"
                    onClick={() => invoke(actionName, rowInputs(row))}
                  >
                    {s(n, "rowActionLabel", "Details")}
                  </Button>
                </TableCell>
              )}
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <ActionStatus text={live ? (state.message ?? null) : null} />
    </div>
  );
}

// requireArray is the eager shape check a registry entry runs so a component
// never has to. An absent prop is an empty array (the ordinary "nothing
// declared" case); a present prop of the wrong shape THROWS, which is what
// renderNode's catch turns into a per-node failure card plus a console line
// naming the component.
//
// Throwing rather than coercing is deliberate: a `columns` that arrived as an
// object is a declaration mistake or a version skew, and silently rendering an
// empty table would hide it behind something that looks like an empty result.
function requireArray(v: unknown, what: string): unknown[] {
  if (v === undefined || v === null) return [];
  if (!Array.isArray(v)) throw new TypeError(`${what} must be an array`);
  return v;
}

// rowInputs flattens one row into the string map an action's inputs are.
// Objects, arrays and null/undefined are dropped — see DataTable's own comment
// for why a value no text field can honestly carry is better left out than
// stringified into a sentence.
function rowInputs(row: Record<string, unknown>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(row)) {
    if (typeof v === "string") out[k] = v;
    else if (typeof v === "number" && Number.isFinite(v)) out[k] = String(v);
    else if (typeof v === "boolean") out[k] = String(v);
  }
  return out;
}

function ActionButton({ n }: { n: Node }) {
  const { states, invoke, busy } = useActions();
  const action = s(n, "action");
  const live = action !== "";
  const state: ActionState = live ? (states[action] ?? { phase: "idle" }) : { phase: "idle" };
  // The OR with the declared/bound `disabled` prop matters because
  // ap:button.disabled is bindable (components.go) and applyBindings may
  // already have set it — a control that is only ever LIVE-disabled would
  // silently drop an author's own disabled binding. `busy` is the page's
  // "the agent is working" (see ActionsContextValue.busy): its own lifecycle
  // cannot see the agent's turn, this can.
  const disabled = !live || isActionPending(state.phase) || Boolean(busy) || Boolean(p(n).disabled);
  return (
    <div className="flex flex-col gap-1">
      <Button
        variant={pickVariant(s(n, "variant"), BUTTON_VARIANTS, "default")}
        disabled={disabled}
        onClick={live ? () => invoke(action, undefined) : undefined}
      >
        {s(n, "label")}
      </Button>
      <ActionStatus text={actionCaption(state)} />
    </div>
  );
}

// ActionForm is ap:form's live renderer, same live/inert split as
// ActionButton. Field values live in LOCAL component state — they are the
// action's Inputs, submitted only on click via invoke, never a binding
// parameter and never streamed anywhere mid-typing.
function ActionForm({ n }: { n: Node }) {
  const { states, invoke, busy } = useActions();
  const action = s(n, "action");
  const live = action !== "";
  const state: ActionState = live ? (states[action] ?? { phase: "idle" }) : { phase: "idle" };
  // Same four reasons as ActionButton, including the page's `busy`: a form
  // whose Prompt action just settled to "Asked the agent." must stay disabled
  // while the agent works on that very message.
  const disabled = !live || isActionPending(state.phase) || Boolean(busy) || Boolean(p(n).disabled);
  const fields = (p(n).fields as { name: string; label?: string; kind?: string; placeholder?: string }[]) ?? [];
  const initial = (p(n).values as Record<string, string> | undefined) ?? {};
  // `values` seeds the state ONCE, on mount: the lazy initializer runs on the
  // first render and never again, so a repaint that carries different
  // `values` does not overwrite what the person has typed into this very
  // form. That is the behavior an Edit loop needs — the agent repainting the
  // page around the form must not eat a half-typed answer. An agent that
  // genuinely must SHOW new text replaces the node (a different component or
  // a different position remounts the form), rather than expecting a new
  // `values` to land in a form already on screen.
  const [values, setValues] = React.useState<Record<string, string>>(() => ({ ...initial }));
  return (
    <form
      className="flex flex-col gap-3"
      onSubmit={(e) => {
        e.preventDefault();
        // Every DECLARED field is submitted, defaulting to "", rather than
        // only the ones the viewer typed into. `values` gains a key on the
        // first keystroke, so an untouched field would otherwise be absent
        // from the POST — and the action's args template still references it,
        // so SubstituteParams returns ErrUnknownParam and the viewer is told
        // "this view is missing a setting it needs; reload the page" for the
        // ordinary act of leaving a field blank.
        if (live) invoke(action, Object.fromEntries(fields.map((f) => [f.name, values[f.name] ?? ""])));
      }}
    >
      {fields.map((f) => (
        <div key={f.name} className="flex flex-col gap-1">
          <Label htmlFor={fieldID(f.name)}>{f.label ?? f.name}</Label>
          {f.kind === "textarea" ? (
            <textarea
              id={fieldID(f.name)}
              name={f.name}
              placeholder={f.placeholder}
              disabled={disabled}
              value={values[f.name] ?? ""}
              onChange={(e) => setValues((v) => ({ ...v, [f.name]: e.target.value }))}
              className="flex min-h-16 w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm shadow-sm placeholder:text-muted-foreground disabled:cursor-not-allowed disabled:opacity-50"
            />
          ) : (
            <Input
              id={fieldID(f.name)}
              name={f.name}
              type={f.kind === "number" ? "number" : "text"}
              placeholder={f.placeholder}
              disabled={disabled}
              value={values[f.name] ?? ""}
              onChange={(e) => setValues((v) => ({ ...v, [f.name]: e.target.value }))}
            />
          )}
        </div>
      ))}
      <Button type="submit" disabled={disabled}>
        {s(n, "submitLabel", "Submit")}
      </Button>
      <ActionStatus text={actionCaption(state)} />
    </form>
  );
}

// sessionViewSrc validates props.sessionRef before it ever becomes part of an
// iframe src, through the shared rule in sessionRef.ts (chatSrc is the same
// rule with a different prefix), and adds only /session-view and the
// per-segment encode.
function sessionViewSrc(ref: unknown): string | null {
  const segs = sessionRefSegments(ref);
  if (segs === null) return null;
  return `/session-view/${encodeURIComponent(segs[0])}/${encodeURIComponent(segs[1])}`;
}

// renderHorizontalSteps is ap:steps' original body, unchanged: an ordered run
// of named stages connected by a rule, with exactly one active — the ones
// before it done (filled, checked), the ones after upcoming (muted). It is
// what StepsNode draws under a column layout (this repo's default, and every
// layout before this one existed).
function renderHorizontalSteps(n: Node, steps: { label?: string; state?: string }[]): React.ReactElement {
  const list = (
    <ol className="flex w-full items-center" data-testid="agent-ui-steps">
      {steps.map((st, i) => {
        const state = st.state === "done" || st.state === "active" ? st.state : "upcoming";
        const isLast = i === steps.length - 1;
        return (
          <li key={i} className={cn("flex items-center", !isLast && "flex-1")} aria-current={state === "active" ? "step" : undefined}>
            <div className="flex flex-col items-center gap-1.5">
              <span
                className={cn(
                  "flex h-7 w-7 items-center justify-center rounded-full border text-xs font-semibold tabular-nums transition-colors",
                  state === "done" && "border-[hsl(var(--primary))] bg-[hsl(var(--primary))] text-[hsl(var(--primary-foreground))]",
                  state === "active" && "border-[hsl(var(--primary))] text-[hsl(var(--primary))] ring-2 ring-[hsl(var(--primary))]/30",
                  state === "upcoming" && "border-border text-muted-foreground",
                )}
              >
                {state === "done" ? "✓" : i + 1}
              </span>
              <span
                className={cn(
                  "whitespace-nowrap text-xs",
                  state === "active" ? "font-medium text-foreground" : "text-muted-foreground",
                )}
              >
                {st.label ?? ""}
              </span>
            </div>
            {!isLast && (
              <span
                className={cn(
                  "mx-2 mb-5 h-px flex-1 transition-colors",
                  state === "done" ? "bg-[hsl(var(--primary))]" : "bg-border",
                )}
                aria-hidden="true"
              />
            )}
          </li>
        );
      })}
    </ol>
  );
  // A pinned timeline stays in view while the page scrolls beneath it —
  // sticky inside the host's scroll container (the session shell's content
  // region), never over the platform chrome above it. Its own background
  // keeps scrolled content from showing through.
  if (!p(n).pinned) return list;
  return (
    <div data-testid="agent-ui-steps-pinned" className="sticky top-0 z-10 -mx-4 bg-background px-4 py-2">
      {list}
    </div>
  );
}

// StepsNode draws ap:steps: horizontal under a column layout (the existing
// timeline, pinned or not), vertical with summaries under a rail layout, where
// each step is also the control that selects it.
//
// steps arrives already validated by the "ap:steps" entry below — an ARRAY,
// every element of it an OBJECT — the same split ap:table's DataTable uses and
// for the same reason (see that entry's comment): StepsNode calls
// usePageLayout(), a hook, so it is mounted by React rather than called
// directly by renderNode, and a throw from inside a mounted component escapes
// renderNode's try/catch and unmounts the page.
function StepsNode({
  node: n,
  steps,
}: {
  node: Node;
  steps: { id?: string; label?: string; state?: string; summary?: string }[];
}): React.ReactElement {
  const { layout, selectedStep, selectStep } = usePageLayout();
  if (layout !== "rail") {
    return renderHorizontalSteps(n, steps); // the column layout's horizontal timeline
  }
  return (
    <ol className="flex flex-col" data-testid="agent-ui-steps" aria-label="Steps">
      {steps.map((st, i) => {
        const state = st.state === "done" || st.state === "active" ? st.state : "upcoming";
        const id = typeof st.id === "string" && st.id !== "" ? st.id : null;
        const selected = id !== null && id === selectedStep;
        return (
          <li key={i} data-testid="agent-ui-rail-step" data-step-id={id ?? undefined} data-selected={selected ? "true" : undefined} className="relative">
            <button
              type="button"
              onClick={() => id !== null && selectStep(id)}
              disabled={id === null}
              // Two different facts, and a screen-reader user needs both: which
              // phase the AGENT is in (aria-current), and which one the person is
              // looking at (aria-pressed, a toggle button, so it renders on every
              // step rather than only the pressed one).
              aria-current={state === "active" ? "step" : undefined}
              aria-pressed={selected}
              className={cn(
                "flex w-full items-start gap-2.5 rounded-md px-2 py-1.5 text-left transition-colors hover:bg-muted/40",
                selected && "bg-muted/60",
              )}
            >
              <span
                className={cn(
                  "mt-0.5 flex h-5 w-5 shrink-0 items-center justify-center rounded-full border text-[10px] font-semibold tabular-nums",
                  state === "done" && "border-[hsl(var(--primary))] bg-[hsl(var(--primary))] text-[hsl(var(--primary-foreground))]",
                  state === "active" && "border-[hsl(var(--primary))] text-[hsl(var(--primary))] ring-2 ring-[hsl(var(--primary))]/30",
                  state === "upcoming" && "border-border text-muted-foreground",
                )}
              >
                {state === "done" ? "✓" : i + 1}
              </span>
              <span className="min-w-0">
                <span className={cn("block text-sm", state === "active" ? "font-medium text-foreground" : state === "upcoming" ? "text-muted-foreground" : "text-foreground")}>
                  {st.label ?? ""}
                </span>
                {st.summary ? <span className="block truncate text-xs text-muted-foreground">{st.summary}</span> : null}
              </span>
            </button>
            {i < steps.length - 1 && <span className="ml-[18px] block h-2 border-l border-border" aria-hidden="true" />}
          </li>
        );
      })}
    </ol>
  );
}

// COMPONENTS maps a vocabulary type to its renderer. This is a MAP, never a
// switch: a new vocabulary type is a new entry here and a new registration in
// pkg/web/uicomponents, and no consumer changes. The same discipline the Go
// registry enforces on the server side.
export const COMPONENTS: Record<string, Renderer> = {
  "ap:stack": (n, renderChild) => (
    <div
      className={cn(
        "flex",
        s(n, "direction") === "horizontal" ? "flex-row" : "flex-col",
        GAP[s(n, "gap", "md")] ?? GAP.md,
        ALIGN[s(n, "align", "start")] ?? ALIGN.start,
      )}
    >
      {(n.children ?? []).map(renderChild)}
    </div>
  ),

  "ap:status": (n) => <StatusLine n={n} />,

  "ap:grid": (n, renderChild) => {
    const raw = typeof p(n).columns === "number" ? (p(n).columns as number) : 0;
    // Go documents 1..12, but validateProps is type-shaped only (it checks
    // "is this a number", not "is this in range") — an out-of-range value
    // reaches the browser as-is and would otherwise land directly in the
    // DOM as `repeat(100000, ...)`. ap:heading's level clamp is the existing
    // precedent for this in the same file; columns gets the same treatment.
    const columns = raw > 0 ? Math.min(Math.max(Math.trunc(raw), 1), 12) : 0;
    return (
      <div
        className={cn("grid", GAP[s(n, "gap", "md")] ?? GAP.md)}
        style={{
          gridTemplateColumns: columns > 0 ? `repeat(${columns}, minmax(0, 1fr))` : "repeat(auto-fit, minmax(16rem, 1fr))",
        }}
      >
        {(n.children ?? []).map(renderChild)}
      </div>
    );
  },

  "ap:card": (n, renderChild) => {
    const header = Boolean(s(n, "title") || s(n, "description"));
    return (
      <Card>
        {header && (
          <CardHeader>
            {s(n, "title") && <CardTitle>{s(n, "title")}</CardTitle>}
            {s(n, "description") && <CardDescription>{s(n, "description")}</CardDescription>}
          </CardHeader>
        )}
        {/* @ap/design's CardContent is `p-6 pt-0`: it assumes a header above
            it holds the top padding. An untitled card has no header, so its
            content sat flush on the card's own top border. */}
        <CardContent className={header ? undefined : "pt-6"}>{(n.children ?? []).map(renderChild)}</CardContent>
      </Card>
    );
  },

  "ap:collapsible": (n, renderChild) => (
    <Disclosure title={s(n, "title")} collapsed={Boolean(p(n).collapsed)} testId="ap-collapsible">
      {(n.children ?? []).map(renderChild)}
    </Disclosure>
  ),

  "ap:heading": (n) => {
    const level = Math.min(Math.max(Number(p(n).level) || 2, 1), 4);
    const Tag = `h${level}` as "h1" | "h2" | "h3" | "h4";
    const size = { 1: "text-2xl", 2: "text-xl", 3: "text-lg", 4: "text-base" }[level] ?? "text-xl";
    return <Tag className={cn(size, "font-semibold text-foreground")}>{s(n, "text")}</Tag>;
  },

  "ap:text": (n) => (
    <p
      className={cn(
        s(n, "variant") === "muted" && "text-muted-foreground",
        s(n, "variant") === "mono" && "font-mono",
        "text-sm",
      )}
    >
      {s(n, "text")}
    </p>
  ),

  // Agent prose goes through @ap/design's Markdown, which renders GFM with raw
  // HTML disabled and dangerous URL protocols stripped. That is why ap:markdown
  // — not ap:raw_html — is the intended home for anything an agent writes.
  "ap:markdown": (n) => <Markdown>{s(n, "body")}</Markdown>,

  "ap:metric": (n) => (
    <div className="flex flex-col gap-1">
      <span className="text-xs uppercase tracking-wide text-muted-foreground">{s(n, "label")}</span>
      <span className="text-2xl font-semibold tabular-nums">{s(n, "value")}</span>
      {s(n, "delta") && (
        <span
          className={cn(
            "text-xs tabular-nums",
            s(n, "trend") === "up" && "text-[hsl(var(--success))]",
            s(n, "trend") === "down" && "text-destructive",
            s(n, "trend") !== "up" && s(n, "trend") !== "down" && "text-muted-foreground",
          )}
        >
          {s(n, "delta")}
        </span>
      )}
      {s(n, "caption") && <span className="text-xs text-muted-foreground">{s(n, "caption")}</span>}
    </div>
  ),

  // ap:progress and ap:question are their own components (not inlined here)
  // for the same reason ActionButton/ParamSelect are: ap:question calls
  // useActions(), a hook, and COMPONENTS' entries are called directly by
  // renderNode rather than by React, so calling a hook from one of them would
  // break the rules of hooks.
  "ap:progress": (n) => <ProgressCard n={n} />,

  "ap:question": (n) => <QuestionCard n={n} />,

  "ap:notice": (n) => <NoticeCard n={n} />,

  // The shape is read HERE, in the eagerly-run registry entry, and handed to
  // DataTable already validated.
  //
  // That split is load-bearing. renderNode's try/catch — the thing that makes
  // one malformed node fail alone instead of taking the page — wraps this
  // call, and its own doc comment explains why a React error boundary cannot
  // stand in: these renderers run before React ever sees the element. A
  // component that read a malformed prop in its own body would throw during
  // React's render, past the catch, and unmount the tree. So anything that can
  // throw on a bad shape happens on this line, and DataTable only ever
  // receives arrays.
  "ap:table": (n) => {
    const columns = requireArray(p(n).columns, "ap:table.columns") as { key: string; header?: string; align?: string }[];
    const rows = requireArray(p(n).rows, "ap:table.rows") as Record<string, unknown>[];
    return <DataTable n={n} columns={columns} rows={rows} />;
  },

  "ap:badge": (n) => <Badge variant={pickVariant(s(n, "variant"), BADGE_VARIANTS, "default")}>{s(n, "text")}</Badge>,

  "ap:alert": (n) => {
    const severity = s(n, "severity");
    return (
      <Alert
        variant={severity === "error" ? "destructive" : "default"}
        // Alert itself only ships "default"/"destructive" cva variants; "warning"
        // rides on top of "default" with the design system's --warning token
        // rather than a third cva branch, so info/warning/error each read as
        // visually distinct instead of warning silently collapsing into info.
        className={severity === "warning" ? "border-warning/50 text-warning [&>svg]:text-warning" : undefined}
      >
        {s(n, "title") && <AlertTitle>{s(n, "title")}</AlertTitle>}
        {s(n, "body") && <AlertDescription>{s(n, "body")}</AlertDescription>}
      </Alert>
    );
  },

  "ap:empty": (n) => (
    <div className="flex flex-col items-center gap-1 py-10 text-center">
      <p className="text-sm font-medium">{s(n, "title", "Nothing here yet")}</p>
      {s(n, "body") && <p className="text-xs text-muted-foreground">{s(n, "body")}</p>}
    </div>
  ),

  // ap:steps is a progress TIMELINE: an ordered run of named stages, with
  // exactly one active — the ones before it done, the ones after upcoming.
  // The agent repaints `steps` via update_view as it advances, so the
  // indicator MOVES through the flow rather than reading as a static row of
  // tab pills. StepsNode (above) draws it horizontally under a column layout
  // (pinned or not) and as a vertical rail — each step its own selecting
  // control — under a rail layout; see page.tsx for what "rail" means.
  //
  // The array is validated HERE (requireArray), before React sees the
  // element, for the same reason ap:table's shape is — see StepsNode's own
  // comment. Its ELEMENTS are checked here too: a null step reads fine as an
  // array and then throws on `st.state` inside the mounted StepsNode, where
  // renderNode's catch cannot reach it and no error boundary stands between
  // it and the page — one malformed step would blank the whole view instead
  // of one section.
  "ap:steps": (n) => {
    const steps = requireArray(p(n).steps, "ap:steps.steps");
    steps.forEach((st, i) => {
      if (typeof st !== "object" || st === null) throw new TypeError(`ap:steps.steps[${i}] must be an object`);
    });
    return <StepsNode node={n} steps={steps as { id?: string; label?: string; state?: string; summary?: string }[]} />;
  },

  // ap:skeleton is the in-progress stand-in applyBindings substitutes for a
  // node whose data is still resolving — a first load, a re-resolve after the
  // viewer changed a filter, or a session being woken to answer.
  //
  // A shaped placeholder rather than a spinner: the bars occupy roughly the
  // space the real content will, so the section does not collapse and then
  // shove the rest of the page down when data lands. animate-pulse is the
  // motion, which is what makes "still working" legible at a glance — the
  // previous behaviour left the declared literal in place, so a re-resolving
  // table looked identical to a finished empty one and a filter change
  // appeared to do nothing.
  //
  // It is platform-owned and NOT part of the author-facing vocabulary in any
  // meaningful sense: applyBindings is the only thing that emits it. It lives
  // in the registry because that is where a Node's component name is resolved,
  // and going through the same table keeps the renderer's contract ("turn a
  // Node into React") unbroken.
  "ap:skeleton": (n) => (
    <div className="flex flex-col gap-2 py-4" data-testid="ap-skeleton" aria-busy="true" aria-live="polite">
      {s(n, "label") && <p className="text-xs text-muted-foreground">{s(n, "label")}</p>}
      <div className="h-4 w-1/3 animate-pulse rounded bg-muted" />
      <div className="h-4 w-full animate-pulse rounded bg-muted" />
      <div className="h-4 w-5/6 animate-pulse rounded bg-muted" />
      {/* Only when there is no label. The bars carry no text, so without this a
          non-visual viewer gets silence from a region that is visibly busy —
          but when a label IS present it is already in the accessibility tree,
          and repeating it here would both announce twice and make the copy
          ambiguous to match in a test. */}
      {!s(n, "label") && <span className="sr-only">Loading…</span>}
    </div>
  ),

  "ap:error": (n) => (
    <Alert variant="destructive">
      <AlertTitle>{s(n, "title", "Something went wrong")}</AlertTitle>
      {s(n, "body") && <AlertDescription>{s(n, "body")}</AlertDescription>}
    </Alert>
  ),

  "ap:chart": (n) => {
    const series = (p(n).series as { key: string; label?: string }[]) ?? [];
    const data = (p(n).data as Record<string, unknown>[]) ?? [];
    const xKey = s(n, "xKey");
    const kind = s(n, "kind", "line");
    if (data.length === 0 || series.length === 0) {
      return <p className="py-6 text-center text-sm text-muted-foreground">No data to chart.</p>;
    }
    const grid = <CartesianGrid stroke="hsl(var(--border))" strokeDasharray="3 3" vertical={false} />;
    const xAxis = <XAxis dataKey={xKey} stroke="hsl(var(--muted-foreground))" tickLine={false} axisLine={false} />;
    const yAxis = <YAxis stroke="hsl(var(--muted-foreground))" tickLine={false} axisLine={false} width={40} />;
    const tooltip = <ChartTooltip />;
    if (kind === "bar") {
      return (
        <ChartContainer>
          <BarChart data={data}>
            {grid}
            {xAxis}
            {yAxis}
            {tooltip}
            {series.map((sr, i) => (
              <Bar key={sr.key} dataKey={sr.key} name={sr.label ?? sr.key} fill={seriesColor(i)} />
            ))}
          </BarChart>
        </ChartContainer>
      );
    }
    if (kind === "area") {
      return (
        <ChartContainer>
          <AreaChart data={data}>
            {grid}
            {xAxis}
            {yAxis}
            {tooltip}
            {series.map((sr, i) => (
              <Area
                key={sr.key}
                type="monotone"
                dataKey={sr.key}
                name={sr.label ?? sr.key}
                // seriesColor already returns a fully-wrapped hsl(var(--chart-N))
                // string — do not wrap it again, that would produce hsl(hsl(...))
                // and Recharts would render an invisible shape with no error.
                stroke={seriesColor(i)}
                fill={seriesColor(i)}
                fillOpacity={0.25}
              />
            ))}
          </AreaChart>
        </ChartContainer>
      );
    }
    return (
      <ChartContainer>
        <LineChart data={data}>
          {grid}
          {xAxis}
          {yAxis}
          {tooltip}
          {series.map((sr, i) => (
            <Line
              key={sr.key}
              type="monotone"
              dataKey={sr.key}
              name={sr.label ?? sr.key}
              stroke={seriesColor(i)}
              strokeWidth={2}
              dot={false}
            />
          ))}
        </LineChart>
      </ChartContainer>
    );
  },

  "ap:chat": (n) => <ChatNode n={n} />,

  "ap:tabs": (n, renderChild) => {
    const tabs = (p(n).tabs as { value: string; label?: string }[]) ?? [];
    const children = n.children ?? [];
    if (tabs.length === 0) {
      // No declared tabs means there's no TabsList/TabsContent pairing to
      // build, but the agent may still have written children into this node.
      // Dropping them would be the exact "blank section reads as the agent
      // did nothing" failure UnknownComponent exists to avoid elsewhere —
      // render them unwrapped instead of losing them.
      return <>{children.map(renderChild)}</>;
    }
    return (
      <Tabs defaultValue={s(n, "value") || tabs[0].value}>
        <TabsList>
          {tabs.map((t) => (
            <TabsTrigger key={t.value} value={t.value}>
              {t.label ?? t.value}
            </TabsTrigger>
          ))}
        </TabsList>
        {tabs.map((t, i) => {
          // Children are paired to tabs by index. A child past the last tab
          // has no panel of its own — rather than silently dropping it (the
          // same failure as the empty-tabs case above), it lands in the last
          // declared panel alongside whatever was already paired to it.
          const isLast = i === tabs.length - 1;
          const pane = isLast ? children.slice(i) : children.slice(i, i + 1);
          return (
            <TabsContent key={t.value} value={t.value}>
              {pane.map((c, j) => renderChild(c, `${t.value}-${j}`))}
            </TabsContent>
          );
        })}
      </Tabs>
    );
  },

  // ap:raw_html is Tier 2. Rendering it needs the sandbox-origin serving path
  // (allow-scripts WITHOUT allow-same-origin, server-built CSP), which is Plan
  // 3's work. Until then it renders an explicit placeholder rather than falling
  // through to UnknownComponent: "not yet available here" is a true statement,
  // "unknown component" is not, and inlining the HTML would be the one thing
  // this whole design exists to prevent. props.html is deliberately never read
  // here — see renderNode.test.tsx's security regression test.
  "ap:raw_html": () => (
    <Alert>
      <AlertTitle>Embedded content</AlertTitle>
      <AlertDescription>This section renders once the sandboxed content view is available.</AlertDescription>
    </Alert>
  ),

  // ap:session_view embeds /session-view/{ns}/{name} — a same-origin,
  // first-party page that re-checks the viewer's own interact permission
  // itself, unlike ap:raw_html's sandbox-origin content. That is why this
  // iframe carries NO sandbox attribute: there is no separate grant to
  // enforce here, the framed page is the enforcement.
  "ap:session_view": (n) => {
    const src = sessionViewSrc(p(n).sessionRef);
    if (src === null) {
      // Mirrors ap:raw_html's inert placeholder — never an iframe with an
      // unvalidated/absolute src.
      return <Alert>Session view unavailable: no valid session reference.</Alert>;
    }
    return (
      <iframe
        src={src}
        title="Session view"
        style={{ width: "100%", height: "600px", border: "0" }}
      />
    );
  },

  // ap:agentlink is button-STYLED but not button-LIVE: unlike ActionButton
  // below, it fires no useActions() call and posts nothing — it is a plain
  // anchor whose href AgentLink builds entirely from namespace/agentClass/
  // prompt (agentlink.tsx), so it needs no hook.
  //
  // It DOES need the same eager-throw shape ap:table/ap:steps use
  // (requireArray, below): agentLinkHref throws on a namespace/agentClass
  // that is not DNS-1123-label-shaped, or an empty label, but AgentLink is a
  // separate function component that React (not this arrow function) would
  // invoke — deferred past renderNode's try/catch, and past the point where
  // a throw fails only this one node. Calling agentLinkHref here, before
  // AgentLink is ever handed to React, is what makes the failure eager.
  "ap:agentlink": (n) => {
    agentLinkHref(n);
    return <AgentLink n={n} />;
  },

  // ap:attachment is its own component (not inlined here) for the same
  // reason ap:question/ap:progress are: it calls useViewSession(), a hook,
  // and COMPONENTS' entries are called directly by renderNode rather than by
  // React.
  "ap:attachment": (n) => <AttachmentCard n={n} />,

  // Interactive components split by whether they have something to drive.
  // ap:select and ap:daterange are LIVE — ParamSelect/ParamDateRange above
  // call useBindingParams() and render enabled exactly when the declaration
  // names a `param` for them to drive. ap:button and ap:form are LIVE the
  // same way — ActionButton/ActionForm above call useActions() and render
  // enabled exactly when the declaration names an `action` for them to fire
  // (falling back to disabled otherwise: a control that looks live but does
  // nothing is worse than one that reads as not-yet-wired).
  "ap:button": (n) => <ActionButton n={n} />,

  "ap:select": (n) => <ParamSelect n={n} />,

  "ap:daterange": (n) => <ParamDateRange n={n} />,

  "ap:form": (n) => <ActionForm n={n} />,

  // A STRUCTURAL type (oap:page, below, is the other), and it resolves through
  // this same table rather than being special-cased in renderNode: a hook is a
  // container whose children are the author's default, and the region chrome
  // around them is GenerativeHook's (see hooks.tsx). Nothing here reads the
  // hook's intent or allowedComponents — both are an author-to-agent contract
  // the server enforces before a fill ever reaches the browser.
  //
  // `title` is the author's own header for the region. It is the fold's title
  // under a column layout, which the page's collapsed map already carries, and
  // a heading under a rail layout, which only the hook itself can draw — so it
  // is passed through here rather than read from the tree twice.
  "oap:generative": (n, renderChild) => (
    <GenerativeHook name={s(n, "name")} title={typeof n.props?.title === "string" ? n.props.title : undefined}>
      {(n.children ?? []).map(renderChild)}
    </GenerativeHook>
  ),

  // oap:page is the layout root; see page.tsx. The registry entry only hands
  // the node over — a renderer is a plain function and the page needs state.
  "oap:page": (n, renderChild) => <PageNode node={n} renderChild={renderChild} />,
};
