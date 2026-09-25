import * as React from "react";
import { Alert, AlertDescription, Card, CardContent, CardHeader, CardTitle, ModelName } from "@ap/design";
import {
  getHealth,
  getOverview,
  type HealthSnapshot,
  type HealthStatus,
  type OverviewData,
} from "../lib/api";
import { navigate } from "../lib/router";
import { EntityLink, ViewLink } from "../lib/EntityLink";
import { fmtTok } from "../lib/fmt";

// budgetQuery deep-links the Budget view pre-filtered to one model bar/legend row.
const budgetQuery = (model: string) => new URLSearchParams({ model });

// An unpriced amount arrives as null (the Go cost.USD marshaler encodes NaN as
// null) — render it as "NaN" rather than a misleading $0.00.
const fmtUSD = (n: number | null | undefined): string =>
  n == null || Number.isNaN(n) ? "NaN" : `$${n.toFixed(2)}`;

// Literal class names (not template-built) so Tailwind's content scanner keeps
// them in the bundle. Models cycle through this palette in rollup order.
// The chart series, in token order. Not --primary: that is ink, and ink as
// data is a black bar on a light page.
const MODEL_COLORS = ["bg-chart-1", "bg-chart-2", "bg-chart-3", "bg-chart-4", "bg-chart-5"];

const HEALTH_DOT: Record<HealthStatus, string> = {
  Healthy: "bg-success",
  Degraded: "bg-warning",
  Down: "bg-destructive",
  NotConfigured: "bg-muted-foreground",
};
const HEALTH_TEXT: Record<HealthStatus, string> = {
  Healthy: "text-success",
  Degraded: "text-warning",
  Down: "text-destructive",
  NotConfigured: "text-muted-foreground",
};

// AreaChart draws a filled inline-SVG sparkline over the 24h token series. The
// gradient + non-scaling stroke mirror the locked Overview prototype.
function AreaChart({ values, width = 600, height = 150 }: { values: number[]; width?: number; height?: number }) {
  const max = Math.max(1, ...values) * 1.12;
  const pad = 6;
  const n = values.length;
  const X = (i: number) => pad + (n <= 1 ? 0 : (i * (width - 2 * pad)) / (n - 1));
  const Y = (v: number) => height - pad - (v / max) * (height - 2 * pad);
  const line = values.map((v, i) => `${i ? "L" : "M"} ${X(i).toFixed(1)} ${Y(v).toFixed(1)}`).join(" ");
  const area = `${line} L ${X(n - 1).toFixed(1)} ${(height - pad).toFixed(1)} L ${X(0).toFixed(1)} ${(height - pad).toFixed(1)} Z`;
  return (
    <svg viewBox={`0 0 ${width} ${height}`} className="h-40 w-full" preserveAspectRatio="none" role="img" aria-label="Tokens over time">
      <defs>
        <linearGradient id="ap-overview-area" x1="0" x2="0" y1="0" y2="1">
          <stop offset="0%" stopColor="hsl(var(--chart-1))" stopOpacity="0.35" />
          <stop offset="100%" stopColor="hsl(var(--chart-1))" stopOpacity="0" />
        </linearGradient>
      </defs>
      <path d={area} fill="url(#ap-overview-area)" />
      <path d={line} fill="none" stroke="hsl(var(--chart-1))" strokeWidth="1.5" vectorEffect="non-scaling-stroke" />
    </svg>
  );
}

function KpiTile({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <div className="rounded-xl border bg-card p-4 shadow">
      <div className="text-[11px] text-muted-foreground">{label}</div>
      <div className="mt-1 text-2xl font-semibold tracking-tight">{value}</div>
      {sub && <div className="mt-0.5 font-mono text-[11px] text-muted-foreground">{sub}</div>}
    </div>
  );
}

