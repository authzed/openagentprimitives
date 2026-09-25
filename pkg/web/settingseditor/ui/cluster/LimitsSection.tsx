import { Label, Select, SelectContent, SelectItem, SelectTrigger, SelectValue, Alert, AlertDescription } from "@ap/design";
import { DurationField, NumberField, BoolTriStateField, AllowlistTextarea, SummaryCard } from "./fields";
import {
  type SettingsSpec,
  updateSpec,
  getPath,
  listTriState,
  summarizePinning,
  summarizeToolGuardCeiling,
  summarizeContentInspectors,
  summarizeAllowedMCPServers,
} from "./spec";

export interface LimitsSectionProps {
  spec: SettingsSpec;
  onChange: (spec: SettingsSpec) => void;
}

const PLAN_GATE_MODES = [
  { value: "unset", label: "Unset (no floor)" },
  { value: "disabled", label: "disabled" },
  { value: "logging", label: "logging" },
  { value: "enforcing", label: "enforcing" },
] as const;

// TriStateListField is the radio(Unrestricted/Restrict-to-list) + textarea
// editor for a *[]string ceiling field. Deliberately local to LimitsSection —
// it's the only section that has these — unlike the primitive field
// controls in fields.tsx, which both Limits and Defaults share.
function TriStateListField({
  spec,
  onChange,
  path,
  fieldLabel,
  idPrefix,
}: {
  spec: SettingsSpec;
  onChange: (spec: SettingsSpec) => void;
  path: readonly string[];
  fieldLabel: string;
  idPrefix: string;
}) {
  const raw = getPath(spec, path) as string[] | undefined;
  const state = listTriState(raw);
  const restricted = state !== "unrestricted";
  return (
    <div className="flex flex-col gap-2">
      <Label>{fieldLabel}</Label>
      <div className="flex flex-col gap-1 text-sm">
        <label className="flex items-center gap-2">
          <input
            type="radio"
            name={idPrefix}
            checked={!restricted}
            onChange={() => onChange(updateSpec(spec, path, undefined))}
          />
          {fieldLabel}: Unrestricted
        </label>
        <label className="flex items-center gap-2">
          <input
            type="radio"
            name={idPrefix}
            checked={restricted}
            onChange={() => onChange(updateSpec(spec, path, raw ?? []))}
          />
          {fieldLabel}: Restrict to list
        </label>
      </div>
      {restricted && (
        <>
          <AllowlistTextarea
            id={`${idPrefix}-list`}
            initial={raw ?? []}
            onCommit={(items) => onChange(updateSpec(spec, path, items))}
          />
          {state === "deny-all" && (
            <Alert variant="destructive">
              <AlertDescription>Empty list — deny-all: no entries are permitted.</AlertDescription>
            </Alert>
          )}
        </>
      )}
    </div>
  );
}

