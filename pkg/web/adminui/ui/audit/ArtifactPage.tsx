import * as React from "react";
import { Alert, AlertDescription, Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@ap/design";
import { DetailPage, type DetailTab } from "../shell/DetailPage";
import { EntityLink } from "../lib/EntityLink";
import { ResourceName } from "../lib/ResourceName";
import { navigate } from "../lib/router";
import { ApiError, getArtifactDetail, type ArtifactDetail } from "../lib/api";
import { PhasePill, fmtBytes } from "./ArtifactsView";

const TABS: DetailTab[] = [
  { id: "overview", label: "Overview" },
  { id: "revisions", label: "Revisions" },
];

// splitId splits an "ns/name" artifact id. Namespaces/names never contain "/",
// so a single split on the first slash is exact.
function splitId(id: string): { ns: string; name: string } {
  const i = id.indexOf("/");
  return i < 0 ? { ns: "", name: id } : { ns: id.slice(0, i), name: id.slice(i + 1) };
}

// hasViewerLink reports whether viewPath is a usable admin viewer URL — i.e. it
// carries the `artifactId` param. admind emits
// /artifact-view?artifactId=…&sessionRef=… (no signed link needed: the viewer
// authenticates the admin via their IdP cookie and authorizes via CheckView), so
// we surface the "Open viewer" link whenever that param is present. A bare
// /artifact-view base (no artifactId) is not openable and stays hidden.
function hasViewerLink(p: string): boolean {
  try {
    const u = new URL(p, "http://placeholder.invalid");
    return u.searchParams.has("artifactId");
  } catch {
    return false;
  }
}

// Fact is one labeled metadata cell.
function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-0.5">
      <dt className="text-[10px] uppercase tracking-wide text-muted-foreground">{label}</dt>
      <dd className="font-mono text-xs text-foreground">{children}</dd>
    </div>
  );
}

// ArtifactPage is the ArtifactRender detail (Overview / Revisions tabs). It owns
// its own fetch of the B5 /artifacts/{ns}/{name} endpoint; a 404 renders a clean
// "not found" state rather than blanking the page.
export function ArtifactPage({ apiBase, id, tab }: { apiBase: string; id: string; tab?: string }) {
  const { ns, name } = splitId(id);
  const activeTab = tab ?? "overview";

  const [detail, setDetail] = React.useState<ArtifactDetail | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [notFound, setNotFound] = React.useState(false);

  React.useEffect(() => {
    let active = true;
    setDetail(null);
    setError(null);
    setNotFound(false);
    getArtifactDetail(apiBase, ns, name)
      .then((d) => { if (active) setDetail(d); })
      .catch((e: Error) => {
        if (!active) return;
        if (e instanceof ApiError && e.status === 404) setNotFound(true);
        else setError(e.message);
      });
    return () => { active = false; };
  }, [apiBase, ns, name]);

  const onTab = (t: string) => navigate({ type: "detail", entity: "artifact", id, tab: t });

  return (
    <DetailPage
      title={<span className="font-mono"><ResourceName ns={ns} name={name} /></span>}
      backRoute={{ type: "view", view: "artifacts" }}
      tabs={TABS}
      activeTab={activeTab}
      onTab={onTab}
      headerRight={detail ? <PhasePill phase={detail.phase} /> : undefined}
    >
      {error ? (
        <Alert variant="destructive">
          <AlertDescription>Could not load artifact: {error}</AlertDescription>
        </Alert>
      ) : notFound ? (
        <p className="text-sm text-muted-foreground">Artifact {id} not found — it may have been garbage-collected.</p>
      ) : !detail ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : activeTab === "revisions" ? (
        <Revisions detail={detail} />
      ) : (
        <Overview detail={detail} />
      )}
    </DetailPage>
  );
}

function Overview({ detail }: { detail: ArtifactDetail }) {
  const hasViewer = hasViewerLink(detail.viewPath);
  return (
    <div className="flex flex-col gap-5">
      <dl className="grid grid-cols-2 gap-x-6 gap-y-3 sm:grid-cols-3">
        <Fact label="Kind">{detail.kind || "—"}</Fact>
        <Fact label="Phase"><PhasePill phase={detail.phase} /></Fact>
        <Fact label="MIME">{detail.mime || "—"}</Fact>
        <Fact label="Size">{fmtBytes(detail.size)}</Fact>
        <Fact label="Created">{new Date(detail.created).toLocaleString()}</Fact>
        <Fact label="Session">
          {detail.session ? (
            <EntityLink entity="session" id={detail.session} className="text-link hover:underline">
              <ResourceName id={detail.session} />
            </EntityLink>
          ) : (
            "—"
          )}
        </Fact>
      </dl>

      <div className="flex flex-col gap-2">
        <span className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">Viewer</span>
        {hasViewer ? (
          <a
            href={detail.viewPath}
            target="_blank"
            rel="noopener noreferrer"
            className="w-fit text-sm text-link hover:underline"
          >
            Open viewer
          </a>
        ) : (
          <span className="text-[11px] text-muted-foreground">No viewer available for this artifact.</span>
        )}
      </div>

      <div className="flex flex-col gap-2">
        <span className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">Download</span>
        {detail.outputRef ? (
          <code className="w-fit break-all rounded border border-border bg-background px-2 py-1 font-mono text-xs text-foreground">
            {detail.outputRef}
          </code>
        ) : (
          <span className="text-sm text-muted-foreground">No content ref yet — the render has not reached Ready.</span>
        )}
      </div>
    </div>
  );
}

function Revisions({ detail }: { detail: ArtifactDetail }) {
  if (detail.revisions.length === 0) {
    return <p className="text-sm text-muted-foreground">No revisions.</p>;
  }
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Name</TableHead>
          <TableHead>Phase</TableHead>
          <TableHead>MIME</TableHead>
          <TableHead className="text-right">Size</TableHead>
          <TableHead>Created</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {detail.revisions.map((r) => (
          <TableRow key={r.name} className={r.name === detail.name ? "bg-muted/40" : undefined}>
            <TableCell className="font-mono text-xs">
              <EntityLink
                entity="artifact"
                id={`${detail.namespace}/${r.name}`}
                className="text-link hover:underline"
              >
                {r.name}
              </EntityLink>
            </TableCell>
            <TableCell><PhasePill phase={r.phase} /></TableCell>
            <TableCell className="font-mono text-xs text-muted-foreground">{r.mime || "—"}</TableCell>
            <TableCell className="text-right font-mono text-xs text-muted-foreground">{fmtBytes(r.size)}</TableCell>
            <TableCell className="whitespace-nowrap font-mono text-xs text-muted-foreground">
              {new Date(r.created).toLocaleString()}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
