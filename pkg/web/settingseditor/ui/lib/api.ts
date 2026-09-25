// Wire shapes mirror cmd/oap/internal/desktop/settingsui (State, configResponse,
// configUpdateRequest/Response, clusterSettingsResponse,
// clusterSettingsUpdateRequest/Response, installInfoResponse) and
// pkg/web/settingseditor (Result, Violation) — field names are those Go
// structs' JSON tags.

// AppState mirrors settingsui.State (GET /api/state, and each GET /api/events
// frame). step is empty/absent when there's nothing more specific to say than
// the phase itself. kubectlCurrent has no omitempty on the Go side — it is
// always present, overlaid from Deps.KubectlCurrent by the server itself (a
// nil seam overlays false), never absent on the wire.
export interface AppState {
  running: boolean;
  phase: string;
  step?: string;
  kubectlCurrent: boolean;
}

// ConfigModel is the redacted model projection settingsui.configResponse
// carries — apiKeyMasked is "••••"+last 4 (or "" when unset), never the real key.
export interface ConfigModel {
  provider: string;
  apiKeyMasked: string;
  name: string;
}

// ConfigResponse mirrors settingsui.configResponse (GET /api/config).
export interface ConfigResponse {
  model: ConfigModel;
  providers: string[];
  ngrokConfigured: boolean;
  healthNotifications: boolean;
  passwordSet: boolean;
  configPath: string;
}

// ConfigUpdateModel mirrors settingsui.configUpdateRequest's Model group.
// apiKey "" means "keep the value already on disk" — there is no other way
// to signal "leave this alone" without echoing the secret back.
export interface ConfigUpdateModel {
  provider: string;
  apiKey: string;
  name: string;
}

// ConfigPasswordChange mirrors settingsui.configUpdateRequest's optional
// Password group — present only when the user is actually changing it.
export interface ConfigPasswordChange {
  current: string;
  new: string;
}

// ConfigUpdateRequest mirrors settingsui.configUpdateRequest (PUT /api/config).
// ngrokAuthToken "" likewise means "keep existing".
export interface ConfigUpdateRequest {
  model: ConfigUpdateModel;
  ngrokAuthToken: string;
  clearNgrok: boolean;
  healthNotifications: boolean;
  password?: ConfigPasswordChange;
}

// FieldOutcome mirrors settingsui.fieldOutcome — what happened to one changed
// field group as a result of a config PUT.
export interface FieldOutcome {
  field: string; // "model" | "password" | "ngrok" | "healthNotifications"
  outcome: string; // "applied" | "next-start" | "failed"
  message?: string;
}

// ConfigUpdateResponse mirrors settingsui.configUpdateResponse (PUT /api/config
// 200 body): one FieldOutcome per field group that actually changed.
export interface ConfigUpdateResponse {
  outcomes: FieldOutcome[];
}

// ClusterSettingsResponse mirrors settingsui.clusterSettingsResponse (GET
// /api/cluster/settings). clusterDown is a STATE (the desktop cluster is not
// up), not an error. spec is the raw ClusterAgentSettings spec JSON — typed
// fully as SettingsSpec in Task 11's cluster/spec.ts, kept as `unknown` here
// since this task only needs to round-trip it, not shape it.
export interface ClusterSettingsResponse {
  clusterDown: boolean;
  found: boolean;
  spec?: unknown;
  yaml?: string;
  managers?: string[];
}

// TokenWrite mirrors settingsui.tokenWrite — one central model-token Secret
// write, applied before the spec itself.
export interface TokenWrite {
  namespace: string;
  name: string;
  key: string;
  value: string;
}

// ClusterSettingsUpdateRequest mirrors settingsui.clusterSettingsUpdateRequest
// — the PUT /api/cluster/settings body, and (minus tokens/takeOwnership,
// which it ignores) the POST /api/cluster/settings/validate body too. Exactly
// one of spec/yaml must be set.
export interface ClusterSettingsUpdateRequest {
  spec?: unknown;
  yaml?: string;
  tokens?: TokenWrite[];
  takeOwnership?: boolean;
}

