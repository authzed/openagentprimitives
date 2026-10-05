//go:build e2e

package e2e

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/go-logr/logr"
	natsserver "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	_ "github.com/authzed/openagentprimitives/pkg/agent/harness/apnative" // register the default harness for agentclassctrl.Reconciler
	"github.com/authzed/openagentprimitives/pkg/agent/restartmarker"
	"github.com/authzed/openagentprimitives/pkg/agent/runner/identityadvisor"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	guardianschema "github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/authz/handoff"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate/hold"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	_ "github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource/imports" // completes the relsource claim table: this harness constructs guarded writers for every wiring under test (relwrites, pttagmint, guardian grants, ...)
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/onepassword" // register onepassword relsync.Kind for the relationshipsource controller (relsync.Get); also reachable transitively via the relsource/imports blank import above, but this is the explicit wiring site for THIS aspect, not an accident of that one
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/resolve"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/historyresp"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/outbound"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	agentclassctrl "github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	agentidentityctrl "github.com/authzed/openagentprimitives/pkg/controllers/agentidentity"
	agentsessionctrl "github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	channelctrl "github.com/authzed/openagentprimitives/pkg/controllers/channel"
	guardianctrl "github.com/authzed/openagentprimitives/pkg/controllers/guardian"
	mcpserverctrl "github.com/authzed/openagentprimitives/pkg/controllers/mcpserver"
	relationshipsourcectrl "github.com/authzed/openagentprimitives/pkg/controllers/relationshipsource"
	sidecartoolboxctrl "github.com/authzed/openagentprimitives/pkg/controllers/sidecartoolbox"
	skillctrl "github.com/authzed/openagentprimitives/pkg/controllers/skill"
	subagentrequestctrl "github.com/authzed/openagentprimitives/pkg/controllers/subagentrequest"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	pkgmemory "github.com/authzed/openagentprimitives/pkg/memory"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/channel_msg_ref"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/envelopefact"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalactor"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttag"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/triggerdelivery"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	memsearch "github.com/authzed/openagentprimitives/pkg/memory/search"
	searchinmem "github.com/authzed/openagentprimitives/pkg/memory/search/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/spicedbauthorizer"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker/inproc"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
	"github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
	skillbundlememory "github.com/authzed/openagentprimitives/pkg/tools/skillbundle/memory"
	"github.com/authzed/openagentprimitives/pkg/tools/workshopmcp"
	"github.com/authzed/openagentprimitives/pkg/web/gateway"
	"github.com/authzed/openagentprimitives/test/testspicedb"

	// Register channel kinds for the in-process operator (T8 wires
	// these into the manager; importing here is harmless and ensures
	// the fake kind is available at AgentClass-validation time).
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"

	// Registers the github channel kind (webhook-driven, input-only) so
	// ApplyGitHubChannel's kind=github Channel resolves via the registry.
	// See reviewbot_dedup.go (Task 6). The SAME import also runs
	// relsync_kind.go's init(), registering github's relsync.Kind for the
	// relationshipsource controller (relsync.Get) — also reachable
	// transitively via the relsource/imports blank import above, but this is
	// the explicit wiring site for THIS aspect, not an accident of that one.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
)

// Options shape what Start boots.
type Options struct {
	// WithGoalExecution enables the durable goals runtime and bounded roots.
	WithGoalExecution bool

	// AgentDir is a path to a testdata directory whose *.yaml files
	// are applied + waited on before tests run. The harness substitutes
	// the literal `{{MCP_URL}}` with the live MCPStub URL before apply.
	// Empty AgentDir skips the apply pass — useful for tests that just
	// want the boot infrastructure.
	AgentDir string

	// Namespace to apply manifests into. Default: "default".
	// (envtest doesn't auto-create namespaces, so the default has to
	// match the namespace metadata.namespace fields in the AgentDir
	// YAMLs.)
	Namespace string

	// DefaultUser is the channel-identity used by SendUserMessage when
	// no AsUser option is provided.
	DefaultUser string

	// DefaultTimeout for Expect* calls in later-task APIs.
	DefaultTimeout time.Duration

	// AwaitIdleTTL, when > 0, makes await_user_message PARK in-process (block on
	// a live InboundCh fed by channelsd's wakeup) for this long before
	// idle-exiting, instead of the default immediate idle-exit. Opt-in per
	// scenario so a test can exercise the real in-process await-resume drain.
	// See InProcessRunnerFactory.AwaitIdleTTL.
	AwaitIdleTTL time.Duration

	// ExtraManifests is a list of YAML strings (NOT file paths) to
	// apply after AgentDir. Used for per-scenario overrides.
	ExtraManifests []string

	// WithToolCallController, when true, registers the real ToolCall
	// reconciler against envtest, wires a fake.Binder + in-mem artifact
	// store + gateway.Registry, and starts gateway.Server on a bufconn.
	// Tests access *fake.Binder + gateway dialer via Harness accessors.
	WithToolCallController bool

	// WithTokenAuthz, when true, wires the durable per-call externaltoken
	// use_token authorization gate end-to-end: the AgentSession reconciler's
	// TokenGranter/TokenChecker (spdbCli — reconcileCredentialGrants writes
	// authorized_token SpiceDB grants before RunnerFactory.Start, and
	// materializeSidecarSecret's pre-handout check) AND the in-process runner
	// factory's MCPTool.SetUseTokenGate wiring (mirrors internal/cmd/runner/main.go).
	// Opt-in (default false) so the many existing scenario tests that don't
	// exercise this feature — and whose credential surfaces were never
	// authored against it — are unaffected; mirrors the WithToolCallController
	// opt-in shape. A scenario testing externaltoken revocation/deny sets this.
	WithTokenAuthz bool

	// WorkspaceStorageClass mirrors the operator flag; set to enable
	// PVC provisioning by the AgentSession reconciler.
	WorkspaceStorageClass string

	// PipelineExtender, if non-nil, is called once with the harness's
	// in-process channelsd pipeline + a typed pre-Start view of the
	// harness's plumbing (k8s client, NATS conn) immediately after
	// pipeline construction and before any Channel listener is started.
	// Scenarios use it to attach fields that production channelsd's
	// internal/cmd/channelsd/main.go wires but the harness leaves nil by default —
	// today, pipeline.PortalAccess (Slice 2.5 α5's chat-triggered
	// portal-access flow). The view shape exists because at extender
	// call time, the test's local `h := e2e.Start(...)` variable is
	// still unassigned (Start hasn't returned), so scenarios cannot
	// capture h itself by reference.
	PipelineExtender func(*pipeline.Pipeline, ExtenderView)

	// SchemaWriteDelay, when > 0, stalls every guardian WriteSchema by this
	// long. It exists to make the harness's own authz-schema readiness
	// barrier (WaitForAuthzSchema) testable deterministically.
	//
	// The window it widens is real and narrow: the AgentClass reconciler
	// creates the class's AgentSessionGrants CR and then stamps Valid=True in
	// the SAME pass, so the guardian's compose→WriteSchema is strictly
	// downstream of the condition every scenario gates on. Measured on an idle
	// machine the schema lands ~45 ms AFTER Valid=True — invisible behind
	// WaitForAgentClassValid's 250 ms poll, and wide open under suite
	// contention, where it surfaced as a session dying with
	// Failed/AuthzWriteFailed "object definition 'agentsession' not found".
	// A delay here reproduces that on demand instead of once per hundred runs.
	SchemaWriteDelay time.Duration

	// PlanGateDenialStreakThreshold, when > 0, wires the forensic-hold tripper
	// (pkg/authz/plangate/hold.NewDenialStreak) with this consecutive-denial
	// threshold. Mirrors internal/cmd/operator/main.go's
	// --plangate-denial-streak-threshold flag, which is an OPERATOR-WIDE
	// setting in production — never per-AgentClass — so there is nowhere in
	// bt.Bundle's declarative schema to carry it; this threads it into the
	// harness the same way WithTokenAuthz mirrors TokenGranter/TokenChecker.
	// Zero (default) leaves the tripper unwired, matching production's own
	// default-off flag — every scenario that does not set this is unaffected.
	PlanGateDenialStreakThreshold int

	// NewOperationID and NewArtifactID override the id minters of the two
	// components that hand a generated id back to the MODEL: the per-session
	// operation registry (op-…) and the artifact service (artifact-…).
	//
	// nil — the default, and what every production binary passes — leaves each
	// component minting its own random ids, so a scenario that does not set
	// these behaves exactly as before.
	//
	// A whole-session REPLAY sets them so the run mints the very ids the
	// captured session did, which is what lets a captured transcript keep its
	// arguments literal. See bt.MintedIDSequence: drawing from a fixed list also
	// makes the count and order of mints an assertion.
	//
	// Typed as plain funcs rather than as the bronzethread sequence so this
	// package does not depend on the bundle format — the seam is the same shape
	// the components themselves expose.
	NewOperationID func() string
	NewArtifactID  func() string

	// NewRenderName and NewRevisionID override the remaining two ids the
	// artifact path hands the MODEL: the ArtifactRender CR name (returned as
	// artifact_prepare's `handle`) and the revision id (`artrev-…`).
	//
	// Same nil-default and same purpose as the pair above. NewRevisionID's
	// argument is the render CR's UID because the production derivation is a
	// function of it — see artifacts.WithRevisionIDMinter for why a replay that
	// ignored the key would break prepare->await idempotence.
	NewRenderName func(session string) string
	NewRevisionID func(uid string) string

	// HoldToolsFromAssembly names non-meta tools that must be kept OUT of the
	// capability assembly — the system prompt's tool listing, introspect_tool's
	// name index and the plan-gate surface are all composed from it — while
	// still reaching the Loop's live tool set, so a later turn can offer them.
	//
	// That is where a tool the runner gains MID-session lands in production:
	// the refresher appends to l.Tools long after everything composed, so the
	// prompt and introspect_tool never learn about it. A whole-session REPLAY
	// needs the same shape for a tool its capture ungated at the fixture, or
	// the composed surfaces describe a session that never happened.
	//
	// Empty — every scenario but a replay — leaves assembly untouched.
	HoldToolsFromAssembly []string

	// FilterOfferedTools narrows each request's tool list to what a captured
	// run recorded for that turn. See runner.Loop.ReplayToolCatalog, whose
	// shape this mirrors; nil (the default) leaves the runner offering
	// everything it composed, exactly as in production.
	FilterOfferedTools func(turnIndex int, offered []string) []string

	// ReplaceAssembledTool rewrites one tool the capability assembly produced,
	// before anything downstream reads the list — the seam a whole-session
	// replay CANS a meta tool's reply through.
	//
	// One tool in, one tool out, so a caller can only substitute, never add or
	// remove: adding would put a tool in the catalog no capability offered, and
	// removing would silently narrow the set the recorded catalog is compared
	// against. An error fails the run loudly rather than leaving the original
	// in place, because a substitution that quietly did not happen is exactly
	// the assertion-that-asserts-nothing this harness keeps failing on.
	//
	// Typed as a plain func over tool.Tool rather than as the bundle's canning
	// map, for the reason the id minters above are plain funcs: this package
	// does not depend on the bundle format. bronzethread's MetaToolCanner
	// supplies one.
	//
	// nil — every scenario but a replay — leaves assembly untouched.
	ReplaceAssembledTool func(tool.Tool) (tool.Tool, error)
}

// delayedSchemaIO wraps a guardian SchemaIO and stalls WriteSchema, widening
// the window between "AgentClass Valid=True" and "composed schema is live".
// Installed only when Options.SchemaWriteDelay > 0; see that field's doc.
type delayedSchemaIO struct {
	inner guardianschema.SchemaIO
	delay time.Duration
}

func (d delayedSchemaIO) ReadSchema(ctx context.Context) (string, error) {
	return d.inner.ReadSchema(ctx)
}

func (d delayedSchemaIO) WriteSchema(ctx context.Context, text string) error {
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return d.inner.WriteSchema(ctx, text)
}

// ExtenderView is the typed slice of harness plumbing visible to a
// PipelineExtender. Today it exposes the k8s client; future scenarios
// may need NATS / signer / etc. — add fields as needed.
type ExtenderView struct {
	K8s client.Client
}

// Harness is the test-facing API. SendUserMessage / ExpectAgentReply
// and the rest of the conversational API land in later tasks; this
// task ships only the boot-time fields.
type Harness struct {
	t       *testing.T
	opts    Options
	LLM     *ScriptedLLM
	MCP     *MCPStub
	SpiceDB *spicedb.Client
	K8s     client.Client
	Scheme  *apiruntime.Scheme

	// NATSURL is the embedded nats-server's client URL. Exposed so T9
	// can dial channelsd against it; T8 wires the manager but does not
	// yet consume the URL itself.
	NATSURL string

	// nc is the harness's own NATS client connection. Shared by the
	// outbound relay, the runner factory's respond_to_user publishing,
	// and any future tests that want to subscribe to envelope subjects
	// for assertions.
	nc *nats.Conn

	// memStore is the in-process memory facade shared between the
	// channelsd pipeline (which Appends inbound user turns) and the
	// runner factory (which reads/writes it through a turn.Appender).
	// Also wired into the AgentSession controller's finalizer for
	// scope cleanup on session deletion (*memory.Local satisfies
	// ScopeDeleter). A single facade across all three boundaries
	// guarantees what the pipeline writes the runner reads.
	memStore *pkgmemory.Local

	// runnerFactory + mgr are the in-process controller plumbing T8
	// installs. Kept on the harness so later tasks can introspect them
	// (e.g. enumerate active sessions during ExpectAgentReply timeouts).
	runnerFactory *InProcessRunnerFactory
	mgr           manager.Manager
	// mgrCancel/mgrDone stop the CURRENT manager goroutine. RestartOperator
	// reuses them to shut the old manager down before booting a new one;
	// Start's own t.Cleanup also closes over them (by value, at cleanup
	// time), so RestartOperator repoints them at the NEW manager's pair,
	// exactly as it repoints h.mgr, so cleanup always tears down whichever
	// manager is current rather than the one Start originally created.
	mgrCancel context.CancelFunc
	mgrDone   chan struct{}

	// secretReader/configMapReader/tokensReg/opSigned are the AgentSession
	// reconciler's own dependencies, promoted from startManager's local
	// variables to the harness so RestartOperator can reconstruct an
	// otherwise-identical Reconciler against a brand-new manager: restarting
	// the OPERATOR does not restart SpiceDB, the memory store, or the
	// runner (a separate process/pod in production, h.runnerFactory's live
	// goroutines here) — only the operator's own Go-process state, which is
	// exactly the manager + Reconciler struct being rebuilt.
	secretReader    *adoptguard.SecretReader
	configMapReader *adoptguard.ConfigMapReader
	tokensReg       *tokens.Registry
	opSigned        pkgmemory.Memory

	// Populated only when Options.WithToolCallController is true. nil otherwise.
	fakeExec      *fake.Binder
	gatewayReg    *gateway.Registry
	gatewayDialer grpc.DialOption
	// toolCallStore is the artifact store the ToolCall reconciler writes each
	// call's stdout/stderr into. Shared with the runner factory so the sandbox
	// tool reads back the SAME bytes when it composes its model-facing result —
	// see installToolCallController for why a private store made every sandbox
	// tool in the harness look like it had printed nothing.
	toolCallStore artifactstore.Store
	// toolSessionDeltas collects every KindToolSessionDelta envelope
	// observed on out.tool_session_delta across all sessions. Filled by
	// a NATS subscription in toolcall_wiring.go. Read via
	// ExpectToolSessionDelta — mirrors the slice + cursor pattern used by
	// ExpectAgentReply (see Drv.Sent()): non-matching envelopes stay in
	// the slice for later Expect calls, and the cursor only advances past
	// envelopes that matched a predicate.
	toolSessionDeltas     []toolSessionDelta
	toolSessionDeltasMu   sync.Mutex
	toolSessionDeltasSeen int

	// slackFake + slackSource back the real Slack channel kind's
	// listener + sender when a test's Channel CRs declare kind: slack —
	// see slack.InstallTestTransport in Start. The install is
	// unconditional and inert for tests that never define a kind: slack
	// Channel: the real slack.Kind is only reached via
	// resolve.ForChannel/ForSession for a Channel whose spec.kind is
	// "slack", so a scenario with only kind: fake Channels never
	// constructs a slack client and never consults these globals.
	slackFake   *fakeslack.Client
	slackSource *fakeslack.SocketSource

	// memAdapter is the channelsd pipeline's Memory implementation
	// (startChannelsdPlumbing). Retained here so SetInboundAssetUploader can
	// reach its uploadFn hook after Start has returned.
	memAdapter *pipelineMemAdapter

	// authzSchemaLive latches once WaitForAuthzSchema has seen the guardian's
	// composed schema go live. The schema is never unwritten within a harness,
	// so every later barrier is a single atomic load instead of a SpiceDB RPC.
	authzSchemaLive atomic.Bool

	// markerSigner attests the status.pendingRestart markers the harness
	// pipeline writes, standing in for channelsd's registered publisher key.
	// Exposed via SignRestartMarker for scenarios that patch a marker directly
	// instead of driving it through an inbound message.
	markerSigner    *restartmarker.Signer
	goalActorSigned pkgmemory.Memory
	goals           *goalRuntime

	// FakeGitHub backs the kind=github webhook e2e (Task 6, reviewbot dedup):
	// an httptest stand-in for the three GitHub REST surfaces the review loop
	// touches (installation-token exchange, check-run list, check-run create).
	// nil until the first ApplyGitHubChannel call constructs it lazily.
	FakeGitHub *FakeGitHub

	// githubAppMinter is what delegatingGitHubAppMinter forwards to, and the
	// lock is not optional: SetGitHubAppMinter writes it on the test goroutine
	// while the toolcall reconciler reads it on its own.
	githubAppMinterMu sync.Mutex
	githubAppMinter   credkind.GitHubAppMinter

	// githubWebhookHandler is the REAL production channelwebhook.UI route
	// handler (pkg/web/webui/channelwebhook), mounted over an httptest
	// request/recorder pair by PostWebhook. Lazily constructed by the first
	// ApplyGitHubChannel call, once h.K8s and h.nc exist.
	githubWebhookHandler http.Handler

	// workshopMCP is the httptest.Server fronting the REAL
	// pkg/tools/workshopmcp.Server, mounted by mountWorkshopMCP against
	// h.K8s (env.Client) — see that func's doc for what this proves and what
	// it deliberately does not. nil would mean Start failed before reaching
	// the mount; every Start call reaches it.
	workshopMCP *httptest.Server
	// workshopMCPServer is the same mount's underlying *workshopmcp.Server —
	// kept alongside workshopMCP (its HTTP front end) so BindWorkshopSession
	// has something to call SetBuilderSession on. See mountWorkshopMCP's doc
	// for why the driver needs to bind this after construction at all.
	workshopMCPServer *workshopmcp.Server
}

