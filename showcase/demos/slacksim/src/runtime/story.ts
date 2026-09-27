import type { Scenario } from "../store/types";
import type { ShowcaseControl } from "../store/simstore";

// A Story is a scenario plus an ordered list of beats that advance it. A beat
// mutates the sim through the control surface (post a message, open a DM,
// resolve an approval) and carries an optional caption — the narration line the
// capture overlay shows while the beat plays. The same story yields both stills
// (capture after beat N) and a narrated clip (play every beat in order).
export interface StoryBeat {
  id: string;
  /** Narration/caption shown while this beat is on screen. */
  caption?: string;
  /** Extra hold (ms) after the beat's mutation, for pacing a clip. */
  hold?: number;
  /** A Block Kit button action_id to animate a cursor-click on before the beat
   *  runs (e.g. "approve", "userPassthrough"). The clip driver finds the visible
   *  button and clicks it; stills ignore this. */
  clickAction?: string;
  run: (c: ShowcaseControl) => void;
}

export interface Story {
  scenario: Scenario;
  beats?: StoryBeat[];
  /** The title-card caption shown at the start of a clip (and its poster). */
  intro?: string;
}

// The runtime handle the capture engine binds to on window, so it can play beats
// by index and read their captions without knowing the story internals.
export interface StoryRuntime {
  count: number;
  intro?: string;
  captions: (string | undefined)[];
  holds: (number | undefined)[];
  clicks: (string | undefined)[];
  ids: string[];
  /** Run a single beat by index. */
  run: (index: number) => void;
  /** Run beats [0, index] in order (for a still after beat `index`). */
  playTo: (index: number) => void;
}

export function bindStory(story: Story, control: ShowcaseControl): void {
  const beats = story.beats ?? [];
  window.__showcaseStory = {
    count: beats.length,
    intro: story.intro,
    captions: beats.map((b) => b.caption),
    holds: beats.map((b) => b.hold),
    clicks: beats.map((b) => b.clickAction),
    ids: beats.map((b) => b.id),
    run: (i) => beats[i]?.run(control),
    playTo: (i) => {
      for (let k = 0; k <= i && k < beats.length; k++) beats[k].run(control);
    },
  };
}
