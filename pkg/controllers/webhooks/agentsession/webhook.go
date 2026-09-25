// Package agentsession holds the AgentSession identity-pinning admission
// webhook.
//
// A session's runner pod runs with a ServiceAccount whose Role grants `patch`
// on its own AgentSession AND on that session's `/status` subresource
// (pkg/controllers/agentsession/rbac.go). Kubernetes RBAC has no field-level
// granularity, so those two verbs cover every field of both — including the
// five surfaces the operator and channelsd treat as authority:
//
//   - metadata.annotations. The worst of them is started-by-canonical-id (+
//     the external-id and email siblings): the AgentSession reconciler re-reads
//     these on EVERY reconcile to decide whose credentials to project — it
//     resolves that subject's UserIdentity and applies
//     BuildPassthroughSecretRBAC, a Role granting this session's runner SA
//     get+update on the named user's OAuth master Secrets. Rewriting the
//     annotation to another human therefore hands the runner that human's
//     upstream credentials.
//   - metadata.labels. Labels on an AgentSession are correlation keys OTHER
//     components select on, which makes them cross-session handles in a way an
//     annotation is not. channelsd's inbound lookup lists sessions by
//     LabelChannelName + LabelChannelKey and delivers the thread to the newest
//     match; both values derive from a channel id and a thread timestamp that
//     everyone in the channel can read. A runner that repoints them at another
//     human's thread takes over that thread's delivery: the interact check
//     denies, and the permission_request that denial publishes carries a
//     preview of the victim's message text and their identity into the
//     ATTACKER's channel, while the victim's own agent never answers.
//   - the status fields another component authors, enumerated in
//     pinnedStatusFields. "Another component" is the criterion, not "the
//     operator": appliedInteractPermission is channelsd's, and the runner
//     holding `patch` on it is the same escalation as holding the operator's.
//     status.pendingRestart: in `takeover` mode the restart reconciler
//     deliberately SKIPS the SpiceDB SessionFork gate — channelsd is the
//     authorization choke point, because a takeover is by definition performed
//     by a different user and the parent's agentsession#fork relation cannot
//     hold for them. Nothing downstream re-verifies that channelsd, and not
//     the runner, authored the marker; restart_decide.go then stamps
//     pendingRestart.triggeredBy onto the CHILD session's
//     started-by-canonical-id, producing a session running as the named victim
//     in the attacker's own thread. status.effectiveIdentityMode reaches the
//     same credential projection as the starter annotations. The three
//     status.audit* fields are the audit log's trust root.
//     status.appliedInteractPermission becomes a SpiceDB grant on ANOTHER
//     session: the same takeover path copies the parent's value onto the
//     child as agentsession:<child>#participant, and participant is a term of
//     `interact`, which memory_entry#read derives from — so a forged
//     subject-set buys the runner read of a different human's session and of
//     the transcript that fork inherited.
//   - the REST of metadata, which decides whether this object can be deleted
//     and by what. metadata.finalizers: adding one wedges the session
//     Terminating forever, since the operator removes only its own; removing
//     the operator's skips finalize, and finalize is the only thing that
//     revokes the session's memory bearer token. metadata.ownerReferences: no
//     component sets one, and a forged reference to a non-existent (or
//     cross-namespace, which is accepted and then read as absent) owner makes
//     the garbage collector delete the session — a deletion the runner Role
//     grants no verb for.
//   - spec. The runner never writes it, but `patch` covers it; repointing
//     spec.class at a class bound to a different AgentIdentity is the same
//     credential-projection escalation by another route.
//
// The status half is a TABLE, not a chain of comparisons, because its failure
// mode is OMISSION: guarding pendingRestart while leaving effectiveIdentityMode
// — which reaches the identical Secret projection — unguarded is the exact shape
// of the mistake, and a later sweep to fix that still missed supersededBy.
//
// A table alone does not fix omission, only makes it cheap to fix. What closes
// it is that pinnedStatusFields and runnerWritableStatusFields together
// PARTITION AgentSessionStatus: TestEveryStatusFieldIsPinnedOrWaived reflects
// over the type and fails the unit suite for any json field classified in
// neither or both. A new status field is therefore not "someone should remember
// to add a row" — it is a red test naming the field, with both choices spelled
// out and a waiver that must carry a written reason.
//
// Metadata gets the SAME discipline, spelled the other way round. A struct can
// be partitioned by reflection; an open key space cannot, so annotations and
// labels are gated by ALLOWLIST — runnerWritableAnnotations (two keys) and
// runnerWritableLabels (empty by design) — and every other key is refused. That
// is what makes the metadata half exhaustive without a hand-maintained pin list
// to keep in sync: a key nobody anticipated is denied by construction, so
// omission fails closed and loudly (the runner sees the admission error) rather
// than silently. metadata.labels was ungated for exactly as long as the
// metadata half was a three-element denylist.
//
// The REST of ObjectMeta is a closed struct like the status type, so it gets
// the status treatment rather than a third allowlist: pinnedMetadataFields,
// allowlistGatedMetadataFields (the two open key spaces, naming the map that
// gates each) and runnerWritableMetadataFields partition metav1.ObjectMeta, and
// TestEveryObjectMetaFieldIsPinnedOrWaived fails the unit suite for any field
// in none or several. That is what keeps this from being a fourth list to
// remember: a field can only be left ungated by writing down why, and most of
// the waivers are "the API server itself refuses this change", a checkable
// claim about apimachinery rather than an assumption.
//
// This webhook is the author check the API server cannot express: it refuses
// those writes when — and only when — the requester is a session runner
// ServiceAccount. Everything the runner Role legitimately exists for (the
// wake-requested-at annotation, the sidecar-failure annotation, status.phase
// and the rest of lifecycle reporting) is untouched.
//
// Why not CEL. A CRD structural schema may only describe `name` and
// `generateName` under `metadata`, so no x-kubernetes-validations rule can
// observe annotations at all. And CRD validation rules are identity-blind —
// `self`/`oldSelf` carry no requester — while status.pendingRestart has two
// legitimate writers (channelsd sets it, the operator clears it), so any
// transition rule strong enough to stop the runner also stops channelsd.
//
// Why failurePolicy: Fail. The ValidatingWebhookConfiguration narrows this
// webhook with `matchConditions` to requests from `*-runner-sa` service
// accounts. matchConditions are evaluated inside the API server BEFORE the
// webhook is dialed, so a non-matching request is never sent and failurePolicy
// never applies to it: a webhook outage cannot block the operator's own status
// writes, channelsd, webd, or a human with kubectl. Ignore is not an option for
// a gate whose whole job is refusing a forgery — an attacker who can crash or
// saturate the webhook, or who simply waits for an operator rolling update,
// would walk straight through it. Same trade, same reason, as the ToolCall
// webhook.
package agentsession

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Path is the webhook route; must match the ValidatingWebhookConfiguration.
const Path = "/validate-agentsession-identity"

