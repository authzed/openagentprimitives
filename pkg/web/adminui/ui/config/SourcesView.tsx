import { ResourceSection } from "./ResourceSection";
import { COLUMNS, type ColumnSpec } from "./columns";
import { badgeVal } from "./detail/shared";
import { httpUrl } from "../lib/safeUrl";

// The Repo column renders the (user-supplied, untrusted) upstream URL as an
// external link — hardened with rel="noopener noreferrer nofollow", opening in a
// new tab — mirroring the SourcePage detail. The anchor renders ONLY when
// httpUrl() confirms an http/https scheme; a javascript:/data: repo degrades to
// plain text so it can't become a clickable script. Other columns stay generic.
const sourceColumns: ColumnSpec[] = COLUMNS.sources.map((c) =>
  c.key === "repo"
    ? {
        ...c,
        render: (row) => {
          const repo = badgeVal(row, "repo");
          if (!repo) return <span className="text-muted-foreground">—</span>;
          if (!httpUrl(repo)) {
            return <code className="whitespace-nowrap font-mono text-[11px] text-muted-foreground">{repo}</code>;
          }
          return (
            <a
              href={repo}
              target="_blank"
              rel="noopener noreferrer nofollow"
              className="whitespace-nowrap font-mono text-[11px] text-link hover:underline"
              // Row-level EntityLink navigation lives on the Name cell; keep a
              // repo click from also triggering it.
              onClick={(e) => e.stopPropagation()}
            >
              {repo}
            </a>
          );
        },
      }
    : c,
);

export function SourcesView({ apiBase }: { apiBase: string }) {
  return (
    <ResourceSection
      apiBase={apiBase}
      resource="sources"
      entity="source"
      columns={sourceColumns}
      note="SkillSource · ClusterSkillSource — synced from git · managed via oap skill source add"
      emptyText="No skill sources configured yet."
    />
  );
}
