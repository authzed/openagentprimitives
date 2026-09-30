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
}

const MANIFEST = manifest as Record<string, MediaAsset>;

export function getAsset(name: string): MediaAsset | undefined {
  return MANIFEST[name];
}
