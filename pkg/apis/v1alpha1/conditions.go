package v1alpha1

// SpiceboxClass condition types.
const (
	SpiceboxClassConditionValid = "Valid"
)

// SpiceboxClass reasons.
const (
	ReasonClassValid                 = "Valid"
	ReasonClassInvalidImage          = "InvalidImage"
	ReasonClassInvalidTool           = "InvalidTool"
	ReasonClassInvalidResource       = "InvalidResource"
	ReasonClassInvalidEnvDefaults    = "InvalidEnvDefaults"
	ReasonClassInvalidPrivateVolumes = "InvalidPrivateVolumes"
	ReasonToolspecCoverageMissing    = "ToolspecCoverageMissing"
	// ReasonClassInvalidSandbox marks a class whose spec.sandbox names an
	// unregistered backend, or a backend that cannot satisfy what the class asks.
	ReasonClassInvalidSandbox = "InvalidSandbox"
	// ReasonClassInvalidSandboxWarmPool marks a class that requests non-zero
	// spec.sandbox.warmPool.replicas on a backend whose Runtime does not
	// implement sandboxkinds.Prewarmer. Distinct from ReasonClassInvalidSandbox
	// so an operator can tell "the backend itself is wrong" apart from "the
	// backend is fine, but it cannot pre-warm."
	ReasonClassInvalidSandboxWarmPool = "InvalidSandboxWarmPool"
	// ReasonSandboxKindNotPermitted marks a resource whose resolved sandbox
	// backend exists but is excluded by an allowedSandboxKinds ceiling. Distinct
	// from ReasonClassInvalidSandbox, which means the named backend is not
	// registered at all â the two need different operator responses.
	ReasonSandboxKindNotPermitted = "SandboxKindNotPermitted"
	// ReasonSandboxConfigIgnored marks a resource where a sandbox config value
	// was not a JSON object and was skipped. Non-fatal: the resolved backend is
	// still usable. Distinct from ReasonSandboxKindNotPermitted, which means the
	// backend itself is forbidden here.
	ReasonSandboxConfigIgnored = "SandboxConfigIgnored"
)

// SpiceboxToolkit reasons.
const (
	ReasonBuiltinCollision = "BuiltinCollision"
)

// SpiceboxToolspec reasons.
const (
	ReasonToolkitMissing    = "ToolkitMissing"
	ReasonCELCompileError   = "CELCompileError"
	ReasonUnknownSubcommand = "UnknownSubcommand"
	ReasonSpecLoadFailed    = "SpecLoadFailed"
)

// SpiceboxSession condition types.
const (
	SpiceboxSessionConditionReady       = "Ready"
	SpiceboxSessionConditionProgressing = "Progressing"
	SpiceboxSessionConditionFailed      = "Failed"
	SpiceboxSessionConditionTerminated  = "Terminated"
)

// SpiceboxSession reasons.
const (
	ReasonPodReady     = "PodReady"
	ReasonPodNotReady  = "PodNotReady"
	ReasonPodMissing   = "PodMissing"
	ReasonCreating     = "Creating"
	ReasonDraining     = "Draining"
	ReasonPodOOMKilled = "PodOOMKilled"
	ReasonPodCrashed   = "PodCrashed"
	// ReasonPodStartFailed marks a bundle pod wedged in a terminal container
	// "Waiting" state it will not recover from on its own â an unpullable image
	// (ImagePullBackOff), an unsatisfiable config (CreateContainerConfigError), a
	// crash loop, etc. The specific kubelet reason is carried in the condition
	// message. Distinct from ReasonPodCrashed (PodFailed phase), since these pods
	// never reach PodFailed â they retry forever â so without this the bundle (and
	// its waiting AgentSession) would hang silently.
	ReasonPodStartFailed = "PodStartFailed"
	// ReasonWorkspaceProvisioningFailed marks a session whose workspace (or
	// snapshot-store) PVC could not be provisioned by its StorageClass — an
	// undersized request the backend rejects (Filestore's minimum share size),
	// a disabled cloud API, quota, or a missing class. The provider's actual
	// event message is carried in the condition message. Distinct from
	// BundleFailed (a pod that scheduled but never became Ready): here no pod can
	// ever schedule because its volume never binds, so we fail fast with the
	// storage reason instead of waiting out bundleReadyDeadline behind the
	// generic "did not become Ready" text.
	ReasonWorkspaceProvisioningFailed = "WorkspaceProvisioningFailed"
	ReasonNodeEvicted                 = "NodeEvicted"
	ReasonClassMissing                = "ClassMissing"
	ReasonClassInvalid                = "ClassInvalid"
	ReasonIdleTTL                     = "IdleTTL"
	ReasonMaxDuration                 = "MaxDuration"
	ReasonUserDelete                  = "UserDelete"
	ReasonToolspecNotInClass          = "ToolspecNotInClass"
)

// ToolCall condition types.
const (
	ToolCallConditionValidated = "Validated"
	ToolCallConditionRunning   = "Running"
	ToolCallConditionSucceeded = "Succeeded"
	ToolCallConditionFailed    = "Failed"
	ToolCallConditionTimeout   = "Timeout"
	ToolCallConditionCanceled  = "Canceled"
	// ToolCallConditionSnapshotAuditRecorded indicates the workspace
	// snapshot audit entry (tool_dispatch_snapshot memory kind) has
	// been recorded for this ToolCall. Used by the toolcall reconciler
	// to skip redundant Record calls on re-entry for streaming ToolCalls.
	ToolCallConditionSnapshotAuditRecorded = "SnapshotAuditRecorded"
	// ToolCallConditionReconcileRetrying is True while the reconciler is
	// failing on this ToolCall and being requeued. Its message carries the
	// last error and its LastTransitionTime marks when the current streak
	// began — which is what bounds how long the call may keep failing before
	// it is finished outright (ReasonToolCallReconcileFailed). It exists so a
	// stuck call is legible on the object from the FIRST failure, rather than
	// only in an operator log nobody is watching while a person waits.
	ToolCallConditionReconcileRetrying = "ReconcileRetrying"
)

// ToolCall reasons.
const (
	ReasonToolCallValid             = "Valid"
	ReasonSessionNotActive          = "SessionNotActive"
	ReasonToolUnknown               = "ToolUnknown"
	ReasonInputArtifactMissing      = "InputArtifactMissing"
	ReasonExecStarted               = "ExecStarted"
	ReasonExecClosed                = "ExecClosed"
	ReasonProcessExited             = "ProcessExited"
	ReasonNonZeroExit               = "NonZeroExit"
	ReasonSessionGone               = "SessionGone"
	ReasonOperatorRestart           = "OperatorRestart"
	ReasonHydrateFailed             = "HydrateFailed"
	ReasonDeadlineExceeded          = "DeadlineExceeded"
	ReasonSpiceboxSessionTerminated = "SpiceboxSessionTerminated"
	ReasonToolspecDenied            = "ToolspecDenied"
	ReasonStreamTokenHashMissing    = "StreamTokenHashMissing"
	// ReasonSessionBindingMismatch: the ToolCall's spec.session names a bundle
	// belonging to a different AgentSession than the one that owns the call.
	// Both fields are creator-written, and everything privileged downstream
	// keys on spec.session — the sandbox to exec into and the session whose
	// use_token grant is consulted — so they must agree.
	ReasonSessionBindingMismatch = "SessionBindingMismatch"
	// ReasonSandboxUnresolved: the session has no sandbox handle yet, or its
	// kind has no runtime linked into this binary. Never a fallback to a
	// default backend â running in the wrong sandbox is worse than refusing.
	ReasonSandboxUnresolved = "SandboxUnresolved"
	// Interactive-mode terminal reasons. ReasonToolCallMaxDuration is named
	// to avoid colliding with SpiceboxSession's ReasonMaxDuration above; both
	// share the same value string but Go const names must be unique.
	ReasonIdleExit            = "IdleExit"
	ReasonToolCallMaxDuration = "ToolCallMaxDuration"
	// Reconcile-failure surfacing. ReasonToolCallReconcileError marks the
	// ReconcileRetrying condition while attempts continue;
	// ReasonToolCallReconcileRecovered clears it after one succeeds; and
	// ReasonToolCallReconcileFailed is the TERMINAL Failed reason for a call
	// that kept failing past the budget, carrying the last error so the
	// runner can hand the agent — and the agent the user — a reason.
	ReasonToolCallReconcileError     = "ReconcileError"
	ReasonToolCallReconcileRecovered = "ReconcileRecovered"
	ReasonToolCallReconcileFailed    = "ReconcileFailed"
)

// AgentIdentity reasons.
const (
	ReasonAllReferencesResolve    = "AllReferencesResolve"
	ReasonSpecInvalid             = "SpecInvalid"
	ReasonSecretMissing           = "SecretMissing"
	ReasonSecretKeyMissing        = "SecretKeyMissing"
	ReasonOAuthSecretIncomplete   = "OAuthSecretIncomplete"
	ReasonBindingPrefixMixed      = "BindingPrefixMixed"
	ReasonBindingModeMismatch     = "BindingModeMismatch"
	ReasonBindingMatchUnparseable = "BindingMatchUnparseable"
	ReasonAgentCredentialExpired  = "AgentCredentialExpired"
	ReasonCredentialEmpty         = "CredentialEmpty"

	// ReasonCredentialShapeMismatch: the stored value does not match the token
	// format its provider declares (provider.TokenShape). It is a Valid=False
	// reason rather than a warning because the alternative is what it replaces:
	// every readiness gate reporting healthy and the credential failing as an
	// opaque 401 inside a tool's own output, minutes later. The message names
	// both the expected shape and the one the value actually has.
	ReasonCredentialShapeMismatch = "CredentialShapeMismatch"

	// PlatformLinked reasons. ReasonPlatformLinkFailed is TRANSIENT (SpiceDB
	// unreachable, schema not composed yet) and keeps being retried. The two
	// Unrepresentable* reasons are PERMANENT: a Kubernetes name may contain
	// '.', a SpiceDB object id may not, so the link can never be written and
	// the reconciler stops retrying and says so. For a class that means it can
	// never appear in the browser's agent picker.
	ReasonPlatformLinked              = "PlatformLinked"
	ReasonPlatformLinkFailed          = "PlatformLinkFailed"
	ReasonUnrepresentableIdentityName = "UnrepresentableIdentityName"
	ReasonUnrepresentableClassName    = "UnrepresentableClassName"

	// AgentIdentity Refresh-condition reasons, set by the OAuth refresh controller.
	ReasonNoOAuthCredentials     = "NoOAuthCredentials"
	ReasonAllOAuthFresh          = "AllOAuthFresh"
	ReasonRefreshSucceeded       = "RefreshSucceeded"
	ReasonNoRefreshToken         = "NoRefreshToken"
	ReasonTokenEndpointError     = "TokenEndpointError"
	ReasonRefreshResponseInvalid = "RefreshResponseInvalid"
)

// ToolCall agent-related condition types and reasons.
const (
	ToolCallConditionEnvHighEntropy = "EnvHighEntropy"

	ReasonToolCallEnvSecretLike     = "ToolCallEnvSecretLike"
	ReasonToolCallEnvShadowsAgent   = "ToolCallEnvShadowsAgent"
	ReasonCredentialSourceForbidden = "CredentialSourceForbidden"
	ReasonAgentNotFound             = "AgentNotFound"
	ReasonAgentBindingMissing       = "AgentBindingMissing"
	ReasonAgentCredentialUnresolved = "AgentCredentialUnresolved"
	ReasonAgentSecretMissing        = "AgentSecretMissing"
	ReasonAgentSecretKeyMissing     = "AgentSecretKeyMissing"

	// ReasonAgentCredentialRevoked is set on the ToolCall Failed condition
	// when the sandbox-exec pre-dispatch use_token check gets a definitive
	// deny from SpiceDB (CheckUseToken returns false, nil) â the credential
	// resolved fine (ReasonAgentCredentialUnresolved does not apply) but the
	// operator's authorized_token grant for it no longer matches (revoked,
	// rotated, or never granted). Distinct from
	// ReasonAgentSessionTokenAuthzUnavailable, which is the indeterminate
	// case (the check itself couldn't be confirmed) and fails the parent
	// AgentSession, not just this ToolCall. A definitive deny is surgical:
	// only this ToolCall fails; the session continues.
	ReasonAgentCredentialRevoked = "AgentCredentialRevoked"
)

// Finalizers.
const (
	FinalizerSpiceboxSession  = "spiceboxsession.agentprimitives.authzed.com/finalizer"
	FinalizerToolCall         = "toolcall.agentprimitives.authzed.com/finalizer"
	FinalizerAgentSession     = "agentsession.agentprimitives.authzed.com/finalizer"
	FinalizerArtifactRender   = "artifactrender.agentprimitives.authzed.com/finalizer"
	FinalizerSpiceDBBootstrap = "spicedbbootstrap.agentprimitives.authzed.com/finalizer"
	FinalizerSidecarToolbox   = "sidecartoolbox.agentprimitives.authzed.com/finalizer"
	FinalizerPublicEndpoint   = "publicendpoint.agentprimitives.authzed.com/finalizer"
	FinalizerUserIdentity     = "useridentity.agentprimitives.authzed.com/finalizer"
	FinalizerWorkshop         = "workshop.agentprimitives.authzed.com/finalizer"
	// FinalizerSubagentRequest is added ONLY to an `attended`-mode
	// SubagentRequest, giving the controller a chance to notify a live
	// attended child's watching parent (a fixed inbound line + a forced wake)
	// before an early delete (a "stop") cascades the child away — the
	// GC-cascade would otherwise remove both objects with nothing left to
	// read. Every other mode never carries it.
	FinalizerSubagentRequest = "subagentrequest.agentprimitives.authzed.com/finalizer"
)

