import "@testing-library/jest-dom/vitest";
import { describe, expect, it } from "vitest";
import {
  PARAM_QUERY_PREFIX,
  paramsFromSearch,
  searchWithParams,
} from "./paramsUrl";

// The query string is where a binding parameter LIVES. Before this, a filter
// selection was React state and nothing else: it did not survive a reload, it
// did not survive switching to the transcript and back, and a filtered console
// could not be sent to anyone — the address described the session and never
// what the viewer had filtered it to.
describe("paramsFromSearch", () => {
  it("reads namespaced parameters and ignores the shell's own keys", () => {
    // The namespace is load-bearing: `session` and `view` belong to the shell,
    // and a declaration is free to name a parameter anything at all.
    const got = paramsFromSearch(
      "?session=ns%2Fname&view=ui&p.window.from=2026-08-01&p.minScore=80",
    );
    expect(got).toEqual({ "window.from": "2026-08-01", minScore: "80" });
  });

  it("does not confuse a declaration parameter NAMED like a shell key", () => {
    // An author who names a filter `view` must drive their own binding, not
    // the shell's view switch. Without the prefix these are the same key.
    const got = paramsFromSearch("?view=chat&p.view=weekly&p.session=alpha");
    expect(got).toEqual({ view: "weekly", session: "alpha" });
  });

  it("ignores a bare prefix with no parameter name", () => {
    // "p." carries no name; keeping it would put an empty-string key in the
    // map, which no declaration can ever declare.
    expect(paramsFromSearch("?p.=orphan&p.real=1")).toEqual({ real: "1" });
  });

  it("returns an empty map for a query with nothing of ours in it", () => {
    expect(paramsFromSearch("?session=ns%2Fname&view=ui")).toEqual({});
    expect(paramsFromSearch("")).toEqual({});
  });

  it("round-trips a value needing encoding", () => {
    const search = searchWithParams("", { "q.text": "a b&c=d" });
    expect(paramsFromSearch(search)).toEqual({ "q.text": "a b&c=d" });
  });
});

describe("searchWithParams", () => {
  it("preserves the shell's keys while writing parameters", () => {
    // Dropping `session` or `view` here would navigate the page as a side
    // effect of picking a date.
    const got = searchWithParams("?session=ns%2Fname&view=ui", {
      minScore: "80",
    });
    const sp = new URLSearchParams(got);
    expect(sp.get("session")).toBe("ns/name");
    expect(sp.get("view")).toBe("ui");
    expect(sp.get("p.minScore")).toBe("80");
  });

  it("REPLACES previous parameters rather than merging them", () => {
    // A parameter the viewer cleared, or one a rewritten declaration no longer
    // declares, has to leave the address. Merging would strand it there and
    // the next reader would seed state from a filter with no control for it.
    const got = searchWithParams("?view=ui&p.stale=1&p.minScore=0", {
      minScore: "80",
    });
    const sp = new URLSearchParams(got);
    expect(sp.get("p.stale")).toBeNull();
    expect(sp.get("p.minScore")).toBe("80");
    expect(sp.get("view")).toBe("ui");
  });

  it("is stable for the same map regardless of key insertion order", () => {
    // The caller compares the built string against the current one to decide
    // whether to touch history at all; an order-dependent result would make
    // that comparison spuriously unequal and rewrite history on every render.
    const a = searchWithParams("?view=ui", {
      "window.to": "2026-08-18",
      "window.from": "2026-08-01",
    });
    const b = searchWithParams("?view=ui", {
      "window.from": "2026-08-01",
      "window.to": "2026-08-18",
    });
    expect(a).toBe(b);
  });

  it("clears every parameter when given an empty map, keeping the rest", () => {
    const got = searchWithParams("?view=ui&p.a=1&p.b=2", {});
    expect(got).toBe("?view=ui");
  });

  it("returns an empty string rather than a bare '?' when nothing is left", () => {
    // A trailing "?" would differ from "" on the compare above and rewrite the
    // address for no reason.
    expect(searchWithParams("?p.a=1", {})).toBe("");
  });

  it("uses the exported prefix rather than a second hardcoded copy", () => {
    const got = searchWithParams("", { k: "v" });
    expect(got).toBe(`?${PARAM_QUERY_PREFIX}k=v`);
  });
});
