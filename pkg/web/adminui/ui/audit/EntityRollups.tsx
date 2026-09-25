import * as React from "react";
import { Alert, AlertDescription, Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@ap/design";
import { getJSON, type AuditQueryRequest, type EntityRow } from "../lib/api";
import { EntityLink } from "../lib/EntityLink";
import { KindDot } from "../lib/KindBadge";
import { ResourceName } from "../lib/ResourceName";
import type { EntityKind } from "../lib/router";
import { statusPillClass } from "../lib/status";
import { decodeSubject } from "../lib/decodeSubject";
import { fmtTokens, fmtRelative } from "../lib/fmt";

export type Axis = "agents" | "sessions" | "tools" | "users";

// AXIS_ENTITY maps a rollup axis to the detail EntityKind its key opens. The
// sessions key ("ns/name") resolves exactly to a Session page; the agents/tools/
// users keys are the audit-side identifiers (agent class / tool name / actor
// subject) used as the detail id sensibly — a dead deep-link degrades to the
// detail page's own "not found" state rather than fabricating anything.
const AXIS_ENTITY: Record<Axis, EntityKind> = {
  agents: "agent",
  sessions: "session",
  tools: "tool",
  users: "user",
};

// drillFilter maps a rollup row to the audit-tab filter it deep-links to.
function drillFilter(axis: Axis, key: string): AuditQueryRequest {
  switch (axis) {
    case "agents":
      return { agentClass: key };
    case "sessions": {
      const i = key.indexOf("/");
      if (i < 0) return { sessionName: key };
      return { sessionNamespace: key.slice(0, i), sessionName: key.slice(i + 1) };
    }
    case "tools":
      return { tool: key };
    case "users":
      return { actor: key };
  }
}

// keyLabel is the human display for a row's key. The users axis carries a
// canonical subject (user:<base64url email>); everything else is already human.
function keyLabel(axis: Axis, key: string): string {
  return axis === "users" ? decodeSubject(key) : key;
}

// StatusPill shows a session's live phase as a colored pill; empty phase (the
// session GC'd out of the live aggregator) renders a muted dash instead.
function StatusPill({ status }: { status?: string }) {
  if (!status) return <span className="text-muted-foreground">—</span>;
  return (
    <span className={`inline-flex rounded-full border px-2 py-0.5 text-[10px] font-medium ${statusPillClass(status)}`}>
      {status}
    </span>
  );
}

export function EntityRollups({
  apiBase, axis, onDrill,
}: { apiBase: string; axis: Axis; onDrill: (f: AuditQueryRequest) => void }) {
  const [rows, setRows] = React.useState<EntityRow[]>([]);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    let active = true;
    getJSON<EntityRow[]>(`${apiBase}/audit/entities/${axis}`)
      .then((r) => { if (active) { setRows(r); setError(null); } })
      .catch((e: Error) => { if (active) setError(e.message); });
    return () => { active = false; };
  }, [apiBase, axis]);

  const isSessions = axis === "sessions";
  const entity = AXIS_ENTITY[axis];

  return (
    <div className="space-y-3">
      {error && (
        <Alert variant="destructive">
          <AlertDescription>Could not load rollups: {error}</AlertDescription>
        </Alert>
      )}
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="capitalize">{axis}</TableHead>
            {isSessions && <TableHead>Status</TableHead>}
            {isSessions && <TableHead className="text-right">Est. tokens</TableHead>}
            {isSessions && <TableHead>Started</TableHead>}
            {isSessions && <TableHead>Started by</TableHead>}
            <TableHead className="text-right">Events</TableHead>
            <TableHead className="text-right">Denied</TableHead>
            <TableHead className="text-right">Logs</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((r) => (
            <TableRow key={r.key}>
              <TableCell className="font-mono text-xs">
                <EntityLink entity={entity} id={r.key} className="text-link hover:underline">
                  {isSessions ? <ResourceName id={r.key} /> : keyLabel(axis, r.key)}
                </EntityLink>
              </TableCell>
              {isSessions && (
                <TableCell><StatusPill status={r.status} /></TableCell>
              )}
              {isSessions && (
                <TableCell
                  className="text-right font-mono text-xs text-muted-foreground"
                  title={r.estimatedCostUSD ? `~$${r.estimatedCostUSD.toFixed(2)} (est.)` : undefined}
                >
                  {r.inputTokens || r.outputTokens
                    ? `${fmtTokens(r.inputTokens ?? 0)} in · ${fmtTokens(r.outputTokens ?? 0)} out`
                    : "—"}
                </TableCell>
              )}
              {isSessions && (
                <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                  {fmtRelative(r.startedAt)}
                </TableCell>
              )}
              {isSessions && (
                <TableCell className="text-xs">
                  {r.startedBy ? (
                    <span className="inline-flex items-center gap-1.5">
                      {/* stable per-subject color dot so the same user reads the
                          same color across the console */}
                      <KindDot kind={r.startedBy} />
                      <EntityLink
                        entity="user"
                        id={r.startedBy}
                        className="font-mono text-link hover:underline"
                      >
                        {decodeSubject(r.startedBy)}
                      </EntityLink>
                    </span>
                  ) : (
                    <span className="font-mono text-muted-foreground">—</span>
                  )}
                </TableCell>
              )}
              <TableCell className="text-right font-mono text-xs">{r.events}</TableCell>
              <TableCell className="text-right font-mono text-xs text-destructive">{r.denied || ""}</TableCell>
              <TableCell className="text-right">
                <button
                  type="button"
                  className="text-xs text-muted-foreground hover:text-link hover:underline"
                  onClick={() => onDrill(drillFilter(axis, r.key))}
                >
                  filter logs
                </button>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <p className="text-[11px] text-muted-foreground">
        Row → {entity} detail · &ldquo;filter logs&rdquo; → the audit log scoped to that {entity}
      </p>
    </div>
  );
}
