package pipeline

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/agent/restartmarker"
	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/grants"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/outputbind"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipelinehost"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/parkedprompt"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/authzed/openagentprimitives/pkg/platform/startup"
	"github.com/authzed/openagentprimitives/pkg/x/besteffort"
)

// Authz is the interface the pipeline uses for SpiceDB writes/checks.
// Implemented by *pkg/authz/spicedb.Client.
type Authz interface {
	TouchStartedBy(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) error
	TouchOwner(ctx context.Context, ns, name, subjectRef string) error
	CheckInteract(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
	// CheckConverse is the agent-to-agent arm of the same inbound gate,
	// answering agentsession#converse for a session-typed acting subject. Both
	// arms are required by engine.SessionInteractChecker, which NewPipeline
	// satisfies from this one interface.
	CheckConverse(ctx context.Context, ns, name, senderNS, senderName string, fullyConsistent bool) (bool, error)
	CheckDenied(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
	TouchInteractParticipant(ctx context.Context, ns, name, subject string) error
	TouchInteractParticipantUser(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) error
	TouchDeniedUser(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) error
	// LookupSubjectIncludes reports whether canonicalID is in the resolved
	// subject-set subjectRef ("<type>:<id>#<relation>") — the approval decision
	// pipe's check that a clicker may approve under the request's approver set.
	LookupSubjectIncludes(ctx context.Context, subjectRef string, canonicalID identity.CanonicalUserID) (bool, error)
	// LookupSubjects and LookupInteractSubjects satisfy engine.LookuperImpl so
	// NewPipeline can build an Engine directly from the Authz arg.
	LookupSubjects(ctx context.Context, subjectRef string) ([]string, error)
	LookupInteractSubjects(ctx context.Context, ns, name string) ([]string, error)
	// CheckApprove and CheckOwnerOnResource satisfy engine.ApproverCheckerImpl
	// so the same Authz can serve as NewPipeline's ApproverChecker dep.
	CheckApprove(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
	CheckOwnerOnResource(ctx context.Context, resType, resID string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
	CheckOnResource(ctx context.Context, resType, resID, permission string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
	// GrantSlots binds instances into a session's slots. Used by thread
	// adoption to seed the values a trusted author already put in the thread,
	// so the agent may reach them without a prompt. Present on *spicedb.Client.
	GrantSlots(ctx context.Context, ns, name string, bindings []authz.SlotBinding, expiresAt time.Time) error
	// DeleteSlotGrants removes every slot grant AND pin a session holds. The
	// mint path calls it to sweep whatever a DEAD PREDECESSOR with this same
	// ns/name left in SpiceDB, synchronously BEFORE its own first bind —
	// because this path pre-stamps the operator's finalizer (see the Create
	// site), which suppresses the operator's own admission sweep for a channelsd
	// mint. A pin never expires, so a leftover would otherwise refuse the new
	// session its own first bind and leak the predecessor's authority under the
	// reused name. Present on *spicedb.Client (same method finalize + the
	// operator's admission sweep use).
	DeleteSlotGrants(ctx context.Context, ns, name string) error
	// Relations is the authz.RelWriter view of the same client, needed so an
	// approval can narrow scope through authz.BindApproved rather than writing
	// a grant alone. Present on *spicedb.Client.
	Relations() authz.RelWriter
}

// Memory is the interface the pipeline uses to read and append turns to
// operator memory.
type Memory interface {
	Append(ctx context.Context, ns, name string, t MemTurn) error
	// ReadAll fetches all turns for the session in ascending Index order. A
	// session with no turns yet returns empty and a nil error; the HTTP impl
	// cannot tell an empty session from a non-existent one — both read as none.
	ReadAll(ctx context.Context, ns, name string) ([]MemTurn, error)

	// RecordChannelMsgRef maps (kind, ref) → turnIndex at the (ns, name) scope,
	// so the channel kind's restart UI can resolve a message the user clicked
	// back to the turn to cut at.
	RecordChannelMsgRef(ctx context.Context, ns, name, kind, ref string, turnIndex int) error

	// RecordTriggerDelivery stores the signed webhook delivery that opened the
	// (ns, name) session — see pkg/memory/kinds/triggerdelivery. Called at
	// most meaningfully once per session, on the delivery that actually
	// created it; the Kind's own id is fixed, so a caller need not track
	// whether it already recorded one.
	RecordTriggerDelivery(ctx context.Context, ns, name, kind, event, channelKey string, body []byte) error

	// RecordEnvelopeFacts writes the facts a TriggerFactProvider kind derived
	// from the SAME verified delivery RecordTriggerDelivery just captured —
	// see pkg/memory/kinds/envelopefact. One entry per (subject, fact) pair,
	// keyed to the objects the delivery named, so a precondition can resolve
	// them without re-deriving anything from the raw payload.
	RecordEnvelopeFacts(ctx context.Context, ns, name, kind, event string, facts []channelkinds.TriggerFact) error

	// UploadInboundAsset streams one attachment to the operator (POST
	// /inbound-asset/{ns}/{sess}), which stores the bytes and, when a backend
	// claims the mime, the extracted text too. body is streamed to EOF, never
	// buffered whole. InboundAssetResult carries permanent-vs-transient so the
	// caller needs no second round trip to tell them apart.
	UploadInboundAsset(ctx context.Context, ns, sess, mime, filename string, body io.Reader) (InboundAssetResult, error)
}

// InboundAssetResult is the operator's answer to one UploadInboundAsset call.
// Raw bytes are stored unconditionally, so Ref is set on every nil error.
type InboundAssetResult struct {
	// Artifactstore ref for the stored raw bytes; always set on a nil error.
	Ref string
	// True when text was extracted and stored at TextRef.
	Extracted bool
	// Artifactstore ref for the extracted text; empty unless Extracted.
	TextRef string
	// Page count for a paginated extraction; 0 when unknown or not paginated.
	Pages int
	// Permanent "no backend claims this MIME". False alongside Extracted=false
	// means extraction failed or was unconfigured — transient, and never to be
	// reported to the user as "this type can't be read".
	Unsupported bool

	// Members is one entry per stored archive member, empty for a
	// non-archive. When set, TextRef points at the archive's INDEX rather
	// than at extracted prose.
	Members []InboundAssetMember
	// ArchiveTruncated reports that a bound stopped the walk early, so the
	// agent can be told the bundle is partial rather than left to assume it
	// is whole.
	ArchiveTruncated       bool
	ArchiveTruncatedReason string
}

// ErrInboundAssetTooLarge is returned when the operator's /inbound-asset route
// answers 413 (over httpsrv.MaxInboundAssetBytes). effectiveLimit clamps to that
// same ceiling, so reaching this means the pre-fetch size check was bypassed —
// a kind under-reporting SizeBytes as 0 ("not reported") is the way in. Mapped
// to outcomeOversize (permanent) so the backstop never mislabels it temporary.
var ErrInboundAssetTooLarge = errors.New("channelsd: attachment exceeds the operator's upload size limit")

// MemTurn is one turn as channelsd writes it to operator memory.
type MemTurn struct {
	// Zero-based position in the session transcript.
	Index int `json:"index"`
	// "user" for a turn the runner answers; "inbox" for one it drains on wake.
	Role string `json:"role"`
	// Ordered content blocks making up the turn body.
	Content []MemContent `json:"content"`
	// Who sent it; zero for turns with no human author.
	Author identity.Subject `json:"author,omitempty"`
	// Signed view URN of the surface it arrived through; empty for the channel.
	Via string `json:"via,omitempty"`
}

// MemContent is one content block within a MemTurn.
type MemContent struct {
	// Block discriminator: "text" or "attachment".
	Type string `json:"type"`
	// Block body; set for Type "text".
	Text string `json:"text,omitempty"`

	// The fields below are set only when Type == "attachment" — one block per
	// successfully-read inbound attachment (see attachments.go). They mirror
	// memory.AttachmentBlock, which internal/cmd/channelsd/memory.go carries across the
	// HTTP hop to the operator's memory.ContentBlock.Attachment.

	// Sanitized original filename as the sender's channel reported it.
	Filename string `json:"filename,omitempty"`
	// MIME type the channel reported for the bytes.
	MIME string `json:"mime,omitempty"`
	// ArchiveRef is the archive this block's member came out of; empty for a
	// directly-attached file. The runner keys its native-window exclusion on
	// it — see memory.AttachmentBlock.ArchiveRef.
	ArchiveRef string `json:"archiveRef,omitempty"`
	// Size of the raw bytes; 0 means the channel did not report a size.
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// Artifactstore ref for the raw bytes.
	Ref string `json:"ref,omitempty"`
	// Artifactstore ref for extracted text; empty when nothing was extracted.
	TextRef string `json:"textRef,omitempty"`
	// Page count of a paginated extraction; 0 when unknown or not paginated.
	Pages int `json:"pages,omitempty"`
}

// NATS publishes an envelope on a subject.
type NATS interface {
	Publish(subject string, payload []byte) error
}

// GrantSource identifies channelsd's approve-path SpiceDB write — the
// per-(session, tool, args) grant tuple Pipeline.GrantWriter below is wired
// for. Qualified (not plain Source) because this package owns more than one
// concept.
//
// Distinct from guardian/grants.Source: same tuple SHAPE, but a different
// component/process (channelsd, not the runner) writing it.
//
// Claims deliberately EMPTY, for the same reason as guardian/grants.Source:
// the relation is "grant_<permission>_<resourceType>", generated per
// (resourceType, permission) pair an AgentClass declares — dynamic and
// per-tenant, not enumerable as a literal claim.
//
// GrantWriter is also DORMANT today: toolApprovalHandler
// (tool_approval_interaction.go) writes a SLOT grant via p.Authz.Relations()
// instead, and nothing in this package ever calls GrantWriter's
// WriteRelationships/DeleteRelationships. Registered anyway, empty, so the
// shape holds if this path is revived.
//
// Declared exactly once here; every wiring site (internal/cmd/channelsd, the
// e2e harness) references this var rather than retyping the name, so the
// (lack of) claim can never drift from what this writer actually presents.
var GrantSource = relsource.Source{Name: "channelsdgrants"}

func init() {
	relsource.Register(GrantSource)
}

// Pipeline implements channelkinds.InboundPipeline.
type Pipeline struct {
	// Terminal interaction transitions share a lock because decision and timeout
	// envelopes arrive on independent subscriptions.
	interactionTransitions [64]sync.Mutex

	RecordGoalActor GoalActorRecorder
	K8s             client.Client

	// envVerify enforces the inter-agent envelope contract (signature,
	// session-window binding, freshness, anti-replay) at the top of
	// HandleAgentMessageSend. Always non-nil after NewPipeline; it reads the
	// sending session's audit key fresh off K8s on every call rather than
	// caching it, so a session recreated under the same name is never
	// verified against a stale predecessor's key.
	envVerify *envelopeVerifier

	Authz        Authz
	Engine       engine.Engine
	Memory       Memory
	NATS         NATS
	Capabilities func(kind string) []string // typically registry-driven
	Now          func() time.Time

	// ReadHistory fetches prior channel messages for thread adoption and
	// catch-up. Nil disables backfill. Wired by internal/cmd/channelsd; nil in unit
	// tests that don't exercise adoption.
	ReadHistory HistoryReadFunc

	// GrantWriter writes per-(session, tool, args) SpiceDB grant tuples from
	// the tool_approval handler's approve path. Injected separately from Authz
	// so that interface and its fakes stay closed to extension.
	GrantWriter grants.Writer

	// PreferenceCommitter commits a user_preference_confirm approval's (key,
	// value) pair to the operator's preferences store. Narrow interface (its
	// own definition lives in preference_commit.go, next to its one caller) so
	// the handler depends on neither the concrete operator memory client nor
	// its component-token transport. Wired in internal/cmd/channelsd from the
	// same *httpclient.Client the memory facade already holds. Nil disables
	// the handler with a loud, non-silent programming-error return rather than
	// a nil-interface panic — see preferenceCommitHandler's committer==nil check.
	PreferenceCommitter  PreferenceCommitter
	GoalConsentCommitter GoalConsentCommitter

	// resolvedCache remembers recently-resolved interaction/approval decisions
	// so a late spectator click (after the matching pending entry has been
	// cleared) gets a spectator response naming who actually approved/denied.
	// 30-minute TTL — long enough for slow approver UI edits, short enough to
	// avoid unbounded growth. Consumed by the generic decision pipe
	// (HandleInteractionDecision).
	resolvedCache *resolvedDecisionCache

	// viewDedup caches recent view_message results by the client's idempotency
	// id; viewFlight is its in-flight complement, joining a retry that RACES
	// the original delivery to the same single-flight call. Together they make
	// a same-RequestID retry idempotent whether it lands during or after the
	// first delivery — no double-post to the agent.
	viewDedup  *viewDedup
	viewFlight singleflight.Group

	// PortalAccess intercepts portal-trigger phrases ("manage my accounts")
	// on inbound user messages and short-circuits before the agent dispatch.
	// Nil disables the trigger and the message flows to the agent as text.
	PortalAccess *PortalAccessTriggerer

	// Executor runs the InboundTurn pipeline (the Interact hook) for the
	// active-session permission check. NewPipeline builds one whenever an
	// Authz is supplied; Deliver calls it with no nil guard, so a Pipeline
	// assembled field-by-field must set it or every inbound to an existing
	// session panics.
	Executor *pipeline.Executor

	// Mem is the durable memory facade, backing three reads: the memapproval
	// record for a resource-owner decision (consulted when the in-process
	// request cache is cold, e.g. after a restart), the parked_prompt record
	// behind resurfacePending's cached leg, and that record's RESOLVED
	// tombstone — the decision pipe's cross-restart idempotency gate.
	//
	// Nil degrades explicitly, never silently: resolveDecisionResources treats
	// it as "no durable source" and FAILS CLOSED on an uncached resource-owner
	// decision rather than hand an empty resource set to the approver gate;
	// resurfacePending's cached leg becomes a logged no-op while its regenerate
	// leg keeps working; the decision pipe falls back to in-process-only
	// idempotency. Wired in internal/cmd/channelsd.
	Mem memory.Memory

	// AuthzWriteRetry bounds how long the session-create path re-attempts the
	// started_by authz write before giving up and stamping the terminal
	// startFailure signal (touchStartedByUnlessNonHumanOrParked explains why that
	// failure is unrecoverable afterwards). Request-scoped, so deliberately
	// short: long enough to ride out a SpiceDB rolling restart or leader
	// change, not long enough to hold an inbound hostage to an outage. Zero
	// means a single attempt; NewPipeline sets the default.
	AuthzWriteRetry time.Duration

	// MarkerSigner attests the status.pendingRestart markers this pipeline
	// writes. The operator verifies the signature before acting on one, because
	// a marker is an authorization input the target session's own runner can
	// also write (see pkg/agent/restartmarker).
	//
	// REQUIRED for the fork-trigger paths: nil makes writeInheritForkTrigger and
	// writeTakeoverForkTrigger fail rather than write a marker the operator will
	// refuse anyway, so the misconfiguration surfaces here — in the component
	// that owns the key — instead of as an unexplained refusal in the user's
	// thread. Wired from the Ed25519 key channelsd registers as system:channelsd.
	MarkerSigner *restartmarker.Signer

	// outages dedups the monitoring alert raised when a conversation stalls on
	// an unhealthy agent — one alert per outage, not one per message that walks
	// into it.
	outages agentOutageAlerts
}

// defaultAuthzWriteRetry is the ceiling for AuthzWriteRetry. startup.Retry's
// 500ms floor doubles under it, buying three attempts across roughly a second
// and a half — a SpiceDB rolling restart's reconnect window, and short enough
// that an inbound never stalls a surface for long.
const defaultAuthzWriteRetry = 1500 * time.Millisecond

func NewPipeline(k client.Client, az Authz, m Memory, n NATS, caps func(string) []string) *Pipeline {
	var eng engine.Engine
	if az != nil {
		eng = engine.New(engine.Deps{
			SessionInteractChecker: az,
			Granter:                az,
			Lookuper:               az,
			ApproverChecker:        az,
		})
	}
	return &Pipeline{
		K8s:             k,
		envVerify:       newEnvelopeVerifier(k),
		Authz:           az,
		Engine:          eng,
		Memory:          m,
		NATS:            n,
		Capabilities:    caps,
		Now:             func() time.Time { return time.Now().UTC() },
		AuthzWriteRetry: defaultAuthzWriteRetry,
		resolvedCache:   newResolvedDecisionCache(),
		viewDedup:       newViewDedup(nil),
		Executor:        buildInteractExecutor(eng),
	}
}

// buildInteractExecutor constructs the InboundTurn executor + Interact hook for
// the channelsd permission check. CheckSessionInbound passes
// fullyConsistent=true for two independent reasons: decidePermission writes the
// participant tuple then immediately replays the inbound via
// ResubmitAuthorized, and the delegation lineage tuple is written by the same
// controller pass that creates a conversational child — MinimizeLatency can
// miss either and re-deny. Returns nil when no Engine is wired, which leaves
// the Pipeline with no InboundTurn gate at all — see the Executor field.
//
// CheckSessionInbound rather than CheckSessionInteract: the acting subject is
// not always a human. It carries its SpiceDB type, and the engine answers
// agentsession#interact for a user and agentsession#converse for a session.
func buildInteractExecutor(eng engine.Engine) *pipeline.Executor {
	if eng == nil {
		return nil
	}
	reg := pipeline.NewRegistry()
	reg.Register(hooks.NewInteract(hooks.InteractDeps{
		CheckInbound: func(ctx context.Context, ns, name string, actor identity.Subject) (bool, error) {
			return eng.CheckSessionInbound(ctx, authz.SessionRef{Namespace: ns, Name: name}, actor, true)
		},
	}), hooks.OrderInteract)
	return pipeline.NewExecutor(reg)
}

// authzSubjectAllowedTypes returns the SpiceDB subject types an inbound
// arriving over this Channel may assert as its acting subject (the
// no-user-identity branch of Deliver below). The Channel decides what is
// ALLOWED; the value being checked is the inbound's own
// InboundEvent.AuthzSubject, which is not always the Channel's
// spec.authzSubject — on an agent message it is the SENDING session, named by
// the payload, while spec.authzSubject names whichever end of the pair the
// Channel is not bound to.
//
// "service:" is accepted for every kind. "agentsession:" — an AgentSession in
// this cluster acting in its own right — is admitted only for a kind that
// declares itself a session-to-session transport via
// channelkinds.SessionCounterparty (asked generically, never by comparing the
// kind's name — see AGENTS.md's registry-not-branching rule); widening it for
// a kind that does not implement that interface would let that kind's traffic
// act with a session's standing.
//
// A kind this code has never heard of (chregistry.Get misses) widens
// nothing: service: only, fail closed. The CRD Pattern already constrains
// the two prefixes it can ever see, but this must not trust that a
// pre-existing CR predates the pattern.
func authzSubjectAllowedTypes(ch *spiceboxv1alpha1.Channel) []authz.SubjectType {
	allowed := []authz.SubjectType{authz.SubjectService}
	if ch == nil {
		return allowed
	}
	k, ok := chregistry.Get(ch.Spec.Kind)
	if !ok {
		return allowed
	}
	if sc, ok := k.(channelkinds.SessionCounterparty); ok && sc.AllowsSessionCounterparty() {
		allowed = append(allowed, authz.SubjectAgentSession)
	}
	return allowed
}

// channelRefForLog renders "<ns>/<name>" for a Channel; nil-safe.
func channelRefForLog(ch *spiceboxv1alpha1.Channel) string {
	if ch == nil {
		return "<nil>"
	}
	return ch.Namespace + "/" + ch.Name
}

// clearPendingPrompt marks a resolved prompt so it is never re-surfaced.
//
// No-op without a memory facade. Idempotent and tolerant of an unknown
// requestRef: the decision path and the gate-side timeout path both call it in
// either order, and a ResurfaceRegenerate category is never stored at all (it
// is rebuilt from the phase), so "not found" is normal, not an error.
func (p *Pipeline) clearPendingPrompt(ctx context.Context, ns, name, requestID string) {
	if p.Mem == nil {
		return
	}
	besteffort.Log(log.FromContext(ctx).Info, "clearPendingPrompt",
		parkedprompt.Resolve(ctx, p.Mem, promptScope(ns, name), requestID),
		"session", ns+"/"+name, "requestRef", requestID)
}

// correlateSessions answers "which AgentSession(s) is this inbound for?" — the
// candidate set Deliver then classifies by continuation disposition. An empty
// result is not an error: Deliver's spawn gate and terminal-continuation
// branches decide what a miss means for the Channel's kind.
//
// Two routes, and which one runs is the caller's declaration, not a guess:
//
//   - ev.TargetSession set — the caller already knows the session
//     authoritatively (a bus handler reading it off the NATS subject, after
//     internal/cmd/channelsd/main.go's envelopeHandler cross-checked it). It is
//     read directly; a NotFound is treated as a miss, exactly like a
//     correlation lookup that matched nothing, and this is the ONE place that
//     decides so. A named target that no longer exists is ordinary — a child
//     answering a garbage-collected parent — so a caller that also refused it
//     would report the same routine condition twice, once as an error. See
//     InboundEvent.TargetSession for why a handler cannot always reach its
//     session through the labels.
//   - otherwise — the (LabelChannelName, LabelChannelKey) lookup a Listener
//     depends on, which turns a channel-side key into the session that owns it.
func (p *Pipeline) correlateSessions(
	ctx context.Context, ev channelkinds.InboundEvent, keyHash string,
) (spiceboxv1alpha1.AgentSessionList, error) {
	var sessions spiceboxv1alpha1.AgentSessionList

	if ev.TargetSession != nil {
		var target spiceboxv1alpha1.AgentSession
		switch err := p.K8s.Get(ctx, *ev.TargetSession, &target); {
		case err == nil:
			sessions.Items = append(sessions.Items, target)
		case apierrors.IsNotFound(err):
			log.FromContext(ctx).Info("inbound names a target session that does not exist; nothing to deliver into",
				"session", ev.TargetSession.Namespace+"/"+ev.TargetSession.Name,
				"channel", channelRefForLog(ev.Channel))
		default:
			return sessions, fmt.Errorf("get target session %s/%s: %w",
				ev.TargetSession.Namespace, ev.TargetSession.Name, err)
		}
		return sessions, nil
	}

	if err := p.K8s.List(ctx, &sessions,
		client.InNamespace(ev.Channel.Namespace),
		client.MatchingLabels{
			spiceboxv1alpha1.LabelChannelName: ev.Channel.Name,
			spiceboxv1alpha1.LabelChannelKey:  keyHash,
		},
	); err != nil {
		return sessions, fmt.Errorf("list sessions: %w", err)
	}
	if len(sessions.Items) > 0 {
		return sessions, nil
	}

	// A cron-spawned session anchors its thread on spec.outputChannel.Key,
	// labelled LabelOutputChannelKey by the outbound relay after the first
	// send. Fall back to it so an in-thread human reply still resolves the
	// originating session.
	//
	// LabelChannelName is deliberately omitted: a cron-spawned session
	// carries its *input* channel name there (the bento Channel CR), not the
	// Channel CR whose listener fired this Deliver. The output-key hash is
	// near-collision-free on its own.
	if err := p.K8s.List(ctx, &sessions,
		client.InNamespace(ev.Channel.Namespace),
		client.MatchingLabels{
			spiceboxv1alpha1.LabelOutputChannelKey: keyHash,
		},
	); err != nil {
		return sessions, fmt.Errorf("list sessions by output key: %w", err)
	}
	// That namespace-wide match ignores which agent owns the thread. A Slack
	// conversation can hold several agents' apps, each listener delivering
	// its own copy of every event, so without this filter all of them
	// resolve the SAME session and one human message lands once per app.
	// A thread belongs 1:1 to the agent bound to it.
	sessions.Items = slices.DeleteFunc(sessions.Items,
		func(s spiceboxv1alpha1.AgentSession) bool {
			return s.Spec.Class != ev.Channel.Spec.AgentClass
		})
	return sessions, nil
}

// Deliver implements channelkinds.InboundPipeline.
func (p *Pipeline) Deliver(ctx context.Context, ev channelkinds.InboundEvent) (channelkinds.InboundDecision, error) {
	canonical := canonicalID(ev.ExternalIDs)
	// nonHumanSubject marks the no-user-identity branch below: the acting
	// subject came from Channel.spec.authzSubject and names a "service:<id>"
	// (a cron/bento identity) or an "agentsession:<ns>/<name>" (a delegating or
	// delegated session), NOT a human. started_by is user-only in the schema,
	// so this suppresses both the started_by write and the started-by
	// annotation — and it is what keeps a refusal off handlePermissionDeny's
	// human join-request flow, which has no identity to address and no button
	// a non-human could click.
	nonHumanSubject := false
	// serviceSubject narrows nonHumanSubject to its "service:<id>" half — the
	// one subject type the agentsession schema can never resolve, and so the
	// only one whose inbound skips the interact check below and carries the
	// acting subject onto a fresh session as an annotation. An
	// "agentsession:<ns>/<name>" subject is non-human too and does neither: the
	// check is precisely what decides whether one session may message another.
	serviceSubject := false
	// No-user-identity branch: a listener supplying AuthzSubject and no per-user
	// external ID (the bento/cron path) has no end-user to attribute to, so the
	// Channel CR's declared SpiceDB subject becomes the canonical verbatim —
	// it is already a fully-qualified "<type>:<id>", which RawSubject preserves.
	if ev.AuthzSubject != "" && ev.ExternalIDs.ExternalID == "" {
		// SECURITY: this is the one site that ASSIGNS the acting subject from
		// channel config. RawSubject.Canonical() strips a leading "user:", so an
		// unconstrained value would run the session AS an arbitrary human. Only
		// a non-human service:<id> (any kind) or agentsession:<ns>/<name>
		// (kind=agent only, see authzSubjectAllowedTypes) may be asserted;
		// anything else fails closed here, independent of the CRD pattern a
		// pre-existing CR could predate.
		if err := authz.ValidateSubject(ev.AuthzSubject, authzSubjectAllowedTypes(ev.Channel)...); err != nil {
			log.FromContext(ctx).Info("pipeline: rejecting inbound — invalid channel authzSubject (refusing to mint an impersonating started_by)",
				"authzSubject", ev.AuthzSubject,
				"channel", channelRefForLog(ev.Channel),
				"err", err.Error())
			return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
				fmt.Errorf("channel authzSubject: %w", err)
		}
		rawCanon, rawErr := identity.RawSubject(ev.AuthzSubject).Canonical()
		if rawErr != nil {
			// Unreachable: RawSubject bypasses the synthetic guard entirely.
			return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
				fmt.Errorf("canonicalize channel authzSubject: %w", rawErr)
		}
		canonical = rawCanon
		nonHumanSubject = true
		// Re-validated rather than string-matched, and fail-closed by
		// construction: anything that is not a well-formed service subject
		// leaves this false, which keeps the interact check ON.
		serviceSubject = authz.ValidateSubject(ev.AuthzSubject, authz.SubjectService) == nil
	}

	// Record the channel-identity -> canonical-user linkage for every human
	// inbound (started-by, active repliers, and would-be joiners at their first
	// send). Best-effort; never blocks delivery.
	p.recordChannelIdentity(ctx, ev)

	// 0. Re-Get the Channel CR: listeners cache it at start time and pass that
	// copy on every event, so without this an in-place spec.agentClass patch
	// would not reach new sessions until channelsd restarted.
	if ev.Channel != nil {
		var fresh spiceboxv1alpha1.Channel
		if err := p.K8s.Get(ctx, client.ObjectKey{
			Namespace: ev.Channel.Namespace, Name: ev.Channel.Name,
		}, &fresh); err == nil {
			ev.Channel = &fresh
		} else {
			// On Get error, fall back to ev.Channel as-is — better stale than
			// failing the inbound entirely. Log at V(1) so a persistent re-Get
			// failure (which silently serves the cached spec) is diagnosable.
			log.FromContext(ctx).V(1).Info("channel re-Get failed; using cached spec",
				"channel", ev.Channel.Namespace+"/"+ev.Channel.Name, "err", err.Error())
		}
	}

	// Identity resolves via identity.Principal at the pipeline boundary; SpiceDB
	// sees only `user:<canonicalID>` from here down. There is deliberately no
	// per-platform indirection (no `slack_user` in the schema) so a divergence
	// between write-shape and check-shape cannot hide.

	// 1. Correlation lookup.
	keyHash := channelkey.LabelValue(ev.ChannelKey)
	sessions, cerr := p.correlateSessions(ctx, ev, keyHash)
	if cerr != nil {
		return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError}, cerr
	}

	// Classify every correlated session by its continuation disposition. This is
	// the anti-strand invariant: a phase mapping to no slot is exactly how a new
	// in-thread message lands at "is starting…" with no runner ever spawned. The
	// pure classifier (pkg/agent/session/lifecycle) is the single place
	// channelsd and the operator agree on which phases a runner will still serve.
	//
	//   - Resume / Queue → the active slot: append and route into the session.
	//     Resume covers parked phases whose pod has exited (Idle, AwaitingRetry,
	//     AwaitingCredentials), respawned by the wake annotation written below;
	//     Queue (AwaitingDecision) appends without dropping while a decision is
	//     live.
	//   - NewInheriting → the operator forks a fresh non-colliding session
	//     inheriting the terminal transcript; channelsd writes an inherit
	//     fork-trigger on the terminal parent and never routes into it.
	//   - Refuse → unrecoverable failure: post a loud notice naming the reason
	//     and spawn nothing.
	//
	// The most recent archived session carries its disposition forward so the
	// terminal-continuation block below picks the right branch.
	var active, archived *spiceboxv1alpha1.AgentSession
	var archivedDisp lifecyclecore.Disposition
	for i := range sessions.Items {
		s := &sessions.Items[i]
		archivedBySweep := spiceboxv1alpha1.ArchivedBySweep(s)
		if archivedBySweep {
			// Resume needs history. Retention can reclaim a swept session's
			// transcript; waking an amnesiac agent in a thread full of prior
			// context is worse than forking and seeding from what survives.
			turns, terr := p.Memory.ReadAll(ctx, s.Namespace, s.Name)
			if terr != nil {
				// Fail closed toward the fork: it seeds from memory too, and a
				// read error must not silently strand the inbound.
				log.FromContext(ctx).Info("archived-session resume: transcript read failed; forking instead",
					"session", s.Namespace+"/"+s.Name, "err", terr.Error())
				archivedBySweep = false
			} else if len(turns) == 0 {
				archivedBySweep = false
			}
		}
		disp := lifecyclecore.ContinuationDisposition(lifecyclecore.State{
			Phase:         lifecyclecore.Phase(s.Status.Phase),
			FailureReason: s.Status.FailureReason,
			Archived:      archivedBySweep,
		})
		if disp == lifecyclecore.DispResume || disp == lifecyclecore.DispQueue {
			if active == nil || s.CreationTimestamp.After(active.CreationTimestamp.Time) {
				active = s
			}
			continue
		}
		if archived == nil || s.CreationTimestamp.After(archived.CreationTimestamp.Time) {
			archived = s
			archivedDisp = disp
		}
	}

	if active != nil {
		// 1a. Attended-child redirect: while a LIVE `attended` delegation child
		// is watching active as its LabelAttendedParentNamespace/Name parent, a
		// human's turn on active's own channel is meant for the child, not
		// active itself (spec §4 -- the person converses WITH the child; active
		// only watches, per attended_watch.go's own doc). buildChild deliberately
		// gives an attended child none of the channel-correlation labels above,
		// so correlateSessions can only ever resolve `active` to the WATCHING
		// session, never the child directly -- this is the one place that gap
		// gets closed.
		//
		// Redirecting HERE, before the initiative gate, is what lets the gate
		// admit the turn: the child's own SubagentRequest permits human
		// initiative (Task 1's PermitsHumanInitiative), so once `active` itself
		// IS the child, every step below -- the gate, the interact check, the
		// memory append, the wake -- naturally applies to the child, and
		// mirrorToAttendedParent further down mirrors this SAME turn back onto
		// the now-watching root exactly as it does any other child turn. The
		// redirect needs no explicit clearing: the very next inbound re-resolves
		// no live child here once the child goes terminal or is gone, and routes
		// to `active` (the root) as before.
		//
		// A List failure is fail-closed (InternalError, retried) rather than a
		// silent fall-through to the root: which conversation a human's message
		// belongs to is exactly the fact that could not be established.
		//
		// Scoped to a genuinely human turn (!nonHumanSubject): the root's own
		// channel is a human surface, and a service/agentsession-subject inbound
		// resolving to it (a bento resume, a sibling delegation's reply) is not
		// the person the child is being tested with -- redirecting THAT would
		// mis-deliver an unrelated message into the middle of someone else's
		// test.
		if !nonHumanSubject {
			if child, rerr := liveAttendedChildOf(ctx, p.K8s, active); rerr != nil {
				return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError}, rerr
			} else if child != nil {
				active = child
			}
		}

		// 1b. The initiative gate: a delegated child whose mode says it may ASK
		// but may not be DRIVEN refuses a turn a PERSON opened. Ahead of the
		// permission check on purpose — it is a property of the delegation, not
		// of the sender's standing, so a human with full `interact` on the child
		// is refused exactly as one without it is, and neither writes a
		// participant tuple or raises a join-request card for a message that was
		// never going to be delivered.
		if dec, refused, err := p.refuseUninvitedInitiative(ctx, active, ev, nonHumanSubject); err != nil || refused {
			return dec, err
		}

		// 1c. A session parked awaiting start approval holds standing for
		// NOBODY yet — intercept before the interact check, which would
		// otherwise route the guest (or a bystander) into the join flow
		// against an approver seat that deliberately has no tuple.
		if dec, handled, err := p.handleParkedStartApproval(ctx, active, ev); handled || err != nil {
			return dec, err
		}

		// 2. Permission check, via the InboundTurn pipeline's Interact hook.
		// Always through SpiceDB — there is deliberately no label/annotation
		// fast-path, because bypassing the check via Kube metadata hides bugs.
		// The canonical user is the sole principal.
		//
		// The hook checks fullyConsistent: decidePermission writes the
		// participant tuple then immediately replays the original inbound
		// through here, and MinimizeLatency can miss that tuple and silently
		// re-deny. The bounded per-inbound cost (single-digit ms on a hot
		// SpiceDB) is worth not re-discovering stale-read bugs. Same at
		// handlePermissionDeny's CheckDenied call.
		//
		// serviceSubject — and ONLY that half of nonHumanSubject — skips this
		// check entirely, the same way it already skips the started_by write on
		// the fresh-session path below. It is NOT an oversight to
		// special-case: agentsession's schema only ever admits `user` or
		// `group#member` on owner/started_by/participant (schema.zed), so a
		// `service:<id>` canonical could NEVER hold #interact — every resumed
		// delivery from a no-user-identity channel (github today; a future
		// bento resume, should one ever correlate to an existing session)
		// would hit a DEFINITIVE Deny and get routed to handlePermissionDeny,
		// which publishes a permission_request notice for what is routine
		// machine-originated traffic. The channel's own authzSubject already
		// IS the trust boundary here — verified once, upstream of Deliver, by
		// the transport itself (github's webhook HMAC, bento's operator-only
		// trigger) — so there is no per-message human identity left to ask
		// "may you continue this conversation" about. Found by
		// TestReviewbot_SecondPushToTheSamePRJoinsTheSameSession: a github
		// Channel's stable per-PR channelKey is the first case in this
		// codebase where a service-subject inbound resumes an EXISTING
		// session rather than always spawning a fresh one (bento's channelKey
		// is unique per firing, so this branch was previously unreachable for
		// any service-subject channel).
		//
		// An "agentsession:<ns>/<name>" subject is non-human too and is NOT
		// skipped: that subject type DOES resolve on the schema, and whether
		// one session may message another is exactly what the check answers.
		// Skipping it here would hand every session-to-session inbound an
		// unconditional pass — the one thing the agent-channel permission
		// exists to prevent.
		//
		// Verdict mapping (serviceSubject == false only):
		//   - Allow            → fall through to portal/liveness/append
		//   - Deny (transient) → OutcomeInternalError; a check error is NOT a
		//                        permission request, so drop/retry instead
		//   - Deny, non-human  → refused outright; a join request needs a person
		//   - Deny (definitive)→ handlePermissionDeny (blocklist + request)
		//   - Halt             → OutcomeInternalError (hook panic / publish error)
		if !serviceSubject {
			host := pipelinehost.New(pipelinehost.Deps{
				Session: pipelinehost.SessionRef{Namespace: active.Namespace, Name: active.Name},
			})
			out, runErr := p.Executor.Run(ctx, pipeline.InboundTurn, pipeline.Input{
				Session:   pipeline.SessionRef{Namespace: active.Namespace, Name: active.Name},
				Requester: canonical,
				Turn:      &pipeline.TurnInfo{Text: ev.MessageText},
			}, host)
			if runErr != nil {
				// The executor reserves its error return for host-primitive failures it
				// cannot turn into a verdict. Treat as internal error (drop/retry).
				return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError}, runErr
			}
			switch {
			case out.Verdict == pipeline.Deny && hooks.IsInteractCheckError(out.Reason):
				// Transient SpiceDB/check error, not a permission request. Surface
				// the underlying error so the caller drops/retries.
				return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
					fmt.Errorf("interact check: %s", out.Reason)
			case out.Verdict == pipeline.Deny && nonHumanSubject:
				// A definitive refusal of a NON-HUMAN acting subject. On this
				// branch that means one thing: a session messaging over an agent
				// channel without the lineage that authorizes it. (A service
				// subject never reaches here — it skipped the check above.)
				//
				// handlePermissionDeny is the human join-request flow, and none of
				// its inputs exist here: ev.ExternalIDs is empty by construction on
				// this branch, so it blocklist-checks an empty canonical and dedups
				// on an empty (kind, externalID). What it then does depends only on
				// which started-by annotations the session happens to carry, and
				// both outcomes are wrong:
				//
				//   - annotations that make an approver ADDRESSABLE (a starter
				//     subject with no email) => a durable pendingRequesters entry
				//     and an approval card asking a person to admit a requester
				//     with no identity to show them;
				//   - anything else => the no-approver arm, which raises a
				//     WARNING-level platform-admin monitoring event reading
				//     "user: tried to join session ..." — naming nobody — once per
				//     refused message.
				//
				// Refuse plainly instead, and say so in the log: the sender is a
				// process, and the fix is a lineage tuple or a channel binding, not
				// a click.
				log.FromContext(ctx).Info("pipeline: refusing inbound — acting subject is not authorized to message this session, and is not a human who could be asked to join",
					"session", active.Namespace+"/"+active.Name,
					"actor", canonical.SubjectRef().String(),
					"channel", channelRefForLog(ev.Channel),
					"reason", out.Reason)
				logAttachmentsDropped(ctx, active.Namespace, active.Name, channelRefForLog(ev.Channel),
					"an unauthorized non-human inbound", ev.Attachments)
				return channelkinds.InboundDecision{
					Outcome: channelkinds.OutcomeDeniedByPermission,
					// Explicit suppression, not an empty message: the sender is
					// another process, so there is no surface a notice would reach
					// and nothing on the far side that could act on one.
					Notice: notice.Suppressed("acting subject is not a human; a join request has nobody to address"),
				}, nil
			case out.Verdict == pipeline.Deny:
				return p.handlePermissionDeny(ctx, active, ev)
			case out.Verdict == pipeline.Halt:
				return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
					fmt.Errorf("interact pipeline halted: %s", out.Reason)
			}
		}
		// Allow (or a service subject, which skips the check) means fall through.
		if err := p.recordGoalActor(ctx, active, ev); err != nil {
			return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError}, err
		}

		// Portal-access chat trigger: "manage my accounts" and friends are UI
		// commands, not work for the agent. The triggerer publishes an
		// interaction_request(portal_access) on the session's .out subject and
		// we short-circuit — no memory append, no NATS wakeup, no liveness
		// routing into a tool. The listener still gets OutcomeRouted so it does
		// not fall back to an internal-error surface; the cosmetic "Starting…"
		// status is accepted, since the portal link arrives in parallel.
		if p.PortalAccess != nil {
			handled, err := p.PortalAccess.TryHandle(ctx, active, ev.MessageText)
			if err != nil {
				// Still "handled": the agent must never see the trigger phrase.
				// But the user is now waiting on a link that will never arrive,
				// and a plain Routed would post a "Starting…" placeholder with no
				// runner behind it until the silence watchdog called it stalled.
				// Say what happened instead of stranding them.
				log.FromContext(ctx).Info("portal-access TryHandle failed",
					"session", active.Namespace+"/"+active.Name, "err", err.Error())
				// This transition carries no ev.Attachments either — fold that
				// into the same notice, since InboundDecision holds exactly one.
				logAttachmentsDropped(ctx, active.Namespace, active.Name, channelRefForLog(ev.Channel), "a portal-access trigger", ev.Attachments)
				return channelkinds.InboundDecision{
					Outcome:              channelkinds.OutcomeHandledNoAgent,
					RequesterCanonicalID: canonical.String(),
					Notice:               portalFailureNotice(len(ev.Attachments)),
				}, nil
			}
			if handled {
				// Same attachment drop as the error branch, with no other
				// failure to fold the notice into. No channel kind's Routed
				// branch reads InboundDecision.Notice — only the terminal-
				// continuation outcomes do — so publish the notice directly.
				logAttachmentsDropped(ctx, active.Namespace, active.Name, channelRefForLog(ev.Channel), "a portal-access trigger", ev.Attachments)
				p.publishAttachmentsNotCarriedNotice(ctx, active.Namespace, active.Name, channelRefForLog(ev.Channel), len(ev.Attachments))
				return channelkinds.InboundDecision{
					Outcome:              channelkinds.OutcomeRouted,
					Session:              channelkinds.SessionInfo{Namespace: active.Namespace, Name: active.Name, Channel: active.Spec.InputChannel},
					RequesterCanonicalID: canonical.String(),
				}, nil
			}
		}

		// Agent health, asked about the session the message is being delivered
		// INTO. It does NOT gate delivery: a session whose class is invalid is
		// parked by the AgentSession controller and resumes when the class
		// recovers, answering the original message without the user re-sending.
		// It drives the SURFACING only — otherwise the user just watches the
		// thread do nothing.
		//
		// The other half of the pair lives in channelsd's listener manager,
		// which keeps a Channel's transport attached whenever the socket works
		// even if the Channel is invalid (transportViable). Without that the
		// message never arrives to be explained — it dies in the transport
		// layer.
		//
		// Asked HERE, below correlation, rather than from ev.Channel at the top
		// of Deliver: the Channel's own agentClass is the target's only when
		// the Channel is that session's binding, which a listener-driven
		// inbound guarantees and a bus handler naming its target does not. An
		// `agent` Channel is ONE conversation shared by a pair and carries the
		// CHILD's class, so resolving from it described the child on a child ->
		// parent delivery — naming a healthy agent for a stalled parent, and
		// saying nothing at all when the parent's own class was the broken one.
		// The target session's spec.class is the same answer for a
		// listener-driven inbound and the right one for both.
		//
		// A Running session is skipped: its pod is up and still replying (a
		// mid-session credential revocation is surgical), and claiming
		// otherwise while it visibly answers would be false.
		unhealthy, agentUnhealthy := p.unhealthyAgentFor(ctx, active.Namespace, active.Spec.Class)
		switch {
		case !agentUnhealthy:
			// Re-arm so a LATER outage of this agent is reported again rather
			// than swallowed as a repeat of one since fixed. Keyed on the same
			// class the alert is: re-arming a class nobody ever alerts on
			// leaves the alerted one armed forever. That an inbound resolving
			// no session now re-arms nothing is the correct pairing — such an
			// inbound cannot raise an alert either.
			p.outages.resolved(agentRef(active.Namespace, active.Spec.Class))
		case !liveRunner(active):
			p.surfaceUnhealthyAgent(ctx, ev, unhealthy,
				channelevents.SessionRef{Namespace: active.Namespace, Name: active.Name}, canonical)
		}

		// Liveness routing: while an interactive ToolCall is live for this
		// session, the channel thread belongs to the tool, not the agent.
		// Publish KindToolSessionInput onto IN so the runner's bridge feeds
		// the bytes to the tool's stdin instead of waking the agent.
		ref, lerr := p.liveInteractiveToolCall(ctx, active.Namespace, active.Name)
		if lerr != nil {
			// Liveness lookup failed. Falling through to agent routing is the
			// safe default, but a persistent failure mis-routes tool-stdin to
			// the agent with no diagnostic — log it so it's locatable.
			log.FromContext(ctx).V(1).Info("liveInteractiveToolCall lookup failed; routing to agent",
				"session", active.Namespace+"/"+active.Name, "err", lerr.Error())
		}
		if lerr == nil && ref != "" {
			pl := channelevents.ToolSessionInputPayload{
				ToolCallRef: ref,
				// canonical is already resolved, so it belongs in Subject:
				// ExternalID is raw-only and must never carry a canonical.
				Requester: channelevents.ExternalIdentity{Subject: canonical.Subject()},
				Data:      append([]byte(ev.MessageText), '\n'),
			}
			if perr := channelevents.PublishIn(p.NATS.Publish, active.Namespace, active.Name,
				channelevents.KindToolSessionInput, pl); perr != nil {
				return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
					fmt.Errorf("publish tool_session_input: %w", perr)
			}
			// A tool's stdin stream has no analog for ev.Attachments, so only
			// MessageText is forwarded. A Routed decision's Notice is never
			// read, so publish it directly.
			logAttachmentsDropped(ctx, active.Namespace, active.Name, channelRefForLog(ev.Channel), "live interactive-tool routing", ev.Attachments)
			p.publishAttachmentsNotCarriedNotice(ctx, active.Namespace, active.Name, channelRefForLog(ev.Channel), len(ev.Attachments))
			return channelkinds.InboundDecision{
				Outcome:              channelkinds.OutcomeRouted,
				Session:              channelkinds.SessionInfo{Namespace: active.Namespace, Name: active.Name, Channel: active.Spec.InputChannel},
				RequesterCanonicalID: canonical.String(),
			}, nil
		}

		// An adopted (mention_only) thread seeds the messages missed since the
		// last turn before the live message is appended. Default-routing
		// sessions skip this entirely.
		if p.ReadHistory != nil &&
			active.Spec.InputChannel != nil &&
			active.Spec.InputChannel.RoutingMode == "mention_only" {
			p.catchUp(ctx, active, ev)
		}

		// 3. Memory append + (respawn-on-wake ? annotation patch :) NATS publish.
		//
		// Attachments are processed here — not before the permission check
		// above — so a message denied interact never triggers a fetch; gating
		// controls bytes, never awareness, but a denied sender gets neither.
		content := p.attachmentContent(ctx, ev, active.Spec.Class, active.Namespace, active.Name)
		if err := p.Memory.Append(ctx, active.Namespace, active.Name, MemTurn{
			Role: "user", Author: authorSubject(ev), Via: ev.Via, Content: content,
		}); err != nil {
			return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
				fmt.Errorf("memory append: %w", err)
		}

		// Index the turn by its channel msg ref so the kind's restart UI can
		// resolve (clicked message) → (turn to cut at). Best-effort: logged,
		// never blocking. Skipped when the kind provides no ref.
		if ref := ev.MsgRef(); ref != "" {
			if idx, ok, qerr := p.lastInboxTurnIndex(ctx, active.Namespace, active.Name); ok && qerr == nil {
				if rerr := p.Memory.RecordChannelMsgRef(ctx, active.Namespace, active.Name, ev.ExternalIDs.Kind.String(), ref, idx); rerr != nil {
					log.FromContext(ctx).Info("RecordChannelMsgRef failed; restart UI will not find this msg",
						"session", active.Namespace+"/"+active.Name, "ref", ref, "err", rerr.Error())
				}
			} else if qerr != nil {
				log.FromContext(ctx).Info("lastInboxTurnIndex failed; skipping channel_msg_ref write",
					"session", active.Namespace+"/"+active.Name, "err", qerr.Error())
			}
		}

		// The wake-requested-at annotation makes the operator respawn a runner
		// for a Resume-disposition session whose pod has exited (Idle,
		// AwaitingRetry). Phases holding a live or starting pod (Running,
		// Pending, AwaitingDecision) pick the appended turn up over the NATS
		// wakeup below instead. AwaitingCredentials skips the respawn but keeps
		// both the append and the wakeup: the credential-link flow resumes that
		// runner, and respawning here would start it before the credential
		// exists.
		//
		// Called UNCONDITIONALLY. Eligibility is annotateWake's decision, made
		// against a fresh read under RetryOnConflict, and is deliberately not
		// gated on `active` — that snapshot is several round-trips old, so a
		// runner that ended its turn inside the window leaves it reading Running
		// while the live object reads Idle. Gating on it skipped the annotation,
		// the NATS wakeup reached an already-exited runner, and the appended
		// turn stranded as an undrained inbox entry until the user happened to
		// send another message.
		//
		// Bounded by wake credit when the message came from ANOTHER AGENT: the
		// append above already happened unconditionally, so a session out of
		// credit still sees the whole conversation and answers when a person
		// next speaks. Seeing is free; being driven is what is scarce.
		//
		// BEFORE either wake: write the annotations the turn's own output
		// depends on. The kind used to do this after Deliver returned, which is
		// after this point — so the runner could reply and the sender could
		// read the session before the write landed, and Slack's missing thread
		// anchor posted the reply top-level in the user's DM.
		preTurnStamped := p.stampPreTurnAnnotations(ctx, active, ev, canonical.String())

		// Both mechanisms live behind ONE call, applyWake, because either one
		// alone drives a turn: the annotation respawns an exited runner, and
		// the NATS wakeup nudges a live one. Two separately-gated call sites
		// is how a later edit gates one and not the other, and the half that
		// gets missed is the one that matters most — a running session is the
		// one most able to sustain a loop.
		if err := p.applyWake(ctx, active, ev); err != nil {
			return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
				fmt.Errorf("annotate wake: %w", err)
		}

		// If active is a live `attended` delegation child, its watching parent
		// (Task 5's LabelAttendedParentNamespace/Name pair) sees this same turn
		// unconditionally and wakes only on an @-mention of it — see
		// attended_watch.go. A no-op for every other session (attendedParentOf
		// returns ok=false), and best-effort: it never turns a successful child
		// inbound into a failed one.
		p.mirrorToAttendedParent(ctx, active, ev, content)

		// Re-surface any prompt this session is currently blocked on to the
		// device the user just re-interacted from. Purely additive: it only
		// acts on parked phases (never Running), so the enqueue-ack block
		// below is untouched.
		p.resurfacePending(ctx, active)

		// Mid-turn enqueue ack: the session is genuinely busy (a live turn in
		// flight) rather than merely parked, so tell the channel kind to post a
		// proactive "your message is queued" ephemeral, correlated by RequestID
		// with the interrupt button it renders alongside. queued also rides the
		// decision below, because a kind that announces a turn start must stay
		// quiet for a message that merely joined the queue — otherwise it
		// contradicts this ack and overwrites the live turn's progress caption.
		//
		// Running alone does not mean busy, hence the second term.
		// await_user_message keeps the pod alive at phase Running and records
		// the yield in status.awaitingUserInputSince, so on phase alone
		// "barged into a live turn" and "answered the question the agent just
		// asked" are indistinguishable. An awaited reply has nothing in flight
		// to queue behind, so offering to interrupt would offer to interrupt
		// nothing — and it must NOT suppress the turn-start status below, or
		// the resumed turn starts invisibly and leaves the silence watchdog
		// unarmed. internal/cmd/channelsd's watchdog reads this same scalar the same way.
		queued := active.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseRunning &&
			active.Status.AwaitingUserInputSince == nil
		// The ACK, unlike the flag, is addressed to a REQUESTER — both shapes
		// below carry one — and a non-human acting subject supplies none:
		// ev.ExternalIDs is empty by construction on that branch. The Slack
		// shape then fails its own Validate ("requester scope needs the
		// requester identity") and the legacy shape names nobody, so for a
		// session-to-session or cron inbound this can only produce a log line
		// or an unaddressable card, once per message. Same reasoning as the
		// nonHumanSubject arm of the deny switch above: a surface meant for a
		// person is skipped when there is no person. The flag still rides the
		// decision, because the message genuinely did queue behind a live turn.
		if queued && !nonHumanSubject {
			requestID := mintRequestID()
			// Slack sessions get the generic interaction_request(queued_messages)
			// so the App Home and mobile agent_view surfaces render it uniformly
			// with every other prompt. Other kinds still take KindEnqueueAck.
			if active.Spec.InputChannel != nil && active.Spec.InputChannel.Kind == "slack" {
				reqIdentity := toEnvelopeIdentity(ev.ExternalIDs)
				req := channelevents.InteractionRequestPayload{
					AgentSessionRef: channelevents.SessionRef{Namespace: active.Namespace, Name: active.Name},
					Category:        categories.QueuedMessages,
					RequestRef:      requestID, // correlates the interrupt round-trip
					Lead:            "You messaged while I'm working — your message is queued.",
					Body:            "", // the EnqueueAckPayload.Caption analog; no caption source wired yet
					Actions: []channelevents.InteractionAction{
						{ID: "interrupt", Label: "Interrupt & Send Now", Kind: channelevents.ActionKindDecision},
					},
					Audience: channelevents.InteractionAudience{
						Scope:     channelevents.AudienceRequester,
						Requester: &reqIdentity,
					},
				}
				// The ack is a side notification: the message is already queued
				// (appended, runner woken), so a malformed payload must not
				// report "failed" for something that in fact succeeded. Log and
				// fall through to the OutcomeRouted return below.
				if err := req.Validate(); err != nil {
					log.FromContext(ctx).Info("queued_messages: built invalid interaction_request, skipping ack publish",
						"session", active.Namespace+"/"+active.Name, "err", err.Error())
				} else {
					besteffort.Log(log.FromContext(ctx).Info, "publishQueuedInteraction",
						channelevents.PublishOut(p.NATS.Publish, active.Namespace, active.Name, channelevents.KindInteractionRequest, req),
						"session", active.Namespace+"/"+active.Name)
				}
			} else {
				ack := channelevents.EnqueueAckPayload{
					RequestID:  requestID,
					Requester:  toEnvelopeIdentity(ev.ExternalIDs),
					SessionRef: active.Namespace + "/" + active.Name,
				}
				besteffort.Log(log.FromContext(ctx).Info, "publishEnqueueAck",
					channelevents.PublishOut(p.NATS.Publish, active.Namespace, active.Name, channelevents.KindEnqueueAck, ack),
					"session", active.Namespace+"/"+active.Name)
			}
		}

		return channelkinds.InboundDecision{
			Outcome:                   channelkinds.OutcomeRouted,
			Session:                   channelkinds.SessionInfo{Namespace: active.Namespace, Name: active.Name, Channel: active.Spec.InputChannel},
			RequesterCanonicalID:      canonical.String(),
			Queued:                    queued,
			PreTurnAnnotationsStamped: preTurnStamped,
		}, nil
	}

	// New-session path: either fresh (no archived) or inheriting from archived.
	//
	// Spawn gate: some kinds own exactly ONE session for the lifetime of their
	// transport (the `local` TUI kind, whose single session `oap agent chat`
	// pre-creates). For those, an inbound reaching here must NOT spawn a phantom
	// `<channel>-<uuid>` session that runs orphaned — the caller's memory token
	// is bound to the original session and gets a 403 against the phantom. Asked
	// via SpawnsSessionOnInbound() rather than branching on the kind name.
	// OutcomeNoActiveSession is neither an error nor a deny, so the transport
	// can simply tell the user the interaction ended.
	if k, ok := chregistry.Get(ev.Channel.Spec.Kind); ok && !k.SpawnsSessionOnInbound() {
		log.FromContext(ctx).Info("inbound found no active session and kind does not spawn one; not delivered",
			"channel", ev.Channel.Namespace+"/"+ev.Channel.Name,
			"kind", ev.Channel.Spec.Kind, "channelKey", ev.ChannelKey)
		return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeNoActiveSession}, nil
	}

	// Terminal-session continuation. A correlated session in a terminal phase is
	// never a runner destination — routing an inbound into it strands the
	// message at "is starting…" with no runner. Two dispositions:
	//
	//   - Refuse: failed unrecoverably (budget, policy halt, scope-review
	//     failure, crash). Post a loud notice naming the reason and spawn
	//     nothing. Replying again re-refuses, so a silent retry is never
	//     implied; the user must start a new thread.
	//   - NewInheriting: finished, or a transient boot failure a clean retry
	//     recovers from. The continuation belongs in a FRESH session inheriting
	//     this transcript, so write an inherit fork-trigger on the terminal
	//     parent and let the operator's fork reconciler (owner-gated,
	//     denied-copying) do it. channelsd never performs the SpiceDB
	//     fork-writes or the transcript copy, and never routes into the
	//     terminal CR.
	if archived != nil {
		// Different-user takeover: someone who is NOT the terminal session's
		// started_by continues in the SAME thread as a NEW owner. Ordinary
		// terminal states inherit the transcript; policy/security halts start
		// fresh (IsPolicyHalt) so a halted conversation is never handed over.
		// "Different" is only identifiable when the parent stamped a started-by
		// external id; otherwise fall through to the same-owner path below. This
		// precedes the Refuse/NewInheriting split because takeover applies to
		// ANY terminal state for a new user.
		//
		// PhaseHeld is explicitly excluded from "terminal" here: a forensic hold
		// is a frozen, releasable containment decision, not a finished session —
		// its FailureReason is deliberately empty (Held is not Failed), so
		// IsPolicyHalt(archived.Status.FailureReason) below would read it as an
		// ordinary non-halt and hand a different user both the takeover AND the
		// held session's transcript. A held session must refuse EVERY user, the
		// same way it already refuses its own started_by (the archivedDisp
		// DispRefuse check a few lines down) — so it never reaches this branch.
		//
		// The absence of a SpiceDB check here is deliberate and worth reading
		// twice. The same-owner fallback below asks "may you continue as this
		// session's owner?" — the wrong question for a takeover, whose child is
		// owned by the NEW user and deliberately does NOT inherit the parent's
		// interact (WriteSpiceDBParticipants). Asking it would deny every
		// legitimate takeover. The authorization basis is the channel: this
		// sender reached the thread, the same basis on which they could start a
		// fresh session, and the archived==nil path makes no SpiceDB call
		// either. The operator's fork gate is skipped for takeover for that same
		// reason, which makes THIS branch the choke point.
		//
		// Inheritance grants exactly one thing over a fresh start: reading the
		// predecessor's transcript, which holds tool output and memory
		// retrievals never posted to the thread. That cross-user exposure was
		// weighed and accepted for a channel-as-trust-boundary deployment, with
		// policy halts carved out — a product decision, not an oversight.
		// Gating inheritance on interact, or defaulting to no-history, narrows a
		// shipped feature to the rejected alternative, so it needs that decision
		// made again rather than a patch here. Pinned by
		// TestTakeoverInheritsWithoutInteractCheck_ByDesign.
		if startedBy := archived.Annotations[spiceboxv1alpha1.AnnotationStartedByExternalID]; startedBy != "" &&
			startedBy != ev.ExternalIDs.ExternalID.String() &&
			archived.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseHeld {
			inheritHistory := !lifecyclecore.IsPolicyHalt(archived.Status.FailureReason)
			wrote, werr := p.writeTakeoverForkTrigger(ctx, archived, ev, canonical, inheritHistory)
			if werr != nil {
				return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
					fmt.Errorf("write takeover fork-trigger on %s/%s: %w", archived.Namespace, archived.Name, werr)
			}
			// PendingRestart.NewUserText carries ev.MessageText but has no field
			// for ev.Attachments. Deliberately not gated on wrote: a follow-up
			// arriving after the takeover is already pending still had a file on
			// it, and the user still needs telling.
			logAttachmentsDropped(ctx, archived.Namespace, archived.Name, channelRefForLog(ev.Channel), "a thread takeover", ev.Attachments)
			var takeover *notice.Notice
			switch {
			case wrote:
				// The one canonical ack for this fork, folding in any dropped
				// attachments rather than raising a second message.
				takeover = takeoverNotice(ev.ExternalIDs.Kind.String(), ev.ExternalIDs.ExternalID.String(), inheritHistory, len(ev.Attachments))
			case len(ev.Attachments) > 0:
				// The takeover already acked on an earlier inbound, so
				// re-announcing it would be spam — but this attachment is new
				// information the user has not been told about yet.
				takeover = attachmentsNotCarriedNotice(len(ev.Attachments))
			}
			return channelkinds.InboundDecision{
				Outcome:              channelkinds.OutcomeForkPending,
				RequesterCanonicalID: canonical.String(),
				Notice:               takeover,
			}, nil
		}

		if refuse, ok := archivedDisp.(lifecyclecore.DispRefuse); ok {
			log.FromContext(ctx).Info("inbound for unrecoverably-failed session; refusing (no fresh session)",
				"session", archived.Namespace+"/"+archived.Name, "reason", refuse.Reason)
			return channelkinds.InboundDecision{
				Outcome: channelkinds.OutcomeRefused,
				Notice:  refusalNotice(refuse.Reason),
			}, nil
		}

		// NewInheriting, SAME owner — the takeover branch above already claimed
		// every mismatched started-by. A parent that stamped a started-by
		// external id vouches for this sender and the fork proceeds. A session
		// with NO such annotation cannot be vouched for that way, so fall back
		// to a fully-consistent SpiceDB interact check on the terminal parent
		// before letting the sender fork its history — the annotation is never a
		// fast-path. FAIL-CLOSED: a missing authz client or a check error
		// refuses. The operator's owner-only fork gate remains the authoritative
		// backstop; this is defense in depth so channelsd never writes a
		// fork-trigger for an entirely unverifiable sender.
		archivedStartedBy := archived.Annotations[spiceboxv1alpha1.AnnotationStartedByExternalID]
		// Channel-as-boundary: a service-triggered session (a github/cron channel)
		// records the service it authorizes as on AuthzServiceSubject, but never a
		// started_by (the schema declares `relation started_by: user`) and never a
		// human interact grant. When the SAME service re-delivers on the SAME
		// channel — a new PR event, a webhook redelivery, all already past that
		// channel's signature verification — the verified channel IS the
		// authorization boundary, exactly as the takeover branch reasons for a
		// human thread. Scoped tightly: only the session's OWN service skips the
		// check below; any other subject still goes through it — EXCEPT that a
		// non-user subject (service, agentsession, ...) can never go through it
		// either, which the next guard handles before the call is ever made.
		svc := spiceboxv1alpha1.AuthzServiceSubject(archived)
		sameServiceReturning := !svc.IsZero() && svc.String() == canonical.String()
		if archivedStartedBy == "" && !sameServiceReturning {
			// A non-user subject can never hold agentsession#interact: the
			// schema's interact relation is user|group only, and checkUser
			// always sends the canonical verbatim as a "user:<canonical>"
			// object id. A type-prefixed canonical (already "service:<id>" or
			// "agentsession:<ns>/<name>") would reach SpiceDB as an object id
			// carrying that embedded colon, which the object_id grammar
			// rejects outright — a guaranteed InvalidArgument, not a maybe, by
			// construction of checkUser and the grammar, independent of which
			// service or session it names. Recognize that before the call
			// instead of after it fails: same fail-closed outcome the
			// !allowed branch below reaches, correctly shaped as a clean
			// denial instead of an opaque RPC error that strands the caller
			// with nothing to resolve (a webhook-triggered caller has no
			// session to retry from and no user to notify).
			//
			// SubjectRef() always returns a FULLY QUALIFIED reference — it
			// prefixes a bare canonical with "user:" rather than leaving it
			// untyped — so ObjectType() is never "" here; a real human is
			// "user", which is excluded alongside "".
			if objType := canonical.SubjectRef().ObjectType(); objType != "" && objType != "user" {
				log.FromContext(ctx).Info("inherit fork: requester is a non-user subject; interact can never hold for it, denying (fail-closed)",
					"session", archived.Namespace+"/"+archived.Name, "requester", canonical, "subjectType", objType)
				return channelkinds.InboundDecision{
					Outcome: channelkinds.OutcomeDeniedByPermission,
					Notice:  archivedSessionNotYoursNotice(ev.ExternalIDs.Kind.String(), ev.ExternalIDs.ExternalID.String(), ""),
				}, nil
			}
			if p.Authz == nil {
				log.FromContext(ctx).Info("inherit fork: no started-by annotation and no authz client to verify sender; refusing (fail-closed)",
					"session", archived.Namespace+"/"+archived.Name, "requester", canonical)
				return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
					fmt.Errorf("inherit fork on %s/%s: cannot verify sender (no authz client)", archived.Namespace, archived.Name)
			}
			allowed, cerr := p.Authz.CheckInteract(ctx, archived.Namespace, archived.Name, canonical, true)
			if cerr != nil {
				log.FromContext(ctx).Info("inherit fork: interact check errored; refusing (fail-closed)",
					"session", archived.Namespace+"/"+archived.Name, "requester", canonical, "err", cerr.Error())
				return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
					fmt.Errorf("inherit fork interact check on %s/%s: %w", archived.Namespace, archived.Name, cerr)
			}
			if !allowed {
				log.FromContext(ctx).Info("inherit fork: sender lacks interact on the terminal parent and no started-by annotation vouches for them; denying",
					"session", archived.Namespace+"/"+archived.Name, "requester", canonical)
				return channelkinds.InboundDecision{
					Outcome: channelkinds.OutcomeDeniedByPermission,
					Notice:  archivedSessionNotYoursNotice(ev.ExternalIDs.Kind.String(), ev.ExternalIDs.ExternalID.String(), ""),
				}, nil
			}
		}
		wrote, err := p.writeInheritForkTrigger(ctx, archived, ev.MessageText, canonical)
		if err != nil {
			return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
				fmt.Errorf("write inherit fork-trigger on %s/%s: %w", archived.Namespace, archived.Name, err)
		}
		// PendingRestart.NewUserText carries ev.MessageText but has no field for
		// ev.Attachments. Deliberately not gated on wrote: a follow-up arriving
		// after the fork is already pending still had a file on it.
		logAttachmentsDropped(ctx, archived.Namespace, archived.Name, channelRefForLog(ev.Channel), "an inherit fork", ev.Attachments)
		var inherited *notice.Notice
		switch {
		case wrote:
			// Ack exactly once, on the inbound that armed the fork, folding in
			// any dropped attachments. A later reply while the fork is still
			// pending is already covered by this ack.
			inherited = inheritNotice(len(ev.Attachments))
		case len(ev.Attachments) > 0:
			// The continuation already acked on an earlier inbound, so repeating
			// it would be spam — but this attachment is new information.
			inherited = attachmentsNotCarriedNotice(len(ev.Attachments))
		}
		return channelkinds.InboundDecision{
			Outcome:              channelkinds.OutcomeForkPending,
			RequesterCanonicalID: canonical.String(),
			Notice:               inherited,
		}, nil
	}

	// Fresh-session path (no terminal predecessor: archived == nil below).
	caps := bindingCaps(p.Capabilities(ev.Channel.Spec.Kind))
	sessName := makeSessionName(ev.Channel, ev.ChannelKey)
	prefix := channelevents.SubjectPrefix(ev.Channel.Namespace, sessName)

	// No InheritFrom on this path: a terminal predecessor is either forked
	// (NewInheriting) or refused above, so nothing reaching here has one.
	inheritFrom := ""

	// Thread adoption: a "reply" mention whose channelKey has never had a
	// session — backfill the thread. Nil ReadHistory means no backfiller.
	adopting := ev.ThreadEntry == channelkinds.ThreadEntryReply &&
		p.ReadHistory != nil

	// Bind-time half of the 1:1 thread↔agent invariant. Filtering the lookup
	// above stops a foreign agent ROUTING into this thread's session; without
	// this guard it would instead fall through and CREATE a second session
	// bound to the same thread — the same cross-talk, one step later.
	//
	// Adoption is the deliberate exception: a human @-mentions another agent
	// into the thread, and that agent gets its own session on its own
	// mention_only binding. A second binding by design, so the guard skips it.
	if !adopting {
		owner, err := p.threadOwnedByAnotherAgent(ctx, ev, keyHash)
		if err != nil {
			return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
				fmt.Errorf("check thread ownership: %w", err)
		}
		if owner != "" {
			// Not surfaced to the user: the thread's owning agent is handling
			// this same message on its own listener, so the human is not left
			// waiting. Logged because a drop must never be silent.
			log.FromContext(ctx).Info("thread is owned by another agent; refusing to bind a second session to it",
				"channel", ev.Channel.Namespace+"/"+ev.Channel.Name,
				"agentClass", ev.Channel.Spec.AgentClass,
				"threadKey", ev.ChannelKey, "ownedBy", owner)
			return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeThreadOwnedByAnotherAgent}, nil
		}
	}

	// Session-START gate (start_gate.go): on an org-membership-attributing
	// channel, a non-member needs agentclass#start_session — or a platform
	// admin's approval — before a session runs for them at all. AFTER the
	// thread-ownership guard, so a message another agent's session owns keeps
	// its silent refusal instead of gaining a gate notice; BEFORE
	// construction, so a fail-closed refusal creates nothing. A parked create
	// still builds the session below, minus every authz standing write, and
	// finalizeStartApproval raises the admin card after Create.
	startParked := false
	var startAdminSubjects []string
	startRequestRef := ""
	if startGateApplies(ev, nonHumanSubject) {
		verdict, gerr := p.evaluateStartGate(ctx, ev, canonical)
		if gerr != nil {
			log.FromContext(ctx).Info("start gate: fail-closed refusal",
				"channel", channelRefForLog(ev.Channel), "requester", ev.ExternalIDs.ExternalID.String(), "err", gerr.Error())
			return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError}, gerr
		}
		if !verdict.allowed {
			if len(verdict.adminSubjects) == 0 {
				return p.refuseUnstartableGuest(ctx, ev), nil
			}
			startParked = true
			startAdminSubjects = verdict.adminSubjects
			startRequestRef = "startappr-" + sessName + "-" + mintRequestID()
		}
	}

	// Fetch the backfill window before building the session so the
	// transcript can seed spec.Prompt in order (history then question).
	var adoptionPage channelkinds.HistoryPage
	if adopting {
		adoptionPage = p.fetchAdoptionHistory(ctx, ev)
	}

	// Cron / split-channel: a role=input Channel delivers its reply into a
	// DIFFERENT Channel, so bind that target at creation. The relay,
	// resolve.ForSession and the slack thread index all prefer
	// spec.outputChannel; without it an input-only kind (bento) falls back to
	// its own nopSender and the reply is undeliverable.
	//
	// role=both is both origin and destination, so the nil-OutputChannel
	// fallback is correct for it and it skips this deliberately.
	var outBinding *spiceboxv1alpha1.ChannelBinding
	var outKeyHash string
	// sessionOpening is the line that will occupy the thread root of that
	// destination — see the SessionOpening call below.
	var sessionOpening string
	if ev.Channel.Spec.Role == spiceboxv1alpha1.ChannelRoleInput {
		outCh, err := outputbind.Resolve(ctx, p.K8s, ev.Channel.Namespace, ev.Channel.Spec.AgentClass)
		if err != nil {
			log.FromContext(ctx).Info("outbound binding unresolvable; refusing to create an undeliverable session",
				"channel", ev.Channel.Namespace+"/"+ev.Channel.Name,
				"agentClass", ev.Channel.Spec.AgentClass, "err", err.Error())
			return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError}, err
		}
		outKey, outExternal, err := outputbind.Anchor(outCh)
		if err != nil {
			log.FromContext(ctx).Info("outbound anchor unavailable; refusing to create an undeliverable session",
				"channel", ev.Channel.Namespace+"/"+ev.Channel.Name,
				"outputChannel", outCh.Name, "err", err.Error())
			return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError}, err
		}
		outBinding = &spiceboxv1alpha1.ChannelBinding{
			Name:              outCh.Name,
			Kind:              outCh.Spec.Kind,
			Key:               outKey,
			Capabilities:      bindingCaps(p.Capabilities(outCh.Spec.Kind)),
			NATSSubjectPrefix: prefix,
			External:          outExternal,
		}
		outKeyHash = channelkey.LabelValue(outKey)
		// For a thread-per-session destination the anchor above can only name
		// the channel; the thread ROOT is whatever lands there first. This
		// inbound may have carried no human — a webhook payload, a cron tick —
		// in which case there is no message of the user's to be that first
		// thing, and the thread would open on whatever the agent happened to
		// emit (a plan checklist, a status caption) with nothing saying what it
		// is about. Ask the trigger's own kind to describe itself and carry the
		// answer on the session; channelsd's outbound relay posts it ahead of
		// the first output and takes the root from that send.
		//
		// Derived HERE because this is where the input Channel is in hand, and
		// it is the only object that can answer whether this session's inbound
		// carried a human. An empty answer is the common case and means the
		// session behaves exactly as it does today.
		if opening, ok := outputbind.SessionOpening(ev.Channel, ev.ChannelKey, ev.DeliveryEvent, ev.RawDelivery); ok {
			sessionOpening = opening
		}
	}

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sessName,
			Namespace: ev.Channel.Namespace,
			// Pre-stamp the operator's finalizer at Create. The operator's
			// EnsureFinalizer runs its admission slot-tuple sweep ONLY on the
			// reconcile that transitions the finalizer absent -> present (its
			// added=true branch). That sweep races the mint-time slot binds this
			// path performs right after the session exists (BindTriggerSlots,
			// seedThreadSlots): the two are unordered, so the sweep can delete the
			// legitimate authority — including the pin — those binds just wrote,
			// deterministically when the operator is briefly backlogged. Stamping
			// the finalizer here makes added=false on the operator's first
			// reconcile, so its sweep never fires for a channelsd mint; this path
			// sweeps stale tuples itself, synchronously BEFORE binding (see the
			// DeleteSlotGrants call below). The operator's sweep still runs for
			// every OTHER creation path (CLI, kubectl), which do not pre-stamp, and
			// its fork-child exemption is untouched.
			Finalizers: []string{spiceboxv1alpha1.FinalizerAgentSession},
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: ev.Channel.Name,
				spiceboxv1alpha1.LabelChannelKind: ev.Channel.Spec.Kind,
				spiceboxv1alpha1.LabelChannelKey:  keyHash,
			},
			// started-by external id + verified email, always. No archived
			// predecessor on this path (terminal sessions fork via the operator
			// above), so there are no inherited backfill cursors to carry.
			// The kind's pre-turn annotations are folded in HERE, at
			// construction, rather than patched on afterwards. On this path
			// nothing wakes anything — the operator starts the runner when it
			// observes the new session — so there is no later moment that is
			// reliably "before the turn". Putting them on the object as it is
			// created removes the window instead of narrowing it: the runner
			// cannot observe a session that lacks them, because no such version
			// of the object ever exists.
			Annotations: withPreTurnAnnotations(
				inheritedAnnotations(ev.ExternalIDs.ExternalID.String(), startedBySubjectAnnotation(canonical, nonHumanSubject), ev.ExternalIDs.Email.String(), nil),
				ev, canonical.String()),
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:         ev.Channel.Spec.AgentClass,
			AgentIdentity: ev.Channel.Spec.AgentIdentity,
			Prompt:        spiceboxv1alpha1.PromptSource{Inline: adoptionPrompt(ev.MessageText, adoptionPage)},
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name:              ev.Channel.Name,
				Kind:              ev.Channel.Spec.Kind,
				Key:               ev.ChannelKey,
				Capabilities:      caps,
				NATSSubjectPrefix: prefix,
				External:          ev.External,
				InheritFrom:       inheritFrom,
				RoutingMode:       adoptionRoutingMode(adopting, nil),
			},
			OutputChannel: outBinding,
		},
	}
	// Cron-spawned sessions are resolved by LabelOutputChannelKey rather than
	// LabelChannelKey (their inbound anchor is per-firing and never repeats),
	// so stamp it whenever a binding exists.
	if outKeyHash != "" {
		sess.Labels[spiceboxv1alpha1.LabelOutputChannelKey] = outKeyHash
	}
	// Tell the runner an inbox turn is still owed for turn 0 before it may
	// dispatch. This MUST ride on the Create itself: the operator spawns the
	// runner off the object's existence, so anything stamped afterwards can
	// lose the race it exists to close. See AnnotationInboundAttachmentCount.
	if n := len(ev.Attachments); n > 0 {
		sess.Annotations[spiceboxv1alpha1.AnnotationInboundAttachmentCount] = strconv.Itoa(n)
	}
	// The thread-opening line, for the same reason and on the same terms: it
	// must be on the object the relay reads before the runner this Create
	// spawns can publish anything. Empty for every session whose inbound
	// carried a human, and for a trigger whose kind offers no sentence.
	if sessionOpening != "" {
		sess.Annotations[spiceboxv1alpha1.AnnotationSessionOpening] = sessionOpening
	}
	// The acting subject for a session with no human in it, on the Create for
	// the same reason as the two above: the runner resolves the subject it
	// authorizes tool calls as from the object the operator spawned it off.
	//
	// This is what the branch at the top of Deliver validated the Channel's
	// declared subject FOR. Suppressing the started_by write is the other half
	// of that branch and not a use of the value: without this stamp the subject
	// would be proven and then dropped, leaving the runner to authorize every
	// governed tool call as nobody — a malformed SpiceDB check rather than an
	// answerable one. started_by remains unwritten either way; see
	// touchStartedByUnlessService for why it cannot hold this subject.
	if serviceSubject {
		sess.Annotations[spiceboxv1alpha1.AnnotationAuthzServiceSubject] = canonical.String()
	}
	// The start-approval marker rides the Create itself, like the two
	// annotations above and for the same reason: the operator spawns runners
	// off the object's existence, so a marker patched afterwards could lose
	// the very race it exists to close. No version of a parked session ever
	// exists without it.
	if startParked {
		sess.Annotations[spiceboxv1alpha1.AnnotationStartApprovalRequestRef] = startRequestRef
	}
	created := false
	if err := p.K8s.Create(ctx, sess); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError}, err
		}
		// Existed concurrently — get it. The other channelsd replica that won
		// the Create race owns the follow-up writes.
		key := types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}
		besteffort.Log(log.FromContext(ctx).Info, "Get session after AlreadyExists race",
			p.K8s.Get(ctx, key, sess), "session", key.String())
	} else {
		created = true
	}

	// Admission sweep for a channelsd mint: wipe whatever a DEAD PREDECESSOR with
	// this same ns/name left in SpiceDB BEFORE this session does anything with its
	// slots. A never-expiring stale slot_pin would otherwise refuse the new
	// session its own first bind, and a stale slot_grant_* would resolve
	// slot_grant->interact for the NEW session (the pin/grant subject is ns/name,
	// not UID) — a leaked authority under the reused name.
	//
	// This IS the operator's admission sweep, MOVED to the creator. This path
	// pre-stamped the operator's finalizer (see the Create site), which makes the
	// operator's EnsureFinalizer added=false and so suppresses its own sweep for a
	// channelsd mint; doing it here runs it synchronously ahead of — rather than
	// unordered against — the mint-time binds below (BindTriggerSlots,
	// seedThreadSlots).
	//
	// Gated on `created`: only the replica that won the Create race owns the
	// follow-up writes, so only it sweeps (the AlreadyExists-race loser must not).
	// Runs BEFORE the startParked return below as well, so a parked session whose
	// name was reused is swept too — the operator no longer will. p.Authz is
	// nil-checked because a created session that binds nothing still reaches here.
	//
	// Fail CLOSED, like the started_by write just below: a leftover grant resolves
	// for the new session, so proceeding past a failed sweep would run it over
	// unverified authority. status.startFailure is the channelsd-owned signal the
	// operator reads to drive Failed; the error still propagates.
	if created && p.Authz != nil {
		if err := p.Authz.DeleteSlotGrants(ctx, sess.Namespace, sess.Name); err != nil {
			log.FromContext(ctx).Info("mint admission sweep: deleting stale slot grants/pins failed; failing the session rather than binding over unverified authority",
				"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
			failed := sess.DeepCopy()
			failed.Status.StartFailure = &spiceboxv1alpha1.AgentSessionStartFailure{
				Reason:  spiceboxv1alpha1.ReasonAgentSessionAuthzWriteFail,
				Message: err.Error(),
			}
			if werr := applyApprovalStatus(ctx, p.K8s, failed, sess); werr != nil {
				log.FromContext(ctx).Info("set startFailure signal after mint admission sweep failure",
					"session", sess.Namespace+"/"+sess.Name, "err", werr.Error())
			}
			return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError}, err
		}
	}

	// started_by is user-only: the schema declares `relation started_by: user`
	// and the write hardcodes ObjectType "user". A "service:<id>" canonical
	// would land as `user:service:<id>`, whose object_id embeds a colon and is
	// rejected by SpiceDB's object-id regex, failing every cron session with
	// AuthzWriteFailed.
	//
	// A cron session has no human starter, so it simply has no started_by — the
	// same state kubectl-driven sessions are in. Ownership arrives separately
	// from the operator's owner resolver. Admitting `service` as a real
	// started_by is a schema change and an authz decision, since started_by
	// confers interact.
	//
	// A start-approval PARK suppresses the write for the same reason in
	// reverse: started_by confers interact, and the whole point of the park
	// is that this human has no standing until a platform admin approves —
	// decideStartApproval writes it on the Approve click.
	if err := p.touchStartedByUnlessNonHumanOrParked(ctx, sess, canonical, nonHumanSubject, startParked); err != nil {
		// status.startFailure is the channelsd-owned signal, so the authz-write
		// failure is visible on status and not only in logs. The operator reads
		// it next reconcile and drives the Failed state; channelsd never writes
		// those operator-owned fields directly. The error still propagates.
		log.FromContext(ctx).Info("TouchStartedBy: authz write failed; setting startFailure signal",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		failed := sess.DeepCopy()
		failed.Status.StartFailure = &spiceboxv1alpha1.AgentSessionStartFailure{
			Reason:  spiceboxv1alpha1.ReasonAgentSessionAuthzWriteFail,
			Message: err.Error(),
		}
		if werr := applyApprovalStatus(ctx, p.K8s, failed, sess); werr != nil {
			log.FromContext(ctx).Info("set startFailure signal after authz write failure",
				"session", sess.Namespace+"/"+sess.Name, "err", werr.Error())
		}
		return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError}, err
	}

	// Fresh session on an unhealthy agent: the controller will park it at
	// ClassResolved=False and no runner will come up, so the thread would
	// otherwise sit at "starting…" indefinitely. There is no liveRunner check
	// here — a session created moments ago has no pod by construction.
	//
	// Asked of the session's OWN class for the same reason the active-session
	// arm above does. Here the two answers happen to agree — a session created
	// from a Channel carries that Channel's class — but asking the session
	// keeps one question with one answer rather than two that can diverge.
	if unhealthy, agentUnhealthy := p.unhealthyAgentFor(ctx, sess.Namespace, sess.Spec.Class); agentUnhealthy {
		p.surfaceUnhealthyAgent(ctx, ev, unhealthy,
			channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name}, canonical)
	} else {
		p.outages.resolved(agentRef(sess.Namespace, sess.Spec.Class))
	}

	// Interact-policy snapshot: only on the create path (not the AlreadyExists
	// re-fetch). Look up the AgentClass to read SessionInteractPermission;
	// capture the value per-session so future spec edits don't retroactively
	// change existing sessions.
	//
	// Declared OUTSIDE the `created` block because thread adoption below needs
	// it too: the policy decides which thread authors adoption may grant, and
	// it must be the same value this snapshot applied.
	var appliedInteractPolicy string
	// Captured alongside the policy for the same reason: thread adoption below
	// needs the class's slot declarations, and re-fetching there could read a
	// spec edited between the two reads.
	var slotSeedRequests []authz.ThreadSeedRequest
	var slotSeedExpiration time.Duration
	var ownership channelkinds.SessionOwnership
	// Also captured alongside the policy, for the trigger-slot binding
	// stanza further below: it needs the class's slot declarations too, and a
	// second Get there could read a spec edited between the two reads. Its
	// zero value (no Authz block, no slots) is the correct fallback when the
	// Get below fails — TriggerSlotRequestsFor reads it as "no slots" rather
	// than panicking on a nil pointer.
	var class spiceboxv1alpha1.AgentClass
	// A parked create skips the whole snapshot: the participant tuple is an
	// interact grant (decideStartApproval writes it on Approve, from the same
	// EffectiveSessionInteractPermission read), and announcing ownership for
	// a session an admin may yet deny would be a lie.
	if created && !startParked {
		logger := log.FromContext(ctx)
		if err := p.K8s.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Spec.Class}, &class); err != nil {
			// Class missing (or API error) — skip participant write and condition.
			// Log so operators can diagnose; not a fatal error for the inbound.
			logger.Info("interact-policy snapshot skipped: AgentClass not found",
				"session", sess.Name, "class", sess.Spec.Class, "error", err)
		} else {
			// Resolve ownership with the SAME function the operator uses to
			// write the tuple, so the sentence a channel posts and the subject
			// SpiceDB holds can never disagree. Errors are not fatal here: the
			// operator resolves authoritatively and fails the session if it
			// cannot: this call only decides what to SAY.
			ownership = resolveSessionOwnership(ctx, p.K8s, sess, ev.Channel, &class)
			slotSeedRequests = threadSeedRequestsFor(&class)
			// EffectiveSettings is stamped by the AgentClass reconciler and is nil
			// until it has run. Zero here is not "no expiry" — SlotGrantExpiry
			// falls back to its own conservative default.
			if class.Status.EffectiveSettings != nil {
				slotSeedExpiration = class.Status.EffectiveSettings.Budget.SessionExpiration.Duration
			}

			patched := sess.DeepCopy()
			// The EFFECTIVE policy — what the class declared, or what its
			// reconciler derived onto status from the single role=output
			// Channel's membership. Reading only the declared half snapshotted
			// nothing for a derived class, so no participant tuple was written
			// and the room the agent posts into could not interact with the
			// session it announced.
			interactPerm := class.EffectiveSessionInteractPermission()
			appliedInteractPolicy = interactPerm
			patched.Status.AppliedInteractPermission = interactPerm
			now := metav1.NewTime(p.Now())
			patched.Status.AppliedInteractPermissionAt = &now

			if interactPerm == "" {
				conditions.SetTrue(patched, &patched.Status.Conditions,
					spiceboxv1alpha1.AgentSessionConditionInteractPolicyApplied,
					spiceboxv1alpha1.ReasonInteractPolicyNotConfigured)
			} else {
				writeErr := p.Engine.TouchInteractParticipant(ctx, authz.SessionRef{Namespace: sess.Namespace, Name: sess.Name}, interactPerm)
				if writeErr != nil {
					logger.Info("interact-policy participant write failed; condition recorded",
						"session", sess.Name, "subject", interactPerm, "error", writeErr)
					conditions.SetFalse(patched, &patched.Status.Conditions,
						spiceboxv1alpha1.AgentSessionConditionInteractPolicyApplied,
						spiceboxv1alpha1.ReasonInteractPolicySpiceDBWriteFailed,
						writeErr.Error())
				} else {
					conditions.SetTrue(patched, &patched.Status.Conditions,
						spiceboxv1alpha1.AgentSessionConditionInteractPolicyApplied,
						spiceboxv1alpha1.ReasonInteractPolicyApplied)
				}
			}

			besteffort.Log(log.FromContext(ctx).Info, "apply InteractPolicyApplied status", applyApprovalStatus(ctx, p.K8s, patched, sess),
				"session", sess.Namespace+"/"+sess.Name)
		}
	}

	if created {
		if err := p.recordGoalActor(ctx, sess, ev); err != nil {
			return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError}, err
		}
	}

	// New-session attachments. The raw message text already landed in
	// spec.Prompt.Inline, which the runner's cold-start path places as turn 0,
	// but that is a bare string with no room for the composed attachment text
	// (a manifest line per file read, a note per one that wasn't) or the
	// structured blocks. Attaching a file to the FIRST message of a thread is
	// the most common case, and nothing downstream of session-creation calls
	// processAttachments, so without this it drops silently.
	//
	// The text and blocks go in as a SEPARATE "inbox"-role turn rather than
	// being folded into the spec.Prompt string: the runner's drainInbox runs
	// right after placing turn 0 and BEFORE its first LLM request, so this
	// lands in the very first context window alongside the raw text, over the
	// same mechanism every other inbound message uses.
	//
	// AnnotationInboundAttachmentCount (stamped on the Create above) fences the
	// runner's first dispatch on this write. Each file costs a
	// fetch/upload/extract round trip and the runner is spawned off the
	// AgentSession's existence, so without the fence it reaches the model while
	// this is still running and answers a message it has not finished receiving.
	//
	// Gated on created, never on the AlreadyExists race: re-running would
	// re-fetch and re-upload the same attachment a second time.
	if created && len(ev.Attachments) > 0 {
		attachmentText, blocks := p.processAttachments(ctx, ev, sess.Spec.Class, sess.Namespace, sess.Name)
		if attachmentText == "" && len(blocks) == 0 {
			// Unreachable by construction: every outcome renders either a
			// manifest line or a note line, so a non-empty list always composes
			// something. Logged rather than assumed away because the fence
			// clears on THIS append — if it were ever skipped the runner would
			// sit out its whole budget, and silence here would make that look
			// like a runner bug.
			log.FromContext(ctx).Info("new-session attachment pass produced no turn content; the runner's first turn will wait out its attachment budget",
				"session", sess.Namespace+"/"+sess.Name, "attachments", len(ev.Attachments))
		} else {
			content := make([]MemContent, 0, 1+len(blocks))
			if attachmentText != "" {
				content = append(content, MemContent{Type: "text", Text: attachmentText})
			}
			content = append(content, blocks...)
			if err := p.Memory.Append(ctx, sess.Namespace, sess.Name, MemTurn{
				Role: "inbox", Author: authorSubject(ev), Via: ev.Via, Content: content,
			}); err != nil {
				// Best-effort: the session was already created — the expensive,
				// hard-to-retry part — so failing the whole inbound is worse
				// than losing this one attachment note. Never silent.
				log.FromContext(ctx).Info("new-session attachment turn append failed",
					"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
			}
		}
	}

	// Parked create: everything a live session still needs is in place (the
	// object with its marker annotation, the prompt as turn 0, the
	// attachment turns above), and everything that confers standing or
	// announces the session was skipped. Stamp the durable pending entry,
	// raise the admin card, and stop — the operator will hold the session in
	// AwaitingStartApproval until decideStartApproval unparks or fails it.
	// The AlreadyExists-race loser answers the inbound and writes nothing:
	// the winning replica owns the stamp and the publish.
	if startParked {
		if !created {
			return channelkinds.InboundDecision{
				Outcome: channelkinds.OutcomeDeniedByPermission,
				Notice:  notice.Suppressed("another channelsd replica owns this parked session's start-approval request"),
			}, nil
		}
		return p.finalizeStartApproval(ctx, sess, ev, startAdminSubjects, startRequestRef)
	}

	// Record the signed delivery that opened this session, when there was
	// one. Gated on `created`, not merely "this path was reached": the
	// AlreadyExists race above means another replica already owns this
	// session's opening record, and recording a second, different-looking
	// delivery against the same id would trip triggerdelivery.Record's
	// conflict check for no reason. RawDelivery is empty for every inbound
	// that did not arrive as a verified webhook (a person typing has none),
	// so this never fires for an ordinary conversational session.
	//
	// Best-effort and logged: a session must not fail because its evidence
	// trail could not be written.
	if created && len(ev.RawDelivery) > 0 {
		if err := p.Memory.RecordTriggerDelivery(ctx, sess.Namespace, sess.Name,
			ev.Channel.Spec.Kind, ev.DeliveryEvent, ev.ChannelKey, ev.RawDelivery); err != nil {
			log.FromContext(ctx).Info("RecordTriggerDelivery failed; this session cannot be captured as a trigger bundle",
				"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		}

		// Envelope facts, from the same verified delivery the record above
		// captured.
		//
		// AFTER the delivery record, deliberately: that record is the evidence
		// of what arrived and must exist even if fact derivation fails.
		// Ordering these the other way would let a derivation bug lose the
		// audit trail of the very payload that caused it.
		//
		// Gated on `created` for the same reason the record above is: on the
		// AlreadyExists race another replica owns this session's opening, and
		// its facts are already written — re-deriving would at best be a
		// byte-identical no-op and at worst an append-only conflict logged for
		// nothing.
		//
		// The gate does more than settle that race, and the second consequence
		// is the one worth naming: only a session's FIRST delivery ever yields
		// envelope facts. A later delivery routed into an already-open session
		// contributes none, so a fact about a second pull request cannot appear
		// in a session opened for the first. That is the design's binding
		// invariant holding at the session boundary, not an oversight — do not
		// "fix" it by moving the derivation outside this branch.
		//
		// TriggerFactProvider is implemented by the kind's WebhookReceiver
		// (github's receiver{}, obtained via Kind.WebhookReceiver), not by the
		// registered Kind value itself — the same shape registry.NeedsWebhook
		// and the channel controller's webhookrbac use to ask a kind "do you
		// have a receiver" without a live Deps context: an empty
		// channelkinds.Deps{} is fine here because the answer (nil, or a
		// stateless receiver) never reads it.
		if k, ok := chregistry.Get(ev.Channel.Spec.Kind); ok {
			if fp, ok := k.WebhookReceiver(channelkinds.Deps{}).(channelkinds.TriggerFactProvider); ok {
				facts, ferr := fp.TriggerFacts(ev.Channel, ev.DeliveryEvent, ev.RawDelivery)
				if ferr != nil {
					// Fail-closed for the gate, not fatal for the delivery: with
					// no facts, every precondition over them reads undetermined
					// and denies with an actionable hint. A session that never
					// started could tell nobody anything.
					log.FromContext(ctx).Info("trigger fact derivation failed; session starts with no envelope facts",
						"session", sess.Namespace+"/"+sess.Name, "kind", ev.Channel.Spec.Kind, "err", ferr.Error())
				} else if len(facts) > 0 {
					if err := p.Memory.RecordEnvelopeFacts(ctx, sess.Namespace, sess.Name,
						ev.Channel.Spec.Kind, ev.DeliveryEvent, facts); err != nil {
						log.FromContext(ctx).Info("RecordEnvelopeFacts failed; preconditions over envelope facts will read undetermined",
							"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
					}
				}
			}
		}

		// Trigger slot binding: the instances this verified delivery names,
		// bound into the class's trigger-eligible slots.
		//
		// AFTER the delivery record and the envelope-facts derivation above,
		// for the same evidence-first reason those two are ordered: what
		// arrived, and what it verifiably asserts, must be on record even if
		// this binding step never runs.
		//
		// No interact dependency: a slot grant resolves through
		// slot_grant->interact at CHECK time, against whatever the graph
		// holds when a tool call actually asks — so writing the grant here
		// never depends on an interact or participant tuple already
		// existing. (For what it's worth, this line is also unreachable
		// before the `if startParked` block above returns: every branch of
		// that block returns, so a parked create never falls through to
		// here — but that is not what makes the write safe.)
		//
		// This is also NOT the claim restart.go's slot-grant ordering note
		// makes; read that note before assuming it transfers.
		// ReconcileRestart's Step 6c (carrying a PARENT's existing slot
		// grants onto a forked child) runs after its Step 6/6b (copying the
		// parent's denied blocklist, then writing the broad participant
		// grant) specifically so a copied slot grant cannot widen THAT race:
		// a should-be-denied parent member briefly resolving interact=true
		// before their denial is copied over. A first delivery has no
		// parent denial state to leak — there is nothing that race can
		// widen here.
		if reqs := TriggerSlotRequestsFor(ctx, &class); len(reqs) > 0 {
			p.BindTriggerSlots(ctx, sess, ev, reqs, authz.SlotGrantExpiry(p.Now(), slotSeedExpiration))
		}
	}

	// Thread adoption backfill: only on the actual-create path, and only with
	// history to backfill. adoptionPrompt already put the transcript in
	// spec.Prompt; this grants interact to thread participants and stamps
	// RoutingMode plus the cursors.
	var adopted bool
	var grantedParticipants, withheldParticipants []string
	if created && adopting && len(adoptionPage.Messages) > 0 {
		grantedParticipants, withheldParticipants = p.applyAdoptionGrants(ctx, sess, ev, adoptionPage, appliedInteractPolicy)
		// Seed slot grants from the thread AFTER interact is settled, matching
		// the ordering restart.go already documents for grant copying: a slot
		// grant resolves through slot_grant->interact, so it must never go live
		// ahead of the membership it resolves against.
		p.seedThreadSlots(ctx, sess, ev, adoptionPage, canonical, slotSeedRequests,
			authz.SlotGrantExpiry(p.Now(), slotSeedExpiration))
		adopted = true
	}

	return channelkinds.InboundDecision{
		Outcome:              channelkinds.OutcomeRouted,
		Session:              channelkinds.SessionInfo{Namespace: sess.Namespace, Name: sess.Name, Channel: sess.Spec.InputChannel},
		NewSession:           created, // true only on the actual-create path; AlreadyExists race leaves it false
		RequesterCanonicalID: canonical.String(),
		Adopted:              adopted,
		GrantedParticipants:  grantedParticipants,
		WithheldParticipants: withheldParticipants,
		Ownership:            ownership,
		// Reported only when this call actually CREATED the object carrying
		// them. On the AlreadyExists race another inbound created the session,
		// possibly through a path that set nothing, so the kind's own stamp is
		// still needed — claiming otherwise would suppress the only writer.
		PreTurnAnnotationsStamped: created,
	}, nil
}

// liveInteractiveToolCall returns the name of a Running interactive ToolCall
// for the session, or "" if there is none. Lists by the agentsession label
// the sandbox tool stamps when creating ToolCalls (see ExecuteWithIDs).
func (p *Pipeline) liveInteractiveToolCall(ctx context.Context, ns, sessName string) (string, error) {
	var list spiceboxv1alpha1.ToolCallList
	if err := p.K8s.List(ctx, &list,
		client.InNamespace(ns),
		client.MatchingLabels{"agentsession": sessName},
	); err != nil {
		return "", err
	}
	for i := range list.Items {
		tc := &list.Items[i]
		if tc.Spec.Mode != spiceboxv1alpha1.ToolCallModeInteractive {
			continue
		}
		running, terminal := false, false
		for _, cond := range tc.Status.Conditions {
			switch cond.Type {
			case spiceboxv1alpha1.ToolCallConditionRunning:
				if cond.Status == metav1.ConditionTrue {
					running = true
				}
			case spiceboxv1alpha1.ToolCallConditionSucceeded,
				spiceboxv1alpha1.ToolCallConditionFailed,
				spiceboxv1alpha1.ToolCallConditionTimeout,
				spiceboxv1alpha1.ToolCallConditionCanceled:
				if cond.Status == metav1.ConditionTrue {
					terminal = true
				}
			}
		}
		if running && !terminal {
			return tc.Name, nil
		}
	}
	return "", nil
}

func (p *Pipeline) publishWakeup(ns, name string) error {
	prefix := channelevents.SubjectPrefix(ns, name)
	env := channelevents.Envelope{
		Version:     1,
		Kind:        channelevents.KindUserMessage,
		Session:     channelevents.SessionRef{Namespace: ns, Name: name},
		PublishedAt: p.Now(),
		Payload:     []byte("{}"),
	}
	if err := env.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return p.NATS.Publish(channelevents.SubjectIn(prefix, channelevents.KindUserMessage), b)
}

// resolveApproverLinkedServices returns the provider labels the approver has
// linked AND the joining session's AgentClass actually uses, so a
// userPassthrough permission_request can warn with a concrete list instead of
// the generic "your connected accounts".
//
// The intersection is the security signal: showing an account the session
// would never touch dilutes the warning.
//
// Best-effort throughout — a missing annotation, an absent UserIdentity or a
// failed MCPServer list all return nil and degrade to the generic wording.
// None of them block the approval flow.
func (p *Pipeline) resolveApproverLinkedServices(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	ac *spiceboxv1alpha1.AgentClass,
) []string {
	logger := log.FromContext(ctx)
	subject := spiceboxv1alpha1.StartedBySubject(sess)
	if subject == "" {
		logger.V(1).Info("permission_request: no started-by canonical annotation; no linked-services list",
			"session", sess.Name)
		return nil
	}
	var ui spiceboxv1alpha1.UserIdentity
	if err := p.K8s.Get(ctx, client.ObjectKey{Name: useridentity.NameForSubject(subject)}, &ui); err != nil {
		logger.V(1).Info("permission_request: UserIdentity lookup miss; no linked-services list",
			"session", sess.Name, "subject", subject, "err", err.Error())
		return nil
	}
	credNames := make([]string, 0, len(ui.Spec.Credentials))
	for _, c := range ui.Spec.Credentials {
		credNames = append(credNames, c.Name)
	}
	classCreds := classCredentialSet(ac)
	labels, err := passthroughcatalog.ResolveLinkedServiceLabels(ctx, p.K8s, credNames, classCreds)
	if err != nil {
		logger.Info("permission_request: linked-services lookup failed; falling back to generic warning",
			"session", sess.Name, "err", err.Error())
		return nil
	}
	return labels
}

// classCredentialSet collects the credential names the AgentClass's MCPServers
// reference, honoring credentialRemap overrides. It is the lens
// ResolveLinkedServiceLabels filters through, so only credentials the joining
// session actually uses reach the warning. nil means "no class filter".
func classCredentialSet(ac *spiceboxv1alpha1.AgentClass) map[string]struct{} {
	if ac == nil || len(ac.Spec.MCPServers) == 0 {
		return nil
	}
	out := map[string]struct{}{}
	for _, ref := range ac.Spec.MCPServers {
		// Keys are the tool's declared names; values are what UserIdentity
		// catalogs, so the values are the credentials actually pulled.
		for _, remapped := range ref.CredentialRemap {
			if remapped != "" {
				out[remapped] = struct{}{}
			}
		}
	}
	// An AgentClass may name MCPServers with no credentialRemap at all, taking
	// its credentials from MCPServer.Spec.Auth.Credential instead.
	// ResolveLinkedServiceLabels covers that via its own MCPServer list, so an
	// empty set here means "no remap-side filter" and is signalled as nil.
	if len(out) == 0 {
		return nil
	}
	return out
}

// canonicalID canonicalizes an inbound channel sender. Senders may be guests or
// foreign-workspace users with no verified email, and keying them by the
// unforgeable synthetic subject is intended, so this opts in via AllowSynthetic.
// That makes the canonical total (email → email encoding, none → synthetic), so
// ErrSyntheticSubject can never fire and a non-nil error is a programming bug —
// hence the panic rather than minting a phantom subject.
//
// The opt-in is DELIBERATE. Refusing email-less senders was considered and
// rejected:
//
//   - This MINTS an identity for one inbound turn; it is not a standing check.
//     All three synthetic inputs (kind, teamScope, externalID) are asserted by
//     the platform on the envelope, never by the person speaking, so the
//     subject is not forgeable.
//   - Email-less senders are the norm. Slack returns a profile email only to an
//     app holding users:read.email and never for guests or Slack-Connect users;
//     the local, browser and fake kinds carry no email at all. Failing closed
//     would silently refuse every turn on those surfaces.
//   - The harm fail-closed guards against — a PHANTOM subject no grant resolves
//     to — is prevented instead by minting the same team-scoped canonical at
//     every site touching this principal, so two workspaces reusing one
//     external id stay distinct subjects.
//   - Where identity is MATCHED rather than minted, fail-closed IS in place:
//     the decision pipe's DecideRequester leg does not opt in, so an
//     email-less principal never matches a recorded addressee.
func canonicalID(id channelkinds.ExternalIdentity) identity.CanonicalUserID {
	c, err := identity.FromExternal(
		identity.Kind(id.Kind),
		identity.TeamScope(id.TeamScope),
		identity.RawExternalID(id.ExternalID),
		identity.Email(id.Email),
	).AllowSynthetic().Canonical()
	if err != nil {
		panic("canonicalID: unreachable with AllowSynthetic: " + err.Error())
	}
	return c
}

// annotateWake stamps the wake-requested-at annotation on a respawn-eligible
// session so the operator respawns its exited runner. It re-reads the latest
// session on every attempt and retries on optimistic-lock conflict.
//
// The conflict retry is load-bearing, not defensive. The session was read at
// the top of Deliver while the operator writes its status concurrently, so a
// stale-base optimistic-lock patch loses the race with Conflict. Failing there
// silently drops the wake: the inbound is already in memory but no runner is
// respawned, so the user's reply strands with no signal.
//
// The fresh read is also the sole eligibility decision, since Deliver calls
// this unconditionally. It keeps a live runner from being respawned behind its
// own back and — the direction that actually stranded users — catches a runner
// that went Idle after Deliver's opening List. Skipping is the common case
// (every mid-turn message), so it logs at V(1); the sweep variants it also
// covers (Idle→Succeeded, AwaitingRetry→Failed) are not dropped wakes either,
// because the next inbound takes the terminal-continuation path.
func (p *Pipeline) annotateWake(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	key := types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur spiceboxv1alpha1.AgentSession
		if err := p.K8s.Get(ctx, key, &cur); err != nil {
			return err
		}
		if !spiceboxv1alpha1.WakeEligible(&cur) {
			log.FromContext(ctx).V(1).Info("no respawn annotation: session is not parked (live or starting runner, or credential-gated); inbound delivered via the NATS wakeup",
				"session", key.String(), "phase", cur.Status.Phase)
			return nil
		}
		base := cur.DeepCopy()
		if cur.Annotations == nil {
			cur.Annotations = map[string]string{}
		}
		cur.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = p.Now().Format(time.RFC3339Nano)
		return p.K8s.Patch(ctx, &cur, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

// writeInheritForkTrigger records a continuation-inherit fork-trigger on a
// terminal parent session: the operator's fork reconciler (owner-gated,
// denied-copying) then materializes a fresh session that inherits the whole
// transcript and appends newText as its first message. channelsd never does
// the SpiceDB fork-writes or the transcript copy — it only writes the marker.
//
// First-writer-wins: if the parent already carries a PendingRestart (a prior
// inbound already triggered, or the operator hasn't consumed it yet), the
// existing trigger is left intact so the first continuation isn't clobbered.
//
// Reports whether THIS inbound wrote the trigger. A false return is not an
// error, but the caller must not then ack a fresh session it did not start:
// acking regardless is how a parent with an unconsumed marker answered every
// reply with "I'm picking it up in a new session" and the session never arrived.
func (p *Pipeline) writeInheritForkTrigger(ctx context.Context, parent *spiceboxv1alpha1.AgentSession, newText string, canonical identity.CanonicalUserID) (bool, error) {
	if existing := parent.Status.PendingRestart; existing != nil {
		// Log the inbound alongside the pending text: identical means a duplicate
		// delivery (two channelsd replicas), different means this message is not
		// covered by the in-flight fork and will not be delivered to it.
		log.FromContext(ctx).Info("inherit fork-trigger already pending; not overwriting",
			"session", parent.Namespace+"/"+parent.Name,
			"target", existing.TargetSessionName,
			"duplicateOfPending", existing.NewUserText == newText)
		return false, nil
	}
	target := deterministicInheritName(parent.Name, newText)
	base := parent.DeepCopy()
	patched := parent.DeepCopy()
	patched.Status.PendingRestart = &spiceboxv1alpha1.PendingRestart{
		Mode:        spiceboxv1alpha1.PendingRestartModeInherit,
		NewUserText: newText,
		// Subject form ("user:<canonical>"); the fork gate strips the prefix
		// before its agentsession#fork check, matching the restart path.
		TriggeredBy:       canonical.Subject(),
		RequestedAt:       metav1.NewTime(p.Now()),
		TargetSessionName: target,
		// CutTurnIndex is unused in inherit mode — the reconciler recomputes
		// the cut as the parent's last turn so the whole transcript carries.
	}
	if err := p.signMarker(patched, patched.Status.PendingRestart); err != nil {
		return false, fmt.Errorf("sign inherit fork-trigger: %w", err)
	}
	if err := applyApprovalStatus(ctx, p.K8s, patched, base); err != nil {
		return false, fmt.Errorf("apply inherit fork-trigger status: %w", err)
	}
	return true, nil
}

// writeTakeoverForkTrigger stamps a takeover PendingRestart on the terminal
// parent for a DIFFERENT-user continuation. Mirrors writeInheritForkTrigger's
// idempotent first-writer-wins semantics but records the NEW owner (external id
// + canonical subject) and the inherit-vs-fresh choice. inheritHistory=false is
// the policy-halt carve-out: the halted transcript is not seeded into the new
// owner's session.
func (p *Pipeline) writeTakeoverForkTrigger(ctx context.Context, parent *spiceboxv1alpha1.AgentSession, ev channelkinds.InboundEvent, canonical identity.CanonicalUserID, inheritHistory bool) (bool, error) {
	if existing := parent.Status.PendingRestart; existing != nil {
		log.FromContext(ctx).Info("takeover fork-trigger already pending; not overwriting",
			"session", parent.Namespace+"/"+parent.Name, "target", existing.TargetSessionName)
		return false, nil
	}
	target := deterministicTakeoverName(parent.Name, canonical.String(), ev.MessageText)
	base := parent.DeepCopy()
	patched := parent.DeepCopy()
	patched.Status.PendingRestart = &spiceboxv1alpha1.PendingRestart{
		Mode:        spiceboxv1alpha1.PendingRestartModeTakeover,
		NewUserText: ev.MessageText,
		// Subject form ("user:<canonical>"); the reconciler strips the prefix
		// and stamps the child's owner from it (WriteSpiceDBParticipants).
		TriggeredBy:        canonical.Subject(),
		NewOwnerExternalID: ev.ExternalIDs.ExternalID.String(),
		InheritHistory:     inheritHistory,
		RequestedAt:        metav1.NewTime(p.Now()),
		TargetSessionName:  target,
	}
	if err := p.signMarker(patched, patched.Status.PendingRestart); err != nil {
		return false, fmt.Errorf("sign takeover fork-trigger: %w", err)
	}
	if err := applyApprovalStatus(ctx, p.K8s, patched, base); err != nil {
		return false, fmt.Errorf("apply takeover fork-trigger status: %w", err)
	}
	return true, nil
}

// signMarker stamps the pipeline's attestation onto a freshly-built
// PendingRestart. It is called after every field is final, because the digest
// covers them all.
//
// A nil MarkerSigner is an error, not a silent skip: an unsigned marker is
// refused by the operator, so writing one would trade a legible failure here
// for an unexplained refusal in the user's thread.
func (p *Pipeline) signMarker(parent *spiceboxv1alpha1.AgentSession, pr *spiceboxv1alpha1.PendingRestart) error {
	if p.MarkerSigner == nil {
		return fmt.Errorf("no restart-marker signer configured; refusing to write a marker the operator will reject")
	}
	return p.MarkerSigner.Sign(parent, pr)
}

// participantsAudience is the audience every pipeline notice below uses: the
// message lands on the thread it concerns. None of these is sensitive — they
// describe the state of a conversation everyone in it can already see.
func participantsAudience() channelevents.InteractionAudience {
	return channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants}
}

// portalFailureNotice is posted when a portal-access trigger matched but the
// link could not be minted or delivered. The user typed a trigger phrase and
// is waiting; silence would leave them on a placeholder until the silence
// watchdog wrongly reports the agent as stalled.
//
// droppedAttachments appends a sentence naming any attachments the same message
// carried. InboundDecision holds exactly one Notice and a portal-access trigger
// has nowhere to put ev.Attachments, so both problems must fit in this one
// message. Zero is the common case and adds nothing.
func portalFailureNotice(droppedAttachments int) *notice.Notice {
	body := "The link couldn't be generated just now."
	if droppedAttachments > 0 {
		body += " Also, " + pluralAttachedFiles(droppedAttachments) + " on this message won't carry through — please re-send after this is resolved."
	}
	return notice.New(categories.PortalLinkFailed, notice.Args{
		Lead:     "Couldn't open your account settings",
		Body:     body,
		NextStep: "Try again in a moment.",
		Audience: participantsAudience(),
	})
}

// refusalNotice is posted when a new inbound arrives for a session that failed
// unrecoverably. It names the failure reason and directs the user to a new
// thread — replying in place can only re-refuse, since the failure is not one
// a retry can fix. That permanence is why it is danger rather than warning.
//
// The reason is an externally-derived string (a controller's condition
// message), so it travels as an Excerpt — rendered inert by every surface —
// rather than being interpolated into the trusted lead.
func refusalNotice(reason string) *notice.Notice {
	args := notice.Args{
		Lead:     "This session has ended and can't continue",
		NextStep: "Start a new thread to begin again.",
		Audience: participantsAudience(),
	}
	if reason != "" {
		args.Excerpt = &channelevents.InteractionExcerpt{Label: "Reason", Content: reason}
	}
	return notice.New(categories.SessionEnded, args)
}

// agentUnavailableBodies translates the Valid=False reason of the AgentClass
// that cannot start into the sentence the person waiting in the thread reads.
// Each entry says what is wrong in terms of the product, never of the object
// that recorded it.
//
// An absent reason is not a defect: it falls back to
// agentUnavailableDefaultBody, which is true of every way a class can go
// invalid. Add a row only when naming the problem's shape gives the reader
// something to act on; re-wording "it's broken" earns nothing.
var agentUnavailableBodies = map[string]string{
	// The common cause, and the one worth naming: the reader may well be the
	// person who can re-authorize it, or knows who is.
	spiceboxv1alpha1.ReasonAgentIdentityInvalid: "One of the services it signs in to needs to be re-authorized.",
	spiceboxv1alpha1.ReasonAgentIdentityMissing: "It hasn't been connected to the services it needs yet.",
	spiceboxv1alpha1.ReasonSecretMissing:        "One of the credentials it needs is missing.",
	spiceboxv1alpha1.ReasonSecretKeyMissing:     "One of the credentials it needs is incomplete.",
	spiceboxv1alpha1.ReasonCredentialEmpty:      "One of the credentials it needs is empty.",
}

// agentUnavailableDefaultBody covers every reason not in the table above.
const agentUnavailableDefaultBody = "Part of its setup isn't working."

// agentUnavailableMissingBody is the one case where the reassurance below is a
// lie: nothing watches an AgentClass that does not exist, so a session parked
// behind a missing one never resumes on its own. Say so instead.
const agentUnavailableMissingBody = "This conversation isn't connected to an agent that exists."

// agentUnavailableBody returns the user-facing sentence for an AgentClass
// Valid=False reason, falling back to the generic one for any reason with no
// row of its own.
func agentUnavailableBody(classReason string) string {
	if body, ok := agentUnavailableBodies[classReason]; ok {
		return body
	}
	return agentUnavailableDefaultBody
}

// agentUnavailableNotice explains a conversation that has stalled because its
// agent cannot start. The message is NOT lost — the session is parked and
// resumes when the agent is healthy — so the copy's job is to stop the user
// waiting on a thread that looks ignored, and to stop them re-sending.
//
// classReason is the class's condition reason, used ONLY to select the copy.
// It is deliberately never rendered: a reason like AgentIdentityInvalid is
// operator vocabulary about internal objects, and the person waiting in the
// thread can neither read it nor act on it. The verbatim detail travels to the
// monitoring channel instead, where the people who can fix it are — see
// publishUnhealthyAgentMonitoring.
func agentUnavailableNotice(classReason string) *notice.Notice {
	// A missing AgentClass has no controller to watch it back into health, so
	// it is the one reason that must NOT promise an eventual answer.
	if classReason == spiceboxv1alpha1.ReasonAgentClassMissing {
		return notice.New(categories.AgentUnavailable, notice.Args{
			Lead:     "There's no agent here to answer you",
			Body:     agentUnavailableMissingBody,
			NextStep: "Your administrators have been alerted — this one won't resolve on its own, so please follow up with them.",
			Audience: participantsAudience(),
		})
	}
	return notice.New(categories.AgentUnavailable, notice.Args{
		Lead:     "This agent can't pick this up right now",
		Body:     agentUnavailableBody(classReason),
		NextStep: "No need to re-send — your administrators have been alerted, and I'll answer this as soon as it's working again.",
		Audience: participantsAudience(),
	})
}

// inheritNotice is posted when a continuation forks a fresh inheriting
// session. The fork stays in this thread, so the notice's job is to explain
// WHY a new session appeared rather than to point elsewhere: the previous one
// had finished, and a finished session has no runner to resume.
//
// No NextStep, and none is required at info severity, because there is nothing
// for the user to do — UNLESS droppedAttachments > 0. The fork trigger stashes
// only ev.MessageText, so attachments on the message that armed the fork do
// need an actionable NextStep: re-send them.
func inheritNotice(droppedAttachments int) *notice.Notice {
	args := notice.Args{
		Lead:     "Continuing in a new session",
		Body:     "That conversation had already finished, so I'm picking it up in a new session — same thread, full history.",
		Audience: participantsAudience(),
	}
	if droppedAttachments > 0 {
		args.Body += " " + pluralAttachedFiles(droppedAttachments) + " on that message did not carry over."
		args.NextStep = "Please re-send the file(s) now that the new session is picking things up."
	}
	return notice.New(categories.SessionContinuedInherited, args)
}

// takeoverNotice is posted when a DIFFERENT user takes over a terminal thread.
//
// The two variants are two CATEGORIES, not one function branching on a bool:
// inheriting prior history is informational, losing it is a warning, because a
// reader who assumes the context carried over will be wrong. One string with
// one tone makes the halted case sound as routine as the inherited one.
//
// The @-mention comes from the channel kind's RenderMention seam, keeping this
// channel-agnostic.
//
// droppedAttachments is needed for the same reason as inheritNotice's: the
// trigger stashes only ev.MessageText into PendingRestart.
func takeoverNotice(kind, newOwnerExternalID string, inheritHistory bool, droppedAttachments int) *notice.Notice {
	who := newOwnerExternalID
	if k, ok := chregistry.Get(kind); ok {
		if mention := k.RenderMention(newOwnerExternalID); mention != "" {
			who = mention
		}
	}
	if inheritHistory {
		args := notice.Args{
			Lead:     "Continuing in a new session",
			Body:     "The earlier session in this thread had ended — " + who + " is continuing in a new session, same thread, with the prior history carried over.",
			Audience: participantsAudience(),
		}
		if droppedAttachments > 0 {
			args.Body += " " + pluralAttachedFiles(droppedAttachments) + " on that message did not carry over."
			args.NextStep = "Please re-send the file(s) now that the new session is picking things up."
		}
		return notice.New(categories.ThreadTakeoverInherited, args)
	}
	nextStep := "Re-send any context from earlier in the thread that still matters."
	if droppedAttachments > 0 {
		nextStep += " That includes " + pluralAttachedFiles(droppedAttachments) + " on the message that started this — they did not carry over either."
	}
	return notice.New(categories.ThreadTakeoverHalted, notice.Args{
		Lead:     "Starting fresh — earlier history not carried over",
		Body:     "The previous session in this thread was halted and can't be continued. Starting a fresh session for " + who + ".",
		NextStep: nextStep,
		Audience: participantsAudience(),
	})
}

// startedBySubjectAnnotation returns the value for the started-by canonical
// annotation. A non-human subject — a cron service identity, or a session on
// an agent channel — has no human starter, so the annotation is omitted
// entirely: the same shape kubectl-driven sessions have, which
// StartedByCanonical/StartedBySubject already document as "" when absent.
// Stamping "user:service:<id>" instead would assert a human who does not exist.
func startedBySubjectAnnotation(canonical identity.CanonicalUserID, nonHumanSubject bool) string {
	if nonHumanSubject {
		return ""
	}
	return "user:" + canonical.String()
}

// touchStartedByUnlessNonHumanOrParked writes agentsession#started_by for a human
// starter and is a no-op for a non-human acting subject (service: or
// agentsession:). See the call site for why those cannot be written under the
// current schema: started_by is user-typed.
//
// The write is retried, bounded by AuthzWriteRetry, because failing it is
// TERMINAL: the caller stamps status.startFailure, the operator drives the
// session to Failed, and every later reconcile short-circuits on the terminal
// phase — so nothing re-attempts the write, and the user's first message, still
// durable in spec.prompt.inline of that dead session, is never processed. One
// rolling restart of SpiceDB was enough to lose a conversation.
//
// Retrying is safe here because this is a WriteRelationships, not a check:
// SpiceDB reports "denied" as a permissionship on a Check response, never as a
// write error, so there is no denial for the loop to grind against — and TOUCH
// is idempotent, so retrying a write that landed is a no-op.
//
// It does NOT fix a SpiceDB whose composed schema has no `agentsession`
// definition yet (FailedPrecondition). That cold-start ordering problem can
// outlast any request-scoped ceiling, so it still lands on startFailure with
// the gRPC text naming the cause; the fix belongs at channelsd's startup
// dependency gate, which accepts ANY non-empty schema.
func (p *Pipeline) touchStartedByUnlessNonHumanOrParked(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	canonical identity.CanonicalUserID,
	nonHumanSubject bool,
	startParked bool,
) error {
	logger := log.FromContext(ctx)
	if nonHumanSubject {
		logger.V(1).Info("skipping started_by write for non-human-subject session",
			"session", sess.Namespace+"/"+sess.Name, "subject", canonical.String())
		return nil
	}
	// started_by confers interact; a session parked for start approval must
	// hold NO standing for its guest until an admin approves.
	// decideStartApproval writes the tuple on the Approve click.
	if startParked {
		logger.V(1).Info("skipping started_by write for a session parked awaiting start approval",
			"session", sess.Namespace+"/"+sess.Name, "subject", canonical.String())
		return nil
	}
	ref := authz.SessionRef{Namespace: sess.Namespace, Name: sess.Name}
	return startup.Retry(ctx, "started_by authz write", p.AuthzWriteRetry,
		func(ctx context.Context) error { return p.Engine.TouchStartedBy(ctx, ref, canonical) },
		func(attempt int, err error, next time.Duration) {
			logger.Info("started_by authz write failed; retrying",
				"session", sess.Namespace+"/"+sess.Name, "attempt", attempt,
				"retryIn", next.String(), "err", err.Error())
		})
}

// publishUnapprovableJoinMonitoring notifies the monitoring channel that a
// human tried to join a session nobody can approve them into. The monitoring
// channel is the platform-admin surface, and it is the only place this can be
// actioned: the session has no owner, so there is no chat-side approver to DM.
//
// Best-effort — the notice to the requester and the log line are the
// load-bearing surfaces; this is the admin-facing fallback on top.
func (p *Pipeline) publishUnapprovableJoinMonitoring(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	ev channelkinds.InboundEvent,
) {
	if p.NATS == nil {
		return
	}
	mev := channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelWarning,
		Category:   "session",
		Transition: channelevents.MonitoringTransitionFailed,
		Source: channelevents.MonitoringSourceRef{
			Kind:      "AgentSession",
			Namespace: sess.Namespace,
			Name:      sess.Name,
		},
		Condition: spiceboxv1alpha1.AgentSessionConditionPermissionRequestPending,
		Reason:    "NoAddressableApprover",
		// Identify the requester by canonical SpiceDB subject, not the raw
		// channel-native id: an admin acting on this needs a value they can
		// paste into spec.owner.explicit or a SpiceDB write, and a raw Slack
		// user id is neither of those nor meaningful outside Slack.
		Summary: fmt.Sprintf("user:%s tried to join session %s/%s, which has no owner who can approve them.",
			canonicalID(ev.ExternalIDs).String(), sess.Namespace, sess.Name),
		// Name the Channel that started the session: this applies to ANY input
		// kind providing no starting user, and naming the wrong one sends the
		// admin to the wrong CR. The session's INPUT channel, not ev.Channel —
		// that is wherever the requester happened to reply.
		Hint:      unapprovableJoinHint(sess),
		Timestamp: p.Now(),
	}
	if err := channelevents.PublishMonitoring(p.NATS.Publish, mev); err != nil {
		log.FromContext(ctx).Info("permission_request: publish unapprovable-join monitoring event failed",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
	}
}

// unapprovableJoinHint names the Channel that started the session so an admin
// is sent to the right CR. Uses the session's INPUT channel — the one that
// spawned it — not whichever Channel the requester happened to reply in.
//
// Deliberately not hardcoded to "cron/bento": any input kind that provides no
// starting user reaches this state, and naming the wrong kind sends the reader
// somewhere unhelpful. Degrades to a kind-agnostic sentence when the session
// carries no input binding (a kubectl-driven session).
func unapprovableJoinHint(sess *spiceboxv1alpha1.AgentSession) string {
	const remedy = "set spec.owner.explicit on that Channel to a subject with standing, " +
		"or ensure the membership source backing owner.ownerless.fromOutputChannel is syncing"
	in := sess.Spec.InputChannel
	if in == nil || in.Name == "" {
		return "the session has no starting user and no input Channel to attribute it to; " + remedy
	}
	return fmt.Sprintf("session started by %s Channel %q, whose kind provides no starting user; %s",
		in.Kind, in.Name, remedy)
}

// approverIdentityFor derives the identity a session's join-approval request
// should be addressed to: the session's started-by user.
//
// Kind mirrors the requester's — both are on the same session channel, and
// permission_request has no cross-kind approver today.
//
// When no verified email is on record, Principal()'s synthetic canonical
// (base64(kind:teamScope:externalID)) cannot be safely re-derived here: there
// is no StartedByTeamScope annotation preserving the TeamScope the ORIGINAL
// canonical was minted with, so a re-derivation with an empty TeamScope could
// diverge from the bytes StartedBySubject stamped at session-creation time.
// Fall back to the exact precomputed canonical via the Subject passthrough
// (Principal() prefers Subject over the raw fields) so delivery stays
// byte-identical off the email-present happy path.
func approverIdentityFor(sess *spiceboxv1alpha1.AgentSession, kind identity.Kind) channelevents.ExternalIdentity {
	approver := channelevents.ExternalIdentity{
		Kind:       kind,
		ExternalID: spiceboxv1alpha1.StartedByExternalID(sess),
		Email:      spiceboxv1alpha1.StartedByEmail(sess),
	}
	if approver.Email == "" {
		approver.Subject = spiceboxv1alpha1.StartedBySubject(sess)
	}
	return approver
}

// approverAddressable reports whether an approver identity can actually be
// delivered to: either the raw form (kind + externalID) or a Subject
// passthrough. Mirrors the addressability rule
// InteractionRequestPayload.Validate applies to an approvers audience.
//
// A session with no started-by (any cron/bento-spawned session) yields an
// identity that satisfies neither, which is why this must be checked before
// any durable record is written.
func approverAddressable(a channelevents.ExternalIdentity) bool {
	return (a.Kind != "" && a.ExternalID != "") || a.Subject != ""
}

// bindingCaps normalizes a kind's advertised capabilities for storage on a
// ChannelBinding. `capabilities` is a REQUIRED field in the AgentSession CRD,
// so a nil slice marshals to `null` and the apiserver rejects the entire
// AgentSession with `spec.inputChannel.capabilities: Required value`.
//
// An input-only kind legitimately advertises none — bento has no rendering
// surface and returns nil. That must serialize as [], not null: otherwise no
// session for that kind can ever be created, and because Bento's output retries
// on error the rejection becomes a hot loop against the apiserver.
func bindingCaps(caps []string) []string {
	if caps == nil {
		return []string{}
	}
	return caps
}

// authorSubject returns the human author subject ("user:<canonical>") for an
// inbound event, or "" for a non-human inbound (bento/service — no per-user
// external ID). It is the single source of the per-turn author stamped on the
// human "inbox" turns channelsd writes.
func authorSubject(ev channelkinds.InboundEvent) identity.Subject {
	if ev.ExternalIDs.ExternalID == "" {
		return ""
	}
	return canonicalID(ev.ExternalIDs).Subject()
}

// makeSessionName produces a deterministic-ish name based on the channel
// and key. Truncated + suffixed with a hash so k8s names stay valid.
func makeSessionName(ch *spiceboxv1alpha1.Channel, key string) string {
	h := sha256.Sum256([]byte(ch.Name + "/" + key))
	suffix := hex.EncodeToString(h[:])[:8]
	base := strings.ReplaceAll(strings.ToLower(ch.Name), "_", "-")
	if len(base) > 40 {
		base = base[:40]
	}
	return base + "-" + suffix
}

// renderMention returns the channel kind's clickable mention syntax for
// externalID (Slack: "<@U123>"), falling back to the bare externalID when
// the kind isn't registered or its RenderMention returns empty. Shared by
// every channel-agnostic pipeline string that needs to address a specific
// user by channel identity, so composing a mention never hardcodes Slack
// syntax outside the slack package.
func renderMention(kind, externalID string) string {
	if k, ok := chregistry.Get(kind); ok {
		if mention := k.RenderMention(externalID); mention != "" {
			return mention
		}
	}
	return externalID
}

// archivedSessionNotYoursNotice builds the notice for someone trying to revive
// an archived session that another user started.
//
// There is no approval flow on this path — an archived session is not reopened
// by consent, and the second user must start their own thread — so the copy
// must never imply that permission has been requested on their behalf. It says
// what is true: this thread is someone else's, and a new one is the way
// forward.
//
// The mention syntax comes from the channel kind via Kind.RenderMention, so
// this stays channel-agnostic.
func archivedSessionNotYoursNotice(kind, requesterExternalID, startedByExternalID string) *notice.Notice {
	args := notice.Args{
		Lead:     "Only the person who started this session can revive it",
		NextStep: "Start a new thread to begin your own session.",
		Audience: participantsAudience(),
	}
	if startedByExternalID != "" {
		args.Body = "This thread belongs to " + renderMention(kind, startedByExternalID) + "'s session."
	}
	return notice.New(categories.ArchivedSessionNotYours, args)
}

// starterDeniedNotice is posted in-thread when the session's own starter is
// denied interact — the self-addressed-join fault (handlePermissionDeny step
// 1b). It must NOT read as a permission decision: nobody denied them anything,
// the session's ownership record is simply missing. Retry is the right advice
// because the common cause is the race between channelsd creating the session
// and the operator's owner resolver writing agentsession#owner; the second
// sentence covers the case where the write never lands, so a user staring at a
// thread that keeps refusing them has a term to hand an operator.
func starterDeniedNotice() *notice.Notice {
	return notice.New(categories.SessionOwnerRecordMissing, notice.Args{
		Lead: "Can't accept that message yet",
		Body: "This session is still finishing its setup, so it doesn't recognise you as its owner yet.",
		// Two steps because there are two causes: the common one is a startup
		// race a retry clears, and the rare one needs an operator. Naming the
		// concrete record gives a stuck user something to hand them.
		NextStep: "Try again in a moment. If it keeps happening, ask an operator to look at this session — it never finished starting.",
		Audience: participantsAudience(),
	})
}

// permissionRequestBody composes the approver-facing prompt body for a
// session-join request. Channel-agnostic, so it carries no thread deep link —
// it has no per-surface ids to build one from.
//
// It names NOBODY and quotes NOTHING. The requester is declared structurally in
// requesterField's Mentions, and their message travels in the payload's
// Excerpt, because this body is untrusted TEXT that every surface renders inert
// while a mention is a structured IDENTITY the surface resolves itself and an
// Excerpt is a region the surface fences. Interpolating a rendered mention into
// these sentences makes the approver read escaped markup, since the Slack card
// escapes Body (escapePublisherPayload). Do not "fix" a dead mention by
// un-escaping the body.
//
// The requester's message is not quoted here for the same reason, and the
// reason is worth stating because the naive fix looks right: a "> " blockquote
// composed in this function reaches the Slack card THROUGH that same escape,
// which maps ">" to "&gt;", so the approver reads the markers as characters.
// Quoting is the surface's job, over a slot it knows is untrusted — see
// InteractionExcerpt's contract.
//
// identityMode == userPassthrough rewrites the lead as an elevated-risk
// warning: approving lets the requester drive actions that execute against the
// approver's connected services with the approver's credentials. A non-empty
// linkedServices names the concrete providers instead of "your connected
// accounts".
func permissionRequestBody(agentClassName, identityMode string, linkedServices []string) string {
	target := "your agent session"
	if agentClassName != "" {
		target = "your *" + agentClassName + "* agent session"
	}
	if identityMode == spiceboxv1alpha1.IdentityModeUserPassthrough {
		// When linkedServices is concrete, swap the vague phrasing for an
		// explicit bullet list. The approver must scan it before clicking
		// Approve.
		stake := "*This session uses YOUR connected accounts.*"
		if len(linkedServices) > 0 {
			var b strings.Builder
			b.WriteString("*This session can act AS YOU on the services you've connected:*\n")
			for _, label := range linkedServices {
				b.WriteString("  • ")
				b.WriteString(label)
				b.WriteString("\n")
			}
			stake = b.String()
		}
		return fmt.Sprintf(
			":warning: *Someone is asking to join %s.*\n"+
				"\n"+
				"%s"+
				"If you approve, they will be able to drive the agent — and "+
				"the agent will call those services *as you*, using the "+
				"accounts you linked.\n"+
				"\n"+
				"Only approve if you trust them with that access. You can "+
				"revoke individual accounts later via your agent.",
			target, stake)
	}
	return fmt.Sprintf("🔔 Someone is asking to talk to %s.", target)
}

// requesterMessageExcerpt carries the requester's own message to the approver.
//
// It is an Excerpt and not a sentence in permissionRequestBody because it is
// the one untrusted string in this payload: the requester wrote it, and
// InteractionExcerpt is the ONE slot the wire contract lets untrusted content
// travel in, on the promise that every surface renders it inert and visibly
// apart from the publisher's copy.
//
// nil when there is nothing to show — an attachment-only join post — so a
// surface renders no empty quoted region rather than a placeholder.
func requesterMessageExcerpt(messageText string) *channelevents.InteractionExcerpt {
	preview := truncateRunes(messageText, 200)
	if preview == "" {
		return nil
	}
	return &channelevents.InteractionExcerpt{Label: "Their message", Content: preview}
}

// requesterField declares WHO is asking to join, as a structured identity the
// surface resolves itself, rather than as mention markup interpolated into
// prose.
//
// Every surface that parses markup escapes this body, and must: the pipeline is
// channel-agnostic and cannot tell a surface which of its strings are safe to
// parse, so interpolated mention markup arrives escaped and dead. A Field
// carrying Mentions is the seam that survives, because it is the one thing the
// renderer knows a KIND composed — resolveFieldMentions swaps in a live mention
// and escapePublisherPayload exempts it.
//
// Value is the display fallback for surfaces resolving no mentions, and for a
// per-recipient miss inside one that does. Never empty — a join request naming
// nobody is unanswerable — so it degrades display name → email → raw id.
func requesterField(id channelkinds.ExternalIdentity) channelevents.InteractionField {
	display := channelDisplay(id)
	if display == "" {
		display = id.ExternalID.String()
	}
	return channelevents.InteractionField{
		Label:    "From",
		Value:    display,
		Mentions: []channelevents.ExternalIdentity{toEnvelopeIdentity(id)},
	}
}

// handlePermissionDeny runs when CheckInteract returns false on an active
// session. It dedups against AgentSession.status.PendingRequesters and, on
// first post, stamps the pending entry plus the PermissionRequestPending
// condition and publishes an interaction_request(permission_request) for the
// kind's generic "interaction" sub-channel. Returns OutcomeDeniedByPermission
// either way; a suppressed notice means "silent drop, do not post in-thread",
// so repeat sends from the same requester don't spam.
//
// It takes no started-by external id. The approver is derived from the
// session's started-by ANNOTATIONS (approverIdentityFor), the only form that
// survives a session with no verified email.
func (p *Pipeline) handlePermissionDeny(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	ev channelkinds.InboundEvent,
) (channelkinds.InboundDecision, error) {
	// Every branch below denies this inbound and none routes ev.Attachments
	// anywhere: the durable PendingRequesters stash carries only MessageText,
	// and the approve-replay rebuilds a synthetic InboundEvent with no
	// Attachments field at all. Logged here unconditionally, whichever branch
	// fires — including the blocklisted and duplicate branches that stay silent
	// toward the user — so the drop is never invisible server-side. The ordinary
	// pending case also tells the user, via the interaction_request below.
	logAttachmentsDropped(ctx, sess.Namespace, sess.Name, channelRefForLog(ev.Channel), "a permission-denied join request", ev.Attachments)

	// 1. SpiceDB blocklist: a requester the original requester already denied is
	//    dropped silently — no DM, no in-thread message. Keyed by canonicalID
	//    so it covers every channel kind; a kind-gate here would silently
	//    bypass blocking for any new transport.
	canonical := canonicalID(ev.ExternalIDs)
	denied, err := p.Authz.CheckDenied(ctx, sess.Namespace, sess.Name, canonical, true)
	if err != nil {
		log.FromContext(ctx).Error(err, "check is_denied",
			"session", sess.Name, "requester", ev.ExternalIDs.ExternalID, "kind", ev.ExternalIDs.Kind)
		// Fall through; better to over-DM than to swallow access errors silently.
	} else if denied {
		return channelkinds.InboundDecision{
			Outcome: channelkinds.OutcomeDeniedByPermission,
			// Explicit suppression, not an empty message: the requester was
			// blocklisted by the session's original requester, so posting
			// anything at all would confirm the session exists.
			Notice: notice.Suppressed("requester is blocklisted by the original requester"),
		}, nil
	}

	// 1b. Self-addressed join guard. The approver is derived from the session's
	//     started-by annotation, so a denied requester who IS the started-by
	//     user would be asked to approve their own join — and cannot, because
	//     `approve = owner` resolves off the very agentsession#owner tuple whose
	//     absence produced this deny. Raising the request would stamp a durable
	//     PendingRequester nobody can resolve, wedging the thread.
	//
	//     A starter denied interact is a provisioning fault, not a permission
	//     decision. The schema is the primary fix: `interact = owner +
	//     started_by + participant - denied` admits the starter on started_by
	//     alone, closing the window between channelsd writing started_by at
	//     create and the operator's owner resolver writing #owner. This remains
	//     the backstop for the residual case where started_by ITSELF is absent,
	//     because a durable unresolvable approval request is far worse than one
	//     extra comparison. Refuse, log loudly, and tell the user plainly.
	if starter := spiceboxv1alpha1.StartedBySubject(sess); starter != "" && starter == canonical.Subject() {
		log.FromContext(ctx).Info("session-join request suppressed: requester IS the session's started-by user, so its only approver would be themselves — agentsession#owner is missing (not yet written by the operator's owner resolver, or never written for this session)",
			"session", sess.Namespace+"/"+sess.Name,
			"requester", canonical.String(),
			"phase", sess.Status.Phase)
		return channelkinds.InboundDecision{
			Outcome: channelkinds.OutcomeDeniedByPermission,
			Notice:  starterDeniedNotice(),
		}, nil
	}

	// 2. Dedup.
	for _, pr := range sess.Status.PendingRequesters {
		if pr.Kind == ev.ExternalIDs.Kind.String() && pr.ExternalID == ev.ExternalIDs.ExternalID.String() {
			return channelkinds.InboundDecision{
				Outcome: channelkinds.OutcomeDeniedByPermission,
				// The user already has a pending rejection in this thread;
				// re-posting one per message is spam, not information.
				Notice: notice.Suppressed("requester already has a pending join request in this thread"),
			}, nil
		}
	}

	// requestRef is minted up front and embedded in BOTH the durable
	// PendingRequester record and the interaction_request's RequestRef, so the
	// bound decision handler can resolve exactly which pending entry an
	// Approve/Deny click decides.
	requestRef := "perm-" + sess.Name + "-" + mintRequestID()

	// 2b. Derive the approver BEFORE writing anything durable, and refuse if
	// there is nobody to address. The approver comes from the started-by
	// annotations, which a cron-spawned session has none of: without this check
	// the request is written to status, published to an unaddressable audience,
	// and delivered to no one — the requester sees silence and the pending
	// entry sits forever. Deriving first means no orphan record is left behind.
	approver := approverIdentityFor(sess, ev.ExternalIDs.Kind)
	if !approverAddressable(approver) {
		log.FromContext(ctx).Info("permission_request: refusing join request — session has no addressable approver",
			"session", sess.Namespace+"/"+sess.Name,
			"requester", ev.ExternalIDs.ExternalID.String(),
			"hint", "session has no started-by (cron/bento-spawned?); no one can approve a join request for it")
		// Tell the platform admins watching the monitoring channel: this needs
		// a human with cluster access, not a chat click. Best-effort.
		p.publishUnapprovableJoinMonitoring(ctx, sess, ev)
		// Refuse cleanly rather than erroring. This is a permanent configuration
		// condition, and a bento inbound retries forever on error, turning one
		// misconfiguration into a request-per-second flood. The notice gives
		// the requester a real explanation instead of silence.
		return channelkinds.InboundDecision{
			Outcome: channelkinds.OutcomeDeniedByPermission,
			Notice: notice.New(categories.JoinNoApprover, notice.Args{
				Lead:     "Can't add you to this session",
				Body:     "It was started automatically, so it has no owner who could approve your request.",
				NextStep: "A platform admin has been notified — start your own thread in the meantime.",
				Audience: participantsAudience(),
			}),
		}, nil
	}

	// 3. Stamp PendingRequesters + condition in a single patch, BEFORE the
	// envelope publish and with a VERIFIED readback below, so the durable record
	// an Approve button resolves against exists before the approver can see it.
	// Publishing first would let a status-patch failure leave a live Approve
	// button with nothing behind it.
	updated := sess.DeepCopy()
	now := metav1.NewTime(p.Now())
	// Stash the message text + channel key so the approve path can replay it
	// and the user need not retype. Deliberately NOT truncated: the approver
	// agreed to a preview-truncated message, so replaying a silent half-message
	// would smuggle different content past approval. Over the cap, MessageText
	// is left empty and the approve flow falls back to "please re-send". 16KB
	// covers any reasonable chat message; etcd's per-resource cap is ~1MB.
	const maxStashedText = 16 * 1024
	stashedText := ev.MessageText
	if len(stashedText) > maxStashedText {
		stashedText = "" // signal "too long, ask user to re-send"
	}
	updated.Status.PendingRequesters = append(updated.Status.PendingRequesters,
		spiceboxv1alpha1.PendingRequester{
			Kind:        ev.ExternalIDs.Kind.String(),
			TeamScope:   ev.ExternalIDs.TeamScope.String(),
			ExternalID:  ev.ExternalIDs.ExternalID.String(),
			Email:       ev.ExternalIDs.Email.String(),
			RequestRef:  requestRef, // matches the published interaction_request's RequestRef
			RequestedAt: now,
			MessageText: stashedText,
			ChannelKey:  ev.ChannelKey,
		})
	conditions.Set(updated, &updated.Status.Conditions, metav1.Condition{
		Type:    spiceboxv1alpha1.AgentSessionConditionPermissionRequestPending,
		Status:  metav1.ConditionTrue,
		Reason:  "RequesterPending",
		Message: fmt.Sprintf("%d pending request(s)", len(updated.Status.PendingRequesters)),
	})
	if err := applyApprovalStatus(ctx, p.K8s, updated, sess); err != nil {
		log.FromContext(ctx).Error(err, "apply agentsession approval status",
			"session", sess.Name, "requester", ev.ExternalIDs.ExternalID)
		return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
			fmt.Errorf("apply agentsession approval status on %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	// Verify the patch actually persisted PendingRequesters. kube-apiserver
	// SILENTLY prunes fields the CRD's OpenAPI schema does not declare, so a
	// stale CRD makes every patch return no error while dropping
	// status.pendingRequesters — and Approve then does nothing. Re-read and
	// assert rather than trust a no-error patch.
	var verify spiceboxv1alpha1.AgentSession
	if err := p.K8s.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &verify); err != nil {
		return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
			fmt.Errorf("verify PendingRequesters readback on %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	if !pendingRequestersContains(verify.Status.PendingRequesters, ev.ExternalIDs.Kind.String(), ev.ExternalIDs.ExternalID.String()) {
		return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
			fmt.Errorf("status.pendingRequesters did not persist after patch on %s/%s — likely a stale CRD: re-apply config/crds/agentprimitives.authzed.com_agentsessions.yaml",
				sess.Namespace, sess.Name)
	}

	// 4. Publish interaction_request(permission_request). The generic
	// "interaction" sub-channel sender posts the public "awaiting approval" note
	// and DMs the owner an Approve/Deny prompt. A publish failure must surface as
	// an internal error, never as a parallel inline post — that would duplicate
	// the rejection once the bus recovers and the relay drains.
	// Resolve AgentClass.Spec.IdentityMode so the sender can show an
	// elevated-risk warning under userPassthrough, where approval grants the
	// requester proxy use of the approver's connected accounts. A lookup miss
	// falls back to the standard approval UX rather than blocking the approval
	// DM on a transient API blip.
	var identityMode string
	var ac spiceboxv1alpha1.AgentClass
	acFound := false
	if sess.Spec.Class != "" {
		if err := p.K8s.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Spec.Class}, &ac); err == nil {
			identityMode = ac.Spec.IdentityMode
			acFound = true
		} else {
			log.FromContext(ctx).Info("permission_request: AgentClass lookup miss for identityMode; falling back to default UX",
				"session", sess.Name, "class", sess.Spec.Class, "err", err.Error())
		}
	}
	// Under userPassthrough, name the approver's linked services concretely in
	// the warning DM. Any failing step degrades to the generic "your connected
	// accounts" phrasing — still effective, just a blunter signal.
	var linkedServices []string
	if identityMode == spiceboxv1alpha1.IdentityModeUserPassthrough && acFound {
		linkedServices = p.resolveApproverLinkedServices(ctx, sess, &ac)
	}
	// The public note names nobody, for the same reason as
	// permissionRequestBody: PublicNoteBody is inert text by wire contract, and
	// the Slack kind escapes it (publicNoteText) before appending the approver
	// mentions IT resolved. So this sentence is second person — the note replies
	// in the requester's own thread — and the approver comes from
	// Audience.Approvers, which the surface renders as its own live "Waiting on
	// …" clause. Interpolating a mention here yields dead escaped markup AND
	// duplicates that clause.
	//
	// The PendingRequesters stash carries only MessageText, and decidePermission
	// rebuilds a synthetic InboundEvent with no Attachments field, so the
	// requester's files cannot survive the approve-replay. Naming that here, in
	// the one public message this flow sends, tells them up front rather than
	// letting the file vanish once they're approved.
	publicNote := "⏳ Approval needed — your request to join this session is awaiting approval."
	if len(ev.Attachments) > 0 {
		publicNote += " Note: " + pluralAttachedFiles(len(ev.Attachments)) + " on this message won't carry through once approved — please re-send after approval."
	}
	// The approver identity keeps the raw+email form from the started-by
	// annotations rather than a precomputed canonical stuffed into ExternalID:
	// ExternalID stays honestly raw, and the Slack sender derives the canonical
	// itself before resolving it to a user id. Email is populated whenever the
	// channel vouched for one, and also serves callers that identify approvers
	// by email directly (the e2e harness's identityHandle). It was derived and
	// addressability-checked in the "2b." block, before any durable write.
	req := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Category:        categories.PermissionRequest,
		RequestRef:      requestRef,
		Lead:            "Session join request",
		Body:            permissionRequestBody(sess.Spec.Class, identityMode, linkedServices),
		// Who is asking, as a structured identity rather than markup baked into
		// the body above — see requesterField.
		Fields: []channelevents.InteractionField{requesterField(ev.ExternalIDs)},
		// WHAT they said, as the payload's one untrusted region rather than a
		// quoted sentence in the body above — see requesterMessageExcerpt.
		Excerpt: requesterMessageExcerpt(ev.MessageText),
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:          channelevents.AudienceApprovers,
			Approvers:      []channelevents.ExternalIdentity{approver},
			PublicNote:     true,
			PublicNoteBody: publicNote,
		},
	}
	if err := req.Validate(); err != nil {
		return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
			fmt.Errorf("built invalid permission interaction_request (session %s/%s): %w", sess.Namespace, sess.Name, err)
	}
	if err := channelevents.PublishOut(p.NATS.Publish,
		sess.Namespace, sess.Name,
		channelevents.KindInteractionRequest, req,
	); err != nil {
		log.FromContext(ctx).Error(err, "publish permission interaction_request",
			"session", sess.Name, "requester", ev.ExternalIDs.ExternalID)
		return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError}, fmt.Errorf("publish permission interaction_request: %w", err)
	}

	// Suppressed deliberately: the permission_request sender is the sole source
	// of the in-thread rejection, and tracks its message ts so it can edit it on
	// the Approve/Deny click. A live notice here would have the listener post a
	// second identical rejection. Suppressed posts nothing and logs the reason.
	return channelkinds.InboundDecision{
		Outcome: channelkinds.OutcomeDeniedByPermission,
		Notice:  notice.Suppressed("the permission_request interaction already told the requester; an in-thread message would double-report it"),
	}, nil
}

// ResubmitAuthorized replays a previously-denied message AFTER the approver
// clicked Approve and the participant relation was written. It skips the
// permission check — that tuple was just written here, and MinimizeLatency may
// not yet reflect it — and runs Deliver's post-permission steps: memory append,
// wake-annotation patch on Idle, NATS wakeup.
//
// CALLER CONTRACT: the participant relation for ev.ExternalIDs must already be
// written and the matching PendingRequester confirmed removed. Otherwise this
// smuggles an unauthorized message past the permission check.
func (p *Pipeline) ResubmitAuthorized(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ev channelkinds.InboundEvent) error {
	if err := p.recordGoalActor(ctx, sess, ev); err != nil {
		return err
	}
	if ev.MessageText == "" && len(ev.Attachments) == 0 {
		return nil // nothing to replay
	}
	content := p.attachmentContent(ctx, ev, sess.Spec.Class, sess.Namespace, sess.Name)
	if err := p.Memory.Append(ctx, sess.Namespace, sess.Name, MemTurn{
		Role: "user", Author: authorSubject(ev), Via: ev.Via, Content: content,
	}); err != nil {
		return fmt.Errorf("resubmit memory append: %w", err)
	}

	// Same watch-relationship mirror the primary Deliver append gives an
	// `attended` child (attended_watch.go): a no-op for every other session,
	// and here too because a denied-then-approved sender's replayed turn is
	// still a turn of the child's own conversation — the parent must SEE it
	// unconditionally, not just the ones that happened to clear permission on
	// the first try. Reuses the SAME content already fetched above rather
	// than re-resolving attachments a second time.
	p.mirrorToAttendedParent(ctx, sess, ev, content)

	// Index the inbox turn by its channel msg ref (best-effort; see Deliver).
	if ref := ev.MsgRef(); ref != "" {
		if idx, ok, qerr := p.lastInboxTurnIndex(ctx, sess.Namespace, sess.Name); ok && qerr == nil {
			if rerr := p.Memory.RecordChannelMsgRef(ctx, sess.Namespace, sess.Name, ev.ExternalIDs.Kind.String(), ref, idx); rerr != nil {
				log.FromContext(ctx).Info("ResubmitAuthorized: RecordChannelMsgRef failed; restart UI will not find this msg",
					"session", sess.Namespace+"/"+sess.Name, "ref", ref, "err", rerr.Error())
			}
		} else if qerr != nil {
			log.FromContext(ctx).Info("ResubmitAuthorized: lastInboxTurnIndex failed; skipping channel_msg_ref write",
				"session", sess.Namespace+"/"+sess.Name, "err", qerr.Error())
		}
	}

	// The wake-requested-at annotation starts a fresh runner pod for an Idle
	// session. Non-Idle already has a runner up and reading memory.
	if sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseIdle {
		updated := sess.DeepCopy()
		if updated.Annotations == nil {
			updated.Annotations = map[string]string{}
		}
		updated.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = p.Now().Format(time.RFC3339Nano)
		// RV-free: a preceding approval status write may have advanced the
		// session's resourceVersion since the caller's snapshot, so an
		// optimistic-locked patch would conflict. This only adds the wake
		// annotation, so a plain merge patch is safe.
		if err := p.K8s.Patch(ctx, updated, client.MergeFrom(sess)); err != nil {
			return fmt.Errorf("resubmit wake annotation: %w", err)
		}
	}
	if err := p.publishWakeup(sess.Namespace, sess.Name); err != nil {
		return fmt.Errorf("resubmit publish wakeup: %w", err)
	}
	return nil
}

// lastInboxTurnIndex returns the highest turn index currently recorded for
// (ns, name) via ReadAll. It is called immediately after a successful
// Memory.Append so the returned index is the just-written turn's position.
//
// Returns (index, true, nil) when at least one turn exists, or
// (0, false, nil) when the session is empty (not an error). An error is
// returned only when ReadAll itself fails.
func (p *Pipeline) lastInboxTurnIndex(ctx context.Context, ns, name string) (int, bool, error) {
	turns, err := p.Memory.ReadAll(ctx, ns, name)
	if err != nil {
		return 0, false, err
	}
	max := -1
	for _, t := range turns {
		if t.Index > max {
			max = t.Index
		}
	}
	if max < 0 {
		return 0, false, nil
	}
	return max, true, nil
}

// pendingRequestersContains reports whether (kind, externalID) is present in
// prs. Verifies a status patch actually persisted, since kube-apiserver
// silently prunes unknown fields when the CRD is stale.
func pendingRequestersContains(prs []spiceboxv1alpha1.PendingRequester, kind, externalID string) bool {
	for _, pr := range prs {
		if pr.Kind == kind && pr.ExternalID == externalID {
			return true
		}
	}
	return false
}

// truncateRunes returns s truncated to at most n runes; appends an
// ellipsis when truncation occurred.
func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// toEnvelopeIdentity copies a channelkinds.ExternalIdentity into the
// channelevents-shaped form (channelevents intentionally doesn't import
// channelkinds — kinds depend on channelevents, not the reverse).
func toEnvelopeIdentity(id channelkinds.ExternalIdentity) channelevents.ExternalIdentity {
	return channelevents.ExternalIdentity{
		Kind: id.Kind, TeamScope: id.TeamScope, ExternalID: id.ExternalID, Email: id.Email,
	}
}

// mintRequestID returns a 32-character hex string from 16 random bytes,
// correlating a KindEnqueueAck with the interrupt button the channel kind
// renders alongside it (echoed back on InterruptRequestPayload and
// InterruptAppliedPayload). Mirrors the runner's newRequestID shape.
//
// Panics on a crypto/rand error: it never fails in practice, and a channelsd
// that cannot generate random correlation IDs is already in deep trouble.
func mintRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("channelsd: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// Compile-time guard that *Pipeline implements channelkinds.InboundPipeline.
var _ channelkinds.InboundPipeline = (*Pipeline)(nil)

// InboundAssetMember is one stored member of an exploded archive, as the
// operator's /inbound-asset route reports it.
//
// It carries no readability class: the operator already wrote that into the
// index, and re-deriving it here would be a second source of truth for one
// fact — the shape this pipeline avoids everywhere else.
type InboundAssetMember struct {
	Name      string
	MIME      string
	SizeBytes int64
	Ref       string
	TextRef   string
	Pages     int
}

// lockInteraction bounds lock storage while serializing competing outcomes for
// the same request, including recovery.
func (p *Pipeline) lockInteraction(namespace, session, request string) func() {
	digest := sha256.Sum256([]byte(namespace + "/" + session + "/" + request))
	lock := &p.interactionTransitions[int(digest[0])%len(p.interactionTransitions)]
	lock.Lock()
	return lock.Unlock
}