// runnerSASuffix is the tail of every per-session runner ServiceAccount name
// (BuildRunnerRBAC mints `<session>-runner-sa`). It must stay in sync with the
// `matchConditions` expression in config/manager/webhook.yaml, which uses the
// same suffix to decide whether to dial this webhook at all.
const runnerSASuffix = "-runner-sa"

// saUsernamePrefix is the fixed prefix of a ServiceAccount's authenticated
// username: system:serviceaccount:<namespace>:<name>.
const saUsernamePrefix = "system:serviceaccount:"

// runnerWritableAnnotations are the ONLY annotation keys a session runner may
// change, each with the reason it is safe — the metadata counterpart of
// runnerWritableStatusFields.
//
// It is an ALLOWLIST, and that is the whole point. AgentSessionStatus is a
// closed struct, so its partition can be a reflect walk that fails on an
// unclassified field; metadata is an open key space, so the same discipline has
// to be spelled the other way round: refuse every key, waive the two the runner
// genuinely writes, and a key nobody anticipated is denied by construction
// rather than by someone remembering to add a row. Omission therefore fails
// CLOSED and loudly — the runner sees the admission error — instead of opening
// a silent hole, which is exactly how metadata.labels went ungated while the
// status half was swept twice.
//
// Both entries are annotations rather than status fields on purpose: status on
// an AgentSession is the operator's observation of the pods, which the runner
// does not own (internal/cmd/runner's sidecarFailureReportedAnnotation says so at its
// definition). That is why the metadata half needs a gate of its own at all.
var runnerWritableAnnotations = map[string]string{
	spiceboxv1alpha1.AnnotationWakeRequestedAt: "the exiting runner stamps it to ask for its own respawn when a message lands as it idles " +
		"(StatusPatcher.RequestWake) — the write the runner Role's `patch` verb on the main resource exists for. It names no other " +
		"session and the operator consumes it against this session's own lastWakeAt",
	// internal/cmd/runner still carries its own unexported sidecarFailureReportedAnnotation
	// (a main package this one cannot import), so the key has two definitions
	// until that one is collapsed onto this constant. Drift denies the runner's
	// write rather than admitting it — the safe direction for a gate to be
	// wrong in.
	spiceboxv1alpha1.AnnotationSidecarFailureReported: "the runner's dedup record of which sidecar failures it has already reported to the user " +
		"(internal/cmd/runner persistNotifiedFailures). Self-scoped: the worst a forged value buys is this session re-reporting, or not " +
		"reporting, its OWN sidecar failure",
}

// runnerWritableLabels is the label half of the same partition, and it is
// deliberately EMPTY: no label on an AgentSession is the runner's to author.
//
// Labels here are correlation keys other components select on, so a runner
// writing one reaches across sessions in a way an annotation does not.
// channelsd's inbound lookup lists sessions by LabelChannelName +
// LabelChannelKey and treats the newest match as the live one, so repointing
// those two at another human's thread hash — both derived from a channel id and
// a thread timestamp anyone in the channel can read — routes that thread's
// messages onto the attacker's session. The interact check then denies, and the
// permission_request that denial publishes carries a preview of the victim's
// message text and their identity into the ATTACKER's channel, while the
// victim's own agent never answers. LabelOutputChannelKey is a second route to
// the same place: its fallback lookup matches on that label alone.
//
// A future runner-writable label goes here as a row with its reason, next to
// the argument for why it cannot be a cross-session handle.
var runnerWritableLabels = map[string]string{}

// pinnedAnnotationReasons carries the specific denial sentence for the
// annotations whose forgery is worst — the ones that carry the session's
// authenticated starter, from which the operator re-derives credential
// projection and namespace-crossing Secret RBAC on every reconcile.
//
// This is MESSAGE DETAIL, not the gate. runnerWritableAnnotations is what
// refuses the write; these keys are absent from it, like every other key. A row
// missing here costs a less specific denial message and nothing else — which is
// the property that makes it safe to hand-maintain.
var pinnedAnnotationReasons = map[string]string{
	spiceboxv1alpha1.AnnotationStartedByCanonicalID: "it is the session's authenticated starter, and the operator re-derives credential projection and " +
		"cross-namespace Secret RBAC from it on every reconcile",
	spiceboxv1alpha1.AnnotationStartedByExternalID: "it is the session's authenticated starter on the channel platform, and feeds the inbound pipeline's " +
		"external-ID fast path",
	spiceboxv1alpha1.AnnotationStartedByEmail: "it is the session's authenticated starter address, and feeds the identity-choice gate's requester",
	spiceboxv1alpha1.AnnotationAuthzServiceSubject: "it is the subject this session's own tool calls are authorized as, so a runner that could " +
		"rewrite it would choose which grants apply to itself",
}

