package v1alpha1

import (
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Workshop is one builder session's isolated build space: the durable record
// of its namespace, RBAC, SpiceDB tuple, tokens and sidecar identity, created
// by the AgentSession reconciler ONLY for a sanctioned class (spec §1.1) and
// owned end-to-end by the workshop controller. A restarted operator
// re-provisions from this CR; the webhook attributes SA → session → workshop
// by reading it; the sweeper enforces its limits without the session's
// cooperation.
//
// SECURITY: status is the trust boundary. status.sidecarIdentity is the ONLY
// source the sidecar-pod builder consumes — never AgentSession.status, which
// the runner holds patch on. spec.session, spec.starterCanonical and
// spec.limits are immutable after creation (CEL below): the sidecar SA holds
// update on this one object for the later-plan request fields, and must not
// be able to re-point the workshop or raise its own limits.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=wksp
// +kubebuilder:printcolumn:name="Session",type=string,JSONPath=`.spec.session.name`
// +kubebuilder:printcolumn:name="Workspace",type=string,JSONPath=`.status.namespace`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Workshop struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkshopSpec   `json:"spec"`
	Status WorkshopStatus `json:"status,omitempty"`
}

type WorkshopSpec struct {
	// Session is the builder AgentSession this workshop belongs to, in the
	// same namespace as this CR. Set once by the AgentSession reconciler.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.session is immutable"
	Session NamespacedRef `json:"session"`

	// StarterCanonical is the canonical id of the person who started the
	// session ("user:" prefix stripped), recorded so maxWorkshopsPerStarter
	// can count without reading every session. Set once.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.starterCanonical is immutable"
	// +optional
	StarterCanonical string `json:"starterCanonical,omitempty"`

	// SidecarToolbox is the name of the sanctioned SidecarToolbox CR (in the
	// class's namespace) that receives this workshop's identity — copied from
	// the BuilderClassRef at creation. The AgentSession reconciler consumes it
	// (workshopIdentityFor) to decide WHICH sidecar of the class gets the
	// projected SA token. Set once.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.sidecarToolbox is immutable"
	SidecarToolbox string `json:"sidecarToolbox"`

	// Limits are copied from ClusterAgentSettings at creation and are
	// immutable: the sidecar SA holds update on this object for the request
	// fields below and must not be able to raise its own ceilings.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.limits is immutable"
	Limits WorkshopLimits `json:"limits"`

	// ExportRequested, InstallRequest and CapabilityRequest are written by the
	// workshop sidecar in later plans (export/install/handoff flows). This
	// plan's controller ignores them; they exist now so the CRD does not churn.
	// +optional
	ExportRequested bool `json:"exportRequested,omitempty"`
	// +optional
	InstallRequest *WorkshopInstallRequest `json:"installRequest,omitempty"`
	// +optional
	CapabilityRequest *WorkshopCapabilityRequest `json:"capabilityRequest,omitempty"`

	// CredentialRequests are the bot/shared credentials the builder has asked a
	// person to connect for AgentIdentities it authored in this workshop (plan
	// 5a). Written by the sidecar's request_credential tool via its update grant
	// on this CR. A pure function of (identity, credential): a re-request of the
	// same pair is byte-identical, so this is SSA-idempotent. AuthKind is
	// pat|static (a pasted secret) or oauth-mcp (a shared/bot OAuth connect the
	// person authorizes through identityd).
	// +optional
	// +listType=atomic
	CredentialRequests []WorkshopCredentialRequest `json:"credentialRequests,omitempty"`

	// CloseRequests are the OTHER workshops this workshop's builder has been
	// asked to end, written by the sidecar's close_others tool through its
	// update grant on this CR. Each Target is a Workshop — bare in this same
	// (builder) namespace, `namespace/name` for one elsewhere — or a wildcard
	// ask the controller expands: `*` the first time this workshop asks to
	// close every other one, and `*:<n>` for each later ask, so a person may
	// ask again and have it expanded afresh. Either way a target is requested
	// at most once under that exact spelling, since the list is keyed by
	// target.
	//
	// A request is only ever a REQUEST: the Workshop controller decides each
	// against SpiceDB's workshop#close for the person this workshop's builder
	// acts for (spec.starterCanonical) and records the outcome once in
	// status.closeRequests. Nothing here authorizes anything.
	// +optional
	// +listType=map
	// +listMapKey=target
	CloseRequests []WorkshopCloseRequest `json:"closeRequests,omitempty"`

	// TestWatch is the sidecar's request to watch the person's own test of a
	// built class (Try it as yourself). See WorkshopTestWatch.
	// +optional
	TestWatch *WorkshopTestWatch `json:"testWatch,omitempty"`
}

