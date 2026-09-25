// pageLayout.ts holds the page-layout context on its own, importing nothing
// from hooks.tsx, tree.ts, registry.tsx or page.tsx. Those four form a
// natural cycle otherwise: page.tsx wants GENERATIVE from hooks.tsx and
// timelineSteps from tree.ts, while hooks.tsx (GenerativeHook's fold) and
// registry.tsx (ap:steps' rail) both want the layout context page.tsx would
// otherwise own. Carving the context out to a leaf module — react and
// ./types only — breaks the cycle without moving GENERATIVE or timelineSteps
// out of the files that already document them.
import * as React from "react";
import type { Node } from "./types";

export type PageLayout = "column" | "rail";

export interface PageLayoutState {
  layout: PageLayout;
  selectedStep: string | null;
  selectStep: (id: string) => void;
}

// NO_PAGE is the context default: no page root, so no rail and no selection.
const NO_PAGE: PageLayoutState = {
  layout: "column",
  selectedStep: null,
  selectStep: () => {},
};

export const PageLayoutContext = React.createContext<PageLayoutState>(NO_PAGE);

// usePageLayout tells a renderer which layout it is inside. ap:steps draws
// itself as a rail under "rail"; GenerativeHook skips folding under "rail"
// (the rail is the fold); AgentUIView widens its root for a rail page.
export function usePageLayout(): PageLayoutState {
  return React.useContext(PageLayoutContext);
}

// pageLayoutOf reads a declaration's layout from its root node, for callers
// outside the rendered tree (the view root's width). "column" for no root.
export function pageLayoutOf(view: Node | undefined): PageLayout {
  return view?.component === "oap:page" && view.props?.layout === "rail"
    ? "rail"
    : "column";
}
