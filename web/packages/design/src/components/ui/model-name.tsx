import { cn } from "@ap/design/lib/utils";
import { Badge } from "./badge";

// ParsedModelDisplay is the decomposition of a uniform model display id:
// "<provider>/<model>" for direct providers, or
// "openrouter/<underlying-provider>/<underlying-model>" for calls routed
// through OpenRouter. `via` is only set for a routed id.
export interface ParsedModelDisplay {
  via?: string;
  provider?: string;
  name: string;
}

// parseModelDisplay mirrors the Go-side display-id composition byte-for-byte
// (pkg/web/admind/aggregator.go modelDisplay, pkg/agent/runner/loop.go
// servedModelDisplay): split on the FIRST "/" into a provider segment and the
// rest. When that provider segment is "openrouter" (a routed call), split the
// REST the same way to recover the underlying provider + name — mechanically,
// with no further special-casing. That mechanical rule is what makes
// "openrouter/openrouter/auto" (OpenRouter's own auto-router "model") parse
// to via="openrouter", provider="openrouter", name="auto" — the same code
// path as any other routed id, not a special case for the auto router.
export function parseModelDisplay(model: string): ParsedModelDisplay {
  if (!model) return { name: "" };
  const i = model.indexOf("/");
  if (i < 0) return { name: model };
  const provider = model.slice(0, i);
  const rest = model.slice(i + 1);
  if (provider !== "openrouter") return { provider, name: rest };
  const j = rest.indexOf("/");
  if (j < 0) return { via: "openrouter", name: rest };
  return {
    via: "openrouter",
    provider: rest.slice(0, j),
    name: rest.slice(j + 1),
  };
}

// ModelName renders a uniform model display id with provider badges — the one
// shared surface for every raw-text model spot in the console (admin session
// detail, Overview tokens-by-model legend, Budget by-model table, chat
// session-info panel) so all four read identically and OpenRouter-routed
// calls are visually distinguished from direct-provider calls everywhere at
// once. Presentational only: no fetching, no state. An empty `model` renders
// nothing — callers own their own empty-state text ("—", "not resolved",
// "(unknown)"), same contract as KindBadge (pkg/web/adminui/ui/lib/KindBadge.tsx).
export function ModelName({
  model,
  className,
}: {
  model: string;
  className?: string;
}) {
  if (!model) return null;
  const { via, provider, name } = parseModelDisplay(model);
  return (
    <span className={cn("inline-flex items-center gap-1.5", className)}>
      {via && (
        <Badge
          variant="secondary"
          className="rounded-full px-2 py-0 text-[10px] font-normal"
        >
          {via}
        </Badge>
      )}
      {provider && (
        <span className="inline-flex items-center rounded-full border bg-background px-2 py-0.5 font-mono text-[10px] text-muted-foreground">
          {provider}
        </span>
      )}
      <span className="font-mono text-xs">{name}</span>
    </span>
  );
}
