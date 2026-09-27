// hooks.tsx renders the ONE structural type in the vocabulary: oap:generative,
// the agent's writable region of the page. A hook is a node like any other —
// renderNode resolves it through COMPONENTS and recurses into its children —
// but what it draws around those children is the platform's, not the
// declaration's: the region's name, its composed marker, and the two live cues.
//
// The per-region state those cues read comes from a context rather than from
// the node, and that is the whole security argument (see COMPOSED_CLASS below):
// a node cannot carry a fact about itself that the page then trusts.
import * as React from "react";
import { Loader2 } from "lucide-react";
// House style: design-package components import cn via the package-qualified
// "@ap/design/lib/utils" subpath rather than a relative path — same reason
// registry.tsx does, and it resolves through the same tsconfig/vite path alias
// without going through the barrel export.
import { cn } from "@ap/design/lib/utils";
import { Disclosure } from "./disclosure";
import { usePageLayout } from "./pageLayout";
import type { Node } from "./types";

// GENERATIVE is the component type of a hook — uicomponents.GenerativeType's
// mirror. It lives in the oap: namespace, never ap:, precisely because the
// agent-facing schema publishes only ap: types: a fill can never name it.
export const GENERATIVE = "oap:generative";

// HookState is everything the page knows about its regions that a node cannot
// say for itself. Each field is keyed by hook NAME, which is the identity the
// wire, the DOM (data-hook) and update_view all agree on.
export interface HookState {
  // composed: hook names the server says the agent wrote — fills AND clears.
  // A cleared hook is still composed: "the agent wrote this region, and what it
  // wrote was nothing" differs from "the author left this region alone", and
  // only the mark distinguishes them.
  composed: ReadonlySet<string>;
  // updated: hook name → a monotonic tick of its latest update_view. The value
  // is the tick rather than a boolean so a SECOND update to a region already
  // showing the cue is still a change the view can key an effect on; presence
  // alone is what shows the cue.
  updated: ReadonlyMap<string, number>;
  // stale: regions whose bindings are re-resolving. Keyed by region, so "" —
  // the region outside every hook — is a legal member; no hook answers to it,
  // and the view draws that one itself.
  stale: ReadonlySet<string>;
  // waiting: hook names the PAGE has decided need a viewer's attention right
  // now — computed from the tree (a hook whose subtree holds an ap:question),
  // never claimed by a node about itself. See tree.ts's hooksContaining,
  // which is what a view derives this set from.
  waiting: ReadonlySet<string>;
  // answered: hook names the SERVER says the viewer already replied to —
  // derived from the fill's write time and the transcript, never something a
  // node or a fragment claims about itself. question.tsx reads it (via
  // useEnclosingHook, below) to render an ap:question as already sent rather
  // than asking again after a reload.
  answered: ReadonlySet<string>;
  // collapsed: hook name → the fold's title, for hooks the PAGE has decided
  // are done with — bound to a timeline step whose state is done. Computed
  // from the tree by the view (tree.ts's stepBindings), never claimed by a
  // node; the person's toggle wins inside the fold until the step changes.
  collapsed: ReadonlyMap<string, string>;
}

// EMPTY_HOOK_STATE is the context default, so this package renders standalone
// with no provider: a hook outside a view is simply a region nothing has been
// said about. It is frozen-by-construction (empty Set/Map literals shared by
// every consumer) and must never be mutated.
export const EMPTY_HOOK_STATE: HookState = {
  composed: new Set<string>(),
  updated: new Map<string, number>(),
  stale: new Set<string>(),
  waiting: new Set<string>(),
  answered: new Set<string>(),
  collapsed: new Map<string, string>(),
};

const HookStateContext = React.createContext<HookState>(EMPTY_HOOK_STATE);

export function HookStateProvider({
  value,
  children,
}: {
  value: HookState;
  children: React.ReactNode;
}): React.ReactElement {
  return (
    <HookStateContext.Provider value={value}>
      {children}
    </HookStateContext.Provider>
  );
}

export function useHookState(): HookState {
  return React.useContext(HookStateContext);
}

