import * as React from "react";
import { Button, cn } from "@ap/design";
import { IdentityShell } from "../shared/IdentityShell";
import { NoticeBanner } from "../shared/NoticeBanner";
import { PatForm } from "../shared/PatForm";
import type { LinkPageProps, LinkMenuRow } from "./types";

// 30s heartbeat (best-effort), mirroring the original shell's setInterval.
function useHeartbeat(sessionRef: string) {
  React.useEffect(() => {
    if (!sessionRef) return;
    const ping = () =>
      fetch(`/heartbeat?session=${encodeURIComponent(sessionRef)}`, { method: "POST", credentials: "same-origin" }).catch(() => {});
    const id = window.setInterval(ping, 30000);
    return () => window.clearInterval(id);
  }, [sessionRef]);
}

function Row({ row, signedLink }: { row: LinkMenuRow; signedLink: string }) {
  return (
    <li className="flex flex-col gap-1.5 py-3 border-b border-border last:border-0">
      <div className="flex items-center gap-2">
        {row.iconUrl && <img src={row.iconUrl} alt="" className="h-4 w-4 rounded-sm" />}
        <span className="font-medium text-sm">{row.label || row.credentialName}</span>
        {row.status === "linked" && (
          <span className="ml-auto text-[10px] px-2 py-0.5 rounded-lg bg-success/20 text-success">linked</span>
        )}
      </div>
      {row.why && <p className="text-xs text-muted-foreground">{row.why}</p>}
      {row.status === "missing" && row.kind === "pat" && (
        <PatForm action="/link/submit" submitLabel="Save"
          hidden={{ link: signedLink, credential: row.credentialName }}
          instructions={row.instructions}
          docsUrl={row.docsUrl} />
      )}
      {row.status === "missing" && row.kind === "oauth" && (
        <Button asChild variant="secondary" size="sm" className="self-start">
          <a href={row.oauthUrl}>Connect {row.label || row.credentialName}</a>
        </Button>
      )}
    </li>
  );
}

export function LinkMenu(props: LinkPageProps) {
  useHeartbeat(props.sessionRef);
  const subtitle = props.agentName ? `for ${props.agentName}` : undefined;
  return (
    <IdentityShell title="Connect your credentials" subtitle={subtitle}>
      <NoticeBanner search={window.location.search} />
      {props.what.length > 0 && (
        <p className="text-sm text-muted-foreground mb-3">{props.what.join(" ")}</p>
      )}
      <ul className={cn("list-none m-0 p-0")}>
        {props.rows.map((row) => (
          <Row key={row.credentialName} row={row} signedLink={props.signedLink} />
        ))}
      </ul>
      <p className="text-[11px] text-muted-foreground/60 mt-4">Session: {props.sessionRef}</p>
    </IdentityShell>
  );
}
