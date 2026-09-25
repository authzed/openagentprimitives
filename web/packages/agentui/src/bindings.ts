// bindings.ts applies SERVER-RESOLVED binding results to a declaration's
// Node tree. It never fetches anything itself — the browser never resolves a
// `tool:`/`memory:`/`artifact:` source (see pkg/web/webui/agentui/bindings.go,
// the only place a subject-scoped resolution happens); this module only
// knows how to lay a resolved value, or its absence, back onto the literal
// props a Node already declared.
import { hookNameOf } from "./hooks";
import { declaredParam } from "./paramSpecs";
import type { Binding, Node } from "./types";

export type BindingState =
  | { status: "loading" }
  | { status: "ok"; value: unknown }
  | { status: "error"; message: string }
  // needs_input is waiting on the viewer, not broken. It arrives when a
  // binding's args reference a parameter whose control has no default and the
  // viewer has not chosen yet — the ordinary state of a date range that must
  // be picked live. Rendered as ap:empty rather than ap:error: on first paint
  // nobody has done anything wrong, and a destructive card there reports a
  // fault that does not exist.
  | { status: "needs_input"; message: string }
  // waking: the session's runner had been reaped and a wake was requested.
  // The data is coming; this renders as a skeleton, not a card, because a
  // section that is about to work must not wear the treatment of one that has
  // failed.
  | { status: "waking"; message: string };

// bindingPath mirrors pkg/web/uicomponents.BindingPath (walk.go) EXACTLY:
// "<region>/<dotted node index path>#<prop>". The first segment is the REGION —
// the nearest enclosing hook's name, or "" for a node outside every hook — and
// the index path is measured from THAT region's root: the hook node, or the
// view root when the region is "". Inside a hook the numbering starts at the
// hook's own children, so a slot default compiled by the server's shim is
// "root/0#rows" and a node two levels under it is "root/0.2#rows"; a bound prop
// with no hook above it at all is "/0#text". An EMPTY index path names the
// region's root, which only the "" region ever produces ("/#rows", a bound view
// root) — a hook node itself declares no bindable prop.
//
// The mirror is pinned by ONE SHARED ARTIFACT read from both languages:
// pkg/web/webui/agentui/ui/testdata/bindings.golden.json holds the paths the Go
// walker emits for a fixture declaration, and
// pkg/web/webui/agentui/ui/bindingsGolden.test.tsx re-derives them through this
// function. A literal spelled out in each language's own test is NOT a pin —
// sweeping both together leaves both suites green while every binding in the
// shipped page silently sticks on its declared placeholder.
export function bindingPath(region: string, nodePath: number[], prop: string): string {
  return `${region}/${nodePath.join(".")}#${prop}`;
}

// applyBindings returns a NEW view tree whose bound props carry their resolved
// values, without mutating the input — it is pure and runs BEFORE renderNode,
// so renderNode's signature stays untouched (see renderChild's comment in
// renderNode.tsx for why growing renderNode's arity would be a hazard).
//
// Per state, each stand-in replacing the whole NODE so one binding costs one
// section and never the page: "ok" replaces the prop; "loading" (and a bound
// prop with no entry in `states` at all, the same "not resolved yet" fact)
// becomes ap:skeleton; "needs_input" becomes ap:empty; "error" becomes
// ap:error. All three stand-ins are already in the vocabulary with their own
// renderers (registry.tsx), so this introduces no new component type.
//
// "loading" used to leave the declared literal in place — the documented
// placeholder pattern, where an author writes `body: "loading..."` and that IS
// the loading UI. It did not survive contact: the literal an author actually
// writes is content (a table declares `empty: "No companies in this window."`),
// so a re-resolving section was indistinguishable from a finished empty one
// and a filter change appeared to do nothing.
//
// A CONTROL is the one exception, and it is not cosmetic. A node that drives a
// binding parameter (paramSpecs.ts's PARAM_SPECS — ap:select, ap:daterange) is
// the only thing on the page that can call setParam, and useBindings
// re-evaluates on a parameter change. Replacing it with an error card would
// remove the sole affordance capable of retrying: the parameter map could
// never change again, so no new request would ever be issued, and a reload
// would re-run the same failing source. So a failed control is rendered
// ALONGSIDE its error, still interactive, with its declared literal props
// intact — the page keeps a way out.
export function applyBindings(view: Node, states: Record<string, BindingState>): Node {
  // Seeding from the view's own root applies the SAME classification the child
  // walk applies to every other node: if the root IS a hook — a legal authored
  // page — everything under it, including a binding on the root node itself, is
  // in that hook's region. Without this the root would be a silent exception to
  // the region rule, since it is the one node never reached through a parent.
  // uicomponents.rootCursor is the Go half of exactly this.
  return applyNode(view, hookNameOf(view) ?? "", [], states);
}

