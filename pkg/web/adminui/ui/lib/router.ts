// Client router for the admin SPA. Real URL paths under the base `/admin`
// (NOT hash routing): deep-linkable, native back/forward via history.pushState
// + popstate. No external router lib. SSR/jsdom-safe (guards `window`).
import { useSyncExternalStore } from "react";
import { NAV, type ViewId } from "../shell/nav";

// Base path the whole admin app is mounted under (webd serves it here).
const BASE = "/admin";

// EntityKind ids are SINGULAR by design: `/admin/agent/x` (detail) is thereby
// distinct from `/admin/agents` (the plural list nav id).
export type EntityKind =
  | "session"
  | "agent"
  | "tool"
  | "skill"
  | "source"
  | "directory"
  | "channel"
  | "identity"
  | "provider"
  | "artifact"
  | "user";

export type Route =
  | { type: "view"; view: ViewId; query?: URLSearchParams }
  | { type: "detail"; entity: EntityKind; id: string; tab?: string };

// Known view ids, derived from NAV so this stays in sync automatically as views
// (e.g. a future "budget") are added to the nav — no second list to maintain.
const VIEW_IDS: ReadonlySet<string> = new Set(
  NAV.flatMap((g) => g.items.map((i) => i.id as string)),
);

// The nav view a detail entity lives under (used for the sidebar highlight and
// the parent-list back affordance). Keys are exhaustive over EntityKind.
const ENTITY_TO_VIEW: Record<EntityKind, ViewId> = {
  session: "sessions",
  agent: "agents",
  tool: "tools",
  skill: "skills",
  source: "sources",
  directory: "directory",
  channel: "channels",
  identity: "identity",
  provider: "identity",
  artifact: "artifacts",
  user: "users",
};

// ALL_ENTITY_KINDS is every EntityKind, derived from ENTITY_TO_VIEW rather
// than listed a second time. Exported so a guard test can walk the complete set
// (e.g. asserting AdminApp's detailContent has a real page for each) without
// transcribing a list that would silently fall behind a new kind.
export const ALL_ENTITY_KINDS: readonly EntityKind[] = Object.keys(ENTITY_TO_VIEW) as EntityKind[];

const ENTITY_KINDS: ReadonlySet<string> = new Set(ALL_ENTITY_KINDS);

export function isEntityKind(seg: string): seg is EntityKind {
  return ENTITY_KINDS.has(seg);
}

const OVERVIEW: Route = { type: "view", view: "overview" };

// parseLocation maps a pathname (+ raw search string like "?tab=activity") to a
// Route. Rules:
//   /admin or /admin/                     → overview view
//   /admin/<viewId>                       → that list/config view
//   /admin/<entity>/<id…>                 → detail (id = remaining segments)
//   anything else                         → overview (fail-safe)
export function parseLocation(pathname: string, search = ""): Route {
  const rest = pathname.startsWith(BASE) ? pathname.slice(BASE.length) : pathname;
  const segments = rest.split("/").filter(Boolean);
  if (segments.length === 0) return OVERVIEW;

  const [first, ...tail] = segments;

  if (segments.length === 1 && VIEW_IDS.has(first)) {
    const view = first as ViewId;
    const params = new URLSearchParams(search);
    return [...params].length > 0 ? { type: "view", view, query: params } : { type: "view", view };
  }

  if (isEntityKind(first) && tail.length > 0) {
    const id = tail.join("/");
    const tab = new URLSearchParams(search).get("tab") ?? undefined;
    return tab ? { type: "detail", entity: first, id, tab } : { type: "detail", entity: first, id };
  }

  return OVERVIEW;
}

// buildPath is the inverse of parseLocation (returns a `/admin/…` path).
export function buildPath(route: Route): string {
  if (route.type === "detail") {
    const base = `${BASE}/${route.entity}/${route.id}`;
    return route.tab ? `${base}?tab=${encodeURIComponent(route.tab)}` : base;
  }
  const qs = route.query ? route.query.toString() : "";
  if (route.view === "overview" && qs === "") return BASE;
  const path = `${BASE}/${route.view}`;
  return qs ? `${path}?${qs}` : path;
}

// viewForRoute is the nav view a route belongs to: itself for a view route, or
// the entity's parent list for a detail route (an agent detail highlights
// "agents").
export function viewForRoute(route: Route): ViewId {
  return route.type === "view" ? route.view : ENTITY_TO_VIEW[route.entity];
}

// ── Tiny external store: notifies subscribers on navigate() and popstate ──
const listeners = new Set<() => void>();

function emit(): void {
  for (const l of listeners) l();
}

function subscribe(onChange: () => void): () => void {
  listeners.add(onChange);
  if (typeof window !== "undefined") window.addEventListener("popstate", onChange);
  return () => {
    listeners.delete(onChange);
    if (typeof window !== "undefined") window.removeEventListener("popstate", onChange);
  };
}

// getSnapshot returns a stable primitive (pathname+search) so useSyncExternal
// Store never loops; the Route object is derived from it per render.
function getSnapshot(): string {
  if (typeof window === "undefined") return BASE;
  return window.location.pathname + window.location.search;
}

function getServerSnapshot(): string {
  return BASE;
}

// navigate pushes a new history entry for `route` and notifies subscribers.
export function navigate(route: Route): void {
  if (typeof window === "undefined") return;
  window.history.pushState({}, "", buildPath(route));
  emit();
}

// useRoute subscribes to the location store and returns the current Route.
export function useRoute(): Route {
  const key = useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
  const qIndex = key.indexOf("?");
  const pathname = qIndex >= 0 ? key.slice(0, qIndex) : key;
  const search = qIndex >= 0 ? key.slice(qIndex) : "";
  return parseLocation(pathname, search);
}
