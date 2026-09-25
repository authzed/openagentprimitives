import * as React from "react";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
  Tooltip, TooltipContent, TooltipProvider, TooltipTrigger,
} from "@ap/design";
import { CopyButton } from "../lib/CopyCmd";
import { EntityLink } from "../lib/EntityLink";
import { KindBadge } from "../lib/KindBadge";
import { StatusFilter } from "../lib/StatusFilter";
import { useCookieState } from "../lib/cookies";
import { statusPillClass } from "../lib/status";
import type { EntityKind } from "../lib/router";
import type { ResourceRow } from "../lib/api";
import { Chip } from "./detail/shared";
import { rowId } from "./detail/useResourceRow";
import type { ColumnSpec } from "./columns";

// StatusPill is the small colored readiness pill. When the row carries a
// statusReason (e.g. a Degraded tool's condition reason), the pill becomes a
// Tooltip trigger so hovering reveals the WHY — the native `title` alone was too
// easy to miss. Rows with no reason render the bare pill.
function StatusPill({ status, reason }: { status: string; reason?: string }) {
  const pill = (
    <span className={`inline-flex items-center rounded-full border px-2 py-0.5 text-[10px] ${statusPillClass(status)}`}>
      {status || "—"}
    </span>
  );
  if (!reason) return pill;
  return (
    <TooltipProvider delayDuration={0}>
      <Tooltip>
        <TooltipTrigger asChild>{pill}</TooltipTrigger>
        <TooltipContent className="max-w-xs whitespace-normal break-words text-xs">{reason}</TooltipContent>
      </Tooltip>
    </TooltipProvider>
  );
}

// Chip (the neutral badge/scope pill) is shared from detail/shared so the table
// and the detail pages render one identical chip.

// cellFor projects one ColumnSpec out of a row. A column may supply its own
// `render` (for links / conditional coloring the generic projection can't do).
// A colorized badge column renders each value as a KindBadge — a stable
// per-value color dot + label, keyed on the value so the same kind reads the
// same color across the whole console (not just within this table).
function cellFor(row: ResourceRow, col: ColumnSpec): React.ReactNode {
  if (col.render) return col.render(row);
  if (col.source === "scope") {
    return <Chip>{row.scope}</Chip>;
  }
  if (col.source === "count") {
    const counts = row.counts ?? [];
    const c = col.key ? counts.find((x) => x.label === col.key) : counts[0];
    return c ? <span className="font-mono text-xs text-muted-foreground">{c.value}</span> : <span className="text-muted-foreground">—</span>;
  }
  // badge
  const badges = (row.badges ?? []).filter((b) => (col.key ? b.key === col.key : true));
  if (badges.length === 0) return <span className="text-muted-foreground">—</span>;
  return (
    <span className="flex flex-wrap gap-1">
      {badges.map((b, i) =>
        col.colorize ? (
          <KindBadge key={`${b.key}-${b.value}-${i}`} kind={b.value} />
        ) : (
          <Chip key={`${b.key}-${b.value}-${i}`}>{b.value}</Chip>
        ),
      )}
    </span>
  );
}

// FacetFilter is an optional second filter row that narrows by a badge value
// (e.g. Agent Identities by credential type). Its distinct values are the union
// of that badge key across the rows.
export interface FacetSpec {
  // The badge key whose values become the filter chips.
  key: string;
  // The label shown before the chips.
  label: string;
}

// usePersistentFilter keeps the selected filter in a cookie when a key is
// supplied, and in plain local state otherwise (so unkeyed usages — bare
// ResourceTable in tests — never write a shared cookie). Both hooks run
// unconditionally to satisfy the rules of hooks; only the chosen pair is used.
function usePersistentFilter(cookieKey: string | undefined): [string, (v: string) => void] {
  const local = React.useState<string>("");
  const stored = useCookieState<string>(cookieKey ?? "", "");
  return cookieKey ? stored : local;
}

export interface ResourceTableProps {
  rows: ResourceRow[];
  columns: ColumnSpec[];
  // entity, when set, turns each row's Name cell into a link to that entity's
  // detail page (keyed by rowId → "namespace/name" or bare "name").
  entity?: EntityKind;
  // facet adds an optional second filter row narrowing by a badge value.
  facet?: FacetSpec;
  // statusCookieKey persists the selected status filter across reloads; omit for
  // ephemeral local state (tests, transient tables).
  statusCookieKey?: string;
  // Optional caller-driven loading + footer note. error is surfaced by callers.
  loading?: boolean;
  note?: string;
  emptyText?: string;
}

