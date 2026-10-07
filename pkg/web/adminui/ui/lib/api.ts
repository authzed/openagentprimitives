// Wire shapes mirror pkg/web/admind (SessionState, RecentEvent) and
// pkg/web/admind/audit (Event, QueryRequest/Response, FacetsResponse,
// EntityRow) — field names are those structs' JSON tags.

export interface SessionState {
  namespace: string;
  name: string;
  class?: string;
  model?: string;
  channelKind?: string;
  // channelName is the input Channel CR's name (namespaced under the session's
  // namespace → channel id = `${namespace}/${channelName}`).
  channelName?: string;
  phase?: string;
  startedAt?: string;
  // startedBy is the creating user's canonical subject (decodeSubject renders it).
  startedBy?: string;
  turnCount: number;
  inputTokens: number;
  outputTokens: number;
  toolCallCount: number;
  elapsedSeconds: number;
  active: boolean;
  activityCause?: string;
  statusText?: string;
  plan?: unknown;
  pendingToolGrants: number;
  pendingLeakageApprovals: number;
  pendingContentInspectionApprovals?: number;
  lastEventAt: string;
}

export interface RecentEvent {
  at: string;
  kind: string;
  payload?: unknown;
}

// BundleSandbox mirrors pkg/web/admind.BundleSandbox — one AgentSession bundle's
// sandbox backend, fetched live (per bundle) from its SpiceboxSession CR when
// the session detail page loads. sandboxKind/sandboxRef/prewarmed/phase are
// absent when that SpiceboxSession has no sandbox yet, or could not be read.
// A session's bundles can legitimately report DIFFERENT sandboxKind values —
// this is one entry per bundle, never collapsed to a session-level value.
export interface BundleSandbox {
  name: string;
  spiceboxSessionName: string;
  agentIdentity?: string;
  sandboxKind?: string;
  sandboxRef?: string;
  prewarmed?: boolean;
  phase?: string;
  reason?: string;
  // workspaceMode is "shared" (RWX claim across bundles) or "isolated"
  // (pod-local /work only). Empty when the bundle's SpiceboxSession could not
  // be read.
  workspaceMode?: string;
}

export interface SessionDetail extends SessionState {
  recentEvents: RecentEvent[];
  // bundles is omitted (not just empty) for a session with no resolved tool
  // bundles.
  bundles?: BundleSandbox[];
}

// LogBlock mirrors pkg/web/admind.logBlock — one structured content block of a
// transcript turn. Only the fields relevant to `type` are populated: text →
// {text}; tool_use → {name, input}; tool_result → {content, isError}. input and
// content are already-parsed JSON values (object/array/string/…) — JSONView
// renders them directly.
export interface LogBlock {
  type: "text" | "tool_use" | "tool_result";
  text?: string;
  name?: string;
  input?: unknown;
  content?: unknown;
  isError?: boolean;
}

// SessionLogEntry mirrors pkg/web/admind.sessionLogEntry — one decoded turn of the
// full transcript (B4 session-logs endpoint), returned oldest-first. `content`
// is the flattened backward-compat summary; `blocks` is the structured
// decomposition the rich viewer renders (falling back to `content` when absent).
export interface SessionLogEntry {
  kind: string;
  id: string;
  index: number;
  role?: string;
  createdAt: string;
  content: string;
  // actor is the subject that authored this turn — the true per-turn author
  // when persisted, else (actorInferred=true) the session initiator fallback.
  actor?: string;
  // actorInferred is true only when actor came from the session-level
  // started-by fallback (a legacy turn), not the turn's own author.
  actorInferred?: boolean;
  // blocks is the typed content decomposition; absent for malformed turns
  // (content carries the raw payload instead).
  blocks?: LogBlock[];
}

// SessionLogsResponse mirrors pkg/web/admind.sessionLogsResponse. truncated=true
// means older turns were dropped (only the most recent cap returned).
export interface SessionLogsResponse {
  entries: SessionLogEntry[];
  truncated: boolean;
}

// ToolCall mirrors pkg/web/admind.ToolCall — one entry in the cluster-wide
// tool-activity stream (newest-first from the server).
export interface ToolCall {
  t: string;
  namespace: string;
  name: string;
  agentClass?: string;
  tool: string;
}

// Approval mirrors pkg/web/admind.Approval — one pending approval in the
// cluster-wide read-only queue. Kind ∈ "tool_call" | "leakage" |
// "content_inspection". tool/requester are present only for the kinds that
// carry them (leakage has no tool; content_inspection has no requester).
export interface Approval {
  kind: string;
  tool?: string;
  namespace: string;
  name: string;
  agentClass?: string;
  requester?: string;
  requestedAt: string;
}

export interface AuditEvent {
  time: string;
  sessionNamespace: string;
  sessionName: string;
  agentClass?: string;
  kind: string;
  actor?: string;
  tool?: string;
  outcome: string;
  summary: string;
  entryId: string;
  raw?: unknown;
}

export interface AuditQueryRequest {
  since?: string;
  until?: string;
  kinds?: string[];
  sessionNamespace?: string;
  sessionName?: string;
  agentClass?: string;
  tool?: string;
  actor?: string;
  outcome?: string;
  limit?: number;
  offset?: number;
}

