// EntityLink / ViewLink render REAL anchors (so middle-click + open-in-new-tab
// work) whose plain left-click is intercepted for client-side navigation. Any
// modifier click (or non-primary button) is left to the browser.
import type { MouseEvent, ReactNode } from "react";
import { buildPath, navigate, type EntityKind, type Route } from "./router";
import type { ViewId } from "../shell/nav";

// A plain left-click is button 0 with no modifier keys — the only click we take
// over. Modifier/middle clicks fall through so the real href behaves natively.
function isPlainLeftClick(e: MouseEvent): boolean {
  return e.button === 0 && !e.metaKey && !e.ctrlKey && !e.shiftKey && !e.altKey;
}

function RouteAnchor({
  route,
  className,
  onClick,
  children,
}: {
  route: Route;
  className?: string;
  // onClick runs before the client-side navigation — used by callers nested
  // inside a clickable row to stopPropagation so the row's own handler (a
  // drawer/overlay) doesn't also fire. Navigation still happens.
  onClick?: (e: MouseEvent) => void;
  children: ReactNode;
}) {
  const href = buildPath(route);
  return (
    <a
      href={href}
      className={className}
      onClick={(e) => {
        onClick?.(e);
        if (!isPlainLeftClick(e)) return;
        e.preventDefault();
        navigate(route);
      }}
    >
      {children}
    </a>
  );
}

export function EntityLink({
  entity,
  id,
  tab,
  className,
  onClick,
  children,
}: {
  entity: EntityKind;
  id: string;
  tab?: string;
  className?: string;
  onClick?: (e: MouseEvent) => void;
  children: ReactNode;
}) {
  return (
    <RouteAnchor route={{ type: "detail", entity, id, ...(tab ? { tab } : {}) }} className={className} onClick={onClick}>
      {children}
    </RouteAnchor>
  );
}

export function ViewLink({
  view,
  query,
  className,
  children,
}: {
  view: ViewId;
  query?: URLSearchParams;
  className?: string;
  children: ReactNode;
}) {
  return (
    <RouteAnchor route={{ type: "view", view, ...(query ? { query } : {}) }} className={className}>
      {children}
    </RouteAnchor>
  );
}