// hookNameOf answers "is this node a hook, and what region does it open?" — the
// browser's mirror of uicomponents.hookOf. It is the one place the question is
// asked, so the region rule in bindings.ts and the renderer here can never
// disagree about which nodes are hooks.
//
// null means "not a hook", and ONLY that. A hook whose name prop is missing or
// not a string opens the region "" — the same answer uicomponents.hookOf gives,
// which reads the zero value when the prop does not decode and leaves the
// rejection to Validate. Reading a nameless hook as "not a hook" instead would
// make the two walks disagree about the shape of the tree: the server would
// number that subtree from zero in its own region while the browser kept
// counting from the ancestor, so every binding under it would look up a key the
// server never wrote and stick on its declared placeholder, on a clean 200.
// Such a page cannot reach a browser through admission — but the walks have to
// agree on every tree, not only the admitted ones, because that agreement is
// what makes admission the single place the rejection lives.
export function hookNameOf(n: Node): string | null {
  if (n.component !== GENERATIVE) return null;
  return typeof n.props?.name === "string" ? n.props.name : "";
}

// COMPOSED_CLASS carries the trust signal the design spec files beside
// platform-rendered approval chrome: "agent-composed regions carry a persistent
// unsuppressible marker". The marker is the REGION'S OWN APPEARANCE and nothing
// else — a wash on the region's own surface, with padding of its own and no
// edge or rule, so nothing pushes the content sideways. It stands alone: the
// view renders no legend naming what the wash means; the contrast with an
// unwashed declared region is what carries it.
//
// A region-level treatment answers "who wrote this?" at a glance and at any
// distance, and says it about the whole region rather than about its first
// line — which a chip inside the region could not, since it added a row of
// furniture to every agent-written block and had to be read one at a time on a
// page mixing agent prose with platform-resolved tables and viewer-driven
// controls.
//
// What the claim rests on, precisely, so no comment here overstates it:
//
//   - It is applied to GenerativeHook's OWN <section> — the same
//     platform-emitted element that carries data-hook — never to anything
//     renderNode produced from the hook's children. The hook node is
//     STRUCTURAL: uicomponents publishes only ap: types to the agent, and
//     ResolveView refuses an oap:generative inside a fill outright, so a
//     region can never be authored by the thing it describes.
//   - It is driven by the server-computed agentComposed list, which arrives as
//     a top-level array of hook NAMES and reaches this component as
//     HookState.composed — never as a field on a node. The Go validator rejects
//     any structural key it does not know on a node, so a fragment has no way
//     to set or clear its own mark.
//   - The vocabulary exposes no style, class, or CSS prop, and ap:raw_html
//     renders in a sandbox-origin iframe without allow-same-origin, so a
//     declared node has no route to the host document's styles either.
//
// data-agent-composed is emitted alongside the classes so the fact stays
// legible to tests and to anyone inspecting the DOM without having to decode a
// Tailwind class string.
//
// Regions the agent has NOT written stay deliberately unstyled — the default
// reading of a page is "author- or viewer-owned", and marking is the exception
// that has to earn attention.
const COMPOSED_CLASS = "rounded-md bg-state/[0.05] px-3 py-2";

// EnclosingHook carries the NAME of the hook a component is rendering inside,
// so a component can read its own enclosing region without any node ever
// naming itself — the same "the page knows, the node cannot claim" split
// HookState's other fields already keep. GenerativeHook is the only
// provider; useEnclosingHook is the only reader.
const EnclosingHook = React.createContext<string | null>(null);

// useEnclosingHook returns the name of the hook the calling component is
// rendered inside, or null outside every hook. question.tsx's ap:question
// uses it to look itself up in HookState.answered — the only way it can
// know "was I already answered?" without a node claiming that fact about
// itself.
export function useEnclosingHook(): string | null {
  return React.useContext(EnclosingHook);
}