export interface AuditQueryResponse {
  events: AuditEvent[];
  hasMore: boolean;
  truncated: boolean;
}

export interface FacetsResponse {
  counts: Record<string, Record<string, number>>;
  truncated: boolean;
}

export interface EntityRow {
  key: string;
  events: number;
  denied: number;

  // The fields below are populated ONLY for the "sessions" axis (the handler
  // joins each "ns/name" key against the live aggregator snapshot + the
  // started-by annotation). They are omitted on the other axes, and empty even
  // on the sessions axis when a row's session has been GC'd out of the live
  // aggregator (audit outlives sessions).
  status?: string;
  inputTokens?: number;
  outputTokens?: number;
  // null when the resolved model has no known price (encoded as JSON null by
  // the Go cost.USD marshaler, which cannot emit NaN); rendered as "NaN".
  estimatedCostUSD?: number | null;
  startedBy?: string;
  // startedAt is the session start (status.startedAt, else the CR creation time);
  // rendered as a humanized relative time in the sessions rollup Started column.
  startedAt?: string;
}

// ArtifactRow mirrors pkg/web/admind.artifactRow — one ArtifactRender cluster-wide.
// session is the owning AgentSession ("ns/name"), or "" when unlinked. revisions
// is the number of renders sharing the logical artifact-id label. artifactId is
// the logical artifact id (the artifact-id label) shared by every revision of
// one logical artifact; unlabeled renders fall back to their own name, so each
// is its own single-revision track.
export interface ArtifactRow {
  name: string;
  namespace: string;
  session: string;
  kind: string;
  phase: string;
  mime: string;
  size: number;
  revisions: number;
  created: string;
  artifactId: string;
}

// ArtifactRevision mirrors pkg/web/admind.artifactRevision — one ArtifactRender
// sharing the logical artifact-id label (a version of the same artifact).
export interface ArtifactRevision {
  name: string;
  phase: string;
  mime: string;
  size: number;
  created: string;
}

// ArtifactDetail mirrors pkg/web/admind.artifactDetail (GET /artifacts/{ns}/{name}):
// the render's own metadata plus every revision (newest-first). viewPath is the
// UNSIGNED /artifact-view base — admind cannot mint the subject-scoped signed
// link, so opening the viewer is best-effort (a signed admin-view link is a
// known backend follow-up). outputRef is the artifactstore content ref, empty
// until the render reaches Ready.
export interface ArtifactDetail {
  name: string;
  namespace: string;
  session: string;
  kind: string;
  phase: string;
  mime: string;
  size: number;
  created: string;
  revisions: ArtifactRevision[];
  viewPath: string;
  outputRef: string;
}

// WorkshopRow mirrors pkg/web/admind.workshopRow — one Workshop projected for
// the admin Workshops table. Nothing here carries a secret: it is
// namespace/name/starter metadata and the install/capability request STATE,
// never any answered value. installPhase/installedRef/suggestedName are
// absent (not just empty) until an install has been requested/observed.
export interface WorkshopRow {
  namespace: string;
  name: string;
  // session is "ns/name" of the builder AgentSession this workshop belongs to.
  session: string;
  // starter is the canonical id of the person who started the builder session.
  starter: string;
  // phase is the workshop lifecycle phase (status.phase), not the install phase.
  phase: string;
  // installPhase is status.install.phase (Requested→Installed/Declined/Failed).
  installPhase?: string;
  // installedRef is "<namespace>/<name>" of the installed AgentClass, set once
  // an install succeeds.
  installedRef?: string;
  // suggestedName is the name the builder proposed for the installed agent.
  suggestedName?: string;
  // pendingInstall is true when the builder asked to install and no admin has
  // reached a terminal decision (Installed or Declined) yet.
  pendingInstall: boolean;
  // pendingCapability is true when the builder recommended a new capability.
  pendingCapability: boolean;
  // exported is true once the workshop has a drafted bundle in the store.
  exported: boolean;
  // created is metadata.creationTimestamp, RFC3339.
  created: string;
}

// WorkshopDecisionResponse mirrors pkg/web/admind.workshopDecisionResponse —
// the 200 body for decline (and any future non-install state write).
export interface WorkshopDecisionResponse {
  phase: string;
  approvedBy: string;
}

// AccessTokenRow mirrors pkg/web/admind.accessTokenRow — one AccessToken
// projected for the admin Tokens table. owner is the BARE canonical user id
// (spec.owner), not a "user:"-prefixed subject — decode it by prefixing
// "user:" before calling decodeSubject, the same way EntityRollups/AccessView
// decode a users-axis key. role is "unknown" when the SpiceDB grant could not
// be read (never the same claim as revoked — see tokens.go). Nothing here
// ever carries spec.tokenHash.
export interface AccessTokenRow {
  name: string;
  owner: string;
  clientName?: string;
  role: string;
  scopeClasses?: string[];
  unfiltered: boolean;
  createdAt: string;
  expiresAt: string;
  lastUsedAt?: string;
  revoked: boolean;
}

// AccessTokensData mirrors pkg/web/admind.tokensResponse (GET /tokens).
export interface AccessTokensData {
  tokens: AccessTokenRow[];
}

