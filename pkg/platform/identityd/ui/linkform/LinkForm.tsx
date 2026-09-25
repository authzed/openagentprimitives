import { Button } from "@ap/design";
import { IdentityShell } from "../shared/IdentityShell";
import { NoticeBanner } from "../shared/NoticeBanner";
import { PatForm } from "../shared/PatForm";
import type { LinkFormProps } from "./types";

export function LinkForm(props: LinkFormProps) {
  return (
    <IdentityShell title={`Link ${props.credentialLabel || props.credentialName}`}>
      <NoticeBanner search={window.location.search} />
      <p className="text-xs text-muted-foreground mb-3 font-mono">{props.credentialName}</p>
      <PatForm action={`/my/accounts/${encodeURIComponent(props.credentialName)}/link/submit`} submitLabel="Connect"
        instructions={props.instructions}
        docsUrl={props.docsUrl} />
      <Button asChild variant="ghost" size="sm" className="mt-3">
        <a href="/my/accounts">Cancel</a>
      </Button>
    </IdentityShell>
  );
}
