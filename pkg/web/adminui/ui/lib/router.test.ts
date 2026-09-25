import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { renderHook, act, cleanup } from "@testing-library/react";
import { buildPath, navigate, parseLocation, useRoute, viewForRoute, type Route } from "./router";

describe("parseLocation", () => {
  it("/admin → overview view", () => {
    expect(parseLocation("/admin", "")).toEqual({ type: "view", view: "overview" });
  });
  it("/admin/ (trailing slash) → overview view", () => {
    expect(parseLocation("/admin/", "")).toEqual({ type: "view", view: "overview" });
  });
  it("/admin/logs → logs view", () => {
    expect(parseLocation("/admin/logs", "")).toEqual({ type: "view", view: "logs" });
  });
  it("/admin/agents → agents LIST view (plural nav id)", () => {
    expect(parseLocation("/admin/agents", "")).toEqual({ type: "view", view: "agents" });
  });
  it("/admin/agent/default/x → agent DETAIL (singular disambiguates from list)", () => {
    expect(parseLocation("/admin/agent/default/x", "")).toEqual({
      type: "detail", entity: "agent", id: "default/x",
    });
  });
  it("/admin/session/ns/name?tab=activity → session detail carrying the tab", () => {
    expect(parseLocation("/admin/session/ns/name", "?tab=activity")).toEqual({
      type: "detail", entity: "session", id: "ns/name", tab: "activity",
    });
  });
  it("unknown first segment → overview (fail-safe)", () => {
    expect(parseLocation("/admin/nope", "")).toEqual({ type: "view", view: "overview" });
  });
  it("entity kind with no id → overview (singular but incomplete)", () => {
    expect(parseLocation("/admin/agent", "")).toEqual({ type: "view", view: "overview" });
  });
});

describe("buildPath round-trips parseLocation", () => {
  const cases: Route[] = [
    { type: "view", view: "overview" },
    { type: "view", view: "logs" },
    { type: "view", view: "agents" },
    { type: "detail", entity: "agent", id: "default/x" },
    { type: "detail", entity: "provider", id: "okta" },
    { type: "detail", entity: "session", id: "ns/name", tab: "activity" },
  ];
  for (const r of cases) {
    it(`round-trips ${JSON.stringify(r)}`, () => {
      const path = buildPath(r);
      const [pathname, search] = path.split("?");
      expect(parseLocation(pathname, search ? "?" + search : "")).toEqual(r);
    });
  }
  it("overview builds to the bare base /admin", () => {
    expect(buildPath({ type: "view", view: "overview" })).toBe("/admin");
  });
  it("a detail tab serializes as ?tab=", () => {
    expect(buildPath({ type: "detail", entity: "session", id: "ns/name", tab: "activity" }))
      .toBe("/admin/session/ns/name?tab=activity");
  });
});

describe("viewForRoute", () => {
  it("returns the view for a view route", () => {
    expect(viewForRoute({ type: "view", view: "logs" })).toBe("logs");
  });
  it("maps a detail entity to the nav view it lives under (agent → agents)", () => {
    expect(viewForRoute({ type: "detail", entity: "agent", id: "x" })).toBe("agents");
  });
  it("maps a session detail under the sessions nav view", () => {
    expect(viewForRoute({ type: "detail", entity: "session", id: "ns/n" })).toBe("sessions");
  });
  it("maps a login-provider detail under the identity nav view", () => {
    expect(viewForRoute({ type: "detail", entity: "provider", id: "okta" })).toBe("identity");
  });
});

describe("navigate (jsdom history)", () => {
  beforeEach(() => window.history.pushState({}, "", "/admin"));
  afterEach(() => cleanup());

  it("pushState updates window.location.pathname for a view route", () => {
    navigate({ type: "view", view: "logs" });
    expect(window.location.pathname).toBe("/admin/logs");
  });
  it("pushState carries a detail path + ?tab query", () => {
    navigate({ type: "detail", entity: "session", id: "ns/name", tab: "activity" });
    expect(window.location.pathname).toBe("/admin/session/ns/name");
    expect(window.location.search).toBe("?tab=activity");
  });
});

describe("useRoute", () => {
  beforeEach(() => window.history.pushState({}, "", "/admin"));
  afterEach(() => cleanup());

  it("reflects the initial location then re-renders on navigate", () => {
    const { result } = renderHook(() => useRoute());
    expect(result.current).toEqual({ type: "view", view: "overview" });
    act(() => navigate({ type: "view", view: "agents" }));
    expect(result.current).toEqual({ type: "view", view: "agents" });
  });
  it("reflects a popstate (native back/forward)", () => {
    const { result } = renderHook(() => useRoute());
    act(() => navigate({ type: "view", view: "logs" }));
    expect(result.current).toEqual({ type: "view", view: "logs" });
    act(() => {
      window.history.pushState({}, "", "/admin");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });
    expect(result.current).toEqual({ type: "view", view: "overview" });
  });
});