// AccessTokenRevokeResponse mirrors pkg/web/admind.accessTokenRevokeResponse
// (the 200 body for POST /tokens/revoke).
export interface AccessTokenRevokeResponse {
  ok: boolean;
}

// MemoryKindRollup mirrors pkg/web/admind.memoryKindRollup — one per-Kind rollup.
export interface MemoryKindRollup {
  kind: string;
  entries: number;
  scopes: number;
  lastWrite: string;
  appendOnly: boolean;
}

// MemoryEntryRow mirrors pkg/web/admind.memoryEntryRow — one entry from a ?kind=
// drill-in or a ?q= search hit (score present only for search).
export interface MemoryEntryRow {
  kind: string;
  scope: string;
  id: string;
  created: string;
  score?: number;
}

// MemoryData mirrors pkg/web/admind.memoryResponse.
export interface MemoryData {
  rollups: MemoryKindRollup[];
  entries?: MemoryEntryRow[];
  truncated: boolean;
}

// Overview wire shapes mirror pkg/web/admind/overview (Overview, ModelTokens,
// ClassTokens, HourBucket, KPIs) + the handler's budget envelope — field
// names are those Go structs' JSON tags.
export interface OverviewModelTokens {
  model: string;
  inputTokens: number;
  outputTokens: number;
  sessions: number;
}

export interface OverviewClassTokens {
  class: string;
  inputTokens: number;
  outputTokens: number;
  sessions: number;
}

export interface OverviewHourBucket {
  hourStartUnix: number;
  inputTokens: number;
  outputTokens: number;
}

export interface OverviewKPIs {
  activeSessions: number;
  tokensToday: number;
  toolCalls24h: number;
  denials24h: number;
  approvalsPending: number;
}

export interface OverviewBudget {
  tokensSpent: number;
  tokenCeiling?: number;
  // null when any tracked model has no known price (NaN poisons the sum, and the
  // Go cost.USD marshaler encodes NaN as null); rendered as "NaN".
  estimatedCostUSD: number | null;
  estimated: boolean;
}

export interface OverviewData {
  byModel: OverviewModelTokens[];
  byAgentClass: OverviewClassTokens[];
  series24h: OverviewHourBucket[];
  kpis: OverviewKPIs;
  computedAt: string;
  truncated: boolean;
  budget: OverviewBudget;
}

// Budget wire shapes mirror pkg/web/admind (BudgetRow, BudgetBreakdown) — field
// names are those Go structs' JSON tags. estimatedCostUSD is a list-price
// estimate (never a metered bill); `estimated` is always true.
export interface BudgetRow {
  key: string;
  inputTokens: number;
  outputTokens: number;
  // null when the model has no known price (Go cost.USD marshals NaN as null);
  // rendered as "NaN".
  estimatedCostUSD: number | null;
}

export interface BudgetBreakdown {
  byModel: BudgetRow[];
  byAgentClass: BudgetRow[];
  bySession: BudgetRow[];
  byUser: BudgetRow[];
  estimated: boolean;
}

// Health wire shapes mirror pkg/web/admind/health (Report, Component, Rollup).
export type HealthStatus = "Healthy" | "Degraded" | "Down" | "NotConfigured";

export interface HealthComponent {
  name: string;
  status: HealthStatus;
  detail: string;
}

export interface HealthRollup {
  pods: number;
  ready: number;
  cpu: string;
  memory: string;
}

export interface HealthSnapshot {
  components: HealthComponent[];
  rollup: HealthRollup;
}

// Config wire shapes mirror pkg/web/admind/config (Badge, Count, ResourceRow),
// pkg/web/admind/access (accessAdmin, accessStats, accessResponse), and
// pkg/web/admind/settings (settingsRow, settingsResponse) — field names are those
// Go structs' JSON tags.
export interface Badge {
  key: string;
  value: string;
}

export interface Count {
  label: string;
  value: number;
}

// ResourceRow is the flattened, presentation-agnostic view of one config CR,
// shared by every generic config table. omitempty fields are optional here.
export interface ResourceRow {
  name: string;
  namespace?: string;
  scope: string;
  status: string;
  statusReason?: string;
  badges?: Badge[];
  counts?: Count[];
  manageCmd?: string;
}

// Config-detail wire shapes mirror pkg/web/admind/config/detail.go (ResourceDetail,
// Section, Field, Link, ListItem) — field names are those Go structs' JSON tags.
// The detail endpoint is the richer per-resource view behind the tabbed detail
// pages: Overview scalars + a list of Sections the UI renders as tabs.

// DetailLink references another admin entity's detail page. entity is the UI
// entity slug ("agent"/"tool"/"identity"/"source"/"skill"/…); id is the detail
// route id ("<ns>/<name>" namespaced, bare "<name>" cluster-scoped).
export interface DetailLink {
  entity: string;
  id: string;
}

// DetailField is one label/value row in a "fields" Section; link turns value
// into an in-console navigation link. href, when non-empty, renders value as the
// TEXT of an external anchor to that URL (scheme-guarded) — e.g. a skill-source
// Commit shows the short SHA linked to the full GitHub commit URL.
export interface DetailField {
  label: string;
  value: string;
  link?: DetailLink;
  href?: string;
}

