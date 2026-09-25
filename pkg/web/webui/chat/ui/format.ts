// Compact number/time formatting for the live status line
// ("working · 34s · 1.2K in · 6.4K out"), mirroring the Slack channel's
// turn-progress caption (pkg/channels/channelkinds/slack/sender_turn_progress.go's
// humanizeTokens / humanizeElapsed).

// stripZero drops a trailing ".0" so "1.0K" reads as "1K".
function stripZero(v: number): string {
  return v.toFixed(1).replace(/\.0$/, "");
}

// formatTokens renders a token count compactly: "999", "1.2K", "47.2K",
// "1.5M". Negative inputs clamp to 0.
export function formatTokens(n: number): string {
  if (!Number.isFinite(n) || n < 0) n = 0;
  if (n < 1000) return String(Math.round(n));
  if (n < 1_000_000) return `${stripZero(n / 1000)}K`;
  return `${stripZero(n / 1_000_000)}M`;
}

// formatElapsed renders a non-negative second count as "34s", "1m", "2m5s".
export function formatElapsed(sec: number): string {
  if (!Number.isFinite(sec) || sec < 0) sec = 0;
  sec = Math.floor(sec);
  if (sec < 60) return `${sec}s`;
  const m = Math.floor(sec / 60);
  const s = sec % 60;
  return s === 0 ? `${m}m` : `${m}m${s}s`;
}

// progressCaption builds the subtle status-line text shown while a turn is
// active: "working · 34s · 1.2K in · 6.4K out". A null progress snapshot (the
// turn started but no tick has landed yet) collapses to just "working".
export function progressCaption(p: {
  elapsedSeconds: number;
  inputTokens: number;
  outputTokens: number;
} | null): string {
  if (!p) return "working";
  return `working · ${formatElapsed(p.elapsedSeconds)} · ${formatTokens(p.inputTokens)} in · ${formatTokens(p.outputTokens)} out`;
}
