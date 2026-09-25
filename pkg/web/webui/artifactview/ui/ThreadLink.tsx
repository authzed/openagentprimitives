import { ArrowUpRight } from "lucide-react";
import { Tooltip, TooltipContent, TooltipTrigger } from "@ap/design";
import { ChannelIcon } from "./icons";

// ThreadLink is the compact, icon-only affordance that opens the originating
// thread in a new tab: the channel glyph plus a small ↗, with a tooltip. It
// renders nothing when there's no thread to open (href empty).
export function ThreadLink({ href, channelKind }: { href: string; channelKind: string }) {
  if (!href) return null;
  const tip = channelKind === "slack" ? "Open the Slack thread" : "Open the thread";
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <a
          href={href}
          target="_blank"
          rel="noreferrer"
          className="group flex items-center gap-[3px] rounded-md px-[7px] py-[5px] text-inherit no-underline hover:bg-muted"
        >
          <ChannelIcon kind={channelKind} className="h-4 w-4" />
          <ArrowUpRight className="h-[11px] w-[11px] text-muted-foreground/70 group-hover:text-muted-foreground" aria-hidden="true" />
        </a>
      </TooltipTrigger>
      <TooltipContent>{tip}</TooltipContent>
    </Tooltip>
  );
}