// DetailListItem is one row in a "list" Section (a tool, a credential, a
// subcommand).
export interface DetailListItem {
  title: string;
  subtitle?: string;
  link?: DetailLink;
  // href, when set, makes title the anchor text of an external (scheme-guarded)
  // link — a synced repository row showing `demo-org/widgets` and linking the
  // full URL. An internal console route uses link instead, and wins over href.
  href?: string;
  badges?: Badge[];
}

// SectionKind discriminates which content shape a Section carries.
export type SectionKind = "fields" | "text" | "list";

// Section is one tab of a ResourceDetail.
//
// `text` carries two different things depending on `kind`: for "text" it is the
// block content, for "list" it is an optional NOTICE about the list — rendered
// as a destructive alert above the rows (SectionRenderer's ListSection) so a
// backend that could read only PART of a list never presents a short one as
// complete. Setting it on a list section for any other reason produces an alert
// the projector did not intend.
export interface Section {
  id: string;
  label: string;
  kind: SectionKind;
  fields?: DetailField[]; // kind === "fields"
  text?: string; // kind === "text": the block; kind === "list": a notice about the list
  items?: DetailListItem[]; // kind === "list"
}

// ResourceDetail is the resource-agnostic detail view: the Overview scalars plus
// the per-tab Sections.
export interface ResourceDetail {
  name: string;
  namespace?: string;
  scope: string;
  status: string;
  statusReason?: string;
  description?: string;
  manageCmd?: string;
  sections?: Section[];
}

// AccessAdmin is one platform-admin grant. kind ∈ "user" | "group".
export interface AccessAdmin {
  subject: string;
  kind: string;
}

// AccessStats is the (degraded for v1) SpiceDB stats block. When available is
// false the counts are null and the UI shows a "stats unavailable" panel.
export interface AccessStats {
  schemaDefs: number | null;
  relationships: number | null;
  available: boolean;
}

export interface AccessData {
  admins: AccessAdmin[];
  grantCmd: string;
  stats: AccessStats;
}

// SettingsRow is one labeled fact grouped under "Limits" or "Defaults".
export interface SettingsRow {
  group: string;
  label: string;
  value: string;
  note?: string;
}

// SettingsData is the bespoke ClusterAgentSettings projection. note is set only
// when there is no ClusterAgentSettings (rows is then empty).
export interface SettingsData {
  rows: SettingsRow[];
  manageCmd: string;
  note?: string;
}

// KG wire shapes mirror pkg/memory (KGFact, KGEntity, KGCommunity) — field
// names are those Go structs' JSON tags.
export interface KGFact {
  uuid: string;
  name: string;
  fact: string;
  fromEntity: string;
  toEntity: string;
  validAt?: string;
  invalidAt?: string;
}

export interface KGEntity {
  uuid: string;
  name: string;
  summary: string;
  attributes?: Record<string, string>;
}

export interface KGCommunity {
  uuid: string;
  name: string;
  summary: string;
  members: string[];
}

// KG response envelopes mirror pkg/web/admind/kg.go. `available` is false ONLY for
// the not-configured degraded payload (which carries `note`); otherwise true,
// with an optional `error` set when the provider failed mid-request (still 200,
// a degraded result rather than a blank failure).
export interface KGFactsResponse {
  available: boolean;
  facts?: KGFact[];
  note?: string;
  error?: string;
}

export interface KGEntityResponse {
  available: boolean;
  entity?: KGEntity | null;
  note?: string;
  error?: string;
}

export interface KGEntitiesResponse {
  available: boolean;
  entities?: KGEntity[];
  note?: string;
  error?: string;
}

export interface KGCommunitiesResponse {
  available: boolean;
  communities?: KGCommunity[];
  note?: string;
  error?: string;
}

// ClusterInfo mirrors pkg/web/admind.ClusterInfo (GET /cluster) — the best-effort
// cluster identity for the admin header. type is always set (one of
// gke|eks|aks|local|unknown); name/consoleURL/distribution are filled only when
// cleanly derivable and omitted (empty) otherwise — the header degrades
// gracefully rather than guessing.
export type ClusterType = "gke" | "eks" | "aks" | "local" | "unknown";

export interface ClusterInfo {
  name?: string;
  type: ClusterType | string;
  consoleURL?: string;
  distribution?: string;
}

export class ApiError extends Error {
  constructor(message: string, public status: number) {
    super(message);
  }
}

// OapQuestionType mirrors pkg/platform/oap.QuestionType — the install-time answer kind
// the auto-generated install form dispatches on to pick an input control.
export type OapQuestionType = "string" | "int" | "bool" | "enum" | "secret" | "resourceList";

