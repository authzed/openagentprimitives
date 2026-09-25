import { Alert, AlertDescription, Badge } from "@ap/design";
import type { ValidationResult } from "./api";

export interface ValidationPanelProps {
  result: ValidationResult;
}

// ValidationPanel renders a settingseditor.Result: hard admission errors as
// blocking Alerts, advisory resolver violations as a reason-badged warning
// list, and — when present — a preview of the effective settings the
// cluster tier alone would produce. Shared between ClusterTab (curated-form
// requests) and AdvancedTab (raw-YAML requests): the endpoint's response
// shape carries no reference to which surface produced the request.
export function ValidationPanel({ result }: ValidationPanelProps) {
  return (
    <div className="flex flex-col gap-2">
      {result.errors.map((err, i) => (
        <Alert variant="destructive" key={`err-${i}`}>
          <AlertDescription>{err}</AlertDescription>
        </Alert>
      ))}
      {result.violations.map((v, i) => (
        <Alert key={`violation-${i}`}>
          <AlertDescription className="flex items-center gap-2">
            <Badge variant={v.fatal ? "destructive" : "secondary"}>{v.reason}</Badge>
            <span>{v.message}</span>
          </AlertDescription>
        </Alert>
      ))}
      {result.effective !== undefined && (
        <div className="flex flex-col gap-1">
          <h4 className="text-xs font-semibold uppercase text-muted-foreground">Effective settings (preview)</h4>
          <pre className="max-h-64 overflow-auto rounded-md border border-border bg-muted/30 p-2 text-xs">
            {JSON.stringify(result.effective, null, 2)}
          </pre>
        </div>
      )}
    </div>
  );
}
