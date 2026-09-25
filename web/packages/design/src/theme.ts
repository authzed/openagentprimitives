// Theme choice for every browser surface (admin, sessions, sign-in is server
// HTML and stays dark). The server stamps data-theme="dark" on <html>, so a
// viewer who never chose follows the OS. Stored per browser
// in localStorage, never on the account: there is no settings surface yet and
// a per-account preference is a server change, not a tokens change. With no
// stored choice the theme FOLLOWS THE OS ("system"); the server's inline
// pre-paint script (webui/document.go) resolves the same rule before first
// paint, so there is no flash in either direction.
export type ThemeChoice = "dark" | "light" | "system";
export type Theme = "dark" | "light";

export const THEME_STORAGE_KEY = "oap.theme";

export function readThemeChoice(): ThemeChoice {
  try {
    const v = window.localStorage.getItem(THEME_STORAGE_KEY);
    return v === "light" || v === "system" || v === "dark" ? v : "system";
  } catch {
    return "system";
  }
}

export function resolveTheme(choice: ThemeChoice): Theme {
  if (choice !== "system") return choice;
  try {
    return window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
  } catch {
    return "dark";
  }
}

// applyTheme writes the ONE attribute tokens.css and styles.css switch on.
export function applyTheme(theme: Theme): void {
  document.documentElement.dataset.theme = theme;
}

export function setThemeChoice(choice: ThemeChoice): Theme {
  try {
    window.localStorage.setItem(THEME_STORAGE_KEY, choice);
  } catch {
    // Private mode or blocked storage: the choice still applies for this page.
  }
  const theme = resolveTheme(choice);
  applyTheme(theme);
  return theme;
}

// applyStoredTheme is called once at app entry. It also follows the OS while
// the choice is "system", and returns the unsubscribe so a test can clean up.
export function applyStoredTheme(): () => void {
  const choice = readThemeChoice();
  applyTheme(resolveTheme(choice));
  if (choice !== "system") return () => {};
  try {
    const mq = window.matchMedia("(prefers-color-scheme: light)");
    const onChange = () => applyTheme(mq.matches ? "light" : "dark");
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  } catch {
    return () => {};
  }
}