// OapQuestion mirrors pkg/platform/oap.Question (the fields the form needs — Binding
// and Secret target the backend's CR-overlay/Secret-creation plumbing and are
// not rendered). required is undefined when the manifest left it unset, which
// Question.IsRequired() treats as required=true — the form must mirror that
// default rather than treating an absent value as optional.
export interface OapQuestion {
  name: string;
  // agentPath is empty/absent for the root. A dependency question retains its
  // local name; callers qualify it externally as agents.<child>[.agents.<grandchild>].<name>.
  agentPath?: string;
  type: OapQuestionType;
  prompt: string;
  description?: string;
  default?: unknown;
  required?: boolean;
  enum?: string[];
  // enumLabels is what the operator READS for each enum entry, paired with
  // `enum` POSITIONALLY. Absent means each value is its own label, which is the
  // common case.
  //
  // It is here because oap.Manifest.ValidateQuestions ACCEPTS it: a bundle
  // author who declares labels sees them in `oap agent install` and, without
  // this, would silently lose them here — the same "silently ignored is worse
  // than absent" hazard the validators close for binding, secret and
  // validation. The values are stable answer keys, routinely not sentences a
  // human should be asked to choose between: a channel wizard offering the
  // Slack-app routes stores "false"/"true"/"provision" while the row should
  // read "Show me the manifest — I'll create it myself".
  enumLabels?: string[];
  validation?: string;
}

// SkillClone mirrors pkg/platform/oap.SkillClone — an external repository a bundled
// SkillSource clones on install. Surfaced so the install form can show the
// operator the network reach-out before it happens.
export interface SkillClone {
  skillSource: string;
  repoURL: string;
  ref?: string;
  subpath?: string;
}

// OapInstallResult mirrors pkg/web/admind.oapInstallResponse (the 200 body).
// adopted lists the conflicts (as "Kind/Name") that this install seized —
// absent when the request carried no `adopt` list at all.
export interface OapInstallResult {
  agentPath?: string;
  name: string;
  appliedKinds: string[];
  secretsCreated: number;
  warnings?: string[];
  skillClones?: SkillClone[];
  adopted?: string[];
  channels?: OapInstallChannel[];
  agents?: OapInstallResult[];
}

// OapInstallConflict mirrors pkg/web/admind.oapInstallConflict: one pre-existing
// object the install would seize. `secret` marks a Secret — adopting one
// overwrites its data, so the UI never pre-ticks it.
export interface OapInstallConflict {
  agentPath?: string;
  kind: string;
  namespace?: string;
  name: string;
  secret?: boolean;
}

export type OapInstallChannelStatus = "ask" | "handoff" | "alreadyWired" | "conflict" | "unknown" | "unavailable";

export interface OapInstallChannelSeed {
  key: string;
  value?: string;
  source?: string;
  redacted?: boolean;
}

export interface OapInstallChannelNote {
  key: string;
  reason: string;
}

export interface OapInstallChannel {
  agentPath?: string;
  name: string;
  kind: string;
  role: string;
  purpose?: string;
  status: OapInstallChannelStatus;
  setupToken?: string;
  questions?: OapQuestion[];
  seeded?: OapInstallChannelSeed[];
  notSeeded?: OapInstallChannelNote[];
  reason?: string;
  remedy?: string;
}

// OapInstallQuestionsError is thrown when POST /agents/oap-install 400s with
// pkg/web/admind.oapInstallMissingQuestionsResponse — one or more required
// questions have no answer. `questions` carries their full typed schema (never
// an answer value, even for a type=secret question) so the install form can
// render exactly the missing fields without a second round trip; `skillClones`
// carries the external repos the install would clone, so the form can show that
// consent notice alongside the missing fields (before the operator re-submits).
// `warnings` carries the capacity-check hook's own notices (e.g. "lowering
// memory from 4Gi to 768Mi to fit largest node…") — the one place they name
// the SPECIFIC value about to be pre-filled, so dropping them here would lose
// the most actionable text this response carries.
export class OapInstallQuestionsError extends ApiError {
  constructor(
    message: string,
    public questions: OapQuestion[],
    public skillClones: SkillClone[] = [],
    public warnings: string[] = [],
    public channels: OapInstallChannel[] = [],
    public conflicts: OapInstallConflict[] = [],
  ) {
    super(message, 400);
  }
}

// OapInstallConflictsError is thrown when POST /agents/oap-install 409s: the
// cluster holds objects this install would overwrite. The form renders one
// tickable row per conflict and re-submits with `adopt` naming the approved
// ones — that re-submit is the approval; the server never prompts.
export class OapInstallConflictsError extends ApiError {
  constructor(
    message: string,
    public conflicts: OapInstallConflict[],
    public skillClones: SkillClone[] = [],
    public channels: OapInstallChannel[] = [],
    public warnings: string[] = [],
    public questions: OapQuestion[] = [],
  ) {
    super(message, 409);
  }
}

