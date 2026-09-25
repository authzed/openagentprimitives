import { afterEach, describe, expect, it } from "vitest";
import { act, cleanup, renderHook } from "@testing-library/react";
import { getCookie, removeCookie, setCookie, useCookieState } from "./cookies";

// jsdom shares one document across a file; wipe every cookie between tests so a
// value set in one case can't leak into the next.
function clearAllCookies() {
  for (const part of document.cookie.split(";")) {
    const name = part.split("=")[0].trim();
    if (name) document.cookie = `${name}=; path=/; Max-Age=0`;
  }
}

afterEach(() => {
  cleanup();
  clearAllCookies();
});

describe("cookies", () => {
  it("round-trips set → get under the ap_admin_ prefix", () => {
    setCookie("view", "grid");
    expect(getCookie("view")).toBe("grid");
    expect(document.cookie).toContain("ap_admin_view=grid");
  });

  it("encodes and decodes values containing cookie-hostile characters", () => {
    setCookie("q", "a b;c=d");
    expect(getCookie("q")).toBe("a b;c=d");
  });

  it("returns null for an unset cookie", () => {
    expect(getCookie("missing")).toBeNull();
  });

  it("removeCookie clears a previously set value", () => {
    setCookie("temp", "x");
    expect(getCookie("temp")).toBe("x");
    removeCookie("temp");
    expect(getCookie("temp")).toBeNull();
  });
});

describe("useCookieState", () => {
  it("uses the fallback when the cookie is unset", () => {
    const { result } = renderHook(() => useCookieState("mode", "table"));
    expect(result.current[0]).toBe("table");
  });

  it("reads an existing cookie on mount", () => {
    setCookie("mode", "grid");
    const { result } = renderHook(() => useCookieState("mode", "table"));
    expect(result.current[0]).toBe("grid");
  });

  it("writes through to the cookie when the setter is called", () => {
    const { result } = renderHook(() => useCookieState("mode", "table"));
    act(() => result.current[1]("grid"));
    expect(result.current[0]).toBe("grid");
    expect(getCookie("mode")).toBe("grid");
  });

  it("decodes a JSON-string cookie value back to the plain string", () => {
    setCookie("mode", JSON.stringify("grid"));
    const { result } = renderHook(() => useCookieState("mode", "table"));
    expect(result.current[0]).toBe("grid");
  });
});