// pinnedMetadataField is one ObjectMeta field, other than the two open key
// spaces, that a session runner may not change. Same shape as
// pinnedStatusField: `get` returns the comparable value and `why` completes
// the denial sentence.
type pinnedMetadataField struct {
	name string
	get  func(*metav1.ObjectMeta) any
	why  string
}

// pinnedMetadataFields, allowlistGatedMetadataFields and
// runnerWritableMetadataFields PARTITION metav1.ObjectMeta, and
// TestEveryObjectMetaFieldIsPinnedOrWaived fails the unit suite for any field
// classified in none or several.
//
// Why a partition rather than a fourth hand-maintained list. Annotations and
// labels are gated by allowlist because their key spaces are OPEN — there is
// nothing to enumerate, so the only shape in which omission fails closed is
// "refuse everything, waive two keys". ObjectMeta itself is a CLOSED struct of
// fifteen json fields, exactly like AgentSessionStatus, so the other thirteen
// get exactly what the status half gets: reflect over the type and fail the
// suite until every field is classified. A table without that walk would be a
// fourth list to keep in sync by hand, and keeping lists in sync by hand is
// what left labels ungated through two sweeps of the status half.
//
// The classification question per field is not "does the runner write it" but
// "can the runner write it, and does anything read it back": the runner Role's
// `patch` verb covers all of ObjectMeta, and several fields are refused only
// because the API SERVER refuses them, which is a fact about apimachinery that
// belongs written down next to the field rather than assumed.
var pinnedMetadataFields = []pinnedMetadataField{
	{
		name: "finalizers",
		get:  func(m *metav1.ObjectMeta) any { return m.Finalizers },
		why: "they decide whether this object can ever be deleted, and both directions are damaging. ADDING one — which " +
			"apimachinery permits until the delete request lands, and the runner is running long before that — wedges the " +
			"session Terminating forever: the operator removes only its OWN finalizer and finalize early-returns when that one " +
			"is absent, so nothing can ever remove a foreign one; channelsd's thread lookup does not filter on deletionTimestamp, " +
			"so the husk keeps winning newestActiveSession and holds the thread, and deleting the namespace never completes. " +
			"REMOVING the operator's skips finalize wholesale, and finalize is the only caller of Tokens.Revoke — the memory " +
			"token registry is in-process and the memory API authenticates against it with no liveness check on the session, so " +
			"deleting the session is the ONLY kill switch for a bearer token the runner may have exfiltrated. It also skips the " +
			"SpiceDB relationship deletion and the reap of the cross-namespace passthrough Role/RoleBinding. The operator is the " +
			"only legitimate writer (EnsureFinalizer) and is not a session runner, so it never reaches this gate",
	},
	{
		name: "ownerReferences",
		get:  func(m *metav1.ObjectMeta) any { return m.OwnerReferences },
		why: "they decide what DELETES this object. No component creates an AgentSession with an owner reference, so any value " +
			"here is a forgery — and the runner Role grants no `delete` on agentsessions, while the garbage collector deletes a " +
			"dependent once its owner references are all dangling. The API server validates neither the owner's existence nor " +
			"(for a namespaced owner) that it shares this namespace, so a reference naming a non-existent or cross-namespace " +
			"owner is accepted and then read as absent: a deletion primitive that routes around the missing verb and is " +
			"attributed to the GC rather than to the runner. blockOwnerDeletion on a real foreign object additionally wedges " +
			"that object's foreground deletion. The blast radius is the forging session, so this is not the credential " +
			"escalation the starter annotations are — it is pinned because nothing legitimate writes it and there is no honest " +
			"waiver sentence to write",
	},
}

// allowlistGatedMetadataFields are the ObjectMeta fields gated by an allowlist
// over their KEYS rather than by a whole-field comparison, naming the map that
// does it. They cannot be pinned whole: the runner has two annotation keys it
// legitimately writes, so refusing every annotations change would refuse the
// write the `patch` verb exists for.
var allowlistGatedMetadataFields = map[string]string{
	"annotations": "gated key-by-key by runnerWritableAnnotations (two waived keys; every other key refused)",
	"labels":      "gated key-by-key by runnerWritableLabels (empty by design; every key refused)",
}

