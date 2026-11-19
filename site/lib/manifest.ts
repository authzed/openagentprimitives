import manifest from "@/content/_manifest.json";

// Media are referenced from MDX by name (e.g. <Clip name="multiplayer-approval" />)
// and resolved here through site/content/_manifest.json, whose entries are
// added by hand. A miss renders a placeholder rather than breaking the page.
export interface MediaAsset {
  kind: "clip" | "screenshot" | "video";
  webm?: string;
  mp4?: string;
  poster?: string;
  src?: string;
  caption?: string;
  width?: number;
  height?: number;
  /** A clip's spoken narration, one line per scene. When present the clip
   *  renders a readable transcript and VideoObject structured data. */
  transcript?: string[];
  /** VideoObject fields: the video's name, publish date (YYYY-MM-DD), and
   *  ISO 8601 duration (PT1M42S). */
  title?: string;
  uploadDate?: string;
  duration?: string;
}

const MANIFEST = manifest as Record<string, MediaAsset>;

export function getAsset(name: string): MediaAsset | undefined {
  return MANIFEST[name];
}
