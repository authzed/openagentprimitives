import { Download } from "lucide-react";
import { cn } from "@ap/design";
import type { LiveRevision } from "./types";

// RevisionList is the toggleable sidebar: revisions newest-first, each clickable
// to pin the view to that revision. The "latest" tag shows a badge. downloadHref,
// when provided, renders a small per-revision download icon in the lower-right.
export function RevisionList({
  revisions,
  currentRevId,
  onSelect,
  downloadHref,
}: {
  revisions: LiveRevision[];
  currentRevId: string | null;
  onSelect: (revisionId: string) => void;
  downloadHref?: (revisionId: string) => string;
}) {
  const newestFirst = [...revisions].reverse();
  return (
    <ul className="list-none m-0 p-1.5">
      {newestFirst.map((rev) => {
        const isCurrent = rev.revisionId && rev.revisionId === currentRevId;
        return (
          <li
            key={rev.revisionId || rev.seq}
            onClick={() => rev.revisionId && onSelect(rev.revisionId)}
            className={cn(
              "relative px-2.5 py-2 rounded-md cursor-pointer border border-transparent text-xs leading-snug hover:bg-muted",
              isCurrent && "bg-muted border-border",
            )}
          >
            {downloadHref && rev.revisionId && (
              <a
                href={downloadHref(rev.revisionId)}
                download
                onClick={(e) => e.stopPropagation()}
                aria-label={`Download revision ${rev.seq ?? ""}`.trim()}
                title="Download this revision"
                className="absolute bottom-1.5 right-1.5 flex h-5 w-5 items-center justify-center rounded text-muted-foreground hover:bg-accent hover:text-accent-foreground"
              >
                <Download className="h-3 w-3" aria-hidden="true" />
              </a>
            )}
            <span className="font-semibold text-primary">
              #{rev.seq ?? "?"}
              {Array.isArray(rev.tags) && rev.tags.includes("latest") && (
                <span className="ml-1.5 text-[10px] px-1.5 rounded-lg bg-success/20 text-success align-middle">
                  latest
                </span>
              )}
            </span>
            <span className="block text-foreground my-0.5">
              {rev.changeDescription || "(no description)"}
            </span>
            {rev.createdAt && (
              <span className="block text-muted-foreground text-[11px]">{rev.createdAt}</span>
            )}
          </li>
        );
      })}
    </ul>
  );
}