// runnerWritableMetadataFields are the ObjectMeta fields this gate does NOT
// compare, each with the reason that is safe. As with
// runnerWritableStatusFields, the written reason IS the value of the waiver —
// most of these are safe because the API server itself refuses the change, and
// that is a claim about apimachinery worth stating where someone can check it.
var runnerWritableMetadataFields = map[string]string{
	"name":      "immutable on update (ValidateObjectMetaUpdate); the object is identified by the request path regardless",
	"namespace": "immutable on update (ValidateObjectMetaUpdate)",
	"uid": "immutable on update (ValidateObjectMetaUpdate). Worth stating because it is load-bearing elsewhere: every " +
		"per-session child object owner-refs this UID, and PassthroughRoleName is \"passthrough-\" + UID",
	"creationTimestamp": "immutable on update (ValidateObjectMetaUpdate). Also worth stating: channelsd's newestActiveSession " +
		"ranks thread candidates by it, so a mutable creationTimestamp would be a thread-takeover primitive on its own",
	"deletionTimestamp": "immutable on update (ValidateObjectMetaUpdate); deletion is requested through DELETE, which the " +
		"runner Role does not grant",
	"deletionGracePeriodSeconds": "immutable on update (ValidateObjectMetaUpdate)",
	"generateName":               "consulted only by the create path when name is empty, so a value written after creation is inert",
	"generation": "server-owned: the CustomResource strategy's PrepareForUpdate takes it from the stored object and bumps it " +
		"only when something outside metadata changed, so a client value never survives",
	"resourceVersion": "server-assigned; a client value is only an optimistic-concurrency precondition, never the stored value",
	"selfLink":        "deprecated and no longer populated by the API server",
	"managedFields": "it must NOT be compared: the field manager rewrites it inside rest.BeforeUpdate, which store.Update runs " +
		"BEFORE updateValidation dials this webhook, so it differs on EVERY write — including the runner's own " +
		"wake-requested-at patch, which a comparison would therefore refuse. What it carries is server-side-apply ownership " +
		"bookkeeping, and the one SSA writer of an AgentSession's main resource (`oap agent run`'s ap-apply field manager) " +
		"applies with ForceOwnership, so a wiped entry costs at most an unpruned field on a human's re-apply of the same " +
		"session name",
}

// pinnedStatusField is one status field a session runner may not change.
// `get` returns the comparable value; `why` completes the denial sentence
// "AgentSession.status.<name> is not writable by a session runner: <why>".
//
// A table rather than a chain of `if`s because the failure mode here is
// omission: status.pendingRestart was guarded and status.effectiveIdentityMode,
// which reaches the SAME credential projection, was not. A new status field
// must land in this table or in runnerWritableStatusFields — the two partition
// AgentSessionStatus, and TestEveryStatusFieldIsPinnedOrWaived enforces it.
type pinnedStatusField struct {
	name string
	get  func(*spiceboxv1alpha1.AgentSessionStatus) any
	why  string
}

