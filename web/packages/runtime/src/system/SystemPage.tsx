import * as React from "react";
import { Card, CardHeader, CardTitle, CardContent, Button, cn } from "@ap/design";
import { AlertTriangle, Ban, FileQuestion, Clock, Lock, Info } from "lucide-react";

export type SystemKind = "unauthorized" | "forbidden" | "notFound" | "expired" | "error" | "info";

export interface SystemAction { label: string; href: string }
export interface SystemPageProps {
  status: number;
  kind: SystemKind;
  title: string;
  message: string;
  actions?: SystemAction[];
}

const ICONS: Record<SystemKind, React.ComponentType<{ className?: string }>> = {
  unauthorized: Lock, forbidden: Ban, notFound: FileQuestion,
  expired: Clock, error: AlertTriangle, info: Info,
};
const ACCENT: Record<SystemKind, string> = {
  unauthorized: "text-warning", forbidden: "text-destructive", notFound: "text-muted-foreground",
  expired: "text-warning", error: "text-destructive", info: "text-primary",
};

export function SystemPage({ status, kind, title, message, actions }: SystemPageProps) {
  const Icon = ICONS[kind] ?? Info;
  return (
    <div className="min-h-screen flex items-center justify-center p-6 bg-background text-foreground">
      <Card className="max-w-md w-full">
        <CardHeader className="flex flex-row items-center gap-3">
          <Icon className={cn("h-6 w-6 shrink-0", ACCENT[kind])} />
          <CardTitle>{title}</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <p className="text-sm text-muted-foreground">{message}</p>
          {actions && actions.length > 0 && (
            <div className="flex gap-2">
              {actions.map((a) => (
                <Button key={a.href} asChild variant="secondary">
                  <a href={a.href}>{a.label}</a>
                </Button>
              ))}
            </div>
          )}
          <p className="text-xs text-muted-foreground/60">Status {status}</p>
        </CardContent>
      </Card>
    </div>
  );
}
