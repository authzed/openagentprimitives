import { useEffect, useState } from "react";

// isTabActive reports whether the tab is BOTH visible and focused — the
// condition under which a user would notice an incoming reply unaided. Using
// focus (not visibility alone) means a reply that lands while a different
// window is focused still counts as "away". Guards for non-DOM environments
// (SSR/jsdom edge cases) so it never throws.
function isTabActive(): boolean {
  if (typeof document === "undefined") return true;
  const visible = document.visibilityState === "visible";
  const focused = typeof document.hasFocus === "function" ? document.hasFocus() : true;
  return visible && focused;
}

// useTabActivity tracks whether the chat tab is currently active, updating on
// visibility changes and window focus/blur. Listeners are attached once and
// removed on unmount.
export function useTabActivity(): boolean {
  const [active, setActive] = useState<boolean>(isTabActive);
  useEffect(() => {
    const update = () => setActive(isTabActive());
    document.addEventListener("visibilitychange", update);
    window.addEventListener("focus", update);
    window.addEventListener("blur", update);
    return () => {
      document.removeEventListener("visibilitychange", update);
      window.removeEventListener("focus", update);
      window.removeEventListener("blur", update);
    };
  }, []);
  return active;
}