// pinnedStatusFields are the status fields the operator treats as authority —
// each one, if a runner could write it, buys the runner either another human's
// credentials or control of the audit log's trust root.
var pinnedStatusFields = []pinnedStatusField{
	{
		name: "closureDenied",
		get:  func(s *spiceboxv1alpha1.AgentSessionStatus) any { return s.ClosureDenied },
		why: "it says some member of this session's delegation closure has already been DENIED, and the trifecta dispatch gate " +
			"refuses on it in EVERY mode — including logging, because a denied closure is a structural fact rather than a policy " +
			"judgement. A runner that could clear it would switch off a gate an operator cannot switch off, from inside the " +
			"process the gate exists to constrain. It is also unanswerable by the runner in the first place: deriving it needs a " +
			"List over the delegation tree, which the runner's Role does not grant, so a runner writing this field is by " +
			"construction reporting something it could not have learned",
	},
	{
		name: "agentWakeCredit",
		get:  func(s *spiceboxv1alpha1.AgentSessionStatus) any { return s.AgentWakeCredit },
		why: "it is the budget of agent-driven wakes remaining before a HUMAN must speak again, and the runner is one of the two " +
			"agents it bounds. A forged credit is not a nuisance — it IS the loop: two sessions that can refill each other's " +
			"budget sustain an agent→agent conversation with no person in it, which is the exact failure cross-agent thread " +
			"participation is bounded to prevent. Channelsd authors it, because only channelsd sees a turn's ORIGIN and can tell " +
			"a human refill from an agent spend; the party being rate-limited must not hold the pen",
	},
	{
		name: "pendingRestart",
		get:  func(s *spiceboxv1alpha1.AgentSessionStatus) any { return s.PendingRestart },
		why: "the restart/takeover marker is authored by channelsd, which is the authorization choke point for a thread takeover " +
			"(takeover deliberately skips the SpiceDB fork gate)",
	},
	{
		name: "effectiveIdentityMode",
		get:  func(s *spiceboxv1alpha1.AgentSessionStatus) any { return s.EffectiveIdentityMode },
		why: "it is the resolved identity mode the passthrough gate binds on, and a forged userPassthrough makes the operator apply " +
			"a Role granting this runner get+update on the starter's OAuth master Secrets. For an ask|dynamic class the value is " +
			"the operator's own mirror of the runner-appended IdentityChoiceResolved event, folded from the SIGNED lifecycle log — " +
			"never a direct status patch",
	},
	{
		name: "auditPublicKey",
		get:  func(s *spiceboxv1alpha1.AgentSessionStatus) any { return s.AuditPublicKey },
		why: "it is the K8s-witnessed trust root for this session's append-only audit chain: the operator registers it as a " +
			"verify-on-write key and `oap audit verify` builds its offline registry from it. The operator derives it from the " +
			"per-session Secret's signing seed",
	},
	{
		name: "auditKeyID",
		get:  func(s *spiceboxv1alpha1.AgentSessionStatus) any { return s.AuditKeyID },
		why:  "it names which key the audit chain verifies under, and is derived by the operator from the same signing seed",
	},
	{
		name: "auditChainHeads",
		get:  func(s *spiceboxv1alpha1.AgentSessionStatus) any { return s.AuditChainHeads },
		why: "they are the tail anchors that make truncation detectable, computed once by the operator at completion; a " +
			"pre-stamped map is never recomputed and permanently displaces the real ones",
	},
	{
		name: "appliedInteractPermission",
		get:  func(s *spiceboxv1alpha1.AgentSessionStatus) any { return s.AppliedInteractPermission },
		why: "it is channelsd's snapshot of the AgentClass interact policy, and it becomes a SpiceDB grant: on a thread " +
			"takeover the operator copies the PARENT session's value onto the victim's newly created child as " +
			"agentsession:<child>#participant, which `interact = owner + started_by + participant - denied` turns into " +
			"interact — and `memory_entry#read = session->read_transcript` turns into read of the inherited transcript — " +
			"on a session owned by a different human. Any subject-set spicedb.ParseSubject accepts (e.g. a group the " +
			"attacker belongs to) is enough",
	},
	{
		name: "appliedInteractPermissionAt",
		get:  func(s *spiceboxv1alpha1.AgentSessionStatus) any { return s.AppliedInteractPermissionAt },
		why: "it dates the snapshot above and is written by channelsd in the same status patch; leaving it writable lets a " +
			"runner backdate the record of when — and therefore under which class policy — the grant was captured",
	},
	{
		name: "bundleSessions",
		get:  func(s *spiceboxv1alpha1.AgentSessionStatus) any { return s.BundleSessions },
		why: "the operator re-reads spiceboxSessionName out of it on EVERY reconcile and turns the list into two grants: the " +
			"per-session Role's resourceNames on spiceboxsessions (BuildRunnerRBAC), and the scopes this runner's memory-API " +
			"bearer token authorizes (Tokens.Set). Both run unconditionally, while the operator's own rebuild of the list is " +
			"skipped for every parked/terminal phase and for a class with no toolBundles — so an injected entry naming another " +
			"session survives and buys that session's transcript and secret-output Secret",
	},
	{
		name: "pendingRequesters",
		get:  func(s *spiceboxv1alpha1.AgentSessionStatus) any { return s.PendingRequesters },
		why: "it is channelsd's record of who asked for access and what they said. The owner's Approve click reads the entry " +
			"back and writes agentsession#participant@user:<that entry's canonical id>, then replays the entry's own message " +
			"text through the inbound pipeline with the permission check skipped — so a rewritten entry converts a legitimate " +
			"human approval into a grant to, and a turn authored by, someone else",
	},
	{
		name: "pendingInteractions",
		get:  func(s *spiceboxv1alpha1.AgentSessionStatus) any { return s.PendingInteractions },
		why: "it is channelsd's queue of interactions awaiting a human. The server re-checks the approver, so this is not the " +
			"authorization gate — but `oap session approve` builds its list from this field and auto-selects when exactly one " +
			"entry is pending, so an injected entry collects a genuinely-attributed owner decision for a request the owner " +
			"never saw. The runner asks for interactions over the bus; it has no reason to author the queue",
	},
	{
		name: "passthroughCredHashes",
		get:  func(s *spiceboxv1alpha1.AgentSessionStatus) any { return s.PassthroughCredHashes },
		why: "it is the operator's per-credential content hash, and the ONLY input to the diff that emits a credential " +
			"invalidation. A runner that clears it sends the next reconcile down the prime branch, which returns without " +
			"emitting — so the broker keeps serving a passthrough credential the user has just replaced or unlinked",
	},
	{
		name: "resolvedSidecarToolboxes",
		get:  func(s *spiceboxv1alpha1.AgentSessionStatus) any { return s.ResolvedSidecarToolboxes },
		why: "the operator reads the PRIOR secretTokenHash out of it to decide whether a separate-pod sidecar is still running " +
			"a rotated-out upstream credential; a blanked hash reads as 'no prior' and the pod is never replaced. It also " +
			"carries the effectiveNetworkMode/effectiveAllowedHosts snapshot a DNS-aware CNI is meant to consume",
	},
	{
		name: "supersededBy",
		get:  func(s *spiceboxv1alpha1.AgentSessionStatus) any { return s.SupersededBy },
		why: "it is the operator's record that this session's fork completed (SupersedeParent, restart.go), and ReconcileRestart " +
			"treats a non-empty value as 'already done' and drops the pending marker. One forged patch is therefore a mute " +
			"button: every LATER restart marker channelsd writes onto this session is cleared without a condition, a notice or " +
			"a log — including a different human's takeover, which is both the one path that deliberately skips the SpiceDB " +
			"fork gate and the recovery mechanism against a runner doing exactly this. channelsd also reads it as the " +
			"tie-breaker for which session owns a thread (newestActiveSession)",
	},
	{
		name: "effectiveSettings",
		get:  func(s *spiceboxv1alpha1.AgentSessionStatus) any { return s.EffectiveSettings },
		why: "the operator recomputes it early in each reconcile, but the revocation sweep reads the PERSISTED value BEFORE " +
			"that: sweepRevokedGrants runs above every gate that can park the reconcile, and reconcileCredentialGrants gates " +
			"each credential's SpiceDB authorized_token grant on runner.ToolkitAllowed(status.effectiveSettings, …), which " +
			"reads a nil allowedToolkits as unconstrained. allowedToolkits is an ADMIN ceiling — the cluster∩namespace " +
			"Settings intersection, not a class preference — so a runner that nils it collects a grant for a toolkit the " +
			"admin forbade, and the authoritative diff that would delete it again is unreachable from exactly the " +
			"parked/terminal phases the sweep exists to cover (idle-sleep is the resting state of every channel-attached " +
			"agent). The sweep's own safety argument — 'the live grant set was written under those same older settings, so " +
			"this pass can only compute a superset' — holds only while the operator is the sole author. It also names the " +
			"Secret and key mounted as the runner pod's llm-api-key volume (podspec.go)",
	},
}

