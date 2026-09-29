import { describe, expect, it } from "vitest";
import {
  isApplePlatform,
  isSearchShortcut,
  searchShortcutLabel,
} from "./shortcut";

const key = (
  k: string,
  mods: Partial<
    Record<"metaKey" | "ctrlKey" | "altKey" | "shiftKey", boolean>
  > = {},
) => ({
  key: k,
  metaKey: false,
  ctrlKey: false,
  altKey: false,
  shiftKey: false,
  ...mods,
});

describe("isApplePlatform", () => {
  it.each([
    ["macOS", true],
    ["MacIntel", true],
    ["iPhone", true],
    ["iPad", true],
    ["Win32", false],
    ["Windows", false],
    ["Linux x86_64", false],
    ["", false],
  ])("%s → %s", (platform, want) =>
    expect(isApplePlatform(platform)).toBe(want),
  );
});

describe("isSearchShortcut", () => {
  it.each([
    ["⌘K on Apple opens search", key("k", { metaKey: true }), true, true],
    [
      "⌘K with caps lock on Apple opens search",
      key("K", { metaKey: true }),
      true,
      true,
    ],
    [
      "Ctrl+K on Apple is left alone (kill-to-end-of-line in text fields)",
      key("k", { ctrlKey: true }),
      true,
      false,
    ],
    ["Ctrl+K elsewhere opens search", key("k", { ctrlKey: true }), false, true],
    [
      "⊞/Super+K elsewhere is left alone",
      key("k", { metaKey: true }),
      false,
      false,
    ],
    [
      "Ctrl+Shift+K elsewhere is left alone (browser devtools)",
      key("K", { ctrlKey: true, shiftKey: true }),
      false,
      false,
    ],
    [
      "⌘⌥K on Apple is left alone",
      key("k", { metaKey: true, altKey: true }),
      true,
      false,
    ],
    [
      "⌘+⌃+K on Apple is left alone",
      key("k", { metaKey: true, ctrlKey: true }),
      true,
      false,
    ],
    ["plain K is left alone", key("k"), false, false],
    ["⌘J on Apple is left alone", key("j", { metaKey: true }), true, false],
  ])("%s", (_name, event, apple, want) =>
    expect(isSearchShortcut(event, apple)).toBe(want),
  );
});

describe("searchShortcutLabel", () => {
  it("names the platform's own modifier", () => {
    expect(searchShortcutLabel(true)).toBe("⌘K");
    expect(searchShortcutLabel(false)).toBe("Ctrl K");
  });
});
