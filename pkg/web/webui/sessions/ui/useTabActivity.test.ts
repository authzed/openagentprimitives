import { describe, it, expect, afterEach, vi } from "vitest";
import { renderHook, act, cleanup } from "@testing-library/react";
import { useTabActivity } from "./useTabActivity";

// setVisibility overrides the read-only document.visibilityState for a test.
function setVisibility(state: "visible" | "hidden") {
  Object.defineProperty(document, "visibilityState", { configurable: true, value: state });
}

afterEach(() => {
  cleanup();
  setVisibility("visible");
  vi.restoreAllMocks();
});

describe("useTabActivity", () => {
  it("starts active when the tab is visible and focused", () => {
    setVisibility("visible");
    vi.spyOn(document, "hasFocus").mockReturnValue(true);
    const { result } = renderHook(() => useTabActivity());
    expect(result.current).toBe(true);
  });

  it("goes inactive when the tab is hidden", () => {
    vi.spyOn(document, "hasFocus").mockReturnValue(true);
    const { result } = renderHook(() => useTabActivity());
    act(() => {
      setVisibility("hidden");
      document.dispatchEvent(new Event("visibilitychange"));
    });
    expect(result.current).toBe(false);
  });

  it("goes inactive on window blur even while visible", () => {
    const focus = vi.spyOn(document, "hasFocus").mockReturnValue(true);
    const { result } = renderHook(() => useTabActivity());
    act(() => {
      focus.mockReturnValue(false);
      window.dispatchEvent(new Event("blur"));
    });
    expect(result.current).toBe(false);
    act(() => {
      focus.mockReturnValue(true);
      window.dispatchEvent(new Event("focus"));
    });
    expect(result.current).toBe(true);
  });
});
