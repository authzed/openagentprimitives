// SettingsSpec mirrors v1alpha1.SettingsSpec (pkg/apis/v1alpha1/settings_common_types.go)
// for the curated surface this tab edits: limits.budget/authz/minPlanGateMode/
// deniedModels/deniedSkills/allowModelOverride/nativeFileHandling/
// requireSubagentDigestPins/allowedToolkits/allowedSkills/allowedSandboxKinds
// (editable), limits.pinning/toolGuard/contentInspectors/allowedMCPServers
// (read-only summary — edited in the Advanced/raw-YAML tab instead, see the
// module doc below), defaults.model/budget/authz/reportSessionCost/sandbox
// (editable; defaults.authz.planGate/metaagent summary-only), and
// modelCatalog (editable, full-array round-trip).
//
// Every interface here is a PARTIAL mirror: the runtime object loaded from
// GET /api/cluster/settings may carry additional fields this file doesn't
// name (requireStandingFor, builderClasses, maxWorkshopsPerStarter,
// ModelCatalogEntry.routing, ...). `updateSpec` is a generic, path-based
// deep-set that clones only the objects on the edited path and preserves
// every sibling key — including ones this file never typed — so a save
// round-trips fields this curated editor doesn't understand rather than
// dropping them. Do not replace it with a reconstruction that lists fields
// explicitly; that would silently truncate the spec to what got typed here.
//
// Deliberate narrowing (flagged in the task-11 brief): the wizard-shaped
// guard policies (pinning, toolGuard ceiling/policy rules, contentInspectors,
// allowedMCPServers) are summary-cards-plus-Advanced here, not editable
// toggles — recomposing them from a curated form risks clobbering
// hand-authored rules the wizard/raw editor wrote. The raw YAML tab (Task 12)
// is the one full-fidelity editor for those fields.

export interface NamespacedSecretKeyRef {
  namespace: string;
  name: string;
  key: string;
}

export interface SecretKeyRef {
  name: string;
  key: string;
}

export interface SettingsBudgetCeiling {
  maxTurns?: number;
  maxTokens?: number;
  maxDuration?: string; // Go duration string, e.g. "30m"
  sessionExpiration?: string;
  maxDelegatedAgents?: number;
}

export interface SettingsAuthzCeiling {
  maxApprovalTimeout?: string;
  maxInformationLeakageApprovalTTL?: string;
}

export interface AllowedMCPServer {
  name: string;
  tools?: string[];
}

export interface PinningRule {
  kind: string;
  minStrength?: string;
  mode?: string;
}

export interface PinningBypass {
  kind: string;
  [key: string]: unknown;
}

export interface PinningPolicy {
  rules?: PinningRule[];
  bypass?: PinningBypass[];
}

export interface ToolGuardCeiling {
  maxFailureThreshold?: number;
  minInitialCoolOff?: string;
  minAction?: string;
  maxCallsPerTurn?: number;
  maxCalls?: number;
  window?: string;
  maxEgressBytes?: number;
  maxIngressBytes?: number;
}

export interface ContentInspectorConfig {
  id: string;
  config?: unknown;
}

export interface SettingsLimits {
  budget?: SettingsBudgetCeiling;
  authz?: SettingsAuthzCeiling;
  deniedModels?: string[];
  allowModelOverride?: boolean;
  minPlanGateMode?: string; // "" | "disabled" | "logging" | "enforcing"
  allowedToolkits?: string[]; // tri-state: absent/[]/[items]
  allowedMCPServers?: AllowedMCPServer[]; // tri-state; read-only summary here
  allowedSkills?: string[]; // tri-state
  deniedSkills?: string[];
  pinning?: PinningPolicy; // read-only summary here
  toolGuard?: ToolGuardCeiling; // read-only summary here
  contentInspectors?: ContentInspectorConfig[]; // tri-state; read-only summary here
  nativeFileHandling?: boolean;
  requireSubagentDigestPins?: boolean;
  allowedSandboxKinds?: string[]; // tri-state
}

export interface DefaultModel {
  provider: string;
  name: string;
  fromCatalog?: string;
  apiKey?: SecretKeyRef;
}

export interface BudgetConfig {
  maxTurns?: number;
  maxTokens?: number;
  maxDuration?: string;
  sessionExpiration?: string;
  maxDelegatedAgents?: number;
}

export interface PlanGateConfig {
  mode?: string; // summary-only here
}

export interface MetaagentConfig {
  trigger?: string; // summary-only here
}

export interface DefaultAuthz {
  approvalTimeout?: string;
  informationLeakageApprovalTTL?: string;
  scopeMaxLlmLatencyMs?: number;
  planGate?: PlanGateConfig;
  metaagent?: MetaagentConfig;
}