// postOapInstall POSTs to /agents/oap-install and classifies the response: 200
// resolves with the install result; a 400 carrying a non-empty `questions`
// list throws OapInstallQuestionsError (the form re-renders around it); a 409
// carrying a non-empty `conflicts` list throws OapInstallConflictsError (the
// form renders the adopt confirm step); every other non-2xx throws a plain
// ApiError. Kept separate from parse<T> because these bodies have shapes
// parse<T> cannot special-case.
async function postOapInstall(apiBase: string, init: RequestInit): Promise<OapInstallResult> {
  const res = await fetch(`${apiBase}/agents/oap-install`, init);
  if (res.ok) return (await res.json()) as OapInstallResult;

  let body: {
    error?: string;
    questions?: OapQuestion[];
    conflicts?: OapInstallConflict[];
    skillClones?: SkillClone[];
    warnings?: string[];
    channels?: OapInstallChannel[];
  } = {};
  try {
    body = (await res.json()) as typeof body;
  } catch {
    // non-JSON error body — fall through to the generic status message
  }
  if (res.status === 400 && ((body.questions?.length ?? 0) > 0 || (body.channels?.length ?? 0) > 0 || (body.conflicts?.length ?? 0) > 0)) {
    throw new OapInstallQuestionsError(
      body.error || "missing required question(s)",
      body.questions ?? [],
      body.skillClones ?? [],
      body.warnings ?? [],
      body.channels ?? [],
      body.conflicts ?? [],
    );
  }
  if (res.status === 409 && body.conflicts && body.conflicts.length > 0) {
    throw new OapInstallConflictsError(
      body.error || "install would overwrite pre-existing object(s)",
      body.conflicts,
      body.skillClones ?? [],
      body.channels ?? [],
      body.warnings ?? [],
      body.questions ?? [],
    );
  }
  throw new ApiError(body.error || `request failed (${res.status})`, res.status);
}

export interface OapChannelSetupResult {
  name: string;
  namespace: string;
  kind: string;
  agentClass: string;
  secretName?: string;
  alreadyWired?: boolean;
  staged?: boolean;
  nextSteps?: string[];
  warnings?: string[];
}

export interface OapChannelHandoffResult {
  setupToken: string;
  nextAction?: "setup";
  explain?: string;
  url?: string;
  formFields?: Record<string, string>;
}

export class OapChannelQuestionsError extends ApiError {
  constructor(message: string, public questions: OapQuestion[]) {
    super(message, 400);
  }
}

async function postChannelAction<T>(url: string, setupToken: string, answers: Record<string, string>): Promise<T> {
  const res = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ setupToken, answers }),
  });
  if (res.ok) return (await res.json()) as T;
  let body: { error?: string; questions?: OapQuestion[] } = {};
  try {
    body = (await res.json()) as typeof body;
  } catch {
    // fall through to the status message
  }
  if (res.status === 400 && (body.questions?.length ?? 0) > 0) {
    throw new OapChannelQuestionsError(body.error || "missing required channel answer(s)", body.questions ?? []);
  }
  throw new ApiError(body.error || `request failed (${res.status})`, res.status);
}

export const oapChannelSetup = (
  apiBase: string, setupToken: string, answers: Record<string, string>,
): Promise<OapChannelSetupResult> => postChannelAction(`${apiBase}/agents/channel-setup`, setupToken, answers);

export const oapChannelHandoff = (
  apiBase: string, setupToken: string, answers: Record<string, string>,
): Promise<OapChannelHandoffResult> => postChannelAction(`${apiBase}/agents/channel-handoff`, setupToken, answers);

// oapInstallByRef installs a bundle pulled from a registry ref (JSON body).
// adopt, when given, names the "Kind/Name" conflicts the operator has ticked
// on a re-submit after an OapInstallConflictsError.
export const oapInstallByRef = (
  apiBase: string,
  req: {
    ref: string;
    plainHttp?: boolean;
    namespace: string;
    name?: string;
    values?: Record<string, string>;
    adopt?: string[];
  },
): Promise<OapInstallResult> =>
  postOapInstall(apiBase, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });

// oapInstallByFile installs an uploaded .oap file (multipart/form-data — the
// endpoint dispatches on Content-Type). values, when given, is JSON-encoded
// into the "values" form field per the backend's parseOapInstallRequest; adopt
// is likewise JSON-encoded into an "adopt" form field (see oapInstallByRef).
export const oapInstallByFile = (
  apiBase: string,
  file: File,
  fields: { namespace: string; name?: string; values?: Record<string, string>; adopt?: string[] },
): Promise<OapInstallResult> => {
  const fd = new FormData();
  fd.append("file", file);
  fd.append("namespace", fields.namespace);
  if (fields.name) fd.append("name", fields.name);
  if (fields.values) fd.append("values", JSON.stringify(fields.values));
  if (fields.adopt?.length) fd.append("adopt", JSON.stringify(fields.adopt));
  return postOapInstall(apiBase, { method: "POST", body: fd });
};

