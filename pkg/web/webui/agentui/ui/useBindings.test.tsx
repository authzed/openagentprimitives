import "@testing-library/jest-dom/vitest";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
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

// The URL is shared, mutable state in jsdom, and AgentUIView both SEEDS its
// parameters from window.location.search and mirrors them back into it. Without
// this reset each test inherits the filters of whichever test ran before it —
// so a test that "changes a date" can silently be setting the value already in
// effect, issuing no request at all while appearing to exercise one.
afterEach(() => { cleanup(); vi.restoreAllMocks(); window.history.replaceState(null, "", "/"); });

const declaration = {
  view: { component: "ap:stack", children: [
    { component: "ap:select", props: { param: "span", value: "7d",
        options: [{ value: "7d", label: "7 days" }, { value: "30d", label: "30 days" }] } },
    { component: "ap:table", props: { columns: [{ key: "name" }], rows: [] },
        bindings: { rows: { source: "tool", ref: "crm_list_leads",
          args: { since: { $param: "span" } } } } },
  ] },
};

// Observed is one request the hook actually issued. The URL, method,
// credentials mode and content type are recorded — not just the body —
// because they are the half of the contract with pkg/web/webui/agentui's route
// registration that nothing else in this suite sees: a review proved by
// mutation that fetching ".../BINDINGZ" with method GET, no credentials and no
// content type left every test here green, while in production that is a 404
// or a 405 on every page load and the whole binding feature is silently inert.
//
// Recorded and asserted afterwards rather than asserted inside the stub: an
// expect() that throws in there rejects the fetch promise, which useBindings
// legitimately catches as a transport failure, so the real assertion message
// would be swallowed and surface as an unrelated timeout.
interface Observed {
  url: string;
  method: string | undefined;
  credentials: string | undefined;
  contentType: string | null;
  body: unknown;
}

function observe(url: string, init: RequestInit): Observed {
  return {
    url,
    method: init.method,
    credentials: init.credentials,
    contentType: new Headers(init.headers).get("Content-Type"),
    body: JSON.parse(String(init.body)),
  };
}

function stubFetch(bySpan: Record<string, unknown>) {
  const spans: string[] = [];
  const requests: Observed[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, init: RequestInit) => {
    const req = observe(url, init);
    requests.push(req);
    const span = (req.body as { params: { span: string } }).params.span;
    spans.push(span);
    return { ok: true, json: async () => ({ bindings: {
      "/1#rows": { status: "ok", value: bySpan[span] },
    } }) } as Response;
  }));
  return { spans, requests };
}

const props = { ns: "demo-ns", name: "demo-session", declaration, shown: true } as const;

