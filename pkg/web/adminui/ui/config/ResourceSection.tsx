import * as React from "react";
import { Alert, AlertDescription } from "@ap/design";
import { getConfigResource, type ResourceRow } from "../lib/api";
import type { EntityKind } from "../lib/router";
import { ResourceTable, type FacetSpec } from "./ResourceTable";
import type { ColumnSpec } from "./columns";

// ResourceSection is the fetch+render shell shared by every table-backed config
// view: it loads one projector slug, surfaces a fetch error in a banner, and
// hands the rows to the generic ResourceTable. The per-section views are thin
// wrappers that pick the slug, columns, and note. Uses the active-flag guard so
// a slow response after unmount is dropped.
export function ResourceSection({
  apiBase, resource, columns, entity, facet, mapRows, note, emptyText,
}: {
  apiBase: string;
  resource: string;
  columns: ColumnSpec[];
  // entity links each row's Name cell to that entity's detail page.
  entity?: EntityKind;
  // facet adds an optional badge-value filter (e.g. identities by cred type).
  facet?: FacetSpec;
  // mapRows lets a view post-process projector rows before display (Channels
  // rewrites a monitoring channel's status from "Unknown" to "monitoring").
  mapRows?: (rows: ResourceRow[]) => ResourceRow[];
  note?: string;
  emptyText?: string;
}) {
  const [rows, setRows] = React.useState<ResourceRow[]>([]);
  const [error, setError] = React.useState<string | null>(null);
  const [loaded, setLoaded] = React.useState(false);

  React.useEffect(() => {
    let active = true;
    getConfigResource(apiBase, resource)
      .then((r) => { if (active) { setRows(r ?? []); setError(null); setLoaded(true); } })
      .catch((e: Error) => { if (active) { setError(e.message); setLoaded(true); } });
    return () => { active = false; };
  }, [apiBase, resource]);

  const shown = mapRows ? mapRows(rows) : rows;

  return (
    <div className="space-y-3">
      {error && (
        <Alert variant="destructive">
          <AlertDescription>Could not load {resource}: {error}</AlertDescription>
        </Alert>
      )}
      {!error && (
        <ResourceTable
          rows={shown}
          columns={columns}
          entity={entity}
          facet={facet}
          statusCookieKey={`status_${resource}`}
          loading={!loaded}
          note={note}
          emptyText={emptyText}
        />
      )}
    </div>
  );
}