// WorkshopMCPURL returns the real workshop MCP server's endpoint (see
// mountWorkshopMCP). driver.go's sidecar-probe routing closure dispatches the
// workshop SidecarToolbox's calls here instead of the canned h.MCP stub, by
// matching on the resolved toolbox's ref.
func (h *Harness) WorkshopMCPURL() string { return h.workshopMCP.URL }

// BindWorkshopSession late-binds the real workshop MCP mount's
// SessionNamespace/SessionName to the builder AgentSession's actual
// (namespace, name), once the caller has confirmed that session exists (see
// test/e2e/threadrun's bindWorkshopSessionOnce, the only caller). A thin
// forward to workshopmcp.Server.SetBuilderSession — see that method's own
// doc for the concurrency guard that makes it safe to call while the mount is
// already serving other tool calls.
func (h *Harness) BindWorkshopSession(namespace, name string) {
	h.workshopMCPServer.SetBuilderSession(namespace, name)
}

// StampSpiceboxClassesValid marks every SpiceboxClass Valid=True. This harness
// deliberately does not run the real SpiceboxClass controller (it does real
// image-registry probing and sandbox-template pre-warming, far outside what
// these suites exercise — see startManager's own scoping note), so nothing
// else ever sets that condition.
//
// It lives HERE, not in one consumer, because it became load-bearing for every
// caller the moment the REAL sidecartoolbox.Reconciler replaced the old
// hand-stamped Valid: that controller's classCheck phase Gets the toolbox's
// spec.sandbox.class and requires ITS Valid condition True before continuing.
// The bundle driver learned that first; the sidecar scenarios, which never go
// through that driver, hit the identical AgentClass reason=ClassInvalid until
// they called this too. One implementation, so a third consumer cannot
// rediscover it the hard way.
//
// A no-op when no SpiceboxClass exists, and idempotent for one already True.
func (h *Harness) StampSpiceboxClassesValid() {
	h.t.Helper()
	var list spiceboxv1alpha1.SpiceboxClassList
	if err := h.K8s.List(context.Background(), &list); err != nil {
		// A List failure here means the CRD is not installed, which is exactly
		// the case where no fixture declares one.
		return
	}
	for i := range list.Items {
		cls := &list.Items[i]
		if meta.IsStatusConditionTrue(cls.Status.Conditions, spiceboxv1alpha1.SpiceboxClassConditionValid) {
			continue
		}
		meta.SetStatusCondition(&cls.Status.Conditions, metav1.Condition{
			Type:               spiceboxv1alpha1.SpiceboxClassConditionValid,
			Status:             metav1.ConditionTrue,
			Reason:             "SpecOK",
			Message:            "stamped Valid=True by the e2e harness (SpiceboxClass controller not wired)",
			ObservedGeneration: cls.Generation,
		})
		if err := h.K8s.Status().Update(context.Background(), cls); err != nil {
			h.t.Fatalf("e2e: stamp SpiceboxClass %q Valid=True: %v", cls.Name, err)
		}
	}
}

// SignRestartMarker stamps the harness's connector attestation onto a
// PendingRestart a scenario built by hand. Scenarios that write
// status.pendingRestart directly (simulating channelsd) MUST call it: the
// operator refuses an unsigned marker, because the session's own runner can
// write that field too. parent must be the session the marker will be stored
// on, with its UID populated — the digest binds to it.
func (h *Harness) SignRestartMarker(parent *spiceboxv1alpha1.AgentSession, pr *spiceboxv1alpha1.PendingRestart) error {
	return h.markerSigner.Sign(parent, pr)
}

// toolSessionDelta is the unexported channel item — exported via the
// accessor helpers in Task 5.
type toolSessionDelta struct {
	Subject string
	Env     channelevents.Envelope
}

// Memory exposes the harness's in-process memory facade so admin/audit
// tests can query the same store the pipeline + runner write to.
func (h *Harness) Memory() *pkgmemory.Local { return h.memStore }

// FakeExec returns the fake executor binder wired by WithToolCallController.
// Panics if WithToolCallController was false.
func (h *Harness) FakeExec() *fake.Binder {
	if h.fakeExec == nil {
		h.t.Fatal("FakeExec: Options.WithToolCallController must be true")
	}
	return h.fakeExec
}

// Registry returns the gateway.Registry wired by WithToolCallController.
// Panics if WithToolCallController was false.
func (h *Harness) Registry() *gateway.Registry {
	if h.gatewayReg == nil {
		h.t.Fatal("Registry: Options.WithToolCallController must be true")
	}
	return h.gatewayReg
}

// GatewayDialer returns a grpc.DialOption that routes the runner's bridge
// over the in-process bufconn. Panics if WithToolCallController was false.
func (h *Harness) GatewayDialer() grpc.DialOption {
	if h.gatewayDialer == nil {
		h.t.Fatal("GatewayDialer: Options.WithToolCallController must be true")
	}
	return h.gatewayDialer
}

// SetSidecarProbeURL installs the in-process runner factory's sidecar
// probe-URL seam. The in-process MCPStub listens on a random httptest
// port, not the controller-allocated rt.Port, so a sidecar scenario must
// redirect the runner's sidecar probe at the stub (h.MCP.URL()). Call
// after Start (the factory exists by then) and before the AgentSession is
// spawned (i.e. before SendUserMessage / applying the session-driving CR).
func (h *Harness) SetSidecarProbeURL(fn func(rt spiceboxv1alpha1.ResolvedSidecarToolbox) string) {
	h.runnerFactory.SidecarProbeURL = fn
}

// SetMinter installs a federation.Minter on the in-process runner
// factory's token broker seam. Must be called BEFORE the session that
// needs federated credentials is spawned (the broker is constructed
// per-session inside buildMCPTools). When nil, federated credentials
// fail closed — existing tests are unaffected because they use only
// static/oauth credentials.
func (h *Harness) SetMinter(m federation.Minter) {
	h.runnerFactory.Minter = m
}

// SetGitHubAppMinter installs a credkind.GitHubAppMinter on BOTH brokers a
// session resolves credentials through — the exact mirror of SetMinter, for the
// other minted credential type. When nil, githubApp credentials fail closed,
// which is what every existing test wants.
//
// # Why both
//
// An MCP call resolves through the runner's own per-session broker; a SANDBOX
// call is a ToolCall CR the toolcall controller resolves for, through a broker
// built once at Start. A minter wired into only the first leaves every sandbox
// dispatch failing with "no GitHub App minter configured" while the MCP path
// works — which reads as a credential problem rather than a wiring one.
//
// Call BEFORE the session that needs the credential is spawned. The runner's
// broker is constructed per-session inside buildMCPTools, and the toolcall
// broker reads the delegate through a lock on every resolve, so a later
// assignment reaches both.
func (h *Harness) SetGitHubAppMinter(m credkind.GitHubAppMinter) {
	h.runnerFactory.GitHubAppMinter = m

	h.githubAppMinterMu.Lock()
	h.githubAppMinter = m
	h.githubAppMinterMu.Unlock()
}

// newToolCallBroker builds the toolcall controller's broker with a minter that
// DELEGATES to whatever SetGitHubAppMinter last installed.
//
// The indirection exists because of an ordering problem with no other clean
// answer: this broker is constructed inside Start, and the fixture provider a
// minter would point at does not exist until a bundle's own setup runs, well
// after. Assigning the field later would be a data race against the reconciler
// goroutine; delegating through a lock is the same seam, read-safe.
func (h *Harness) newToolCallBroker() *inproc.Broker {
	b := inproc.New(h.mgr.GetClient())
	b.GitHubApp = delegatingGitHubAppMinter{h: h}
	return b
}

// delegatingGitHubAppMinter forwards to the harness's current minter, or fails
// closed exactly as a nil one does.
//
// The refusal wording is deliberately the credkind's own, so a test that has
// simply not wired a minter sees the same message production shows when nobody
// configured one — rather than a harness-specific phrase nothing else explains.
type delegatingGitHubAppMinter struct{ h *Harness }

func (d delegatingGitHubAppMinter) Mint(
	ctx context.Context, appID string, privateKeyPEM []byte, installationID string,
) (sensitive.SensitiveValue, time.Time, error) {
	d.h.githubAppMinterMu.Lock()
	m := d.h.githubAppMinter
	d.h.githubAppMinterMu.Unlock()

	if m == nil {
		return sensitive.SensitiveValue{}, time.Time{},
			fmt.Errorf("no GitHub App minter configured")
	}
	return m.Mint(ctx, appID, privateKeyPEM, installationID)
}

// SetContentInspectors installs content-guard instances on the in-process
// runner factory. Call BEFORE the session spawns (the Loop is built per
// session). Mirrors SetMinter.
func (h *Harness) SetContentInspectors(instances []contentguard.Instance, ids []string) {
	h.runnerFactory.ContentInspectors = instances
	h.runnerFactory.ContentInspectorIDs = ids
}

// SetIdentityRecommender installs the isolated advisory recommender the runner
// wires for identityMode=dynamic sessions. Call BEFORE the session spawns (the
// Loop is built per session). Mirrors SetMinter / SetContentInspectors. Tests
// pass an identityadvisor.Fake so a dynamic-mode identity-choice request carries
// a deterministic {mode, reason} suggestion; leaving it unset degrades dynamic
// to a plain ask (no recommendation), matching the gate's fail-open contract.
func (h *Harness) SetIdentityRecommender(p identityadvisor.Provider) {
	h.runnerFactory.IdentityRecommender = p
}

// SetInboundAssetUploader installs a per-scenario UploadInboundAsset
// implementation on the harness's channelsd Memory adapter, standing in for
// the operator's real POST /inbound-asset/{ns}/{sess} route (production:
// internal/cmd/channelsd/memory.go's memoryClient.UploadInboundAsset →
// pkg/memory/httpsrv/inbound_asset.go's serveInboundAsset). Unset (the
// default) always returns ErrInboundAssetUploadNotWired — see that
// sentinel's doc — so every e2e attachment resolves through the pipeline's
// own transient-failure path unless a scenario opts in here. Call before
// driving any inbound message carrying attachments.
func (h *Harness) SetInboundAssetUploader(fn func(ctx context.Context, ns, sess, mime, filename string, body io.Reader) (pipeline.InboundAssetResult, error)) {
	if h.memAdapter == nil {
		h.t.Fatal("SetInboundAssetUploader: called before Start finished wiring the channelsd pipeline (h.memAdapter is nil)")
	}
	h.memAdapter.uploadFn = fn
}

// SetArtifactStore installs an artifactstore.Store on the in-process runner
// factory, activating RunnerEnv.Artifacts (artifacts.NewService(h.memStore),
// matching internal/cmd/runner's own artifactSvc wiring) and the files modality's
// Tier-1 fetch_artifact reader (files.StoreReader{Store: store}) — see
// buildLoop's env construction. Both stay nil (as before this seam existed)
// when unset, so every scenario that doesn't need fetch_artifact is
// unaffected. Call before the session that needs it spawns (the RunnerEnv is
// built per-session inside buildLoop). Mirrors SetMinter / SetSidecarProbeURL.
func (h *Harness) SetArtifactStore(store artifactstore.Store) {
	h.runnerFactory.ArtifactStore = store
}

