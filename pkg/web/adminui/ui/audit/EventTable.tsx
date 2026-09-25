import { Badge, Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@ap/design";
import type { AuditEvent } from "../lib/api";
import { decodeSubject } from "../lib/decodeSubject";
import { EntityLink } from "../lib/EntityLink";

// stopRow keeps a nested EntityLink click from also firing the row's onClick
// (which opens the event drawer) — the link navigates, the drawer stays closed.
const stopRow = (e: { stopPropagation: () => void }) => e.stopPropagation();

const outcomeVariant = (o: string) =>
  o === "denied" || o === "read_denied" || o === "failed" ? "destructive" : o === "pending" ? "secondary" : "outline";

// The five audit dimensions the Logs view colors with an ordinal scale. Each
// spec knows how to project an event to the display string that is BOTH the
// legend chip label and the color-map key, so AuditExplorer (which builds the
// maps) and EventTable (which colors the cells) agree on every value. The actor
// is decoded from its canonical subject to the human email for display.
export type LogColorField = "kind" | "outcome" | "agent" | "tool" | "actor";

export interface LogColorFieldSpec {
  key: LogColorField;
  label: string;
  value: (e: AuditEvent) => string;
}

export const LOG_COLOR_FIELDS: readonly LogColorFieldSpec[] = [
  { key: "kind", label: "Kind", value: (e) => e.kind },
  { key: "outcome", label: "Outcome", value: (e) => e.outcome },
  { key: "agent", label: "Agent", value: (e) => e.agentClass ?? "" },
  { key: "tool", label: "Tool", value: (e) => e.tool ?? "" },
  { key: "actor", label: "Actor", value: (e) => (e.actor ? decodeSubject(e.actor) : "") },
];

// LogColorMaps holds one value→color map per colored field.
export type LogColorMaps = Record<LogColorField, Map<string, string>>;

// eventFieldValues projects one event to its per-field display strings, so a
// cell's swatch keys the same value the legend map was built from.
function eventFieldValues(e: AuditEvent): Record<LogColorField, string> {
  return {
    kind: e.kind,
    outcome: e.outcome,
    agent: e.agentClass ?? "",
    tool: e.tool ?? "",
    actor: e.actor ? decodeSubject(e.actor) : "",
  };
}

// Swatch is the small category dot; renders nothing when there is no color (an
// empty/uncategorized value, or when no color maps were passed).
function Swatch({ color }: { color?: string }) {
  if (!color) return null;
  return (
    <span
      className="mr-1.5 inline-block h-2 w-2 shrink-0 rounded-full align-middle"
      style={{ backgroundColor: color }}
    />
  );
}

// ColorLegend renders one compact legend per colored field: a row of chips, each
// the category value beside its swatch. Fields with no distinct values (nothing
// loaded, or all-empty) show a muted dash.
export function ColorLegend({ colorMaps }: { colorMaps: LogColorMaps }) {
  return (
    <div
      role="group"
      aria-label="Color legends"
      className="grid gap-2 rounded-md border border-border bg-card p-2 text-xs sm:grid-cols-2 lg:grid-cols-3"
    >
      {LOG_COLOR_FIELDS.map((f) => {
        const entries = [...colorMaps[f.key].entries()];
        return (
          <div key={f.key} className="min-w-0">
            <div className="mb-1 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">{f.label}</div>
            {entries.length === 0 ? (
              <span className="text-muted-foreground">—</span>
            ) : (
              <div className="flex flex-wrap gap-1">
                {entries.map(([val, color]) => (
                  <span
                    key={val}
                    className="inline-flex max-w-full items-center gap-1 rounded border border-border bg-background px-1.5 py-0.5 font-mono"
                  >
                    <span className="h-2 w-2 shrink-0 rounded-full" style={{ backgroundColor: color }} />
                    <span className="truncate">{val}</span>
                  </span>
                ))}
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}

export function EventTable({
  events, onRow, colorMaps,
}: {
  events: AuditEvent[];
  onRow: (e: AuditEvent) => void;
  // colorMaps colors the kind/outcome/agent/tool/actor cells; omit for a plain
  // (uncolored) table.
  colorMaps?: LogColorMaps;
}) {
  const colorOf = (field: LogColorField, value: string): string | undefined =>
    colorMaps?.[field].get(value);

  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>When</TableHead>
          <TableHead>Kind</TableHead>
          <TableHead>Outcome</TableHead>
          <TableHead>Agent</TableHead>
          <TableHead>Tool</TableHead>
          <TableHead>Actor</TableHead>
          <TableHead>Session</TableHead>
          <TableHead>Event</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {events.map((e) => {
          const v = eventFieldValues(e);
          return (
            <TableRow
              key={e.entryId}
              className={`cursor-pointer${e.outcome === "denied" || e.outcome === "read_denied" ? " bg-destructive/5" : ""}`}
              onClick={() => onRow(e)}
            >
              <TableCell className="whitespace-nowrap font-mono text-xs">{new Date(e.time).toLocaleString()}</TableCell>
              <TableCell className="whitespace-nowrap">
                <Swatch color={colorOf("kind", v.kind)} />
                <Badge variant="outline">{e.kind}</Badge>
              </TableCell>
              <TableCell className="whitespace-nowrap">
                <Swatch color={colorOf("outcome", v.outcome)} />
                <Badge variant={outcomeVariant(e.outcome)}>{e.outcome}</Badge>
              </TableCell>
              <TableCell className="font-mono text-xs">
                <Swatch color={colorOf("agent", v.agent)} />
                {e.agentClass ? (
                  <EntityLink
                    entity="agent"
                    id={`${e.sessionNamespace}/${e.agentClass}`}
                    className="text-link hover:underline"
                    onClick={stopRow}
                  >
                    {e.agentClass}
                  </EntityLink>
                ) : (
                  "—"
                )}
              </TableCell>
              <TableCell className="font-mono text-xs">
                <Swatch color={colorOf("tool", v.tool)} />
                {e.tool || "—"}
              </TableCell>
              <TableCell className="font-mono text-xs" title={e.actor || undefined}>
                <Swatch color={colorOf("actor", v.actor)} />
                {v.actor || "—"}
              </TableCell>
              <TableCell className="font-mono text-xs">
                <EntityLink
                  entity="session"
                  id={`${e.sessionNamespace}/${e.sessionName}`}
                  className="text-link hover:underline"
                  onClick={stopRow}
                >
                  {e.sessionNamespace}/{e.sessionName}
                </EntityLink>
              </TableCell>
              <TableCell className="max-w-md truncate text-xs">{e.summary}</TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}