// runnerWritableStatusFields are the AgentSessionStatus json field names a
// session runner MAY write, each with the reason it is safe to leave open.
// Together with pinnedStatusFields it partitions the whole type: every field
// must appear in exactly one map, and TestEveryStatusFieldIsPinnedOrWaived
// fails the unit suite until a newly-added field is classified.
//
// This map declares PERMISSION, not observed behaviour. Most entries are
// fields the runner genuinely writes through pkg/agent/runner.StatusPatcher —
// its only status-write surface — but a few are fields it never touches and
// that cost nothing to leave writable, because the operator recomputes them
// every reconcile or a forgery only affects the forging session. Each reason
// says which case it is.
//
// The reason is the point of the map. A bare name list cannot force a sweep of
// the type to be exhaustive — one such sweep still missed supersededBy. A waiver
// you must justify in a sentence is a classification; a name you can add
// silently is not.
var runnerWritableStatusFields = map[string]string{
	// ---- written by the runner's StatusPatcher (pkg/agent/runner/status.go) ----
	"phase": "the runner drives its own lifecycle (WriteSucceeded/WriteFailed/WriteIdle/WriteAwaitingRetry); " +
		"the operator derives parked phases from conditions, not from the runner's value",
	"startedAt":  "stamped by the runner's first PatchProgress when the phase leaves Pending",
	"finishedAt": "stamped by the runner at terminal transition, alongside phase",
	"runDuration": "the runner's own accumulated active run-time (PatchRunDuration); the operator reads it against the " +
		"class's maxDuration budget, so inflating it only ends the forging session sooner",
	"progress": "the per-turn/per-tool caption the runner publishes for the status surface",
	"result":   "the agent's own summary + artifact refs, written by WriteSucceeded",
	"pinnedMessage": "the runner's projection of the triggered session's opening-message status (badge/body/link), " +
		"written by setPinnedMessage each turn; channelsd reads it only to re-render the session's OWN output-channel " +
		"opening message. The body is model text already bounded to that same thread the runner writes freely via " +
		"respond_to_user (and gated identically), and badge/link are cosmetic — no credential or cross-session reach, " +
		"so no honest pin sentence",
	"completionBypasses": "the runner records its own overrides of this class's completion requirements " +
		"(RecordCompletionBypass). Nothing reads it back: the gate that produced it runs in the same runner, " +
		"in-process, against live observations rather than against this record, so a forged entry authorizes " +
		"nothing and reaches no other session. What it CAN do is misreport the forging session's own history to " +
		"a human — which a runner that simply declined to record a real bypass could do anyway, and which the " +
		"user-facing notice published on that same session's channel is the check on",
	"failureReason": "the runner's classification of its own failure; the operator maps it to a disposition for THIS " +
		"session's retry decision",
	"retryAttempts": "incremented by the runner's WriteAwaitingRetry; bounded by the class's own retry ceiling, so a " +
		"forged count only burns the forging session's retries",
	"runnerNotes":            "the runner's append-only operator-facing notes (AppendRunnerNote)",
	"observedPins":           "the runner's record of the image/skill digests it actually resolved; reported, not re-consumed as authority",
	"sidecarReachability":    "the runner's own sidecar prober result (RecordSidecarReachability)",
	"credentialAuthFailures": "the runner is the only component that sees a tool call's upstream auth outcome (RecordCredentialAuthFailure)",
	"activeWidgets":          "the runner's durable ref for a persisted MCP-UI widget (AppendActiveWidget)",
	"satisfiedSecretOutputs": "the runner records which declared secret outputs it produced (WriteSatisfiedSecretOutput)",
	"awaitingUserInputSince": "the runner marks and clears its own wait for a human reply",
	"parentExchange": "a delegated child's own question to the agent that delegated to it, and whether it is still " +
		"outstanding (ask_parent / the loop's resume clears). The SubagentRequest controller mirrors it onto the " +
		"request, which is why it lives here rather than being written to that request directly — RBAC cannot scope a " +
		"status write to one field, so a runner able to write subagentrequests/status could fabricate a Denied or " +
		"Succeeded delegation. Forging this one only makes the forging session's OWN delegation report a question it " +
		"did not ask, to the parent that is already reading its answers",
	"inputRequest": "a delegated child's own mid-flight ask for DATA, naming one of its declared input slots " +
		"(request_input). Runner-written for the same reason parentExchange is, and forging it is bounded the same " +
		"way — but more tightly: a fabricated request cannot WIDEN anything, because the parent may only bind a tag " +
		"it can itself read (the attenuation check at handoff) and a disclosing one still routes to a human. The " +
		"worst a forging child achieves is asking for something nobody sends",
	"estimatedCost": "the runner's running token-cost estimate (PatchEstimatedCost)",
	"toolGuard":     "the runner's own budget/breaker counters (PatchToolGuard), enforced in-process by the same runner",
	"conditions": "owned per condition TYPE, not as a whole field — the runner sets its lifecycle conditions and the " +
		"operator/channelsd set theirs (v1alpha1.IsApprovalCondition, agentstatus.WriteOwned). A whole-field pin would " +
		"refuse the runner's own Succeeded/Failed/Idle writes",
	"permissionSurface": "the runner's best-effort enumeration of what its own tools can reach (StatusPatcher." +
		"PublishPermissionSurface, permsurface_publish.go), published once per Run. The only reader is `oap session " +
		"capabilities`, which renders it as a display listing; nothing folds it into an authorization decision, a " +
		"credential projection, or an action on another session — a forged/inflated value only makes the CLI show an " +
		"inaccurate capability list for the forging session's own tools",

	// ---- not written by the runner, but writable without consequence ----
	"observedGeneration": "the operator overwrites it with metadata.generation on every reconcile and nothing reads it back " +
		"(no AgentSession reader anywhere in pkg/, cmd/ or web/)",
	"runnerPodName": "nothing dereferences it. Every create/get/delete/reap of the runner pod goes through the pure " +
		"RunnerPodName(sess) = \"<session>-runner\" (podspec.go), and sidecar/detector cleanup selects by this session's " +
		"own labels — so a value naming another session's pod is never acted on. The one reader, `oap agent run`, tests it " +
		"for emptiness as a readiness poll and discards the string",
	"runnerRestarts": "re-read from the LIVE pod's runner containerStatus on every reflectRunnerPod pass, then compared " +
		"only against this session's own restart ceiling. A forged over-cap value can win that comparison on a pass where " +
		"the pod publishes no containerStatus yet — which fails the forging session, i.e. self-DoS",
	"lastWakeAt": "this session's own idle/sleep bookkeeping (WakePending → shouldWake); the worst a forgery buys is " +
		"respawning or not respawning ITS OWN runner",
	"lastUIServeAt": "as lastWakeAt, for the serve-only spawn (UIServePending → shouldServeUI). It names no other session, " +
		"and the pod it gates runs NO agent loop — so a forgery cannot obtain a turn, a phase change, or a terminal status. " +
		"Forward buys suppressing serve spawns for its own dashboard; backward buys respawning its own serve pod: self-DoS " +
		"in both directions",
	"lastIdleAt": "as lastWakeAt — an input to this session's own reap/archive timer, and to channelsd's watchdog-clear " +
		"dedup key for this same session",
	"sleptAt": "as lastWakeAt — read only by idleSleepDue, for this session's own wake/reap timer",
	"startFailure": "channelsd's record of an authz-write failure at creation; the operator reads it into markBootFailed, " +
		"which fails THIS session with the reason/message verbatim. That is a primitive the runner already holds — " +
		"StatusPatcher.WriteFailed sets the same Failed condition with the same arbitrary reason/message. Residual: " +
		"markBootFailed additionally appends a ProvablyUnschedulable event to this session's signed lifecycle log, which " +
		"WriteFailed does not; the event is self-scoped and correctly attributed to the operator",
	"identityChoiceParkedAt": "the deadline for this session's own ask|dynamic identity prompt. Backdating it produces " +
		"Failed, never a resolved mode — what actually gates the credential projection is effectiveIdentityMode, which is " +
		"folded from the SIGNED lifecycle log and is pinned above. Residual: it is stamped once and never re-derived, so " +
		"forward-dating lets a runner park ITSELF, pod alive, past both identityChoiceTimeout and budget.sessionExpiration " +
		"— self-scoped lifetime evasion, not a credential one",
	"startApprovalParkedAt": "the deadline for this session's own guest start-approval park. The gate that withholds the " +
		"runner keys on AnnotationStartApprovalRequestRef — object METADATA a status patch cannot touch — and no runner " +
		"exists while a session is parked, so by the time one could forge this field the gate no longer reads it: without " +
		"the marker the operator ignores the timestamp entirely. Backdating on a still-marked session could only come from " +
		"another session's runner (blocked by the per-session identity check above) and would produce Failed, never an " +
		"approval — the standing writes come from decideStartApproval off the admin's click, not from any status field",
	"resolvedWorkspaceSource": "deliberately NOT re-resolved: workspaceoverlay.go freezes the snapshot once OverlayCut is " +
		"true. Safe on three counts anyway — the operator's only read is that boolean latch; the overlay Job takes its base " +
		"PVC from the WorkspaceSource CR's own status, not from this snapshot, so no other session's volume is reachable; " +
		"and the only consumer of the frozen fields is the runner itself, which builds that Job in-process with the " +
		"jobs create/delete RBAC it already holds. Residual: forging OverlayCut early empties its own /workspace, and a " +
		"rewritten locator redirects its own human-approved push target — both self-scoped, both bounded by power the " +
		"runner already has",
	"resolvedSkillBundles": "the operator rebuilds the list before the read that mounts it, and a forged entry cannot " +
		"survive the rebuild: skills.go's carry-forward requires digest, mountName AND configMapName to equal values " +
		"recomputed in the same loop from the Skill/ClusterSkill CR. Worth knowing that the rebuild is guarded by " +
		"BundleStore != nil (always wired in internal/cmd/operator) — the safety rests on that wiring invariant, because the list " +
		"does become a ConfigMap untarred into the sandbox's /skills",
	"resolvedContentGuardDetectors": "the operator rebuilds the whole slice — image included, from " +
		"status.effectiveSettings.contentInspectors through the contentguard registry — on every reconcile that reaches " +
		"the detector step, and builds the pod from that fresh local slice, never from what is persisted. The pod's name, " +
		"namespace and ownerRef are derived from the session (cosidecar.BuildPod), so a forged ref cannot name someone " +
		"else's. This waiver depends on effectiveSettings being pinned above",
	"interactorTuplesWritten": "the controller's dedup cache of subjects for whom the idempotent " +
		"agentclass#interactor SpiceDB touch has already landed (writeInteractorTuples); the runner never writes it. " +
		"can_personalize is derived from the LIVE relationship, not from this cache, so a forged entry grants nothing — " +
		"the check that consumes it (slices.Contains against this session's own PINNED starter annotation) can only " +
		"ever test the one subject this session's own runner already acts as. The worst it buys is suppressing this " +
		"session's own retry of an already-idempotent write, denying its own starter no more than the runner could " +
		"already withhold by declining to run at all — self-scoped, and any other session sharing that class/subject " +
		"still attempts the same touch",
}

