// params.tsx is the seam every live binding-parameter control (ap:select,
// ap:daterange — see registry.tsx's ParamSelect/ParamDateRange) reaches the
// page's current parameter map through, without registry.tsx or renderNode.tsx
// knowing anything about how that map is fetched, re-evaluated, or POSTed
// anywhere. The view (pkg/web/webui/agentui/ui/AgentUIView.tsx) owns the map and
// supplies it via BindingParamsProvider; a control just reads/writes through
// useBindingParams.
import * as React from "react";
import { declaredParam, paramKey } from "./paramSpecs";
import type { Declaration, Node } from "./types";

export interface BindingParams {
  params: Record<string, string>;
  setParam: (name: string, value: string) => void;
}

// The default value — {} params, a no-op setParam — is what
// `@ap/agentui` renders when used standalone with no provider (Plan 1's
// fixture tests exercise exactly that path, and this package has no other
// consumer that is guaranteed to wrap a provider around it). A control reads
// this as "no parameter is set yet", not as an error: it renders live and
// interactive, and its onChange calls a setParam that quietly does nothing,
// rather than the control failing to render at all.
const defaultBindingParams: BindingParams = { params: {}, setParam: () => {} };

const BindingParamsContext =
  React.createContext<BindingParams>(defaultBindingParams);

export function BindingParamsProvider({
  value,
  children,
}: {
  value: BindingParams;
  children: React.ReactNode;
}): React.ReactElement {
  return (
    <BindingParamsContext.Provider value={value}>
      {children}
    </BindingParamsContext.Provider>
  );
}

export function useBindingParams(): BindingParams {
  return React.useContext(BindingParamsContext);
}

// collectDefaultParams seeds the parameter map from the declaration's own
// declared control values, so first paint uses the author's defaults and a
// Tier-0 UI is functional with no interaction and no agent turn — the same
// "the browser never resolves a binding, but a viewer can still change WHICH
// readonly tool argument is asked about" contract bindings.go documents
// server-side (uicomponents.ParamNames is that same declaration's server-side
// mirror of this walk).
//
// Which controls declare a parameter, and which runtime keys each one drives,
// comes from paramSpecs.ts's PARAM_SPECS — the browser's mirror of
// pkg/web/uicomponents' Component.ParamProp/ParamValues. Resolving it through that
// table rather than switching on n.component is what makes a new
// parameterizing control one registration on each side, matching how the Go
// walk (uicomponents.ParamKeys) answers the identical question.
//
// Both walks descend through a hook exactly as through any other container: a
// control the AGENT wrote into a region is still a control the page must seed,
// and the walk has no business asking who authored a node. The server's
// ParamNames walks the identical tree.
//
// Only a LITERAL default is read. A control's value prop is deliberately not
// bindable on the Go side (see components.go): a server-resolved value would
// leave the control displaying one thing while this map — the only thing the
// server is told — said nothing, so display and resolution would diverge with
// no signal.
export function collectDefaultParams(
  decl: Declaration,
): Record<string, string> {
  const params: Record<string, string> = {};
  // A declaration with no view is a session that has not produced one yet, not
  // an error: the view renders its own "nothing declared" state and the
  // parameter map is simply empty. Throwing here would blank the page instead.
  if (decl.view) collectFromNode(decl.view, params);
  return params;
}

// reconcileParams merges a NEW declaration's own declared parameters with an
// EXISTING parameter map — the seam a live view update reaches through
// (AgentUIView's onView, fired when a `view` frame arrives over the live
// socket) so a rewritten slot's own default parameter values don't silently
// overwrite a viewer's current choice, and a parameter the new declaration no
// longer declares doesn't linger forever in a request it can no longer
// affect:
//
//   - a key the new declaration still declares AND prev already holds: prev's
//     value wins — the viewer's own choice survives an unrelated slot's
//     rewrite (an agent turn must not silently reset the dashboard).
//   - a key the new declaration declares for the FIRST time: seeded from the
//     new declaration's own literal default (collectDefaultParams).
//   - a key prev holds that the new declaration no longer declares: dropped.
//
// Built on the SAME collectFromNode walk collectDefaultParams uses, not a
// second hand-rolled walk — a second walk here would be free to drift from
// what the server (uicomponents.ParamNames) considers "declared" for the
// identical declaration.
export function reconcileParams(
  decl: Declaration,
  prev: Record<string, string>,
): Record<string, string> {
  const defaults = collectDefaultParams(decl);
  const next: Record<string, string> = {};
  // Iterate DECLARED keys, not defaulted ones. These are different sets, and
  // conflating them silently discarded the viewer's choice on exactly the
  // controls that need it most.
  //
  // collectDefaultParams records a key only when the control carries a
  // non-empty literal default (collectFromNode's `value !== ""` guard). A
  // control that declares a parameter but ships NO default — an ap:daterange
  // whose window must be picked live, which is the shape an author is pushed
  // toward whenever a literal default would go stale — therefore contributes
  // no key at all. Reconciling over that set made every such parameter look
  // like one "the new declaration no longer declares", so the third rule above
  // dropped the viewer's own pick on every `view` frame: the agent writes one
  // slot, and the viewer's date range silently resets to empty while the
  // bindings below fall back to "choose a value for this view's settings".
  for (const key of collectDeclaredParamKeys(decl)) {
    if (key in prev) {
      next[key] = prev[key];
    } else if (key in defaults) {
      next[key] = defaults[key];
    }
    // Declared but neither chosen nor defaulted: deliberately ABSENT rather
    // than "". The server distinguishes the two — an unset parameter is
    // ErrUnknownParam, which renders "choose a value for this view's
    // settings", while an empty string is a real value it would pass to the
    // tool. Seeding "" here would turn "nothing picked yet" into a query.
  }
  return next;
}

// collectDeclaredParamKeys answers which parameter keys a declaration DECLARES,
// independent of whether any of them carries a default value — the question
// reconcileParams actually needs, and the one pkg/web/uicomponents' ParamNames
// answers server-side.
//
// It walks through the same PARAM_SPECS table collectFromNode does, so a new
// parameterizing control stays one registration on each side; what it omits is
// that walk's value filter.
export function collectDeclaredParamKeys(decl: Declaration): string[] {
  const keys: string[] = [];
  const visit = (n: Node): void => {
    const declared = declaredParam(n);
    if (declared) {
      for (const v of declared.spec.values) {
        const key = paramKey(declared.name, v.suffix);
        if (!keys.includes(key)) keys.push(key);
      }
    }
    for (const child of n.children ?? []) visit(child);
  };
  if (decl.view) visit(decl.view);
  return keys;
}

function collectFromNode(n: Node, params: Record<string, string>): void {
  const declared = declaredParam(n);
  if (declared) {
    const props = n.props ?? {};
    for (const v of declared.spec.values) {
      const value = props[v.valueProp];
      if (typeof value === "string" && value !== "")
        params[paramKey(declared.name, v.suffix)] = value;
    }
  }
  for (const child of n.children ?? []) collectFromNode(child, params);
}