type WorkshopLimits struct {
	// MaxAge bounds the workshop's total life from provisionedAt, regardless
	// of session state. The sweeper deletes past it.
	MaxAge metav1.Duration `json:"maxAge"`
	// +kubebuilder:validation:Minimum=1
	MaxObjectsPerKind int32 `json:"maxObjectsPerKind"`
	// +kubebuilder:validation:Minimum=1
	MaxObjects int32 `json:"maxObjects"`
	// MaxConcurrentProbes must be at least 1: zero is never a valid config —
	// it would deadlock every WorkshopProbe in the namespace permanently
	// (workshopprobe.Reconciler's concurrency-cap check has no slot to ever
	// grant). The default is 2 (defaultWorkshopLimits,
	// pkg/controllers/agentsession/workshop_hook.go).
	// +kubebuilder:validation:Minimum=1
	MaxConcurrentProbes int32 `json:"maxConcurrentProbes"`
}

// WorkshopInstallRequest and WorkshopCapabilityRequest are the later-plan
// sidecar-written requests (spec §2.1). Digest/refs only — never a secret.
type WorkshopInstallRequest struct {
	SuggestedName string `json:"suggestedName"`
	BundleDigest  string `json:"bundleDigest"`
	// +optional
	Answers map[string]string `json:"answers,omitempty"`
}

type WorkshopCapabilityRequest struct {
	Summary     string `json:"summary"`
	ArtifactRef string `json:"artifactRef"`
	// +optional
	DraftRef string `json:"draftRef,omitempty"`
}

// WorkshopCredentialRequest is one bot/shared credential the builder asked a
// person to connect (plan 5a §6). Refs and names only — never a secret value.
type WorkshopCredentialRequest struct {
	// Identity is the AgentIdentity (in the workshop namespace W) the credential
	// belongs to.
	// +kubebuilder:validation:MinLength=1
	Identity string `json:"identity"`
	// Credential is the credential name on that AgentIdentity.
	// +kubebuilder:validation:MinLength=1
	Credential string `json:"credential"`
	// AuthKind is pat, static, or oauth-mcp.
	// +kubebuilder:validation:Enum=pat;static;oauth-mcp
	AuthKind string `json:"authKind"`
}

// WorkshopCloseRequest asks the operator to end ANOTHER workshop of the same
// person (or anyone's, when the requester is a platform admin). Written by the
// sidecar's close_others tool onto the REQUESTING workshop's spec; decided by
// the Workshop controller against SpiceDB workshop#close and recorded in
// status, once per target. The workshop a builder is IN is never a valid
// target: the tool refuses it before writing, and the controller refuses it
// again.
//
// RequestedAt is a wall-clock value in a spec field, which the repo's SSA rule
// otherwise warns against — it is safe here for the same reason
// WorkshopTestWatch.StartedAt is: the tool writes an entry ONCE per target with
// a plain Update, never a repeated apply, and the list is keyed by target so
// there is no second write of the same entry to be non-idempotent about. The
// controller's own observations live in status.closeRequests, never here.
type WorkshopCloseRequest struct {
	// Target is a Workshop: its name in the requester's builder namespace,
	// `namespace/name` for one elsewhere, `*` for every other workshop of the
	// same person, `*:<n>` for a later such ask.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=127
	// +kubebuilder:validation:Pattern=`^(\*(:[1-9][0-9]*)?|([a-z0-9]([-a-z0-9]*[a-z0-9])?/)?[a-z0-9]([-a-z0-9]*[a-z0-9])?)$`
	Target string `json:"target"`
	// RequestedAt is when the tool asked.
	RequestedAt metav1.Time `json:"requestedAt"`
}