// AgentClass condition types.
const (
	AgentClassConditionValid = "Valid"
	// AgentClassConditionPlatformLinked is True once the controller has written
	// agentclass:<ns>/<name>#platform@platform:platform â the tuple that makes
	// agentclass#start_session satisfiable, and so the only reason this class
	// can appear in the browser's agent picker at all. A condition rather than
	// a silent write because its absence is otherwise invisible: the schema
	// compiles, the dashboard renders, and the picker is simply empty. Reasons
	// are shared with AgentIdentity's platform link, plus
	// ReasonUnrepresentableClassName for the permanent case.
	AgentClassConditionPlatformLinked = "PlatformLinked"
	// AgentClassConditionStartersLinked is True once agentclass#starter holds
	// exactly spec.authz.session.allowedStarters (reason StartersLinked), or
	// when the class declares none and the relation was cleared (reason
	// NoAllowedStarters). False with StartersLinkFailed on a transient write
	// error (requeued), or ReasonUnrepresentableClassName (permanent).
	AgentClassConditionStartersLinked = "StartersLinked"
	// AgentClassConditionSettingsAccepted is True when the 4-tier settings
	// chain resolves without any fatal violation for this class.
	AgentClassConditionSettingsAccepted = "SettingsAccepted"
	// AgentClassConditionSchemaValidated is True when the operator has verified
	// every referenced tool's permission_check against the live SpiceDB schema.
	// Lazy: only re-runs on an AgentClass or referenced-tool Generation bump.
	AgentClassConditionSchemaValidated = "SchemaValidated"
	// AgentClassConditionAgentSessionGrantsWritten is True when the controller
	// has reconciled the owned "<className>-grants" AgentSessionGrants CR from
	// the union of resolved tool Permission.Check pairs. False with reason
	// AgentSessionGrantsWriteFailed when the write errored â surfaced without
	// failing the overall reconcile, so an API-server hiccup does not flap Valid.
	AgentClassConditionAgentSessionGrantsWritten = "AgentSessionGrantsWritten"
	// AgentClassConditionToolAuthDisabled is True when spec.authz.toolCalls.mode is
	// "disabled" â the operator-visible signal that per-tool authz is bypassed
	// for every session of this class. Reason is always ReasonToolAuthBypassed;
	// absent or False on enforcing/permissive classes.
	AgentClassConditionToolAuthDisabled = "ToolAuthDisabled"
	// AgentClassConditionInformationLeakageReady is True when every bound
	// Channel's kind satisfies the capability level required by
	// spec.authz.informationLeakage.mode. False (with a specific reason)
	// when mode=enforcing and a bound channel's kind either has no
	// AudienceResolver or is SingleUser with singleUserBypass=false.
	// Always True when mode=disabled or mode=logging.
	AgentClassConditionInformationLeakageReady = "InformationLeakageReady"
	// AgentClassConditionCapabilitiesValid is True when every key in
	// spec.capabilities names a registered capability and its config parses
	// (both the common {enabled} envelope and the capability-specific
	// ParseConfig). False (with a message) does NOT block readiness â an
	// unrecognized or malformed grant is dropped gracefully at assembly
	// time (see pkg/agent/tool/meta/capability), so this condition is purely
	// diagnostic. Absent capabilities map â vacuously True.
	AgentClassConditionCapabilitiesValid = "CapabilitiesValid"
	// AgentClassConditionSubagentsCapabilityGranted is True when
	// spec.subagents is empty (nothing to delegate to, so the question does
	// not apply) OR spec.capabilities carries a "subagents" entry. False when
	// spec.subagents names one or more classes but spec.capabilities has no
	// "subagents" key at all: the roster then has nowhere to send a delegate
	// call, since the subagents capability (DefaultOn() == false) is what
	// offers the delegate/reply_to_subagent tools in the first place — a
	// class in this state is fully valid and reconciles cleanly, but can
	// never actually hand work to anyone on its own roster. Purely
	// diagnostic, same as CapabilitiesValid: does NOT block readiness, since
	// nothing about this is a spec error, only an easy-to-miss omission.
	AgentClassConditionSubagentsCapabilityGranted = "SubagentsCapabilityGranted"
)

// AgentClass reasons.
const (
	ReasonConfigMapMissing            = "ConfigMapMissing"
	ReasonConfigMapKeyMissing         = "ConfigMapKeyMissing"
	ReasonToolspecMissing             = "ToolspecMissing"
	ReasonAgentIdentityMissing        = "AgentIdentityMissing"
	ReasonAgentIdentityBindingMissing = "AgentIdentityBindingMissing"
	// ReasonAgentIdentityInvalid fires when the AgentClass's bound
	// AgentIdentity exists but is itself not Valid=True (e.g. an empty-
	// value Secret backing one of its credentials). Propagating the
	// identity's invalidity into the class completes the start-gate
	// chain: the AgentSession reconciler parks any session whose
	// AgentClass is not Valid (ReasonAgentClassNotValid).
	ReasonAgentIdentityInvalid    = "AgentIdentityInvalid"
	ReasonBundleToolNameCollision = "BundleToolNameCollision"
	ReasonTestProviderNotAllowed  = "TestProviderNotAllowed"

	// StartersLinked reasons. ReasonStartersLinked is the AgentClassConditionStartersLinked
	// True reason when a non-empty allowedStarters was written; ReasonNoAllowedStarters
	// is the True reason when the class declares none and the relation was cleared.
	// ReasonStartersLinkFailed is the transient False reason (requeued); the permanent
	// False reason reuses ReasonUnrepresentableClassName, shared with PlatformLinked.
	ReasonStartersLinked     = "StartersLinked"
	ReasonNoAllowedStarters  = "NoAllowedStarters"
	ReasonStartersLinkFailed = "StartersLinkFailed"
)

// AgentClass reasons for per-tool permission validation.
const (
	ReasonToolPermissionMissing = "ToolPermissionMissing"
	// ReasonPlanGateRequiresToolCallEnforcing: planGate.mode=enforcing with
	// toolCalls.mode not enforcing. The plan gate enforces the CLASS axis
	// itself, but the INSTANCE axis rides on tool_call_authz â so with that
	// hook permissive or unregistered, per-resource gating would silently stop
	// while the class axis kept refusing, which reads as "the gate is working".
	ReasonPlanGateRequiresToolCallEnforcing = "PlanGateRequiresToolCallEnforcing"
	// ReasonPlanExampleInvalid: an authored planGate.examples entry names a
	// handle or slot type this class cannot declare. Refused rather than
	// dropped: the prompt tells agents an undeclarable handle is silently
	// removed from their phase, and planners transcribe these examples closely,
	// so admitting one would teach that failure into every plan the agent
	// writes — surfacing much later as calls denied for no visible reason.
	ReasonPlanExampleInvalid = "PlanExampleInvalid"
	// ReasonTrifectaCeilingContradicted: the class declares
	// authz.trifecta.neverConsequential yet holds a readwrite or external tool
	// permission. A declared ceiling checked against the derived surface, so a
	// contradiction is surfaced at apply time rather than as an untraceable
	// refusal during a later delegation.
	ReasonTrifectaCeilingContradicted = "TrifectaCeilingContradicted"
	// ReasonBoundEntitiesRenamed: spec.boundEntities still set after the
	// rename. Rejected rather than ignored â silently dropping an authz field
	// would leave the agent less constrained than its author wrote.
	ReasonBoundEntitiesRenamed = "BoundEntitiesRenamed"
	// ReasonSlotDeclarationInvalid: an authz.slots entry is internally
	// inconsistent â an unknown enum value, a duplicate resourceType, or a
	// trust policy declared where it cannot take effect. Rejected rather than
	// ignored: a policy field that never runs is worse than an absent one,
	// because its presence in the YAML reads as protection.
	ReasonSlotDeclarationInvalid = "SlotDeclarationInvalid"
	// ReasonSlotPreconditionUnsatisfiable: a slot precondition reads an
	// observed fact whose every declared producer is a tool gated by that same
	// slot. Nothing can ever satisfy it — the agent is handed a hint naming a
	// call it will be denied, and burns its budget retrying. Refused at
	// admission for the same reason a subagent cycle is: a bound that only
	// holds at runtime does not hold.
	//
	// Its OWN reason rather than ReasonSlotDeclarationInvalid because this is
	// not an internal-consistency claim about the declaration — it is a claim
	// about the declaration against the class's resolved tools, and it can only
	// be judged once those are resolved.
	ReasonSlotPreconditionUnsatisfiable = "SlotPreconditionUnsatisfiable"
	// ReasonSlotPreconditionUnroutable: a slot precondition's waiver has no one
	// to route to. A Refused verdict raises a human waiver card whose approver
	// pool is the precondition's own approvers[] when SET (authoritative), or the
	// slot type's resolved standing when UNSET. Two shapes leave that pool empty
	// by SHAPE: a SET approvers[] that names no subject (a list of blanks, which
	// overrides the standing default with a route to no one), or an UNSET
	// approvers[] whose slot type resolves to no standing at all.
	//
	// Refused at admission, and only for the structurally-empty case: this
	// cannot and does not claim SpiceDB currently holds zero approver subjects
	// for a declared standing (that is a runtime fact, not a shape). Its OWN
	// reason rather than ReasonSlotPreconditionUnsatisfiable because the defect
	// is the absent APPROVER route, not an unsatisfiable predicate, and rather
	// than ReasonSlotDeclarationInvalid because it is a claim about the
	// precondition's own approver routing, judged against the resolved standings.
	ReasonSlotPreconditionUnroutable = "SlotPreconditionUnroutable"
	// ReasonRosterInvalid marks an AgentClass whose delegation roster does not
	// hold together: a spec.subagents graph with a cycle, one that exceeds the
	// depth ceiling, or one naming a class that does not exist — plus a
	// spec.subagentModes entry keyed on a name absent from spec.subagents, or
	// carrying a value that is not a delegation mode.
	ReasonRosterInvalid = "RosterInvalid"
	// ReasonConfigInvalid: spec.config violates spec.configSchema, or a bound
	// toolspec constraint references a config key not declared in configSchema.
	ReasonConfigInvalid = "ConfigInvalid"
	// ReasonUserPreferencesInvalid: spec.userPreferences is self-inconsistent
	// (duplicate key, default violating its own type/enum/pattern, …).
	ReasonUserPreferencesInvalid        = "UserPreferencesInvalid"
	ReasonPermissionSpecInvalid         = "PermissionSpecInvalid"
	ReasonPermissionTemplateUnresolved  = "PermissionTemplateUnresolved"
	ReasonPermissionTransformUnknown    = "PermissionTransformUnknown"
	ReasonPermissionSchemaMismatch      = "PermissionSchemaMismatch"
	ReasonSpiceDBUnreachable            = "SpiceDBUnreachable"
	ReasonAgentSessionGrantsWritten     = "AgentSessionGrantsWritten"
	ReasonAgentSessionGrantsWriteFailed = "AgentSessionGrantsWriteFailed"
	// ReasonToolAuthBypassed is the Reason on AgentClassConditionToolAuthDisabled
	// when status=True. Message includes a fixed operator-readable note that
	// tool calls run without SpiceDB validation.
	ReasonToolAuthBypassed = "ToolAuthBypassed"

	// ReasonInfoLeakageReady is the Reason on
	// AgentClassConditionInformationLeakageReady when status=True.
	ReasonInfoLeakageReady = "CapabilitySatisfied"
	// ReasonChannelKindLacksAudienceResolver is set when a bound Channel's
	// kind has no AudienceResolver and
	// informationLeakage.onUnsupportedChannel=blockBinding.
	ReasonChannelKindLacksAudienceResolver = "ChannelKindLacksAudienceResolver"
	// ReasonSingleUserBypassDisabled is set when a bound Channel's kind
	// reports CapabilitySingleUser but informationLeakage.singleUserBypass
	// is false.
	ReasonSingleUserBypassDisabled = "SingleUserBypassDisabled"

	// ReasonCapabilitiesValid is the Reason on
	// AgentClassConditionCapabilitiesValid when status=True.
	ReasonCapabilitiesValid = "CapabilitiesValid"
	// ReasonUnknownCapability is set when spec.capabilities has a key that
	// names no registered capability.
	ReasonUnknownCapability = "UnknownCapability"
	// ReasonInvalidCapabilityConfig is set when spec.capabilities has a
	// known key whose value fails to parse (the common {enabled} envelope
	// or the capability-specific config).
	ReasonInvalidCapabilityConfig = "InvalidCapabilityConfig"

	// ReasonSubagentsCapabilityGranted is the Reason on
	// AgentClassConditionSubagentsCapabilityGranted when status=True (either
	// no roster, or the capability is present).
	ReasonSubagentsCapabilityGranted = "SubagentsCapabilityGranted"
	// ReasonSubagentsCapabilityMissing is set when spec.subagents is
	// non-empty but spec.capabilities has no "subagents" entry: the roster
	// exists but nothing offers the delegate tool that would use it.
	ReasonSubagentsCapabilityMissing = "SubagentsCapabilityMissing"
)