describe("useBindings", () => {
  it("POSTs the session's own bindings route, with the cookie and a JSON content type", async () => {
    const { requests } = stubFetch({ "7d": [{ name: "Acme" }] });
    render(<AgentUIView {...props} />);
    await waitFor(() => expect(requests).toHaveLength(1));

    const req = requests[0];
    // The exact route pkg/web/webui/agentui registers, built from THIS page's
    // ns/name — not a prefix match, because a wrong suffix is a 404 and a
    // wrong session is someone else's data.
    expect(req.url).toBe(`/agent-ui/${props.ns}/${props.name}/bindings`);
    // POST: the route is registered for POST only, and the body is the
    // parameter map. A GET would 405 and carry no params at all.
    expect(req.method).toBe("POST");
    // The endpoint authenticates from the webui cookie and 401s without it.
    expect(req.credentials).toBe("same-origin");
    // The handler decodes JSON; the CSRF origin pin also depends on this
    // being a request the browser attaches an Origin header to.
    expect(req.contentType).toBe("application/json");
  });

  it("paints the first result using the declaration's own default parameter", async () => {
    const { spans } = stubFetch({ "7d": [{ name: "Acme" }], "30d": [{ name: "Globex" }] });
    render(<AgentUIView {...props} />);
    await waitFor(() => expect(screen.getByText("Acme")).toBeInTheDocument());
    expect(spans).toEqual(["7d"]);
  });

  it("re-evaluates the bound binding when a control changes the parameter", async () => {
    const { spans } = stubFetch({ "7d": [{ name: "Acme" }], "30d": [{ name: "Globex" }] });
    render(<AgentUIView {...props} />);
    await waitFor(() => expect(screen.getByText("Acme")).toBeInTheDocument());
    await userEvent.selectOptions(screen.getByRole("combobox"), "30d");
    await waitFor(() => expect(screen.getByText("Globex")).toBeInTheDocument());
    expect(spans).toEqual(["7d", "30d"]);
  });

  it("renders the error card for a failed binding without blanking the page", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => ({ ok: true, json: async () => ({ bindings: {
      "/1#rows": { status: "error", message: "This view's data is not available." },
    } }) }) as Response));
    render(<AgentUIView {...props} />);
    await waitFor(() =>
      expect(screen.getByText("This view's data is not available.")).toBeInTheDocument());
    expect(screen.getByRole("combobox")).toBeInTheDocument();
  });

  it("a transport failure is surfaced, never a silent blank", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => { throw new Error("offline"); }));
    render(<AgentUIView {...props} />);
    await waitFor(() => expect(screen.getByText(/could not be loaded/i)).toBeInTheDocument());
  });

  // bindings.go authors viewer-facing copy for every non-2xx it decides
  // (401/403/400/422/503) and puts it in {"error": …}. Collapsing all of it
  // into the transport message tells a viewer whose interact grant was just
  // revoked to check their network — advice that cannot help, and that sends
  // them to the wrong place. Only a body that carries no readable `error`
  // falls back to the generic copy.
  describe("a non-2xx carrying the server's own authored copy shows THAT, not the network message", () => {
    const cases = [
      { name: "403: the grant was revoked mid-session", status: 403, error: "not authorized to interact with this session" },
      { name: "400: the tab's declaration is stale", status: 400, error: "this view's settings do not match the page; reload the page" },
      { name: "503: the authz backend is down", status: 503, error: "authorization check failed" },
    ];
    for (const tc of cases) {
      it(tc.name, async () => {
        vi.stubGlobal("fetch", vi.fn(async () => ({
          ok: false, status: tc.status, json: async () => ({ error: tc.error }),
        }) as Response));
        render(<AgentUIView {...props} />);
        await waitFor(() => expect(screen.getByText(tc.error)).toBeVisible());
        expect(screen.queryByText(/check your connection/i)).not.toBeInTheDocument();
      });
    }

    it("falls back to the generic transport copy when the body carries no readable error", async () => {
      vi.stubGlobal("fetch", vi.fn(async () => ({
        ok: false, status: 502, json: async () => { throw new SyntaxError("not JSON"); },
      }) as Response));
      render(<AgentUIView {...props} />);
      await waitFor(() => expect(screen.getByText(/check your connection/i)).toBeVisible());
    });
  });

  // The brief does not exercise a response covering more than one binding
  // path at once, so this is added coverage: a response naming two DIFFERENT
  // paths must land each value on its own node, not on whichever node
  // happens to render first. MUTATION-CHECK (a): keying the applied value by
  // response order (e.g. Object.values(...)[0]) instead of by path passes
  // every OTHER test in this file (each only ever has one bound path in
  // flight) and fails only this one.
  it("keys each resolved value to its OWN binding path when a response covers more than one binding", async () => {
    const multiBinding = {
      view: {
        component: "ap:stack",
        children: [
          { component: "ap:table", props: { columns: [{ key: "name" }], rows: [] },
            bindings: { rows: { source: "tool", ref: "crm_list_leads" } } },
          { component: "ap:markdown", props: { body: "loading..." },
            bindings: { body: { source: "artifact", ref: "artifact-x" } } },
        ],
      },
    };
    vi.stubGlobal("fetch", vi.fn(async () => ({ ok: true, json: async () => ({ bindings: {
      "/0#rows": { status: "ok", value: [{ name: "Acme" }] },
      "/1#body": { status: "ok", value: "Refreshed hourly." },
    } }) }) as Response));
    render(<AgentUIView {...props} declaration={multiBinding} />);
    await waitFor(() => expect(screen.getByText("Acme")).toBeInTheDocument());
    expect(screen.getByText("Refreshed hourly.")).toBeInTheDocument();
  });

  // A viewer scrubbing a date range fires setParam once per field per
  // keystroke, and every POST re-resolves EVERY binding in the declaration (up
  // to maxBindingsPerRequest = 64) — including bindings that reference no
  // parameter at all. The generation guard discards the superseded RESPONSE
  // but does nothing about the work: without the two defenses below, one range
  // adjustment leaves dozens of already-dispatched tool executions running
  // against the runner, the great majority of them for values nothing will
  // ever display. The spec's own rationale for the UI ingress ceiling names
  // "cost / upstream DoS" as a surviving concern; this is that concern on the
  // request axis.
  describe("bounding the work a parameter scrub creates", () => {
    const rangeDeclaration = {
      view: { component: "ap:stack", children: [
        { component: "ap:daterange", props: { param: "window", from: "2026-01-01", to: "2026-01-31" } },
        { component: "ap:table", props: { columns: [{ key: "name" }], rows: [] },
          bindings: { rows: { source: "tool", ref: "crm_list_leads",
            args: { since: { $param: "window.from" } } } } },
      ] },
    };

    function stubRangeFetch() {
      const seen: { signal: AbortSignal | null | undefined; params: Record<string, string> }[] = [];
      vi.stubGlobal("fetch", vi.fn(async (_url: string, init: RequestInit) => {
        seen.push({ signal: init.signal, params: JSON.parse(String(init.body)).params });
        return { ok: true, json: async () => ({ bindings: {
          "/1#rows": { status: "ok", value: [{ name: "Acme" }] },
        } }) } as Response;
      }));
      return seen;
    }

    it("aborts a superseded request instead of leaving it running", async () => {
      const seen = stubRangeFetch();
      render(<AgentUIView {...props} declaration={rangeDeclaration} />);
      await waitFor(() => expect(seen).toHaveLength(1));
      expect(seen[0].signal).toBeInstanceOf(AbortSignal);
      expect(seen[0].signal?.aborted).toBe(false);

      fireEvent.change(screen.getByLabelText("From date"), { target: { value: "2026-02-01" } });

      // The first request's signal is aborted the moment its parameters are
      // superseded — the fetch is cancelled, not merely ignored on arrival.
      await waitFor(() => expect(seen[0].signal?.aborted).toBe(true));
    });

    it("coalesces a two-field range scrub into ONE request carrying both values", async () => {
      const seen = stubRangeFetch();
      render(<AgentUIView {...props} declaration={rangeDeclaration} />);
      await waitFor(() => expect(seen).toHaveLength(1));

      fireEvent.change(screen.getByLabelText("From date"), { target: { value: "2026-02-01" } });
      fireEvent.change(screen.getByLabelText("To date"), { target: { value: "2026-02-28" } });

      await waitFor(() => expect(seen).toHaveLength(2));
      // RFC3339, not the input's raw date-only value: ap:daterange normalizes
      // on the way out because DateRangeProps contracts instants, and the
      // upstream rules that consume these args parse them as such. See
      // params.test.tsx, which owns that conversion's own coverage.
      expect(seen[1].params).toEqual({ "window.from": "2026-02-01T00:00:00Z", "window.to": "2026-02-28T00:00:00Z" });

      // And nothing further: the intermediate state (new `from`, old `to`) was
      // never sent, so the runner never ran a fan-out for a range no one asked
      // about.
      await new Promise((r) => setTimeout(r, 400));
      expect(seen).toHaveLength(2);
    });

    // Re-evaluation is often fast enough that the page changed with no sign
    // anything had happened — a viewer moved a date and watched a still
    // screen, unable to tell "working on it" from "that did nothing". The
    // previous answer stays on screen and readable, marked as being replaced.
    it("marks a bound region as busy while a re-evaluation is in flight, and clears it when the answer lands", async () => {
      let release: (() => void) | null = null;
      const gate = new Promise<void>((r) => { release = r; });
      let calls = 0;
      vi.stubGlobal("fetch", vi.fn(async () => {
        calls += 1;
        if (calls > 1) await gate; // hold the RE-evaluation open, not the first load
        return { ok: true, json: async () => ({ bindings: {
          "/1#rows": { status: "ok", value: [{ name: "Acme" }] },
        } }) } as Response;
      }));

      // Every query is scoped to THIS render's container rather than to the
      // document. `screen` searches document.body, which in a file this size
      // can still hold a prior test's tree — and a "Acme" matched from someone
      // else's DOM would satisfy the wait before this component had loaded
      // anything, so the parameter change below would supersede the first
      // request and leave the region with nothing resolved.
      const { container } = render(<AgentUIView {...props} declaration={rangeDeclaration} />);
      const view = within(container);
      const busy = () => container.querySelector("[data-testid=\"agent-ui-region-refreshing\"]");

      // Wait for the FIRST answer to actually land, not merely for the absence
      // of a marker: the absence is true at t=0 too, and a region with no
      // resolved binding is correctly never "out of date".
      await waitFor(() => expect(view.getByText("Acme")).toBeInTheDocument());
      expect(busy()).toBeNull();

      fireEvent.change(view.getByLabelText("From date"), { target: { value: "2026-02-01" } });

      // Busy while it is in flight — and the already-resolved content is still
      // there, because a correct-but-stale answer beats an empty region. The
      // gate holds the response open, so this is a steady state to observe,
      // not a transient to catch.
      await waitFor(() => expect(busy()).not.toBeNull());
      expect(view.getByText("Acme")).toBeInTheDocument();

      release?.();
      await waitFor(() => expect(busy()).toBeNull());
    });

    // The same fact for a NAMED region. Every other staleness row here drives a
    // "/N#prop" key, so the region they exercise is "" — the one the view draws
    // itself. A staleness derivation that only ever produced "" would satisfy
    // all of them and leave a re-resolving HOOK showing nothing, because the
    // hook reads its own busy state out of HookState.stale by NAME and would
    // never find itself there.
    //
    // The binding lives inside the hook, so its key is region-relative:
    // "data/0#rows", not "/1.0#rows". That is the same region rule the
    // bindings golden pins across both languages.
    it("marks a bound HOOK as busy by name while its own re-evaluation is in flight", async () => {
      const hookDeclaration = {
        view: { component: "ap:stack", children: [
          { component: "ap:daterange", props: { param: "window", from: "2026-01-01", to: "2026-01-31" } },
          { component: "oap:generative", props: { name: "data", allowedComponents: ["*"] }, children: [
            { component: "ap:table", props: { columns: [{ key: "name" }], rows: [] },
              bindings: { rows: { source: "tool", ref: "crm_list_leads",
                args: { since: { $param: "window.from" } } } } },
          ] },
        ] },
      };

      let release: (() => void) | null = null;
      const gate = new Promise<void>((r) => { release = r; });
      let calls = 0;
      vi.stubGlobal("fetch", vi.fn(async () => {
        calls += 1;
        if (calls > 1) await gate; // hold the RE-evaluation open, not the first load
        return { ok: true, json: async () => ({ bindings: {
          "data/0#rows": { status: "ok", value: [{ name: "Acme" }] },
        } }) } as Response;
      }));

      const { container } = render(<AgentUIView {...props} declaration={hookDeclaration} />);
      const view = within(container);
      const region = () => container.querySelector('[data-hook="data"]');
      const busy = () => container.querySelector("[data-testid=\"agent-ui-hook-refreshing\"]");

      await waitFor(() => expect(view.getByText("Acme")).toBeInTheDocument());
      expect(busy()).toBeNull();
      expect(region()).not.toHaveAttribute("aria-busy");

      fireEvent.change(view.getByLabelText("From date"), { target: { value: "2026-02-01" } });

      // The HOOK says it, not the page: the cue is on the region that owns the
      // re-resolving binding, and the view's own root-region indicator stays
      // silent because no binding outside the hook went stale.
      await waitFor(() => expect(region()).toHaveAttribute("aria-busy", "true"));
      expect(busy()).not.toBeNull();
      expect(container.querySelector("[data-testid=\"agent-ui-region-refreshing\"]")).toBeNull();
      expect(view.getByText("Acme")).toBeInTheDocument();

      release?.();
      await waitFor(() => expect(region()).not.toHaveAttribute("aria-busy"));
    });

    // The first load has its own vocabulary — the skeleton stand-ins
    // applyBindings substitutes for unresolved paths — and must not ALSO be
    // announced as a refresh. Two different "waiting" treatments stacked on
    // one region reads as a fault.
    it("says nothing on the first load, where the skeletons already answer the question", async () => {
      let release: (() => void) | null = null;
      const gate = new Promise<void>((r) => { release = r; });
      vi.stubGlobal("fetch", vi.fn(async () => {
        await gate;
        return { ok: true, json: async () => ({ bindings: {
          "/1#rows": { status: "ok", value: [{ name: "Acme" }] },
        } }) } as Response;
      }));

      const { container } = render(<AgentUIView {...props} declaration={rangeDeclaration} />);
      await new Promise((r) => setTimeout(r, 50));
      expect(container.querySelector("[data-testid=\"agent-ui-region-refreshing\"]")).toBeNull();

      release?.();
      await waitFor(() => expect(screen.getByText("Acme")).toBeInTheDocument());
    });
  });

  // The brief calls this out by name as the classic bug in this shape: a
  // rapid sequence of parameter changes must not let a slow response to an
  // EARLIER parameter overwrite a fast response to a LATER one.
  // MUTATION-CHECK (c): neutering useBindings.ts's isCurrent() guard (making
  // every response unconditionally accepted) passes every OTHER test in this
  // file — none of them have two requests genuinely racing — and fails only
  // this one.
  it("ignores a stale response: a slow FIRST request resolving after a fast SECOND one must not overwrite it", async () => {
    const calls: string[] = [];
    const resolvers: Record<string, (r: Response) => void> = {};
    const responseFor = (span: string): Response =>
      ({ ok: true, json: async () => ({ bindings: {
        "/1#rows": { status: "ok", value: [{ name: span === "7d" ? "Acme" : "Globex" }] },
      } }) }) as Response;
    vi.stubGlobal("fetch", vi.fn((_url: string, init: RequestInit) => {
      const span = (JSON.parse(String(init.body)).params as { span: string }).span;
      calls.push(span);
      return new Promise<Response>((resolve) => {
        resolvers[span] = resolve;
      });
    }));

    render(<AgentUIView {...props} />);
    await waitFor(() => expect(calls).toEqual(["7d"]));

    await userEvent.selectOptions(screen.getByRole("combobox"), "30d");
    await waitFor(() => expect(calls).toEqual(["7d", "30d"]));

    // Resolve the NEWER request first, then let the stale older one land —
    // exactly the out-of-order arrival the generation guard exists for.
    resolvers["30d"](responseFor("30d"));
    await waitFor(() => expect(screen.getByText("Globex")).toBeInTheDocument());
    resolvers["7d"](responseFor("7d"));
    await new Promise((r) => setTimeout(r, 0));

    expect(screen.getByText("Globex")).toBeInTheDocument();
    expect(screen.queryByText("Acme")).not.toBeInTheDocument();
  });
});