// WorkshopCloseStatus is the Workshop controller's decision for one
// spec.closeRequests entry. Written once per CANONICAL target — the
// namespace and name the spelling resolves to — and never revised: the close
// it records has already happened (the target's builder session is deleted),
// so re-deciding could only reach through to a LATER session that happens to
// carry the same name.
//
// Two spellings of one workshop ("x" and "<builder namespace>/x") are one
// canonical target, so the second is answered by copying the first's decision
// into an entry of its own: a second entry, never a second close.
//
// A wildcard ask ("*", "*:<n>") is answered in an entry of its own too — its
// summary of what THAT ask decided — and every workshop the ask decided carries
// the ask in Ask. A later ask reads that to skip what is already settled, and
// the tool reads it to report the answer to its own call.
type WorkshopCloseStatus struct {
	// Target echoes the spec request's target, and is this list's key.
	Target string `json:"target"`
	// Phase is Closed, Refused, or NotFound.
	// +kubebuilder:validation:Enum=Closed;Refused;NotFound
	Phase string `json:"phase"`
	// Message is the plain-words why, for a decision that was not a close.
	// +optional
	Message string `json:"message,omitempty"`
	// Ask is the wildcard request this entry was decided for; empty for a
	// named request. An entry decided before the field existed carries none
	// and reads as a named decision; deploy the CRD and operator before the
	// workshop image.
	// +optional
	Ask string `json:"ask,omitempty"`
	// DecidedAt is when the controller decided.
	// +optional
	DecidedAt *metav1.Time `json:"decidedAt,omitempty"`
}

// WorkshopTestWatch asks the Workshop controller to watch for the person's own
// test of one class authored in this workshop, and to wake the builder as it
// starts, pauses, resumes, ends, or times out. Written by the sidecar's
// watch_test tool through its update grant on this CR — a plain Update, not an
// SSA apply.
//
// (Class, StartedAt) is the watch's IDENTITY: the controller keys its own
// delivered-events record on the pair (testwatch.go's reconcileTestWatch) and
// ignores sessions created before StartedAt. So StartedAt is volatile, not
// idempotent — a watch_test call for a new test stamps the current time and
// begins a new record — with one deliberate exception: a call for the same
// class while its recorded test is still running KEEPS the pair and moves
// only Deadline, because a new StartedAt there would arm a watch that could
// never see the test already under way (handleWatchTest's
// watchStillCoversItsTest, pkg/tools/workshopmcp/tools_trytest.go).
//
// The observations this watch produces live in status.testWatch, never here.
type WorkshopTestWatch struct {
	// Class is the AgentClass (in the workshop namespace) whose sessions count.
	// +kubebuilder:validation:MinLength=1
	Class string `json:"class"`
	// StartedAt is when the watch began; only sessions created after it are
	// the person's test.
	StartedAt metav1.Time `json:"startedAt"`
	// Deadline is when the watch times out.
	Deadline metav1.Time `json:"deadline"`
}

// WorkshopTestWatchStatus is the controller's record of a spec.testWatch.
type WorkshopTestWatchStatus struct {
	// Class echoes the spec.testWatch.class this status describes, so a
	// replaced watch (a different class or startedAt) starts a fresh record.
	Class string `json:"class"`
	// StartedAt echoes spec.testWatch.startedAt for the same reason.
	StartedAt metav1.Time `json:"startedAt"`
	// Session is the test session, once one qualified.
	// +optional
	Session string `json:"session,omitempty"`
	// Delivered lists the events the builder has been told about, in order:
	// started, paused, resumed, ended, timedOut. Each is delivered at most
	// once, except paused and resumed, which repeat each time the session
	// pauses and comes back. A re-armed watch that kept its identity drops
	// timedOut so the rest of that test can still be reported.
	// +optional
	// +listType=atomic
	Delivered []string `json:"delivered,omitempty"`
	// LastPhase is the test session's phase at the last delivery, so a repeat
	// of the same phase delivers nothing.
	// +optional
	LastPhase string `json:"lastPhase,omitempty"`
}

type WorkshopStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Namespace is the provisioned workshop namespace (ws-<uid12>).
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Phase is Provisioning, Ready, Expired or Deleting.
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	ProvisionedAt *metav1.Time `json:"provisionedAt,omitempty"`
	// SidecarIdentity is the identity the AgentSession reconciler hands the
	// workshop sidecar pod: run as this ServiceAccount (projected token) and
	// envFrom this Secret (the workshop bearer). Controller-owned status on an
	// object the runner cannot touch — the ONLY source BuildSidecarPod
	// consumes (spec §13).
	// +optional
	SidecarIdentity *WorkshopSidecarIdentity `json:"sidecarIdentity,omitempty"`
	// Export records the most recently stored drafted .oap bundle for this
	// workshop. Written by the tuple-authorized draft-export operator route
	// (pkg/web/workshopdraftsrv) immediately after it durably stores the
	// bytes under the builder session's scope — never by the sidecar, which
	// holds no update on this object's status subresource. Set-once per
	// digest: a re-export whose content is byte-identical to the last one
	// leaves this field (and ExportedAt) untouched, so a retry is not
	// volatile churn on a controller-owned field.
	// +optional
	Export *WorkshopExport `json:"export,omitempty"`
	// Standins is the AUTHORITATIVE registry of every credential-free
	// rehearsal stand-in AgentClass this workshop has projected (agent-builder
	// plan 9b, Task 1; pkg/web/workshopprojectsrv), written by the
	// tuple-authorized project-agent operator route immediately after it
	// creates or updates the stand-in AgentClass — never by the sidecar,
	// which holds no update on this object's status subresource (same
	// division of labor as Export, above).
	//
	// This is the fix for a real hole: the stand-in AgentClass itself also
	// carries AnnotationStandinSource, but that annotation is a
	// human-readable label ONLY and must NEVER be trusted as a security
	// decision — a builder's own workshop_apply tool server-side-applies
	// arbitrary metadata.annotations into the workshop namespace, and the
	// admission webhook never inspects them, so a builder (or a
	// prompt-injected model driving one) can stamp the marker onto a class
	// it authored itself. This list is what the project-agent route's
	// collision check and soleAuthoredClassName's authored-count now consult
	// instead: a bare name is a stand-in for {SourceNamespace, SourceName}
	// iff an entry here says so, never because some object merely carries
	// the annotation. This is this repo's SSA discipline applied literally
	// (observations belong in controller-owned status, never a
	// client-writable field) — see CLAUDE.md's "Server-side apply" section.
	//
	// Set-once per (Name, SourceNamespace, SourceName): re-projecting the
	// same source is a no-op here. No timestamp, unlike Export/ExportedAt
	// above: an entry carries no field that a retry could ever leave stale
	// (Export's ExportedAt exists to record WHEN a digest was first seen;
	// a stand-in entry has no analogous volatile fact — the same three
	// fields are true for as long as the entry exists at all), so there is
	// nothing here for the SSA-churn rule to protect against.
	//
	// Bounded at spec.limits.maxObjectsPerKind entries: the project-agent
	// route writes as the operator, so it bypasses the admission webhook's
	// checkLimits (which only ever fires for a write attributed to the
	// sidecar's own SA) — capping this list at the SAME per-kind ceiling a
	// sidecar-authored AgentClass would have been held to is what keeps a
	// workshop's stand-in count from becoming an unbounded mint through this
	// one route (see pkg/web/workshopprojectsrv's own package doc).
	// +optional
	// +listType=atomic
	Standins []WorkshopStandin `json:"standins,omitempty"`
	// Install is the observed state of spec.installRequest. status.install.phase
	// (Requested→Approved/Installed or Declined/Failed) is written by TWO owners
	// with disjoint fields: the channelsd WorkshopHandoffWatcher sets Requested +
	// DeliveredAt when it delivers the admin card; admind sets Installed/Declined/
	// Failed + ApprovedBy + InstalledRef on the admin's click. Volatile values here.
	// +optional
	Install *WorkshopInstallStatus `json:"install,omitempty"`
	// CapabilityRequest is the observed delivery state of spec.capabilityRequest,
	// written SOLELY by the WorkshopHandoffWatcher.
	// +optional
	CapabilityRequest *WorkshopCapabilityRequestStatus `json:"capabilityRequest,omitempty"`
	// CredentialRequests mirrors spec.credentialRequests with the observed
	// delivery state. Written SOLELY by the channelsd WorkshopCredentialWatcher
	// (deliveredAt/noticeRef) — a single writer, so the atomic list cannot
	// clobber. Volatile values live HERE, never in the applied spec.
	// +optional
	// +listType=atomic
	CredentialRequests []WorkshopCredentialRequestStatus `json:"credentialRequests,omitempty"`
	// CloseRequests is the Workshop controller's decision for each
	// spec.closeRequests entry — Closed, Refused or NotFound — written SOLELY
	// by that controller, one entry per requested spelling, SET ONCE. A
	// decision already recorded for a target is what stops it being decided
	// (and closed) a second time under any spelling, so nothing here is ever
	// rewritten.
	// +optional
	// +listType=map
	// +listMapKey=target
	CloseRequests []WorkshopCloseStatus `json:"closeRequests,omitempty"`
	// TestWatch is the observed state of spec.testWatch, written SOLELY by the
	// Workshop controller. Set-on-change: each delivered event is appended
	// once; a replaced watch resets it.
	// +optional
	TestWatch *WorkshopTestWatchStatus `json:"testWatch,omitempty"`
	// +optional
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

