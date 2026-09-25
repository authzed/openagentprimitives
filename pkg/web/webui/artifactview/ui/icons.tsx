import { MessageSquare } from "lucide-react";

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

// ChannelIcon picks the originating-channel glyph for the thread link: the Slack
// logo for "slack", a generic message bubble otherwise.
export function ChannelIcon({ kind, className }: { kind: string; className?: string }) {
  if (kind === "slack") return <SlackIcon className={className} />;
  return <MessageSquare className={className} aria-hidden="true" />;
}