// MarkSidecarReady simulates the operator bringing up a secret-gated
// separate-pod SidecarToolbox in-process: it stamps an entry into
// AgentSession.status.resolvedSidecarToolboxes for the given (LLM-facing
// `name`, CR `ref`) with RunMode=separate-pod, AwaitingSecret=false, and a
// reachable SidecarPodIP:port pointing at `dispatchURL` (typically the
// harness MCP stub, h.MCP.URL()). The in-process runner's mid-session
// ToolRefresher (wired in buildLoop when K8s is set) then re-reads this
// status on its next turn, probes the stub, and synthesizes the sidecar's
// tools into the live tool set — exactly as internal/cmd/runner's newSidecarToolRefresher
// does against a real separate-pod sidecar whose pod just went Ready.
//
// Production does this via real pod-create (the reconciler reflects the
// pod's PodIP into status once Ready); the in-process harness has no real
// pod, so the test stamps the equivalent status directly. spec is snapshotted
// onto the resolved entry so the synthesizer sees the same Tools allowlist +
// SecretInputs the SidecarToolbox CR declares.
//
// Call AFTER Start and (typically) BEFORE SendUserMessage — the refresher's
// own secret-output-Secret gate (see newSidecarToolRefresher) holds the
// sidecar tool back until the producer publishes its secret, so stamping
// readiness up front does not leak the tool into the boot tool set.
func (h *Harness) MarkSidecarReady(ns, name, llmName, ref, dispatchURL string, spec spiceboxv1alpha1.SidecarToolboxSpec) {
	h.t.Helper()
	ctx := context.Background()

	host, port := splitHostPort(h.t, dispatchURL)

	var sess spiceboxv1alpha1.AgentSession
	require.NoError(h.t, h.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess),
		"MarkSidecarReady: get AgentSession %s/%s", ns, name)
	base := sess.DeepCopy()

	entry := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name:           llmName,
		Ref:            ref,
		Port:           port,
		Spec:           spec,
		RunMode:        "separate-pod",
		AwaitingSecret: false,
		SidecarPodName: name + "-sidecar-" + ref,
		SidecarPodIP:   host,
	}
	// Replace any prior entry for this ref (idempotent re-stamp / pod replace),
	// else append.
	replaced := false
	for i := range sess.Status.ResolvedSidecarToolboxes {
		if sess.Status.ResolvedSidecarToolboxes[i].Ref == ref {
			sess.Status.ResolvedSidecarToolboxes[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		sess.Status.ResolvedSidecarToolboxes = append(sess.Status.ResolvedSidecarToolboxes, entry)
	}
	require.NoError(h.t, h.K8s.Status().Patch(ctx, &sess, client.MergeFrom(base)),
		"MarkSidecarReady: patch status.resolvedSidecarToolboxes for %s/%s", ns, name)
}

// splitHostPort parses a http://host:port URL into its host + numeric port.
// Fatals the test on a malformed URL — the caller passes h.MCP.URL(), which
// is always well-formed, so a failure here is a harness bug, not a test input.
func splitHostPort(t *testing.T, raw string) (host string, port int32) {
	t.Helper()
	u, err := neturl.Parse(raw)
	require.NoError(t, err, "MarkSidecarReady: parse dispatch URL %q", raw)
	p, err := strconv.Atoi(u.Port())
	require.NoError(t, err, "MarkSidecarReady: parse port from %q", raw)
	return u.Hostname(), int32(p)
}

// Start boots envtest + SpiceDB + MCP stub + embedded NATS, starts
// the controller manager with the centerdot-relevant reconcilers
// (AgentClass, AgentIdentity, MCPServer, Channel, Guardian,
// AgentSession with the InProcessRunnerFactory), then applies AgentDir
// if set. Registers a t.Cleanup chain that tears each subsystem down
// in reverse order. Channelsd lands in T9.
func Start(t *testing.T, opts Options) *Harness {
	t.Helper()
	applyDefaults(&opts)

	// Reset the fake-kind global driver registry up-front so a
	// previous iteration's outbounds / inbound queue / approval
	// prompts don't leak into this test. Production code never
	// touches this — the registry is test-only state. Doing it at
	// Start (not cleanup) keeps Start the single fence: a test that
	// inspects drivers AFTER Start gets a clean slate.
	fakekind.ResetAllDrivers()

	// SpiceDB: ONE container per test binary, shared by every harness in the
	// package; isolation comes from the per-test bearer token, which
	// serve-testing routes to its own datastore. This comment claimed "shared
	// container" from the harness's first commit while the call was
	// Endpoint(t) — one container started, health-probed and purged per
	// e2e.Start, 102 of them across the suite's 39 scenario packages, all on
	// the critical path because no test/e2e test calls t.Parallel.
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	spdbCli, err := spicedb.NewClient(endpoint, token, true)
	if err != nil {
		t.Fatalf("dial spicedb at %s: %v", endpoint, err)
	}
	t.Cleanup(func() { _ = spdbCli.Close() })

	// envtest apiserver (testenv.Start registers its own t.Cleanup).
	env := testenv.Start(t)

	// Embedded NATS + a single client connection used by the harness's
	// outbound relay, the runner factory's respond_to_user publishing,
	// and any future tests that want to subscribe to envelope subjects.
	natsSrv := natstest.RunServer(&natsserver.Options{
		Host:   "127.0.0.1",
		Port:   -1,
		NoLog:  true,
		NoSigs: true,
	})
	t.Cleanup(natsSrv.Shutdown)

	nc, err := apnats.Connect(apnats.Options{
		URL:  natsSrv.ClientURL(),
		Name: "e2e-harness",
	})
	if err != nil {
		t.Fatalf("dial embedded NATS at %s: %v", natsSrv.ClientURL(), err)
	}
	t.Cleanup(func() {
		if err := nc.Drain(); err != nil {
			// Drain returns once outstanding callbacks finish; an error
			// here is rare in-test (e.g., conn already closed) but
			// worth surfacing so a leaked subscription doesn't hide.
			t.Logf("nats drain on cleanup: %v", err)
		}
	})

	// Shared in-process memory facade: the channelsd pipeline writes
	// inbound user turns into it, the runner reads/writes via the
	// turn.Appender wired in InProcessRunnerFactory, and the
	// AgentSession controller uses it for finalizer scope cleanup.
	//
	// The backend is a named variable because the search provider reads it
	// directly: searchinmem answers the STRUCTURED half of a search out of
	// Backend.Query, so provider and facade must be over the SAME backend or a
	// search would span a store nothing ever wrote to.
	memBackend := memoryinmem.NewBackend()
	// Search, wired with the same options internal/cmd/operator/main.go
	// assembles before its own NewLocal: the inmem provider, a
	// CompositeSearcher over it, and BOTH memory options — WithSearchProviders
	// so the facade can index/reindex, WithSearcher so Local.Search has
	// something to delegate to. Without the second, Search returns
	// ErrNoSearchProviders and the memory capability withholds search_memory
	// (see InProcessRunnerFactory's SearchAvailable).
	//
	// WithAuthorizer is NOT optional here. CompositeSearcher post-filters its
	// ranking through the Authorizer, and a searcher built without one returns
	// every entry in every scope it was handed — so a scenario about which
	// entries a search may reach would pass with authorization deleted. It is
	// wired from the same *spicedb.Client every controller in this harness
	// gets, constructed above, so the filter runs against the real service and
	// the real schema (memory_entry#read = session->read_transcript).
	//
	// The filter is caller-gated inside CompositeSearcher — no
	// memory.CallerFrom(ctx), no per-entry check — which mirrors production
	// exactly: the AgentSession reconciler registers a runner's memory token
	// with an EMPTY callerID, so an agent's own search_memory carries no caller
	// on either path. What gates an agent's search is the per-scope approval
	// door, not this filter; the filter is what gates a USER-attributed read
	// (webd, `oap memory search`), and TestHarnessSearcherFiltersByAuthorization
	// is what keeps it honest here.
	searchProvider := searchinmem.New(memBackend)
	searcher := memsearch.New(
		memsearch.WithProviders(searchProvider),
		memsearch.WithLogger(logr.FromSlogHandler(slog.Default().Handler()).WithName("e2e-search")),
		memsearch.WithAuthorizer(spicedbauthorizer.New(spdbCli)),
	)
	memStore := pkgmemory.NewLocal(memBackend,
		pkgmemory.WithSearchProviders(searchProvider),
		pkgmemory.WithSearcher(searcher),
	)

	workshopMCP, workshopMCPServer := mountWorkshopMCP(t, env.Client, opts.Namespace)
	h := &Harness{
		t:                 t,
		opts:              opts,
		LLM:               NewScriptedLLM(t),
		MCP:               NewMCPStub(t),
		SpiceDB:           spdbCli,
		K8s:               env.Client,
		Scheme:            env.Scheme,
		NATSURL:           natsSrv.ClientURL(),
		nc:                nc,
		memStore:          memStore,
		slackFake:         fakeslack.New(),
		slackSource:       fakeslack.NewSocketSource(),
		workshopMCP:       workshopMCP,
		workshopMCPServer: workshopMCPServer,
	}

	// Install the shared fake Slack transport process-wide BEFORE the
	// manager (and its Channel-listener starter) spins up. This is
	// unconditional — installed for every harness, not just tests with a
	// kind: slack Channel — because the override is inert until the
	// real slack.Kind's listener/sender factories are actually invoked,
	// which only happens when resolve.ForChannel/ForSession resolves a
	// Channel whose spec.kind == "slack" (see slack/testhooks.go). A
	// scenario with only kind: fake Channels never touches these globals.
	t.Cleanup(slack.InstallTestTransport(h.slackFake, h.slackSource))
	// Closes the fake's lazily-started file-download server (fakeslack.Close's
	// doc) — a no-op for the overwhelming majority of scenarios that never
	// call SlackFake().SeedFile.
	t.Cleanup(h.slackFake.Close)

	// Build + start the controller manager BEFORE applying the
	// AgentDir so that the moment CRs land, controllers are watching
	// and Reconcile gets called. Otherwise the first reconcile is
	// gated on the controller's resync interval (minutes) rather than
	// the watch-driven re-enqueue (sub-second).
	h.startManager(t, env, spdbCli)

	if opts.AgentDir != "" {
		h.applyAgentDir(opts.AgentDir)
	}
	for i, m := range opts.ExtraManifests {
		h.applyManifestLabeled(m, fmt.Sprintf("ExtraManifests[%d]", i))
	}

	return h
}

// startManager constructs the controller-runtime manager, registers
// the controllers the centerdot fixture transitively needs to reach
// AgentClass Valid=True, and spawns mgr.Start in a goroutine with a
// cleanup-bound cancel. Blocks until the cache is synced — Apply calls
// downstream would otherwise race the informer.
//
// Scope (intentional): T8 ships only the controllers centerdot needs.
// SpiceboxClass/Session/Toolkit/Toolspec, ToolCall, and ArtifactRender
// are skipped — the in-process runner factory doesn't dispatch sandbox
// tools (see InProcessRunnerFactory godoc), and the centerdot
// AgentClass has no toolBundles, so the bundle-resolution path in the
// AgentSession controller short-circuits with NoBundles. Adding those
// controllers when a future fixture requires them is the right time
// to widen this set.
func (h *Harness) startManager(t *testing.T, env *testenv.Env, spdbCli *spicedb.Client) {
	t.Helper()

	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:  env.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"}, // disable metrics endpoint
		// SkipNameValidation is mandatory in test binaries that build a
		// fresh manager per test: controller-runtime's process-wide
		// controller-name registry would otherwise reject the second
		// test's SetupWithManager with "controller with name X already
		// exists." Mirrors pkg/controllers/spiceboxclass tests.
		Controller: ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("e2e: ctrl.NewManager: %v", err)
	}

	// adoptguard readers: several reconcilers require a SecretReader /
	// ConfigMapReader (the operator adopts a referenced Secret/ConfigMap before
	// reading it). The e2e harness exercises functional behavior, not the panic
	// tripwire, so use Warn mode with no fixed-infra allowlist — matching the
	// controllers' unit-test wiring. Without these the reconcilers nil-deref on
	// the reader and silently fail to set conditions, stalling the AgentClass.
	noAllowlist := func(types.NamespacedName) bool { return false }
	secretReader := adoptguard.NewSecretReader(mgr.GetClient(), mgr.GetAPIReader(), adoptguard.Warn, noAllowlist)
	configMapReader := adoptguard.NewConfigMapReader(mgr.GetClient(), mgr.GetAPIReader(), adoptguard.Warn, noAllowlist)
	h.secretReader = secretReader
	h.configMapReader = configMapReader

	// AgentClass — AllowTestProvider=true admits the centerdot
	// fixture's model.provider="test"; production refuses it.
	//
	// MaxDelegationDepth mirrors internal/cmd/operator/main.go's literal 3 —
	// the zero value rejects EVERY spec.subagents roster (even depth 1) as
	// "exceeds the maximum delegation depth of 0", which is invisible until a
	// fixture actually declares one (the subagent-delegation bundle is the
	// first).
	if err := (&agentclassctrl.Reconciler{
		Client:             mgr.GetClient(),
		APIReader:          mgr.GetAPIReader(),
		SpiceDBSchema:      spdbCli,
		AllowTestProvider:  true,
		SecretReader:       secretReader,
		ConfigMapReader:    configMapReader,
		MaxDelegationDepth: 3,
		StarterLinker:      spdbCli,
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("e2e: register AgentClass controller: %v", err)
	}

	// AgentIdentity — resolves the centerdot static credential binding.
	if err := (&agentidentityctrl.Reconciler{
		Client:       mgr.GetClient(),
		APIReader:    mgr.GetAPIReader(),
		SecretReader: secretReader,
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("e2e: register AgentIdentity controller: %v", err)
	}

	// RelationshipSource — a nil dependency here would silently disable the
	// feature under this harness while every unit test stays green (the
	// recurring "second wiring site" defect this repo has hit before: see
	// AGENTS.md's own note on the e2e harness building its own wiring).
	// spdbCli is required-non-nil here (constructed above; see the Guardian
	// wiring's own comment on the same client for why).
	if err := relationshipsourcectrl.NewReconciler(mgr.GetClient(), secretReader, spdbCli).
		SetupWithManager(mgr); err != nil {
		t.Fatalf("e2e: register RelationshipSource controller: %v", err)
	}

	// MCPServer — validates the centerdot MCPServer's tool/permission
	// shape so the AgentClass binding-coverage check can resolve. The
	// 1s revalidate interval (vs production default 5m) keeps the
	// allowlist-drift recovery window short enough that a test which
	// seeds the MCPStub *after* Start returns can still see Valid=True
	// before the WaitForAgentClassValid deadline elapses.
	if err := (&mcpserverctrl.Reconciler{
		Client:             mgr.GetClient(),
		SecretReader:       secretReader,
		RevalidateInterval: 1 * time.Second,
		// The e2e MCPStub is an httptest server bound to 127.0.0.1, which
		// the production SSRF-guarded probe client (correctly) refuses.
		// Inject a plain client so the controller can probe the loopback
		// stub; production omits HTTP and gets the guarded default.
		HTTP: http.DefaultClient,
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("e2e: register MCPServer controller: %v", err)
	}

	// Channel — reconciles the centerdot kind=fake Channel so the
	// AgentClass's BoundChannels list populates.
	if err := (&channelctrl.Reconciler{Client: mgr.GetClient(), SecretReader: secretReader}).
		SetupWithManager(mgr); err != nil {
		t.Fatalf("e2e: register Channel controller: %v", err)
	}

	// Skill — sets Valid/Pinned on any Skill a fixture ships. The AgentClass
	// reconciler parks at Valid=False until every spec.skills[].ref resolves to
	// a Skill that is itself Valid=True, so without this a fixture carrying
	// skills can never boot.
	//
	// The REAL controller, not a driver-side stamp, and that is the whole
	// point. The stamp this harness still performs (toolspecs) stands in for a
	// controller that reaches outside the cluster — resolving a digest — which
	// envtest has no way to do. This one reaches nowhere: it is a pure
	// spec-to-status reconcile whose only reads are the object itself and,
	// through pkg/tools/skills/materialize, the SkillSource its owner-ref
	// names. Registering it means a captured fixture's Skill has to SATISFY
	// the provenance gate (a controller owner-ref to a same-namespace
	// SkillSource whose repoURL matches the canonical name's authority) rather
	// than have a verdict asserted over it, which is exactly the check a stamp
	// would have skipped.
	//
	// A no-op for every bundle that ships no Skill, which is all 39 bronze
	// bundles.
	if err := (&skillctrl.Reconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		t.Fatalf("e2e: register Skill controller: %v", err)
	}

	// SidecarToolbox — sets Valid on any SidecarToolbox a fixture ships, the
	// same role Skill plays above for Skills. The AgentClass reconciler parks
	// at Valid=False until every spec.sidecarToolboxes[].ref resolves to a
	// SidecarToolbox that is itself Valid=True, so without this a fixture
	// carrying a sidecar toolbox can never boot.
	//
	// The REAL controller — replacing threadrun's driver-side
	// stampSidecarToolboxesValid, which stamped Valid=True directly with a
	// comment admitting this controller was not wired. SkipProbe:true takes
	// the same probeGate→finalize path production takes for a secret-gated
	// (separate-pod) sidecar, whose probe/pin phases are always skipped
	// because reachability is deferred to the runner — but here it applies to
	// EVERY toolbox, in-pod ones included, because envtest has no pod runtime
	// to run a real probe Pod against regardless of run mode. A no-op for
	// every bundle that ships no SidecarToolbox.
	if err := (&sidecartoolboxctrl.Reconciler{
		Client:          mgr.GetClient(),
		ConfigMapReader: configMapReader,
		SkipProbe:       true,
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("e2e: register SidecarToolbox controller: %v", err)
	}

	// Guardian — composes MCPServer.spec.spiceDBSchema fragments and
	// SpiceDBBootstrap CRs into the live SpiceDB schema. The
	// AgentClass schema-validation step REQUIRES this to have run at
	// least once before it can resolve permissions against the live
	// schema — without it, the AgentClass stalls on
	// PermissionSchemaMismatch.
	//
	// SchemaIOFor + spdbCli are required-non-nil here (per AGENTS.md
	// "Nil interfaces": spdbCli is a concrete *spicedb.Client created
	// above and never nil; SchemaIOFor wraps it in a value-type
	// adapter, so the interface assignment is a genuine non-nil).
	var schemaIO guardianschema.SchemaIO = spicedb.SchemaIOFor(spdbCli)
	if h.opts.SchemaWriteDelay > 0 {
		schemaIO = delayedSchemaIO{inner: schemaIO, delay: h.opts.SchemaWriteDelay}
	}
	guardianReconciler := guardianctrl.NewReconciler(mgr.GetClient(), schemaIO, spdbCli.Writer(spicedb.BootstrapSource))
	// Reader wires SpiceDBBootstrap drift detection's read-back. spdbCli is
	// the same required-non-nil *spicedb.Client as above — assigned directly
	// into the Reader interface field (never through a possibly-nil
	// intermediate variable), so this is a genuine non-nil interface, not
	// the typed-nil trap AGENTS.md documents against this exact file.
	guardianReconciler.Reader = spdbCli
	if err := guardianReconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("e2e: register Guardian controller: %v", err)
	}

	// Construct the manager-context up front so installToolCallController
	// (which wires the bufconn gateway server's t.Cleanup to it) and the
	// AgentSession factory both see the same cancellation lifecycle.
	mgrCtx, mgrCancel := context.WithCancel(context.Background())

	// Make the manager visible before installToolCallController runs so
	// the latter's `h.mgr == nil` guard passes. Earlier visibility is
	// safe — h.mgr is only read by the tool-call wiring path.
	h.mgr = mgr

	// Install the ToolCall controller + gateway bufconn BEFORE
	// constructing the InProcessRunnerFactory so the factory captures a
	// non-nil BridgeDialOpt. The dialer is populated as a side-effect of
	// installToolCallController; constructing the factory first would
	// leave BridgeDialOpt nil and the runner's interactive-tool dispatch
	// would attempt a real TCP dial.
	if h.opts.WithToolCallController {
		h.installToolCallController(t, mgrCtx)
	}

	// AgentSession with the in-process runner factory. The other
	// fields mirror the operator's wiring with in-process equivalents:
	//   - Tokens: a fresh per-test registry (no channelsd token set;
	//     channelsd lands in T9, and the AgentSession reconciler
	//     tolerates an empty registry).
	//   - Memory: in-memory facade, isolated per-test.
	//   - SpiceDBDeleter: the same client used for schema/relationship
	//     writes — relationships seeded by the test get cleaned up on
	//     session finalization.
	//   - DefaultChannelArchiveAfter: 4h matches the operator default;
	//     channel-attached sessions in tests finish well inside this.
	//   - BridgeDialOpt: harness's bufconn dialer when
	//     WithToolCallController is true (otherwise nil — harmless,
	//     the runner's bridge code skips appending nil opts).
	// Shared tokens registry: the AgentSession reconciler registers the
	// minted per-session memory bearer tokens here, and the in-process
	// runner factory registers each session's audit signing public key in
	// the same registry — so a verify-on-write facade (Task 8) finds the key
	// the runner signs with.
	tokensReg := tokens.NewRegistry()
	h.tokensReg = tokensReg

	// Operator-signing lifecycle facade. The operator's sequencer (applyEvent /
	// foldLifecycle) reads AND writes the per-session signed transition log to
	// derive fold-only phases — notably AwaitingIdentityChoice, which the runner
	// only appends (IdentityChoicePending) and never writes to status directly, so
	// the operator MUST re-read the log to project it (see identity_choice_test.go's
	// harness note + reconcileIdentityChoice). Mirrors internal/cmd/operator/main.go's
	// opSigned = NewSigningMemory(memLocal, opSigner) with publisher
	// "system:operator", over the SAME shared h.memStore the runner factory signs
	// into as session:<ns/name> — so the operator's fold sees the runner's events.
	// h.memStore is a plain Local (no verify-on-write), so signing is not strictly
	// required here, but wrapping keeps the harness production-faithful and the
	// operator's own append-only writes carry a valid provenance chain. A fixed
	// 0x11 seed keeps it deterministic; the pubkey is registered so a future
	// verify-on-write facade would accept these writes.
	opSeed := make([]byte, ed25519.SeedSize)
	for i := range opSeed {
		opSeed[i] = 0x11
	}
	opPriv := ed25519.NewKeyFromSeed(opSeed)
	opPub := opPriv.Public().(ed25519.PublicKey)
	tokensReg.SetPublisherKey("system:operator", provenance.KeyID(opPub), opPub)
	opSigner := provenance.NewSigner(opPriv, "system:operator")
	opSigned := provenance.NewSigningMemory(h.memStore, opSigner)
	h.opSigned = opSigned

	// Restart-marker signing key, standing in for the one channelsd mints and
	// registers at startup. The operator refuses a status.pendingRestart marker
	// it cannot attribute to channelsd, so the harness must register the public
	// half (below, as the Reconciler's PublisherKeys) and hand the private half
	// to the pipeline — otherwise every continuation/takeover in every scenario
	// is denied. Fixed 0x22 seed for determinism, mirroring the operator key.
	chSeed := make([]byte, ed25519.SeedSize)
	for i := range chSeed {
		chSeed[i] = 0x22
	}
	chPriv := ed25519.NewKeyFromSeed(chSeed)
	chPub := chPriv.Public().(ed25519.PublicKey)
	tokensReg.SetPublisherKey(restartmarker.Publisher, provenance.KeyID(chPub), chPub)
	h.markerSigner = restartmarker.NewSigner(chPriv, restartmarker.Publisher)
	h.goalActorSigned = provenance.NewSigningMemory(h.memStore, provenance.NewSigner(chPriv, "system:channelsd"))

	// Token-use authorization (externaltoken): opt-in via Options.WithTokenAuthz
	// (see its doc). Declared as the interface types — not the *spicedb.Client
	// pointer — and only assigned when opted in, so a WithTokenAuthz=false test
	// gets a genuine nil interface (per AGENTS.md "Nil interfaces: never assign
	// a typed-nil pointer directly") rather than a non-nil interface wrapping a
	// pointer that happens to be unused.
	var tokenGranter agentsessionctrl.TokenGranter
	var tokenChecker agentsessionctrl.TokenChecker
	if h.opts.WithTokenAuthz {
		tokenGranter = spdbCli
		tokenChecker = spdbCli
	}

	h.runnerFactory = &InProcessRunnerFactory{
		LLM:       h.LLM,
		K8s:       mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		DirectK8s: env.Client,
		NATS:      h.nc,
		MemStore:  h.memStore,
		SpiceDB:   spdbCli,
		// The operator's own signing facade — see ensurePrefsServer's doc for
		// why the preferences server needs this SAME writer (not h.memStore
		// directly) to audit a ?user-ref= read: preference_access is
		// ComponentWritten AND append-only, so an unsigned Put would be
		// refused the instant the facade's verifier saw it.
		OpSigned:      opSigned,
		BridgeDialOpt: h.gatewayDialer,
		Tokens:        tokensReg,
		AwaitIdleTTL:  h.opts.AwaitIdleTTL,
		TokenAuthz:    h.opts.WithTokenAuthz,
		// The store the ToolCall controller writes stdout/stderr into, so a
		// sandbox tool can read them back when it composes its result. nil
		// unless WithToolCallController — the only mode that has a controller
		// writing them — and the sandbox tool is nil-tolerant.
		ToolCallArtifacts: h.toolCallStore,
		// nil unless a replay pinned the ids the run must mint; see the
		// Options fields.
		NewOperationID: h.opts.NewOperationID,
		NewArtifactID:  h.opts.NewArtifactID,
		NewRenderName:  h.opts.NewRenderName,
		NewRevisionID:  h.opts.NewRevisionID,
		// nil/empty unless a replay pinned the tool catalog its captured run
		// recorded; see the Options fields.
		HoldToolsFromAssembly: h.opts.HoldToolsFromAssembly,
		FilterOfferedTools:    h.opts.FilterOfferedTools,
		ReplaceAssembledTool:  h.opts.ReplaceAssembledTool,
	}
	var goalValidator agentsessionctrl.GoalSessionValidator
	if h.opts.WithGoalExecution {
		h.goals = h.startGoalsRuntime(t, mgr, opSigner)
		goalValidator = h.goals.dispatcher
	}
	t.Cleanup(h.runnerFactory.Shutdown)
	if err := (&agentsessionctrl.Reconciler{
		Client:          mgr.GetClient(),
		APIReader:       mgr.GetAPIReader(),
		SecretReader:    secretReader,
		ConfigMapReader: configMapReader,
		Tokens:          tokensReg,
		Memory:          h.memStore,
		// TokenGranter/TokenChecker: nil unless Options.WithTokenAuthz — see the
		// var block above. Mirrors internal/cmd/operator/main.go's
		// TokenGranter: spiceDBClient, TokenChecker: spiceDBClient wiring.
		TokenGranter: tokenGranter,
		TokenChecker: tokenChecker,
		// LifecycleMemory: the operator's signing facade over the shared store
		// (see opSigned above). Enables the fold-based phase projection the
		// identity-choice flow depends on (AwaitingIdentityChoice) and that
		// production always wires; without it foldLifecycle returns the bootstrap
		// Pending and the runner-appended IdentityChoicePending is never projected.
		//
		// DELIBERATELY UNIVERSAL (every session), while the RUNNER's LifecycleMemory
		// and placeholder Pod are gated to identity-choice sessions only (see
		// inprocess_runner_factory.go's identityGatePending gating). That asymmetry
		// is intentional: for a non-identity session the operator folds a log the
		// runner never appends to — a permanently incomplete log — which reproduces
		// production's transient operator/runner eventual-consistency window and
		// keeps sequencer.go's reconcilePhase incomplete-log anti-demotion guard
		// under test. Do NOT make this symmetric to "match" the runner gating:
		// complete logs re-mask that demotion bug AND reintroduce the centerdot
		// timing regression the runner-side gating fixed.
		LifecycleMemory: opSigned,
		// AuditKeyMemory: the same operator signing facade, mirroring
		// internal/cmd/operator — the durable witness of each session's audit key.
		AuditKeyMemory:             opSigned,
		RunnerFactory:              h.runnerFactory,
		GoalValidator:              goalValidator,
		DefaultChannelArchiveAfter: 4 * time.Hour,
		SpiceDBDeleter:             spdbCli,
		WorkspaceStorageClass:      h.opts.WorkspaceStorageClass,
		WorkspaceSize:              "1Gi", // small fixed default for tests
		// Plan-2 restart-from-here deps. RestartMemory shares the same
		// facade as Memory; AuthzGranter is the SpiceDB client (it
		// satisfies authz.Granter via TouchStartedBy/TouchInteractParticipant);
		// Snapshotter uses the no-op test impl (the E2E suite simulates
		// the IMPACTFUL path by manually populating snapshot audit
		// entries when needed).
		// BundleStore backs skill-bundle staging. Wired EMPTY on purpose: a
		// fixture never ships a bundle archive (steelthread.rewriteSkills drops
		// the ref, because the bytes live in the operator's store and in no
		// record a capture can read), so every Get here misses and the skill
		// stages instruction-only — which is the same degradation production
		// applies to an uncached bundle.
		//
		// It is wired rather than left nil because the field is an INTERFACE
		// the staging path calls unconditionally once a Skill carries
		// spec.bundle. Nil there is not "no store", it is a nil-interface
		// method call — a panic controller-runtime recovers and requeues
		// forever, which is the silent crash-loop AGENTS.md's nil-interface
		// rule exists to prevent.
		BundleStore:   skillbundlememory.New(),
		RestartMemory: h.memStore,
		AuthzGranter:  spdbCli,
		// OrgViewerSyncer levels the class's artifactVisibility opt-in, same
		// wiring as production (the client implements authz.ArtifactOrgViewerSyncer).
		OrgViewerSyncer: spdbCli,
		Snapshotter:     noopSnapshotter{},
		// PublisherKeys authenticates status.pendingRestart. tokensReg carries
		// the channelsd marker key registered above, so a marker the harness
		// pipeline signed verifies and a hand-written unsigned one does not —
		// the same fail-closed posture production has.
		PublisherKeys: tokensReg,
		// SessionFork gate (agentsession#fork = started_by): the same SpiceDB
		// client implements authz.ForkChecker (CheckFork). Required since
		// ReconcileRestart now gates fork materialization; without it the gate's
		// mandatory-dependency check errors and the child is never created.
		// ForkNoticePublish is left nil (the deny path is not exercised here; the
		// forker is the session's started_by, so the gate Allows).
		ForkChecker: spdbCli,
		// StartChecker: the same SpiceDB client answers agentclass#start_explicit /
		// #start_session. Wired here so the start gate is enforced in e2e exactly
		// as in production; StartRefusedNoticePublish is left nil (a refusal is
		// logged and dropped, not posted — no bundle asserts on the notice yet).
		StartChecker: spdbCli,
		// DeniedLister (ListDeniedUsers) copies the parent's denied blocklist
		// onto the child. Same SpiceDB client; required by ReconcileRestart's
		// mandatory-dependency check.
		DeniedLister:    spdbCli,
		SlotGrantCopier: spdbCli,
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("e2e: register AgentSession controller: %v", err)
	}

	// SubagentRequest — authorizes and performs delegation (roster/identity
	// checks, child creation, lineage tuples) AND propagates the child's
	// completion back onto the request (Task 12a) via a watch on
	// AgentSession, so it must run on this SAME manager, after AgentSession
	// above. Mirrors internal/cmd/operator/main.go's wiring: spdbCli
	// implements LineageWriter (TouchLineage), and MaxDelegationDepth matches
	// the AgentClass controller's own ceiling set above, so a roster
	// re-validated here is judged by the same bound it was admitted under.
	// DefaultMaxDelegatedAgents mirrors internal/cmd/operator/main.go's literal
	// 8 — the zero value leaves the pooled per-root total-agent ceiling OFF for
	// every tree whose settings fold resolves no explicit
	// BudgetConfig.MaxDelegatedAgents, which is invisible until a fixture
	// actually relies on the built-in fallback (a fixture setting its own
	// budget.maxDelegatedAgents is unaffected either way).
	if err := (&subagentrequestctrl.Reconciler{
		Client:                    mgr.GetClient(),
		Scheme:                    mgr.GetScheme(),
		Authz:                     spdbCli,
		MaxDelegationDepth:        3,
		DefaultMaxDelegatedAgents: 8,
		// Grader, publisher and source lookup mirror internal/cmd/operator.
		// Left unset, bindDataSlots takes its no-grader branch — attenuation
		// alone — so every slot binds without ever asking whether handing it
		// to THIS child discloses. A bundle written to prove a disclosure was
		// routed would pass with the routing switched off, which is the one
		// thing a harness must never do.
		Grader: func(ctx context.Context, child authz.SessionRef, tagID string) (handoff.Grade, string, error) {
			return handoff.GradeRequest(ctx, handoff.Deps{
				ChildAudience:       spdbCli.SessionReadTranscriptAudience,
				TagReaders:          spdbCli.TagReaders,
				TagCarriesUntrusted: spdbCli.TagCarriesUntrusted,
			}, child, tagID)
		},
		// Mirrors internal/cmd/operator. Left unset, a delegated child would get
		// no plan-gate root — and a bundle written to prove a plan-gated child
		// inherits its parent's ceiling would pass with the inheritance switched
		// off, the one thing a harness must never do. opSigned because
		// plan_gate_audit is append-only.
		DeriveChildPlanRoot: func(ctx context.Context, parent, child authz.SessionRef) error {
			return plangate.WriteChildRoot(ctx, opSigned,
				parent.Namespace+"/"+parent.Name, child.Namespace+"/"+child.Name)
		},
		PublishInteraction: func(_ context.Context, envNS, envName string, env channelevents.Envelope) error {
			if h.nc == nil {
				return fmt.Errorf("harness: no NATS; nobody can be asked")
			}
			body, merr := json.Marshal(env)
			if merr != nil {
				return fmt.Errorf("marshal disclosure envelope: %w", merr)
			}
			return h.nc.Publish(
				channelevents.SubjectIn(channelevents.SubjectPrefix(envNS, envName), env.Kind), body)
		},
		ResourceOwners: func(ctx context.Context, resourceType, resourceID string) ([]string, error) {
			return spdbCli.LookupSubjects(ctx, resourceType+":"+resourceID+"#owner")
		},
		TagSources: func(ctx context.Context, parent authz.SessionRef, tagID string) ([]string, error) {
			return pttag.SourcesOf(
				pkgmemory.WithSystemApproval(ctx, "data_slot_disclosure"),
				h.memStore,
				pkgmemory.Scope{Kind: "session", ID: parent.Namespace + "/" + parent.Name},
				tagID)
		},
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("e2e: register SubagentRequest controller: %v", err)
	}

	// Plan-gate denial-streak tripper (pkg/authz/plangate/hold): see
	// Options.PlanGateDenialStreakThreshold's doc for why this is opt-in
	// harness wiring rather than a bt.Bundle field. hold.Setup is PACKAGE
	// GLOBAL (the memory facade's plangate_hold Kind reads it via
	// NewScopeHooks, mirroring kg_ingestion's own wiring) so it is torn down
	// unconditionally here before deciding this harness's own state — an
	// earlier test's Client/Mem, bound to an already-stopped envtest, must
	// never answer this one's signals — and unconditionally on cleanup so a
	// later test in the same binary does not inherit it either.
	hold.Teardown()
	if h.opts.PlanGateDenialStreakThreshold > 0 {
		hold.Setup(hold.NewDenialStreak(hold.DenialStreakDeps{
			Threshold: h.opts.PlanGateDenialStreakThreshold,
			Mem:       h.memStore,
			Client:    mgr.GetClient(),
		}))
		t.Cleanup(hold.Teardown)
	}

	// Spawn the manager. Context is independent of t (t.Context isn't
	// available across all Go versions and we want explicit cancel
	// timing). cleanup cancels + waits up to 5s for graceful shutdown.
	mgrDone := make(chan struct{})
	h.mgrCancel = mgrCancel
	h.mgrDone = mgrDone
	go func() {
		defer close(mgrDone)
		if err := mgr.Start(mgrCtx); err != nil {
			// Post-cancel exit returns nil; any non-nil err is a real
			// runtime failure. Surface to stderr (the t.Log channel
			// may be closed by the time this races with cleanup).
			fmt.Fprintf(os.Stderr, "e2e: manager exited with err: %v\n", err)
		}
	}()
	// Reads h.mgrCancel/h.mgrDone at CLEANUP time, not the mgrCancel/mgrDone
	// locals captured now — RestartOperator repoints both fields at a new
	// manager's pair, and cleanup must tear down whichever manager is
	// current, not the one Start originally created.
	t.Cleanup(func() {
		h.mgrCancel()
		select {
		case <-h.mgrDone:
		case <-time.After(5 * time.Second):
			t.Errorf("e2e: manager did not shut down within 5s")
		}
	})

	// Block until the informer caches are populated. Without this,
	// the first Apply downstream can land before the AgentClass
	// informer is watching, and the first reconcile is delayed until
	// the resync. WaitForCacheSync blocks during context lifetime; we
	// give it a generous local timeout so a sync stall fails the test
	// with a clear message instead of hanging.
	syncCtx, syncCancel := context.WithTimeout(mgrCtx, 30*time.Second)
	defer syncCancel()
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		t.Fatalf("e2e: controller cache failed to sync within 30s")
	}

	// T9: wire the channelsd pipeline + outbound relay + Channel
	// listener starter. The pipeline turns InboundEvents from the fake
	// listener into AgentSession CRs (which the controller above picks
	// up and hands to the InProcessRunnerFactory); the relay turns
	// out.user_message envelopes the respond_to_user tool publishes
	// back into Sender.Send calls on the fake driver. Together they
	// close the loop: SendUserMessage → fake listener → pipeline →
	// session → runner → respond_to_user → NATS → relay → fake sender
	// → Driver.Sent → ExpectAgentReply.
	h.startChannelsdPlumbing(t, mgrCtx)
}

// RestartOperator simulates the operator PROCESS restarting: it stops the
// current controller-runtime manager and boots a genuinely new one — a fresh
// cache, a fresh AgentSession Reconciler struct, zero Go-level state carried
// over — against the SAME envtest API server. Only the AgentSession
// reconciler is re-registered; the fixture's other controllers (AgentClass,
// MCPServer, Guardian, Channel) already reconciled everything relevant
// before the restart and read no in-memory state of their own, so a test
// proving a property survives an operator restart does not need them
// running again to observe it.
//
// Deliberately REUSES h.SpiceDB, h.memStore, h.opSigned and h.runnerFactory
// rather than rebuilding them: in production those are separate, durable
// processes (SpiceDB, the memory backend) or a separate pod (the runner) that
// an operator restart does not touch. Only the manager + Reconciler — the
// operator's own process state — get discarded and rebuilt, which is exactly
// the property this exists to test: if something survives THIS, it lived in
// a CR, not in the old manager's memory.
func (h *Harness) RestartOperator(t *testing.T) {
	t.Helper()

	if h.mgr == nil {
		t.Fatalf("e2e: RestartOperator called before Start finished booting a manager")
	}

	// Stop the CURRENT manager and wait for its goroutine to exit before
	// starting a new one against the same API server.
	h.mgrCancel()
	select {
	case <-h.mgrDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("e2e: RestartOperator: old manager did not shut down within 5s")
	}

	mgr, err := ctrl.NewManager(h.mgr.GetConfig(), ctrl.Options{
		Scheme:  h.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		// SkipNameValidation: this test process already registered a
		// controller named "agentsession" once; a second manager in the
		// same binary needs the same escape hatch startManager's first
		// call used.
		Controller: ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("e2e: RestartOperator: ctrl.NewManager: %v", err)
	}

	var tokenGranter agentsessionctrl.TokenGranter
	var tokenChecker agentsessionctrl.TokenChecker
	if h.opts.WithTokenAuthz {
		tokenGranter = h.SpiceDB
		tokenChecker = h.SpiceDB
	}

	if err := (&agentsessionctrl.Reconciler{
		Client:                     mgr.GetClient(),
		APIReader:                  mgr.GetAPIReader(),
		SecretReader:               h.secretReader,
		ConfigMapReader:            h.configMapReader,
		Tokens:                     h.tokensReg,
		Memory:                     h.memStore,
		TokenGranter:               tokenGranter,
		TokenChecker:               tokenChecker,
		LifecycleMemory:            h.opSigned,
		AuditKeyMemory:             h.opSigned,
		RunnerFactory:              h.runnerFactory,
		DefaultChannelArchiveAfter: 4 * time.Hour,
		SpiceDBDeleter:             h.SpiceDB,
		WorkspaceStorageClass:      h.opts.WorkspaceStorageClass,
		WorkspaceSize:              "1Gi",
		BundleStore:                skillbundlememory.New(), // see startManager's note
		RestartMemory:              h.memStore,
		AuthzGranter:               h.SpiceDB,
		OrgViewerSyncer:            h.SpiceDB, // see startManager's note on OrgViewerSyncer
		Snapshotter:                noopSnapshotter{},
		PublisherKeys:              h.tokensReg,
		ForkChecker:                h.SpiceDB,
		// StartChecker: the start gate is enforced in e2e exactly as in
		// production; StartRefusedNoticePublish is left nil (logged and dropped).
		StartChecker:    h.SpiceDB,
		DeniedLister:    h.SpiceDB,
		SlotGrantCopier: h.SpiceDB,
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("e2e: RestartOperator: register AgentSession controller: %v", err)
	}

	mgrCtx, mgrCancel := context.WithCancel(context.Background())
	mgrDone := make(chan struct{})
	go func() {
		defer close(mgrDone)
		if err := mgr.Start(mgrCtx); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: restarted manager exited with err: %v\n", err)
		}
	}()
	t.Cleanup(func() {
		h.mgrCancel()
		select {
		case <-h.mgrDone:
		case <-time.After(5 * time.Second):
			t.Errorf("e2e: restarted manager did not shut down within 5s")
		}
	})

	syncCtx, syncCancel := context.WithTimeout(mgrCtx, 30*time.Second)
	defer syncCancel()
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		t.Fatalf("e2e: RestartOperator: controller cache failed to sync within 30s")
	}

	// Repoint LAST: the old manager's own t.Cleanup (registered by Start)
	// reads h.mgrCancel/h.mgrDone at cleanup time, so it tears down
	// whichever manager is current instead of double-cancelling this one.
	h.mgr = mgr
	h.mgrCancel = mgrCancel
	h.mgrDone = mgrDone
}

// startChannelsdPlumbing constructs the in-process channelsd equivalent:
// the inbound pipeline, the outbound relay, and a one-shot Channel-CR
// listener starter. Mirrors internal/cmd/channelsd/main.go's wiring with the
// production-only pieces (watchdog, permission_decision subs, bento
// adapter) stripped — what remains is the minimum the ping/pong
// conversation roundtrip needs. Test-scoped: every goroutine is bound to
// mgrCtx so the harness's startManager cleanup terminates them.
func (h *Harness) startChannelsdPlumbing(t *testing.T, mgrCtx context.Context) {
	t.Helper()

	// Capabilities thunk — used by the pipeline to populate
	// AgentSession.spec.inputChannel.Capabilities on session creation.
	caps := func(kind string) []string {
		k, ok := chregistry.Get(kind)
		if !ok {
			return nil
		}
		return k.Capabilities()
	}

	// Memory adapter: the pipeline's Memory interface is satisfied by a
	// thin adapter on the harness's in-process memory facade. Production
	// uses an HTTP-backed client (operator memory server); the harness
	// shortcuts straight to the *memory.Local both the runner and the
	// AgentSession finalizer share.
	mem := &pipelineMemAdapter{mem: h.memStore}
	// Retained so a scenario can call SetInboundAssetUploader after Start
	// returns (mirrors the SetMinter/SetContentInspectors seams below).
	h.memAdapter = mem

	// NATS publisher adapter: the pipeline's NATS interface is the
	// minimal Publish(subject, payload) shape; *nats.Conn satisfies it
	// directly via a one-line wrapper.
	pubAdapter := &natsPublisher{nc: h.nc}

	// Inbound pipeline: identity + authz + memory + NATS publish +
	// session correlation. SpiceDB writes flow through the real
	// *spicedb.Client (the same one wired into the AgentClass /
	// AgentSession controllers above), so a SpiceDB schema mismatch
	// surfaces here as a test failure rather than a silent deny.
	pl := pipeline.NewPipeline(h.K8s, h.SpiceDB, mem, pubAdapter, caps)
	pl.RecordGoalActor = func(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass, owner identity.CanonicalUserID) error {
		return goalactor.Record(ctx, h.goalActorSigned, pkgmemory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}, goalactor.Content{Owner: owner.String(), SessionUID: string(sess.UID), ClassUID: string(class.UID)})
	}

	// The durable memory facade, mirroring internal/cmd/channelsd/main.go's pl.Mem. It
	// backs BOTH the resource-owner decision recovery and the parked prompts a
	// session is blocked on: the outbound relay records them (notePendingPrompt)
	// and the pipeline re-surfaces them (resurfacePending). Wired here rather
	// than left nil so a scenario that parks and re-enters exercises the same
	// durable path production does. No longer only the D3 cross-restart
	// fallback (see the tool_approval bind below) — every JIT tool approval now
	// reads/writes session_scope through this on every call, cache-warm or
	// not, so it must be non-nil even though this harness never exercises the
	// cold-cache path.
	pl.Mem = sysApprovedMem{inner: h.goalActorSigned}
	pl.MarkerSigner = h.markerSigner

	// Pending-prompt re-surfacing (an outstanding "waiting for the user"
	// prompt gets re-rendered to a re-interacting user on their current
	// device) is backed by the shared memory facade — the outbound relay's
	// notePendingPrompt writes and the pipeline's resurfacePending reads both
	// go through pl.Mem (wired above) and the relay's own Mem (wired below on
	// the outbound.Relay), not an injected registry.

	// GrantWriter wires the tool_approval interaction handler's approve-path
	// SpiceDB write. Assigned as a concrete (not interface) per the
	// AGENTS.md "Nil interfaces" rule: NewGrantWriter returns nil if
	// the writer is nil, but h.SpiceDB.Writer(...) is always non-nil here
	// so this is a real assignment.
	if gw := spicedb.NewGrantWriter(h.SpiceDB.Writer(pipeline.GrantSource)); gw != nil {
		pl.GrantWriter = gw
	}

	// Per-scenario pipeline extension (e.g. Slice 2.5 α5's PortalAccess
	// triggerer). Runs before subscribers and the outbound relay so the
	// extender can wire its own NATS subscribers if it ever needs to,
	// and before the Channel listener starter so the first inbound that
	// matches a portal trigger phrase finds the triggerer already
	// attached. See Options.PipelineExtender.
	if h.opts.PipelineExtender != nil {
		h.opts.PipelineExtender(pl, ExtenderView{K8s: h.K8s})
	}

	// permission_decision (KindPermissionDecision) is retired: an
	// Approve/Deny click on a session-join prompt now arrives as a
	// category-generic interaction_decision (subscribed below), handled by
	// decidePermission — see pkg/channels/channelsd/pipeline/permission_interaction.go
	// and the ResetBindings + Bind calls below. Mirrors production
	// internal/cmd/channelsd/main.go, which dropped the equivalent subscription in
	// the same change.
	//
	// The typed tool_approval + info_leakage subscriptions are likewise retired:
	// the runner now publishes a generic interaction_request (category
	// tool_approval / info_leakage), so both flows route through the
	// interaction_request / _decision / _applied subscriptions below plus the
	// bound handlers. Mirrors production internal/cmd/channelsd/main.go.

	// Every inbound subscription below binds through inboundEnvelopeHandler /
	// respondingEnvelopeHandler (subject_authority.go) — the harness's
	// envelopeHandler / respondingHandler. They are not conveniences: each
	// subscription is the cluster-wide "ap.session.*.*.in.<kind>" wildcard, and
	// the pipeline handlers behind them route on env.Session, which is sound
	// ONLY because the wrapper cross-checked that claim against the
	// subject-authorized session first. Dispatching straight off env.Session
	// here — as the harness used to — let a publisher on one session's inbound
	// subject resolve another's parked approval, and left the production gate
	// with no e2e coverage at all. See test/e2e/scenarios/subject_authority.

	// interaction_request on IN: published by the runner's migrated approval
	// gates (content_inspection in C1) to park a generic interaction. The
	// category-generic park handler writes/dedups PendingInteractions and
	// re-emits on OUT for the fake "interaction" sub-channel sender to record.
	// Mirrors production internal/cmd/channelsd/main.go; replaces the deleted legacy
	// content_inspection_approval_request subscription (content_inspection
	// migrated onto the generic Interaction model in Slice C1).
	if _, err := h.nc.Subscribe(channelevents.SubjectIn(channelevents.AnySessionPrefix(), channelevents.KindInteractionRequest),
		inboundEnvelopeHandler("interaction_request", "HandleInteractionRequest", pl.HandleInteractionRequest),
	); err != nil {
		t.Fatalf("subscribe interaction_request: %v", err)
	}

	// resurface_request on IN: published by a VIEW of the session the moment it
	// attaches (a chat tab's websocket, the TUI's outbound relay), asking for
	// whatever the session is parked on to be re-delivered — a parked prompt
	// goes out exactly once and its publisher dedups, so a surface attaching
	// afterwards would otherwise never see it. Mirrors production
	// internal/cmd/channelsd/main.go: the harness IS channelsd for a scenario, and a
	// subscription missing here is precisely the kind of divergence that makes
	// a defect invisible in the mode everything is tested in.
	if _, err := h.nc.Subscribe(channelevents.SubjectIn(channelevents.AnySessionPrefix(), channelevents.KindResurfaceRequest),
		inboundEnvelopeHandler("resurface_request", "HandleResurfaceRequest", pl.HandleResurfaceRequest),
	); err != nil {
		t.Fatalf("subscribe resurface_request: %v", err)
	}

	// agent_message_send on IN: published by a SENDING session's
	// reply_to_subagent (a delegating parent answering the child that asked
	// it a question) on its OWN subject, naming the child in the payload.
	// Mirrors production internal/cmd/channelsd/main.go.
	//
	// Its mirror row, `agent_message` — the direction the `agent` kind's own
	// Sender publishes, child to parent — is NOT wired here, and was not
	// before this row existed either. A scenario that needs a child's message
	// to actually reach its parent has to add it; nothing publishes this new
	// kind except reply_to_subagent, so adding this row alone changes no
	// existing scenario.
	if _, err := h.nc.Subscribe(channelevents.SubjectIn(channelevents.AnySessionPrefix(), channelevents.KindAgentMessageSend),
		inboundEnvelopeHandler("agent_message_send", "HandleAgentMessageSend", pl.HandleAgentMessageSend),
	); err != nil {
		t.Fatalf("subscribe agent_message_send: %v", err)
	}

	// interaction_applied on IN: synthetic publishes from the runner's
	// gate-side timeout watcher (Outcome=expired). Clears the matching durable
	// PendingInteractions entry so the park record doesn't leak. Mirrors
	// production internal/cmd/channelsd/main.go; replaces the deleted legacy
	// content_inspection_approval_decision subscription (the harness's click
	// equivalent now publishes a generic interaction_decision, handled by the
	// interaction_decision subscription below).
	if _, err := h.nc.Subscribe(channelevents.SubjectIn(channelevents.AnySessionPrefix(), channelevents.KindInteractionApplied),
		inboundEnvelopeHandler("interaction_applied", "HandleInteractionApplied", pl.HandleInteractionApplied),
	); err != nil {
		t.Fatalf("subscribe interaction_applied: %v", err)
	}

	// Bind (or re-bind) both categories' decision handlers before the
	// interaction_decision subscription below can possibly fire.
	// channelinteractions' handler registry is a package-wide singleton
	// (pkg/channels/channelinteractions/decision.go) — Bind panics on a double-bind —
	// and startChannelsdPlumbing runs once per Harness, with many e2e test
	// functions in a package each booting their own Harness within the same
	// test binary process.
	//
	// A sync.Once guard used to solve the panic by binding only the FIRST
	// harness's handlers and leaving them in place for the rest of the
	// process — which is correct for IdentityChoiceDecisionHandler (a
	// stateless free function) but WRONG for BindPermissionHandler, which
	// binds a *Pipeline-scoped method value (p.decidePermission) that closes
	// over that harness's own K8s/NATS/Engine. Every subsequent test in the
	// same package would then run its permission_request decisions against
	// the FIRST harness's already-torn-down apiserver — surfacing as
	// `p.K8s.Get: connect: connection refused` deep inside an async NATS
	// callback, with the join/grant silently never applying (Task 13
	// diagnosed this after TestCenterdot_SessionInteract_JoinViaApproval and
	// the new interaction_roundtrips scenarios started intermittently timing
	// out — but ONLY when 2+ tests in the same package exercised
	// permission_request, and ALWAYS on the 2nd+ test, never the 1st or an
	// isolated run — the signature of a stale singleton, not flaky
	// contention). ResetBindings + a fresh Bind on every Harness closes it:
	// each test's own decision handler is what's live while that test runs.
	channelinteractions.ResetBindings()
	channelinteractions.Bind(categories.IdentityChoice, pipeline.IdentityChoiceDecisionHandler)
	pipeline.BindPermissionHandler(pl)
	pipeline.BindStartApprovalHandler(pl)
	// provider_error_retry + queued_messages decision handlers (Slice B). Mirror
	// production internal/cmd/channelsd/main.go: the interaction_decision path drives
	// decideProviderRetry (stamps the wake annotation → operator respawns the
	// parked runner) and decideQueuedInterrupt (fires interrupt_request +
	// returns Suppressed). Both are DecideParticipant categories re-checked
	// server-side by HandleInteractionDecision before the handler runs.
	pipeline.BindProviderRetryHandler(pl)
	pipeline.BindQueuedInterruptHandler(pl)
	// content_inspection decision handler (Slice C1). Mirror production
	// internal/cmd/channelsd/main.go: the interaction_decision pipe validates standing
	// (DecideApprovers — the session approve-set) before invoking the generic
	// approve/deny handler; the runner resumes its content-guard gate via the
	// interaction_applied bridge (subscribeFactoryInteractionApplied). No
	// channelsd-side grant is written — content_inspection's approve is a pure
	// allow/deny the runner acts on.
	channelinteractions.Bind(categories.ContentInspection, pipeline.ApprovalDecisionHandler)
	// Plan gate, mirroring cmd/channelsd. Without these an approve is published,
	// clicked, and DROPPED — the runner waits out the full approval timeout and
	// the scenario reads as a hang rather than as a missing binding.
	channelinteractions.Bind(categories.PlanPhase, pipeline.ApprovalDecisionHandler)
	channelinteractions.Bind(categories.PlanAmendment, pipeline.ApprovalDecisionHandler)
	// tool_approval + info_leakage decision handlers (Slice C2). Mirror production
	// internal/cmd/channelsd/main.go: tool_approval binds the SIDE-EFFECTING handler
	// (writes the grant tuple on approve — pl.GrantWriter is wired above); the pipe's
	// DecideResourceOwners policy gates the clicker as a resource #owner FIRST.
	// info_leakage binds the PURE handler — its grant is written runner-side via the
	// interaction_applied bridge. pl.Mem (wired above, alongside pl's
	// construction) is no longer only the D3 cross-restart fallback the
	// resource-owner recovery reads on a cold cache — the approve branch now
	// calls authz.BindApproved on every approval, which reads/writes
	// session_scope through Mem unconditionally, so a nil Mem here would fail
	// every tool approval in this harness, warm cache or not.
	pipeline.BindToolApprovalHandler(pl)
	// precondition_waiver: the side-effecting handler binds a slot grant on
	// approve (authz.BindApproved with PreconditionsWaived), the same as
	// channelsd's main.go binds it (main.go:1054). Without this binding a
	// waiver card's decision has no handler and the session hangs on the
	// prompt until the class timeout — every triggered precondition bundle
	// would read as a hang rather than a gate.
	pipeline.BindPreconditionWaiverHandler(pl)
	channelinteractions.Bind(categories.InfoLeakage, pipeline.ApprovalDecisionHandler)
	// data_slot_disclosure needs its REAL handler, not the pure one: the
	// consent it records on the SubagentRequest is what the operator re-grades
	// against, so binding ApprovalDecisionHandler here would resolve the card
	// and bind nothing, and every routed slot would still expire.
	pipeline.BindDataSlotDisclosureHandler(pl)
	// user_preference_confirm: the SIDE-EFFECTING preference-save decision
	// handler (commits the confirmed value on approve, via the real
	// pkg/memory/httpsrv preferences-commit route — see
	// InProcessRunnerFactory.preferencesChannelsdClient). Mirrors production
	// internal/cmd/channelsd/main.go's `pl.PreferenceCommitter = mem.client` +
	// `pipeline.BindPreferenceCommitHandler(pl)`. Guarded like pl.GrantWriter
	// above (assign only a non-nil concrete client) so a harness with no K8s
	// client leaves the category unbound rather than wiring a typed-nil
	// PreferenceCommitter into the interface field.
	if c := h.runnerFactory.preferencesChannelsdClient(); c != nil {
		pl.PreferenceCommitter = c
		pl.GoalConsentCommitter = c
		pipeline.BindGoalConsentHandler(pl)
		pipeline.BindPreferenceCommitHandler(pl)
	}

	// interaction_decision: published by a surface (the harness's click
	// equivalent — IdentityChoice.Choose today, more categories over time) when
	// a user picks an interaction action. Category-generic: the pipe validates
	// fail-closed per the category's DeciderPolicy (identity_choice is
	// DecideRequester — only the addressee may answer, per
	// channelinteractions/categories.go), invokes the bound decision handler,
	// and publishes Applied on both IN (runner resume, via internal/cmd/runner's
	// subscribeInteractionApplied — wired below in the in-process runner
	// factory) and OUT (surface ack, the fake "interaction" sub-channel
	// sender). Mirrors production internal/cmd/channelsd/main.go's interaction_decision
	// subscription. Replaces the deleted legacy identity_choice_decision /
	// HandleIdentityChoiceDecision path (identity_choice migrated onto this
	// generic model).
	if _, err := h.nc.Subscribe(channelevents.SubjectIn(channelevents.AnySessionPrefix(), channelevents.KindInteractionDecision),
		inboundEnvelopeHandler("interaction_decision", "HandleInteractionDecision", pl.HandleInteractionDecision),
	); err != nil {
		t.Fatalf("subscribe interaction_decision: %v", err)
	}

	// provider_error_retry no longer needs a bespoke wake-writer subscription:
	// the Retry click now arrives as a generic interaction_decision
	// (provider_error_retry) on the subscription above, and the bound
	// decideProviderRetry handler stamps the wake annotation itself (via
	// annotateWake) — the same wake the deleted e2eRetryWakeWriter used to
	// perform. See BindProviderRetryHandler above and SimulateRetryClick below.

	// view_message: request/reply counterpart to every handler above (which
	// are fire-and-forget pub/sub). builtin/local SubmitUserMessage has no
	// live Listener to publish to — it sends a NATS request on this subject
	// instead (channelkinds.RequestViewMessage, wired via Deps.NATSRequest)
	// and blocks on the reply for channelkinds.ViewMessageTimeout. The
	// harness stands in for channelsd (production wiring:
	// internal/cmd/channelsd/main.go's respondingHandler over pl.HandleViewMessage),
	// so it must answer here too or any e2e scenario driving a builtin/local
	// SubmitUserMessage would hang for the full timeout and fail opaquely
	// instead of exercising the pipeline.
	//
	// respondingEnvelopeHandler mirrors respondingHandler's always-reply
	// discipline: a decode failure, a REFUSED SUBJECT, a handler error and a
	// marshal failure each still produce a reply, logged via the harness's
	// fmt.Fprintf(os.Stderr, ...) idiom (h has no Logf method) so the failure
	// is visible without leaving the requester blocked for the full
	// ViewMessageTimeout.
	if _, err := h.nc.Subscribe(channelevents.SubjectIn(channelevents.AnySessionPrefix(), channelevents.KindViewMessage),
		respondingEnvelopeHandler("view_message", "HandleViewMessage", pl.HandleViewMessage),
	); err != nil {
		t.Fatalf("subscribe view_message: %v", err)
	}

	// Outbound relay: subscribes to ap.session.*.*.out.> and dispatches
	// each envelope to the per-Channel Sender via senderResolver.
	// Watchdog hooks left nil — the e2e tests assert on Driver.Sent
	// directly and don't depend on "agent appears stuck" warnings.
	sr := newE2ESenderResolver(h.K8s, h.nc)
	relay := &outbound.Relay{
		NC:      h.nc,
		K8s:     h.K8s,
		Senders: sr,
		// Same underlying store as pl.Mem above — the relay is the WRITER
		// (notePendingPrompt on every resurfaceable REQUEST it dispatches) and
		// the pipeline is the READER (resurfacePending). Mirrors
		// internal/cmd/channelsd/main.go:660.
		Mem: sysApprovedMem{inner: h.goalActorSigned},
		// Mirrors channelsd's wiring: send outcomes stamp the Channel
		// Deliverable condition, so scenarios exercise the same status
		// writes the live relay makes.
		RecordDeliverability: true,
	}
	if err := relay.Start(mgrCtx); err != nil {
		t.Fatalf("e2e: outbound relay start: %v", err)
	}
	t.Cleanup(func() {
		// Stop() drains the subscription; failure here is logged but
		// not fatal — the harness is on its way down regardless.
		if err := relay.Stop(context.Background()); err != nil {
			t.Logf("e2e: outbound relay stop: %v", err)
		}
	})

	// Channel-history responder: serves the runner's read_channel_history
	// tool over NATS the same way production channelsd does (see
	// internal/cmd/channelsd/main.go). Started here alongside the outbound relay so
	// an in-process runner's read_channel_history tool call round-trips
	// through the same responder + gating + kind resolution as production
	// — Resolve defaults to resolve.ForChannel (nil here means "use the
	// default"), and Authz is the harness's real *spicedb.Client, which
	// satisfies MemberChecker via LookupSubjectIncludes. mgrCtx.Done()
	// implicitly stops the subscription (nats.Conn.Drain on the harness's
	// t.Cleanup above already tears down every subscription on nc, so no
	// separate cleanup is needed here).
	chr := &historyresp.ChannelResponder{K8s: h.K8s, Authz: h.SpiceDB, Clock: clock.RealClock{}}
	if err := chr.Start(mgrCtx, h.nc); err != nil {
		t.Fatalf("e2e: channel-history responder start: %v", err)
	}

	// webhook_inbound: the channelsd side of the webd → channelsd
	// verified-webhook-delivery handoff (pkg/channels/channelevents/webhook.go).
	// Mirrors internal/cmd/channelsd/webhook_inbound.go's wiring exactly (that
	// file is package main and cannot be imported), routing a verified
	// delivery into the SAME pl.Deliver every other subscription here uses.
	// Wired unconditionally, like every subscription above — an idle
	// subscription costs nothing, and a scenario that never calls
	// ApplyGitHubChannel/PostWebhook never publishes on this subject. See
	// reviewbot_dedup.go (Task 6, reviewbot dedup e2e).
	if _, err := h.nc.Subscribe(channelevents.WebhookInboundSubject, webhookInboundNATSHandler(h.K8s, pl)); err != nil {
		t.Fatalf("subscribe webhook_inbound: %v", err)
	}

	// One-shot Channel CR listener starter: lists every Channel in the
	// harness namespace and spawns its kind-specific Listener. The
	// production channelManager polls every 5s to pick up newly-applied
	// Channels; the harness applies all Channels up-front in
	// applyAgentDir so a one-shot list at startup is sufficient. Tests
	// that need dynamic Channel CR adds/removes mid-test will need to
	// extend this.
	//
	// The listener-startup is wrapped in a goroutine because Channel CRs
	// haven't been applied yet at this point in startManager — they
	// land in applyAgentDir, which runs AFTER startManager returns.
	// The goroutine retries the List until at least one Channel exists
	// (with a generous timeout) so the first inbound dispatched via
	// SendUserMessage finds a running Listener to consume it.
	go h.startChannelListeners(mgrCtx, pl)
}

// startChannelListeners polls for Channel CRs until ctx is canceled,
// starting a kind-specific Listener for each new Channel. Listeners
// inherit ctx so they shut down with the rest of startManager's plumbing.
func (h *Harness) startChannelListeners(ctx context.Context, pl *pipeline.Pipeline) {
	started := map[string]bool{} // ns/name → already started
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()

	startNewChannels := func() {
		var channels spiceboxv1alpha1.ChannelList
		if err := h.K8s.List(ctx, &channels); err != nil {
			// During shutdown the rate limiter / apiserver may be
			// closed; suppress those expected-on-cancel errors and
			// surface only genuine list failures so a structural
			// problem (CRD missing, etc.) is visible rather than a
			// silent listener-never-started.
			if ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "e2e: channel listener starter: list channels: %v\n", err)
			}
			return
		}
		for i := range channels.Items {
			ch := &channels.Items[i]
			key := ch.Namespace + "/" + ch.Name
			if started[key] {
				continue
			}
			// resolve.ForChannel loads the Secret + looks up the
			// kind from the registry. If the Channel's referenced
			// Secret hasn't been applied yet (unlikely — examples/
			// apply Secrets before Channels), resolve returns an
			// error and we retry on the next tick.
			sec, k, err := resolve.ForChannel(ctx, h.K8s, ch)
			if err != nil {
				// Log once per missing channel so a chronic resolve
				// failure (bad credentialsRef) is visible.
				fmt.Fprintf(os.Stderr, "e2e: resolve channel %s: %v\n", key, err)
				continue
			}
			nc := h.nc
			deps := channelkinds.Deps{
				Channel:     ch,
				Secret:      sec,
				NATSPublish: func(subj string, p []byte) error { return nc.Publish(subj, p) },
				Inbound:     pl,
				K8sClient:   h.K8s,
				AuthzReader: h.SpiceDB,
			}
			listener := k.NewListener(deps)
			if err := listener.Start(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "e2e: start listener for %s: %v\n", key, err)
				continue
			}
			started[key] = true
		}
	}

	// Initial pass — picks up Channels that already exist before
	// applyAgentDir is called (unlikely on a fresh harness, but
	// harmless). Subsequent ticks pick up the post-applyAgentDir
	// Channels.
	startNewChannels()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			startNewChannels()
		}
	}
}