// postWorkshopInstall POSTs to a workshop's own install route and classifies
// the response exactly like postOapInstall does: handleWorkshopInstall
// (pkg/web/admind/workshops.go) answers with the SAME oapInstallResponse /
// oapInstallMissingQuestionsResponse / oapInstallConflictsResponse shapes as
// /agents/oap-install, so the Workshops page reuses OapInstallQuestionsError /
// OapInstallConflictsError to drive the identical missing-questions /
// adopt-conflicts re-submit flow. Kept as its own function (not a call
// through postOapInstall) only because the URL is per-workshop rather than
// the fixed /agents/oap-install endpoint — the classification logic is
// deliberately identical.
async function postWorkshopInstall(url: string, body: unknown): Promise<OapInstallResult> {
  const res = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (res.ok) return (await res.json()) as OapInstallResult;

  // skillClones/warnings are carried here even though handleWorkshopInstall
  // does not populate them today (its 400/409 responses omit both) — the
  // shape is IDENTICAL to postOapInstall's, and a future change there (e.g.
  // wiring the same skill-clone consent notice) must not be silently
  // swallowed for having been typed narrower here.
  let respBody: {
    error?: string;
    questions?: OapQuestion[];
    conflicts?: OapInstallConflict[];
    skillClones?: SkillClone[];
    warnings?: string[];
  } = {};
  try {
    respBody = (await res.json()) as typeof respBody;
  } catch {
    // non-JSON error body — fall through to the generic status message
  }
  if (res.status === 400 && respBody.questions && respBody.questions.length > 0) {
    throw new OapInstallQuestionsError(
      respBody.error || "missing required question(s)",
      respBody.questions,
      respBody.skillClones ?? [],
      respBody.warnings ?? [],
    );
  }
  if (res.status === 409 && respBody.conflicts && respBody.conflicts.length > 0) {
    throw new OapInstallConflictsError(
      respBody.error || "install would overwrite pre-existing object(s)",
      respBody.conflicts,
      respBody.skillClones ?? [],
    );
  }
  throw new ApiError(respBody.error || `request failed (${res.status})`, res.status);
}

// installWorkshop installs the workshop at (ns, name)'s drafted bundle under
// the admin-chosen target/answers in `req` — POST
// /admin/api/workshops/{ns}/{name}/install (handleWorkshopInstall). adopt
// names the "Kind/Name" conflicts the admin has approved seizing, mirroring
// oapInstallByRef's own adopt re-submit.
export const installWorkshop = (
  apiBase: string,
  ns: string,
  name: string,
  req: { namespace: string; name: string; values: Record<string, string>; adopt: string[] },
): Promise<OapInstallResult> => postWorkshopInstall(`${apiBase}/workshops/${ns}/${name}/install`, req);

async function parse<T>(res: Response): Promise<T> {
  if (!res.ok) {
    let msg = `request failed (${res.status})`;
    try {
      const body = (await res.json()) as { error?: string };
      if (body.error) msg = body.error;
    } catch {
      // non-JSON error body — keep the status message
    }
    throw new ApiError(msg, res.status);
  }
  return (await res.json()) as T;
}

export const getJSON = async <T>(url: string): Promise<T> => parse<T>(await fetch(url));

export const getOverview = (apiBase: string): Promise<OverviewData> =>
  getJSON<OverviewData>(`${apiBase}/overview`);

export const getHealth = (apiBase: string): Promise<HealthSnapshot> =>
  getJSON<HealthSnapshot>(`${apiBase}/health`);

// getCluster fetches the best-effort cluster identity (type + optional
// name/consoleURL) for the header badge.
export const getCluster = (apiBase: string): Promise<ClusterInfo> =>
  getJSON<ClusterInfo>(`${apiBase}/cluster`);

// getBudget fetches the four-axis token/estimated-cost breakdown (byModel,
// byAgentClass, bySession, byUser).
export const getBudget = (apiBase: string): Promise<BudgetBreakdown> =>
  getJSON<BudgetBreakdown>(`${apiBase}/budget`);

export const getToolCalls = (apiBase: string): Promise<ToolCall[]> =>
  getJSON<ToolCall[]>(`${apiBase}/toolcalls`);

// getSessionLogs fetches a session's full decoded transcript (B4 endpoint):
// the append-only turn Kind, chronological, capped with a truncated flag.
export const getSessionLogs = (apiBase: string, ns: string, name: string): Promise<SessionLogsResponse> =>
  getJSON<SessionLogsResponse>(`${apiBase}/sessions/${ns}/${name}/logs`);

export const getApprovals = (apiBase: string): Promise<Approval[]> =>
  getJSON<Approval[]>(`${apiBase}/approvals`);

// getConfigResource fetches one generic config projector table by URL slug
// (agents/tools/skills/sources/channels/identities/providers/users). Some
// admind projectors build their row slice with a bare `var rows []T` and
// never append when the CRD list is empty, which marshals to JSON `null`
// rather than `[]` — normalize that here so every ResourceSection consumer
// gets an array to iterate, never null.
export const getConfigResource = async (apiBase: string, resource: string): Promise<ResourceRow[]> =>
  (await getJSON<ResourceRow[]>(`${apiBase}/config/${resource}`)) ?? [];

// getConfigDetail fetches one resource's rich detail (Overview scalars + tabbed
// Sections) by URL slug + id. id is "<ns>/<name>" for namespaced resources and
// a bare "<name>" for cluster-scoped ones — appended verbatim onto the
// {resource}/{id...} detail route (ns/name segments are DNS-safe, no encoding).
export const getConfigDetail = (apiBase: string, resource: string, id: string): Promise<ResourceDetail> =>
  getJSON<ResourceDetail>(`${apiBase}/config/${resource}/${id}`);

export const getAccess = (apiBase: string): Promise<AccessData> =>
  getJSON<AccessData>(`${apiBase}/access`);

// getArtifacts fetches the cross-session ArtifactRender list (newest-first).
export const getArtifacts = (apiBase: string): Promise<ArtifactRow[]> =>
  getJSON<ArtifactRow[]>(`${apiBase}/artifacts`);

