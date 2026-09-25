import * as React from "react";
import {
  Alert, AlertDescription, Badge, ModelName,
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@ap/design";
import { getBudget, type BudgetBreakdown, type BudgetRow } from "../lib/api";
import { useRoute, type EntityKind } from "../lib/router";
import { EntityLink } from "../lib/EntityLink";
import { ResourceName } from "../lib/ResourceName";
import { decodeSubject } from "../lib/decodeSubject";
import { ordinalColorMap } from "../lib/ordinalColor";
import { fmtTok } from "../lib/fmt";

// An unpriced amount arrives as null (the Go cost.USD marshaler encodes NaN as
// null) — render it as "NaN" rather than a misleading $0.00.
const fmtUSD = (n: number | null | undefined): string =>
  n == null || Number.isNaN(n) ? "NaN" : `$${n.toFixed(2)}`;

// The four breakdown axes, in the order they render as tabs. `keyLabel` is the
// first column's header for that axis; `keyEntity`, when set, links each row's
// key to that entity's detail page (bySession keys are exact "ns/name" ids;
// byAgentClass keys are bare class names resolved via the config detail page).
// `decode` reverses the canonical subject encoding (byUser keys are encoded
// subjects — show the human email, not the base64url hash).
const AXES: {
  id: keyof Omit<BudgetBreakdown, "estimated">;
  label: string;
  keyLabel: string;
  keyEntity?: EntityKind;
  decode?: boolean;
}[] = [
  { id: "byModel", label: "By model", keyLabel: "Model" },
  { id: "byAgentClass", label: "By agent", keyLabel: "Agent class", keyEntity: "agent" },
  { id: "bySession", label: "By session", keyLabel: "Session", keyEntity: "session" },
  { id: "byUser", label: "By user", keyLabel: "User", decode: true },
];

// KeyDot is the small colored category swatch shown before each breakdown key —
// stable per value (mirrors the Logs legend) so rows differentiate at a glance.
function KeyDot({ color }: { color?: string }) {
  if (!color) return null;
  return (
    <span
      aria-hidden="true"
      className="inline-block h-2.5 w-2.5 shrink-0 rounded-sm"
      style={{ backgroundColor: color }}
    />
  );
}

// BreakdownTable renders one axis' rows: the axis key, input/output tokens, and
// the labeled-estimate cost. `highlightKey` (from the ?model= deep link) tints
// the matching row so a click-through from Overview lands on the right line.
function BreakdownTable({
  rows,
  keyLabel,
  highlightKey,
  keyEntity,
  decode,
  isModel,
}: {
  rows: BudgetRow[];
  keyLabel: string;
  highlightKey?: string;
  // keyEntity, when set, links each row's key to that entity's detail page.
  keyEntity?: EntityKind;
  // decode reverses the canonical subject encoding on the key (byUser).
  decode?: boolean;
  // isModel marks the byModel axis, whose keys are uniform model display ids
  // rendered via <ModelName> rather than as plain text.
  isModel?: boolean;
}) {
  if (rows.length === 0) {
    return <p className="px-1 py-6 text-center text-xs text-muted-foreground">No usage recorded.</p>;
  }
  // Stable value→color map over this axis' keys (sorted internally), so the same
  // key always gets the same swatch color across renders.
  const colors = ordinalColorMap(rows.map((r) => r.key));
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>{keyLabel}</TableHead>
          <TableHead className="text-right">Input tokens</TableHead>
          <TableHead className="text-right">Output tokens</TableHead>
          <TableHead className="text-right">Est. cost</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((r) => {
          const on = highlightKey != null && r.key === highlightKey;
          // bySession keys are "ns/name" (render namespace muted); byUser keys are
          // encoded subjects (decode to the email); the rest are already human.
          const label =
            keyEntity === "session" ? <ResourceName id={r.key} /> : decode ? decodeSubject(r.key) : r.key;
          return (
            <TableRow key={r.key} className={on ? "bg-accent/60" : undefined}>
              <TableCell className="font-mono text-xs">
                <span className="flex items-center gap-2">
                  <KeyDot color={colors.get(r.key)} />
                  {r.key && keyEntity ? (
                    <EntityLink entity={keyEntity} id={r.key} className="text-link hover:underline">
                      {label}
                    </EntityLink>
                  ) : isModel && r.key ? (
                    <ModelName model={r.key} />
                  ) : (
                    <span>{r.key ? label : "(unknown)"}</span>
                  )}
                  {on && <Badge variant="secondary" className="align-middle text-[10px]">from Overview</Badge>}
                </span>
              </TableCell>
              <TableCell className="text-right font-mono text-xs text-muted-foreground">{fmtTok(r.inputTokens)}</TableCell>
              <TableCell className="text-right font-mono text-xs text-muted-foreground">{fmtTok(r.outputTokens)}</TableCell>
              <TableCell className="text-right font-mono text-xs">{fmtUSD(r.estimatedCostUSD)}</TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}

// BudgetView is the Audit › Budget page: estimated spend broken down along four
// axes (model, agent class, session, user). A `?model=` query param (linked
// from the Overview per-model bars) pre-selects the By-model tab and highlights
// that row. Costs are list-price estimates — always labeled as such.
export function BudgetView({ apiBase }: { apiBase: string }) {
  const route = useRoute();
  const highlightModel =
    route.type === "view" && route.query ? route.query.get("model") ?? undefined : undefined;

  const [data, setData] = React.useState<BudgetBreakdown | null>(null);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    let active = true;
    getBudget(apiBase)
      .then((d) => { if (active) { setData(d); setError(null); } })
      .catch((e: Error) => { if (active) setError(e.message); });
    return () => { active = false; };
  }, [apiBase]);

  // The By-model tab is the default landing tab (and where a ?model= deep link
  // highlights its row).
  const [tab, setTab] = React.useState<string>("byModel");
  const tablistRef = React.useRef<HTMLDivElement>(null);

  // Roving-tabindex arrow navigation for the hand-rolled tablist: Left/Right
  // move (and wrap) selection, Home/End jump to the ends, and focus follows so
  // keyboard users land on the newly-selected tab.
  const onTabKeyDown = (e: React.KeyboardEvent) => {
    const keys = ["ArrowRight", "ArrowLeft", "Home", "End"];
    if (!keys.includes(e.key)) return;
    e.preventDefault();
    const i = AXES.findIndex((a) => a.id === tab);
    const next =
      e.key === "ArrowRight" ? (i + 1) % AXES.length
      : e.key === "ArrowLeft" ? (i - 1 + AXES.length) % AXES.length
      : e.key === "Home" ? 0
      : AXES.length - 1;
    const nextId = AXES[next].id;
    setTab(nextId);
    tablistRef.current?.querySelector<HTMLButtonElement>(`#budget-tab-${nextId}`)?.focus();
  };

  if (error) {
    return (
      <Alert variant="destructive">
        <AlertDescription>Could not load budget: {error}</AlertDescription>
      </Alert>
    );
  }
  if (!data) {
    return <p className="text-sm text-muted-foreground">Loading budget…</p>;
  }

  // Any unpriced (null) row makes the total genuinely unknown — coerce null to
  // NaN so it poisons the sum and the headline reads "NaN", consistent with the
  // Overview total (which the backend already sums with NaN poisoning).
  const grandCost = data.byModel.reduce((s, r) => s + (r.estimatedCostUSD ?? NaN), 0);

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2 rounded-xl border bg-card p-4 shadow">
        <div>
          <div className="text-[11px] text-muted-foreground">Total estimated spend</div>
          <div className="mt-0.5 text-2xl font-semibold tracking-tight">{fmtUSD(grandCost)}</div>
        </div>
        <p className="max-w-md text-[11px] text-muted-foreground">
          Estimated from list prices — not a metered bill. Same live-session scope as the Overview budget panel.
        </p>
      </div>

      <div
        ref={tablistRef}
        role="tablist"
        aria-label="Budget breakdown axes"
        onKeyDown={onTabKeyDown}
        className="inline-flex h-9 items-center gap-1 rounded-lg bg-muted p-1 text-muted-foreground"
      >
        {AXES.map((a) => {
          const on = tab === a.id;
          return (
            <button
              key={a.id}
              id={`budget-tab-${a.id}`}
              type="button"
              role="tab"
              aria-selected={on}
              aria-controls={`budget-panel-${a.id}`}
              tabIndex={on ? 0 : -1}
              onClick={() => setTab(a.id)}
              className={[
                "inline-flex items-center justify-center whitespace-nowrap rounded-md px-3 py-1 text-sm font-medium transition-all",
                on ? "bg-background text-foreground shadow" : "hover:text-foreground",
              ].join(" ")}
            >
              {a.label}
            </button>
          );
        })}
      </div>

      {AXES.filter((a) => a.id === tab).map((a) => (
        <div
          key={a.id}
          id={`budget-panel-${a.id}`}
          role="tabpanel"
          aria-labelledby={`budget-tab-${a.id}`}
          tabIndex={0}
          className="rounded-xl border bg-card p-2 shadow"
        >
          <BreakdownTable
            rows={data[a.id]}
            keyLabel={a.keyLabel}
            highlightKey={a.id === "byModel" ? highlightModel : undefined}
            keyEntity={a.keyEntity}
            decode={a.decode}
            isModel={a.id === "byModel"}
          />
        </div>
      ))}
    </div>
  );
}
