import * as React from "react";
import {
  Alert, AlertDescription,
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@ap/design";
import { getConfigSettings, type SettingsData, type SettingsRow } from "../lib/api";
import { CopyCmd } from "../lib/CopyCmd";

// Render Limits before Defaults; any other group falls through after, in first-
// seen order, so an added group never silently disappears.
const GROUP_ORDER = ["Limits", "Defaults"];

// orderRows flattens the rows into a single table order (Limits, then Defaults,
// then any other group first-seen), tagging the first row of each group so the
// Group column reads once per block rather than repeating on every line.
function orderRows(rows: SettingsRow[]): { row: SettingsRow; group?: string }[] {
  const byGroup = new Map<string, SettingsRow[]>();
  for (const r of rows) {
    const list = byGroup.get(r.group) ?? [];
    list.push(r);
    byGroup.set(r.group, list);
  }
  const ordered = [...byGroup.keys()].sort((a, b) => {
    const ia = GROUP_ORDER.indexOf(a);
    const ib = GROUP_ORDER.indexOf(b);
    if (ia === -1 && ib === -1) return 0;
    if (ia === -1) return 1;
    if (ib === -1) return -1;
    return ia - ib;
  });
  return ordered.flatMap((g) =>
    (byGroup.get(g) as SettingsRow[]).map((row, i) => ({ row, group: i === 0 ? g : undefined })),
  );
}

// SettingsView is the bespoke cluster Settings panel: the singleton
// ClusterAgentSettings projected into a Group / Setting / Value / Note table,
// plus the read-only manage command. When no ClusterAgentSettings exists the
// server returns an explanatory note and empty rows — rendered as a gentle
// empty state.
export function SettingsView({ apiBase }: { apiBase: string }) {
  const [data, setData] = React.useState<SettingsData | null>(null);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    let active = true;
    getConfigSettings(apiBase)
      .then((d) => { if (active) { setData(d); setError(null); } })
      .catch((e: Error) => { if (active) setError(e.message); });
    return () => { active = false; };
  }, [apiBase]);

  if (error) {
    return (
      <Alert variant="destructive">
        <AlertDescription>Could not load settings: {error}</AlertDescription>
      </Alert>
    );
  }
  if (!data) {
    return <p className="text-sm text-muted-foreground">Loading settings…</p>;
  }

  const rows = orderRows(data.rows);

  return (
    <div className="space-y-4">
      {data.rows.length === 0 ? (
        <div className="rounded-xl border border-dashed bg-card p-4 shadow">
          <p className="text-sm text-muted-foreground">
            {data.note || "No ClusterAgentSettings configured — cluster governance uses built-in defaults."}
          </p>
        </div>
      ) : (
        <div className="rounded-xl border bg-card shadow">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-28">Group</TableHead>
                <TableHead>Setting</TableHead>
                <TableHead>Value</TableHead>
                <TableHead>Note</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map(({ row, group }, i) => (
                <TableRow key={`${row.group}-${row.label}-${i}`}>
                  <TableCell className="align-top text-xs font-semibold text-muted-foreground">
                    {group ?? ""}
                  </TableCell>
                  <TableCell className="align-top text-xs text-muted-foreground">{row.label}</TableCell>
                  <TableCell className="align-top font-mono text-xs text-foreground">{row.value}</TableCell>
                  <TableCell className="align-top text-[11px] text-muted-foreground">{row.note ?? ""}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      <CopyCmd text={data.manageCmd} />
      <p className="text-[11px] text-muted-foreground">
        ClusterAgentSettings (singleton <span className="font-mono">cluster</span>) → namespace AgentSettings →
        AgentClass → session. Edit via the CLI — the admin UI is read-only.
      </p>
    </div>
  );
}
