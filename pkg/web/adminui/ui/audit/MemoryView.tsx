import * as React from "react";
import { Search } from "lucide-react";
import { Alert, AlertDescription, Badge, Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@ap/design";
import { getMemory, type MemoryData } from "../lib/api";

// MemoryView is the Audit › Memory browser: per-Kind rollups by default, and a
// ranked search over the authorized memory store when a query is entered.
export function MemoryView({ apiBase }: { apiBase: string }) {
  const [input, setInput] = React.useState("");
  const [query, setQuery] = React.useState("");
  const [data, setData] = React.useState<MemoryData | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [loaded, setLoaded] = React.useState(false);

  React.useEffect(() => {
    let active = true;
    getMemory(apiBase, query ? { q: query } : {})
      .then((d) => { if (active) { setData(d); setError(null); setLoaded(true); } })
      .catch((e: Error) => { if (active) { setError(e.message); setLoaded(true); } });
    return () => { active = false; };
  }, [apiBase, query]);

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    setQuery(input.trim());
  };

  const rollups = data?.rollups ?? [];
  const entries = data?.entries ?? [];
  const searching = query !== "";

  return (
    <div className="space-y-3">
      <form onSubmit={onSubmit} className="flex items-center gap-2">
        <label className="flex h-9 flex-1 items-center gap-2 rounded-md border border-input bg-background px-3 text-xs">
          <Search className="h-3.5 w-3.5 text-muted-foreground" />
          <input
            type="search"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder="search memory — text, tags, vectors…"
            aria-label="Search memory"
            className="flex-1 bg-transparent text-xs text-foreground outline-none placeholder:text-muted-foreground"
          />
        </label>
      </form>

      {error && (
        <Alert variant="destructive">
          <AlertDescription>Could not load memory: {error}</AlertDescription>
        </Alert>
      )}
      {data?.truncated && (
        <Alert>
          <AlertDescription>Results truncated — narrow the scan for complete rollup counts.</AlertDescription>
        </Alert>
      )}

      {searching ? (
        entries.length === 0 ? (
          <p className="text-sm text-muted-foreground">No matches for “{query}”.</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Kind</TableHead>
                <TableHead>Scope</TableHead>
                <TableHead>Entry</TableHead>
                <TableHead>When</TableHead>
                <TableHead className="text-right">Score</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {entries.map((e) => (
                <TableRow key={`${e.scope}/${e.kind}/${e.id}`}>
                  <TableCell><Badge variant="outline">{e.kind}</Badge></TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">{e.scope}</TableCell>
                  <TableCell className="font-mono text-xs text-foreground">{e.id}</TableCell>
                  <TableCell className="whitespace-nowrap font-mono text-xs text-muted-foreground">
                    {e.created ? new Date(e.created).toLocaleString() : "—"}
                  </TableCell>
                  <TableCell className="text-right font-mono text-xs text-muted-foreground">
                    {e.score != null ? e.score.toFixed(2) : ""}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )
      ) : rollups.length === 0 ? (
        loaded && <p className="text-sm text-muted-foreground">No memory entries yet.</p>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Kind</TableHead>
              <TableHead className="text-right">Entries</TableHead>
              <TableHead className="text-right">Scopes</TableHead>
              <TableHead>Last write</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {rollups.map((r) => (
              <TableRow key={r.kind}>
                <TableCell className="font-mono text-xs text-foreground">{r.kind}</TableCell>
                <TableCell className="text-right font-mono text-xs text-muted-foreground">{r.entries}</TableCell>
                <TableCell className="text-right font-mono text-xs text-muted-foreground">{r.scopes}</TableCell>
                <TableCell className="whitespace-nowrap font-mono text-xs text-muted-foreground">
                  {r.lastWrite ? new Date(r.lastWrite).toLocaleString() : "—"}
                </TableCell>
                <TableCell>{r.appendOnly && <Badge>append-only</Badge>}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      <p className="text-[11px] text-muted-foreground">
        Append-only kinds are tamper-evident (Ed25519 hash chains) — verify with{" "}
        <span className="font-mono">oap audit verify</span> · oap memory list / search / query
      </p>
    </div>
  );
}