function applyNode(n: Node, region: string, nodePath: number[], states: Record<string, BindingState>): Node {
  // The REGION rule, mirroring uicomponents.regionCursor: a child that is
  // itself a hook OPENS a new region rooted at it, so its own subtree is
  // numbered from zero again; any other child stays in this region, one level
  // deeper. The two languages must answer identically or every key the server
  // computes misses the browser's lookup and every bound prop sticks on its
  // declared placeholder, on a clean 200.
  const children = n.children?.map((child, i) => {
    const childName = hookNameOf(child);
    return applyNode(child, childName ?? region, childName !== null ? [] : [...nodePath, i], states);
  });

  if (!n.bindings) {
    return { ...n, children };
  }

  // Bound props are walked in a fixed (sorted) order so that, if more than
  // one prop on the same node is bound and more than one has failed, which
  // failure is reported is deterministic rather than an accident of object key
  // iteration order.
  const props: Record<string, unknown> = { ...n.props };
  // Three non-ok outcomes, tracked separately because they render differently
  // and their PRECEDENCE matters. A node with one binding genuinely broken and
  // another merely waiting is broken: reporting the gentler of the two would
  // hide a real fault behind a prompt. So: failure > loading > awaiting.
  let failure: string | null = null;
  let awaiting: string | null = null;
  let working: string | null = null;
  for (const prop of Object.keys(n.bindings as Record<string, Binding>).sort()) {
    const state = states[bindingPath(region, nodePath, prop)] ?? { status: "loading" };
    if (state.status === "error") {
      if (failure === null) failure = state.message;
      continue;
    }
    if (state.status === "needs_input") {
      if (awaiting === null) awaiting = state.message;
      continue;
    }
    if (state.status === "waking") {
      // Same treatment as loading, and deliberately so: from the section's
      // point of view they are one fact — the answer is coming. The message
      // differs (a wake takes seconds, not milliseconds) but the render must
      // not, or a page would show two kinds of "please wait" side by side
      // depending on which sections happened to be cached.
      if (working === null) working = state.message;
      continue;
    }
    if (state.status === "loading") {
      // A bound prop with no resolved value yet. Previously this left the
      // declared literal in place and the section simply sat there looking
      // finished — indistinguishable from "loaded, and the answer is empty" —
      // so a viewer who changed a filter saw nothing happen at all.
      if (working === null) working = "";
      continue;
    }
    props[prop] = state.value;
  }

  if (failure === null && awaiting === null && working === null) return { ...n, props, children };

  // One stand-in per outcome, all from components already in the vocabulary,
  // so this introduces no new type:
  //
  //   failure  -> ap:error     (destructive; something is wrong)
  //   loading  -> ap:skeleton  (in progress)
  //   awaiting -> ap:empty     (neutral; waiting on YOU)
  //
  // The split is the whole point. ap:error over an untouched filter announces
  // a fault nobody caused, and a silent placeholder over a re-resolving table
  // announces nothing at all.
  const stand: Node =
    failure !== null
      ? { component: "ap:error", props: { title: "Cannot load this section", body: failure } }
      : working !== null
        ? { component: "ap:skeleton", props: working === "" ? {} : { label: working } }
        : { component: "ap:empty", props: { title: "Nothing to show yet", body: awaiting } };
  const errorNode: Node = stand;
  // Not a control: the node is only a view of data that failed to arrive, so
  // the card stands in for it entirely.
  if (!declaredParam(n)) return errorNode;
  // A control: keep it usable and put the error next to it. ap:stack is
  // already in the vocabulary and already renders children, so this introduces
  // no new component type — and the resolved-and-successful props above are
  // still applied to the surviving control.
  return { component: "ap:stack", props: { gap: "sm" }, children: [{ ...n, props, children }, errorNode] };
}
