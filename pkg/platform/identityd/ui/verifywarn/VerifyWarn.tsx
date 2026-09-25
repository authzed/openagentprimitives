import { Alert, AlertDescription, Button } from "@ap/design";
import { IdentityShell } from "../shared/IdentityShell";
import type { VerifyWarnProps } from "./types";

// VerifyWarn is the confirm page shown when a pasted token failed live
// verification: it explains the rejection and offers an explicit
// "Store anyway" re-submit (carrying verifyConfirm=1) or a way back.
export function VerifyWarn(props: VerifyWarnProps) {
  return (
    <IdentityShell title="Token could not be verified">
      <Alert className="mb-4 border-destructive text-destructive">
        <AlertDescription>{props.warning}</AlertDescription>
      </Alert>
      <p className="text-sm text-muted-foreground mb-4">
        The provider rejected this token, so it probably won&apos;t work. You can
        store it anyway if you&apos;re sure it&apos;s correct — for example, a token
        restricted to specific source IPs.
      </p>
      <form method="POST" action={props.action} className="flex gap-2 items-center">
        {Object.entries(props.hidden).map(([k, v]) => (
          <input key={k} type="hidden" name={k} value={v} />
        ))}
        <Button type="submit" variant="destructive" size="sm">Store anyway</Button>
        <Button asChild variant="ghost" size="sm">
          <a href={props.cancelUrl}>Go back</a>
        </Button>
      </form>
    </IdentityShell>
  );
}
