import * as React from "react";
import { Alert, AlertDescription, Badge, Button } from "@ap/design";
import { postJSON, type AuditEvent, type AuditQueryRequest, type AuditQueryResponse, type FacetsResponse } from "../lib/api";
import { decodeSubject } from "../lib/decodeSubject";
import { ordinalColorMap } from "../lib/ordinalColor";
import { ColorLegend, EventTable, LOG_COLOR_FIELDS, type LogColorMaps } from "./EventTable";
import { EventDrawer } from "./EventDrawer";

const PAGE = 50;
const FACET_FIELDS: Array<{ facet: string; field: keyof AuditQueryRequest; label: string }> = [
  { facet: "kind", field: "kinds", label: "Kind" },
  { facet: "outcome", field: "outcome", label: "Outcome" },
  { facet: "agentClass", field: "agentClass", label: "Agent" },
  { facet: "tool", field: "tool", label: "Tool" },
  { facet: "actor", field: "actor", label: "Actor" },
];

// matchesQuery is the client-side free-text filter: case-insensitive substring
// across every visible field of a loaded event (the actor is matched both raw
// and decoded so a search for a human email hits an encoded-subject actor).
function matchesQuery(e: AuditEvent, q: string): boolean {
  const t = q.trim().toLowerCase();
  if (!t) return true;
  const hay = [
    e.kind, e.agentClass, e.sessionNamespace, e.sessionName,
    `${e.sessionNamespace}/${e.sessionName}`,
    e.outcome, e.tool, e.actor, e.actor ? decodeSubject(e.actor) : "", e.summary,
  ]
    .filter(Boolean)
    .join(" ")
    .toLowerCase();
  return hay.includes(t);
}

export function AuditExplorer({ apiBase, initialFilter }: { apiBase: string; initialFilter: AuditQueryRequest }) {
  const [filter, setFilter] = React.useState<AuditQueryRequest>(initialFilter);
  const [page, setPage] = React.useState(0);
  const [resp, setResp] = React.useState<AuditQueryResponse | null>(null);
  const [facets, setFacets] = React.useState<FacetsResponse | null>(null);
  const [selected, setSelected] = React.useState<AuditEvent | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [search, setSearch] = React.useState("");
  const [showLegend, setShowLegend] = React.useState(true);

  React.useEffect(() => {
    let active = true;
    const req = { ...filter, limit: PAGE, offset: page * PAGE };
    Promise.all([
      postJSON<AuditQueryResponse>(`${apiBase}/audit/query`, req),
      postJSON<FacetsResponse>(`${apiBase}/audit/facets`, filter),
    ])
      .then(([q, f]) => {
        if (!active) return;
        setResp(q);
        setFacets(f);
        setError(null);
      })
      .catch((e: Error) => {
        if (!active) return;
        setError(e.message);
      });
    return () => { active = false; };
  }, [apiBase, filter, page]);

  const setField = (field: keyof AuditQueryRequest, value: string) => {
    setPage(0);
    setFilter((prev) => ({ ...prev, [field]: field === "kinds" ? [value] : value }));
  };
  const clearField = (field: keyof AuditQueryRequest) => {
    setPage(0);
    setFilter((prev) => {
      const next = { ...prev };
      delete next[field];
      return next;
    });
  };

  const chips = FACET_FIELDS.filter(({ field }) => filter[field]).map(({ field, label }) => ({
    field, label,
    value: field === "kinds" ? (filter.kinds ?? []).join(",") : String(filter[field]),
  }));

  // The typed filter narrows the loaded page client-side; the color legends +
  // cell swatches are computed over the visible (filtered) rows so the legend
  // reflects exactly what the table shows.
  const events = resp?.events ?? [];
  const filtered = React.useMemo(() => events.filter((e) => matchesQuery(e, search)), [events, search]);
  const colorMaps = React.useMemo<LogColorMaps>(() => {
    const out = {} as LogColorMaps;
    for (const f of LOG_COLOR_FIELDS) out[f.key] = ordinalColorMap(filtered.map(f.value));
    return out;
  }, [filtered]);

  return (
    // Below lg the facet rail sits ABOVE the table as a wrapped grid of groups
    // instead of beside it: at the widths a split pane gives this page (~700px)
    // a fixed 12rem rail plus a 6-column table left the table one column wide.
    <div className="flex flex-col gap-4 lg:flex-row lg:gap-6">
      <aside className="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:block lg:w-48 lg:shrink-0 lg:space-y-4">
        {FACET_FIELDS.map(({ facet, field, label }) => (
          <div key={facet}>
            <h4 className="text-xs font-semibold uppercase text-muted-foreground">{label}</h4>
            <ul className="mt-1 space-y-0.5 text-xs">
              {Object.entries(facets?.counts[facet] ?? {})
                .sort(([, a], [, b]) => b - a)
                .slice(0, 8)
                .map(([val, n]) => (
                  <li key={val}>
                    <button className="hover:text-primary" onClick={() => setField(field, val)}>
                      {val} ({n})
                    </button>
                  </li>
                ))}
            </ul>
          </div>
        ))}
      </aside>

      <div className="min-w-0 flex-1 space-y-3">
        {error && (
          <Alert variant="destructive">
            <AlertDescription>Audit query failed: {error}</AlertDescription>
          </Alert>
        )}
        {resp?.truncated && (
          <Alert>
            <AlertDescription>Results truncated — narrow the time range or filters for complete counts.</AlertDescription>
          </Alert>
        )}
        <div className="flex flex-wrap gap-2">
          {chips.map((c) => (
            <Badge key={c.field} variant="secondary" className="cursor-pointer" onClick={() => clearField(c.field)}>
              {c.label.toLowerCase()}: {c.value} ✕
            </Badge>
          ))}
        </div>

        <div className="flex flex-wrap items-center gap-2">
          <div className="relative min-w-0 flex-1">
            <input
              type="search"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Filter loaded events…"
              aria-label="Filter loaded events"
              className="w-full rounded-md border border-border bg-background px-2 py-1 text-sm text-foreground placeholder:text-muted-foreground focus:outline-none focus:ring-1 focus:ring-ring"
            />
          </div>
          {search && (
            <Button variant="ghost" size="sm" onClick={() => setSearch("")}>Clear</Button>
          )}
          <Button variant="outline" size="sm" onClick={() => setShowLegend((s) => !s)}>
            {showLegend ? "Hide legend" : "Show legend"}
          </Button>
          <span className="whitespace-nowrap text-xs text-muted-foreground">
            {filtered.length}{search ? ` of ${events.length}` : ""} shown
          </span>
        </div>

        {showLegend && <ColorLegend colorMaps={colorMaps} />}

        <EventTable events={filtered} onRow={setSelected} colorMaps={colorMaps} />
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm" disabled={page === 0} onClick={() => setPage((p) => p - 1)}>Prev</Button>
          <Button variant="outline" size="sm" disabled={!resp?.hasMore} onClick={() => setPage((p) => p + 1)}>Next</Button>
          <span className="text-xs text-muted-foreground">page {page + 1}</span>
        </div>
      </div>

      {selected && <EventDrawer event={selected} onClose={() => setSelected(null)} />}
    </div>
  );
}
