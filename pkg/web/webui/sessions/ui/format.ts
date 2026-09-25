// Formatting helpers for the session shell's own chrome. Deliberately separate
// from pkg/web/webui/chat/ui/format.ts, which formats the TRANSCRIPT's live status
// line (token counts, elapsed time): the two files share no reader, and the
// shell must not import the transcript's module just to reach one function.

// relativeTime renders an ISO timestamp as a compact "just now" / "5m" / "2h" /
// "3d" recency label for a session-list row. A missing/unparseable value
// returns "" so the row simply omits the time rather than showing "NaN" —
// SessionRow.startedAt is optional (a session whose status carries no start
// time yet), so the empty case is reached in normal operation, not only on
// malformed input.
export function relativeTime(iso: string, now: number = Date.now()): string {
  if (!iso) return "";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "";
  const secs = Math.max(0, Math.floor((now - t) / 1000));
  // Under a minute is "just now" — extended from 45s so 45–59s-old sessions no
  // longer render the nonsensical "0m" (mins floors to 0 in that window).
  if (secs < 60) return "just now";
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h`;
  const days = Math.floor(hours / 24);
  return `${days}d`;
}
