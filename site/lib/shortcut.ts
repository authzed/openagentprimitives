// The docs search shortcut: ⌘K on Apple platforms, Ctrl K everywhere else.
// Each platform listens for its own modifier only. On macOS, Ctrl+K is the
// system "delete to end of line" binding in every text field, so claiming it
// there would break editing; and Super/Windows+K belongs to the OS elsewhere.

export interface ShortcutEvent {
  key: string;
  metaKey: boolean;
  ctrlKey: boolean;
  altKey: boolean;
  shiftKey: boolean;
}

/** `platform` is navigator.userAgentData?.platform or navigator.platform. */
export function isApplePlatform(platform: string): boolean {
  return /mac|iphone|ipad|ipod/i.test(platform);
}

export function isSearchShortcut(e: ShortcutEvent, apple: boolean): boolean {
  if (e.key.toLowerCase() !== "k" || e.altKey || e.shiftKey) return false;
  return apple ? e.metaKey && !e.ctrlKey : e.ctrlKey && !e.metaKey;
}

export function searchShortcutLabel(apple: boolean): string {
  return apple ? "⌘K" : "Ctrl K";
}