// pipelineMemAdapter satisfies channelsd/pipeline.Memory over the
// unified memory facade. It mirrors internal/cmd/channelsd/memory.go (which talks
// to the operator memory server over HTTP) but shortcuts straight to the
// in-process *memory.Local: each call builds a turn.Appender for the
// session scope. The only translation is between pipeline.MemTurn /
// pipeline.MemContent (no CreatedAt, no Usage) and memory.Turn /
// memory.ContentBlock (with both). Index handling follows the same
// "auto-assign on Index==0" rule the channelsd memoryClient uses.
// sysApprovedMem wraps a memory.Memory so every operation carries a system
// approval — the harness stand-in for the operator httpsrv's per-request
// capability mint, for in-process components (e.g. an artifacts.Service built
// over a raw *memory.Local in a test) that would otherwise hit the memory
// capability doors with a bare context.
type sysApprovedMem struct{ inner pkgmemory.Memory }

func (m sysApprovedMem) Put(ctx context.Context, e pkgmemory.Entry) (pkgmemory.Entry, error) {
	return m.inner.Put(pkgmemory.WithSystemApproval(ctx, "e2e"), e)
}
func (m sysApprovedMem) Query(ctx context.Context, q pkgmemory.Query) (pkgmemory.QueryResult, error) {
	return m.inner.Query(pkgmemory.WithSystemApproval(ctx, "e2e"), q)
}
func (m sysApprovedMem) Search(ctx context.Context, req pkgmemory.SearchRequest) (pkgmemory.MergedSearchResult, error) {
	return m.inner.Search(pkgmemory.WithSystemApproval(ctx, "e2e"), req)
}
func (m sysApprovedMem) SendSignal(ctx context.Context, sig pkgmemory.Signal) error {
	return m.inner.SendSignal(pkgmemory.WithSystemApproval(ctx, "e2e"), sig)
}

