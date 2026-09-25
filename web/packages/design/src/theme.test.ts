import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { applyStoredTheme, readThemeChoice, resolveTheme, setThemeChoice, THEME_STORAGE_KEY } from "./theme";

function mockMatchMedia(lightMatches: boolean) {
  const listeners: Array<() => void> = [];
  const mq = {
    matches: lightMatches,
    addEventListener: (_: string, fn: () => void) => listeners.push(fn),
    removeEventListener: (_: string, fn: () => void) => listeners.splice(listeners.indexOf(fn), 1),
  };
  vi.stubGlobal("matchMedia", () => mq);
  return { mq, fire: () => listeners.forEach((l) => l()) };
}

// The project's jsdom setup ships a partial localStorage; the module only
// needs get/set, so the test brings its own Map-backed one and drops it after.
function mockStorage() {
  const m = new Map<string, string>();
  vi.stubGlobal("localStorage", {
    getItem: (k: string) => m.get(k) ?? null,
    setItem: (k: string, v: string) => void m.set(k, v),
    removeItem: (k: string) => void m.delete(k),
  });
  return m;
}

describe("theme", () => {
  let store: Map<string, string>;
  beforeEach(() => { store = mockStorage(); delete document.documentElement.dataset.theme; });
  afterEach(() => vi.unstubAllGlobals());

  it("defaults to system when nothing is stored, and system follows the OS", () => {
    expect(readThemeChoice()).toBe("system");
    mockMatchMedia(true);
    applyStoredTheme();
    expect(document.documentElement.dataset.theme).toBe("light");
  });

  it("persists a choice and applies it", () => {
    mockMatchMedia(false);
    expect(setThemeChoice("light")).toBe("light");
    expect(store.get(THEME_STORAGE_KEY)).toBe("light");
    expect(document.documentElement.dataset.theme).toBe("light");
  });

  it("resolves system from the OS and follows changes while system is chosen", () => {
    const m = mockMatchMedia(true);
    expect(resolveTheme("system")).toBe("light");
    setThemeChoice("system");
    const off = applyStoredTheme();
    expect(document.documentElement.dataset.theme).toBe("light");
    m.mq.matches = false; m.fire();
    expect(document.documentElement.dataset.theme).toBe("dark");
    off();
  });

  it("ignores garbage in storage", () => {
    store.set(THEME_STORAGE_KEY, "sepia");
    expect(readThemeChoice()).toBe("system");
  });
});
