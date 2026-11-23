import { useEffect, useState } from "react";
import { Bell, X } from "lucide-react";
import { Button } from "@ap/design";
import { sessionHref } from "./SessionList";
import type { SelectedView, SessionRow } from "./types";

type Saved = { announced: string[]; dismissed: string[] };
const identity = (s: SessionRow) => `${s.ns}/${s.name}@${s.uid ?? s.startedAt ?? ""}`;

function readSaved(key: string): Saved {
  try {
    const raw = sessionStorage.getItem(key);
    if (raw) {
      const value = JSON.parse(raw);
      if (Array.isArray(value.announced) && Array.isArray(value.dismissed) &&
          [...value.announced, ...value.dismissed].every((id) => typeof id === "string")) return value;
    }
  } catch (error) {
    console.warn("sessions: could not restore goal session alerts", error);
  }
  return { announced: [], dismissed: [] };
}

// Remains outside collapsible chrome. Seeing an alert does not acknowledge it:
// it stays until opened/dismissed, including after a page reload in this tab.
export function GoalSessionAlerts({ sessions, selected, subject, notify }: {
  sessions: readonly SessionRow[];
  selected?: Pick<SelectedView, "ns" | "name">;
  subject: string;
  notify: (title: string, body: string) => void;
}) {
  const key = `oap:goal-session-alerts:${encodeURIComponent(subject)}`;
  const [saved, setSaved] = useState(() => readSaved(key));
  const isSelected = (s: SessionRow) => s.ns === selected?.ns && s.name === selected?.name;
  const alerts = sessions.filter((s) => s.goalCreated && !s.ended && !isSelected(s) && !saved.dismissed.includes(identity(s)));

  useEffect(() => {
    const next = { announced: [...saved.announced], dismissed: [...saved.dismissed] };
    for (const session of sessions) {
      if (!session.goalCreated) continue;
      const id = identity(session);
      if (isSelected(session) && !next.dismissed.includes(id)) next.dismissed.push(id);
    }
    for (const session of alerts) {
      const id = identity(session);
      if (next.announced.includes(id)) continue;
      next.announced.push(id);
      // Avoid leaking private goal content to an OS notification/lock screen.
      notify("New goal session", "A goal started a private session. Open OAP to review it.");
    }
    if (JSON.stringify(next) !== JSON.stringify(saved)) setSaved(next);
    try {
      sessionStorage.setItem(key, JSON.stringify(next));
    } catch (error) {
      console.warn("sessions: could not persist goal session alerts", error);
    }
  }, [sessions, selected?.ns, selected?.name, saved, key, notify]);

  if (!alerts.length) return null;
  return (
    <aside aria-label="New goal sessions" className="fixed bottom-4 right-4 z-50 flex max-h-[70vh] w-[min(24rem,calc(100vw-2rem))] flex-col gap-2 overflow-auto">
      {alerts.map((session) => (
        <div key={identity(session)} role="alert" className="rounded-lg border border-primary bg-card p-3 shadow-lg">
          <div className="flex items-center gap-2">
            <Bell className="h-4 w-4" aria-hidden="true" />
            <strong className="flex-1 text-sm">New goal session</strong>
            <Button size="icon" variant="ghost" aria-label={`Dismiss alert for ${session.title}`} onClick={() =>
              setSaved((prev) => ({ ...prev, dismissed: [...prev.dismissed, identity(session)] }))}>
              <X className="h-4 w-4" aria-hidden="true" />
            </Button>
          </div>
          <p className="my-2 whitespace-pre-wrap break-words text-sm">{session.openingSummary || `${session.title} started a session for your goal.`}</p>
          <a className="text-sm font-medium text-primary underline" href={sessionHref(session.ns, session.name)}>
            {session.awaitingHuman ? "Open session to review approval" : "Open session"}
          </a>
        </div>
      ))}
    </aside>
  );
}
