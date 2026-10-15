import { ArrowUpRight } from "lucide-react";
import { Tooltip, TooltipContent, TooltipTrigger } from "@ap/design";
import { ChannelIcon } from "./icons";

// ThreadLink is the compact, icon-only affordance that opens an originating
// surface in a new tab: the channel glyph plus a small ↗, with a tooltip. The
// label (tooltip text) and icon (glyph key) come from the originating channel
// kind — see pkg/channels/channelkinds OriginLinkProvider — so this component
// never branches on which kind it is. It renders nothing when there's no link
// to open (href empty).
export function ThreadLink({ href, label, icon }: { href: string; label: string; icon: string }) {
  if (!href) return null;
  const tip = label || "Open the thread";
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <a
          href={href}
          target="_blank"
          rel="noreferrer"
          className="group flex items-center gap-[3px] rounded-md px-[7px] py-[5px] text-inherit no-underline hover:bg-muted"
        >
          <ChannelIcon icon={icon} className="h-4 w-4" />
          <ArrowUpRight className="h-[11px] w-[11px] text-muted-foreground/70 group-hover:text-muted-foreground" aria-hidden="true" />
        </a>
      </TooltipTrigger>
      <TooltipContent>{tip}</TooltipContent>
    </Tooltip>
  );
}