// AgentSession condition types.
const (
	AgentSessionConditionClassResolved = "ClassResolved"
	AgentSessionConditionBundlesReady  = "BundlesReady"
	AgentSessionConditionRunnerReady   = "RunnerReady"
	AgentSessionConditionSucceeded     = "Succeeded"
	AgentSessionConditionFailed        = "Failed"
	// AgentSessionConditionSettingsAccepted is True when the 4-tier settings
	// chain resolves without any fatal violation for this session.
	AgentSessionConditionSettingsAccepted = "SettingsAccepted"
	// AgentSessionConditionSkillBundlesIntegrity is False when a staged skill
	// bundle's store-returned bytes did not hash to its SkillBundleRef digest â
	// a corrupt or poisoned cache. The skill still runs instruction-only, but
	// the failure is surfaced here so `kubectl describe` distinguishes it from
	// the benign not-cached case, which produces no condition at all. Absent
	// when every bundle the store served verified cleanly.
	AgentSessionConditionSkillBundlesIntegrity = "SkillBundlesIntegrity"
	// AgentSessionConditionSandboxScheduling is the durable, channelsd-visible
	// twin of the one-shot "capacity" monitoring event: False
	// (ReasonSandboxUnschedulable) when a bundle or content-guard detector pod
	// has been stuck Pending on a scheduler failure (insufficient cpu/memory,
	// taints, â¦) past the grace window, with the scheduler's real reason in
	// Message; True (ReasonSandboxScheduled) once nothing is stalled, so a
	// resolved capacity problem is reflected as promptly as the stall was.
	// Absent until the first wait observes a scheduling result.
	//
	// Unlike the monitoring event it is set regardless of whether
	// MonitoringPublish is configured: it is the primary signal channelsd
	// reacts to, and must work on clusters with no NATS wiring.
	AgentSessionConditionSandboxScheduling = "SandboxScheduling"
)

// Settings reasons (shared by AgentClass + AgentSession SettingsAccepted and the
// Settings CR SelfConsistent conditions).
const (
	ReasonSettingsResolved         = "SettingsResolved" // accepted, no warnings
	ReasonSettingsClamped          = "SettingsClamped"  // accepted, budget clamped
	ReasonSettingsSelfInconsistent = "SelfInconsistent" // a Settings default violates its own ceiling
	ReasonSettingsNotSingleton     = "NotSingleton"     // a Settings CR has a non-well-known name
	ReasonSettingsConsistent       = "Consistent"       // Settings CR SelfConsistent=True
	ReasonClassPreferencesValid    = "ClassPreferencesValid"
	ReasonClassPreferencesInvalid  = "ClassPreferencesInvalid"
)

// ClusterAgentSettings / AgentSettings condition types.
const (
	SettingsConditionSelfConsistent = "SelfConsistent"

	// SettingsConditionClassPreferencesValid reports whether every
	// classUserPreferences global matches an installed class's declared
	// userPreferences schema. Unknown class names are tolerated (install
	// order); unknown keys and type mismatches against an INSTALLED class
	// are violations.
	SettingsConditionClassPreferencesValid = "ClassPreferencesValid"
)

// AgentSession phases (status.phase string values).
const (
	AgentSessionPhasePending   = "Pending"
	AgentSessionPhaseRunning   = "Running"
	AgentSessionPhaseSucceeded = "Succeeded"
	AgentSessionPhaseFailed    = "Failed"
	// AgentSessionPhaseAwaitingApproval is the legacy phase name; see
	// AgentSessionPhaseAwaitingDecision. Kept for back-compat with
	// in-flight CRs written before the unified state machine.
	AgentSessionPhaseAwaitingApproval = "AwaitingApproval"
	// AgentSessionPhaseAwaitingDecision is the canonical phase entered
	// when one or more tool dispatches are paused waiting for a human
	// decision (tool_call, leakage_share, content_inspect, scope_review).
	// Restored to the pre-decision phase when all pending decisions resolve.
	AgentSessionPhaseAwaitingDecision = "AwaitingDecision"
	// AgentSessionPhaseHeld is a forensic hold: the session is parked with its
	// workspace preserved, pending a human decision to release it. Byte-identical
	// to lifecycle.PhaseHeld, which projects onto status.phase.
	AgentSessionPhaseHeld = "Held"
	// AgentSessionPhaseAwaitingRetry is entered when the LLM provider
	// returned an error on a channel-attached session. Pod exits clean;
	// channelsd posts a Retry button on the channel. Click sets the
	// wake annotation; the controller transitions back to Pending and
	// respawns the runner. Non-terminal.
	AgentSessionPhaseAwaitingRetry = "AwaitingRetry"
)

// AgentSessionPhaseStarted reports whether an AgentSession has progressed past
// initial scheduling: "" and Pending mean the runner has not come up yet, every
// other phase means it has.
//
// A session parked in AwaitingCredentials or AwaitingIdentityChoice counts as
// STARTED and is legitimately waiting on the user. Callers gating on "is the
// runner still coming up?" (startup-grace clocks, browser/TUI readiness waits,
// channel-health probes) must therefore treat it as started, not stuck, or a
// session parked on a prompt is mis-declared a failed startup. Shared so the
// predicate is defined exactly once.
func AgentSessionPhaseStarted(phase string) bool {
	switch phase {
	case "", AgentSessionPhasePending:
		return false
	default:
		return true
	}
}

// AgentSession reasons.
const (
	ReasonAgentClassMissing  = "AgentClassMissing"
	ReasonAgentClassNotValid = "AgentClassNotValid"
	ReasonRunnerCreating     = "RunnerCreating"
	ReasonRunnerReady        = "RunnerReady"
	// ReasonRunnerPodRefused is set on RunnerReady=False when creating the
	// runner workload was REFUSED — an admission verdict (a quota, a policy, a
	// webhook), as opposed to ReasonSandboxUnschedulable, where it was created
	// and could not be placed. The condition message carries the refusal's own
	// words, and it is a positive readiness gate
	// (pkg/controllers/agentstatus.notReadyGates), so a person watching the
	// session is told why instead of watching it sit.
	ReasonRunnerPodRefused = "RunnerPodRefused"
	// ReasonRunnerStartError is set on RunnerReady=False when the runner
	// workload could not be created for any reason OTHER than an admission
	// refusal (a transient apiserver error, a client-side pod-build failure).
	// It deliberately does not carry ReasonRunnerPodRefused's "not allowed the
	// resources it needs" phrasing (pkg/controllers/agentstatus/friendly.go) —
	// that wording is reserved for a genuine refusal, and applying it to an
	// unrelated error would misattribute the cause to a quota or policy that
	// was never involved.
	ReasonRunnerStartError    = "RunnerStartError"
	ReasonAgentSessionStalled = "Stalled"
	ReasonAgentSessionBudget  = "BudgetExceeded"
	// ReasonAgentSessionExpired is set on the Failed condition when a session
	// exceeds its wall-clock budget.sessionExpiration (measured from
	// status.startedAt). Distinct from ReasonAgentSessionBudget (turns/tokens/
	// active run-time). Its string value "SessionExpired" is mirrored by the
	// lifecycle Expired event's FailureReason.
	ReasonAgentSessionExpired     = "SessionExpired"
	ReasonAgentSessionRunnerCrash = "RunnerCrashed"
	ReasonAgentSessionBundleFail  = "BundleFailed" // a tool bundle's sandbox failed terminally
	ReasonAgentSessionMemoryDown  = "MemoryUnavailable"
	ReasonAgentSessionProviderErr = "ProviderError"
	// ReasonAgentSessionRefusal: the provider returned stop_reason=refusal
	// (content-policy block). Channel-attached sessions park in AwaitingRetry
	// under this reason (recoverable, Retry button); local sessions fail.
	ReasonAgentSessionRefusal = "Refusal"
	// ReasonAgentSessionScopeReviewFailed is set on the Failed condition when
	// the runner's cold-start scope review could not complete (publish error,
	// timeout, or authzd error). The session fails closed: the agent is never
	// run unscoped when scope review is unavailable.
	ReasonAgentSessionScopeReviewFailed = "ScopeReviewFailed"
	// ReasonAgentSessionColdStartDenied is set on the Failed condition when
	// cold-start scope review COMPLETED and left the session no turn-0 to run:
	// the approver denied the prompt, or the cleaned remainder was empty.
	// Distinct from ReasonAgentSessionScopeReviewFailed, which means review
	// could not complete at all (publish error, timeout, authzd error).
	//
	// Only a DELEGATED child terminalizes on this arm. A session with a person
	// on the other end parks Idle instead (the conversation stays alive for a
	// follow-up) and a kubectl-driven one completes with an empty result;
	// neither ever carries this reason. A delegated child has no person and no
	// exit code, so its refusal has to reach the waiting parent, and this
	// string is what the parent's delegate / reply_to_subagent call reads back
	// out of the SubagentRequest.
	ReasonAgentSessionColdStartDenied = "ColdStartDenied"
	// ReasonAgentSessionToolGuardHalt is set on the Failed condition when a
	// tool-guard rule with action=halt fires (circuit breaker or rate limit).
	ReasonAgentSessionToolGuardHalt = "ToolGuardHalt"
	// ReasonAgentSessionFederatedIdPSecretMissing is set when a synthesized
	// type=federated credential references an IdP-identity Secret that does not
	// exist â fail-closed and loud, never a silent runner hang.
	ReasonAgentSessionFederatedIdPSecretMissing = "FederatedIdPSecretMissing"
	// ReasonAgentSessionContentGuardHalt is set on the Failed condition when a
	// content-inspector plugin is misconfigured at session start (unregistered ID
	// or invalid plugin config). The session fails closed rather than running with
	// a broken content-guard policy.
	ReasonAgentSessionContentGuardHalt = "ContentGuardHalt"
	// ReasonAgentSessionAwaitingDetector is set while one or more content-guard
	// detector pods have been created but have not yet reported a Ready PodIP.
	// Runner-pod creation is held until every detector IP is reflected; this
	// reason makes the wait visible rather than leaving the session in a blank
	// phase (no-silent-hang policy).
	ReasonAgentSessionAwaitingDetector = "AwaitingDetector"
	// ReasonAgentSessionComplete is set on the Succeeded condition when the
	// agent_work_complete tool fires for a kubectl-driven session (no
	// spec.channel). The string deliberately does not match the tool name:
	// monitoring and alerting filter on it, so it is frozen for compatibility.
	ReasonAgentSessionComplete = "AgentComplete"
	// ReasonAgentSessionAwaitingToolApproval is the Reason set on the
	// Working / AwaitingDecision phase transition.
	ReasonAgentSessionAwaitingToolApproval   = "AwaitingToolApproval"
	ReasonAgentSessionAwaitingEntityApproval = "AwaitingEntityApproval"
	// ReasonAgentSessionSkillBundleDigestMismatch is the reason on
	// SkillBundlesIntegrity=False. The digest is the bundle's content identity,
	// so a mismatch is a tamper signal: the skill falls back to
	// instruction-only and the mismatched bytes are never mounted.
	ReasonAgentSessionSkillBundleDigestMismatch = "SkillBundleDigestMismatch"
)

// AgentSessionConditionSandboxScheduling reasons.
const (
	// ReasonSandboxUnschedulable is the Reason on SandboxScheduling=False.
	ReasonSandboxUnschedulable = "Unschedulable"
	// ReasonSandboxScheduled is the Reason on SandboxScheduling=True.
	ReasonSandboxScheduled = "Scheduled"
)

// Channel condition types.
const (
	ChannelConditionValid     = "Valid"
	ChannelConditionConnected = "Connected"
	// ChannelConditionDeliverable records the outcome of the most recent
	// outbound delivery attempt through this Channel. Owned by channelsd's
	// outbound relay: False (DeliveryFailed) when a Sender's Send errored —
	// the bot evicted from or never invited to its Slack channel, a deleted
	// destination, a revoked token — and True (DeliverySucceeded) as soon as
	// a send lands again. Distinct from Connected, which reports
	// listener/socket health: a Slack channel whose bot token connects fine
	// still cannot deliver into a channel the bot is not a member of, and
	// that fact is observable only at send time. Absent until the first
	// outbound send is attempted; monitored by pkg/controllers/monitoring so
	// a channel that silently swallows agent replies alerts instead of
	// looking healthy.
	ChannelConditionDeliverable = "Deliverable"
	// ChannelConditionScopesValid is True when the runtime credential carries
	// every scope the listener needs. Owned by channelsd's listener, not the
	// operator. On False the message names the exact missing scopes, so an
	// operator can reinstall the app with the right grants without grepping
	// channelsd logs after the first failed message.
	ChannelConditionScopesValid = "ScopesValid"
	// ChannelConditionInformationLeakageReady mirrors the AgentClass-side
	// InformationLeakageReady check from the Channel's perspective. True
	// when this Channel's kind satisfies the capability level required by
	// the bound AgentClass's informationLeakage policy. Set by the channel
	// controller; absent on monitoring-role channels (no AgentClass).
	ChannelConditionInformationLeakageReady = "InformationLeakageReady"

	// ChannelConditionWebhookURLDrift is True when a Channel's inbound
	// webhook, as currently registered with its third-party provider (a
	// GitHub App's hook_attributes.url), no longer matches where this
	// cluster actually serves deliveries
	// (channelevents.WebhookPathFor under the cluster's external base URL).
	// Status=Unknown means the provider could not be reached/read — that is
	// NOT the same fact as "no drift" and is reported distinctly, never
	// folded into False. Set by the channel controller for whichever kind
	// implements channelkinds.WebhookURLDriftChecker (github today);
	// absent for every other kind, and absent when the check has nothing to
	// run against yet (no ExternalBaseURL configured, no credentials
	// Secret).
	//
	// Whether the controller CORRECTS a mismatch, rather than only reporting
	// it, turns on one fact: whether this tool registered the application in
	// the first place, recorded by the provenance marker a wizard stamps on
	// the Channel (channelkinds.AnnotationAppProvisionedBy).
	//
	//   - MARKED — the application is this tool's own. The controller
	//     repoints it to where the cluster now serves and reports the result
	//     here: False/URLRepointed when the write lands, True/RepointFailed,
	//     carrying the provider's error, when it does not.
	//   - UNMARKED — a human registered it. Silently changing an
	//     outward-facing setting on a resource the cluster does not own needs
	//     a human, not an automatic fix, so the mismatch is reported
	//     (True/URLDrifted) and never written back. A human corrects it on
	//     the provider's own settings page; the next reconcile clears the
	//     condition once the registered URL matches again.
	ChannelConditionWebhookURLDrift = "WebhookURLDrift"
)