// Webhook validates AgentSession update admission requests.
type Webhook struct{ decoder admission.Decoder }

// New constructs the webhook with the scheme decoder.
func New(d admission.Decoder) *Webhook { return &Webhook{decoder: d} }

func (w *Webhook) Handle(_ context.Context, req admission.Request) admission.Response {
	// CREATE is not gated here: no runner Role grants `create` on
	// agentsessions, and the starter annotations are stamped at creation by
	// channelsd / webd. DELETE carries no object to compare.
	if req.Operation != admissionv1.Update {
		return admission.Allowed("")
	}
	if !isSessionRunner(req.UserInfo.Username) {
		return admission.Allowed("requester is not a session runner ServiceAccount")
	}

	var newer, older spiceboxv1alpha1.AgentSession
	if err := w.decoder.Decode(req, &newer); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if err := w.decoder.DecodeRaw(req.OldObject, &older); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Branch on the subresource rather than checking both halves on both
	// paths. A main-resource write cannot persist a status change (the API
	// server strips it for a CRD with a status subresource) and a status write
	// cannot persist a metadata/spec change, so cross-checking would only
	// manufacture false denials on a stale round-trip.
	if req.SubResource == "status" {
		for _, f := range pinnedStatusFields {
			if !reflect.DeepEqual(f.get(&older.Status), f.get(&newer.Status)) {
				return admission.Denied("AgentSession.status." + f.name + " is not writable by a session runner: " + f.why)
			}
		}
		return admission.Allowed("")
	}

	if key, ok := firstUnwaivedChange(older.Annotations, newer.Annotations, runnerWritableAnnotations); ok {
		why, named := pinnedAnnotationReasons[key]
		if !named {
			why = "no session runner writes it. The runner's `patch` verb on the main resource exists only for the two annotations in " +
				"runnerWritableAnnotations (wake-requested-at, sidecar-failure-reported); every other key is refused by default, " +
				"because metadata is an open key space and a denylist can only ever pin what someone remembered to enumerate"
		}
		return admission.Denied("AgentSession annotation " + key + " is not writable by a session runner: " + why)
	}
	if key, ok := firstUnwaivedChange(older.Labels, newer.Labels, runnerWritableLabels); ok {
		return admission.Denied("AgentSession label " + key + " is not writable by a session runner: labels on an AgentSession are " +
			"correlation keys OTHER components select on — channelsd's inbound lookup lists sessions by the channel name+key labels " +
			"and delivers a thread to the newest match — so a runner that writes one reaches across sessions. No label is " +
			"runner-writable (runnerWritableLabels is empty by design)")
	}
	for _, f := range pinnedMetadataFields {
		if !reflect.DeepEqual(f.get(&older.ObjectMeta), f.get(&newer.ObjectMeta)) {
			return admission.Denied("AgentSession.metadata." + f.name + " is not writable by a session runner: " + f.why)
		}
	}
	if !reflect.DeepEqual(older.Spec, newer.Spec) {
		return admission.Denied("AgentSession.spec is not writable by a session runner: the runner's `patch` verb exists only to stamp the wake-requested-at and sidecar-failure annotations")
	}
	return admission.Allowed("")
}

