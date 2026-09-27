// tree.ts answers questions about the WHOLE declared page rather than about
// one node — the shape a view needs when a rendering decision depends on
// what else is on screen, not on the node currently being rendered.
//
// It imports hookNameOf from hooks.tsx (the one place "is this node a hook,
// and what region does it open" is answered) and must never be imported back
// from there: hooks.tsx renders one region at a time and has no reason to
// walk the whole tree, and a two-way import between the two would make an
// accidental cycle possible for no benefit.
import { hookNameOf } from "./hooks";
import type { Node } from "./types";

// treeContains answers "is this component anywhere on the page" — the whole
// rendered tree, author defaults included. The view derives its fallbacks
// from it (spec §7): a question on screen is a question on screen whoever
// wrote it, so the modal must not appear beside one.
export function treeContains(
  view: Node | undefined,
  component: string,
): boolean {
  if (!view) return false;
  if (view.component === component) return true;
  return (view.children ?? []).some((c) => treeContains(c, component));
}

// hooksContaining names the hooks whose subtree holds `component` — what the
// page marks "waiting on you" (a hook with an ap:question inside). Computed
// here from the tree, never claimed by a node about itself.
export function hooksContaining(
  view: Node | undefined,
  component: string,
): Set<string> {
  const out = new Set<string>();
  const walk = (n: Node, hook: string | null) => {
    const name = hookNameOf(n) ?? hook;
    if (n.component === component && name !== null) out.add(name);
    for (const c of n.children ?? []) walk(c, name);
  };
  if (view) walk(view, null);
  return out;
}

// questionOutsideHooks reports whether the page holds an ap:question that is
// NOT inside any oap:generative — an author's own question, in the fixed part
// of the declaration.
//
// It exists because "the page is still asking" is answered in two halves, and
// only one of them can be subtracted. A question inside a hook can be marked
// answered (the server derives that from the fill's write time against the
// transcript), so the view subtracts the answered hooks from
// hooksContaining(view, "ap:question"). A question outside every hook has no
// hook name to mark and nothing ever answers it on the viewer's behalf, so it
// counts as asking for exactly as long as it is declared — and the platform's
// reply modal must not open beside it.
//
// Walks like hooksContaining: the same "which hook encloses this node"
// descent, keeping the nodes it reaches with no enclosing hook rather than the
// ones it reaches with a name.
export function questionOutsideHooks(view: Node | undefined): boolean {
  const walk = (n: Node, hook: string | null): boolean => {
    const name = hookNameOf(n) ?? hook;
    if (n.component === "ap:question" && name === null) return true;
    return (n.children ?? []).some((c) => walk(c, name));
  };
  return view ? walk(view, null) : false;
}

// StepBinding is what a bound hook knows about its step: the id it named, the
// step's label (the fold's title) and the step's current state.
export interface StepBinding {
  step: string;
  label: string;
  state: "done" | "active" | "upcoming";
  // title is the hook's own fold header, when the author gave one.
  title?: string;
}

// TimelineStep is one step of the page's single ap:steps, as the page reads it.
export interface TimelineStep {
  id: string;
  label: string;
  state: "done" | "active" | "upcoming";
}

// timelineSteps returns the page's one ap:steps as a list — every step with an
// id, in order — or an empty list when the page has no single timeline. It is
// the PAGE's reading of that node: which step is selected by default, and
// (through stepBindings) which hook belongs to which step. The rail draws
// itself from the node's own props instead, so nothing here decides what a
// step looks like.
export function timelineSteps(view: Node | undefined): TimelineStep[] {
  if (!view) return [];
  const timelines: Node[] = [];
  const collect = (n: Node) => {
    if (n.component === "ap:steps") timelines.push(n);
    for (const c of n.children ?? []) collect(c);
  };
  collect(view);
  if (timelines.length !== 1) return [];
  const raw = timelines[0].props?.steps;
  const out: TimelineStep[] = [];
  for (const st of Array.isArray(raw)
    ? (raw as { id?: unknown; label?: unknown; state?: unknown }[])
    : []) {
    if (!st || typeof st.id !== "string" || st.id === "") continue;
    const state =
      st.state === "done" || st.state === "active" ? st.state : "upcoming";
    out.push({
      id: st.id,
      label: typeof st.label === "string" ? st.label : st.id,
      state,
    });
  }
  return out;
}

// stepBindings maps each hook bound to a timeline step (its `step` prop) to
// that step's current label and state, read from the page's ONE ap:steps —
// derived from the tree, never claimed by a node about itself. A page with no
// timeline or more than one yields nothing; admission refuses the latter, so
// this only ever answers for a page the server already accepted, and answers
// nothing rather than guessing when it sees something else.
export function stepBindings(view: Node | undefined): Map<string, StepBinding> {
  const out = new Map<string, StepBinding>();
  if (!view) return out;

  const byId = new Map<string, StepBinding>();
  for (const st of timelineSteps(view)) {
    byId.set(st.id, { step: st.id, label: st.label, state: st.state });
  }
  if (byId.size === 0) return out;

  const walk = (n: Node) => {
    const name = hookNameOf(n);
    const step = n.props?.step;
    if (name !== null && typeof step === "string") {
      const b = byId.get(step);
      if (b) {
        const title = n.props?.title;
        out.set(
          name,
          typeof title === "string" && title !== "" ? { ...b, title } : b,
        );
      }
    }
    for (const c of n.children ?? []) walk(c);
  };
  walk(view);
  return out;
}