// MetaagentChannelMembership condition tracks whether the metaagent bot
// has been auto-invited to the channel by the AgentSession controller.
// Set only when the bound AgentClass has scope.enabled=true.
const (
	ChannelConditionMetaagentChannelMembership = "MetaagentChannelMembership"

	ChannelReasonMetaagentInvited            = "Invited"
	ChannelReasonMetaagentInsufficientScopes = "InsufficientScopes"
	ChannelReasonMetaagentDMNotSupported     = "DMNotSupported"
	ChannelReasonMetaagentAppNotInstalled    = "MetaagentAppNotInstalled"
)

// Channel reasons.
const (
	ReasonChannelAllReferencesResolve    = "AllReferencesResolve"
	ReasonChannelSpecInvalid             = "SpecInvalid"
	ReasonChannelSecretMissing           = "SecretMissing"
	ReasonChannelSecretKeyMissing        = "SecretKeyMissing"
	ReasonChannelAgentClassMissing       = "AgentClassMissing"
	ReasonChannelAgentClassNotValid      = "AgentClassNotValid"
	ReasonChannelAgentIdentityMissing    = "AgentIdentityMissing"
	ReasonChannelSocketAttached          = "SocketAttached"          // set by channelsd on Connected=True
	ReasonChannelSocketDetached          = "SocketDetached"          // set by channelsd on Connected=False
	ReasonChannelDependenciesUnavailable = "DependenciesUnavailable" // memory or SpiceDB unreachable
	ReasonChannelListenerStartFailed     = "ListenerStartFailed"     // set by channelsd when listener.Start fails
	ReasonChannelScopesGranted           = "ScopesGranted"           // set by channelsd on ScopesValid=True
	ReasonChannelMissingScopes           = "MissingScopes"           // set by channelsd on ScopesValid=False; message lists the missing scopes
	ReasonChannelDeliverySucceeded       = "DeliverySucceeded"       // set by channelsd's relay on Deliverable=True
	ReasonChannelDeliveryFailed          = "DeliveryFailed"          // set by channelsd's relay on Deliverable=False; message carries the send error

	// ReasonChannelOutputBindingUnresolvable is set on a role=input Channel
	// whose reply target cannot be resolved: its AgentClass has zero or more
	// than one role=output Channel, or the resolved Channel's kind provides no
	// outbound anchor. Reported at apply time so the misconfiguration does not
	// first surface when the cron fires.
	ReasonChannelOutputBindingUnresolvable = "OutputBindingUnresolvable"

	// ReasonChannelOutputDestinationMissing is set on a role=output Channel
	// bound to an AgentClass whose status.userlessInput is true: the class's
	// input carries no human, so no inbound message supplies a destination and
	// this Channel has to configure its own. It is refused at apply time
	// because the alternative is a Channel that reports Valid=True with nowhere
	// to post: the failure would then wait to surface until the agent finished
	// its work and the reply went nowhere.
	//
	// Distinct from ReasonChannelOutputBindingUnresolvable, which is reported
	// on the role=input Channel about ITS target; this one is reported on the
	// output Channel itself, which is the object a human has to edit.
	ReasonChannelOutputDestinationMissing = "OutputDestinationMissing"

	// WebhookURLDrift reasons. ReasonChannelWebhookURLDrifted is Status=True
	// (a mismatch was found); ReasonChannelWebhookURLMatches is Status=False
	// (registered and expected agree); ReasonChannelWebhookProviderUnreachable
	// is Status=Unknown (the provider could not be reached/read — distinct
	// from both, never conflated with "matches").
	ReasonChannelWebhookURLDrifted          = "URLDrifted"
	ReasonChannelWebhookURLMatches          = "URLMatches"
	ReasonChannelWebhookProviderUnreachable = "ProviderUnreachable"

	// Repoint reasons, set only on a Channel whose application this tool
	// registered. ReasonChannelWebhookURLRepointed is Status=False — the
	// registration now matches because this controller just wrote it, which
	// is a different fact from URLMatches ("it already agreed") and is worth
	// telling apart when reading a Channel's history.
	// ReasonChannelWebhookRepointFailed is Status=True: the write was refused,
	// so deliveries still go somewhere wrong, and the condition's message
	// carries the provider's error.
	ReasonChannelWebhookURLRepointed  = "URLRepointed"
	ReasonChannelWebhookRepointFailed = "RepointFailed"
)

// AgentSession new phase + condition + reasons added by channels.
const (
	AgentSessionPhaseIdle     = "Idle"
	AgentSessionConditionIdle = "Idle"
	// AgentSessionConditionAwaitingRetry is True while the runner has
	// exited for a provider error and channelsd is waiting on the user
	// to click Retry. Set by the runner's WriteAwaitingRetry; cleared
	// by the controller on wake (AwaitingRetry â Pending).
	AgentSessionConditionAwaitingRetry = "AwaitingRetry"
	ReasonAgentSessionAwaitingUserMsg  = "AwaitingUserMessage"
	ReasonAgentSessionWakeRequested    = "WakeRequested"
	ReasonAgentSessionAuthzWriteFail   = "AuthzWriteFailed" // SpiceDB write failure on new-session creation

	// AgentSessionConditionStarterAllowed records the start-gate verdict for a
	// class that declares spec.authz.session.allowedStarters. True (reason
	// StarterAllowed) is set ONCE and never re-asked, so an operator restart
	// cannot re-decide a running session; False with NotAnAllowedStarter is
	// the refusal (the session is Failed); False with StartAuthzUnavailable is
	// indeterminate (requeued, never a refusal).
	AgentSessionConditionStarterAllowed     = "StarterAllowed"
	ReasonAgentSessionStarterAllowed        = "StarterAllowed"
	ReasonAgentSessionNotAnAllowedStarter   = "NotAnAllowedStarter"
	ReasonAgentSessionStartAuthzUnavailable = "StartAuthzUnavailable"

	// ReasonAgentSessionRetryBudgetExhausted is set on the AwaitingRetry
	// condition (cleared) and the Failed condition when the runner has hit
	// the maxRetry cap â no more automatic AwaitingRetry cycles are allowed.
	ReasonAgentSessionRetryBudgetExhausted = "RetryBudgetExhausted"
	// ReasonAgentSessionRetryTimeout is set on the Failed condition when the
	// operator's AwaitingRetry TTL elapses without the user clicking Retry.
	ReasonAgentSessionRetryTimeout = "RetryTimeout"
)

// Multiplayer-sessions condition types and reasons.
const (
	// AgentSessionConditionInteractPolicyApplied is stamped after the
	// AgentClass.spec.sessionInteractPermission snapshot step at session
	// creation. True when the snapshot was captured (or the field was
	// empty); False when the snapshot was captured but the SpiceDB write
	// failed.
	AgentSessionConditionInteractPolicyApplied = "InteractPolicyApplied"
	// AgentSessionConditionPermissionRequestPending is True when
	// len(Status.PendingRequesters) > 0.
	AgentSessionConditionPermissionRequestPending = "PermissionRequestPending"
	// AgentSessionConditionStartApprovalPending is True while a session an
	// org non-member started sits parked awaiting a platform admin's
	// start_approval decision. Written by channelsd alongside the
	// PendingRequesters entry it refers to; the operator reads the sibling
	// annotation (AnnotationStartApprovalRequestRef), not this condition, to
	// withhold the runner.
	AgentSessionConditionStartApprovalPending = "StartApprovalPending"
	// The three *ApprovalPending conditions are channelsd-owned observable
	// surfaces for an outstanding decision of each category. All are written by
	// the generic park handler (registry-driven via
	// channelinteractions.Category.PendingCondition) alongside the
	// PendingInteractions list; none of them derives phase â
	// phase=AwaitingDecision comes from the runner's signed lifecycle
	// projection.
	AgentSessionConditionToolApprovalPending              = "ToolApprovalPending"
	AgentSessionConditionInfoLeakageApprovalPending       = "InfoLeakageApprovalPending"
	AgentSessionConditionContentInspectionApprovalPending = "ContentInspectionApprovalPending"
	// AgentSessionConditionPreconditionWaiverPending is True while a
	// precondition_waiver decision is outstanding: a human is being asked to
	// waive a slot precondition whose CEL predicate over signed facts Refused.
	// Same generic-park-handler machinery and same observable-only status as the
	// three above — approving it binds a slot grant, which IS the waiver, but the
	// phase stays derived from the runner's signed lifecycle projection.
	AgentSessionConditionPreconditionWaiverPending = "PreconditionWaiverPending"
	// AgentSessionConditionPreferenceConfirmPending is True while a user_preference_confirm
	// decision is outstanding: the addressee is being asked to confirm a preference update.
	AgentSessionConditionPreferenceConfirmPending = "PreferenceConfirmPending"
	// AgentSessionConditionScopeReviewPending is True while a cold-start scope_review
	// decision is outstanding and turn-0 execution is blocked. Written by the operator
	// from the folded lifecycle projection; cleared when the decision resolves.
	AgentSessionConditionScopeReviewPending = "ScopeReviewPending"
	// AgentSessionConditionToolCallGated is True when one or more tool calls were
	// denied by a hook (toolguard, revocation, content-guard) but not surfaced to a
	// human approver. The operator writes this from the folded lifecycle projection so
	// the count is readable from CR status without grepping logs (no-silent-errors).
	AgentSessionConditionToolCallGated = "ToolCallGated"

	// ReasonInteractPolicyApplied marks that the AgentSession received
	// the AgentClass snapshot at creation.
	ReasonInteractPolicyApplied = "AppliedFromAgentClass"
	// ReasonInteractPolicyNotConfigured marks that AgentClass had no
	// sessionInteractPermission set.
	ReasonInteractPolicyNotConfigured = "NotConfigured"
	// ReasonInteractPolicySpiceDBWriteFailed marks that the snapshot was
	// captured but the SpiceDB participant write failed.
	ReasonInteractPolicySpiceDBWriteFailed = "SpiceDBWriteFailed"

	// Restart-from-here condition types and reasons. Set by the
	// AgentSession controller's restart reconciler and the toolcall
	// controller's JIT-snapshot path, respectively.
	// AgentSessionConditionSupersededByRestart indicates the session was
	// archived because the user triggered Restart-from-here, producing a
	// child AgentSession (status.supersededBy). The child carries the
	// continuation; this session is read-only.
	AgentSessionConditionSupersededByRestart = "SupersededByRestart"

	// AgentSessionConditionWorkspaceSnapshotFailed indicates a JIT
	// workspace snapshot Job failed. While True, the runner refuses to
	// dispatch further stateImpact={readwrite,external} tool calls â the
	// next would lose the recoverable cut point for fork-at-this-turn.
	AgentSessionConditionWorkspaceSnapshotFailed = "WorkspaceSnapshotFailed"

	// ReasonReplacedByForkedSession is the reason for
	// SupersededByRestart=True. The condition Message names the child.
	ReasonReplacedByForkedSession = "ReplacedByForkedSession"

	// ReasonSnapshotJobFailed is the reason for
	// WorkspaceSnapshotFailed=True. The condition Message names the
	// failed Job and its terminating phase.
	ReasonSnapshotJobFailed = "SnapshotJobFailed"

	// AgentSessionConditionRestartDenied indicates a restart-from-here (fork)
	// was denied by the SessionFork authz gate â the forker is not the session
	// owner. The PendingRestart marker is cleared; no child is created.
	//
	// It reports the CURRENT attempt's verdict, not "a denial happened once":
	// the operator sets it False (ReasonRestartAttemptPending) whenever it
	// picks up a new PendingRestart, so a session whose later continuation
	// succeeds stops reading as denied, and a repeat denial is a genuine
	// FalseâTrue transition â which is what lets anything keying off
	// LastTransitionTime see the user's retry at all.
	AgentSessionConditionRestartDenied = "RestartDenied"

	// ReasonForkNotAuthorized is the reason for AgentSessionConditionRestartDenied.
	ReasonForkNotAuthorized = "ForkNotAuthorized"

	// ReasonRestartAttemptPending is the reason for
	// AgentSessionConditionRestartDenied=False: the operator has picked up a new
	// PendingRestart and cleared the previous attempt's verdict before running
	// the fork gate. It is what makes the condition report current state â and
	// what makes a repeat denial a real FalseâTrue transition, so consumers that
	// key off LastTransitionTime see the retry.
	ReasonRestartAttemptPending = "RestartAttemptPending"

	// ReasonRestartMarkerUnverified is the reason for RestartDenied=True when
	// status.pendingRestart could not be authenticated as connector-authored:
	// an unregistered signing key, or a digest mismatch. A marker carrying no
	// attestation envelope at all gets RestartMarkerUnsigned instead â that is
	// what a pre-upgrade connector writes, so it must not read as a forgery.
	// Distinct from ForkNotAuthorized, where a KNOWN requester failed the
	// SpiceDB fork check; here the requester on the marker cannot be believed
	// at all.
	ReasonRestartMarkerUnverified = "RestartMarkerUnverified"
)