// getArtifactDetail fetches one ArtifactRender's metadata + its revisions
// (the B5 detail endpoint).
export const getArtifactDetail = (apiBase: string, ns: string, name: string): Promise<ArtifactDetail> =>
  getJSON<ArtifactDetail>(`${apiBase}/artifacts/${ns}/${name}`);

// getWorkshops fetches every Workshop cluster-wide (newest-first) for the
// admin Workshops page.
export const getWorkshops = (apiBase: string): Promise<WorkshopRow[]> =>
  getJSON<WorkshopRow[]>(`${apiBase}/workshops`);

// getTokens fetches every AccessToken in the configured namespace for the
// admin Tokens page.
export const getTokens = (apiBase: string): Promise<AccessTokenRow[]> =>
  getJSON<AccessTokensData>(`${apiBase}/tokens`).then((d) => d.tokens ?? []);

// revokeToken deletes the named AccessToken CR — POST /admin/api/tokens/revoke
// (handleTokensRevoke). The CR's finalizer removes its SpiceDB tuples.
export const revokeToken = (apiBase: string, name: string): Promise<AccessTokenRevokeResponse> =>
  postJSON<AccessTokenRevokeResponse>(`${apiBase}/tokens/revoke`, { name });

// getMemory fetches the Memory browser payload: per-Kind rollups, plus an entry
// list when `kind` (recent entries) or `q` (ranked search) is given.
export const getMemory = (
  apiBase: string,
  opts: { kind?: string; q?: string } = {},
): Promise<MemoryData> => {
  const params = new URLSearchParams();
  if (opts.q) params.set("q", opts.q);
  if (opts.kind) params.set("kind", opts.kind);
  const qs = params.toString();
  return getJSON<MemoryData>(`${apiBase}/memory${qs ? `?${qs}` : ""}`);
};

export const getConfigSettings = (apiBase: string): Promise<SettingsData> =>
  getJSON<SettingsData>(`${apiBase}/config/settings`);

// kgURL builds a /kg/{action} request URL with optional query params.
const kgURL = (apiBase: string, action: string, params: Record<string, string> = {}): string => {
  const qs = new URLSearchParams(params).toString();
  return `${apiBase}/kg/${action}${qs ? `?${qs}` : ""}`;
};

// kg* fetch the read-only Knowledge graph proxy. Each resolves with an envelope
// whose `available` flag drives the degraded ("not configured") panel.
export const kgSearch = (apiBase: string, q: string): Promise<KGFactsResponse> =>
  getJSON<KGFactsResponse>(kgURL(apiBase, "search", q ? { q } : {}));

export const kgEntity = (apiBase: string, uuid: string): Promise<KGEntityResponse> =>
  getJSON<KGEntityResponse>(kgURL(apiBase, "entity", { uuid }));

export const kgFacts = (apiBase: string, uuid: string): Promise<KGFactsResponse> =>
  getJSON<KGFactsResponse>(kgURL(apiBase, "facts", { uuid }));

export const kgRelated = (apiBase: string, uuid: string): Promise<KGEntitiesResponse> =>
  getJSON<KGEntitiesResponse>(kgURL(apiBase, "related", { uuid }));

export const kgCommunities = (apiBase: string): Promise<KGCommunitiesResponse> =>
  getJSON<KGCommunitiesResponse>(kgURL(apiBase, "communities"));

export const postJSON = async <T>(url: string, body: unknown): Promise<T> =>
  parse<T>(
    await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }),
  );

export const del = async (url: string): Promise<void> => {
  const res = await fetch(url, { method: "DELETE" });
  if (!res.ok && res.status !== 404) {
    await parse(res); // throws with the server's error message
  }
};

export type SSEHandler = {
  onSession: (s: SessionState) => void;
  onRemove: (ref: { namespace: string; name: string }) => void;
  onStale: (stale: boolean) => void;
};

// openSessionStream subscribes to the live SSE feed. Staleness: if no
// frame (heartbeats included) arrives for 3 heartbeat intervals the UI
// shows a stale indicator rather than silently freezing.
export function openSessionStream(apiBase: string, h: SSEHandler): () => void {
  const es = new EventSource(`${apiBase}/sessions/stream`);
  let staleTimer: ReturnType<typeof setTimeout> | undefined;
  const armStale = () => {
    if (staleTimer) clearTimeout(staleTimer);
    h.onStale(false);
    staleTimer = setTimeout(() => h.onStale(true), 15000);
  };
  es.addEventListener("session", (ev) => {
    armStale();
    h.onSession(JSON.parse((ev as MessageEvent).data) as SessionState);
  });
  es.addEventListener("remove", (ev) => {
    armStale();
    h.onRemove(JSON.parse((ev as MessageEvent).data) as { namespace: string; name: string });
  });
  es.addEventListener("heartbeat", armStale);
  es.onerror = () => h.onStale(true);
  armStale();
  return () => {
    if (staleTimer) clearTimeout(staleTimer);
    es.close();
  };
}

export const sessionKey = (s: { namespace: string; name: string }) => `${s.namespace}/${s.name}`;
