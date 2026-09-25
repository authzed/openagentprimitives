import * as React from "react";

// Typed cookie store for admin view preferences (grid/table toggles, status
// filters, …). Every admin cookie is namespaced with the ap_admin_ prefix so it
// never collides with app cookies. All access is document-guarded, so the module
// is import-safe under SSR / jsdom where `document` may be absent — a missing DOM
// degrades to null/no-op, never a throw.

const PREFIX = "ap_admin_";

function hasDocument(): boolean {
  return typeof document !== "undefined";
}

// getCookie returns the decoded value of the ap_admin_<name> cookie, or null when
// it is unset (or there is no document).
export function getCookie(name: string): string | null {
  if (!hasDocument()) return null;
  const target = PREFIX + name + "=";
  for (const part of document.cookie.split(";")) {
    const c = part.trimStart();
    if (c.startsWith(target)) {
      return decodeURIComponent(c.slice(target.length));
    }
  }
  return null;
}

// setCookie writes the ap_admin_<name> cookie. It persists for `days` (default
// 365, path=/, SameSite=Lax); pass days<=0 for a session cookie.
export function setCookie(name: string, value: string, opts?: { days?: number }): void {
  if (!hasDocument()) return;
  let cookie = `${PREFIX + name}=${encodeURIComponent(value)}; path=/; SameSite=Lax`;
  const days = opts?.days ?? 365;
  if (days > 0) {
    cookie += `; Expires=${new Date(Date.now() + days * 864e5).toUTCString()}`;
  }
  document.cookie = cookie;
}

// removeCookie expires the ap_admin_<name> cookie immediately.
export function removeCookie(name: string): void {
  if (!hasDocument()) return;
  document.cookie = `${PREFIX + name}=; path=/; Max-Age=0; SameSite=Lax`;
}

// decodeValue tolerates values persisted as a JSON string (e.g. '"grid"') as well
// as plain strings, so a cookie written by either path reads back cleanly.
function decodeValue<T extends string>(raw: string): T {
  if (raw.length > 0 && raw[0] === '"') {
    try {
      const parsed: unknown = JSON.parse(raw);
      if (typeof parsed === "string") return parsed as T;
    } catch {
      /* not JSON — fall through to the raw string */
    }
  }
  return raw as T;
}

// useCookieState is a useState-shaped hook backed by an ap_admin_ cookie: it reads
// the persisted value on first render (falling back when unset) and writes through
// on every setter call. T is a string (typically a small literal union like
// "grid" | "table").
export function useCookieState<T extends string>(
  name: string,
  fallback: T,
): [T, (v: T) => void] {
  const [value, setValue] = React.useState<T>(() => {
    const raw = getCookie(name);
    return raw === null ? fallback : decodeValue<T>(raw);
  });

  const set = React.useCallback(
    (v: T) => {
      setValue(v);
      setCookie(name, v);
    },
    [name],
  );

  return [value, set];
}
