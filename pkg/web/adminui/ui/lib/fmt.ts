// Token-count formatters shared across the admin surfaces, consolidated here so
// the live/rollup views and the spend views each have one canonical copy.

// fmtTokens renders a token count as a one-decimal-K value (12.4K / 940). Used by
// the live-session list, the session detail counters, and the audit rollups.
export function fmtTokens(n: number): string {
  return n >= 1000 ? `${(n / 1000).toFixed(1)}K` : `${n}`;
}

// fmtTok renders a token count compactly with millions support (1.84M / 312K /
// 940). Used by the Overview and Budget spend surfaces so the two read the same.
export function fmtTok(n: number): string {
  if (n >= 1e6) return `${(n / 1e6).toFixed(2)}M`;
  if (n >= 1e3) return `${Math.round(n / 1e3)}K`;
  return `${n}`;
}

// fmtRelative renders an ISO timestamp as a compact relative-time label
// ("just now", "5m ago", "3h ago", "2d ago"); anything a week or older falls
// back to the locale date. An unparseable/empty value returns "—" so callers
// can render it verbatim. Used by the live-session "Started" column.
export function fmtRelative(iso?: string, now: number = Date.now()): string {
  if (!iso) return "—";
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return "—";
  const secs = Math.round((now - t) / 1000);
  if (secs < 45) return "just now";
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.round(mins / 60);
  if (hrs < 24) return `${hrs}h ago`;
  const days = Math.round(hrs / 24);
  if (days < 7) return `${days}d ago`;
  return new Date(t).toLocaleDateString();
}