type pipelineMemAdapter struct {
	mem pkgmemory.Memory

	// uploadFn, when set (via Harness.SetInboundAssetUploader), backs
	// UploadInboundAsset for a scenario that needs real attachment
	// storage/extraction wired end to end. nil (the default) preserves the
	// original always-fails behavior — see ErrInboundAssetUploadNotWired.
	uploadFn func(ctx context.Context, ns, sess, mime, filename string, body io.Reader) (pipeline.InboundAssetResult, error)
}

// appenderFor builds a turn.Appender bound to the (ns, name) session
// scope — the same Kind-parameterized path the production channelsd
// memoryClient uses.
func (a *pipelineMemAdapter) appenderFor(ns, name string) *turn.Appender {
	return turn.NewAppender(a.mem, pkgmemory.Scope{Kind: "session", ID: ns + "/" + name})
}

func (a *pipelineMemAdapter) Append(ctx context.Context, ns, name string, t pipeline.MemTurn) error {
	// Harness shortcut to the shared *memory.Local bypasses the operator httpsrv
	// that mints capability approvals in production; mint a system approval so the
	// channelsd inbound writes clear the memory doors.
	ctx = pkgmemory.WithSystemApproval(ctx, "e2e-channelsd")
	idx := t.Index
	role := t.Role
	if idx == 0 {
		// Active-session reply path. Matches internal/cmd/channelsd/memory.go so
		// the in-process harness and the deployed channelsd stay
		// shape-equivalent: auto-assign max(Index)+1, and write under
		// the distinct "inbox" role. The runner is the sole writer of
		// "user"/"assistant" transcript turns and drains "inbox" turns
		// into real "user" turns at indices it controls (loop.drainInbox)
		// — so channelsd/the harness must NOT write "user"-role turns,
		// which would race the runner's tool_result turns.
		turns, err := a.appenderFor(ns, name).ReadAll(ctx)
		if err != nil {
			return fmt.Errorf("pipelineMemAdapter: ReadAll for index assign: %w", err)
		}
		max := -1
		for _, existing := range turns {
			if existing.Index > max {
				max = existing.Index
			}
		}
		idx = max + 1
		role = "inbox"
	}
	turn := pkgmemory.Turn{
		Index:     idx,
		Role:      role,
		Content:   convertPipelineContent(t.Content),
		CreatedAt: time.Now().UTC(),
		// Carry the per-turn Author through verbatim, mirroring the
		// production memoryClient (internal/cmd/channelsd/memory.go). Without this
		// the harness silently drops channelsd's per-turn authorship stamp
		// and the multiplayer-attribution assertion would test a harness
		// artifact, not the real inbox->user Author-preservation path.
		Author: t.Author,
	}
	return a.appenderFor(ns, name).Append(ctx, turn)
}