type WorkshopSidecarIdentity struct {
	ServiceAccount string `json:"serviceAccount"`
	TokenSecret    string `json:"tokenSecret"`
}

// WorkshopExport is the durable record of the workshop's most recently
// exported drafted .oap bundle.
type WorkshopExport struct {
	// ArtifactRef is the opaque artifactstore reference the bundle bytes are
	// stored under, scoped to the builder session (spec.session) — never the
	// workshop namespace.
	ArtifactRef string `json:"artifactRef"`
	// Digest is the sha256 (hex-encoded) of the stored bytes.
	Digest string `json:"digest"`
	// ExportedAt is when this digest was first recorded.
	ExportedAt metav1.Time `json:"exportedAt"`
}

// WorkshopStandin is one authoritative registry entry naming a
// credential-free rehearsal stand-in AgentClass this workshop has projected
// (see WorkshopStatus.Standins' own doc for why this list, not the stand-in
// object's own annotation, is the security boundary).
type WorkshopStandin struct {
	// Name is the stand-in's own metadata.name — bare, per the projection
	// route's Ruling C, so it can collide with a bare name the workshop's
	// builder authored itself; that collision is exactly what this registry
	// exists to adjudicate.
	Name string `json:"name"`
	// SourceNamespace/SourceName are the foreign AgentClass this stand-in
	// stands in for — the same {namespace, name} pair the project-agent
	// route's reachability check verified before projecting.
	SourceNamespace string `json:"sourceNamespace"`
	SourceName      string `json:"sourceName"`
}

// WorkshopCredentialRequestStatus is the observed delivery state for one
// entry in spec.credentialRequests (plan 5a §6). Identity/Credential key it
// back to its spec request; DeliveredAt/NoticeRef are set by the
// WorkshopCredentialWatcher when it DELIVERS the credential card to the builder
// session (NoticeRef is that interaction's request id). A non-nil DeliveredAt is
// the dedup marker that stops re-delivery — it records card delivery, not that
// the person has connected the credential (Secret existence is read directly).
type WorkshopCredentialRequestStatus struct {
	Identity   string `json:"identity"`
	Credential string `json:"credential"`
	// +optional
	DeliveredAt *metav1.Time `json:"deliveredAt,omitempty"`
	// +optional
	NoticeRef string `json:"noticeRef,omitempty"`
}

