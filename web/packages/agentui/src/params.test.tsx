import "@testing-library/jest-dom/vitest";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import {
  BindingParamsProvider,
  collectDeclaredParamKeys,
  collectDefaultParams,
  reconcileParams,
  useBindingParams,
} from "./params";
import { PARAM_SPECS } from "./paramSpecs";
import { renderNode } from "./renderNode";
import type { Declaration, Node } from "./types";

afterEach(cleanup);

const node = (
  component: string,
  props: Record<string, unknown> = {},
  children: Node[] = [],
): Node => ({
  component,
  props,
  children,
});

describe("collectDefaultParams", () => {
  it("reads ap:select's declared value into <param>", () => {
    const decl: Declaration = {
      view: node("ap:select", {
        param: "span",
        value: "7d",
        options: [{ value: "7d" }],
      }),
    };
    expect(collectDefaultParams(decl)).toEqual({ span: "7d" });
  });

  it("reads ap:daterange's declared from/to into <param>.from and <param>.to", () => {
    const decl: Declaration = {
      view: node("ap:daterange", {
        param: "window",
        from: "2026-01-01",
        to: "2026-01-31",
      }),
    };
    expect(collectDefaultParams(decl)).toEqual({
      "window.from": "2026-01-01",
      "window.to": "2026-01-31",
    });
  });

  it("descends into children and skips a control with no declared param name", () => {
    const decl: Declaration = {
      view: node("ap:stack", {}, [
        node("ap:select", { value: "unbound" }), // no `param` — contributes nothing
        node("ap:card", {}, [
          node("ap:select", { param: "region", value: "us-east" }),
        ]),
      ]),
    };
    expect(collectDefaultParams(decl)).toEqual({ region: "us-east" });
  });

  it("returns an empty map for a declaration with no controls", () => {
    const decl: Declaration = { view: node("ap:heading", { text: "hi" }) };
    expect(collectDefaultParams(decl)).toEqual({});
  });

  // A hook is a node in the same tree, so a control the AGENT wrote into one is
  // a control the page must seed — the walk has no business asking who authored
  // a node. The server's own ParamNames walks the identical tree; a browser
  // that skipped hook children would leave such a parameter unset and every
  // binding under it answering "choose a value for this view's settings".
  it("descends into a hook's children, which the agent may have written", () => {
    const decl: Declaration = {
      view: node("ap:stack", {}, [
        node("oap:generative", { name: "filters", allowedComponents: ["*"] }, [
          node("ap:select", {
            param: "region",
            value: "us-east",
            options: [{ value: "us-east" }],
          }),
        ]),
      ]),
    };
    expect(collectDefaultParams(decl)).toEqual({ region: "us-east" });
    expect(collectDeclaredParamKeys(decl)).toEqual(["region"]);
  });

  // A declaration with no view is what a session that has not produced one yet
  // looks like. It is not an error and must not throw: the view renders its own
  // "nothing declared" state, and the parameter map is simply empty.
  it("returns an empty map, not a throw, for a declaration with no view", () => {
    const decl = {} as Declaration;
    expect(collectDefaultParams(decl)).toEqual({});
    expect(collectDeclaredParamKeys(decl)).toEqual([]);
    expect(reconcileParams(decl, { span: "90d" })).toEqual({});
  });

  // The mechanism, not just the outcome: pkg/web/uicomponents answers "which
  // parameter does this node declare?" through registry.Get(...).ParamProp so
  // that a new parameterizing control is a registration. This side must be the
  // same, and a type switch would pass every row above while failing this one
  // — which is exactly the drift that would let a third Go-registered control
  // pass the server's ParamKeys check while the browser never seeds it.
  it("picks up a control registered at runtime, with no consumer edit — the walk is table-driven, not a type switch", () => {
    PARAM_SPECS["ap:fixture_slider"] = {
      prop: "param",
      values: [
        { suffix: "min", valueProp: "low" },
        { suffix: "max", valueProp: "high" },
      ],
    };
    try {
      const decl: Declaration = {
        view: node("ap:fixture_slider", {
          param: "range",
          low: "10",
          high: "90",
        }),
      };
      expect(collectDefaultParams(decl)).toEqual({
        "range.min": "10",
        "range.max": "90",
      });
    } finally {
      // The table is module state; leaving a fixture type in it would change
      // the vocabulary every later test sees.
      delete PARAM_SPECS["ap:fixture_slider"];
    }
  });
});

