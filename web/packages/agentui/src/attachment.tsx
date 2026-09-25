// attachment.tsx renders ap:attachment — a file of this session as a row
// with a download control. The node names only an artifact id; everything
// else comes from the platform: the file's name, size and kind from the
// artifact-view meta route, the download link from the artifact-download
// route, both addressed to the HOST's session (useViewSession) and both
// authorized server-side on the viewer's own CheckView. Nothing here fetches
// or links anything a node supplied.
//
// Like question.tsx, this file must NOT import registry.tsx.
import * as React from "react";
import { Download } from "lucide-react";
import { Alert, AlertDescription, AlertTitle, buttonVariants } from "@ap/design";
import { cn } from "@ap/design/lib/utils";
import { s } from "./props";
import type { Node } from "./types";
import { useViewSession } from "./viewSession";

interface FileFacts {
  name: string;
  filename: string;
  size: number;
  mime: string;
}

const BUNDLE_MIME = "application/vnd.agentprimitives.authzed.com.agent.v1";

// kindLabel names a file's kind for a person: the platform's own bundle type,
// the two kinds an agent produces most, else the MIME itself rather than a
// guess.
function kindLabel(mime: string): string {
  if (mime === BUNDLE_MIME) return "Agent bundle";
  if (mime === "text/html") return "Web page";
  if (mime.startsWith("image/")) return "Image";
  return mime;
}

// formatSize renders bytes in the locale's unit style, one decimal at most.
function formatSize(bytes: number): string {
  const [value, unit] = bytes >= 1_000_000 ? [bytes / 1_000_000, "megabyte"] : bytes >= 1_000 ? [bytes / 1_000, "kilobyte"] : [bytes, "byte"];
  return new Intl.NumberFormat(undefined, { style: "unit", unit, unitDisplay: "short", maximumFractionDigits: 1 }).format(value);
}

function Unavailable({ reason }: { reason: string }): React.ReactElement {
  return (
    <Alert variant="destructive" data-testid="ap-attachment-unavailable">
      <AlertTitle>Attachment unavailable</AlertTitle>
      <AlertDescription>{reason}</AlertDescription>
    </Alert>
  );
}

export function AttachmentCard({ n }: { n: Node }): React.ReactElement {
  const session = useViewSession();
  const artifact = s(n, "artifact");
  const label = s(n, "label");
  const [facts, setFacts] = React.useState<FileFacts | null>(null);
  const [failure, setFailure] = React.useState<string | null>(null);

  React.useEffect(() => {
    if (session === null || artifact === "") return;
    let cancelled = false;
    const params = new URLSearchParams({ artifactId: artifact, sessionRef: `${session.ns}/${session.name}` });
    fetch(`/artifact-view/meta?${params.toString()}`, { credentials: "same-origin" })
      .then(async (res) => {
        if (!res.ok) throw new Error(`${res.status} ${await res.text()}`);
        return (await res.json()) as FileFacts;
      })
      .then((f) => {
        if (!cancelled) setFacts(f);
      })
      .catch((err: unknown) => {
        // The card is the user-visible half of no-silent-errors; this line
        // is the other half, for whoever is debugging why a file never showed.
        console.error("agentui: ap:attachment lookup failed", artifact, err);
        if (!cancelled) setFailure("This file could not be found in this conversation.");
      });
    return () => {
      cancelled = true;
    };
  }, [session, artifact]);

  if (session === null) return <Unavailable reason="This page is not attached to a conversation." />;
  if (failure !== null) return <Unavailable reason={failure} />;
  if (facts === null) {
    return (
      <div data-testid="ap-attachment-loading" className="text-sm text-muted-foreground">
        Loading file…
      </div>
    );
  }

  const download = new URLSearchParams({ artifactId: artifact, sessionRef: `${session.ns}/${session.name}`, fn: facts.filename || artifact });
  return (
    <div data-testid="ap-attachment" className="flex items-center justify-between gap-3 rounded-md border border-border px-3 py-2">
      <div className="min-w-0">
        <div className="truncate text-sm font-medium">{label || facts.filename || facts.name}</div>
        <div className="text-xs text-muted-foreground">
          {kindLabel(facts.mime)} · {formatSize(facts.size)}
        </div>
      </div>
      <a
        href={`/artifact-download?${download.toString()}`}
        download={facts.filename || undefined}
        className={cn(buttonVariants({ variant: "outline", size: "sm" }), "shrink-0")}
        aria-label={`Download ${facts.filename || facts.name}`}
      >
        <Download className="mr-1.5 h-4 w-4" aria-hidden="true" />
        Download
      </a>
    </div>
  );
}
