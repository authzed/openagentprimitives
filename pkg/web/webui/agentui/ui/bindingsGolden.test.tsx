import "@testing-library/jest-dom/vitest";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { bindingPath, hookNameOf } from "@ap/agentui";
import type { Declaration, Node } from "@ap/agentui";
// AgentUIView subscribes to the shell's session socket for its own fallbacks;
// every mount below renders the view ALONE, outside that provider, and this
// file is not about those signals. Stubbed to the idle value so the provider's
// own "no frames will arrive" report — correct, and exactly what a view with no
// socket must say — does not bury this file's real output.
vi.mock("../../chat/ui/sessionSignals", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../chat/ui/sessionSignals")>();
  return { ...actual, useSessionSignals: () => actual.INITIAL_SESSION_SIGNALS };
});

import { AgentUIView } from "./AgentUIView";
import golden from "./testdata/bindings.golden.json";

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

// testdata/bindings.golden.json is the ONE artifact both halves of the
// data-binding seam read. Its `paths` and `response` are produced by the REAL
// Go code (uicomponents.WalkBindings and the real bindingsHandler,
// bindings_golden_internal_test.go); this file re-derives the paths through
// @ap/agentui's bindingPath and parses that exact response through
// useBindings, so neither side can move alone.
//
// Before it existed, both seams were pinned only by two independent copies of
// the same literal — one in walk_test.go, one in bindings.test.tsx — and a
// reviewer proved by mutation that changing the Go side AND its own test
// literals together left both suites green while every binding in the shipped
// page silently stopped resolving.

const declaration = golden.declaration as Declaration;

// derivePaths mirrors uicomponents.WalkBindings' documented walk order: one
// page tree walked depth-first in document order, bound props sorted by name
// within a node. It is written out here rather than imported because the point
// is to re-derive the keys the browser will look up, using bindingPath, and
// compare them against what the Go producer emitted.
//
// The REGION rule is what this has to reproduce, and it is the half a
// per-language literal cannot pin: a child that is itself a hook OPENS a new
// region rooted at it, so its subtree is numbered from zero again, while any
// other child stays in the enclosing region one level deeper. The view root
// goes through the same classification (uicomponents.rootCursor), so a page
// whose root IS a hook is not a silent exception. Get this wrong in one
// language and every key the server computes misses the browser's lookup, on a
// clean 200, with both suites green.
function derivePaths(decl: Declaration): string[] {
  const out: string[] = [];
  const walk = (n: Node, region: string, path: number[]) => {
    for (const prop of Object.keys(n.bindings ?? {}).sort()) out.push(bindingPath(region, path, prop));
    (n.children ?? []).forEach((child, i) => {
      const childName = hookNameOf(child);
      walk(child, childName ?? region, childName !== null ? [] : [...path, i]);
    });
  };
  walk(decl.view, hookNameOf(decl.view) ?? "", []);
  return out;
}

const props = {
  ns: "demo-ns",
  name: "demo-session",
  declaration,
  shown: true,
} as const;

describe("the Go↔TS binding-path mirror", () => {
  it("re-derives exactly the keys uicomponents.WalkBindings emitted for the same declaration", () => {
    expect(derivePaths(declaration)).toEqual(golden.paths);
  });

  // The golden's page carries BOTH shapes of path on purpose, and the hook half
  // is the one that needs saying: a fixture with no hook would pin the region
  // rule only through the "" case, where the rule is indistinguishable from
  // "there is no rule". The `data` hook re-roots its subtree's numbering, so its
  // table's rows land at data/0#rows rather than at the /2#rows the same node
  // would answer to outside a hook.
  it("covers both a hook region and the region outside every hook", () => {
    expect(golden.paths.some((p) => p.startsWith("data/"))).toBe(true);
    expect(golden.paths.some((p) => p.startsWith("/"))).toBe(true);
  });
});

describe("the bindings response envelope the server actually writes", () => {
  it("resolves every binding when the REAL handler's response bytes are what arrives", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => ({ ok: true, json: async () => golden.response }) as Response));
    render(<AgentUIView {...props} />);

    // The "ok" entry's value reaches the bound ap:table, cell for cell.
    await waitFor(() => expect(screen.getByText("Acme")).toBeVisible());
    expect(screen.getByText("Globex")).toBeVisible();
    // The "error" entry's message reaches its own node's card, and the
    // declared placeholder it replaced is gone.
    expect(screen.getByText("this action failed")).toBeVisible();
    expect(screen.queryByText("loading...")).not.toBeInTheDocument();

    // The `data` hook's rows are the value AFTER pkg/web/uiselect ran server-side
    // (golden.declaration's own `select`, never applied again here — see
    // Binding.select's doc comment in types.ts). Names distinct from
    // Acme/Globex on purpose: screen.getByText throws on more than one
    // match, so reusing them would fail this assertion for a reason that has
    // nothing to do with selectors.
    expect(screen.getByText("Initech")).toBeVisible();
    expect(screen.getByText("Umbrella")).toBeVisible();
    // Proves only that the cursor is not RENDERED, not that the browser never
    // received it — the real proof that the browser gets only the extracted
    // value is the Go half asserting the handler's exact response bytes
    // (TestBindingsResponseMatchesTheGoldenTheBrowserParses). This is a
    // cheap second signal that catches a future regression where extraction
    // stops happening and the raw envelope starts reaching the browser.
    expect(screen.queryByText("c1")).not.toBeInTheDocument();
  });

  it("POSTs the exact request body the Go handler decoded through bindingsRequestBody", async () => {
    const bodies: unknown[] = [];
    vi.stubGlobal("fetch", vi.fn(async (_url: string, init: RequestInit) => {
      bodies.push(JSON.parse(String(init.body)));
      return { ok: true, json: async () => golden.response } as Response;
    }));
    render(<AgentUIView {...props} />);
    await waitFor(() => expect(bodies).toHaveLength(1));
    // Not just "has a params key": the declaration's own control defaults are
    // what seeds this map (collectDefaultParams), and the Go side asserts the
    // same bytes decode to the same parameter through ParamNames. A drift in
    // either walk shows up here as a different body.
    expect(bodies[0]).toEqual(golden.request);
  });
});