// ResourceTable is the generic config table: a name-filter box, a status filter,
// an optional badge facet filter, and a @ap/design Table whose middle columns are
// configured per resource. The Name (+namespace, linkable), Status pill, and
// Manage (command + copy) cells are universal.
export function ResourceTable({
  rows, columns, entity, facet, statusCookieKey, loading, note, emptyText,
}: ResourceTableProps) {
  const [search, setSearch] = React.useState("");
  const [statusFilter, setStatusFilter] = usePersistentFilter(statusCookieKey);
  const [facetFilter, setFacetFilter] = React.useState("");

  const statuses = React.useMemo(() => {
    const seen = new Set<string>();
    for (const r of rows) if (r.status) seen.add(r.status);
    return Array.from(seen).sort();
  }, [rows]);

  const facetValues = React.useMemo(() => {
    if (!facet) return [];
    const seen = new Set<string>();
    for (const r of rows) for (const b of r.badges ?? []) if (b.key === facet.key) seen.add(b.value);
    return Array.from(seen).sort();
  }, [rows, facet]);

  // The Manage column is elided entirely when NO row carries a manageCmd (e.g.
  // the Users table, whose projector dropped the empty command) — an all-blank
  // "Manage" header/column is pure noise.
  const showManage = rows.some((r) => r.manageCmd);

  const q = search.trim().toLowerCase();
  const filtered = rows.filter(
    (r) =>
      (statusFilter === "" || r.status === statusFilter) &&
      (facetFilter === "" ||
        (r.badges ?? []).some((b) => facet && b.key === facet.key && b.value === facetFilter)) &&
      (q === "" ||
        r.name.toLowerCase().includes(q) ||
        (r.namespace ?? "").toLowerCase().includes(q)),
  );

  if (loading) {
    return <p className="text-sm text-muted-foreground">Loading…</p>;
  }
  if (rows.length === 0) {
    return <p className="text-sm text-muted-foreground">{emptyText ?? "Nothing configured yet."}</p>;
  }

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <input
          type="text"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="Filter by name…"
          aria-label="Filter by name"
          className="h-8 w-56 rounded-md border border-input bg-background px-3 text-xs outline-none focus-visible:ring-1 focus-visible:ring-ring"
        />
        {statuses.length > 1 && (
          <StatusFilter statuses={statuses} value={statusFilter} onChange={setStatusFilter} />
        )}
      </div>

      {facet && facetValues.length > 1 && (
        <div className="flex flex-wrap items-center gap-1.5">
          <span className="text-[10px] uppercase tracking-wide text-muted-foreground">{facet.label}</span>
          <StatusFilter statuses={facetValues} value={facetFilter} onChange={setFacetFilter} />
        </div>
      )}

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            {columns.map((c) => (
              <TableHead key={c.header} className={c.align === "right" ? "text-right" : ""}>{c.header}</TableHead>
            ))}
            <TableHead>Status</TableHead>
            {showManage && <TableHead>Manage</TableHead>}
          </TableRow>
        </TableHeader>
        <TableBody>
          {filtered.map((r) => (
            <TableRow key={`${r.namespace ?? ""}/${r.name}`}>
              <TableCell>
                {entity ? (
                  <EntityLink entity={entity} id={rowId(r)} className="font-mono text-xs text-link hover:underline">
                    {r.name}
                  </EntityLink>
                ) : (
                  <div className="font-mono text-xs text-foreground">{r.name}</div>
                )}
                {r.namespace && <div className="font-mono text-[10px] text-muted-foreground">{r.namespace}</div>}
              </TableCell>
              {columns.map((c) => (
                <TableCell key={c.header} className={c.align === "right" ? "text-right" : ""}>
                  {cellFor(r, c)}
                </TableCell>
              ))}
              <TableCell><StatusPill status={r.status} reason={r.statusReason} /></TableCell>
              {showManage && (
                <TableCell>
                  {r.manageCmd ? (
                    <div className="flex items-center gap-2">
                      <code className="truncate font-mono text-[11px] text-muted-foreground" title={r.manageCmd}>{r.manageCmd}</code>
                      <CopyButton text={r.manageCmd} />
                    </div>
                  ) : (
                    <span className="text-muted-foreground">—</span>
                  )}
                </TableCell>
              )}
            </TableRow>
          ))}
          {filtered.length === 0 && (
            <TableRow>
              <TableCell colSpan={columns.length + (showManage ? 3 : 2)} className="text-center text-xs text-muted-foreground">
                No matches.
              </TableCell>
            </TableRow>
          )}
        </TableBody>
      </Table>

      {note && <p className="text-[11px] text-muted-foreground">{note}</p>}
    </div>
  );
}