describe("reconcileParams", () => {
  // A live view update (AgentUIView's onView) rewrites the declaration; this
  // is the seam that decides what happens to the viewer's CURRENT parameter
  // choices when that happens.
  it("keeps the viewer's existing value for a key BOTH the old and new declaration declare", () => {
    const decl: Declaration = {
      view: node("ap:select", {
        param: "span",
        value: "7d",
        options: [{ value: "7d" }],
      }),
    };
    expect(reconcileParams(decl, { span: "90d" })).toEqual({ span: "90d" });
  });

  it("seeds a key the NEW declaration declares for the first time from its own default", () => {
    const decl: Declaration = {
      view: node("ap:select", {
        param: "region",
        value: "us-east",
        options: [{ value: "us-east" }],
      }),
    };
    expect(reconcileParams(decl, {})).toEqual({ region: "us-east" });
  });

  it("drops a key the new declaration no longer declares", () => {
    const decl: Declaration = { view: node("ap:heading", { text: "hi" }) };
    expect(reconcileParams(decl, { span: "90d" })).toEqual({});
  });

  it("does all three at once, over a declaration with two controls", () => {
    const decl: Declaration = {
      view: node("ap:stack", {}, [
        node("ap:select", {
          param: "span",
          value: "7d",
          options: [{ value: "7d" }],
        }), // survives
        node("ap:select", {
          param: "region",
          value: "us-east",
          options: [{ value: "us-east" }],
        }), // new
      ]),
    };
    // "stale" is not declared by this decl at all — must be dropped.
    expect(reconcileParams(decl, { span: "90d", stale: "gone" })).toEqual({
      span: "90d",
      region: "us-east",
    });
  });

  // Every case above drives a control that carries a literal default, which is
  // why this whole class of bug stayed invisible: for those, "declared" and
  // "has a default" are the same set. They are not the same set in general,
  // and an author is pushed toward the difference — a date range whose literal
  // default would go stale has to ship without one.
  describe("a control that declares a parameter but ships NO default", () => {
    const dateOnly: Declaration = {
      view: node("ap:daterange", { param: "window" }),
    };

    it("keeps the viewer's picked range across a live view update", () => {
      // The regression. The agent writes an unrelated slot, a `view` frame
      // arrives, and the viewer's date range must still be theirs. Before the
      // declared/defaulted split this returned {} — the picker reset to empty
      // and every binding under it fell back to "choose a value for this
      // view's settings", one agent turn after the viewer chose.
      expect(
        reconcileParams(dateOnly, {
          "window.from": "2026-08-01",
          "window.to": "2026-08-18",
        }),
      ).toEqual({
        "window.from": "2026-08-01",
        "window.to": "2026-08-18",
      });
    });

    it("leaves an unpicked key ABSENT rather than empty-string", () => {
      // Absent and "" mean different things to the server: absent is
      // ErrUnknownParam ("choose a value"), while "" is a real value it would
      // pass to the tool. Seeding "" would turn "nothing picked yet" into a
      // query for the empty window.
      expect(reconcileParams(dateOnly, {})).toEqual({});
    });

    it("keeps one half of a range when only one end is picked", () => {
      expect(
        reconcileParams(dateOnly, { "window.from": "2026-08-01" }),
      ).toEqual({ "window.from": "2026-08-01" });
    });

    it("still drops a key this declaration does not declare at all", () => {
      // The third rule must survive the fix: a genuinely undeclared key is
      // still dropped, so a parameter a rewritten declaration removed cannot
      // linger in a request it can no longer affect.
      expect(
        reconcileParams(dateOnly, {
          "window.from": "2026-08-01",
          stale: "gone",
        }),
      ).toEqual({
        "window.from": "2026-08-01",
      });
    });

    it("mixes a defaulted control and an undefaulted one in one declaration", () => {
      // The console's real shape: a score select WITH a default beside a date
      // range WITHOUT one. Both must survive.
      const mixed: Declaration = {
        view: node("ap:stack", {}, [
          node("ap:daterange", { param: "window" }),
          node("ap:select", {
            param: "minScore",
            value: "0",
            options: [{ value: "0" }],
          }),
        ]),
      };
      expect(
        reconcileParams(mixed, { "window.from": "2026-08-01", minScore: "80" }),
      ).toEqual({
        "window.from": "2026-08-01",
        minScore: "80",
      });
    });
  });

  describe("collectDeclaredParamKeys", () => {
    it("reports a parameter with no default, which collectDefaultParams cannot", () => {
      const decl: Declaration = {
        view: node("ap:daterange", { param: "window" }),
      };
      // The two functions answering differently for the same declaration IS
      // the distinction the fix rests on; asserting both together is what
      // stops a future edit collapsing them back into one.
      expect(collectDefaultParams(decl)).toEqual({});
      expect(collectDeclaredParamKeys(decl)).toEqual([
        "window.from",
        "window.to",
      ]);
    });

    it("reports both keys of a range and the single key of a select", () => {
      const decl: Declaration = {
        view: node("ap:stack", {}, [
          node("ap:daterange", { param: "window" }),
          node("ap:select", {
            param: "minScore",
            value: "0",
            options: [{ value: "0" }],
          }),
        ]),
      };
      expect(collectDeclaredParamKeys(decl)).toEqual([
        "window.from",
        "window.to",
        "minScore",
      ]);
    });

    it("reports nothing for a declaration with no parameterizing control", () => {
      const decl: Declaration = { view: node("ap:heading", { text: "hi" }) };
      expect(collectDeclaredParamKeys(decl)).toEqual([]);
    });
  });
});

