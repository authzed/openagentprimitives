// page.tsx renders oap:page, the page's layout root. Under layout="rail" the
// hook that holds the page's one ap:steps becomes a sticky rail on the left
// and the remaining children are the stage; a hook bound to a step shows in
// the stage only while that step is selected (the active step by default, the
// last finished one once nothing is active, the first one while nothing has
// started, or the one the person clicked), and a hook with no step always
// shows.
//
// A hook the page is WAITING on is the exception: it is staged whatever its
// step says. The view suppresses its own reply modal whenever any hook in the
// DECLARED tree holds a question, so a question painted into a hook the stage
// filtered out would be neither on screen nor answerable anywhere else.
//
// The stage filter reads only the DIRECT children of oap:page — a hook nested
// inside another container is not bound-filtered and shows wherever its
// container does — so the hooks of a rail page belong at its top level.
//
// Under layout="column" — or with no page root at all — none of that is
// involved: the children render in a column exactly as they always have.
//
// The selection is view state the browser owns: the agent decides which step
// is DEFAULT by repainting the timeline; the person decides which step they
// are LOOKING at. When the default step changes, the selection follows it, so
// advancing the timeline — or finishing it — always shows the new phase.
//
// The layout context itself lives in pageLayout.ts, not here — see that
// file's header for why (the import cycle this module would otherwise sit
// in the middle of).
import * as React from "react";
import { GENERATIVE, hookNameOf, useHookState } from "./hooks";
import {
  type PageLayout,
  PageLayoutContext,
  type PageLayoutState,
} from "./pageLayout";
import { timelineSteps } from "./tree";
import type { Node } from "./types";

function containsComponent(n: Node, component: string): boolean {
  if (n.component === component) return true;
  return (n.children ?? []).some((c) => containsComponent(c, component));
}

function boundStep(n: Node): string | null {
  const step = n.component === GENERATIVE ? n.props?.step : undefined;
  return typeof step === "string" && step !== "" ? step : null;
}

export function PageNode({
  node,
  renderChild,
}: {
  node: Node;
  renderChild: (n: Node, key: React.Key) => React.ReactElement;
}): React.ReactElement {
  const layout: PageLayout = node.props?.layout === "rail" ? "rail" : "column";
  const children = node.children ?? [];
  // PageNode is mounted by React inside HookStateProvider, so it reads the
  // page's own state the way every hook in the tree does.
  const { waiting } = useHookState();
  const steps = React.useMemo(() => timelineSteps(node), [node]);
  // The default selection is the active step; when nothing is active — every
  // step done, the end of the flow — it is the last finished one, so the stage
  // keeps showing the outcome rather than emptying until the person clicks;
  // and when nothing is active OR done — a timeline painted but not started —
  // it is the first step, for the same reason.
  const defaultStep = React.useMemo(() => {
    const active = steps.find((s) => s.state === "active");
    if (active) return active.id;
    const done = steps.filter((s) => s.state === "done");
    if (done.length > 0) return done[done.length - 1].id;
    return steps.length > 0 ? steps[0].id : null;
  }, [steps]);
  const [picked, setPicked] = React.useState<string | null>(null);
  // Follow the default step whenever the timeline advances: a pick is only
  // ever an excursion from the current phase, never a way to miss the next.
  React.useEffect(() => {
    setPicked(null);
  }, [defaultStep]);
  const selectedStep = picked ?? defaultStep;
  const state = React.useMemo<PageLayoutState>(
    () => ({ layout, selectedStep, selectStep: setPicked }),
    [layout, selectedStep],
  );

  if (layout !== "rail") {
    return (
      <PageLayoutContext.Provider value={state}>
        <div className="flex flex-col gap-2">{children.map(renderChild)}</div>
      </PageLayoutContext.Provider>
    );
  }

  const railIndex = children.findIndex((c) => containsComponent(c, "ap:steps"));
  const rail = railIndex >= 0 ? children[railIndex] : null;
  const onStage = (c: Node, i: number): boolean => {
    if (i === railIndex) return false;
    const step = boundStep(c);
    if (step === null || step === selectedStep) return true;
    const name = hookNameOf(c);
    return name !== null && waiting.has(name);
  };
  return (
    <PageLayoutContext.Provider value={state}>
      <div className="grid gap-6 md:grid-cols-[220px_minmax(0,1fr)]">
        <div
          data-testid="agent-ui-page-rail"
          className="md:sticky md:top-4 md:self-start"
        >
          {rail ? renderChild(rail, "rail") : null}
        </div>
        <div
          data-testid="agent-ui-page-stage"
          className="flex min-w-0 flex-col gap-3"
        >
          {/* Keyed by a child's ORIGINAL index, never by its position in the
              staged subset: React reconciles by key, so a key that moved with
              the selection would hand one step's component instances — a
              fold's open state, a form's typed values — to the next step's. */}
          {children.map((c, i) => (onStage(c, i) ? renderChild(c, i) : null))}
        </div>
      </div>
    </PageLayoutContext.Provider>
  );
}
