import * as React from "react";
import { Alert, AlertDescription, Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@ap/design";
import { getToolCalls, sessionKey, type ToolCall } from "../lib/api";
import { EntityLink } from "../lib/EntityLink";
import { ResourceName } from "../lib/ResourceName";

// fmtWhen renders the dispatch timestamp as a local wall-clock time; a bad
// timestamp falls back to the raw string rather than throwing.
function fmtWhen(t: string): string {
  const d = new Date(t);
  return Number.isNaN(d.getTime()) ? t : d.toLocaleTimeString();
}

const POLL_MS = 4000;

export function ToolCallsView({ apiBase }: { apiBase: string }) {
  const [rows, setRows] = React.useState<ToolCall[]>([]);
  const [error, setError] = React.useState<string | null>(null);
  const [loaded, setLoaded] = React.useState(false);

  React.useEffect(() => {
    let active = true;
    const load = () => {
      getToolCalls(apiBase)
        .then((r) => { if (active) { setRows(r); setError(null); setLoaded(true); } })
        .catch((e: Error) => { if (active) { setError(e.message); setLoaded(true); } });
    };
    load();
    const id = setInterval(load, POLL_MS);
    return () => { active = false; clearInterval(id); };
  }, [apiBase]);

  return (
    <div className="space-y-3">
      {error && (
        <Alert variant="destructive">
          <AlertDescription>Could not load tool calls: {error}</AlertDescription>
        </Alert>
      )}
      {loaded && rows.length === 0 && !error && (
        <p className="text-sm text-muted-foreground">No tool calls yet — they appear here as agents dispatch them.</p>
      )}
      {rows.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>When</TableHead>
              <TableHead>Session</TableHead>
              <TableHead>Agent</TableHead>
              <TableHead>Tool</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((c, i) => (
              <TableRow key={`${sessionKey(c)}-${c.t}-${i}`}>
                <TableCell className="whitespace-nowrap font-mono text-xs text-muted-foreground">{fmtWhen(c.t)}</TableCell>
                <TableCell className="font-mono text-xs">
                  <EntityLink entity="session" id={sessionKey(c)} className="text-link hover:underline">
                    <ResourceName id={sessionKey(c)} />
                  </EntityLink>
                </TableCell>
                <TableCell className="text-xs">
                  {c.agentClass ? (
                    <EntityLink entity="agent" id={`${c.namespace}/${c.agentClass}`} className="text-link hover:underline">
                      {c.agentClass}
                    </EntityLink>
                  ) : (
                    "(unknown)"
                  )}
                </TableCell>
                {/* Tool cell is plain text: a ToolCall carries only a tool NAME,
                    not a CR reference, so a 1:1 tool→page link needs a name→CR
                    resolution that does not exist yet (tracked as deferred). */}
                <TableCell className="font-mono text-xs text-foreground">{c.tool || "(tool)"}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
}