export interface ToolGuardPolicy {
  rules?: unknown[]; // summary-only here (rule count)
}

export interface SandboxBackend {
  kind?: string;
  config?: unknown;
}

export interface SettingsDefaults {
  model?: DefaultModel;
  budget?: BudgetConfig;
  authz?: DefaultAuthz;
  toolGuard?: ToolGuardPolicy; // summary-only here
  reportSessionCost?: boolean;
  sandbox?: SandboxBackend;
}

export interface ModelCatalogEntry {
  name: string;
  provider?: string;
  tokenRef?: NamespacedSecretKeyRef;
  default?: boolean;
  inputPerMTok?: number;
  outputPerMTok?: number;
  routing?: unknown; // not curated; preserved on round-trip via updateSpec
}

export interface SettingsSpec {
  limits?: SettingsLimits;
  defaults?: SettingsDefaults;
  modelCatalog?: ModelCatalogEntry[];
}

// --- updateSpec: pure, generic, path-based immutable setter -----------------

type JsonRecord = Record<string, unknown>;

function isPlainObject(v: unknown): v is JsonRecord {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

// updateSpec returns a NEW spec with `value` set at `path`, without mutating
// `spec`. Only the objects along `path` are cloned; every sibling — typed
// here or not — is carried over by reference via spread. `value === undefined`
// DELETES the key at that path rather than setting it to a literal undefined,
// which is how the JSON wire tri-states are expressed: an absent key ("no
// ceiling"/"unrestricted") is a different fact than a present key holding an
// empty array ("deny-all") or `false`.
export function updateSpec(spec: SettingsSpec, path: readonly string[], value: unknown): SettingsSpec {
  if (path.length === 0) {
    throw new Error("updateSpec: path must not be empty");
  }
  return setPath(spec as JsonRecord, path, value) as SettingsSpec;
}

// getPath is updateSpec's read-side counterpart: walks `path` through `obj`
// and returns whatever is there, or undefined the moment the walk hits a
// non-object (including past a missing key). Shared by every field control
// in fields.tsx so a control's read side and write side agree on exactly
// what a path means.
export function getPath(obj: unknown, path: readonly string[]): unknown {
  let cur: unknown = obj;
  for (const key of path) {
    if (!isPlainObject(cur)) return undefined;
    cur = cur[key];
  }
  return cur;
}

function setPath(obj: JsonRecord, path: readonly string[], value: unknown): JsonRecord {
  const [key, ...rest] = path;
  const next: JsonRecord = { ...obj };
  if (rest.length === 0) {
    if (value === undefined) {
      delete next[key];
    } else {
      next[key] = value;
    }
    return next;
  }
  const childRaw = next[key];
  const child = isPlainObject(childRaw) ? childRaw : {};
  next[key] = setPath(child, rest, value);
  return next;
}

// --- bool tri-state (undefined | true | false) ------------------------------

export type BoolTriState = "unset" | "true" | "false";

export function boolTriState(value: boolean | undefined): BoolTriState {
  if (value === undefined) return "unset";
  return value ? "true" : "false";
}

export function boolFromTriState(state: BoolTriState): boolean | undefined {
  if (state === "unset") return undefined;
  return state === "true";
}

// --- *[]string ceiling tri-state (undefined | [] | [items]) ----------------

export type ListTriState = "unrestricted" | "deny-all" | "restricted";

// listTriState classifies a tri-state allowlist's current value: absent
// (no ceiling from this tier), present-and-empty (deny-all), or
// present-and-populated (restricted to the named set).
export function listTriState(list: string[] | undefined): ListTriState {
  if (list === undefined) return "unrestricted";
  if (list.length === 0) return "deny-all";
  return "restricted";
}

// --- newline-textarea <-> string[] for plain union lists (deniedModels,
// deniedSkills) — these are NOT tri-state: a plain []string with omitempty
// has no "deny-all vs unset" distinction, so an emptied textarea deletes the
// key exactly like the ceiling lists' "unrestricted" state does. ------------

export function parseLines(text: string): string[] {
  return text
    .split("\n")
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
}

export function joinLines(items: string[] | undefined): string {
  return (items ?? []).join("\n");
}

// setUnionList sets a plain (non-tri-state) string-list field from newline
// text: a non-empty parse sets the array, an empty parse deletes the key.
export function setUnionList(spec: SettingsSpec, path: readonly string[], text: string): SettingsSpec {
  const items = parseLines(text);
  return updateSpec(spec, path, items.length > 0 ? items : undefined);
}

// --- model catalog helpers ---------------------------------------------------

// defaultTokenRefFor is the token Secret target a NEW catalog entry gets
// unless/until edited — see the task-11 brief's "Token secret defaults for
// NEW catalog entries" note.
export function defaultTokenRefFor(entryName: string): NamespacedSecretKeyRef {
  return { namespace: "agentprimitives-system", name: `model-default-token-${entryName}`, key: "token" };
}

// upsertCatalogEntry replaces the entry named `entry.name` (or appends it)
// in spec.modelCatalog, ATOMICALLY: it always writes back the FULL array
// (every untouched entry included) since ModelCatalog round-trips as one
// list. Enforces at-most-one default client-side: an entry saved with
// default:true clears default on every other entry in the same write.
export function upsertCatalogEntry(spec: SettingsSpec, entry: ModelCatalogEntry): SettingsSpec {
  const existing = spec.modelCatalog ?? [];
  const idx = existing.findIndex((e) => e.name === entry.name);
  const next = existing.slice();
  if (idx >= 0) {
    next[idx] = entry;
  } else {
    next.push(entry);
  }
  const deduped = entry.default ? next.map((e) => (e.name === entry.name ? e : { ...e, default: false })) : next;
  return updateSpec(spec, ["modelCatalog"], deduped);
}

// removeCatalogEntry drops the named entry from spec.modelCatalog, writing
// back the full remaining array.
export function removeCatalogEntry(spec: SettingsSpec, name: string): SettingsSpec {
  const existing = spec.modelCatalog ?? [];
  return updateSpec(
    spec,
    ["modelCatalog"],
    existing.filter((e) => e.name !== name),
  );
}

// replaceCatalogEntry is upsertCatalogEntry that also handles a rename:
// removes whatever entry was named `oldName` first, then upserts `entry`
// (which may carry a different name). For a non-renaming edit or a brand-new
// entry, oldName === entry.name and the remove is a no-op.
export function replaceCatalogEntry(spec: SettingsSpec, oldName: string, entry: ModelCatalogEntry): SettingsSpec {
  return upsertCatalogEntry(removeCatalogEntry(spec, oldName), entry);
}

// setDefaultCatalogEntry marks exactly the named entry as default, clearing
// every other entry's default flag in the same write.
export function setDefaultCatalogEntry(spec: SettingsSpec, name: string): SettingsSpec {
  const existing = spec.modelCatalog ?? [];
  const next = existing.map((e) => ({ ...e, default: e.name === name }));
  return updateSpec(spec, ["modelCatalog"], next);
}

// --- read-only summary flattening, mirroring pkg/web/admind/settings.go's
// pinningValue (line ~217) and toolGuardCeilingValue (line ~241) phrasing. --

export function summarizePinning(p: PinningPolicy | undefined): string {
  const rules = p?.rules ?? [];
  const bypass = p?.bypass ?? [];
  if (rules.length === 0 && bypass.length === 0) return "—";
  const parts = rules.map((r) => `${r.kind}(min=${r.minStrength || "any"},mode=${r.mode || "approve"})`);
  let out = parts.join(", ");
  if (bypass.length > 0) out += ` · ${bypass.length} bypass`;
  return out;
}

export function summarizeToolGuardCeiling(tg: ToolGuardCeiling | undefined): string {
  if (!tg) return "—";
  const parts: string[] = [];
  if (tg.maxFailureThreshold !== undefined) parts.push(`maxFailureThreshold=${tg.maxFailureThreshold}`);
  if (tg.minInitialCoolOff !== undefined) parts.push(`minInitialCoolOff=${tg.minInitialCoolOff}`);
  if (tg.minAction !== undefined) parts.push(`minAction=${tg.minAction}`);
  if (tg.maxCallsPerTurn !== undefined) parts.push(`maxCallsPerTurn=${tg.maxCallsPerTurn}`);
  if (tg.maxCalls !== undefined && tg.window !== undefined) parts.push(`maxCalls=${tg.maxCalls}/${tg.window}`);
  if (tg.maxEgressBytes !== undefined) parts.push(`maxEgressBytes=${tg.maxEgressBytes}`);
  if (tg.maxIngressBytes !== undefined) parts.push(`maxIngressBytes=${tg.maxIngressBytes}`);
  return parts.length === 0 ? "—" : parts.join(", ");
}

export function summarizeContentInspectors(list: ContentInspectorConfig[] | undefined): string {
  if (list === undefined) return "not set";
  if (list.length === 0) return "(none)";
  return list.map((ci) => ci.id).join(", ");
}

export function summarizeAllowedMCPServers(list: AllowedMCPServer[] | undefined): string {
  if (list === undefined) return "no constraint";
  if (list.length === 0) return "deny-all";
  return list.map((s) => s.name).join(", ");
}