type WorkshopInstallStatus struct {
	// Phase is Requested, Approved, Installed, Declined, or Failed.
	// +kubebuilder:validation:Enum=Requested;Approved;Installed;Declined;Failed
	Phase string `json:"phase"`
	// +optional
	RequestedAt *metav1.Time `json:"requestedAt,omitempty"`
	// +optional
	DeliveredAt *metav1.Time `json:"deliveredAt,omitempty"`
	// ApprovedBy is the canonical id of the admin who approved/declined. Set by admind.
	// +optional
	ApprovedBy string `json:"approvedBy,omitempty"`
	// InstalledRef is "<namespace>/<name>" of the installed AgentClass on success.
	// +optional
	InstalledRef string `json:"installedRef,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

type WorkshopCapabilityRequestStatus struct {
	// +optional
	DeliveredAt *metav1.Time `json:"deliveredAt,omitempty"`
	// +optional
	NoticeRef string `json:"noticeRef,omitempty"`
}

// +kubebuilder:object:root=true
type WorkshopList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workshop `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Workshop{}, &WorkshopList{})
}

// Workshop phases. Released is the terminal phase for a workshop whose builder
// session finished (and whose install request, if it made one, has been
// decided) — recorded just before the CR is deleted, so the ordinary finalizer
// teardown runs and the workshop stops counting against the starter's cap.
const (
	WorkshopPhaseProvisioning = "Provisioning"
	WorkshopPhaseReady        = "Ready"
	WorkshopPhaseExpired      = "Expired"
	WorkshopPhaseReleased     = "Released"
	WorkshopPhaseDeleting     = "Deleting"
)

// WorkshopCloseStatus.Phase values, matching WorkshopCloseStatus.Phase's
// +kubebuilder:validation:Enum exactly. Written only by the Workshop
// controller's close pass (pkg/controllers/workshop/close.go).
const (
	WorkshopClosePhaseClosed   = "Closed"
	WorkshopClosePhaseRefused  = "Refused"
	WorkshopClosePhaseNotFound = "NotFound"
)

// WorkshopCloseTargetAll is the WorkshopCloseRequest.Target that means "every
// other live workshop of this workshop's own starter".
//
// It exists because the two halves of a close cannot see the same thing. The
// sidecar's Role is name-restricted to its OWN Workshop (get/update/patch), so
// the tool that writes a close request cannot list the person's workshops and
// has no way to name them. The Workshop controller can, so a wildcard is the
// only form "close all of mine" can take: the tool writes this target for a
// workshop's FIRST such ask (and "*:<n>" for each later one, which is how a
// person asks again), and the controller resolves it against the workshops it
// can see, records one ordinary decision per resolved target it may decide, and
// answers the ask itself with a summary of what that ask decided.
const WorkshopCloseTargetAll = "*"

// workshopCloseWildcard matches the wildcard spellings of a close target:
// WorkshopCloseTargetAll, and the sequenced "*:<n>" a later ask carries. It is
// the wildcard half of WorkshopCloseRequest.Target's own CRD pattern, kept
// here so there is ONE copy of it: the close pass reads a target with it to
// decide whether to expand, and the close_others tool to decide what key its
// next ask may take. Two hand-written copies is how a target gets admitted as
// an ask and then read as a workshop name.
//
// A regexp rather than a parse: a run of digits too long for an int is still
// not a workshop name, and reading it as one would send the close pass to the
// apiserver for a name it can never hold.
var workshopCloseWildcard = regexp.MustCompile(`^\*(:[1-9][0-9]*)?$`)

// IsWorkshopCloseWildcard reports whether a WorkshopCloseRequest target is a
// wildcard ask — "every other live workshop of this workshop's own starter" —
// rather than a Workshop the request names.
func IsWorkshopCloseWildcard(target string) bool {
	return workshopCloseWildcard.MatchString(target)
}

// WorkshopInstallStatus.Phase values, matching WorkshopInstallStatus.Phase's
// +kubebuilder:validation:Enum exactly. The channelsd WorkshopHandoffWatcher
// only ever writes Requested (on card delivery); admind writes
// Installed/Declined/Failed on the admin's click. Approved is reserved for a
// future two-step approve→install flow and is not written today.
const (
	WorkshopInstallPhaseRequested = "Requested"
	WorkshopInstallPhaseApproved  = "Approved"
	WorkshopInstallPhaseInstalled = "Installed"
	WorkshopInstallPhaseDeclined  = "Declined"
	WorkshopInstallPhaseFailed    = "Failed"
)