func (a *pipelineMemAdapter) ReadAll(ctx context.Context, ns, name string) ([]pipeline.MemTurn, error) {
	ctx = pkgmemory.WithSystemApproval(ctx, "e2e-channelsd")
	raw, err := a.appenderFor(ns, name).ReadAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]pipeline.MemTurn, len(raw))
	for i, t := range raw {
		out[i] = pipeline.MemTurn{
			Index:   t.Index,
			Role:    t.Role,
			Content: convertMemContent(t.Content),
		}
	}
	return out, nil
}

// RecordChannelMsgRef writes a channel_msg_ref memory entry mapping
// (kind, ref) → turnIndex at the (ns, name) session scope. Uses the
// in-process memory.Local directly (no HTTP hop) to stay
// shape-equivalent with the production memoryClient.
func (a *pipelineMemAdapter) RecordChannelMsgRef(ctx context.Context, ns, name, kind, ref string, turnIndex int) error {
	ctx = pkgmemory.WithSystemApproval(ctx, "e2e-channelsd")
	scope := pkgmemory.Scope{Kind: "session", ID: ns + "/" + name}
	if err := channel_msg_ref.Record(ctx, a.mem, scope, kind, ref, turnIndex); err != nil {
		return fmt.Errorf("pipelineMemAdapter: RecordChannelMsgRef: %w", err)
	}
	return nil
}

