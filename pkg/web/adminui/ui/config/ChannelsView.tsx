import { ResourceSection } from "./ResourceSection";
import { COLUMNS, type ColumnSpec } from "./columns";
import { badgeVal } from "./detail/shared";
import { EntityLink } from "../lib/EntityLink";
import type { ResourceRow } from "../lib/api";

// A channel is "monitoring" when its Role is the framework-event-sink role. A
// monitoring channel has no socket, so it never gains a Connected condition —
// the projector reports its status as "Unknown". We treat that as its own
// neutral "monitoring" state rather than a health unknown.
function isMonitoring(row: ResourceRow): boolean {
  return badgeVal(row, "role") === "monitoring";
}

// RoleChip colors the role: input/output/both are live message roles (accent),
// while monitoring is a passive sink (muted gray) — visually distinct so a
// monitoring channel doesn't read as a broken input/output channel.
function RoleChip({ row }: { row: ResourceRow }) {
  const role = badgeVal(row, "role");
  if (!role) return <span className="text-muted-foreground">—</span>;
  const muted = role === "monitoring";
  return (
    <span
      className={`rounded-full border px-2 py-0.5 text-[10px] font-mono ${
        muted
          ? "border-border bg-background text-muted-foreground"
          : "border-primary/40 bg-primary/10 text-primary"
      }`}
    >
      {role}
    </span>
  );
}

// AgentClassLink turns the agentClass into a link to that Agent's detail page
// (in the channel's own namespace). Monitoring channels have no bound agent.
function AgentClassLink({ row }: { row: ResourceRow }) {
  const cls = badgeVal(row, "agentClass");
  if (!cls) return <span className="text-muted-foreground">—</span>;
  const id = row.namespace ? `${row.namespace}/${cls}` : cls;
  return (
    <EntityLink entity="agent" id={id} className="font-mono text-[11px] text-link hover:underline">
      {cls}
    </EntityLink>
  );
}

const channelColumns: ColumnSpec[] = COLUMNS.channels.map((c) => {
  if (c.key === "role") return { ...c, render: (row) => <RoleChip row={row} /> };
  if (c.key === "agentClass") return { ...c, render: (row) => <AgentClassLink row={row} /> };
  return c;
});

// mapRows rewrites a monitoring channel's "Unknown" status to the neutral
// "monitoring" label so the Status column and StatusFilter show intent, not a
// spurious health-unknown.
function mapRows(rows: ResourceRow[]): ResourceRow[] {
  return rows.map((r) => (isMonitoring(r) ? { ...r, status: "monitoring" } : r));
}

export function ChannelsView({ apiBase }: { apiBase: string }) {
  return (
    <ResourceSection
      apiBase={apiBase}
      resource="channels"
      entity="channel"
      columns={channelColumns}
      mapRows={mapRows}
      note="Channel — where agents are reachable · managed via oap channel / kubectl channel"
      emptyText="No channels configured yet."
    />
  );
}