describe("ap:select — live when bound to a parameter", () => {
  it("is not disabled and calls setParam with the chosen option's value on change, when a provider is present", () => {
    const setParam = vi.fn();
    render(
      <BindingParamsProvider value={{ params: { span: "7d" }, setParam }}>
        {renderNode(
          node("ap:select", {
            param: "span",
            value: "7d",
            options: [
              { value: "7d", label: "7 days" },
              { value: "30d", label: "30 days" },
            ],
          }),
        )}
      </BindingParamsProvider>,
    );
    const select = screen.getByRole("combobox");
    expect(select).not.toBeDisabled();
    fireEvent.change(select, { target: { value: "30d" } });
    expect(setParam).toHaveBeenCalledWith("span", "30d");
  });

  it("stays disabled when the declaration gives it no param name — a control that looks live but drives nothing is worse than one that reads as not-yet-wired", () => {
    render(
      <BindingParamsProvider value={{ params: {}, setParam: vi.fn() }}>
        {renderNode(
          node("ap:select", { value: "7d", options: [{ value: "7d" }] }),
        )}
      </BindingParamsProvider>,
    );
    expect(screen.getByRole("combobox")).toBeDisabled();
  });

  it("renders and does not throw with NO provider at all — the standalone @ap/agentui case Plan 1's fixtures exercise", () => {
    expect(() =>
      render(
        renderNode(
          node("ap:select", {
            param: "span",
            value: "7d",
            options: [{ value: "7d" }],
          }),
        ),
      ),
    ).not.toThrow();
    // Still not disabled — a missing provider degrades to an inert no-op
    // setParam (params.tsx's context default), not to the old blanket-disabled
    // rendering; the control looking alive is what "inert" means here.
    expect(screen.getByRole("combobox")).not.toBeDisabled();
  });
});