// AgentSessionConditionStorageReclaimed is set True by the operator's
// storage-reclaim sweeps once a session's workspace and snapshot-store PVCs
// have been deleted. Two sweeps set it: the terminal sweep, once a
// TERMINAL session's retention has elapsed past FinishedAt; and the idle sweep,
// once a SLEPT, node-pinned Idle session has been asleep past
// --idle-storage-reclaim-after (that session stays Idle and wakeable). Either
// way the AgentSession itself — transcript, memory records, SpiceDB
// relationships — is untouched; only the re-creatable scratch volumes go.
//
// The condition records that a sweep ran; it does NOT gate future sweeps. A
// session that wakes after being swept re-provisions both claims (and, if
// terminal, finishes again), and that next lifetime's storage must still be
// reclaimable — a write-once reading stranded it on the node's local disk
// forever. What keeps re-provisioned storage safe from a stale decision is the
// deadline: the terminal sweep measures from the CURRENT FinishedAt, the idle
// sweep from the CURRENT SleptAt, so fresh storage always gets a fresh window.
const (
	AgentSessionConditionStorageReclaimed = "StorageReclaimed"
	ReasonAgentSessionStorageReclaimed    = "RetentionElapsed"
)

// Reasons for the AgentSession.Idle condition's per-cause subdivision.
const (
	ReasonAgentSessionAgentWorkComplete = "AgentWorkComplete" // agent_work_complete tool was called
	ReasonAgentSessionArchived          = "Archived"          // operator archive sweep transitioned Idle â Succeeded
)

// AnnotationPinRefreeze requests that the dependency's pin baseline be
// re-frozen to the given identity ("sha256:â¦" manifest hash or image
// digest). Honored by the owning reconciler ONLY when the live identity
// equals the annotation value (race-free accept: you re-freeze the exact
// thing you reviewed); the annotation is cleared once honored. A stale
// value (live moved again) leaves PinDrift in place with a message noting
// the failed refreeze.
const AnnotationPinRefreeze = "agentprimitives.authzed.com/pin-refreeze"

// Annotation key for channelsd â operator wake-up signaling.
const (
	AnnotationWakeRequestedAt = "agentprimitives.authzed.com/wake-requested-at"

	// AnnotationUIServeRequestedAt asks the operator to spawn a SERVE-ONLY
	// runner: one that answers a browser's agent-UI requests and runs no agent
	// loop. Written by webd when a data binding finds no runner subscribed.
	//
	// A SECOND key rather than a second meaning for the one above, and the
	// distinction is the point: AnnotationWakeRequestedAt resumes the
	// CONVERSATION, so the runner it spawns replays memory, claims the live
	// region and takes an LLM turn. Reusing it made a dashboard load put
	// content in the agent's context that no human asked for, and on a live
	// cluster left the session Failed. A pod spawned via THIS key must not move
	// phase or write a terminal status; internal/cmd/runner enforces that structurally
	// by never calling Loop.Run, the sole writer of both.
	//
	// Both keys carry an RFC3339Nano timestamp the reconciler compares against
	// what it has already acted on, so re-stamping while a spawn is in flight
	// coalesces rather than duplicating.
	AnnotationUIServeRequestedAt = "agentprimitives.authzed.com/ui-serve-requested-at"

	// AnnotationSidecarFailureReported carries, as a JSON object, the
	// sidecar-name â failure-fingerprint map the runner has already reported to
	// the user â the durable half of its once-per-failure contract, since a
	// process-local record alone re-reports the same failure on every restart.
	// Written by the runner, which already patches session annotations, and
	// deliberately not status: status here is the operator's observation of the
	// pods.
	//
	// It lives beside AnnotationWakeRequestedAt because those two are the whole
	// of what a session runner may write on the main resource â
	// pkg/controllers/webhooks/agentsession's runnerWritableAnnotations refuses
	// every other key. internal/cmd/runner still defines its own copy
	// (sidecarFailureReportedAnnotation); it should be collapsed onto this one.
	AnnotationSidecarFailureReported = "agentprimitives.authzed.com/sidecar-failure-reported"

	// AnnotationAwaitingRetrySince records when the operator first observed
	// the session in phase=AwaitingRetry. Used by the retry-TTL mechanism:
	// the operator stamps this once on entry and requeueAfter the remainder;
	// when the TTL elapses the operator emits RetryTTLExpired â Failed[RetryTimeout].
	// Cleared when the session wakes (RetryRequested â Pending).
	AnnotationAwaitingRetrySince = "agentprimitives.authzed.com/awaiting-retry-since"

	// AnnotationStartedByExternalID records the kind-specific external identity
	// (a Slack user_id, say) of the inbound that created the AgentSession.
	// Stamped and consumed by the channelsd pipeline as a fast-path "same
	// external user" check, bypassing the canonical_id round-trip â which is
	// fragile when upstream identity resolution flickers across inbounds and
	// yields different canonical_ids for the same human.
	AnnotationStartedByExternalID = "agentprimitives.authzed.com/started-by-external-id"

	// AnnotationStartedByEmail records the VERIFIED email of the human who
	// originally created the AgentSession, when the originating channel kind
	// vouches for one (webchat/idp always; Slack when a users.info lookup
	// resolved it) â the same email fed into identity.FromExternal(...).Email
	// when the session's started-by canonical was computed (see
	// AnnotationStartedByCanonicalID). Stamped alongside
	// AnnotationStartedByExternalID at session-creation time.
	//
	// Consumed by IdentityChoiceGate.requester() so the ask|dynamic prompt's
	// Audience.Requester carries the SAME verified email the deciding click's
	// payload does; both sides then canonicalize to the identical SpiceDB
	// subject. Without it, Canonical() rejects the email-less requester with
	// ErrSyntheticSubject and DecideRequester fails every decision closed.
	//
	// Empty when the channel kind has no verified email for the starter
	// (kubectl-driven sessions, or a channel-native id with no email lookup).
	// The gate's requester then has no Email either and fails closed like any
	// guest identity â intended: such an initiator cannot decide their own
	// identity_choice prompt, just as they could never pass through as
	// themselves.
	AnnotationStartedByEmail = "agentprimitives.authzed.com/started-by-email"

	// AnnotationBackfilledFromTS records the kind-opaque cursor of the
	// OLDEST message seeded into memory at thread adoption. The
	// read_thread_history tool defaults its BeforeTS to this value so
	// the agent pages strictly older than what is already in memory.
	AnnotationBackfilledFromTS = "agentprimitives.authzed.com/backfilled-from-ts"

	// AnnotationBackfilledThroughTS records the kind-opaque cursor of the
	// NEWEST message seeded into memory. The pipeline advances it on
	// every catch-up so the next catch-up only seeds messages newer
	// than this.
	AnnotationBackfilledThroughTS = "agentprimitives.authzed.com/backfilled-through-ts"
)

// Label keys for channel-spawned AgentSessions.
const (
	LabelChannelName = "channel.agentprimitives.authzed.com/name"
	LabelChannelKind = "channel.agentprimitives.authzed.com/kind"
	LabelChannelKey  = "channel.agentprimitives.authzed.com/key" // sha256-hex of channelKey

	// LabelOutputChannelKey is the sha256 of the session's
	// spec.outputChannel.Key, patched by channelsd's outbound relay once a
	// cron-spawned session's thread anchor is captured on the first send. The
	// pipeline's session lookup matches it as a union alongside
	// LabelChannelKey, so an inbound reply to a cron-spawned thread resolves
	// the right AgentSession.
	LabelOutputChannelKey = "channel.agentprimitives.authzed.com/output-key"
)

// PinDriftCondition reports whether a dependency's live identity diverged
// from its recorded pin baseline. Shared by every pinned-dependency CR
// (MCPServer and SidecarToolbox today).
const PinDriftCondition = "PinDrift"

const (
	ReasonPinDrifted      = "PinDrifted"      // live identity != baseline
	ReasonPinMatch        = "PinMatch"        // live identity == baseline
	ReasonPinVerifyFailed = "PinVerifyFailed" // could not compute live identity
)

// MCPServer reconciler condition reasons.
const (
	ReasonMCPServerAllowlistResolved      = "AllowlistResolved"
	ReasonMCPServerProbeOK                = "ProbeOK"
	ReasonMCPServerProbeFailed            = "ProbeFailed"
	ReasonMCPServerAllowlistDrift         = "AllowlistDrift"
	ReasonMCPServerConstraintCompileError = "ConstraintCompileError"
	ReasonMCPServerAuthResolutionFailed   = "AuthResolutionFailed"
	// ReasonMCPServerSpicedbSchemaMissing fires when one or more
	// `tools[*].permission.check.resourceType` or
	// `tools[*].permissionVariants[*].check.resourceType` references a
	// resource that is not declared in this MCPServer's
	// `spec.spiceDBSchema.resources[*].name` (the implicit `user` type
	// is always allowed). The condition message lists every offending
	// reference so operators can fix the spec without source-diving.
	ReasonMCPServerSpicedbSchemaMissing = "SpicedbSchemaMissing"

	// ReasonMCPServerRouteViaSessionGrantMissingLeaf is RETIRED along with the
	// field it policed. It required a wildcard leaf in the permission
	// expression, because the post-approval walk re-evaluated that expression
	// for a user who lacked the permission and could pass no other way. The
	// grant now points from the resource at the session, so nothing needs a
	// wildcard and there is no invariant to enforce.

	// ReasonMCPServerWildcardLeafNotRouted is the converse of
	// ReasonMCPServerRouteViaSessionGrantMissingLeaf, and the fail-OPEN
	// half of the same invariant: a tool's Permission.Check targets a
	// permission that a `wildcard: true` relation can satisfy, but the
	// check does NOT set routeViaSessionGrant. The direct
	// `<resourceType>:<id>#<permission>@user:<subject>` check then
	// succeeds for every user, so the gate never denies and the approval
	// flow it exists to trigger never fires â silently. The condition
	// message names each offending (tool spec path, resourceType,
	// permission, wildcard relation) and both ways to resolve it.
	ReasonMCPServerWildcardLeafNotRouted = "WildcardLeafNotRouted"

	// ReasonMCPServerAuthCredentialMissing fires when spec.auth.provider is
	// set but spec.auth.credential is empty. Credential names must be
	// declared explicitly â there is no metadata.name fallback â so both
	// setup-identity and the runner descriptor agree on what credential to
	// look up. The condition message names the missing field.
	ReasonMCPServerAuthCredentialMissing = "AuthCredentialMissing"

	// ReasonMCPServerLabelsCompileError fires when a tool's `labels` block has
	// a malformed CEL expression (when / forEach / label.*). The condition
	// message lists each offending (tool, block-index, field, err) tuple so
	// authors can fix the spec without source-diving.
	ReasonMCPServerLabelsCompileError = "LabelsCompileError"

	// ReasonMCPServerRelationshipsCompileError fires when a tool's
	// `writesRelationships` block has a malformed CEL expression (when /
	// forEach / tuple.resource / tuple.relation / tuple.subject). The message
	// names the offending (tool, block-index, field, err) so authors can fix
	// the spec without source-diving.
	//
	// Validation compiles through the SAME relwrites env that executes these
	// expressions, so "validated" and "will run" cannot drift. Compiling them
	// first at dispatch time instead would let a server carrying an
	// uncompilable expression report Valid=True and fail mid-turn, as an error
	// the agent cannot act on.
	ReasonMCPServerRelationshipsCompileError = "RelationshipsCompileError"
)

// AgentSession session-fatal reasons specific to MCP tool dispatch.
const (
	ReasonAgentSessionMCPServerMissing          = "MCPServerMissing"
	ReasonAgentSessionMCPServerInvalid          = "MCPServerInvalid"
	ReasonAgentSessionMCPAuthResolutionFailed   = "MCPAuthResolutionFailed"
	ReasonAgentSessionMCPServerUnreachable      = "MCPServerUnreachable"
	ReasonAgentSessionMCPAllowlistDrift         = "MCPAllowlistDrift"
	ReasonAgentSessionMCPConstraintCompileError = "MCPConstraintCompileError"
)

