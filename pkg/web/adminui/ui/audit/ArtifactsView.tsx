import * as React from "react";
import { Alert, AlertDescription, Badge, Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@ap/design";
import { getArtifacts, type ArtifactRow, type AuditQueryRequest } from "../lib/api";
import { EntityLink } from "../lib/EntityLink";
import { ResourceName } from "../lib/ResourceName";
import { buildTrackModel, TrackCell } from "./ArtifactTracks";

// PHASE_CLASS color-codes the ArtifactRender lifecycle phase pill.
const PHASE_CLASS: Record<string, string> = {
  Ready: "border-success/40 bg-success/10 text-success",
  Failed: "border-destructive/40 bg-destructive/10 text-destructive",
  Rendering: "border-primary/40 bg-primary/10 text-primary",
  Pending: "border-border bg-muted text-muted-foreground",
};

export function PhasePill({ phase }: { phase: string }) {
  const cls = PHASE_CLASS[phase] ?? "border-border bg-background text-muted-foreground";
  return (
    <span className={`inline-flex rounded-full border px-2 py-0.5 text-[10px] font-medium ${cls}`}>
      {phase || "—"}
    </span>
  );
}

export function fmtBytes(n: number): string {
  if (!n) return "—";
  const u = ["B", "KB", "MB", "GB"];
  let i = 0, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return `${i === 0 ? v : v.toFixed(1)}${u[i]}`;
}

// sessionFilter maps "ns/name" to the audit-tab filter the row deep-links to.
function sessionFilter(session: string): AuditQueryRequest {
  const i = session.indexOf("/");
  if (i < 0) return { sessionName: session };
  return { sessionNamespace: session.slice(0, i), sessionName: session.slice(i + 1) };
}

// ArtifactsView lists every ArtifactRender cluster-wide (newest-first). The
// Artifact name opens that render's detail page; a row with an owning session
// also deep-links to that session's Logs (the Session cell) via onDrill. The
// far-right Tracks column draws a git-commit-graph: revisions of one logical
// artifact (grouped by namespace + artifactId) share a colored lane, one dot
// per revision, connected top-to-bottom — see ArtifactTracks.
export function ArtifactsView({
  apiBase, onDrill,
}: { apiBase: string; onDrill?: (f: AuditQueryRequest) => void }) {
  const [rows, setRows] = React.useState<ArtifactRow[]>([]);
  const [error, setError] = React.useState<string | null>(null);
  const [loaded, setLoaded] = React.useState(false);

  React.useEffect(() => {
    let active = true;
    getArtifacts(apiBase)
      .then((r) => { if (active) { setRows(r); setError(null); setLoaded(true); } })
      .catch((e: Error) => { if (active) { setError(e.message); setLoaded(true); } });
    return () => { active = false; };
  }, [apiBase]);

  // buildTrackModel keys latest-per-artifact + lane/color on (namespace,
  // artifactId), so the name-color decision and the git-graph agree.
  const track = React.useMemo(() => buildTrackModel(rows), [rows]);

  if (error) {
    return (
      <Alert variant="destructive">
        <AlertDescription>Could not load artifacts: {error}</AlertDescription>
      </Alert>
    );
  }

  return (
    <div className="space-y-3">
      {loaded && rows.length === 0 && (
        <p className="text-sm text-muted-foreground">No artifacts rendered yet.</p>
      )}
      {rows.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>When</TableHead>
              <TableHead>Artifact</TableHead>
              <TableHead>Session</TableHead>
              <TableHead>Kind</TableHead>
              <TableHead>MIME</TableHead>
              <TableHead className="text-right">Size</TableHead>
              <TableHead className="text-right">Revisions</TableHead>
              <TableHead>Phase</TableHead>
              <TableHead className="text-right">Tracks</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((a, i) => {
              const drillable = !!(a.session && onDrill);
              const rt = track.rows[i];
              // Only the latest revision of a logical artifact renders its name in
              // the primary (link) color; older revisions render muted so the
              // current version stands out at a glance.
              const isLatest = rt.isLatest;
              return (
                <TableRow key={rt.uid}>
                  <TableCell className="whitespace-nowrap font-mono text-xs text-muted-foreground">
                    {new Date(a.created).toLocaleString()}
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    <EntityLink
                      entity="artifact"
                      id={`${a.namespace}/${a.name}`}
                      className={
                        isLatest
                          ? "text-link hover:underline"
                          : "text-muted-foreground hover:underline"
                      }
                    >
                      {a.name}
                    </EntityLink>
                  </TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">
                    {a.session ? (
                      drillable ? (
                        <button
                          type="button"
                          className="hover:text-link hover:underline"
                          onClick={() => onDrill!(sessionFilter(a.session))}
                        >
                          <ResourceName id={a.session} />
                        </button>
                      ) : (
                        <ResourceName id={a.session} />
                      )
                    ) : (
                      "—"
                    )}
                  </TableCell>
                  <TableCell><Badge variant="outline">{a.kind}</Badge></TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">{a.mime || "—"}</TableCell>
                  <TableCell className="text-right font-mono text-xs text-muted-foreground">{fmtBytes(a.size)}</TableCell>
                  <TableCell className="text-right font-mono text-xs text-muted-foreground">{a.revisions || ""}</TableCell>
                  <TableCell><PhasePill phase={a.phase} /></TableCell>
                  <TableCell className="p-0 pl-2 align-middle">
                    <TrackCell row={rt} width={track.width} />
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      )}
      <p className="text-[11px] text-muted-foreground">
        ArtifactRender — versioned, sanitized agent outputs · click the name → artifact detail · the session → its Logs · Tracks = git-graph of revisions per logical artifact · oap artifact
      </p>
    </div>
  );
}
