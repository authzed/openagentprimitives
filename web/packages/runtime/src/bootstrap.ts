// readBootstrap parses the server-injected props from the non-executable
// <script type="application/json" id="ap-bootstrap"> element. Returns {} when
// the element is absent (e.g. an error page with no props).
export function readBootstrap<T = Record<string, unknown>>(): T {
  const el = document.getElementById("ap-bootstrap");
  if (!el || !el.textContent) return {} as T;
  try {
    return JSON.parse(el.textContent) as T;
  } catch {
    return {} as T;
  }
}
