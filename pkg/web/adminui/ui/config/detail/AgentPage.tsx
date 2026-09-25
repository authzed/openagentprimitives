import * as React from "react";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@ap/design";
import { EntityLink } from "../../lib/EntityLink";
import { ResourceName } from "../../lib/ResourceName";
import { fmtRelative } from "../../lib/fmt";
import { getJSON, type SessionState } from "../../lib/api";
import { PhasePill } from "../../live/SessionCard";
import { ConfigDetail } from "./ConfigDetail";

// AgentSessions fetches the cross-platform live-session list and lists the ones
// belonging to this AgentClass as links into their session detail pages. It is a
// best-effort tab — a session-fetch failure degrades to an inline note, never
// blanks the whole agent page.
function AgentSessions({ apiBase, agentClass }: { apiBase: string; agentClass: string }) {
  const [sessions, setSessions] = React.useState<SessionState[] | null>(null);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    let active = true;
    getJSON<SessionState[]>(`${apiBase}/sessions`)
      .then((s) => {
        if (active) {
          setSessions(s);
          setError(null);
        }
      })
      .catch((e: Error) => {
        if (active) setError(e.message);
      });
    return () => {
      active = false;
    };
  }, [apiBase]);

  if (error) return <p className="text-xs text-destructive">Could not load sessions: {error}</p>;
  if (sessions === null) return <p className="text-xs text-muted-foreground">Loading sessions…</p>;

  const mine = sessions.filter((s) => s.class === agentClass);
  if (mine.length === 0) return <p className="text-sm text-muted-foreground">No live sessions for this agent.</p>;

  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Session</TableHead>
          <TableHead>Phase</TableHead>
          <TableHead>Started</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {mine.map((s) => (
          <TableRow key={`${s.namespace}/${s.name}`}>
            <TableCell className="font-mono text-xs">
              <EntityLink
                entity="session"
                id={`${s.namespace}/${s.name}`}
                className="text-link hover:underline"
              >
                <ResourceName ns={s.namespace} name={s.name} />
              </EntityLink>
            </TableCell>
            <TableCell><PhasePill phase={s.phase} /></TableCell>
            <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
              {fmtRelative(s.startedAt)}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

// AgentPage is the AgentClass detail. Tabs = Overview (description + status +
// manage command) + a frontend-only Sessions tab (live sessions of this class) +
// the backend section tabs (Prompt / Tools / Skills / Settings / Identity, plus
// Health when degraded).
export function AgentPage({ apiBase, id, tab }: { apiBase: string; id: string; tab?: string }) {
  return (
    <ConfigDetail
      apiBase={apiBase}
      resource="agents"
      entity="agent"
      id={id}
      tab={tab}
      backRoute={{ type: "view", view: "agents" }}
      extraTabs={[
        {
          id: "sessions",
          label: "Sessions",
          render: (detail) => <AgentSessions apiBase={apiBase} agentClass={detail.name} />,
        },
      ]}
    />
  );
}