// ReasonAgentSessionTokenGrantFailed is set on the AgentSession Failed condition
// when the operator cannot reconcile the session's externaltoken authorized_token
// grants (a credential value that won't resolve, or a SpiceDB write failure). It
// is fail-closed: the runner is not dispatched with an incomplete grant set, so
// the token-use pre-send check can never wave through an un-authorized token.
const ReasonAgentSessionTokenGrantFailed = "TokenGrantFailed"

// ReasonAgentSessionTokenAuthzUnavailable is set on the AgentSession Failed
// condition when the runner's per-call use_token SpiceDB check is
// indeterminate â the checker is unconfigured (typed-nil) or the RPC itself
// errored (SpiceDB unreachable/timeout). This is distinct from a definitive
// deny (revoked token), which is surgical â an IsError tool result â and
// does NOT fail the session. An indeterminate check must fail closed: never
// let a MCP call proceed when authorization could not be confirmed.
const ReasonAgentSessionTokenAuthzUnavailable = "TokenAuthzUnavailable"

// ReasonAgentSessionSkillResolutionFailed is set on the AgentSession Failed
// condition when the runner cannot resolve every skill the AgentClass opts into
// at startup. The AgentClass gate already proved each skill materialized and is
// Valid=True before the session was allowed to spawn, so an unresolvable skill
// in the runner is a genuine inconsistency â usually the per-session runner
// ServiceAccount lacking read on skills/clusterskills â never a benign "not
// synced yet". Fail closed and diagnosable, rather than silently composing a
// skill-less prompt.
const ReasonAgentSessionSkillResolutionFailed = "SkillResolutionFailed"

// AgentClass condition reasons covering Channel-level validation
// (authz attribution for non-user-attributable input channels).
const (
	// ReasonAgentClassUserLessMissingAuthz fires when an AgentClass binds
	// an input-role (or both-role) Channel whose kind reports
	// UserAttributable=false (e.g. bento) but the AgentClass has no
	// sessionInteractPermission AND/OR the Channel has no authzSubject â
	// leaving the spawned AgentSession with no SpiceDB subject to attribute
	// work to and no broad grant for humans to interact with the session.
	ReasonAgentClassUserLessMissingAuthz = "UserLessChannelMissingAuthz"
)

// AgentClass condition reasons specific to MCP server refs.
const (
	ReasonAgentClassMCPServerMissing = "AgentClassMCPServerMissing"
	ReasonAgentClassMCPServerInvalid = "AgentClassMCPServerInvalid"
	// ReasonAgentClassSkillMissing fires when the AgentClass opts into a
	// skill (spec.skills, a canonical name) for which no Skill or
	// ClusterSkill has materialized yet â so the class is parked
	// (Valid=False) rather than silently dropping the skill at session
	// start. ReasonAgentClassSkillInvalid fires when such a Skill exists
	// but is not itself Valid=True (e.g. its SKILL.md was rejected).
	ReasonAgentClassSkillMissing = "AgentClassSkillMissing"
	ReasonAgentClassSkillInvalid = "AgentClassSkillInvalid"
	// ReasonAgentClassSpicedbSchemaConflict fires when two or more
	// MCPServers referenced by the AgentClass declare the same SpiceDB
	// resource (by name) with non-identical definitions â the
	// guardian/schema composer cannot merge them into a single coherent
	// SpiceDB schema. The condition message names the conflicting
	// resource(s). The cross-MCPServer mirror of
	// ReasonMCPServerSpicedbSchemaMissing's intra-fragment check.
	ReasonAgentClassSpicedbSchemaConflict = "SpicedbSchemaConflict"

	// ReasonAgentClassSlotAutofillUnfillable marks a slot whose autoFillArgs
	// target a sandbox tool. Auto-fill writes a NAMED argument; a sandbox tool
	// takes argv, so the value would be ignored by both the tool and its
	// permission check â silently. Declaring autoFillArgs is an opt-in, and an
	// opt-in that cannot be honoured makes the class not-ready rather than
	// quietly binding nothing.
	ReasonAgentClassSlotAutofillUnfillable = "SlotAutofillUnfillable"
)

// ArtifactRender condition reasons.
const (
	ReasonArtifactRenderRendered        = "Rendered"
	ReasonArtifactRenderRendererUnknown = "RendererUnknown"
	ReasonArtifactRenderPayloadTooLarge = "PayloadTooLarge"
	ReasonArtifactRenderRendererError   = "RendererError"
	ReasonArtifactRenderOutputTooLarge  = "OutputTooLarge"
	ReasonArtifactRenderTimeout         = "Timeout"
	ReasonArtifactRenderInternalError   = "InternalError"
	// ReasonArtifactRenderMalformedInput: the payload was structurally broken
	// before rendering (e.g. truncated mid-tag, or fully HTML-entity-encoded so
	// it would render as literal source). Distinct from RendererError so a bad
	// input is greppable separately from an internal renderer fault.
	ReasonArtifactRenderMalformedInput = "MalformedInput"
)

// IdentitiesNamespace is the fixed namespace that holds cluster-scoped
// UserIdentity master credential Secrets. UserIdentity is cluster-scoped
// and has no namespace of its own, so its credentials' secretRefs always
// resolve against this namespace.
const IdentitiesNamespace = "agentprimitives-identities"

// AgentClass identity modes (AgentClassSpec.IdentityMode).
const (
	IdentityModeAgent           = "agent"
	IdentityModeUserPassthrough = "userPassthrough"
	IdentityModeAsk             = "ask"     // interactive: ask the initiating user
	IdentityModeDynamic         = "dynamic" // interactive: LLM recommends, user confirms
)

// AgentSession passthrough-identity phase. Entered when an
// identityMode=userPassthrough session's starter is missing one or more
// credentials the agent needs. The operator does not spawn a runner pod
// while a session is in this phase.
const AgentSessionPhaseAwaitingCredentials = "AwaitingCredentials"

// AwaitingIdentityChoice: an identityMode=ask|dynamic session is waiting for the
// initiating user to choose an identity. UNLIKE AwaitingCredentials, the operator
// DOES keep the runner pod alive in this phase â the runner drives the choice prompt.
const AgentSessionPhaseAwaitingIdentityChoice = "AwaitingIdentityChoice"

// AwaitingStartApproval: a session started by an org non-member (a channel
// guest) without agentclass#start_session is parked awaiting a platform
// admin's decision. Like AwaitingCredentials, the operator spawns NO runner
// pod while a session is in this phase — the session has no started_by tuple
// yet, so nothing may act on the guest's behalf until an admin approves.
const AgentSessionPhaseAwaitingStartApproval = "AwaitingStartApproval"

// Reasons on the Failed condition for a start_approval that ended without an
// approve: StartDenied when a platform admin clicked Deny, and
// StartApprovalTimeout when the request expired with no decision at all.
const (
	ReasonAgentSessionStartDenied          = "StartDenied"
	ReasonAgentSessionStartApprovalTimeout = "StartApprovalTimeout"
)

// AgentSession passthrough-identity condition + reasons.
const (
	AgentSessionConditionCredentialsReady = "CredentialsReady"

	ReasonAwaitingUserCredentials = "AwaitingUserCredentials"
	ReasonCredentialLinkTimeout   = "CredentialLinkTimeout"
	ReasonUserIdentityResolved    = "UserIdentityResolved"
	ReasonNotPassthrough          = "NotPassthroughClass"
	ReasonMissingStarterSubject   = "MissingStarterSubject"

	ReasonIdentityChoiceTimeout   = "IdentityChoiceTimeout"
	ReasonIdentityChoiceCancelled = "IdentityChoiceCancelled"
	// ReasonIdentityChoiceFailed is the terminal Failed reason for an
	// identityMode=ask|dynamic session whose choice gate failed CLOSED for a
	// reason other than cancel or timeout: the interactive choice path was
	// unavailable (no channel), the choice envelope failed to build, the await
	// transport errored, or the channel returned an unknown action. Distinct from
	// ScopeReviewFailed â an identity halt is never mislabelled as a scope-review
	// failure. Cancel/timeout use their own reasons above so the runner's terminal
	// write agrees with the gate's IdentityChoiceCancelled fold and the operator's
	// IdentityChoiceTimeout backstop.
	ReasonIdentityChoiceFailed = "IdentityChoiceFailed"
)

// AgentSessionConditionCredentialRequestPublished is set True by channelsd once
// it has minted a passthrough link and delivered a KindCredentialRequest on the
// session's credential_request sub-channel. It doubles as the dedup key:
// channelsd's watcher skips a session that is already True, so a long-parked
// session does not spam the user with a fresh "Connect your accounts" prompt
// every poll. LastTransitionTime is effectively the link's mint time.
const AgentSessionConditionCredentialRequestPublished = "CredentialRequestPublished"

// ReasonCredentialRequestPublished is the Reason on a True
// AgentSessionConditionCredentialRequestPublished.
const ReasonCredentialRequestPublished = "CredentialRequestPublished"

// AgentSessionConditionCredentialUpdatePending marks a session parked in
// AwaitingCredentials because a CredentialUpdateRequest it owns is Open â as
// distinct from the SAME phase entered via the passthrough identity gate. The
// two causes share one phase but need independent unpark signals, and this
// condition is what tells the credential-update park/unpark logic "I am the one
// who parked this session", so it never mistakes an identity-gate park for its
// own or vice versa.
//
// Unlike the channelsd-owned CredentialRequestPublished, the operator's
// AgentSession reconciler writes this, through the same agentstatus.WriteOwned
// path as every other operator-owned condition here.
const AgentSessionConditionCredentialUpdatePending = "CredentialUpdatePending"

// ReasonCredentialUpdateRequested is the Reason on a True
// AgentSessionConditionCredentialUpdatePending: an Open CredentialUpdateRequest
// exists for this session.
const ReasonCredentialUpdateRequested = "CredentialUpdateRequested"

// ReasonCredentialUpdateResolved is the Reason on a False
// AgentSessionConditionCredentialUpdatePending: the CredentialUpdateRequest
// that caused the park reached a terminal phase (Fulfilled/Expired/Refused)
// or no longer exists.
const ReasonCredentialUpdateResolved = "CredentialUpdateResolved"

// CredentialUpdateRequestConditionCardDelivered is stamped True by channelsd's
// CredentialUpdateWatcher in the SAME patch that persists
// status.interactionRef, so the two are always set together and can never
// disagree; absent means "not yet delivered", exactly like an empty
// interactionRef.
//
// It exists because determination (the operator's reconciler) and publication
// (channelsd, a separate process) are split, and publication has several silent
// skip paths â no InputChannel, no started-by subject, no ResolvedCredential, a
// watcher disabled by a bad signing key. Without the signal, a request that was
// never delivered still expires reading "nobody updated the credential in
// time", when in truth nobody was ever asked.
const CredentialUpdateRequestConditionCardDelivered = "CardDelivered"

// ReasonCredentialUpdateCardDelivered is the Reason on a True
// CredentialUpdateRequestConditionCardDelivered.
const ReasonCredentialUpdateCardDelivered = "Published"

// ReasonCredentialUpdateNoRecipient is the Reason on CardDelivered=False when
// the request resolved to an agent's OWN shared credential and nobody could be
// shown a card: no role=monitoring Channel to broadcast to, and the turn's
// author does not hold agentidentity#update_credential either.
//
// Surfaced rather than skipped, because a park with no reachable recipient is
// otherwise indistinguishable from one a human is still thinking about â the
// request would sit Open for its whole window and expire having asked nobody.
// status.interactionRef stays EMPTY here, which keeps the Expired reason
// honest: "never delivered", not "nobody updated it in time".
const ReasonCredentialUpdateNoRecipient = "NoRecipient"

// UserIdentity condition types. The string values intentionally match
// AgentIdentity's ("Valid"/"Refresh"); the UserIdentity reconcilers reuse
// the AgentIdentity reason constants (ReasonAllReferencesResolve,
// ReasonSpecInvalid, ReasonNoOAuthCredentials, ...).
const (
	UserIdentityConditionValid   = "Valid"
	UserIdentityConditionRefresh = "Refresh"
)

// SessionUserIdentity condition type + reasons.
const (
	SessionUserIdentityConditionReady = "Ready"

	ReasonSessionUserIdentityResolved = "AllCredentialsBound"
	ReasonSessionUserIdentityMissing  = "CredentialsMissing"
)

// SessionUserIdentityConditionCredentialLinkAvailable reports whether channelsd
// can mint a usable "Connect your accounts" deep-link for the parked session.
// False (ReasonWebdExternalURLNotConfigured) when the platform's external web
// address is unconfigured: a link minted then would be hostless or silently
// point at localhost, so channelsd fails CLOSED rather than ship a broken
// button. It recovers on the next tick after the ConfigMap is fixed. The
// session-observable counterpart of the monitoring event the watcher emits.
const SessionUserIdentityConditionCredentialLinkAvailable = "CredentialLinkAvailable"

// ReasonWebdExternalURLNotConfigured is the Reason on a False
// SessionUserIdentityConditionCredentialLinkAvailable: the
// spicebox-webd-external-url ConfigMap carries no externally reachable URL,
// so no credential link can be generated.
const ReasonWebdExternalURLNotConfigured = "WebdExternalURLNotConfigured"

// AnnotationStartedByCanonicalID records the canonical SpiceDB subject
// ("user:<base64(email)>") of the human who created the AgentSession.
// Stamped by the channelsd pipeline at session creation â the same place
// it canonicalizes identity for the SpiceDB started_by write. The
// AgentSession passthrough gate reads it to locate the starter's
// UserIdentity. Absent on kubectl-driven sessions.
const AnnotationStartedByCanonicalID = "agentprimitives.authzed.com/started-by-canonical-id"

