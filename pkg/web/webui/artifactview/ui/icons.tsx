import { Github, MessageSquare } from "lucide-react";
import type { ComponentType } from "react";

// Glyph is the uniform shape every entry in channelGlyphs accepts, so the map
// can be a plain key → component lookup.
type Glyph = ComponentType<{ className?: string }>;

// SlackIcon is the multicolor Slack glyph, copied verbatim from the approved
// live-view mockup (the four-color hashtag). It carries no currentColor — the
// brand colors are fixed — so a `className` only sizes/positions it.
export function SlackIcon({ className }: { className?: string }) {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" className={className} aria-hidden="true">
      <path fill="#E01E5A" d="M5.6 14.5a2.3 2.3 0 1 1-2.3-2.3h2.3z" />
      <path fill="#E01E5A" d="M6.8 14.5a2.3 2.3 0 0 1 4.6 0v5.7a2.3 2.3 0 1 1-4.6 0z" />
      <path fill="#36C5F0" d="M9.5 5.6a2.3 2.3 0 1 1 2.3-2.3v2.3z" />
      <path fill="#36C5F0" d="M9.5 6.8a2.3 2.3 0 0 1 0 4.6H3.8a2.3 2.3 0 1 1 0-4.6z" />
      <path fill="#2EB67D" d="M18.4 9.5a2.3 2.3 0 1 1 2.3 2.3h-2.3z" />
      <path fill="#2EB67D" d="M17.2 9.5a2.3 2.3 0 0 1-4.6 0V3.8a2.3 2.3 0 1 1 4.6 0z" />
      <path fill="#ECB22E" d="M14.5 18.4a2.3 2.3 0 1 1-2.3 2.3v-2.3z" />
      <path fill="#ECB22E" d="M14.5 17.2a2.3 2.3 0 0 1 0-4.6h5.7a2.3 2.3 0 1 1 0 4.6z" />
    </svg>
  );
}

// GithubGlyph / MessageGlyph wrap the lucide marks so every channelGlyphs entry
// shares one `{ className }` signature (and carries aria-hidden, since the label
// rides the tooltip).
const GithubGlyph: Glyph = ({ className }) => <Github className={className} aria-hidden="true" />;
const MessageGlyph: Glyph = ({ className }) => <MessageSquare className={className} aria-hidden="true" />;

// channelGlyphs maps an origin link's icon KEY — a small presentation vocabulary
// the originating channel kind picks (OriginLinkProvider), not the kind name —
// to its glyph. A new channel kind that reuses an existing key needs no change
// here; only one that wants a brand-new glyph adds a row.
const channelGlyphs: Record<string, Glyph> = {
  slack: SlackIcon,
  github: GithubGlyph,
};

// ChannelIcon renders the glyph for an origin link's icon key, falling back to a
// generic message bubble for an unknown key.
export function ChannelIcon({ icon, className }: { icon: string; className?: string }) {
  const Icon = channelGlyphs[icon] ?? MessageGlyph;
  return <Icon className={className} />;
}
