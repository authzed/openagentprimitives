import * as React from "react";
import { Alert, AlertDescription, cn } from "@ap/design";
import { parseNotice } from "./notice";

// NoticeBanner renders a banner derived from the URL ?notice/?error params the
// action-handler redirects set. Renders nothing when neither is present.
export function NoticeBanner({ search }: { search: string }) {
  const [n] = React.useState(() => parseNotice(search));
  if (!n) return null;
  return (
    <Alert className={cn("mb-4", n.kind === "error" ? "border-destructive text-destructive" : "border-success text-success")}>
      <AlertDescription>{n.text}</AlertDescription>
    </Alert>
  );
}