// AnnotationStartApprovalRequestRef marks an AgentSession as parked awaiting a
// platform admin's start_approval decision, and names the RequestRef of the
// PendingRequesters entry the decision resolves. Stamped by the channelsd
// pipeline ON THE CREATE itself — the operator spawns runners off the object's
// existence, so a marker patched afterwards could lose the race it exists to
// close. The operator withholds the runner (and owner resolution) while it is
// present; the approve handler removes it to unpark.
const AnnotationStartApprovalRequestRef = "agentprimitives.authzed.com/start-approval-request-ref"

// AnnotationAuthzServiceSubject records the fully-qualified, non-human SpiceDB
// subject ("service:<id>") an AgentSession acts as when its inbound carried no
// human at all — a webhook delivery, a cron tick. The value is
// Channel.spec.authzSubject, validated as a service subject and canonicalized
// by the channelsd pipeline at session creation, in the same branch that
// decides there is no user to attribute the session to.
//
// It is the SIBLING of AnnotationStartedByCanonicalID, never a substitute for
// it: started_by is `relation started_by: user` in the schema, so a service
// subject stamped there would assert a human who does not exist. The two
// annotations are mutually exclusive by construction — the pipeline writes
// this one exactly where it suppresses that one.
//
// The runner reads it as the FINAL fallback for the tool-call authorization
// subject (see ResolveAuthSubjects). Without it such a session authorizes
// every tool call as the empty subject, which SpiceDB rejects as a malformed
// request rather than answering the authorization question at all.
const AnnotationAuthzServiceSubject = "agentprimitives.authzed.com/authz-service-subject"

// AnnotationForkedFromThread records the Slack thread the parent session
// occupied when a child was forked via Restart-from-here. Format:
//
//	"<team_id>:<channel_id>:<thread_ts>"
//
// Stamped on the child by BuildChildSession so the Slack sender can:
//   - Render the new-thread starter message with a backlink URL to the
//     parent thread.
//   - Post a forward-link notice in the parent thread once the new thread
//     root ts is known (first-send capture).
//   - Post an ephemeral in the new thread to the original requester.
//
// Absent on sessions that are not restarts or whose parent had no Slack
// thread binding.
const AnnotationForkedFromThread = "agentprimitives.authzed.com/forked-from-thread"

// AnnotationSessionOpening carries the one line that opens this session's
// outbound thread: what triggered it, in the trigger's own words.
//
// Only a session whose INBOUND CARRIES NO HUMAN gets one. A webhook payload or
// a cron tick arrives with no message of its own, so the outbound thread's root
// is whatever the agent happens to emit first — a plan checklist, a status
// caption — with nothing saying what it is about. channelsd's outbound relay
// posts this ahead of the session's first output and takes the thread root from
// that send.
//
// Retained after posting — never cleared — so its text survives for channelsd
// to re-render the session's pinned status from later. See
// AnnotationSessionOpeningSent for what stops the line from being posted a
// second time.
//
// Stamped at session creation by the channelsd pipeline, from
// channelkinds/outputbind.SessionOpening — the same place the outbound anchor
// itself is derived, because it is the same question: what roots this thread.
// Absent on a human-initiated session, whose thread is already rooted by the
// person's own message, and on a trigger whose kind cannot describe itself.
const AnnotationSessionOpening = "agentprimitives.authzed.com/session-opening"

// AnnotationSessionOpeningSent marks that the AnnotationSessionOpening line has
// already been posted, independently of the text itself (which is retained,
// not cleared, for the pinned-status renderer).
//
// The outputChannel's own thread_ts is the primary "already open" signal, but
// it is only ever populated for a kind whose Sender reports routing metadata
// back on first send — today, only slack (and its fake test double). Any other
// kind implementing OutboundAnchorProvider would report no root, and without
// this marker the opening line would be re-posted ahead of every subsequent
// envelope for the session's life. Stamped by channelsd's outbound relay right
// after a successful post; left unset on a send failure so the next envelope
// retries.
const AnnotationSessionOpeningSent = "agentprimitives.authzed.com/session-opening-sent"

// The transport message reference keeps edits on the summary itself, even
// when an async session posts into an existing conversation thread.
const AnnotationSessionOpeningMessageChannel = "agentprimitives.authzed.com/session-opening-message-channel"
const AnnotationSessionOpeningMessageID = "agentprimitives.authzed.com/session-opening-message-id"

// AnnotationTriggerOwnerSubject carries the subject-set reference of the
// external account a triggered session's inbound belongs to — for a GitHub
// pull-request session, the PR author as "github_user:<numeric-id>#user".
//
// Written by channelsd from a VERIFIED webhook delivery (the kind's
// TriggerOwnerProvider derives it from provider-structural payload fields,
// never submitter-authored text) and carried through the same
// PreTurnAnnotations stamp every kind-supplied annotation uses. The operator's
// owner resolver reads it and writes an ADDITIONAL agentsession#owner tuple —
// additional to, never instead of, the channel-policy-resolved owner, which
// remains the approval anchor a session must always have.
//
// The subject-set resolves to a platform user only through the attested
// identity edge (github_user#user@user:<canonical>) minted from a verified
// credential, so a session whose author never linked one grants nobody
// anything — and lights up retroactively, with no rewrite here, the moment
// they do. The resolver validates the value's TYPE against the channel-kind
// session links the composed schema actually admits, fail-closed: a value
// naming any other type is logged and not written.
const AnnotationTriggerOwnerSubject = "agentprimitives.authzed.com/trigger-owner-subject"

// PassthroughLinkSigningKeySecret names the Secret in the system
// namespace whose "key" data holds the HMAC signing key for
// passthrough deep-links. Generated once by `oap install`; mounted by
// channelsd (mint) and identityd (verify).
const PassthroughLinkSigningKeySecret = "spicebox-passthrough-link-key"

// SlackOAuthSecret names the Secret holding the Slack app's OAuth
// client_id + client_secret for "Sign in with Slack". Operator-populated
// before deploying identityd. `oap install` creates an empty skeleton.
const SlackOAuthSecret = "spicebox-slack-oauth"

// IdentitydExternalURLConfigMap names the ConfigMap holding identityd's
// external URL (the URL used to mint signed links + to register as the
// OAuth redirect). Operator-populated; default is a kubectl-port-forward
// example.
const IdentitydExternalURLConfigMap = "spicebox-identityd-external-url"

// WebdExternalURLConfigMap names the ConfigMap holding webd's externally
// reachable base URL. The "trusted-url" key carries the URL (the webd
// deployment uses this to mint artifact deep-links). Created by the webd
// install task; may be unpopulated until that task runs.
const WebdExternalURLConfigMap = "spicebox-webd-external-url"

// WebdTrustedURLKey + WebdSandboxURLKey are the ConfigMap keys carrying webd's
// two externally-reachable origin URLs: the auth/trusted surface and the
// sandbox artifact-content surface. webd polls both.
const (
	WebdTrustedURLKey = "trusted-url"
	WebdSandboxURLKey = "sandbox-url"
)

// WebdExternalURLFieldOwner is the server-side-apply field manager the
// PublicEndpoint controller writes the two keys above under.
//
// It lives here rather than in that controller because it is a CONTRACT, not
// an implementation detail: it is how anything else can tell whether the
// controller has actually taken ownership of those keys. A host-side writer of
// the same ConfigMap (`oap desktop`, whose cluster kind creates an endpoint
// only on demand) must stand down once the handover has happened and keep
// writing until it has — and "an endpoint exists" is not that question, since
// an endpoint whose first reconcile fails never writes anything at all.
const WebdExternalURLFieldOwner = "publicendpoint-external-url"

// AgentClass condition reasons specific to SidecarToolbox refs.
const (
	ReasonAgentClassSidecarToolboxMissing = "AgentClassSidecarToolboxMissing"
	ReasonAgentClassSidecarToolboxInvalid = "AgentClassSidecarToolboxInvalid"
)

// SidecarToolbox reasons (Valid / Reachable conditions).
const (
	ReasonSidecarToolboxSpecOK                 = "SpecOK"
	ReasonSidecarToolboxClassMissing           = "ClassMissing"
	ReasonSidecarToolboxClassInvalid           = "ClassInvalid"
	ReasonSidecarToolboxProviderMissing        = "ProviderMissing"
	ReasonSidecarToolboxConstraintCompileError = "ConstraintCompileError"
	ReasonSidecarToolboxSourceInvalid          = "SourceInvalid"
	ReasonSidecarToolboxProbeFailed            = "ProbeFailed"
	ReasonSidecarToolboxAllowlistDrift         = "AllowlistDrift"
	ReasonSidecarToolboxProbeOK                = "ProbeOK"
	// ReasonSidecarToolboxDeferredToSession is set on Reachable=Unknown for
	// secret-gated (separate-pod) sidecars, which the operator does NOT probe at
	// admission time: the real sidecar exists only per-session, once its gating
	// secret is delivered, and runs in a namespace the operator's egress is
	// deliberately locked out of. The runner probes the live pod instead and the
	// result lands on AgentSession.status.resolvedSidecarToolboxes[]. NOT a
	// failure â the admin UI renders it as "Deferred", not red/ProbeFailed.
	ReasonSidecarToolboxDeferredToSession = "DeferredToSession"
	// ReasonSidecarToolboxDeletionBlocked is set when deletion is blocked
	// because one or more AgentClasses still reference this SidecarToolbox.
	// Operators must remove the reference from each AgentClass before the
	// finalizer can be released.
	ReasonSidecarToolboxDeletionBlocked = "DeletionBlocked"
)

// AgentSession sidecar-related fatal reasons.
const (
	ReasonAgentSessionSidecarToolboxMissing = "SidecarToolboxMissing"
	ReasonAgentSessionSidecarToolboxInvalid = "SidecarToolboxInvalid"
	ReasonAgentSessionSidecarBootFailed     = "SidecarBootFailed"
	// ReasonAgentSessionImagePinDrifted is set on AgentSessionConditionSettingsAccepted
	// (False) when a SidecarToolbox's image has drifted from its baseline digest
	// and the cluster/namespace pinning policy mode is "block". The session is
	// held in Pending until the operator updates or re-freezes the toolbox.
	ReasonAgentSessionImagePinDrifted = "ImagePinDrifted"
)

// Skill condition types.
const (
	// SkillConditionValid is True when the skill's canonical name + frontmatter
	// + body pass validation. The admission webhook hard-denies invalid specs;
	// this condition is the controller-set mirror for already-stored objects.
	SkillConditionValid = "Valid"
	// SkillConditionPinned is advisory: True for frozen/named refs, False (with
	// ReasonSkillUnpinnedRolling) for an unpinned, rolling skill.
	SkillConditionPinned = "Pinned"
)

// Skill reasons.
const (
	ReasonSkillValid           = "Valid"
	ReasonSkillInvalidSpec     = "InvalidSpec"
	ReasonSkillPinFrozen       = "Frozen"
	ReasonSkillPinNamed        = "NamedRef"
	ReasonSkillUnpinnedRolling = "UnpinnedRolling"
)

// SkillSource condition types.
const (
	// SkillSourceConditionReady is True when the last sync cloned, discovered,
	// cached, and materialized Skills without a fatal error.
	SkillSourceConditionReady = "Ready"
)

// SkillSource reasons.
const (
	ReasonSkillSourceSynced            = "Synced"
	ReasonSkillSourceAuthResolveFailed = "AuthResolutionFailed"
	ReasonSkillSourceFetchFailed       = "FetchFailed"
	ReasonSkillSourceDiscoverFailed    = "DiscoverFailed"
	ReasonSkillSourceMaterializeFailed = "MaterializeFailed"
	// ReasonSkillSourceNoSkillsDiscovered is set when the fetch itself
	// succeeded but the pass materialized no skill at all — the tree held no
	// SKILL.md under spec.subpath, or every one it held was rejected. Ready is
	// False because a source that yields nothing satisfies no AgentClass
	// opting into it, and Synced would render it identically to a source that
	// yielded everything.
	ReasonSkillSourceNoSkillsDiscovered = "NoSkillsDiscovered"
)

// SpiceboxToolchain condition types.
const (
	SpiceboxToolchainConditionValid = "Valid"
)

// SpiceboxToolchain reasons.
const (
	ReasonToolchainValid         = "Valid"
	ReasonToolchainInvalidSpec   = "InvalidSpec"
	ReasonToolchainUnknownKind   = "UnknownSourceKind"
	ReasonToolchainInvalidSource = "InvalidSource"
)

// SpiceboxClass toolchain reasons.
const (
	ReasonClassInvalidToolchains = "InvalidToolchains"
)

// SpiceboxSession toolchain reasons.
const (
	ReasonToolchainMissing    = "ToolchainMissing"
	ReasonToolchainNotValid   = "ToolchainNotValid"
	ReasonToolchainResolveErr = "ToolchainResolveError"
)

