// paramsUrl.ts is the one place binding parameters are encoded into, and read
// back out of, a page's query string.
//
// It exists because binding parameters used to live ONLY in React state inside
// the view component. That made a filter selection strictly ephemeral: it did
// not survive a reload, it did not survive switching to the transcript and
// back, and a filtered console could not be sent to anyone — the address bar
// described the session, never what the viewer had actually filtered it to.
// The complaint that surfaced it was "I selected dates... now what?" followed
// by the fields being empty again a moment later.
//
// The query string is the persistence layer rather than storage because it is
// the only one that also answers the sharing question: a link now carries the
// view someone is looking at, not merely which session they had open.

// PARAM_QUERY_PREFIX namespaces every binding parameter in the query string.
//
// It is NOT decoration. The shell owns its own keys in the same query string
// (`session`, `view`, `new`), and a declaration is free to name a parameter
// anything — including "view" or "session". Without a namespace, an author who
// named a date filter `view` would silently drive the shell's view switch
// instead of their own binding, and the collision would look like a bug in the
// binding rather than in the naming.
export const PARAM_QUERY_PREFIX = "p.";

// paramsFromSearch reads every namespaced parameter out of a query string.
//
// Unknown keys are returned as-is rather than filtered against a declaration:
// this function does not know one, and the caller (reconcileParams) already
// drops keys the current declaration does not declare. Filtering twice, in two
// places, is how the two answers drift.
export function paramsFromSearch(search: string): Record<string, string> {
  const out: Record<string, string> = {};
  const sp = new URLSearchParams(search);
  sp.forEach((value, key) => {
    if (!key.startsWith(PARAM_QUERY_PREFIX)) return;
    const name = key.slice(PARAM_QUERY_PREFIX.length);
    // A bare "p." carries no parameter name. Keeping it would put an
    // empty-string key in the map, which reconcileParams would then try to
    // match against a declared key that can never be empty.
    if (name === "") return;
    out[name] = value;
  });
  return out;
}

// searchWithParams returns `search` with its namespaced parameters REPLACED by
// `params`, leaving every other key untouched and in place.
//
// Replace, not merge: a parameter the viewer cleared, or one a rewritten
// declaration no longer declares, has to leave the URL. Merging would let a
// stale key sit in the address forever, and the next reader would seed state
// from a filter the page no longer has a control for.
//
// Non-parameter keys are preserved because the shell's own state lives beside
// these — dropping `session` or `view` while writing a filter would navigate
// the page as a side effect of picking a date.
export function searchWithParams(
  search: string,
  params: Record<string, string>,
): string {
  const sp = new URLSearchParams(search);
  for (const key of Array.from(sp.keys())) {
    if (key.startsWith(PARAM_QUERY_PREFIX)) sp.delete(key);
  }
  // Sorted so the same parameter map always produces the same string. Without
  // it, key order would follow insertion order and two identical views could
  // differ in the address bar — which matters here because the caller compares
  // the built string against the current one to decide whether to touch
  // history at all.
  for (const name of Object.keys(params).sort()) {
    sp.set(PARAM_QUERY_PREFIX + name, params[name]);
  }
  const s = sp.toString();
  return s === "" ? "" : `?${s}`;
}
