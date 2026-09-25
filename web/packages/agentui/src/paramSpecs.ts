// paramSpecs.ts mirrors, for the browser, what pkg/web/uicomponents registers
// about a control that drives a binding parameter: Component.ParamProp — which
// prop carries the parameter NAME — and Component.ParamValues — which runtime
// KEYS that name expands to, and which prop holds each key's declared default.
//
// It is a MAP keyed by vocabulary type, never a switch. Go answers "which
// parameter does this node declare?" with registry.Get(n.Component).ParamProp
// precisely so that a new parameterizing control is a registration rather than
// an edit to every consumer; a `if (n.component === "ap:select")` chain over
// here would give that up on one side of the seam, and the consequence is
// concrete: register a third control in Go and its parameter name passes the
// server's ParamKeys check while the browser never seeds it, so every binding
// referencing it fails on a page nothing can fix.
//
// It lives in its own module rather than inside registry.tsx because
// registry.tsx already imports params.tsx (for useBindingParams) and both need
// this table — putting it in either would make the two import each other.
import type { Node } from "./types";

// ParamValue is one runtime parameter key a control drives, mirroring
// pkg/web/uicomponents/component.ParamValue.
export interface ParamValue {
  // suffix is appended to the declared parameter name, after a ".", to form
  // the runtime key. Absent means the declared name IS the key.
  suffix?: string;
  // valueProp names the prop carrying this key's declared default value.
  valueProp: string;
}

export interface ParamSpec {
  // prop is the prop carrying the parameter NAME (Go's Component.ParamProp).
  prop: string;
  values: ParamValue[];
}

export const PARAM_SPECS: Record<string, ParamSpec> = {
  "ap:select": { prop: "param", values: [{ valueProp: "value" }] },
  "ap:daterange": {
    prop: "param",
    values: [{ suffix: "from", valueProp: "from" }, { suffix: "to", valueProp: "to" }],
  },
};

// paramKey builds the runtime key a declared parameter name contributes for a
// given suffix — the one place the "<name>.<suffix>" convention is spelled on
// this side, so a renderer and the default collector cannot disagree about it.
export function paramKey(name: string, suffix?: string): string {
  return suffix ? `${name}.${suffix}` : name;
}

// declaredParam resolves the parameter name a node declares, through the table
// above. Returns null for a node whose component drives no parameter, or whose
// param prop is absent, empty, or not a string — the same three rejections
// pkg/web/uicomponents' declaredParam makes.
export function declaredParam(n: Node): { name: string; spec: ParamSpec } | null {
  const spec = PARAM_SPECS[n.component];
  if (!spec) return null;
  const name = (n.props ?? {})[spec.prop];
  if (typeof name !== "string" || name === "") return null;
  return { name, spec };
}