// WorkspaceSource controller reasons.
const (
	ReasonWorkspaceSourceSpecOK            = "SpecOK"
	ReasonWorkspaceSourceSpecInvalid       = "SpecInvalid"
	ReasonWorkspaceSourceUnknownKind       = "UnknownDriverKind"
	ReasonWorkspaceSourceBaseUnconfigured  = "BaseStorageUnconfigured"
	ReasonWorkspaceSourceMaterializing     = "Materializing"
	ReasonWorkspaceSourceMaterialized      = "Materialized"
	ReasonWorkspaceSourceMaterializeFailed = "MaterializeFailed"
	// ReasonWorkspaceSourceRefreshing marks the Ready condition while a
	// scheduled base-refresh Job (spec.base.refresh) is in progress. The base
	// stays Ready=True/Refreshing rather than flipping unusable â the prior
	// checkout is still valid while the refresh Job runs.
	ReasonWorkspaceSourceRefreshing = "Refreshing"
	// ReasonWorkspaceSourceSpecChanged fires when the materialize Job already
	// succeeded for an older generation of the spec. The base checkout is
	// materialized exactly once (by stable Job name) and a spec edit does not
	// re-materialize it, so the controller reports the divergence honestly
	// instead of latching Ready=True for a generation it never materialized.
	ReasonWorkspaceSourceSpecChanged = "SpecChanged"
)

// WorkspaceSource has no deletion-blocking finalizer: the base PVC and
// materialize Job are owner-referenced by it, so Kubernetes GC tears them down
// on delete.

// WorkspaceSource binding reasons (AgentClass validation + AgentSession overlay).
const (
	ReasonAgentClassWorkspaceSourceMissing     = "WorkspaceSourceMissing"
	ReasonAgentClassWorkspaceSourceInvalid     = "WorkspaceSourceInvalid"
	ReasonAgentSessionWorkspaceSourceNotReady  = "WorkspaceSourceNotReady"
	ReasonAgentSessionWorkspaceOverlayFailed   = "WorkspaceOverlayFailed"
	ReasonAgentSessionWorkspaceStorageRequired = "WorkspaceStorageRequired"
)

// AgentUI binding reasons (AgentClass validation of spec.agentUI.ref).
// Mirrors the WorkspaceSource pair above: an AgentClass that opts into an
// AgentUI it cannot resolve, or one that is not itself Valid, parks at
// Valid=False rather than reporting AllReferencesResolve and failing later
// as an empty browser-tool surface.
const (
	ReasonAgentClassAgentUIMissing = "AgentUIMissing"
	ReasonAgentClassAgentUIInvalid = "AgentUIInvalid"
)

// AgentUI reasons (Valid condition).
const (
	// ReasonAgentUISpecOK marks the page — spec.view, or spec.slots compiled
	// — as having parsed and validated cleanly against the platform
	// vocabulary.
	ReasonAgentUISpecOK = "SpecOK"
	// ReasonAgentUIInvalidDefault marks a page — spec.view, or spec.slots
	// compiled — that failed either the strict wire parse (an unknown
	// structural key) or vocabulary validation (an unknown/duplicate/missing
	// hook name, an unknown component, an out-of-bounds tree, or a binding to
	// a tool outside the eligible-tools ceiling). The condition message
	// carries the *uicomponents.ValidationError's real Path and Reason, so the
	// failure is diagnosable without source-diving. A default that fails its
	// own parse is attributed to the CR's real index ("slots[1]"); everything
	// caught after compilation is attributed to the compiled tree
	// ("view.children[1]…"), which is where the node actually sits on the page
	// the runner serves.
	ReasonAgentUIInvalidDefault = "InvalidDefault"
	// ReasonAgentUIGrantUnresolved marks Valid=Unknown when the AgentClasses
	// in this namespace could not be listed to compute the deployment half
	// of the eligible-tools ceiling. Deliberately Unknown, not False: this
	// status is not itself an authorization decision, so persisting a
	// denial derived from an input this reconcile failed to read would be a
	// false diagnosis, not a safe default. status.eligibleTools is left
	// untouched (the last successfully-observed ceiling) rather than reset.
	ReasonAgentUIGrantUnresolved = "GrantUnresolved"
	// ReasonAgentUIActionToolNotRequested is the Valid=False reason for an
	// action naming a tool the UI never requested. Distinct from
	// ReasonAgentUIInvalidDefault because the fix is a different field
	// (spec.tools) than the declaration the author is looking at.
	ReasonAgentUIActionToolNotRequested = "ActionToolNotRequested"
)

// AgentUI reasons (ToolsGranted condition).
const (
	// ReasonAgentUIToolsFullyGranted marks ToolsGranted=True: every
	// (deduplicated) tool in spec.tools made the eligible-tools ceiling.
	ReasonAgentUIToolsFullyGranted = "AllGranted"
	// ReasonAgentUIToolsPartiallyGranted marks ToolsGranted=False: at least
	// one requested tool did not make the ceiling. The condition message
	// carries uigrant.ExplainCeiling's per-tool diagnosis.
	ReasonAgentUIToolsPartiallyGranted = "PartiallyGranted"
)

// Workshop condition types + reasons. Each names one provisioning layer so a
// failed layer is visible on the CR rather than only in operator logs.
const (
	WorkshopConditionNamespaceReady = "NamespaceReady"
	WorkshopConditionRBACReady      = "RBACReady"
	WorkshopConditionTupleWritten   = "TupleWritten"
	WorkshopConditionTokensReady    = "TokensReady"

	ReasonWorkshopProvisioned     = "Provisioned"
	ReasonWorkshopProvisionFailed = "ProvisionFailed"
	ReasonWorkshopExpired         = "Expired"
	// ReasonWorkshopSessionFinished marks the workshop released because its
	// builder session reached a terminal phase (and any install it requested
	// was decided) — the standing reason for a teardown nobody asked for, the
	// way ReasonWorkshopExpired is for the max-age sweeper.
	ReasonWorkshopSessionFinished = "SessionFinished"
)

// WorkshopProbe phases, condition type, and reasons.
const (
	WorkshopProbePhasePending   = "Pending"
	WorkshopProbePhaseRunning   = "Running"
	WorkshopProbePhaseSucceeded = "Succeeded"
	WorkshopProbePhaseFailed    = "Failed"

	WorkshopProbeConditionProbed = "Probed"

	ReasonWorkshopProbeSucceeded = "ProbeSucceeded"
	ReasonWorkshopProbeFailed    = "ProbeFailed"
	ReasonWorkshopProbeDenied    = "ProbeDenied" // tuple absent
)

// ReasonAgentSessionWorkshopLimitExceeded is the boot-failure reason when a
// starter already holds maxWorkshopsPerStarter live workshops. A resource
// limit, not a policy halt: it is NOT in policyHaltReasons, so a takeover of
// such a thread inherits normally.
const ReasonAgentSessionWorkshopLimitExceeded = "WorkshopLimitExceeded"

// FailureReasonWrittenForThePerson reports whether a Failed condition
// carrying this reason ALSO carries a message composed for the person who
// tried to start the session — plain words, and where possible a next step
// they can take.
//
// It names the boot-refusal reasons only, and it is an allowlist because it
// has to be: most Failed messages are written for an operator and name pods,
// CR kinds, relations and retry budgets. A browser surface that forwarded
// every Failed message would put that vocabulary in front of the very person
// the fixed refusal copy exists to protect.
//
// A reason is added here ONLY TOGETHER WITH ITS COPY — that is, only once the
// writer that fails the session passes a person-facing constant as the
// message — never because the reason string itself sounds user-facing. The
// two today, with the message each carries:
//
//   - ReasonAgentSessionWorkshopLimitExceeded → WorkshopLimitBody
//   - ReasonAgentSessionNotAnAllowedStarter   → startRefusedBody
//     (pkg/controllers/agentsession/start_gate.go), the fixed copy that
//     refuseStart passes to markBootFailed in place of its operator detail
//
// Deliberately absent: ReasonAgentSessionStartDenied and
// ReasonAgentSessionStartApprovalTimeout, whose messages are inline prose
// about the start-approval request rather than copy written for the person.
// Give either one a person-facing constant and it belongs here.
func FailureReasonWrittenForThePerson(reason string) bool {
	switch reason {
	case ReasonAgentSessionWorkshopLimitExceeded, ReasonAgentSessionNotAnAllowedStarter:
		return true
	default:
		return false
	}
}

// Builder-class validation reasons (spec §1.0's deferred set, enforced by the
// AgentClass controller when the class is sanctioned).
const (
	ReasonBuilderClassInvalid = "BuilderClassInvalid"
	// ReasonReferencesWorkshopTool marks a NON-workshop class that references
	// a workshop-labeled cluster-scoped toolspec/toolkit.
	ReasonReferencesWorkshopTool = "ReferencesWorkshopTool"
)

// RelationshipSource condition types.
const (
	// RelationshipSourceConditionReady is True when the last pass enumerated
	// (fully or partially) and synced without a fatal error. Mirrors
	// SkillSourceConditionReady's single-condition shape; see the CRD's own
	// "Ready" printcolumn.
	RelationshipSourceConditionReady = "Ready"
	// RelationshipSourceConditionPartialFailure is True when the last
	// COMPLETED pass finished with per-scope errors, and False when that pass
	// finished clean. It is the second half of a two-condition split that
	// exists because Ready alone could not express partial success.
	//
	// Ready keeps its meaning exactly: a pass that enumerated and synced
	// without a fatal error is Ready=True/Synced, and per-scope failures stay
	// non-fatal (relsync.Pass's own doc: "one scope failing is not a pass
	// failure"). That is correct, and it is also why a whole arm of a sync can
	// fail invisibly — a GitHub source whose every per-repository team fetch
	// answered 403 reported Ready=True, Synced, scopesProcessed: 156, with the
	// 403s reaching only the operator log. Nothing downstream could tell
	// partial success from success. This condition is that missing signal:
	// True here and Ready=True together mean "ran, and some of it failed".
	//
	// It is set on the SAME status write status.sync.lastPass's counts ride,
	// from the same PassResult, so the condition and the counts can never
	// disagree. A pass that never completed (a fatal error, a total
	// enumeration failure, a parked source) leaves it untouched: it describes
	// the last pass that actually finished, and inventing a verdict for a pass
	// that did not run would be a false diagnosis. Ready=False is the signal
	// for those.
	RelationshipSourceConditionPartialFailure = "PartialFailure"
)

// RelationshipSource reasons.
const (
	// ReasonRelationshipSourceSynced marks Ready=True after a pass completed
	// (fully or partially resumed) with no fatal error. Per-scope failures
	// (PassResult.ScopeErrors) are logged, not fatal — see relsync.Pass's own
	// doc on "one scope failing is not a pass failure". Whether this pass had
	// any is reported by RelationshipSourceConditionPartialFailure, not here.
	ReasonRelationshipSourceSynced = "Synced"
	// ReasonRelationshipSourceScopeErrors marks PartialFailure=True: the last
	// completed pass reported at least one PassResult.ScopeError. The
	// condition message carries the failure count and one sampled error; the
	// bounded sample set is on status.sync.lastPass.scopeErrorSamples.
	ReasonRelationshipSourceScopeErrors = "ScopeErrors"
	// ReasonRelationshipSourceAllScopesSynced marks PartialFailure=False: the
	// last completed pass reported no scope errors at all.
	ReasonRelationshipSourceAllScopesSynced = "AllScopesSynced"
	// ReasonRelationshipSourceKindUnregistered marks Ready=False when
	// spec.kind names no relsync.Kind registered in this binary.
	ReasonRelationshipSourceKindUnregistered = "KindUnregistered"
	// ReasonRelationshipSourceKindClaimed marks Ready=False when another
	// RelationshipSource already claims spec.kind — see the package doc on
	// why two CRs of the same kind must never both sync (the reap-vs-reap
	// annihilation case).
	ReasonRelationshipSourceKindClaimed = "KindClaimed"
	// ReasonRelationshipSourceAuthResolveFailed marks Ready=False when
	// spec.auth's AgentIdentity/credential/Secret cannot be resolved.
	ReasonRelationshipSourceAuthResolveFailed = "AuthResolutionFailed"
	// ReasonRelationshipSourcePassFailed marks Ready=False if relsync.Pass
	// itself ever returns a non-nil error — defensive, not reachable in the
	// current implementation. relsync.Pass's own doc says its error return
	// is reserved for a failure the algorithm cannot even attempt to
	// recover from, and today it always returns nil: every anticipated
	// failure (enumeration, a scope's fetch/write, the reap scan) folds
	// into PassResult.ScopeErrors instead — see
	// ReasonRelationshipSourceEnumerationFailed below for the "enumeration
	// produced nothing" case that reason exists to catch. Kept as the
	// defensive branch it is, not because it fires today.
	ReasonRelationshipSourcePassFailed = "PassFailed"
	// ReasonRelationshipSourceEnumerationFailed marks Ready=False when a pass
	// enumerated NOTHING and processed NOTHING — EnumComplete=false with zero
	// scopes visited. "One scope failing is not a pass failure" (Synced,
	// above) does not extend to a total enumeration failure (a revoked
	// token, missing_scope, a network outage): that pass wrote nothing, and
	// without this reason it folds into ScopeErrors while Ready still reads
	// True/Synced, so `kubectl get relationshipsources` shows a healthy
	// source that has never written a tuple. A pass that visited at least
	// one scope (a budget-truncated but otherwise productive cycle) still
	// reports Synced — see PassResult.Processed.
	ReasonRelationshipSourceEnumerationFailed = "EnumerationFailed"
)
