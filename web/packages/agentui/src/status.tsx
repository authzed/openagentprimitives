// status.tsx renders ap:status — one line that is always true about something
// the page tracks, with a dot in the state's colour. Read-only: it fires
// nothing. The agent repaints it at every transition.
import { cn } from "@ap/design/lib/utils";
import { s } from "./props";
import type { Node } from "./types";

const DOT: Record<string, string> = {
  idle: "bg-muted-foreground",
  running: "bg-[hsl(var(--success))] animate-pulse",
  paused: "bg-[hsl(var(--warning))]",
  ended: "bg-muted-foreground",
  unwatched: "bg-[hsl(var(--warning))]",
};

// localTime is `since` as the viewer's own HH:MM, or null when `since` is
// absent or is not a moment. The agent writes the moment (an RFC3339 time);
// only the browser knows which clock the person actually reads, so this is
// the one place it may be turned into a wall-clock reading. An unparsable
// value draws nothing rather than the string it could not read — the line's
// own text is still true without it.
function localTime(since: string): string | null {
  if (since === "") return null;
  const at = new Date(since);
  if (Number.isNaN(at.getTime())) return null;
  return at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

export function StatusLine({ n }: { n: Node }): JSX.Element {
  const state = s(n, "state");
  const dot = DOT[state] ?? DOT.idle;
  const since = s(n, "since");
  const local = localTime(since);
  return (
    <div data-testid="ap-status" data-state={state} role="status" title={since || undefined} className="flex items-center gap-2 text-sm font-medium">
      <span aria-hidden="true" className={cn("inline-block h-2 w-2 shrink-0 rounded-full", dot)} />
      <span>
        {s(n, "text")}
        {local !== null && (
          <>
            {" "}
            <time dateTime={since}>{local}</time>
          </>
        )}
      </span>
    </div>
  );
}