// RecordTriggerDelivery stores the signed webhook delivery that opened the
// (ns, name) session. Same in-process, no-HTTP-hop shape as
// RecordChannelMsgRef above, kept shape-equivalent with memoryClient.
func (a *pipelineMemAdapter) RecordTriggerDelivery(ctx context.Context, ns, name, kind, event, channelKey string, body []byte) error {
	ctx = pkgmemory.WithSystemApproval(ctx, "e2e-channelsd")
	scope := pkgmemory.Scope{Kind: "session", ID: ns + "/" + name}
	if err := triggerdelivery.Record(ctx, a.mem, scope, triggerdelivery.Content{
		Kind: kind, Event: event, ChannelKey: channelKey, Body: body,
	}); err != nil {
		return fmt.Errorf("pipelineMemAdapter: RecordTriggerDelivery: %w", err)
	}
	return nil
}

// RecordEnvelopeFacts writes one envelope_fact entry per (subject, fact)
// pair. Same in-process, no-HTTP-hop shape as RecordTriggerDelivery above,
// kept shape-equivalent with memoryClient.
func (a *pipelineMemAdapter) RecordEnvelopeFacts(ctx context.Context, ns, name, kind, event string, facts []channelkinds.TriggerFact) error {
	ctx = pkgmemory.WithSystemApproval(ctx, "e2e-channelsd")
	scope := pkgmemory.Scope{Kind: "session", ID: ns + "/" + name}
	for _, f := range facts {
		if err := envelopefact.Record(ctx, a.mem, scope, factcontent.Observation{
			Subjects: f.Subjects,
			Facts:    f.Facts,
			Source:   factcontent.Source{ChannelKind: kind, Event: event},
		}); err != nil {
			return fmt.Errorf("pipelineMemAdapter: RecordEnvelopeFacts: %w", err)
		}
	}
	return nil
}

// ErrInboundAssetUploadNotWired is returned by pipelineMemAdapter's
// UploadInboundAsset when no scenario has called
// Harness.SetInboundAssetUploader: the harness has no artifactstore
// reference at this call site by default (h.memStore is a bare
// pkgmemory.Memory, not wired to an artifactstore.Store), so real
// storage/extraction wiring belongs to whichever scenario needs it.
// Returning a clear error means every e2e attachment resolves through the
// pipeline's own transient-failure path (honest, logged, never a silent
// skip) rather than fabricating a fake success.
var ErrInboundAssetUploadNotWired = errors.New("pipelineMemAdapter: inbound-asset upload not wired in the e2e harness")

func (a *pipelineMemAdapter) UploadInboundAsset(ctx context.Context, ns, sess, mime, filename string, body io.Reader) (pipeline.InboundAssetResult, error) {
	if a.uploadFn != nil {
		return a.uploadFn(ctx, ns, sess, mime, filename, body)
	}
	if body != nil {
		_, _ = io.Copy(io.Discard, body)
	}
	return pipeline.InboundAssetResult{}, ErrInboundAssetUploadNotWired
}

func convertPipelineContent(in []pipeline.MemContent) []pkgmemory.ContentBlock {
	out := make([]pkgmemory.ContentBlock, len(in))
	for i, c := range in {
		block := pkgmemory.ContentBlock{Type: c.Type, Text: c.Text}
		if c.Type == "attachment" {
			block.Attachment = &pkgmemory.AttachmentBlock{
				Filename: c.Filename, MIME: c.MIME, SizeBytes: c.SizeBytes,
				Ref: c.Ref, TextRef: c.TextRef, Pages: c.Pages,
			}
		}
		out[i] = block
	}
	return out
}

func convertMemContent(in []pkgmemory.ContentBlock) []pipeline.MemContent {
	out := make([]pipeline.MemContent, len(in))
	for i, b := range in {
		c := pipeline.MemContent{Type: b.Type, Text: b.Text}
		if b.Attachment != nil {
			c.Filename = b.Attachment.Filename
			c.MIME = b.Attachment.MIME
			c.SizeBytes = b.Attachment.SizeBytes
			c.Ref = b.Attachment.Ref
			c.TextRef = b.Attachment.TextRef
			c.Pages = b.Attachment.Pages
		}
		out[i] = c
	}
	return out
}

// natsPublisher adapts *nats.Conn to channelsd/pipeline.NATS.
type natsPublisher struct{ nc *nats.Conn }

func (n *natsPublisher) Publish(subject string, payload []byte) error {
	return n.nc.Publish(subject, payload)
}

func applyDefaults(o *Options) {
	if o.Namespace == "" {
		o.Namespace = "default"
	}
	if o.DefaultUser == "" {
		o.DefaultUser = "user@example.com"
	}
	if o.DefaultTimeout == 0 {
		o.DefaultTimeout = 10 * time.Second
	}
}

// applyAgentDir reads every *.yaml file in dir (sorted), performs
// {{MCP_URL}} substitution, and applies each via ApplyManifest. Does
// NOT wait for any condition — that's the caller's job (use
// WaitForAgentClassValid for the Valid=True wait).
func (h *Harness) applyAgentDir(dir string) {
	h.t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		h.t.Fatalf("applyAgentDir: read %s: %v", dir, err)
	}
	// Sort by name so the 00-/01-/02- prefixes encode apply order.
	// os.ReadDir already returns entries sorted by filename.
	yamls := []string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		yamls = append(yamls, e.Name())
	}
	for _, name := range yamls {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			h.t.Fatalf("applyAgentDir: read %s: %v", path, err)
		}
		substituted := strings.ReplaceAll(string(raw), "{{MCP_URL}}", h.MCP.URL())
		h.applyManifestLabeled(substituted, path)
	}
}

// ApplyManifest decodes a multi-doc YAML blob and Create-or-Updates
// each object via the K8s client. Uses the project's scheme so any
// CR registered in pkg/apis/v1alpha1 round-trips correctly.
func (h *Harness) ApplyManifest(yamlBlob string) {
	h.t.Helper()
	h.applyManifestLabeled(yamlBlob, "<inline>")
}

// applyManifestLabeled is the internal worker that carries a source
// label (file path or "<inline>") into error messages so a failing
// apply tells you WHICH manifest blew up.
func (h *Harness) applyManifestLabeled(yamlBlob, source string) {
	h.t.Helper()
	decoder := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(yamlBlob), 4096)
	ctx := context.Background()
	for {
		// Decode into a raw map first to read TypeMeta, then re-decode
		// into a typed object via the scheme so we can Create/Update
		// through controller-runtime client.
		var raw map[string]any
		if err := decoder.Decode(&raw); err != nil {
			if err == io.EOF {
				return
			}
			h.t.Fatalf("ApplyManifest(%s): decode: %v", source, err)
		}
		if len(raw) == 0 {
			continue // empty doc separator
		}
		obj, gvk, err := h.decodeToTyped(raw)
		if err != nil {
			h.t.Fatalf("ApplyManifest(%s): decode typed: %v", source, err)
		}
		// Try Create; on AlreadyExists, fall back to Update. The Get +
		// Update is wrapped in RetryOnConflict: a controller may bump the
		// object's resourceVersion between our Get and Update (e.g. a
		// status write), which would otherwise fail the apply with a
		// spurious optimistic-concurrency conflict.
		if err := h.K8s.Create(ctx, obj); err != nil {
			upErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				existing := obj.DeepCopyObject().(client.Object)
				if getErr := h.K8s.Get(ctx, client.ObjectKeyFromObject(obj), existing); getErr != nil {
					return getErr
				}
				obj.SetResourceVersion(existing.GetResourceVersion())
				return h.K8s.Update(ctx, obj)
			})
			if upErr != nil {
				h.t.Fatalf("ApplyManifest(%s): %s/%s Create failed (%v) and Update failed: %v",
					source, gvk.Kind, obj.GetName(), err, upErr)
			}
		}
	}
}

// decodeToTyped converts a YAML doc (already parsed to map) into a
// typed client.Object via the harness's scheme. Errors if the
// apiVersion/kind isn't registered — both clientgoscheme (Secret,
// ConfigMap, ...) and spiceboxv1alpha1 (AgentClass, MCPServer, ...)
// are added to env.Scheme via testenv.Start, so anything the
// centerdot fixture references should resolve.
func (h *Harness) decodeToTyped(raw map[string]any) (client.Object, *metav1.GroupVersionKind, error) {
	apiVersion, _ := raw["apiVersion"].(string)
	kind, _ := raw["kind"].(string)
	if apiVersion == "" || kind == "" {
		return nil, nil, fmt.Errorf("missing apiVersion/kind in document")
	}
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, nil, fmt.Errorf("parse apiVersion %q: %w", apiVersion, err)
	}
	gvk := gv.WithKind(kind)

	obj, err := h.Scheme.New(gvk)
	if err != nil {
		return nil, nil, fmt.Errorf("scheme.New(%s): %w", gvk, err)
	}
	co, ok := obj.(client.Object)
	if !ok {
		return nil, nil, fmt.Errorf("%s does not implement client.Object", gvk)
	}

	// JSON-roundtrip the map → typed.
	jsonBytes, err := json.Marshal(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal raw %s: %w", gvk, err)
	}
	if err := json.Unmarshal(jsonBytes, co); err != nil {
		return nil, nil, fmt.Errorf("unmarshal into %s: %w", gvk, err)
	}
	out := &metav1.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind}
	return co, out, nil
}

// authzSchemaBarrier is how long WaitForAuthzSchema waits for the guardian's
// first compose→WriteSchema pass. Generous on purpose: the pass reads every
// AgentSessionGrants / MCPServer / SpiceDBBootstrap in the cluster, validates
// and partitions the fragments, then does a ReadSchema + WriteSchema round
// trip against a containerized SpiceDB. Under suite contention that is
// seconds, and absorbing it here is the entire point of the barrier.
const authzSchemaBarrier = 60 * time.Second

// authzSchemaProbe is the object/subject id WaitForAuthzSchema checks against.
// No scenario names a session or a user this, and the check is read-only, so
// the probe can never collide with or perturb a scenario's SpiceDB state.
const authzSchemaProbe = "__e2e_authz_schema_probe__"

// WaitForAuthzSchema blocks until the guardian has composed and written the
// cluster's SpiceDB schema — i.e. until `definition agentsession` is live.
//
// Required before ANYTHING creates an AgentSession, because creating one
// writes agentsession:<ns>/<name>#started_by (channelsd's TouchStartedBy) and
// SpiceDB answers FailedPrecondition "object definition `agentsession` not
// found" until the schema lands. channelsd records that as
// status.startFailure{reason: AuthzWriteFailed} and the operator drives the
// session to Failed — the test then dies several layers from the cause.
//
// No existing barrier implies this:
//
//   - AgentClass Valid=True does NOT. The AgentClass reconciler creates the
//     class's "<class>-grants" AgentSessionGrants CR and stamps Valid=True in
//     the SAME pass; the guardian's compose→WriteSchema is a downstream,
//     independent reconcile triggered BY that create. Measured on an idle
//     machine the schema lands ~45 ms after Valid=True — hidden behind
//     WaitForAgentClassValid's 250 ms poll on a quiet machine, and wide open
//     under contention. That is why WaitForAgentClassValid now takes this
//     barrier itself.
//   - WaitForSpiceDBBootstrap does not either: it returns immediately when the
//     fixture ships no SpiceDBBootstrap CR, which is most of testdata/.
//
// The probe is the failing operation's own predicate rather than a substring
// match on the schema text: a read-only CheckPermission against the
// `agentsession` definition, which errors until that definition exists. The
// permissionship it returns is irrelevant and deliberately ignored.
//
// Latches on success — a schema, once written, is never unwritten inside one
// harness — so repeated barriers cost one atomic load.
func (h *Harness) WaitForAuthzSchema(deadline time.Duration) {
	h.t.Helper()
	if h.authzSchemaLive.Load() {
		return
	}
	cutoff := time.Now().Add(deadline)
	var lastErr error
	for {
		if lastErr = h.probeAuthzSchema(); lastErr == nil {
			h.authzSchemaLive.Store(true)
			return
		}
		if !time.Now().Before(cutoff) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The guardian only composes when it has something to reconcile. If no
	// AgentSessionGrants exists, no MCPServer and no SpiceDBBootstrap was
	// applied either, nothing ever triggered a pass — say so, because the
	// bare SpiceDB error reads like a connectivity problem.
	var asgs spiceboxv1alpha1.AgentSessionGrantsList
	listErr := h.K8s.List(context.Background(), &asgs)
	h.t.Fatalf("WaitForAuthzSchema: the guardian did not write the composed SpiceDB schema within %s; "+
		"last probe error: %v (AgentSessionGrants in cluster: %d, list err: %v)",
		deadline, lastErr, len(asgs.Items), listErr)
}

// probeAuthzSchema asks SpiceDB the failing operation's own question: does the
// `agentsession` definition exist? A read-only CheckPermission against an
// object id no scenario uses, so it can neither collide with nor perturb a
// scenario's state. nil means the definition is live; the permissionship it
// returns is irrelevant and deliberately ignored.
//
// Probing beats substring-matching the schema text: this is exactly what
// TouchStartedBy does, so the barrier cannot drift away from what it fences.
func (h *Harness) probeAuthzSchema() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := h.SpiceDB.CheckPermission(ctx, &v1.CheckPermissionRequest{
		Resource: &v1.ObjectReference{
			ObjectType: "agentsession",
			ObjectId:   h.opts.Namespace + "/" + authzSchemaProbe,
		},
		Permission: "interact",
		Subject: &v1.SubjectReference{
			Object: &v1.ObjectReference{ObjectType: "user", ObjectId: authzSchemaProbe},
		},
		Consistency: &v1.Consistency{
			Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true},
		},
	})
	return err
}

// AuthzSchemaLive reports whether the guardian's composed schema is live RIGHT
// NOW, without waiting and without consulting WaitForAuthzSchema's latch. It
// is the assertable form of the barrier: a scenario proving that some other
// wait implies schema readiness checks this immediately after that wait
// returns.
func (h *Harness) AuthzSchemaLive() bool {
	h.t.Helper()
	return h.probeAuthzSchema() == nil
}