// LimitsSection edits SettingsLimits (the cluster ceiling/allowlist group):
// budget + authz ceilings, minPlanGateMode, the plain deny-lists, the
// tri-state *bool grants, and the tri-state allowlists. The nested guard
// policies (pinning, toolGuard, contentInspectors, allowedMCPServers) are
// READ-ONLY summary cards here — see spec.ts's module doc for why.
export function LimitsSection({ spec, onChange }: LimitsSectionProps) {
  const planGateMode = spec.limits?.minPlanGateMode || "unset";

  return (
    <section className="flex flex-col gap-6">
      <h3 className="text-sm font-medium">Limits (ceilings)</h3>

      <div className="flex flex-col gap-3">
        <h4 className="text-xs font-semibold uppercase text-muted-foreground">Budget ceiling</h4>
        <NumberField
          spec={spec}
          onChange={onChange}
          path={["limits", "budget", "maxTurns"]}
          id="limits-budget-max-turns"
          label="Ceiling: max turns"
        />
        <NumberField
          spec={spec}
          onChange={onChange}
          path={["limits", "budget", "maxTokens"]}
          id="limits-budget-max-tokens"
          label="Ceiling: max tokens"
        />
        <DurationField
          spec={spec}
          onChange={onChange}
          path={["limits", "budget", "maxDuration"]}
          id="limits-budget-max-duration"
          label="Ceiling: max duration"
        />
        <DurationField
          spec={spec}
          onChange={onChange}
          path={["limits", "budget", "sessionExpiration"]}
          id="limits-budget-session-expiration"
          label="Ceiling: session expiration"
        />
        <NumberField
          spec={spec}
          onChange={onChange}
          path={["limits", "budget", "maxDelegatedAgents"]}
          id="limits-budget-max-delegated-agents"
          label="Ceiling: max delegated agents"
        />
      </div>

      <div className="flex flex-col gap-3">
        <h4 className="text-xs font-semibold uppercase text-muted-foreground">Authz ceiling</h4>
        <DurationField
          spec={spec}
          onChange={onChange}
          path={["limits", "authz", "maxApprovalTimeout"]}
          id="limits-authz-max-approval-timeout"
          label="Ceiling: max approval timeout"
        />
        <DurationField
          spec={spec}
          onChange={onChange}
          path={["limits", "authz", "maxInformationLeakageApprovalTTL"]}
          id="limits-authz-max-leakage-ttl"
          label="Ceiling: max information-leakage approval TTL"
        />
      </div>

      <div className="flex flex-col gap-1">
        <Label htmlFor="limits-min-plan-gate-mode">Minimum plan-gate mode</Label>
        <Select
          value={planGateMode}
          onValueChange={(v) =>
            onChange(updateSpec(spec, ["limits", "minPlanGateMode"], v === "unset" ? undefined : v))
          }
        >
          <SelectTrigger id="limits-min-plan-gate-mode">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {PLAN_GATE_MODES.map((m) => (
              <SelectItem key={m.value} value={m.value}>
                {m.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      <div className="flex flex-col gap-1">
        <Label htmlFor="limits-denied-models">Denied models</Label>
        <AllowlistTextarea
          id="limits-denied-models"
          initial={spec.limits?.deniedModels ?? []}
          onCommit={(items) =>
            onChange(updateSpec(spec, ["limits", "deniedModels"], items.length > 0 ? items : undefined))
          }
        />
      </div>

      <div className="flex flex-col gap-1">
        <Label htmlFor="limits-denied-skills">Denied skills</Label>
        <AllowlistTextarea
          id="limits-denied-skills"
          initial={spec.limits?.deniedSkills ?? []}
          onCommit={(items) =>
            onChange(updateSpec(spec, ["limits", "deniedSkills"], items.length > 0 ? items : undefined))
          }
        />
      </div>

      <div className="flex flex-col gap-3">
        <BoolTriStateField
          spec={spec}
          onChange={onChange}
          path={["limits", "allowModelOverride"]}
          id="limits-allow-model-override"
          label="Allow model override"
          hint="Top-down grant: nil/false means catalog-only."
        />
        <BoolTriStateField
          spec={spec}
          onChange={onChange}
          path={["limits", "nativeFileHandling"]}
          id="limits-native-file-handling"
          label="Native file handling"
        />
        <BoolTriStateField
          spec={spec}
          onChange={onChange}
          path={["limits", "requireSubagentDigestPins"]}
          id="limits-require-subagent-digest-pins"
          label="Require subagent digest pins"
        />
      </div>

      <div className="flex flex-col gap-4">
        <TriStateListField
          spec={spec}
          onChange={onChange}
          path={["limits", "allowedToolkits"]}
          fieldLabel="Allowed toolkits"
          idPrefix="limits-allowed-toolkits"
        />
        <TriStateListField
          spec={spec}
          onChange={onChange}
          path={["limits", "allowedSkills"]}
          fieldLabel="Allowed skills"
          idPrefix="limits-allowed-skills"
        />
        <TriStateListField
          spec={spec}
          onChange={onChange}
          path={["limits", "allowedSandboxKinds"]}
          fieldLabel="Allowed sandbox kinds"
          idPrefix="limits-allowed-sandbox-kinds"
        />
      </div>

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <SummaryCard label="Pinning" value={summarizePinning(spec.limits?.pinning)} />
        <SummaryCard label="Tool guard ceiling" value={summarizeToolGuardCeiling(spec.limits?.toolGuard)} />
        <SummaryCard label="Content inspectors" value={summarizeContentInspectors(spec.limits?.contentInspectors)} />
        <SummaryCard label="Allowed MCP servers" value={summarizeAllowedMCPServers(spec.limits?.allowedMCPServers)} />
      </div>
    </section>
  );
}
