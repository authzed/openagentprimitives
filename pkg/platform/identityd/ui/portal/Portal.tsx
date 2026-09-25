import * as React from "react";
import { Button } from "@ap/design";
import { IdentityShell } from "../shared/IdentityShell";
import { NoticeBanner } from "../shared/NoticeBanner";
import type { PortalProps, PortalCredential } from "./types";

function CredRow({ c, action }: { c: PortalCredential; action: React.ReactNode }) {
  return (
    <li className="flex items-center gap-2 py-2.5 border-b border-border last:border-0">
      {c.iconUrl && <img src={c.iconUrl} alt="" className="h-4 w-4 rounded-sm" />}
      <span className="text-sm">{c.label || c.name}</span>
      <span className="ml-auto">{action}</span>
    </li>
  );
}

export function Portal(props: PortalProps) {
  return (
    <IdentityShell title="Your connected accounts" subtitle={props.displayName ? `Signed in as ${props.displayName}` : undefined}>
      <NoticeBanner search={window.location.search} />
      <h3 className="text-xs uppercase tracking-wide text-muted-foreground mt-1 mb-1">Linked</h3>
      {props.linked.length === 0 ? (
        <p className="text-sm text-muted-foreground">Nothing linked yet.</p>
      ) : (
        <ul className="list-none m-0 p-0">
          {props.linked.map((c) => (
            <CredRow key={c.name} c={c}
              action={
                <div className="flex items-center gap-1">
                  <Button asChild variant="ghost" size="sm">
                    <a href={c.isOAuth ? `/link/oauth/${encodeURIComponent(c.name)}` : `/my/accounts/${encodeURIComponent(c.name)}/link`}>
                      Replace
                    </a>
                  </Button>
                  <form method="POST" action={`/my/accounts/${encodeURIComponent(c.name)}/revoke`} className="m-0">
                    <Button type="submit" variant="ghost" size="sm" className="text-destructive">Remove</Button>
                  </form>
                </div>
              } />
          ))}
        </ul>
      )}
      {props.suggested.length > 0 && (
        <>
          <h3 className="text-xs uppercase tracking-wide text-muted-foreground mt-5 mb-1">Suggested</h3>
          <ul className="list-none m-0 p-0">
            {props.suggested.map((c) => (
              <CredRow key={c.name} c={c}
                action={
                  <Button asChild variant="secondary" size="sm">
                    <a href={c.isOAuth ? `/link/oauth/${encodeURIComponent(c.name)}` : `/my/accounts/${encodeURIComponent(c.name)}/link`}>
                      {c.isOAuth ? "Connect" : "Link"}
                    </a>
                  </Button>
                } />
            ))}
          </ul>
        </>
      )}
    </IdentityShell>
  );
}