// Labels stamped on the workshop NAMESPACE (session attribution) and on the
// cluster-scoped tool CRs a workshop authors (reaping + the outside-reference
// refusal). Values are namespace/name segments, all label-safe.
const (
	LabelWorkshopSessionNamespace = "agentprimitives.authzed.com/workshop-session-namespace"
	LabelWorkshopSessionName      = "agentprimitives.authzed.com/workshop-session-name"
	LabelWorkshopNamespace        = "agentprimitives.authzed.com/workshop-namespace"
)

// AnnotationStandinSource marks an AgentClass as a credential-free REHEARSAL
// stand-in projected for another agent (agent-builder plan 9b, Task 1;
// pkg/web/workshopprojectsrv), rather than something a workshop's builder
// session authored itself. Value is "<namespace>/<name>" of the source class
// the reachability check verified — WHEN the annotation is genuine.
//
// SECURITY: this annotation is a HUMAN-READABLE LABEL ONLY and MUST NEVER be
// trusted as the basis for a security decision. A workshop's builder session
// can server-side-apply arbitrary metadata.annotations into its own
// namespace via its workshop_apply tool, and the admission webhook's
// checkAgentClass never inspects annotations — so a builder (or a
// prompt-injected model driving one) can stamp this marker onto a class it
// authored itself. The authoritative record is Workshop.status.standins
// (WorkshopStatus.Standins, this same file) — an operator-owned status field
// a builder's SA holds no update on — which is what
// pkg/web/workshopprojectsrv's collision check and
// pkg/tools/workshopmcp's soleAuthoredClassName both consult instead. This
// annotation still gets stamped, purely so a human reading `kubectl get
// agentclass -o yaml` can see at a glance what a bare name stands in for.
//
// Declared here (pkg/apis, not the route's own package) because it is a
// cross-package marker: pkg/web/workshopprojectsrv stamps it when it
// projects a stand-in, and a human (or a debugging session) reading either
// package's output benefits from the same constant name.
const AnnotationStandinSource = "agentprimitives.authzed.com/standin-source"

// WorkshopLimitBody is what the person who tried to start a builder workshop
// reads when they are already at the per-starter cap. Plain words only — see
// refuseStart's identical rule in pkg/controllers/agentsession/start_gate.go.
//
// Declared here rather than in either refusing package because TWO gates say
// it: the browser start route refuses the request outright (it names the
// person's open workshops after this sentence), and the operator's
// ensureWorkshop boot-fails a session that reached it anyway. Two copies of
// one sentence is how the two come to word the same refusal differently.
const WorkshopLimitBody = "You already have the maximum number of builder workshops open at once. Finish or close one before starting another."

// WorkshopName is the Workshop CR's name for a session: deterministic, so the
// runner Role can pin it by resourceName and a restarted reconciler finds it.
func WorkshopName(session string) string { return session + "-workshop" }

// WorkshopServiceAccountName is the sidecar's ServiceAccount in the SESSION's
// namespace. The "-workshop-sa" suffix is load-bearing: the workshop webhook
// attributes it to the session the same way "-runner-sa" is attributed.
func WorkshopServiceAccountName(session string) string { return session + "-workshop-sa" }

// WorkshopTokenSecretName is the per-session Secret holding the workshop
// bearer (key "token"). Covered by PerSessionSecretSuffixes ownership guards.
func WorkshopTokenSecretName(session string) string { return session + WorkshopTokenSecretSuffix }

// WorkshopNamespaceName derives the workshop namespace from the session UID:
// "ws-" + the first 12 hex characters (hyphens stripped). Short, DNS-safe, and
// collision-free for the lifetime that matters (a UID is unique per session
// object; a session re-created under the same name gets a new UID and so a
// fresh, empty workshop — never a predecessor's leftovers).
func WorkshopNamespaceName(uid types.UID) string {
	hex := strings.ReplaceAll(string(uid), "-", "")
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return "ws-" + hex
}
