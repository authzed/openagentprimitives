// A terminal Story is a fixed shell prompt plus an ordered list of beats. A beat
// drives the terminal through the async TermControl — typing a command char by
// char, or emitting colored output over time — and carries a caption (the
// narration line the capture overlay shows while it plays). Same model as
// slacksim, but the actions are terminal I/O rather than Slack mutations.

// TermControl is the async surface a beat uses to drive the terminal. Everything
// is a Promise so the capture engine's `await run(i)` spans the whole animation
// (page.evaluate awaits the returned promise), and the beat's `hold` is extra
// dwell after it settles.
export interface TermControl {
  /** Show the shell prompt. */
  prompt: () => Promise<void>;
  /** Type text char by char, like a human at the keyboard. */
  type: (text: string, opts?: { cps?: number }) => Promise<void>;
  /** Print a line of (optionally ANSI-colored) output plus a newline. */
  line: (text?: string) => Promise<void>;
  /** Write raw text with no trailing newline. */
  write: (text: string) => Promise<void>;
  /** Press Enter. */
  enter: () => Promise<void>;
  /** Pause for ms. */
  wait: (ms: number) => Promise<void>;
  /** Clear the screen. */
  clear: () => Promise<void>;
  /** Set (or clear, with null) the narration caption overlay. */
  caption: (text: string | null) => void;
}

export interface TermBeat {
  id: string;
  caption?: string;
  /** Extra hold (ms) after the beat settles, for pacing a clip. */
  hold?: number;
  run: (c: TermControl) => Promise<void>;
}

export interface TermStory {
  /** The shell prompt string, e.g. "❯ ". */
  prompt?: string;
  cols?: number;
  rows?: number;
  beats: TermBeat[];
  /** Title-card caption shown at the start of a clip (and on its poster). */
  intro?: string;
}

// The runtime handle the capture engine binds to on window — identical shape to
// slacksim's, so clip.mjs / still.mjs drive both without knowing the difference.
export interface StoryRuntime {
  count: number;
  intro?: string;
  captions: (string | undefined)[];
  holds: (number | undefined)[];
  clicks: (string | undefined)[];
  ids: string[];
  run: (index: number) => Promise<void> | void;
  playTo: (index: number) => Promise<void>;
}

export function bindStory(story: TermStory, control: TermControl): void {
  const beats = story.beats;
  window.__showcaseStory = {
    count: beats.length,
    intro: story.intro,
    captions: beats.map((b) => b.caption),
    holds: beats.map((b) => b.hold),
    // No cursor-clicks in a terminal; the field exists only for engine parity.
    clicks: beats.map(() => undefined),
    ids: beats.map((b) => b.id),
    run: (i) => beats[i]?.run(control),
    playTo: async (i) => {
      for (let k = 0; k <= i && k < beats.length; k++) {
        await beats[k].run(control);
      }
    },
  };
}
