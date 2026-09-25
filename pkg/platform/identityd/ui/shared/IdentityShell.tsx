import * as React from "react";
import { Card, CardHeader, CardTitle, CardContent } from "@ap/design";

// IdentityShell is the shared centered-card chrome for the identity pages.
export function IdentityShell({ title, subtitle, children }: { title: string; subtitle?: string; children: React.ReactNode }) {
  return (
    <div className="min-h-screen flex items-start justify-center p-6 bg-background text-foreground">
      <Card className="max-w-xl w-full mt-[8vh]">
        <CardHeader>
          <CardTitle>{title}</CardTitle>
          {subtitle && <p className="text-sm text-muted-foreground mt-1">{subtitle}</p>}
        </CardHeader>
        <CardContent>{children}</CardContent>
      </Card>
    </div>
  );
}
