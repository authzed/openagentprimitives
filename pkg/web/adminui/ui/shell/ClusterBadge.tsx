import * as React from "react";
import { Boxes, Laptop } from "lucide-react";
import { getCluster, type ClusterInfo } from "../lib/api";
import { httpUrl } from "../lib/safeUrl";

// Short, human type labels for the badge. Cloud kinds keep their uppercase
// product acronym; local/unknown get a plain word so an unlabeled cluster still
// reads sensibly.
const TYPE_LABEL: Record<string, string> = {
  gke: "GKE",
  eks: "EKS",
  aks: "AKS",
  local: "Local",
  unknown: "Cluster",
};

const ICON_CLASS = "h-3.5 w-3.5 shrink-0";

// Provider marks — tiny inline brand-tinted SVGs so each cloud is recognizable
// at a glance (blue GKE hexagon, AWS orange smile, Azure blue "A"). local/unknown
// fall back to neutral lucide glyphs. Decorative → aria-hidden; the visible short
// label carries the meaning for assistive tech.

// GkeMark — Google-blue hexagon with an inner helm ring (the GKE product mark).
function GkeMark() {
  return (
    <svg viewBox="0 0 24 24" className={ICON_CLASS} aria-hidden="true" fill="none">
      <path
        d="M12 2.5 L20.16 7.25 V16.75 L12 21.5 L3.84 16.75 V7.25 Z"
        fill="#4285F4"
        fillOpacity="0.16"
        stroke="#4285F4"
        strokeWidth="1.5"
        strokeLinejoin="round"
      />
      <circle cx="12" cy="12" r="3" stroke="#4285F4" strokeWidth="1.5" />
    </svg>
  );
}

// EksMark — the AWS "smile" swoosh + arrowhead in AWS orange.
function EksMark() {
  return (
    <svg viewBox="0 0 24 24" className={ICON_CLASS} aria-hidden="true" fill="none">
      <path
        d="M4 12.5 C 8 16.2, 16 16.2, 20 12.5"
        stroke="#FF9900"
        strokeWidth="2"
        strokeLinecap="round"
      />
      <path d="M16.6 12.2 L20.4 12.4 L18.9 15.6 Z" fill="#FF9900" />
    </svg>
  );
}

// AksMark — the Azure "A" wedge in Azure blue.
function AksMark() {
  return (
    <svg viewBox="0 0 24 24" className={ICON_CLASS} aria-hidden="true">
      <path d="M12.8 3 L21 20 H14.8 L12.4 13.6 Z" fill="#0078D4" />
      <path d="M10.8 8 L3.2 20 H15 L12.8 15.4 H9 Z" fill="#0078D4" fillOpacity="0.7" />
    </svg>
  );
}

function TypeIcon({ type }: { type: string }) {
  switch (type) {
    case "gke":
      return <GkeMark />;
    case "eks":
      return <EksMark />;
    case "aks":
      return <AksMark />;
    case "local":
      return <Laptop aria-hidden="true" className={`${ICON_CLASS} text-muted-foreground`} />;
    default:
      return <Boxes aria-hidden="true" className={`${ICON_CLASS} text-muted-foreground`} />;
  }
}

// ClusterBadge shows the cluster identity in the header: an origin-tinted
// provider mark, the cluster name (falling back to the type label), and — when
// the backend derived a console URL — a deep-link to the cloud console.
//
// It fetches once on mount and degrades quietly: while loading, or on any fetch
// error, it renders nothing so the header still comes up. The console link's
// scheme is guarded through httpUrl (never a javascript:/data: anchor).
export function ClusterBadge({ apiBase }: { apiBase: string }) {
  const [info, setInfo] = React.useState<ClusterInfo | null>(null);

  React.useEffect(() => {
    let active = true;
    getCluster(apiBase)
      .then((c) => { if (active) setInfo(c); })
      .catch((e) => { console.warn("cluster info fetch failed", e); });
    return () => { active = false; };
  }, [apiBase]);

  if (!info) return null;

  const typeLabel = TYPE_LABEL[info.type] ?? "Cluster";
  const displayName = info.name || typeLabel || "cluster";
  // Only show the type token separately when the name is distinct from it, so a
  // nameless cluster doesn't read "GKE GKE".
  const showType = Boolean(info.name);
  const href = info.consoleURL ? httpUrl(info.consoleURL) : null;

  const inner = (
    <>
      <TypeIcon type={info.type} />
      <span className="max-w-[16rem] truncate font-medium text-foreground">{displayName}</span>
      {showType && (
        <span className="font-mono text-[10px] uppercase tracking-wide text-muted-foreground">
          {typeLabel}
        </span>
      )}
    </>
  );

  const chipClass =
    "flex items-center gap-1.5 rounded-full border bg-card/60 px-2 py-0.5 text-xs";

  if (href) {
    return (
      <a
        href={href}
        target="_blank"
        rel="noopener noreferrer"
        title={`Open ${displayName} in the cloud console`}
        className={`${chipClass} transition-colors hover:border-primary/50 hover:bg-accent/40`}
      >
        {inner}
      </a>
    );
  }

  return (
    <span className={chipClass} title={displayName}>
      {inner}
    </span>
  );
}
