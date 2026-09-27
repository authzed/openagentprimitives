import * as React from "react";
import { Alert, AlertDescription, AlertTitle } from "@ap/design";
import { COMPONENTS } from "./registry";
import type { Node } from "./types";

// UnknownComponent is the renderer's fail-visible path. The server validates
// every declaration before it reaches the browser, so an unknown type here
// means version skew between a running tab and a redeployed server — rendering
// nothing would silently blank part of the page, which reads as "the agent did
// nothing" rather than "this client is stale".
function UnknownComponent({ type }: { type: string }) {
  return (
    <Alert variant="destructive">
      <AlertTitle>Cannot render this section</AlertTitle>
      <AlertDescription>
        Unknown component <code className="font-mono">{type}</code>. Reload the
        page; if it persists, this view needs an update.
      </AlertDescription>
    </Alert>
  );
}

// FailedComponent is the renderer's second fail-visible path, for a node whose
// TYPE is known but whose props are the wrong SHAPE — an ap:table with
// `columns` as an object, an ap:chart with a non-array `series`. Those are the
// same version-skew class UnknownComponent covers (a tab running an older
// bundle than the server that validated the declaration), and prop shapes skew
// at least as readily as type names do.
//
// It exists because every renderer in COMPONENTS is invoked EAGERLY, at the
// point renderNode reaches its node. A TypeError inside one of them (calling
// .map on a non-array) therefore propagates synchronously up through every
// ancestor's own eager invocation and out of the top-level render — blanking
// the WHOLE page over one malformed node. That is strictly worse than the
// failure UnknownComponent exists to prevent: not "this section reads as if the
// agent did nothing", but "the entire view reads that way".
function FailedComponent({ type }: { type: string }) {
  return (
    <Alert variant="destructive">
      <AlertTitle>Cannot render this section</AlertTitle>
      <AlertDescription>
        The <code className="font-mono">{type}</code> section could not be
        displayed. The rest of this view is unaffected.
      </AlertDescription>
    </Alert>
  );
}

// RenderErrorSink is told the component type of every node this module could
// not render, synchronously, as that node is reached. The fail-visible card is
// returned either way — the sink is an ADDITIONAL channel, not a replacement
// for it.
//
// It exists because a failure has to leave this module as data, not only as an
// element. The platform chrome wrapping a rendered declaration must auto-reveal
// on a hard error (the agent-defined-UI design's chrome-authority decision), and
// the component that owns that reveal is a sibling of the content region, not
// an ancestor of these elements — it can neither catch an error from here (the
// catch below already contains it, deliberately) nor sniff the rendered DOM for
// an error card without coupling itself to this module's markup.
export type RenderErrorSink = (component: string) => void;

// childRenderer builds the exact (child, key) => ReactElement callback Renderer
// expects, closing over the error sink so a failure anywhere in a container's
// subtree still reaches the caller that passed it in.
//
// It is a factory rather than a bare reference to renderNode because
// renderNode's third parameter is that sink: Array.prototype.map always calls
// its callback with (element, index, array), so a container writing
// `.map(renderNode)` would pass the whole children ARRAY as the sink, with
// nothing failing at compile time or runtime. The callback returned here is
// fixed at two parameters, so it absorbs that risk on every container
// renderer's behalf instead of each one depending on renderNode's exact arity.
const childRenderer =
  (onError?: RenderErrorSink) =>
  (child: Node, key: React.Key): React.ReactElement =>
    renderNode(child, key, onError);

// renderNode turns one declaration node into React. Recursion lives here and
// nowhere else: registry.tsx's container renderers receive a child-renderer as
// their second argument, so the two modules never import each other.
export function renderNode(
  n: Node,
  key?: React.Key,
  onError?: RenderErrorSink,
): React.ReactElement {
  const renderer = COMPONENTS[n.component];
  if (!renderer) {
    onError?.(n.component);
    return <UnknownComponent key={key} type={n.component} />;
  }
  try {
    return React.cloneElement(renderer(n, childRenderer(onError)), { key });
  } catch (err) {
    // Logged, not swallowed: the visible fallback tells the USER a section is
    // missing, but only this line tells whoever is debugging WHICH node and
    // why. A React error boundary would not do here — boundaries catch during
    // React's own render of a mounted component, and these renderers run
    // eagerly inside this function, before React ever sees the element.
    //
    // The failure is contained to ONE node: this catch sits inside the
    // recursion, so an ancestor's `.map(renderChild)` gets a real element back
    // for the bad child and keeps rendering its remaining siblings.
    console.error(`agentui: failed to render ${n.component}`, err);
    onError?.(n.component);
    return <FailedComponent key={key} type={n.component} />;
  }
}
