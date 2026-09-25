// The two globals the capture engine (clip.mjs / still.mjs) drives every demo
// through. They are declared here, once, rather than in each demo.
//
// A `declare global` is program-wide, not module-scoped. Each demo used to
// declare these itself with its own richer types — slacksim's control also posts
// messages and opens threads, consolesim's story runtime is async — and that is
// harmless at runtime, where exactly one demo is loaded per page. Under a single
// tsconfig that compiles both, it is two declarations of one property with two
// types, which TypeScript rejects (TS2717).
//
// What is declared here is the ENGINE's contract, not the union of what the
// demos expose: the engine only sets captions and moves the pointer. A demo that
// binds something richer assigns it as a subtype and keeps its own type for its
// own use, which is what slacksim's ShowcaseControl does.

interface ShowcaseRect {
  x: number;
  y: number;
  width: number;
  height: number;
}

/** The overlay handle each demo binds as `window.__showcase`. */
interface ShowcaseOverlayControl {
  /** Move the pointer to viewport coords; `down` shows the click-press ring. */
  pointer: (x: number, y: number, down?: boolean) => void;
  hidePointer: () => void;
  /** Draw a highlight box around a rect, or clear it with null. */
  highlight: (rect: ShowcaseRect | null) => void;
  /** Set the narration caption text, or hide it with null. */
  caption: (text: string | null) => void;
}

/**
 * The beat runner each demo binds as `window.__showcaseStory`.
 *
 * `run` and `playTo` may be synchronous or asynchronous: slacksim's beats are
 * immediate store mutations, consolesim's type into a terminal over time. The
 * engine awaits both either way, so a demo is free to return void.
 */
interface ShowcaseStoryRuntime {
  count: number;
  intro?: string;
  captions: (string | undefined)[];
  holds: (number | undefined)[];
  clicks: (string | undefined)[];
  ids: string[];
  /** Run a single beat by index. */
  run: (index: number) => Promise<void> | void;
  /** Run beats [0, index] in order (for a still after beat `index`). */
  playTo: (index: number) => Promise<void> | void;
}

declare global {
  interface Window {
    __showcase?: ShowcaseOverlayControl;
    __showcaseStory?: ShowcaseStoryRuntime;
  }
}

export {};
