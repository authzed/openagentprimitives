import * as React from "react";
import { User, Users } from "lucide-react";
import {
  Alert, AlertDescription, Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@ap/design";
import { getAccess, type AccessData } from "../lib/api";
import { CopyCmd } from "../lib/CopyCmd";
import { decodeSubject } from "../lib/decodeSubject";

function StatTile({ label, value, sub }: { label: string; value: string; sub: string }) {
  return (
    <div className="rounded-xl border bg-card p-4 shadow">
      <div className="text-[11px] text-muted-foreground">{label}</div>
      <div className="mt-1 text-2xl font-semibold">{value}</div>
      <div className="text-[11px] text-muted-foreground">{sub}</div>
    </div>
  );
}

// AccessView is the bespoke platform Access panel: who holds platform admin
// (from SpiceDB), the read-only grant command, and a degraded SpiceDB stats
// block that renders an "unavailable" state gracefully (Available is false in
// v1 — wiring a live source is a follow-up).
export function AccessView({ apiBase }: { apiBase: string }) {
  const [data, setData] = React.useState<AccessData | null>(null);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    let active = true;
    getAccess(apiBase)
      .then((d) => { if (active) { setData(d); setError(null); } })
      .catch((e: Error) => { if (active) setError(e.message); });
    return () => { active = false; };
  }, [apiBase]);

  if (error) {
    return (
      <Alert variant="destructive">
        <AlertDescription>Could not load access: {error}</AlertDescription>
      </Alert>
    );
  }
  if (!data) {
    return <p className="text-sm text-muted-foreground">Loading access…</p>;
  }

  const statsAvailable = data.stats.available;

  return (
    <div className="space-y-4">
      <div>
        <div className="mb-2 text-xs font-semibold text-muted-foreground">
          Platform admins <span className="font-normal">· who can reach this admin UI (platform:platform#admin)</span>
        </div>
        {data.admins.length === 0 ? (
          <p className="text-sm text-muted-foreground">No platform admins granted yet.</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Subject</TableHead>
                <TableHead>Kind</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.admins.map((a) => (
                <TableRow key={a.subject}>
                  <TableCell className="font-mono text-xs text-foreground" title={a.subject}>{decodeSubject(a.subject)}</TableCell>
                  <TableCell>
                    <span className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
                      {a.kind === "group" ? <Users className="h-3.5 w-3.5" /> : <User className="h-3.5 w-3.5" />}
                      {a.kind}
                    </span>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </div>

      <div className="space-y-1.5">
        <CopyCmd text={data.grantCmd} />
        <p className="text-[11px] text-muted-foreground">
          Run via the CLI — the admin UI is read-only. Also: oap platform revoke-admin / list-admins.
        </p>
      </div>

      {statsAvailable ? (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
          <StatTile
            label="SpiceDB schema"
            value={data.stats.schemaDefs != null ? `${data.stats.schemaDefs}` : "—"}
            sub="definitions"
          />
          <StatTile
            label="Relationships"
            value={data.stats.relationships != null ? data.stats.relationships.toLocaleString() : "—"}
            sub="stored tuples"
          />
        </div>
      ) : (
        <div className="rounded-xl border border-dashed bg-card p-4 shadow">
          <div className="text-[11px] text-muted-foreground">SpiceDB stats</div>
          <div className="mt-1 text-sm text-muted-foreground">
            Stats unavailable — schema-definition & relationship counts aren't exposed through the
            permissions API in v1. Wiring a live source is a follow-up.
          </div>
        </div>
      )}

      <p className="text-[11px] text-muted-foreground">
        Authorization primitive — schema composed by the operator · oap spicedb apply-schema / oap spicedb check
      </p>
    </div>
  );
}
