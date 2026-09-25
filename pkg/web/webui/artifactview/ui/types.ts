// Mirrors the Go JSON shapes from pkg/web/webui/artifactview (live.go liveMessage,
// deps.go StatusSnapshot/PlanStatusItem) and the Page.Build props (page.go).
export interface ArtifactViewProps {
  artifactName: string;
  artifactDescription: string;
  hostUrl: string;
  contentUrl: string;
  sandboxOrigin: string;
  backLink: string;
  channelKind: string;
  // ns/name identify the session (for the /session/{ns}/{name}/interact POST);
  // artifactId is the shell-held, server-re-checked id stamped onto outgoing
  // interaction requests (see annotationSend.ts) — the sandbox never needs it.
  ns: string;
  name: string;
  artifactId: string;
  // interactions lists the session_views interaction kinds this session's class
  // grants (e.g. ["user_message"]) — empty when the capability is absent or
  // inactive. UX-only gate: the real check is server-side on /interact.
  interactions: string[];
}

// ConnState is the live-socket connection state surfaced by the toolbar's
// connection indicator: a single connecting attempt, an established link, a
// retry in flight, or a sustained outage (still retrying in the background).
export type ConnState = "connecting" | "connected" | "reconnecting" | "offline";

export interface LiveCurrent {
  seq: number;
  revisionId: string;
  hostUrl?: string;
  contentUrl: string;
}

export interface LiveRevision {
  seq: number;
  revisionId: string;
  changeDescription: string;
  createdAt: string;
  tags: string[];
}

export interface PlanItem {
  label: string;
  status: string; // pending | in_progress | done | error
}

export interface StatusSnapshot {
  phase: string;
  paused: boolean;
  pauseCause: string;
  plan: PlanItem[];
  statusMessage: string;
}

// MirrorMessage mirrors the Go MirrorMessage (deps.go): one chat line for the
// live-view chat panel — an agent reply (respond_to_user) or the user's own
// send echoed back (user_echo).
export interface MirrorMessage {
  role: string; // "agent" | "user"
  text: string;
  author: string;
  via?: string;
  at: string;
}

export interface LiveMessage {
  type: "snapshot" | "revision" | "status" | "message";
  current?: LiveCurrent;
  revisions?: LiveRevision[];
  status?: StatusSnapshot;
  message?: MirrorMessage;
}
