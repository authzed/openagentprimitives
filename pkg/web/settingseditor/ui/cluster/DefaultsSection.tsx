import { Label, Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@ap/design";
import { DurationField, NumberField, TextField, BoolTriStateField } from "./fields";
import { type SettingsSpec, updateSpec, getPath } from "./spec";

export interface DefaultsSectionProps {
  spec: SettingsSpec;
  onChange: (spec: SettingsSpec) => void;
}

const MODEL_PROVIDERS = ["anthropic", "openai", "openrouter"] as const;
const NO_CATALOG_DEFAULT = "__none__";

// DefaultsSection edits SettingsDefaults: the fallback model/budget/authz
// values a lower tier inherits when it declares nothing. planGate/metaagent
// (nested under defaults.authz) are summary-only lines here — they're
// wizard-authored guard policy, same deliberate narrowing as LimitsSection's
// summary cards (see spec.ts's module doc).
export function DefaultsSection({ spec, onChange }: DefaultsSectionProps) {
  // provider is left undefined (not defaulted to "anthropic") when
  // defaults.model is absent: DefaultModel.Provider is a required field on
  // the wire, so visually pre-selecting a provider before the user has
  // chosen one would misrepresent an uncommitted value as already set —
  // Select's placeholder communicates "nothing chosen" instead.
  const provider = spec.defaults?.model?.provider;
  const fromCatalog = spec.defaults?.model?.fromCatalog || NO_CATALOG_DEFAULT;
  const catalogNames = (spec.modelCatalog ?? []).map((e) => e.name);
  const reportSessionCost = spec.defaults?.reportSessionCost;
  const planGateMode = getPath(spec, ["defaults", "authz", "planGate", "mode"]);
  const metaagentTrigger = getPath(spec, ["defaults", "authz", "metaagent", "trigger"]);

  return (
    <section className="flex flex-col gap-6">
      <h3 className="text-sm font-medium">Defaults (fallbacks)</h3>

      <div className="flex flex-col gap-3">
        <h4 className="text-xs font-semibold uppercase text-muted-foreground">Default model</h4>
        <div className="flex flex-col gap-1">
          <Label htmlFor="defaults-model-provider">Provider</Label>
          <Select
            value={provider}
            onValueChange={(v) => onChange(updateSpec(spec, ["defaults", "model", "provider"], v))}
          >
            <SelectTrigger id="defaults-model-provider">
              <SelectValue placeholder="Select a provider…" />
            </SelectTrigger>
            <SelectContent>
              {MODEL_PROVIDERS.map((p) => (
                <SelectItem key={p} value={p}>
                  {p}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>

        <TextField
          spec={spec}
          onChange={onChange}
          path={["defaults", "model", "name"]}
          id="defaults-model-name"
          label="Model name"
        />

        <div className="flex flex-col gap-1">
          <Label htmlFor="defaults-model-from-catalog">From catalog</Label>
          <Select
            value={fromCatalog}
            onValueChange={(v) =>
              onChange(updateSpec(spec, ["defaults", "model", "fromCatalog"], v === NO_CATALOG_DEFAULT ? undefined : v))
            }
          >
            <SelectTrigger id="defaults-model-from-catalog">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={NO_CATALOG_DEFAULT}>(use provider/name above)</SelectItem>
              {catalogNames.map((n) => (
                <SelectItem key={n} value={n}>
                  {n}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      </div>

      <div className="flex flex-col gap-3">
        <h4 className="text-xs font-semibold uppercase text-muted-foreground">Default budget</h4>
        <NumberField
          spec={spec}
          onChange={onChange}
          path={["defaults", "budget", "maxTurns"]}
          id="defaults-budget-max-turns"
          label="Default: max turns"
        />
        <NumberField
          spec={spec}
          onChange={onChange}
          path={["defaults", "budget", "maxTokens"]}
          id="defaults-budget-max-tokens"
          label="Default: max tokens"
        />
        <DurationField
          spec={spec}
          onChange={onChange}
          path={["defaults", "budget", "maxDuration"]}
          id="defaults-budget-max-duration"
          label="Default: max duration"
        />
        <DurationField
          spec={spec}
          onChange={onChange}
          path={["defaults", "budget", "sessionExpiration"]}
          id="defaults-budget-session-expiration"
          label="Default: session expiration"
        />
        <NumberField
          spec={spec}
          onChange={onChange}
          path={["defaults", "budget", "maxDelegatedAgents"]}
          id="defaults-budget-max-delegated-agents"
          label="Default: max delegated agents"
        />
      </div>

      <div className="flex flex-col gap-3">
        <h4 className="text-xs font-semibold uppercase text-muted-foreground">Default authz</h4>
        <DurationField
          spec={spec}
          onChange={onChange}
          path={["defaults", "authz", "approvalTimeout"]}
          id="defaults-authz-approval-timeout"
          label="Default: approval timeout"
        />
        <DurationField
          spec={spec}
          onChange={onChange}
          path={["defaults", "authz", "informationLeakageApprovalTTL"]}
          id="defaults-authz-leakage-ttl"
          label="Default: information-leakage approval TTL"
        />
        <NumberField
          spec={spec}
          onChange={onChange}
          path={["defaults", "authz", "scopeMaxLlmLatencyMs"]}
          id="defaults-authz-scope-max-llm-latency-ms"
          label="Default: scope max LLM latency (ms)"
        />

        <div className="flex flex-col gap-0.5 text-xs text-muted-foreground">
          <span>Plan-gate mode: {typeof planGateMode === "string" && planGateMode ? planGateMode : "not set"}</span>
          <span>Metaagent trigger: {typeof metaagentTrigger === "string" && metaagentTrigger ? metaagentTrigger : "not set"}</span>
          <span>Edit in Advanced tab</span>
        </div>
      </div>

      <div className="flex flex-col gap-3">
        <BoolTriStateField
          spec={spec}
          onChange={onChange}
          path={["defaults", "reportSessionCost"]}
          id="defaults-report-session-cost"
          label="Report session cost"
          hint="Unset means on: the end-of-session cost estimate is shown by default."
        />
        <TextField
          spec={spec}
          onChange={onChange}
          path={["defaults", "sandbox", "kind"]}
          id="defaults-sandbox-kind"
          label="Sandbox kind"
          hint='Defaults to "pod" when unset.'
        />
      </div>

      {reportSessionCost === undefined && (
        <p className="text-xs text-muted-foreground" data-testid="report-session-cost-default-note">
          No value set — cost reporting is ON by default.
        </p>
      )}
    </section>
  );
}