// GenerativeHook draws one region: its children in place, plus whatever the
// page's HookState says about it.
//
// An EMPTY hook still renders its <section>. A region the agent has yet to
// write, or has just cleared, is present and addressable — the element is what
// data-hook names and what the composed mark hangs on — it simply shows
// nothing, so it costs the page no space.
//
// A hook bound to a timeline step the page has finished is folded shut behind
// a Disclosure (the same folding container ap:collapsible uses), titled by
// the step's own label — only when it has content: folding an empty region
// would show a header over nothing to hide.
//
// Under a RAIL layout none of that folding happens: the page stages a hook by
// its step and shows it open, so the fold's title would never be drawn. The
// hook's own `title`, when the author gave one, is a heading over its content
// instead — otherwise an agent-written region reaches the stage as unlabelled
// prose.
export function GenerativeHook({
  name,
  title,
  children,
}: {
  name: string;
  title?: string;
  children?: React.ReactNode;
}): React.ReactElement {
  const { composed, updated, stale, waiting, collapsed } = useHookState();
  const { layout } = usePageLayout();
  const isComposed = composed.has(name);
  const isUpdated = updated.has(name);
  const isStale = stale.has(name);
  const isWaiting = waiting.has(name);
  const hasContent = React.Children.count(children) > 0;
  const foldTitle = collapsed.get(name);
  // Under a rail layout the rail IS the fold: a finished step's hook is hidden
  // by the page until its step is selected, and shown open when it is.
  const isCollapsed =
    layout !== "rail" && foldTitle !== undefined && hasContent;
  // A heading needs both a title and something to head, for the same reason
  // the fold needs content: a header over nothing is chrome the region has no
  // use for.
  const showTitle =
    layout === "rail" && title !== undefined && title !== "" && hasContent;
  // Under a rail the chip is redundant: the staged card IS the ask — a hook
  // the agent is waiting on is the one the page stages — so a label saying so
  // repeats what the person is already reading. The attribute is unaffected;
  // it is the machine-readable fact, and the page's staging reads it.
  const showWaiting = isWaiting && layout !== "rail";
  return (
    <section
      data-hook={name}
      data-agent-composed={isComposed ? "true" : undefined}
      data-hook-updated={isUpdated ? "true" : undefined}
      data-hook-waiting={isWaiting ? "true" : undefined}
      data-hook-collapsed={isCollapsed ? "true" : undefined}
      // A region rewritten under the viewer's eyes is an update a screen reader
      // must hear about; polite, not assertive, because it never interrupts
      // what the viewer is doing.
      aria-live="polite"
      aria-busy={isStale || undefined}
      // The wash covers CONTENT. A hook the agent cleared is still composed
      // — the attribute above says so — but washing nothing paints a stray
      // tinted box on the page, so the wash needs content as well as the
      // mark.
      className={cn(
        "relative w-full",
        isComposed && hasContent && COMPOSED_CLASS,
        isStale && "opacity-60 transition-opacity",
      )}
    >
      {(showWaiting || isUpdated || isStale) && (
        // The cues sit ABOVE the content, in flow, so they reserve their own
        // line rather than being drawn over the region's first line. Waiting
        // takes the left, updated/refreshing the right: a hook holding a live
        // ap:question is routinely also the hook that just composed it, so
        // "waiting" and "updated" are often true at once and must never hide
        // one another. pointer-events-none so a cue can never eat a click
        // meant for a control beneath it.
        <div
          data-testid="agent-ui-hook-cues"
          className="pointer-events-none mb-1 flex min-h-5 items-center justify-between gap-2 text-[11px]"
        >
          <span>
            {showWaiting && (
              <span
                data-testid="agent-ui-hook-waiting"
                className="rounded bg-accent/10 px-1.5 py-0.5 font-medium text-accent-foreground"
              >
                Waiting on you
              </span>
            )}
          </span>
          {isUpdated ? (
            <span
              data-testid="agent-ui-hook-updated"
              className="rounded bg-accent/10 px-1.5 py-0.5 text-muted-foreground"
            >
              Updated
            </span>
          ) : isStale ? (
            // Small and to the side: the content stays readable and in place,
            // which is the point — the previous answer is still the best
            // available one until the next arrives. It yields to the updated
            // cue, which reports the newer, larger fact.
            <div
              data-testid="agent-ui-hook-refreshing"
              className="flex items-center gap-1.5 text-muted-foreground"
            >
              <Loader2
                className="h-3.5 w-3.5 animate-spin"
                aria-hidden="true"
              />
              <span>Updating…</span>
            </div>
          ) : null}
        </div>
      )}
      <EnclosingHook.Provider value={name}>
        {showTitle && (
          <h3
            data-testid="agent-ui-hook-title"
            className="mb-2 text-sm font-medium text-foreground"
          >
            {title}
          </h3>
        )}
        {isCollapsed ? (
          // The page closed this region because its phase ended. Always
          // `collapsed`: a hook leaves this map — and this fold — when its
          // step is no longer done, so the person's reopen survives exactly
          // until the page has a new reason to say otherwise.
          <Disclosure
            title={foldTitle}
            collapsed
            testId="agent-ui-hook-collapsed"
          >
            {children}
          </Disclosure>
        ) : (
          children
        )}
      </EnclosingHook.Provider>
    </section>
  );
}