// WaitForSpiceDBBootstrap blocks until every SpiceDBBootstrap CR in the
// harness namespace reports RelationshipsApplied=True, or deadline
// elapses. Required before any test that depends on a bootstrap-written
// tuple (most centerdot scenarios): the guardian controller's debounce
// can delay the WriteRelationships RPC by up to 5s after the AgentClass
// converges, and a Check fired in that window sees the pre-bootstrap
// snapshot. AgentClass Valid=True does NOT imply the bootstrap finished
// — they're independent reconcile loops.
//
// Treats zero bootstraps in the namespace as a successful no-op (some
// fixtures don't seed any). On timeout, reports the per-CR
// RelationshipsApplied condition so the test author can see WHY the
// bootstrap stalled (SpiceDB unreachable, schema mismatch, etc.).
func (h *Harness) WaitForSpiceDBBootstrap(deadline time.Duration) {
	h.t.Helper()
	cutoff := time.Now().Add(deadline)
	var lastList spiceboxv1alpha1.SpiceDBBootstrapList
	for time.Now().Before(cutoff) {
		var list spiceboxv1alpha1.SpiceDBBootstrapList
		if err := h.K8s.List(context.Background(), &list,
			client.InNamespace(h.opts.Namespace)); err != nil {
			h.t.Fatalf("WaitForSpiceDBBootstrap: list: %v", err)
		}
		lastList = list
		if len(list.Items) == 0 {
			return // no bootstraps to wait on
		}
		allApplied := true
		for i := range list.Items {
			b := &list.Items[i]
			cond := meta.FindStatusCondition(b.Status.Conditions,
				spiceboxv1alpha1.SpiceDBBootstrapConditionRelationshipsApplied)
			if cond == nil || cond.Status != metav1.ConditionTrue {
				allApplied = false
				break
			}
		}
		if allApplied {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	// Build a diagnostic of what's outstanding.
	var b strings.Builder
	fmt.Fprintf(&b, "WaitForSpiceDBBootstrap: timed out after %s\n", deadline)
	for i := range lastList.Items {
		bs := &lastList.Items[i]
		cond := meta.FindStatusCondition(bs.Status.Conditions,
			spiceboxv1alpha1.SpiceDBBootstrapConditionRelationshipsApplied)
		if cond == nil {
			fmt.Fprintf(&b, "  %s/%s: no RelationshipsApplied condition\n",
				bs.Namespace, bs.Name)
		} else {
			fmt.Fprintf(&b, "  %s/%s: RelationshipsApplied=%s reason=%s message=%q\n",
				bs.Namespace, bs.Name, cond.Status, cond.Reason, cond.Message)
		}
	}
	h.t.Fatalf("%s", b.String())
}

// grantSchemaReady reports whether every AgentSessionGrants CR has had its
// pairs composed into the live SpiceDB schema, and when not, what is
// outstanding. Split out from WaitForGrantSchema so the readiness rule is
// testable without standing up a control plane.
//
// An empty list is deliberately NOT ready. "The AgentClass has not written its
// grants yet" and "this fixture has no grants" are the same shape here, and
// reading the pair as success would make the barrier return instantly in
// exactly the case it exists to cover. Callers are scenarios that gate a tool
// on a grant relation, so they always have at least one.
func grantSchemaReady(items []spiceboxv1alpha1.AgentSessionGrants) (bool, string) {
	if len(items) == 0 {
		return false, "no AgentSessionGrants exist yet (the AgentClass has not written its grant pairs)"
	}
	var pending []string
	for i := range items {
		asg := &items[i]
		cond := meta.FindStatusCondition(asg.Status.Conditions,
			spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded)
		switch {
		case cond == nil:
			pending = append(pending, fmt.Sprintf("%s/%s: no SchemaIncluded condition (guardian has not reconciled it)",
				asg.Namespace, asg.Name))
		case cond.Status != metav1.ConditionTrue:
			pending = append(pending, fmt.Sprintf("%s/%s: SchemaIncluded=%s reason=%s message=%q",
				asg.Namespace, asg.Name, cond.Status, cond.Reason, cond.Message))
		}
	}
	if len(pending) > 0 {
		return false, strings.Join(pending, "; ")
	}
	return true, ""
}

// WaitForGrantSchema blocks until every AgentSessionGrants CR in the harness
// namespace reports SchemaIncluded=True — i.e. the guardian has composed their
// (resourceType, permission) pairs into the `agentsession` definition and
// written it to SpiceDB.
//
// Required before any scenario that APPROVES a gated tool call. Approving
// writes agentsession:<sess>#grant_<perm>_<resType>, and the guardian composes
// that relation on a reconcile that lands one debounce interval AFTER the
// AgentClass reports Valid — the two are independent loops. An approval inside
// that window is rejected by SpiceDB with FailedPrecondition ("relation ...
// not found"), the paused dispatch never resumes, and the tool call eventually
// returns SYSTEM_TIMEOUT — which the runner describes to the user as an
// approval nobody acted on. The scenario then fails at ExpectAgentReply, several
// layers away from the cause.
//
// Neither existing barrier covers this: WaitForAgentClassValid says the class
// resolved, and WaitForSpiceDBBootstrap waits on bootstrap RELATIONSHIPS.
// Neither implies the grant SCHEMA is live.
func (h *Harness) WaitForGrantSchema(deadline time.Duration) {
	h.t.Helper()
	cutoff := time.Now().Add(deadline)
	var why string
	for time.Now().Before(cutoff) {
		var list spiceboxv1alpha1.AgentSessionGrantsList
		if err := h.K8s.List(context.Background(), &list,
			client.InNamespace(h.opts.Namespace)); err != nil {
			h.t.Fatalf("WaitForGrantSchema: list: %v", err)
		}
		var ready bool
		if ready, why = grantSchemaReady(list.Items); ready {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	h.t.Fatalf("WaitForGrantSchema: timed out after %s; outstanding: %s", deadline, why)
}

// SchemaRel is one (definition, relation) pair WaitForComposedSchema polls
// SpiceDB's live schema for.
type SchemaRel struct {
	Definition string
	Relation   string
}

// WaitForComposedSchema blocks until SpiceDB's live COMPOSED schema declares
// every (definition, relation) pair in want, or deadline elapses.
//
// This is a DIFFERENT readiness gap than WaitForSpiceDBBootstrap covers.
// Guardian's agentsessiongrants_controller composes an AgentClass's
// authz.slots (via pkg/authz/guardian/schema.ComposeSlots) into the live
// SpiceDB schema on its OWN async reconcile loop — a hop that is independent
// of both AgentClass Valid=True and any SpiceDBBootstrap CR's
// RelationshipsApplied condition. WaitForSpiceDBBootstrap does not stand in
// for it: that function polls SpiceDBBootstrap CRs and returns immediately as
// a no-op when a fixture ships none — which a fixture proving a slot binds
// with NO standing tuple anywhere legitimately does. Every bundle that
// happens to ship its own SpiceDBBootstrap has been getting this composition
// wait for free, as a side effect of THAT poll loop; a bundle shipping
// neither gets no wait at all and can race a slot-gated Check against a
// schema that has not composed the slot_grant_<perm> relation yet —
// bindSlots then fails with a SpiceDB FailedPrecondition ("relation/
// permission ... not found"), which is logged (not silently dropped) but
// leaves the plan phase "approved" with no grant tuple ever written.
//
// Callers derive `want` from the AgentClass under test's authz.slots — one
// SchemaRel per declared slot, built with authz.SlotGrantRelationName so the
// wait and the composer that writes the relation can never name it
// differently.
func (h *Harness) WaitForComposedSchema(deadline time.Duration, want []SchemaRel) {
	h.t.Helper()
	if len(want) == 0 {
		return
	}
	cutoff := time.Now().Add(deadline)
	var lastSchema *spicedb.Schema
	var lastErr error
	for time.Now().Before(cutoff) {
		schema, err := spicedb.FetchAndParseSchema(context.Background(), h.SpiceDB)
		if err != nil {
			lastErr = err
			lastSchema = nil
			time.Sleep(250 * time.Millisecond)
			continue
		}
		lastErr = nil
		lastSchema = schema
		if schemaHasAllRels(schema, want) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "WaitForComposedSchema: timed out after %s waiting for the composed schema to declare:\n", deadline)
	for _, w := range want {
		fmt.Fprintf(&b, "  %s#%s\n", w.Definition, w.Relation)
	}
	if lastErr != nil {
		fmt.Fprintf(&b, "last ReadSchema error: %v\n", lastErr)
		h.t.Fatalf("%s", b.String())
	}
	fmt.Fprintf(&b, "last observed schema state:\n")
	for _, w := range want {
		if lastSchema == nil || !lastSchema.HasDefinition(w.Definition) {
			fmt.Fprintf(&b, "  %s#%s: definition %q not present\n", w.Definition, w.Relation, w.Definition)
			continue
		}
		switch res := lastSchema.ResolvePermission(w.Definition, w.Relation); res {
		case spicedb.PermissionResolutionRelation:
			fmt.Fprintf(&b, "  %s#%s: present (should have matched — a race in this check itself)\n", w.Definition, w.Relation)
		case spicedb.PermissionResolutionNotFound:
			fmt.Fprintf(&b, "  %s#%s: definition exists but has no relation or permission %q\n", w.Definition, w.Relation, w.Relation)
		default:
			fmt.Fprintf(&b, "  %s#%s: %q resolved as a %s, not a relation\n", w.Definition, w.Relation, w.Relation, res)
		}
	}
	h.t.Fatalf("%s", b.String())
}

// schemaHasAllRels reports whether every (definition, relation) pair in want
// resolves to a direct RELATION (not merely a permission of the same name,
// and not merely a defined definition) in schema.
func schemaHasAllRels(schema *spicedb.Schema, want []SchemaRel) bool {
	for _, w := range want {
		if schema.ResolvePermission(w.Definition, w.Relation) != spicedb.PermissionResolutionRelation {
			return false
		}
	}
	return true
}

// WaitForAgentClassValid blocks until the named AgentClass reaches
// Valid=True AND the guardian's composed SpiceDB schema is live, or
// deadline elapses. On timeout, the failure message includes the
// last-observed Valid condition (status/reason/message) so an operator
// reading the test output can see WHY validation stalled — almost always
// more informative than "did not reach Valid=True within Ns".
//
// The schema half is folded in here rather than left to each scenario
// because the two are not independent and the ordering is counterintuitive:
// the AgentClass reconciler creates the class's AgentSessionGrants CR and
// stamps Valid=True in one pass, and the guardian's schema write is the
// reconcile that create triggers. Every scenario that drives traffic gates on
// this call, so this is the one place that makes "the class is ready to serve
// a message" true rather than merely likely. See WaitForAuthzSchema.
func (h *Harness) WaitForAgentClassValid(name string, deadline time.Duration) {
	h.t.Helper()
	cutoff := time.Now().Add(deadline)
	var lastCond *metav1.Condition
	var lastErr error
	for time.Now().Before(cutoff) {
		var ac spiceboxv1alpha1.AgentClass
		if err := h.K8s.Get(context.Background(),
			client.ObjectKey{Namespace: h.opts.Namespace, Name: name}, &ac); err == nil {
			lastErr = nil
			cond := meta.FindStatusCondition(ac.Status.Conditions,
				spiceboxv1alpha1.AgentClassConditionValid)
			if cond != nil {
				lastCond = cond
				if cond.Status == metav1.ConditionTrue {
					h.WaitForAuthzSchema(authzSchemaBarrier)
					return
				}
			}
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	switch {
	case lastErr != nil:
		h.t.Fatalf("AgentClass %s/%s: get failed within %s: %v",
			h.opts.Namespace, name, deadline, lastErr)
	case lastCond != nil:
		h.t.Fatalf("AgentClass %s/%s did not reach Valid=True within %s; last condition: status=%s reason=%s message=%q",
			h.opts.Namespace, name, deadline,
			lastCond.Status, lastCond.Reason, lastCond.Message)
	default:
		h.t.Fatalf("AgentClass %s/%s did not reach Valid=True within %s (no Valid condition ever observed — controllers may not be running)",
			h.opts.Namespace, name, deadline)
	}
}

// WaitForAgentClassInvalid is the negative counterpart of
// WaitForAgentClassValid: it waits for Valid=False carrying a specific reason.
//
// It exists so a test can assert the FAIL-CLOSED direction of a validation rule
// end to end. Asserting the condition in a controller test proves the reason is
// computed; asserting it here proves the class an operator actually applied
// never becomes runnable, which is the property that keeps a misdeclared agent
// from starting.
//
// An empty wantReason matches any reason.
func (h *Harness) WaitForAgentClassInvalid(name, wantReason string, deadline time.Duration) {
	h.t.Helper()
	cutoff := time.Now().Add(deadline)
	var lastCond *metav1.Condition
	var lastErr error
	for time.Now().Before(cutoff) {
		var ac spiceboxv1alpha1.AgentClass
		if err := h.K8s.Get(context.Background(),
			client.ObjectKey{Namespace: h.opts.Namespace, Name: name}, &ac); err == nil {
			lastErr = nil
			cond := meta.FindStatusCondition(ac.Status.Conditions,
				spiceboxv1alpha1.AgentClassConditionValid)
			if cond != nil {
				lastCond = cond
				if cond.Status == metav1.ConditionFalse &&
					(wantReason == "" || cond.Reason == wantReason) {
					return
				}
			}
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	switch {
	case lastErr != nil:
		h.t.Fatalf("AgentClass %s/%s: get failed within %s: %v",
			h.opts.Namespace, name, deadline, lastErr)
	case lastCond != nil:
		h.t.Fatalf("AgentClass %s/%s did not reach Valid=False reason=%s within %s; last condition: status=%s reason=%s message=%q",
			h.opts.Namespace, name, wantReason, deadline,
			lastCond.Status, lastCond.Reason, lastCond.Message)
	default:
		h.t.Fatalf("AgentClass %s/%s never published a Valid condition within %s (controllers may not be running)",
			h.opts.Namespace, name, deadline)
	}
}

// WaitForSessionPhase polls the (ns, name) AgentSession until status.phase
// equals want, or fails the test after deadline. Phase transitions the
// AgentSession reconciler drives asynchronously off a watch (e.g. a
// SessionHold creation, per pkg/controllers/agentsession/hold.go) are not
// synchronous with the API call that triggered them, so a scenario asserting
// on the RESULT of one polls rather than checking immediately after.
func (h *Harness) WaitForSessionPhase(ns, name, want string, deadline time.Duration) {
	h.t.Helper()
	cutoff := time.Now().Add(deadline)
	var last string
	var lastErr error
	for time.Now().Before(cutoff) {
		var sess spiceboxv1alpha1.AgentSession
		if err := h.K8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &sess); err == nil {
			lastErr = nil
			last = sess.Status.Phase
			if last == want {
				return
			}
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr != nil {
		h.t.Fatalf("AgentSession %s/%s: get failed within %s: %v", ns, name, deadline, lastErr)
	}
	// Dump the state a stuck phase most often hinges on, so the failure names
	// WHICH step of an async chain stalled instead of only its final symptom.
	var sess spiceboxv1alpha1.AgentSession
	detail := "(state re-read failed)"
	if err := h.K8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &sess); err == nil {
		conds := make([]string, 0, len(sess.Status.Conditions))
		for _, c := range sess.Status.Conditions {
			conds = append(conds, fmt.Sprintf("%s=%s/%s", c.Type, c.Status, c.Reason))
		}
		detail = fmt.Sprintf("annotations=%v pendingRequesters=%d pendingInteractions=%d startFailure=%v failureReason=%q conditions=%v",
			sess.Annotations, len(sess.Status.PendingRequesters), len(sess.Status.PendingInteractions),
			sess.Status.StartFailure, sess.Status.FailureReason, conds)
	}
	h.t.Fatalf("AgentSession %s/%s did not reach phase %q within %s (last observed: %q); %s",
		ns, name, want, deadline, last, detail)
}

// SimulateRetryClick publishes a generic interaction_decision
// (provider_error_retry) envelope on the session's IN NATS subject, as if a
// participant clicked the "Retry" button on the interaction_request the session
// watcher broadcast. The pipeline's HandleInteractionDecision (subscribed in
// startChannelsdPlumbing) re-checks the decider's interact standing
// (DecideParticipant → CheckSessionInteract, fail-closed) and invokes the bound
// decideProviderRetry handler, which stamps the wake annotation — driving the
// operator to respawn the parked runner (AwaitingRetry → Pending).
//
// requestRef is the value from an InteractionPrompt whose Category ==
// provider_error_retry (Driver.InteractionPrompts()[i].Payload.RequestRef). The
// decider defaults to the harness DefaultUser (the session's started_by, who
// holds interact standing via `interact = owner + participant`); AsUser
// overrides it. A decider without standing is a silent no-op on the pipeline
// side (logged, no Applied published) — see interaction_decision.go.
func (h *Harness) SimulateRetryClick(t *testing.T, sess *spiceboxv1alpha1.AgentSession, requestRef string, opts ...SendOption) {
	t.Helper()
	cfg := sendCfg{user: h.opts.DefaultUser}
	for _, o := range opts {
		o(&cfg)
	}
	payload := channelevents.InteractionDecisionPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Category:        categories.ProviderErrorRetry,
		RequestRef:      requestRef,
		ActionID:        "retry",
		Decider: channelevents.ExternalIdentity{
			Kind:       "fake",
			ExternalID: identity.RawExternalID(cfg.user),
			Email:      identity.Email(cfg.user),
		},
	}
	require.NoError(t, channelevents.PublishIn(
		func(subj string, b []byte) error { return h.nc.Publish(subj, b) },
		sess.Namespace, sess.Name, channelevents.KindInteractionDecision, payload,
	))
	require.NoError(t, h.nc.Flush())
}