// firstUnwaivedChange returns the lexically first key whose value differs
// between older and newer and is not waived — appearing in only one of the two
// maps counts as a change, so a delete is refused exactly like a rewrite.
//
// Sorted rather than map-ordered so a patch touching several forbidden keys
// always names the same one: an admission denial is the only diagnostic the
// runner's author gets, and a message that varies run to run reads as a flake.
func firstUnwaivedChange(older, newer, waived map[string]string) (string, bool) {
	changed := make([]string, 0, len(older)+len(newer))
	for key, oldVal := range older {
		if newVal, ok := newer[key]; !ok || newVal != oldVal {
			changed = append(changed, key)
		}
	}
	for key := range newer {
		if _, ok := older[key]; !ok {
			changed = append(changed, key)
		}
	}
	slices.Sort(changed)
	for _, key := range changed {
		if _, ok := waived[key]; !ok {
			return key, true
		}
	}
	return "", false
}

// isSessionRunner reports whether username is a per-session runner
// ServiceAccount.
//
// It matches on the `-runner-sa` suffix rather than reconstructing the
// session's own expected SA name, and does so deliberately: the runner Role
// pins agentsessions to its own session by resourceName, so a runner can only
// ever reach its own object anyway, and matching the whole family is
// fail-closed — an unexpected `*-runner-sa` principal is restricted MORE, never
// less. A same-named ServiceAccount created by an admin for some other purpose
// loses the ability to rewrite a session's starter identity, which is the safe
// direction to be wrong in.
func isSessionRunner(username string) bool {
	if !strings.HasPrefix(username, saUsernamePrefix) {
		return false
	}
	ns, name, ok := strings.Cut(strings.TrimPrefix(username, saUsernamePrefix), ":")
	return ok && ns != "" && strings.HasSuffix(name, runnerSASuffix)
}
