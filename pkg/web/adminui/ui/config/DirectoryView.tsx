import { ResourceSection } from "./ResourceSection";
import { COLUMNS } from "./columns";

// Directory syncs (RelationshipSource) — distinct from the Sources view, which
// is skill sources synced from git. Both are "sources" in English and nothing
// else.
export function DirectoryView({ apiBase }: { apiBase: string }) {
  return (
    <ResourceSection
      apiBase={apiBase}
      resource="directory"
      entity="directory"
      columns={COLUMNS.directory}
      note="RelationshipSource — Slack · GitHub · 1Password · managed via oap directory configure"
      emptyText="No directory sources configured yet."
    />
  );
}