describe("ap:daterange — live when bound to a parameter", () => {
  // This test previously pinned the BUG: it asserted the raw date-only value
  // the native input produces. DateRangeProps.From/To are contracted RFC3339
  // (pkg/web/uicomponents), and the tool args they feed are consumed as instants —
  // an MCPServer's CEL trust constraint calls timestamp(f.value) on them, and
  // CEL rejects "2026-02-01" outright. That conversion error failed closed and
  // reached the viewer as "you do not have access to this data": every date
  // the picker produced was refused, with nothing on the page hinting at why.
  it("calls setParam with RFC3339 instants, not the input's raw date-only value", () => {
    const setParam = vi.fn();
    render(
      <BindingParamsProvider
        value={{
          params: { "window.from": "2026-01-01", "window.to": "2026-01-31" },
          setParam,
        }}
      >
        {renderNode(
          node("ap:daterange", {
            param: "window",
            from: "2026-01-01",
            to: "2026-01-31",
          }),
        )}
      </BindingParamsProvider>,
    );
    fireEvent.change(screen.getByLabelText("From date"), {
      target: { value: "2026-02-01" },
    });
    fireEvent.change(screen.getByLabelText("To date"), {
      target: { value: "2026-02-28" },
    });
    expect(setParam).toHaveBeenCalledWith(
      "window.from",
      "2026-02-01T00:00:00Z",
    );
    expect(setParam).toHaveBeenCalledWith("window.to", "2026-02-28T00:00:00Z");
  });

  // The display half of the same conversion. A native date input renders
  // NOTHING unless its value is exactly YYYY-MM-DD, so an RFC3339 parameter —
  // which is what a shared link or a reload now carries — has to be trimmed
  // back before it reaches the control. Without this a viewer returning to a
  // filtered page sees an empty picker over filtered data: the two halves of
  // the screen disagreeing about what is being shown.
  it("displays an RFC3339 parameter as a date the input can actually render", () => {
    render(
      <BindingParamsProvider
        value={{
          params: {
            "window.from": "2026-02-01T00:00:00Z",
            "window.to": "2026-02-28T00:00:00Z",
          },
          setParam: vi.fn(),
        }}
      >
        {renderNode(node("ap:daterange", { param: "window" }))}
      </BindingParamsProvider>,
    );
    expect(screen.getByLabelText("From date")).toHaveValue("2026-02-01");
    expect(screen.getByLabelText("To date")).toHaveValue("2026-02-28");
  });

  // A DECLARED default may be a real instant rather than a bare date — the
  // props are RFC3339 — and it must still reach the control as something the
  // control can render. Asserted through the declaration rather than through
  // the input, because a native date input cannot produce a value carrying a
  // time at all: it normalizes anything else to "". The passthrough branch in
  // dateParamValue therefore protects the declared-default path, which is the
  // only one that can reach it.
  it("renders a declared RFC3339 default as a date the input accepts", () => {
    render(
      <BindingParamsProvider value={{ params: {}, setParam: vi.fn() }}>
        {renderNode(
          node("ap:daterange", {
            param: "window",
            from: "2026-02-01T09:30:00Z",
          }),
        )}
      </BindingParamsProvider>,
    );
    expect(screen.getByLabelText("From date")).toHaveValue("2026-02-01");
  });

  // Clearing the picker must clear the PARAMETER, not send a converted empty
  // string. "T00:00:00Z" alone is not a date, and the server would take it as
  // a real value rather than "the viewer cleared this".
  it("sends an empty value unchanged when the viewer clears the field", () => {
    const setParam = vi.fn();
    render(
      <BindingParamsProvider
        value={{ params: { "window.from": "2026-02-01T00:00:00Z" }, setParam }}
      >
        {renderNode(node("ap:daterange", { param: "window" }))}
      </BindingParamsProvider>,
    );
    fireEvent.change(screen.getByLabelText("From date"), {
      target: { value: "" },
    });
    expect(setParam).toHaveBeenCalledWith("window.from", "");
  });

  it("stays disabled (the pre-existing button rendering) when the declaration gives it no param name", () => {
    render(
      renderNode(
        node("ap:daterange", { from: "2026-01-01", to: "2026-01-31" }),
      ),
    );
    expect(screen.getByRole("button")).toBeDisabled();
  });
});

describe("useBindingParams", () => {
  it("returns the inert default ({} params, no-op setParam) outside any provider", () => {
    let captured: ReturnType<typeof useBindingParams> | undefined;
    function Probe() {
      captured = useBindingParams();
      return null;
    }
    render(<Probe />);
    expect(captured?.params).toEqual({});
    expect(() => captured?.setParam("x", "y")).not.toThrow();
  });
});
