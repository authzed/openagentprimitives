import * as React from "react";
import { Alert, AlertDescription } from "@ap/design";
import { getConfigResource, type ResourceRow } from "../lib/api";
import { EntityLink } from "../lib/EntityLink";
import { ViewToggle } from "../lib/ViewToggle";
import { useCookieState } from "../lib/cookies";
import { ResourceTable } from "./ResourceTable";
import { COLUMNS, type ColumnSpec } from "./columns";
import { badgeVal } from "./detail/shared";
import { rowId } from "./detail/useResourceRow";
import { InstallAgentDialog } from "./InstallAgentDialog";

// healthy reads the row's primary condition as OK. AgentClass's primary word is
// "Valid"; we also treat Ready as healthy defensively.
function healthy(status: string): boolean {
  return status === "Valid" || status === "Ready";
}

function countVal(row: ResourceRow, label: string): number {
  return row.counts?.find((c) => c.label === label)?.value ?? 0;
}

// identityDotColor maps an AgentClass identityMode to a fixed semantic dot color
// so the two modes read distinctly at a glance: an agent-bound service identity
// (identityMode "agent") is one hue, a user-passthrough class (the caller's own
// credentials flow through) is another. Anything else (empty/unknown) is muted.
function identityDotColor(mode?: string): string {
  // Tokens, not Tailwind's own violet/teal: those were the only two colours in
  // the admin UI outside the design system and did not survive light mode.
  // --state (teal since palette round 5) is the product's one hue and marks
  // the agent-bound case; --success (neutral stone) marks the caller's own
  // credentials flowing through.
  if (mode === "agent") return "bg-state"; // teal — bound AgentIdentity
  if (mode === "userPassthrough") return "bg-success"; // stone — caller's creds
  return "bg-muted-foreground/40";
}

// IdentityCell renders an AgentClass's identity: a mode-colored dot (agent vs
// userPassthrough) followed by the bound AgentIdentity name. In the table the
// name links to the identity's detail page; in the card (`linked={false}`) it is
// plain text, because the whole card is already an anchor and nesting a second
// <a> is invalid HTML. Classes with no bound identity (userPassthrough) show the
// mode word as plain text after the dot.
export function IdentityCell({ row, linked = true }: { row: ResourceRow; linked?: boolean }) {
  const mode = badgeVal(row, "identityMode");
  const ref = badgeVal(row, "identityRef");
  return (
    <span className="inline-flex items-center gap-1.5">
      <span
        aria-hidden="true"
        title={mode || undefined}
        className={`inline-block h-2 w-2 shrink-0 rounded-full ${identityDotColor(mode)}`}
      />
      {ref && linked ? (
        <EntityLink
          entity="identity"
          id={row.namespace ? `${row.namespace}/${ref}` : ref}
          className="font-mono text-[11px] text-link hover:underline"
          onClick={(e) => e.stopPropagation()}
        >
          {ref}
        </EntityLink>
      ) : (
        <span className="font-mono text-[11px] text-muted-foreground">{ref || mode || "—"}</span>
      )}
    </span>
  );
}

// agentColumns overrides the generic "Identity" column with the mode-dot +
// identity-link cell (the projector's identityMode/identityRef badges).
const agentColumns: ColumnSpec[] = COLUMNS.agents.map((c) =>
  c.header === "Identity" ? { ...c, render: (row) => <IdentityCell row={row} /> } : c,
);

// AgentCard mirrors renderConfigAgents: a card per AgentClass with its name,
// status pill, the failure reason when not healthy, an identityMode chip, and a
// tools/skills rollup. The whole card links to the Agent detail page.
function AgentCard({ a }: { a: ResourceRow }) {
  const ok = healthy(a.status);
  const tools = countVal(a, "tools");
  const skills = countVal(a, "skills");
  return (
    <EntityLink
      entity="agent"
      id={rowId(a)}
      className="block rounded-xl border bg-card p-4 shadow transition-colors hover:border-primary/40"
    >
      <div className="flex items-center justify-between">
        <span className="font-mono text-sm font-medium text-foreground">{a.name}</span>
        <span
          title={a.statusReason || undefined}
          className={`inline-flex items-center rounded-full border px-2 py-0.5 text-[10px] ${
            ok ? "border-success/40 bg-success/10 text-success" : "border-destructive/40 bg-destructive/10 text-destructive"
          }`}
        >
          {a.status || "Unknown"}
        </span>
      </div>
      {a.namespace && <p className="mt-0.5 font-mono text-[10px] text-muted-foreground">{a.namespace}</p>}
      {!ok && a.statusReason && <p className="mt-2 text-xs text-destructive">{a.statusReason}</p>}
      <div className="mt-3 flex flex-wrap gap-1.5">
        <IdentityCell row={a} linked={false} />
      </div>
      <p className="mt-3 font-mono text-xs text-muted-foreground">
        {tools} tool{tools === 1 ? "" : "s"} · {skills} skill{skills === 1 ? "" : "s"}
      </p>
    </EntityLink>
  );
}

// AgentsView is the Config → Agents surface: AgentClasses shown as a card grid
// (default) or the generic table, toggled via ViewToggle and persisted in a
// cookie. Both views link each AgentClass to its detail page. Every AgentClass
// here is still managed via the CLI (oap class / kubectl agentclass) — the one
// write path the console offers is InstallAgentDialog, which lands a NEW
// AgentClass from a .oap bundle rather than editing an existing one.
export function AgentsView({ apiBase }: { apiBase: string }) {
  const [rows, setRows] = React.useState<ResourceRow[]>([]);
  const [error, setError] = React.useState<string | null>(null);
  const [loaded, setLoaded] = React.useState(false);
  const [mode, setMode] = useCookieState<"grid" | "table">("agents_view", "grid");

  const load = React.useCallback(() => {
    let active = true;
    getConfigResource(apiBase, "agents")
      .then((r) => { if (active) { setRows(r); setError(null); setLoaded(true); } })
      .catch((e: Error) => { if (active) { setError(e.message); setLoaded(true); } });
    return () => { active = false; };
  }, [apiBase]);

  React.useEffect(() => load(), [load]);

  const note = "AgentClass — readiness gates sessions · managed via oap class / kubectl agentclass";

  const body = (): React.ReactNode => {
    if (error) {
      return (
        <Alert variant="destructive">
          <AlertDescription>Could not load agents: {error}</AlertDescription>
        </Alert>
      );
    }
    if (!loaded) {
      return <p className="text-sm text-muted-foreground">Loading agents…</p>;
    }
    if (rows.length === 0) {
      return <p className="text-sm text-muted-foreground">No AgentClasses configured yet.</p>;
    }
    return mode === "grid" ? (
      <>
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3">
          {rows.map((a) => (
            <AgentCard key={rowId(a)} a={a} />
          ))}
        </div>
        <p className="text-[11px] text-muted-foreground">{note}</p>
      </>
    ) : (
      <ResourceTable rows={rows} columns={agentColumns} entity="agent" statusCookieKey="status_agents" note={note} />
    );
  };

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <InstallAgentDialog apiBase={apiBase} onInstalled={load} />
        {loaded && !error && rows.length > 0 && <ViewToggle value={mode} onChange={setMode} />}
      </div>
      {body()}
    </div>
  );
}