export function OverviewView({ apiBase }: { apiBase: string }) {
  const [data, setData] = React.useState<OverviewData | null>(null);
  const [health, setHealth] = React.useState<HealthSnapshot | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [healthError, setHealthError] = React.useState<string | null>(null);

  React.useEffect(() => {
    let active = true;
    getOverview(apiBase)
      .then((d) => { if (active) { setData(d); setError(null); } })
      .catch((e: Error) => { if (active) setError(e.message); });
    getHealth(apiBase)
      .then((h) => { if (active) { setHealth(h); setHealthError(null); } })
      .catch((e: Error) => { if (active) setHealthError(e.message); });
    return () => { active = false; };
  }, [apiBase]);

  if (error) {
    return (
      <Alert variant="destructive">
        <AlertDescription>Could not load overview: {error}</AlertDescription>
      </Alert>
    );
  }
  if (!data) {
    return <p className="text-sm text-muted-foreground">Loading overview…</p>;
  }

  const k = data.kpis;
  const b = data.budget;
  const seriesVals = data.series24h.map((h) => h.inputTokens + h.outputTokens);
  const seriesTotal = seriesVals.reduce((a, v) => a + v, 0);

  const pct = b.tokenCeiling && b.tokenCeiling > 0 ? (b.tokensSpent / b.tokenCeiling) * 100 : null;
  // Budget-card headline: the percentage when a ceiling exists, else the absolute
  // tokens spent — so it doesn't duplicate the est-$ line beneath it.
  const budgetHeadline = pct != null ? `${pct.toFixed(0)}%` : fmtTok(b.tokensSpent);

  // Spend lives in its own Budget card (and the Budget page) — not duplicated as
  // a KPI tile here.
  const kpis = [
    { label: "Active sessions", value: `${k.activeSessions}` },
    { label: "Tokens today", value: fmtTok(k.tokensToday) },
    { label: "Tool calls (24h)", value: `${k.toolCalls24h}` },
    { label: "Approvals pending", value: `${k.approvalsPending}` },
    { label: "Denials (24h)", value: `${k.denials24h}` },
  ];

  const modelTotal = data.byModel.reduce((s, m) => s + m.inputTokens + m.outputTokens, 0) || 1;
  const classMax = Math.max(1, ...data.byAgentClass.map((c) => c.inputTokens + c.outputTokens));

  const degraded = (health?.components ?? []).filter((c) => c.status === "Degraded" || c.status === "Down").length;

  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-5">
        {kpis.map((t) => (
          <KpiTile key={t.label} label={t.label} value={t.value} />
        ))}
      </div>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2">
          <CardHeader className="flex-row items-center justify-between pb-2">
            <div>
              <CardTitle className="text-sm">Tokens over time</CardTitle>
              <div className="text-[11px] text-muted-foreground">last 24 hours · all models</div>
            </div>
            <div className="font-mono text-xs text-muted-foreground">{fmtTok(seriesTotal)} total</div>
          </CardHeader>
          <CardContent>
            <AreaChart values={seriesVals} />
          </CardContent>
        </Card>

        <ViewLink view="budget" className="block h-full rounded-xl">
          <Card className="h-full transition-colors hover:border-primary/50 hover:bg-accent/20">
            <CardHeader className="flex-row items-center justify-between pb-2">
              <CardTitle className="text-sm">Budget</CardTitle>
              <span className="text-[11px] text-link">View breakdown →</span>
            </CardHeader>
            <CardContent className="space-y-2">
              <div className="flex items-end gap-2">
                <div className="text-3xl font-semibold tracking-tight">{budgetHeadline}</div>
                <div className="mb-1 font-mono text-xs text-muted-foreground">
                  {pct != null ? `${fmtTok(b.tokensSpent)} tokens` : "tokens spent"}
                </div>
              </div>
              <div className="font-mono text-sm text-muted-foreground">est. {fmtUSD(b.estimatedCostUSD)}</div>
              <div className="text-[11px] text-muted-foreground">
                {b.estimated ? "estimated · list-price" : "metered"} · daily budget resets 00:00 UTC
              </div>
            </CardContent>
          </Card>
        </ViewLink>
      </div>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm">Tokens by model</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            {/* Purely-decorative proportion bar: hidden from assistive tech since
                the per-model legend links below carry the accessible (keyboard +
                AT) path to each model's budget breakdown. Mouse users still get
                the clickable segments. */}
            <div aria-hidden="true" className="flex h-3 w-full overflow-hidden rounded">
              {data.byModel.map((m, i) => (
                <div
                  key={m.model}
                  onClick={() => navigate({ type: "view", view: "budget", query: budgetQuery(m.model) })}
                  title={`${m.model || "(unknown)"} — view budget`}
                  className={`h-full cursor-pointer ${MODEL_COLORS[i % MODEL_COLORS.length]}`}
                  style={{ width: `${(((m.inputTokens + m.outputTokens) / modelTotal) * 100).toFixed(1)}%` }}
                />
              ))}
            </div>
            <div className="space-y-2">
              {data.byModel.length === 0 && <p className="text-xs text-muted-foreground">No live model usage.</p>}
              {data.byModel.map((m, i) => (
                <ViewLink
                  key={m.model}
                  view="budget"
                  query={budgetQuery(m.model)}
                  className="-mx-1 flex items-center justify-between rounded px-1 py-0.5 text-xs transition-colors hover:bg-accent/40"
                >
                  <span className="flex items-center gap-2">
                    <span aria-hidden="true" className={`h-2.5 w-2.5 rounded-sm ${MODEL_COLORS[i % MODEL_COLORS.length]}`} />
                    {m.model ? <ModelName model={m.model} /> : <span className="font-mono">(unknown)</span>}
                  </span>
                  <span className="font-mono text-muted-foreground">
                    {fmtTok(m.inputTokens + m.outputTokens)} · {m.sessions} sess
                  </span>
                </ViewLink>
              ))}
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm">Tokens by agent class</CardTitle>
          </CardHeader>
          <CardContent className="space-y-2.5">
            {data.byAgentClass.length === 0 && <p className="text-xs text-muted-foreground">No live agent-class usage.</p>}
            {data.byAgentClass.map((c) => {
              const tot = c.inputTokens + c.outputTokens;
              // The class key is a BARE name (no namespace); useResourceRow's
              // bare-name match resolves it to the AgentClass detail page.
              const bar = (
                <>
                  <span className="w-28 shrink-0 truncate font-mono">{c.class || "(unknown)"}</span>
                  <div className="h-2.5 flex-1 overflow-hidden rounded bg-muted">
                    <div className="h-full rounded bg-chart-1" style={{ width: `${((tot / classMax) * 100).toFixed(1)}%` }} />
                  </div>
                  <span className="w-12 shrink-0 text-right font-mono text-muted-foreground">{fmtTok(tot)}</span>
                </>
              );
              return c.class ? (
                <EntityLink
                  key={c.class}
                  entity="agent"
                  id={c.class}
                  className="-mx-1 flex items-center gap-3 rounded px-1 py-0.5 text-xs transition-colors hover:bg-accent/40"
                >
                  {bar}
                </EntityLink>
              ) : (
                <div key="__unknown__" className="flex items-center gap-3 px-1 py-0.5 text-xs text-muted-foreground">
                  {bar}
                </div>
              );
            })}
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader className="flex-row items-center justify-between pb-2">
          <CardTitle className="text-sm">Cluster health</CardTitle>
          {degraded > 0 ? (
            <span className="flex items-center gap-1.5 text-[11px] text-warning">
              <span className="h-1.5 w-1.5 rounded-full bg-warning" />
              {degraded} component{degraded > 1 ? "s" : ""} degraded
            </span>
          ) : (
            health && (
              <span className="flex items-center gap-1.5 text-[11px] text-success">
                <span className="h-1.5 w-1.5 rounded-full bg-success" />
                all healthy
              </span>
            )
          )}
        </CardHeader>
        <CardContent className="space-y-3">
          {healthError && (
            <Alert variant="destructive">
              <AlertDescription>Could not load cluster health: {healthError}</AlertDescription>
            </Alert>
          )}
          {health && (
            <>
              <div className="grid grid-cols-2 gap-2 md:grid-cols-4">
                {health.components.map((c) => (
                  <div key={c.name} className="flex items-center justify-between rounded-md border bg-background px-3 py-2">
                    <span className="flex items-center gap-2 font-mono text-xs">
                      <span className={`h-1.5 w-1.5 rounded-full ${HEALTH_DOT[c.status]}`} />
                      {c.name}
                    </span>
                    <span className={`text-[10px] ${HEALTH_TEXT[c.status]}`} title={c.detail}>{c.status}</span>
                  </div>
                ))}
              </div>
              <div className="grid grid-cols-2 gap-2 md:grid-cols-4">
                <div className="rounded-md border bg-background px-3 py-2">
                  <div className="text-[10px] text-muted-foreground">Pods</div>
                  <div className="mt-0.5 font-mono text-sm">{health.rollup.ready} / {health.rollup.pods} ready</div>
                </div>
                <div className="rounded-md border bg-background px-3 py-2">
                  <div className="text-[10px] text-muted-foreground">CPU</div>
                  <div className="mt-0.5 font-mono text-sm">{health.rollup.cpu}</div>
                </div>
                <div className="rounded-md border bg-background px-3 py-2">
                  <div className="text-[10px] text-muted-foreground">Memory</div>
                  <div className="mt-0.5 font-mono text-sm">{health.rollup.memory}</div>
                </div>
              </div>
            </>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