// Violation mirrors settingseditor.Violation — one advisory resolver finding.
export interface Violation {
  reason: string;
  message: string;
  fatal: boolean;
}

// ValidationResult mirrors settingseditor.Result: hard admission errors (the
// webhook would reject), advisory resolver violations, and — when the spec
// passes — the effective settings the cluster tier alone would produce.
// `effective` is v1alpha1.EffectiveSettings; left untyped here as no Task-9
// consumer renders it yet.
export interface ValidationResult {
  errors: string[];
  violations: Violation[];
  effective?: unknown;
}

// ClusterSettingsUpdateResponse mirrors settingsui.clusterSettingsUpdateResponse
// (PUT /api/cluster/settings 200/422 body).
export interface ClusterSettingsUpdateResponse {
  applied: boolean;
  validation: ValidationResult;
  drift?: string[];
}

// ComponentInfo mirrors settingsui.componentInfo — one Deployment in the
// system namespace.
export interface ComponentInfo {
  name: string;
  image: string;
  ready: string; // "1/1"
}

// EnvEntry mirrors settingsui.envEntry — one allowlisted operator env var.
export interface EnvEntry {
  name: string;
  value: string;
}

// NodeInfo mirrors settingsui.nodeInfo — the first node reported by the cluster.
export interface NodeInfo {
  name: string;
  kubeletVersion: string;
  ready: boolean;
}

// InstallInfoResponse mirrors settingsui.installInfoResponse (GET
// /api/cluster/install-info). clusterDown is a STATE, not an error, like
// ClusterSettingsResponse.
export interface InstallInfoResponse {
  clusterDown: boolean;
  clusterKind: string;
  components: ComponentInfo[];
  operatorEnv: EnvEntry[];
  node?: NodeInfo;
  appVersion: string;
}

export class ApiError extends Error {
  constructor(
    message: string,
    public status: number,
  ) {
    super(message);
  }
}

// parse mirrors pkg/web/adminui/ui/lib/api.ts's parse<T>: throws ApiError on
// a non-2xx response, reading the body only once (a Response body can't be
// read twice) as text and then trying to interpret it as this package's
// {error} JSON shape (cluster.go's writeJSONError). settingsui's config PUT
// handler (config.go) instead uses plain http.Error for its 400/403/500
// paths, which is plain text, not JSON — so a body that fails to parse as
// JSON falls back to the raw text itself (e.g. "settings: current password
// is incorrect") rather than a generic status message, as long as the body
// wasn't empty.
async function parse<T>(res: Response): Promise<T> {
  if (!res.ok) {
    const text = await res.text();
    let msg = text.trim() || `request failed (${res.status})`;
    try {
      const body = JSON.parse(text) as { error?: string };
      if (body.error) msg = body.error;
    } catch {
      // Not JSON — the raw text (already assigned to msg) is the message.
    }
    throw new ApiError(msg, res.status);
  }
  return (await res.json()) as T;
}

// postAction issues a POST with no request body, for the settings UI's
// fire-and-forget actions (switch kubectl context, reveal a file in the
// platform file browser) whose success response is 204 No Content — there is
// nothing to parse on success, only a possible {error}/text failure body via
// parse's shared error handling.
async function postAction(url: string): Promise<void> {
  const res = await fetch(url, { method: "POST" });
  if (!res.ok) {
    await parse(res); // throws ApiError; never returns
    return;
  }
}

export const getJSON = async <T>(url: string): Promise<T> => parse<T>(await fetch(url));

export const postJSON = async <T>(url: string, body: unknown): Promise<T> =>
  parse<T>(
    await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }),
  );

export const putJSON = async <T>(url: string, body: unknown): Promise<T> =>
  parse<T>(
    await fetch(url, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }),
  );

