import { Loader2, Wifi, WifiOff } from "lucide-react";
import { Tooltip, TooltipContent, TooltipTrigger } from "@ap/design";
import type { ConnState } from "./types";

// connMeta maps each live-socket state to its toolbar presentation: a glyph, a
// color class, whether it spins, and the tooltip copy.
const connMeta: Record<ConnState, { Icon: typeof Wifi; color: string; spin: boolean; tip: string }> = {
  connected: { Icon: Wifi, color: "text-success", spin: false, tip: "Live — connected" },
  connecting: { Icon: Loader2, color: "text-warning", spin: true, tip: "Connecting…" },
  reconnecting: { Icon: Loader2, color: "text-warning", spin: true, tip: "Reconnecting…" },
  offline: { Icon: WifiOff, color: "text-destructive", spin: false, tip: "Offline — retrying" },
};

// ConnectionIndicator renders the live-socket health as an icon-only badge with a
// tooltip: green wifi when connected, an amber spinner while (re)connecting, and a
// red wifi-off once the link is sustained-offline.
export function ConnectionIndicator({ state }: { state: ConnState }) {
  const meta = connMeta[state];
  const { Icon } = meta;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <div className={`flex h-7 w-7 items-center justify-center rounded-md cursor-default ${meta.color}`}>
          <Icon className={`h-[18px] w-[18px] ${meta.spin ? "animate-spin" : ""}`} aria-hidden="true" />
        </div>
      </TooltipTrigger>
      <TooltipContent>{meta.tip}</TooltipContent>
    </Tooltip>
  );
}