export const getState = (apiBase: string): Promise<AppState> => getJSON<AppState>(`${apiBase}/state`);

export const getConfig = (apiBase: string): Promise<ConfigResponse> =>
  getJSON<ConfigResponse>(`${apiBase}/config`);

export const putConfig = (apiBase: string, req: ConfigUpdateRequest): Promise<ConfigUpdateResponse> =>
  putJSON<ConfigUpdateResponse>(`${apiBase}/config`, req);

export const getClusterSettings = (apiBase: string): Promise<ClusterSettingsResponse> =>
  getJSON<ClusterSettingsResponse>(`${apiBase}/cluster/settings`);

// validateClusterSettings runs settingseditor.Validate over the request
// WITHOUT touching the cluster — it keeps working while the cluster is down.
export const validateClusterSettings = (
  apiBase: string,
  req: ClusterSettingsUpdateRequest,
): Promise<ValidationResult> => postJSON<ValidationResult>(`${apiBase}/cluster/settings/validate`, req);

export const putClusterSettings = (
  apiBase: string,
  req: ClusterSettingsUpdateRequest,
): Promise<ClusterSettingsUpdateResponse> =>
  putJSON<ClusterSettingsUpdateResponse>(`${apiBase}/cluster/settings`, req);

export const getInstallInfo = (apiBase: string): Promise<InstallInfoResponse> =>
  getJSON<InstallInfoResponse>(`${apiBase}/cluster/install-info`);

// tryParseValidationResult recovers the structured settingseditor.Result body
// a 422 from POST /cluster/settings/validate carries. parse<T> above throws
// on any non-2xx and only special-cases a `{error: string}` body
// (writeJSONError's shape) — this endpoint's 422 body is the Result itself
// (see handleClusterSettingsValidate), which has no "error" field, so
// ApiError.message ends up holding the raw JSON text. Parsing it back out
// here is what lets a failed validation still render structured
// errors/violations instead of a JSON blob in a generic error banner. Shared
// by ClusterTab (spec requests) and AdvancedTab (yaml requests) — the
// endpoint's response shape doesn't depend on which body it validated.
export function tryParseValidationResult(e: unknown): ValidationResult | null {
  if (!(e instanceof ApiError)) return null;
  try {
    const parsed = JSON.parse(e.message) as Partial<ValidationResult>;
    if (Array.isArray(parsed.errors)) return parsed as ValidationResult;
  } catch {
    // Not JSON — a genuine transport/auth error, not a validation failure.
  }
  return null;
}

// tryParseUpdateResponse is tryParseValidationResult's counterpart for PUT
// /cluster/settings: a 422 body is a clusterSettingsUpdateResponse (applied
// false, validation set, drift absent) with the same "no {error} field"
// shape, recovered the same way.
export function tryParseUpdateResponse(e: unknown): ClusterSettingsUpdateResponse | null {
  if (!(e instanceof ApiError)) return null;
  try {
    const parsed = JSON.parse(e.message) as Partial<ClusterSettingsUpdateResponse>;
    if (parsed.validation && Array.isArray(parsed.validation.errors)) {
      return parsed as ClusterSettingsUpdateResponse;
    }
  } catch {
    // Not JSON — a genuine transport error (409 cluster-down, 500, ...).
  }
  return null;
}

// useKubectl switches the current kubectl context to the desktop cluster
// (POST /api/kubectl/use). Named for the verb the General tab's button
// performs, not the wire route, to read naturally at the call site.
export const useKubectl = (apiBase: string): Promise<void> => postAction(`${apiBase}/kubectl/use`);

// revealConfig opens the platform file browser on config.json
// (POST /api/reveal/config).
export const revealConfig = (apiBase: string): Promise<void> => postAction(`${apiBase}/reveal/config`);

// revealLogs opens the platform file browser on the log directory
// (POST /api/reveal/logs).
export const revealLogs = (apiBase: string): Promise<void> => postAction(`${apiBase}/reveal/logs`);
