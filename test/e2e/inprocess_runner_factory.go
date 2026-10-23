//go:build e2e

package e2e

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/modality/files"
	_ "github.com/authzed/openagentprimitives/pkg/agent/postsession/cost" // mirror production session accounting hook
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/runner/identityadvisor"
	"github.com/authzed/openagentprimitives/pkg/agent/runner/leakagewiring"
	"github.com/authzed/openagentprimitives/pkg/agent/runner/userprofilegate"
	"github.com/authzed/openagentprimitives/pkg/agent/secretout"
	// The completion-requirement kinds an AgentClass may declare, mirroring the
	// blank imports in internal/cmd/runner: which requirements exist is a
	// property of the build, and the gate resolves a declared key through the
	// registry at call time.
	_ "github.com/authzed/openagentprimitives/pkg/agent/completion/kinds/artifactdelivery"
	_ "github.com/authzed/openagentprimitives/pkg/agent/completion/kinds/plansteps"
	_ "github.com/authzed/openagentprimitives/pkg/agent/completion/kinds/triggerconcluded"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	// Registers the "deliveries" state kind, which respond_to_user writes and
	// the artifact-delivered requirement reads.
	_ "github.com/authzed/openagentprimitives/pkg/agent/session/state/deliveries"
	// Registers the "openingsummary" state kind, which the enrichment tool
	// writes and the runner reads to keep a triggered session's pinned
	// opening message current.
	_ "github.com/authzed/openagentprimitives/pkg/agent/session/state/openingsummary"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	_ "github.com/authzed/openagentprimitives/pkg/agent/session/state/plans" // registers the "plans" state kind so update_plan works in-process
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/authfail"
	mcpdispatch "github.com/authzed/openagentprimitives/pkg/agent/tool/mcp"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/originfmt"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	sidecartoolboxsynth "github.com/authzed/openagentprimitives/pkg/agent/tool/sidecartoolbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/grants"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
	"github.com/authzed/openagentprimitives/pkg/authz/trifecta"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	// The renderer kinds, mirroring internal/cmd/runner. AvailableAssetKinds
	// reads this registry, and an empty one makes the artifacts capability skip
	// with "no renderer registered" — so a scenario granting `artifacts` would
	// silently get no artifact tools at all.
	goalcore "github.com/authzed/openagentprimitives/pkg/agent/goals"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/css"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/image"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/svg"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/resolve"
	agentsession "github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
	"github.com/authzed/openagentprimitives/pkg/memory"
	goalinmem "github.com/authzed/openagentprimitives/pkg/memory/goals/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagedecision" // registers the durable leakage-decision kind the InfoLeakAudience hook reads
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttagcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/pttagmint"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	clikind "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/cli"
	mcpkind "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/mcp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker/inproc"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/externaltoken"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation"
	"github.com/authzed/openagentprimitives/pkg/platform/settings"
	mcpprobe "github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	"github.com/authzed/openagentprimitives/pkg/tools/toolkitstream"
	_ "github.com/authzed/openagentprimitives/pkg/tools/toolkitstream/claude" // registers claude-stream-json
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	goalweb "github.com/authzed/openagentprimitives/pkg/web/goals"
	"github.com/authzed/openagentprimitives/pkg/web/secretoutsrv"
	eventweb "github.com/authzed/openagentprimitives/pkg/web/sessionevents"
	"github.com/authzed/openagentprimitives/pkg/web/uigrant"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
	"github.com/authzed/openagentprimitives/toolkits"
)

// ptTagOperatorPublisher is the provenance publisher the harness signs minted
// pt_tags under. It matches the operator's own publisher string, because in
// production the operator is the only component that may author a tag — see
// the pt-tag block in buildLoop for why the harness must not reuse the session
// signer here.
const ptTagOperatorPublisher = "system:operator"

// InProcessRunnerFactory satisfies pkg/controllers/agentsession.RunnerFactory
// by constructing a *runner.Loop in-process per session and running it in
// a goroutine. The e2e harness wires this into the AgentSession controller
// instead of the production PodRunnerFactory; everything else (controllers,
// channelsd, SpiceDB) runs unchanged.
//
// Tool scope: meta tools are assembled through capability.Assemble — the SAME
// registry seam internal/cmd/runner drives — so this factory and the real runner cannot
// drift. The active capabilities (core, planning, introspection, and, when
// channel-attached, channel_interaction / channel_history / mention_lookup /
// thread_history, plus opt-in memory / knowledge / artifacts when the class
// grants them) decide which meta tools appear. Non-meta tools are handed to
// Assemble as NonMetaTools: MCP-backed tools synthesized from
// AgentClass.spec.mcpServers when SpiceDB is wired (the path T14's canonical
// scenario exercises) PLUS sandbox tools synthesized per-bundle from
// AgentSession.Status.BundleSessions when the bundle's SpiceboxSession has
// Status.ResolvedClass populated (the p1_defaultenv_reaches_toolcall scenario
// exercises this path; the harness stamps ResolvedClass since the
// SpiceboxSession controller is not wired) PLUS resolved sidecar-toolbox tools
// PLUS the harness ExtraTools seam. Toolkit (custom SpiceboxToolkit CR) tools
// remain out of scope; toolkit resolution is built-ins only.
//
// The RunnerEnv the factory builds is HONEST about what its fakes back:
// MemoryAvailable is true (loop.Mem is wired, so query_memory works) and
// SearchAvailable follows MemStore — e2e.Start's shared facade carries a real
// CompositeSearcher with a real SpiceDB authorizer, so search_memory returns
// rows, while a factory built directly in a Go test with no MemStore falls back
// to a searcher-less facade and withholds the tool. KGAvailable stays false (no
// KG client is wired, so query_knowledge would be a broken tool and the
// knowledge capability gracefully skips). It leaves the artifacts
// capability's deps (Artifacts/RenderFetch/
// MarkupGen) and the await pump (InboundCh/IdleTTL/Clock) unset, and late-binds
// the leakage gate + await yield/resume to the Loop — capabilities whose fakes
// are absent gracefully skip (logged, never silent).
//
// Prompt scope: only AgentClass.Spec.SystemPrompt.Inline is honored.
// ConfigMap-backed prompts resolve to an empty system prompt — fine for the
// LLM-scripted scenarios this harness targets; extend if a test needs full
// prompt resolution.
type InProcessRunnerFactory struct {
	// LLM is the provider every spawned runner.Loop uses. Tests pass a
	// *ScriptedLLM. The interface field is initialized from a concrete
	// pointer at the call site; never assign a typed-nil pointer here
	// (see AGENTS.md "Nil interfaces").
	LLM llm.Provider

	// K8s is optional. When set, the runner's StatusPatcher patches
	// AgentSession.status against this client (envtest scenarios see the
	// updates). When nil, runner.LocalStatusPatcher() is used —
	// status writes short-circuit to in-memory captures.
	K8s client.Client

	// APIReader, when set, is a non-cached direct-to-apiserver reader —
	// mirrors mgr.GetAPIReader() (the same seam agentsession.Reconciler's own
	// APIReader/adoptguard.SecretReader use). Used only by mcpArgsHashKey: the
	// args-hash-key Secret this reads is created by the SAME AgentSession
	// Reconcile() invocation that, moments later, calls RunnerFactory.Start —
	// on first boot there's no guarantee the manager's informer cache (which
	// f.K8s reads through) has observed that Create yet, so a cached Get could
	// spuriously NotFound and requeue the whole reconcile. Also handed to the
	// StatusPatcher (WithDirectReader) for the same class of read: the post-idle
	// wake decision must observe the phase the patcher itself just wrote.
	// Nil-safe at both call sites.
	APIReader client.Reader

	// DirectK8s, when set, is a non-cached direct-to-apiserver client that can
	// also WRITE — the read-only APIReader above is not enough for a consumer
	// that creates an object and then immediately reads it back.
	//
	// It exists because f.K8s is the manager's CACHED client, while
	// internal/cmd/runner/main.go hands RunnerEnv.CredentialUpdateClient its own DIRECT
	// client, with a comment saying exactly why: request_credential_update
	// creates a CredentialUpdateRequest and then polls it, and a cached Get
	// issued microseconds after the Create reads an informer that has not
	// observed it yet. The tool treats a NotFound as "the request vanished
	// before a decision was reached" and gives up — correct against a direct
	// client (only owner-ref GC can make a live request disappear), fatal
	// against a cached one. Wiring the cached client here made the harness test
	// a configuration production does not run. Nil falls back to f.K8s.
	DirectK8s client.Client

	// NATS, when set, is wired into the respond_to_user meta tool so the
	// runner can publish outbound user_message envelopes on
	// ap.session.<ns>.<name>.out.user_message — the same subject channelsd's
	// outbound relay subscribes to. When nil, respond_to_user is still
	// registered but its publish function is a no-op (the agent's reply
	// goes nowhere; useful for tests that don't exercise the outbound path).
	NATS *nats.Conn

	// MemStore, when set, is the shared in-process memory facade the
	// runner reads/writes via a turn.Appender. The channelsd pipeline
	// (T9) writes inbound user turns into this same facade so a
	// mid-session reply on an Idle session is visible to the resumed
	// runner. When nil, each spawned runner gets its own fresh
	// in-memory facade — adequate for cold-start scenarios where the
	// user's first message rides through session.spec.prompt, but
	// breaks any test that needs multi-turn memory continuity.
	MemStore memory.Memory

	// SpiceDB, when set, enables MCP tool dispatch + per-tool SpiceDB
	// Checks + the approval round-trip (publish interaction_request,
	// subscribe to interaction_applied, resume). nil disables the
	// authz layer entirely — the runner runs every tool unchecked
	// (ToolAuthMode behaves as "disabled" regardless of the AgentClass
	// setting). T14 requires this to be set.
	SpiceDB *spicedb.Client

	// OpSigned, when set, is the operator's own signing facade over MemStore
	// (provenance.NewSigningMemory under "system:operator" — the harness's
	// analogue of internal/cmd/operator's d.OpSigned). Threaded through so
	// ensurePrefsServer can wire httpsrv.WithPreferenceAudit, which — like
	// production — refuses every ?user-ref= preferences read outright when
	// this is nil rather than serving it unaudited. nil is a legitimate
	// harness configuration (most tests need no ComponentWritten append-only
	// writer at all); it just means get_preferences' named-user form is
	// unavailable for that run, the same outcome a real deployment missing
	// this wiring would report.
	OpSigned memory.Memory

	// TokenAuthz, when true, wires each MCPTool's durable per-call
	// externaltoken use_token gate (MCPTool.SetUseTokenGate) in buildMCPTools,
	// mirroring internal/cmd/runner/main.go. Set from Options.WithTokenAuthz — off by
	// default because the gate only functions correctly when the paired
	// AgentSession reconciler TokenGranter/TokenChecker are ALSO wired (the
	// operator must actually write the grant this gate checks); the harness
	// gates both on the same opt-in so a plain SpiceDB-wired test isn't
	// silently broken by every MCP call denying for want of a grant nothing
	// ever wrote. Requires SpiceDB != nil (checked in buildMCPTools).
	TokenAuthz bool

	// BridgeDialOpt is appended to Loop.InteractiveHooks.BridgeDialOpts so
	// the runner's gateway bridge dials the harness's in-process bufconn
	// instead of a real TCP socket. Set by the harness when
	// WithToolCallController is true; nil otherwise.
	BridgeDialOpt grpc.DialOption

	// SidecarProbeURL, when non-nil, overrides the loopback probe URL for a
	// resolved sidecar. Test seam: the in-process MCP stub listens on a random
	// httptest port, not the controller-allocated rt.Port. Production leaves
	// this nil and probes http://127.0.0.1:<rt.Port>.
	SidecarProbeURL func(rt spiceboxv1alpha1.ResolvedSidecarToolbox) string

	// ExtraTools, when non-empty, are appended to every spawned runner.Loop's
	// tool set. Test seam for scenarios that need a tool the CRD-driven
	// synthesis paths can't produce in-process (e.g. a producer tool that
	// emits a secretOutput — the SpiceboxToolspec CRD does not yet mirror
	// the toolspec library's secretOutput field, so the sandbox synthesis
	// path can't carry it). Production has no equivalent; the tools here run
	// through the identical runner dispatch + applySecretOutput path a real
	// sandbox producer would.
	ExtraTools []tool.Tool

	// TriggerStatusAPIBaseURL points the trigger-status meta tools at a
	// stand-in provider instead of the real one. Empty (the default) leaves
	// each channel kind's own default, which is what production runs.
	//
	// Without it a scenario whose input channel kind reports trigger status —
	// github — would reach out to api.github.com from a test, so this is the
	// seam that makes the claim/conclude path exercisable at all. Mirrors
	// pkg/controllers/channel's GitHubAPIBaseURL, which exists for the same
	// reason on the drift-check path.
	TriggerStatusAPIBaseURL string

	// Tokens is the shared per-test tokens registry the harness also gives
	// the AgentSession reconciler. buildLoop registers each session's audit
	// signing public key here (under provenance.SessionPublisher) so a
	// verify-on-write facade accepts the runner's signed writes. May be nil
	// in unit tests that construct the factory directly; buildLoop nil-guards
	// the registration.
	Tokens *tokens.Registry

	// AwaitIdleTTL, when > 0, makes await_user_message actually PARK in-process
	// (block on a live InboundCh fed by channelsd's wakeup) for this long before
	// idle-exiting, instead of the default immediate idle-exit. Opt-in per
	// scenario (default 0 preserves the idle-exit-then-respawn path every other
	// e2e scenario relies on) so a test can exercise the real in-process
	// await-resume drain — the path a live conversation takes and the one the
	// budget-blowup incident occurred on. Wired together with the wake→InboundCh
	// pump in buildLoop, gated on the same > 0 condition.
	AwaitIdleTTL time.Duration

	// Minter, when non-nil, is wired into the in-process token broker so
	// that type=federated credentials are resolved via ID-JAG (or a fake
	// equivalent in e2e tests). When nil the broker fails closed on any
	// federated credential — existing tests are unaffected because they use
	// only static/oauth credentials.
	Minter federation.Minter

	// GitHubAppMinter, when non-nil, is wired into the in-process token broker
	// so that type=githubApp credentials resolve — the exact mirror of Minter
	// above, for the other minted credential type. When nil the broker fails
	// closed on one, which is what production does wherever no App minter is
	// configured.
	//
	// A CAPTURED session of a GitHub App agent cannot replay without it: the
	// fixture rewrite emits a placeholder Secret, which is enough for the
	// replayed AgentIdentity to go Valid and worth nothing at resolve time, so
	// every drawing call fails at dispatch with the identity reporting healthy.
	// Pointed at the fixture provider, the credential is minted FOR REAL — the
	// App JWT is signed by githubapp's own code and exchanged over HTTP — so
	// nothing about its minted-ness is stood in for. That is why the capture
	// refuses a minted type this harness does NOT serve rather than rewriting
	// it to a stored one; see bt.StandInMintedCredentialTypes.
	GitHubAppMinter credkind.GitHubAppMinter

	// ContentInspectors / ContentInspectorIDs, when non-empty, are wired into
	// every spawned runner.Loop's content-guard pipeline — the e2e seam mirroring
	// SetMinter. When empty the Loop derives inspectors from
	// sess.Status.EffectiveSettings.ContentInspectors (the production path).
	ContentInspectors   []contentguard.Instance
	ContentInspectorIDs []string

	// IdentityRecommender, when set, is wired as every spawned runner.Loop's
	// isolated advisory recommender for identityMode=dynamic sessions — the e2e
	// seam mirroring SetMinter / SetContentInspectors. Its presence is what
	// turns a first-boot dynamic session's identity-choice request into a
	// "dynamic" (recommended) prompt rather than a plain "ask". Tests inject an
	// identityadvisor.Fake. nil ⇒ dynamic degrades to a plain ask (no
	// recommendation), matching the gate's fail-open recommender contract.
	IdentityRecommender identityadvisor.Provider

	// ArtifactStore, when set (Harness.SetArtifactStore), backs
	// RunnerEnv.Artifacts (artifacts.NewService(f.MemStore), mirroring
	// internal/cmd/runner's own artifactSvc) and RunnerEnv.ArtifactReader
	// (files.StoreReader{Store: ArtifactStore}) for every spawned
	// runner.Loop — the e2e seam mirroring SetMinter / SetContentInspectors.
	// nil (the default) leaves both RunnerEnv fields nil, exactly the
	// pre-existing behavior: the "artifacts" capability's Offer skips
	// (Env.Artifacts == nil) and the files modality contributes no
	// fetch_artifact tool (Env.Reader == nil) — see capability/artifacts.go
	// and modality/files/files.go. A scenario that needs an agent to read a
	// stored attachment's extracted text via fetch_artifact sets this to the
	// SAME store its UploadInboundAsset hook (SetInboundAssetUploader) wrote
	// the text into, so the handle the pipeline names in its manifest line
	// actually resolves.
	ArtifactStore artifactstore.Store

	// ToolCallArtifacts is the store the ToolCall reconciler writes each
	// sandbox call's stdout and stderr into. It backs
	// SessionContext.ArtifactClient, which is how the sandbox tool reads those
	// bytes back to compose the result the model sees — ToolCall.status carries
	// only artifact REFS, never the output itself.
	//
	// Distinct from ArtifactStore directly above, which is the AGENT's artifact
	// store (artifact_prepare, fetch_artifact). Production has one cluster-wide
	// store serving both through the operator's endpoint; the harness has two
	// because they are wired by two different seams, and conflating them would
	// make a scenario that never calls SetArtifactStore silently lose its
	// sandbox output again.
	//
	// Set by Start from Harness.toolCallStore, so it is non-nil exactly when
	// Options.WithToolCallController is. nil otherwise, and the sandbox tool
	// tolerates that: readArtifact returns "" for a nil client, which is what
	// every pre-existing non-toolcall scenario already got.
	ToolCallArtifacts artifactstore.Store

	// NewOperationID and NewArtifactID pin the ids the two id-minting
	// components hand back to the MODEL, for a whole-session replay that has to
	// mint what its captured run minted. nil (the default, and what every
	// production binary passes) leaves each component minting its own. Mirrors
	// internal/cmd/runner/main.go, which passes nil to both.
	NewOperationID func() string
	NewArtifactID  func() string

	// NewRenderName and NewRevisionID pin the other two ids the artifact path
	// hands back to the MODEL: the ArtifactRender CR name that leaves as
	// artifact_prepare's `handle`, and the revision id derived from that CR's
	// UID. Same nil-is-production rule as the two above.
	//
	// NewRevisionID takes the UID rather than nothing, and that is load-bearing
	// rather than incidental: the production derivation is a pure function of
	// the UID, and prepare->await idempotence rests on it. See
	// artifacts.WithRevisionIDMinter.
	NewRenderName func(session string) string
	NewRevisionID func(uid string) string

	// HoldToolsFromAssembly and FilterOfferedTools hold a whole-session replay
	// to the tool catalog its captured run recorded: the first keeps named
	// tools out of capability.Assemble (so the composed surfaces match the
	// recording) while leaving them in the Loop's live set, the second narrows
	// each request. Both are empty/nil for every other scenario and for every
	// production binary. See the Options fields of the same names.
	HoldToolsFromAssembly []string
	FilterOfferedTools    func(turnIndex int, offered []string) []string

	// ReplaceAssembledTool rewrites one assembled tool before anything reads
	// the list. See Options.ReplaceAssembledTool; nil leaves every tool as
	// assembled.
	ReplaceAssembledTool func(tool.Tool) (tool.Tool, error)

	// secretOutSrv is a lazily-started httptest server mounting the operator's
	// secretoutsrv.NewHandler over f.K8s + secretOutTokens. The in-process
	// runner's SecretOutPublisher POSTs captured secret values here, so the
	// real operator endpoint (auth + per-session Secret write) is exercised
	// end-to-end without standing up the full operator HTTP mux. Started on
	// first buildLoop when f.K8s is set; closed in Shutdown.
	secretOutOnce   sync.Once
	secretOutSrv    *httptest.Server
	secretOutTokens *tokens.Registry

	// prefsSrv is a lazily-started httptest server mounting the REAL
	// pkg/memory/httpsrv preferences routes (httpsrv.WithPreferences(f.K8s))
	// over f.MemStore — same shape as secretOutSrv immediately above, and for
	// the same reason: the interesting behavior (turn-author resolution,
	// admin-global/lock precedence, the durable user_preference read/write)
	// lives in that handler, not in a thin data-plane pass-through, so a
	// hand-rolled e2e stand-in would prove nothing about it. Both
	// PreferencesReader (GET, wired in buildLoop below) and channelsd's
	// PreferenceCommitter (POST, wired in startChannelsdPlumbing) point an
	// httpclient.Client at this SAME server. Started on first
	// preferencesClientFor when f.K8s is set; closed in Shutdown.
	GoalServer  *goalweb.Server
	EventServer *eventweb.Server
	prefsOnce   sync.Once
	prefsSrv    *httptest.Server
	prefsTokens *tokens.Registry

	mu      sync.Mutex
	running map[string]*runnerHandle

	// toolSessionShutdownMu guards the per-session cancel funcs the
	// InteractiveHooks plumbing installs in buildLoop. Each entry is the
	// cancel for a subscribeToolSessionInput goroutine that must be
	// stopped when the session is torn down (Stop) so the harness's
	// goroutine-leak baseline stays stable across tests.
	toolSessionShutdownMu sync.Mutex
	toolSessionShutdowns  map[string]context.CancelFunc

	// applySubOnce + applySubCancel ensure we install the single
	// process-wide interaction_applied (tool_approval) subscriber lazily on the
	// first session start, instead of in the harness's startup chain (which
	// would force every test — even non-approval ones — to pay the
	// subscribe-cost and goleak.IgnoreCurrent dance). Cleanup is bound
	// to the manager context the harness owns; see Start below.
	applySubOnce   sync.Once
	applySubCancel context.CancelFunc
	// applyOrchestrators is the per-session orchestrator registry the
	// shared subscriber routes Apply envelopes into. Indexed by
	// "<ns>/<name>" so subjects can be matched without re-parsing.
	applyMu            sync.Mutex
	applyOrchestrators map[string]*approval.Orchestrator
	// sessionCaches holds each running session's persistent MCP session cache
	// (the harness equivalent of internal/cmd/runner's `mcpSessionCache`), indexed by
	// "<ns>/<name>" and Closed in Stop.
	sessionCacheMu sync.Mutex
	sessionCaches  map[string]*mcpprobe.SessionCache
}

type runnerHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Compile-time check: keeps drift between this package and the controller
// visible at build time rather than at envtest-startup time.
var _ agentsession.RunnerFactory = (*InProcessRunnerFactory)(nil)

// Start launches a runner.Loop goroutine for the session. Idempotent per
// (sess.Namespace, sess.Name) — re-calling for an already-running session
// is a no-op.
// The opts argument carries per-call inputs (resolved sidecars) the
// reconciler computes from live cluster state at spawn time. The
// production PodRunnerFactory uses them to compose sidecar containers;
// the in-process factory has no real Pod, but it DOES synthesize
// sidecar-toolbox tools from opts.ResolvedSidecars (mirroring
// internal/cmd/runner/main.go's sidecar block) — so they're threaded into
// buildLoop rather than ignored. We prefer opts.ResolvedSidecars over
// re-reading sess.Status.ResolvedSidecarToolboxes because the snapshot
// the reconciler passes is guaranteed current; the status write may not
// have round-tripped through the informer cache by the time Start runs.
func (f *InProcessRunnerFactory) Start(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass, opts agentsession.StartOpts) error {
	key := sessionKey(sess)

	// Hold the lock across loop construction + goroutine spawn so two
	// concurrent Start(sess) for the same key can't both pass the
	// existence check and spawn duplicate goroutines (leaking the
	// first cancel-fn). Loop construction is cheap and pure; goroutine
	// spawn is sub-microsecond. No callbacks under the lock.
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running == nil {
		f.running = map[string]*runnerHandle{}
	}
	if _, exists := f.running[key]; exists {
		return nil
	}

	// Synthesize a placeholder runner Pod so the reconciler's "5b. Runner Pod"
	// existingPod Get (controller.go) finds one. That Get gates a hard early
	// return on NotFound (RunnerCreating, applyStatus, return) that sits BEFORE
	// the end-of-loop lifecycle fold which derives phases like
	// AwaitingIdentityChoice / Running from the signed log (see
	// controller.go's foldLifecycle + derivePhase call, reached only once the
	// pod exists). PodRunnerFactory's real Pod satisfies this in production;
	// the in-process factory has no real Pod at all, so without this the fold
	// never runs and the operator can never project any phase beyond the
	// bootstrap Pending/whatever an explicit applyEvent set directly (Failed,
	// Idle, Succeeded, AwaitingCredentials all bypass the fold via direct
	// writes — AwaitingIdentityChoice does not; it is fold-only). Best-effort:
	// log and continue on failure rather than block Start, since some fixtures
	// run without f.K8s wired.
	//
	// Scoped to identity-choice first-boot sessions only (identityGatePending):
	// every extra reconcile that finds a real (if placeholder) Pod also flows
	// through reflectRunnerPod + the end-of-loop lifecycle fold on EVERY
	// steady-state reconcile for the session's whole lifetime, not just while
	// awaiting the identity choice. For the vast majority of sessions that
	// never needed AwaitingIdentityChoice, that's pure additional per-reconcile
	// K8s API + memory-fold work with no behavioral payoff — it was found to
	// measurably increase e2e wall-clock and flake rate under suite contention
	// (see test/e2e/scenarios/centerdot/contacts_owner_timeout, a
	// timing-sensitive 3s-approval-deadline scenario that regressed from
	// reliably ~14s to intermittently 40s+ once this ran unconditionally).
	// Gating it keeps non-identity sessions byte-for-byte on the pre-Task-10
	// (no placeholder Pod) code path.
	hasPlaceholderPod := identityGatePending(sess, class) || sess.Spec.GoalExecution != nil
	if hasPlaceholderPod {
		if err := f.ensureRunnerPodPresent(ctx, sess); err != nil {
			slog.Default().Info("inprocess: ensure placeholder runner pod failed (best-effort)",
				"session", key, "err", err.Error())
		}
	}

	loop, err := f.buildLoop(sess, class, opts.ResolvedSidecars)
	if err != nil {
		return fmt.Errorf("inprocess: build loop for %s: %w", key, err)
	}

	// The runner goroutine owns its own context so Stop can cancel
	// independently of the caller's request context (the AgentSession
	// reconcile that triggered Start typically completes well before
	// the loop does).
	runCtx, cancel := context.WithCancel(context.Background())
	// The in-process harness wires the runner straight to the shared *memory.Local,
	// bypassing the operator httpsrv that mints a capability approval per request in
	// production. Mint a system approval here so the runner's memory reads/writes
	// (transcript, leakage taint, artifacts) clear the capability doors — the
	// harness equivalent of the httpsrv mint.
	runCtx = memory.WithSystemApproval(runCtx, "e2e-runner")
	done := make(chan struct{})
	// Snapshot the placeholder Pod's key before the goroutine starts: the
	// reconciler keeps mutating the *AgentSession it passed to Start, and the
	// exit path below runs long after Start returned.
	runnerPodKey := client.ObjectKey{Namespace: sess.Namespace, Name: agentsession.RunnerPodName(sess)}
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				// Surface to stderr so harness self-tests / goleak
				// hooks in later tasks can detect it. Don't propagate
				// — the goroutine has no caller to return to.
				fmt.Fprintf(os.Stderr, "InProcessRunnerFactory: runner goroutine panic for %s: %v\n", key, r)
			}
		}()
		// Run returns either:
		//   - nil after a terminal status write (Succeeded / Idle), or
		//   - the status-write error from l.fail (which already logged
		//     reason+message into the status patcher).
		// In both cases the goroutine's job is to exit; the error is
		// already captured / surfaced through StatusPatcher.
		if err := loop.Run(runCtx); err != nil {
			fmt.Fprintf(os.Stderr, "InProcessRunnerFactory: runner for %s exited with err: %v\n", key, err)
		}
		// The runner process just ended. Reflect that on the placeholder Pod so
		// the operator observes it the way it observes a real runner Pod going
		// Succeeded — see markRunnerPodExited for why the userPassthrough
		// handoff has no other way to reach the operator. Gated on the same
		// predicate that created the Pod, so a session that never got one costs
		// nothing at exit.
		if hasPlaceholderPod {
			f.markRunnerPodExited(runnerPodKey, key)
		}
	}()

	f.running[key] = &runnerHandle{cancel: cancel, done: done}
	return nil
}

// IsRunning reports whether a runner goroutine is currently registered for
// (ns, name). Test seam for identity-choice e2e assertions: "a runner is up in
// AwaitingIdentityChoice" (unlike AwaitingCredentials, which has none) and "the
// runner was stopped when the passthrough choice parked AwaitingCredentials".
func (f *InProcessRunnerFactory) IsRunning(ns, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.running[ns+"/"+name]
	return ok
}

// Stop cancels the runner goroutine and waits up to 5s for it to exit.
// Idempotent — returns nil if no runner is registered for the session.
func (f *InProcessRunnerFactory) Stop(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	key := sessionKey(sess)

	f.mu.Lock()
	h, ok := f.running[key]
	delete(f.running, key)
	f.mu.Unlock()

	// Delete the placeholder runner Pod ensureRunnerPodPresent created in
	// Start, so a later re-Start (e.g. the passthrough re-spawn after a
	// credential link) recreates a fresh one rather than colliding with a
	// stale name. Best-effort — a stop that races a not-yet-created pod, or
	// a fixture with no f.K8s wired, must not fail the stop.
	f.deleteRunnerPod(ctx, sess)

	// Drop the per-session orchestrator from the Apply-subscriber's
	// registry so a stray Apply envelope post-Stop doesn't deliver to a
	// runner that's about to exit. Independent of the runner-handle
	// existence check — sessions that registered an orchestrator but
	// haven't been started (shouldn't happen, but defensive) still need
	// the registry entry cleared.
	f.applyMu.Lock()
	delete(f.applyOrchestrators, key)
	f.applyMu.Unlock()

	// Close this session's MCP sessions, mirroring internal/cmd/runner's deferred
	// mcpSessionCache.Close at session end.
	f.closeSessionCache(sess)

	// Cancel the per-session subscribeToolSessionInput goroutine (if
	// any) so it drains its NATS subscription and exits. Matches the
	// same shape as the orchestrator drop above: independent of the
	// runner-handle existence check so a partially-built session still
	// gets its goroutine cleaned up.
	f.toolSessionShutdownMu.Lock()
	if cancel, ok := f.toolSessionShutdowns[key]; ok {
		cancel()
		delete(f.toolSessionShutdowns, key)
	}
	f.toolSessionShutdownMu.Unlock()

	if !ok {
		return nil
	}

	h.cancel()
	select {
	case <-h.done:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("inprocess: runner for %s did not exit within 5s", key)
	}
}

// ObservedName returns "" — the in-process runner is a goroutine, and the
// placeholder Pod ensureRunnerPodPresent creates exists only so the
// reconciler's pod watch has something to observe. Session status does not
// surface it.
func (f *InProcessRunnerFactory) ObservedName(_ *spiceboxv1alpha1.AgentSession) string {
	return ""
}

// ensureRunnerPodPresent creates (idempotently) a minimal placeholder Pod
// named agentsession.RunnerPodName(sess). This is NOT a real runner
// container — the in-process factory runs the loop as a goroutine — it
// exists solely so the reconciler's raw client.Get for the runner Pod
// (controller.go's "5b. Runner Pod" step) finds an object instead of
// NotFound. See the call site in Start for why that Get gates the
// lifecycle-fold phase derivation (foldLifecycle + derivePhase, reached
// only once the Get succeeds).
//
// Deliberately left at Phase=Pending with NO Ready container status: marking
// it Running/Ready would flip the RunnerReady condition false→true, which
// makes the reconciler append a RunnerClaimed event (controller.go's
// "Authority handoff ①"). RunnerClaimed's transition unconditionally sets
// Phase=Running (transition.go), and a real pod's readiness only flips after
// the runner process has been alive for a beat — in the harness the pod would
// go Ready before the runner goroutine reaches SessionStart, so RunnerClaimed
// would fold chronologically AFTER IdentityChoicePending and clobber
// AwaitingIdentityChoice straight back to Running. Staying Pending sidesteps
// that ordering race entirely: the fold then derives phase purely from the
// events the runner itself appends (IdentityChoicePending, CredsMissing,
// etc.), which is all today's e2e assertions need. A no-op when f.K8s is nil
// (fixtures that construct the factory directly, without a live client).
//
// It carries the SAME controller ownerReference podspec.go puts on the real
// runner Pod (cosidecar.OwnerRef). That is not cosmetic: the AgentSession
// reconciler registers Owns(&corev1.Pod{}), which maps an event to a session
// only through a controller ownerReference. Without one this Pod is invisible
// to the controller — its create, its status writes, and its delete enqueue
// nothing — so the harness silently loses every reconcile trigger production
// gets from the runner Pod's lifecycle. markRunnerPodExited (below) depends on
// that mapping to deliver the runner's exit.
func (f *InProcessRunnerFactory) ensureRunnerPodPresent(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	if f.K8s == nil {
		return nil
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            agentsession.RunnerPodName(sess),
			Namespace:       sess.Namespace,
			OwnerReferences: cosidecar.OwnerRef(sess),
			Labels: map[string]string{
				"agentprimitives.authzed.com/agentsession": sess.Name,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyOnFailure,
			Containers: []corev1.Container{{
				Name:  "runner",
				Image: "e2e-inprocess-runner-placeholder",
			}},
		},
	}
	if err := f.K8s.Create(ctx, pod); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create placeholder runner pod: %w", err)
	}
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending}
	if err := f.K8s.Status().Update(ctx, pod); err != nil {
		return fmt.Errorf("patch placeholder runner pod status: %w", err)
	}
	return nil
}

// markRunnerPodExited reflects the runner goroutine's exit onto the
// placeholder Pod as Phase=Succeeded, the way the kubelet reflects a real
// runner container returning 0.
//
// This is the harness's stand-in for the single most important reconcile
// trigger the in-process runner otherwise has no way to produce. A
// userPassthrough handoff (identitygate.go's "exiting non-terminally for
// operator re-drive") ends the runner WITHOUT writing CR status: the choice
// lives only in the signed lifecycle log, and the operator has to reconcile
// once more to fold IdentityChoiceResolved and park AwaitingCredentials. In
// production the exiting runner Pod goes Succeeded, that status write is an
// Owns(&corev1.Pod{}) event, and the re-drive happens in milliseconds. In the
// harness the runner is a goroutine: it exits, f.running still holds its
// handle, the placeholder Pod never changes, and nothing at all reaches the
// operator — the session sits in AwaitingIdentityChoice indefinitely. Writing
// the terminal pod phase here restores production's trigger instead of faking
// one with a test-side poke.
//
// Best-effort by construction: a NotFound (Stop already deleted the Pod before
// the goroutine finished unwinding) is an expected race and must not be noisy.
// Any other error is logged rather than dropped, per AGENTS.md's
// no-silent-errors rule. Uses its own context: the runner's runCtx is already
// cancelled whenever the exit came from Stop.
func (f *InProcessRunnerFactory) markRunnerPodExited(podKey client.ObjectKey, sessKey string) {
	if f.K8s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var pod corev1.Pod
	if err := f.K8s.Get(ctx, podKey, &pod); err != nil {
		if !apierrors.IsNotFound(err) {
			slog.Default().Info("inprocess: get placeholder runner pod to mark exited failed (best-effort)",
				"session", sessKey, "err", err.Error())
		}
		return
	}
	if pod.Status.Phase == corev1.PodSucceeded {
		return
	}
	pod.Status.Phase = corev1.PodSucceeded
	if err := f.K8s.Status().Update(ctx, &pod); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		slog.Default().Info("inprocess: mark placeholder runner pod exited failed (best-effort)",
			"session", sessKey, "err", err.Error())
	}
}

// deleteRunnerPod removes the placeholder Pod ensureRunnerPodPresent
// created, if any. Best-effort: errors are swallowed (NotFound is expected
// whenever f.K8s is nil or Start's create never landed); Stop must remain
// idempotent and non-fatal regardless.
func (f *InProcessRunnerFactory) deleteRunnerPod(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) {
	if f.K8s == nil {
		return
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentsession.RunnerPodName(sess),
			Namespace: sess.Namespace,
		},
	}
	_ = f.K8s.Delete(ctx, pod)
}

// Shutdown stops the shared Apply-envelope subscriber goroutine. Called
// from the harness's cleanup chain so VerifyNone-style goroutine leak
// checks don't fire on the orphaned subscriber. Idempotent — calling it
// when no subscriber was ever installed is a no-op.
//
// Also sweeps any still-registered per-session subscribeToolSessionInput
// cancel funcs. Stop(sess) handles the per-session case during normal
// teardown, but a test that fails mid-run (or only partially tears down)
// can leave entries in toolSessionShutdowns whose goroutines would sit
// on <-ctx.Done() past the harness's NATS-conn drain. Lifting Stop's
// per-session cleanup into a full sweep keeps the goroutine-leak
// baseline stable even on the failure paths.
func (f *InProcessRunnerFactory) Shutdown() {
	// Cancel every runner still registered, INCLUDING ones the test never
	// Stopped. Shutdown is the harness's per-test cleanup, so it is the last
	// thing between one test's runner goroutine and the rest of the package —
	// and a test that fails an assertion mid-conversation returns without ever
	// reaching its Stop.
	//
	// A missed runner does not go quiet. It keeps turning against the envtest
	// apiserver that testenv.Start has just torn down, logging a failed status
	// patch per turn and burning CPU for the remainder of the package run, while
	// still holding its share of the package-wide SpiceDB. We found four such
	// sessions mid-run, advancing through turns 5..10 against seven dead
	// apiservers — enough drag that later tests missed 45s poll deadlines they
	// clear in ~1s when run alone. That is the "flaky e2e, just re-run it
	// isolated" symptom, and this is its source.
	//
	// Cancel under the lock, then wait outside it: a runner's own exit path can
	// call back into the factory, and holding f.mu across that would deadlock.
	f.mu.Lock()
	handles := make([]*runnerHandle, 0, len(f.running))
	for key, h := range f.running {
		h.cancel()
		handles = append(handles, h)
		delete(f.running, key)
	}
	f.mu.Unlock()
	for _, h := range handles {
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			// Best-effort: Shutdown has no error return and runs in cleanup, but
			// a runner that will not exit is exactly what this is here to catch,
			// so say so rather than move on quietly.
			fmt.Fprintf(os.Stderr, "InProcessRunnerFactory: runner did not exit within 5s of Shutdown; it may still be running\n")
		}
	}

	// Close every still-registered MCP session cache — see closeAllSessionCaches'
	// own doc for why Stop's per-session close is not enough here: a session
	// still running when Shutdown fires (the common case; Shutdown is cleanup,
	// not a per-test Stop) never went through Stop at all.
	f.closeAllSessionCaches()

	if f.applySubCancel != nil {
		f.applySubCancel()
		f.applySubCancel = nil
	}

	f.toolSessionShutdownMu.Lock()
	for key, cancel := range f.toolSessionShutdowns {
		cancel()
		delete(f.toolSessionShutdowns, key)
	}
	f.toolSessionShutdownMu.Unlock()

	// Close the lazily-started secret-output operator endpoint, if any.
	if f.secretOutSrv != nil {
		f.secretOutSrv.Close()
		f.secretOutSrv = nil
	}

	// Close the lazily-started preferences operator endpoint, if any.
	if f.prefsSrv != nil {
		f.prefsSrv.Close()
		f.prefsSrv = nil
	}
}

// secretOutPublisherFor lazily stands up the secret-output operator endpoint
// (an httptest server mounting secretoutsrv.NewHandler over f.K8s) and returns
// an HTTPPublisher scoped to this session, mirroring internal/cmd/runner/main.go's
// secretout.NewHTTPPublisher(memURL, ns, name, memToken). A fresh per-session
// token is minted and registered so the operator endpoint's per-session auth
// path is exercised. Returns nil when f.K8s is nil (no cluster to write the
// Secret into) — the runner then captures into the in-memory store only,
// leaving the value-free handle on the tool_result (the leak-safety invariant
// still holds, since it depends only on the runner scrub).
func (f *InProcessRunnerFactory) secretOutPublisherFor(sess *spiceboxv1alpha1.AgentSession) secretout.Publisher {
	if f.K8s == nil {
		return nil
	}
	f.secretOutOnce.Do(func() {
		f.secretOutTokens = tokens.NewRegistry()
		f.secretOutSrv = httptest.NewServer(secretoutsrv.NewHandler(f.K8s, f.secretOutTokens))
	})
	// Mint + register a per-session bearer token authorizing exactly this
	// {ns}/{name}. Deterministic so repeated buildLoop calls (restart) reuse it.
	token := "sotok-" + sess.Namespace + "-" + sess.Name
	f.secretOutTokens.Set(memory.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}, token, "")
	return secretout.NewHTTPPublisher(f.secretOutSrv.URL, sess.Namespace, sess.Name, token)
}

// preferencesClientFor lazily stands up the preferences operator endpoint (an
// httptest server mounting the REAL pkg/memory/httpsrv preferences routes —
// httpsrv.WithPreferences(f.K8s) — over f.MemStore, always non-nil per
// secretOutPublisherFor's sibling comment above) and returns an
// httpclient.Client bearer-scoped to this session.
//
// A real HTTP round trip through the real handler, not a hand-rolled
// stand-in: the interesting behavior — resolving the CURRENT TURN's author
// from the transcript, admin-global/lock precedence, the durable
// user_preference read — lives in that handler (pkg/memory/httpsrv/
// preferences.go), not in a thin data-plane pass-through, so anything less
// would prove nothing about it. Mirrors internal/cmd/runner/main.go's
// memHTTP := httpclient.New(memURL, memToken), scoped down to the one route
// this harness wires. Returns nil when f.K8s is nil (no cluster for the
// handler's AgentSession/AgentClass/AgentSettings lookups) — the preferences
// capability then skips with "preferences service not available", the same
// outcome a real deployment missing this wiring would report.
func (f *InProcessRunnerFactory) preferencesClientFor(sess *spiceboxv1alpha1.AgentSession) *httpclient.Client {
	if !f.ensurePrefsServer() {
		return nil
	}
	// Mint + register a per-session bearer token authorizing exactly this
	// {ns}/{name}, deterministic so repeated buildLoop calls (restart) reuse
	// it — same idiom as secretOutPublisherFor's token immediately above.
	token := "preftok-" + sess.Namespace + "-" + sess.Name
	f.prefsTokens.Set(memory.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}, token, "")
	return httpclient.New(f.prefsSrv.URL, token)
}

// preferencesChannelsdClient returns an httpclient.Client authorized as
// CHANNELSD ITSELF (a component-wide bearer, not a per-session one) against
// the same lazily-started preferences server preferencesClientFor uses — the
// route pkg/channels/channelsd/pipeline/preference_commit.go's
// PreferenceCommitter calls to commit a confirmed save. Mirrors
// internal/cmd/channelsd/main.go's `pl.PreferenceCommitter = mem.client`
// (channelsd's own memory httpclient.Client there too), scoped down the same
// way preferencesClientFor is. Returns nil when f.K8s is nil, in which case
// the caller (startChannelsdPlumbing) leaves user_preference_confirm unbound —
// the harness equivalent of a deployment missing this wiring.
func (f *InProcessRunnerFactory) preferencesChannelsdClient() *httpclient.Client {
	if !f.ensurePrefsServer() {
		return nil
	}
	const channelsdToken = "preftok-channelsd" //nolint:gosec // test-only bearer for an in-process httptest server
	f.prefsTokens.SetChannelsdToken(channelsdToken)
	return httpclient.New(f.prefsSrv.URL, channelsdToken)
}

// ensurePrefsServer lazily starts the shared preferences httptest server
// (used by both preferencesClientFor and preferencesChannelsdClient above)
// and reports whether it is now available. False only when f.K8s is nil.
//
// WithSubjectResolution/WithPreferenceAudit are wired here, together, the
// same "both or neither" pairing internal/cmd/operator/main.go documents at
// its own call site — a bundle exercising get_preferences' `user` argument
// needs both: resolution to answer who a reference names, the audit writer
// because the route refuses ?user-ref= outright without one. Each is nil-
// guarded independently (SpiceDB/OpSigned unset just means that half of the
// route degrades exactly as an under-configured real deployment would),
// but in practice every e2e.Start harness wires both.
func (f *InProcessRunnerFactory) ensurePrefsServer() bool {
	if f.K8s == nil {
		return false
	}
	f.prefsOnce.Do(func() {
		f.prefsTokens = tokens.NewRegistry()
		opts := []httpsrv.HandlerOption{httpsrv.WithPreferences(f.K8s)}
		if f.SpiceDB != nil {
			opts = append(opts, httpsrv.WithSubjectResolution(subjectResolveSpiceDBAdapter{cl: f.SpiceDB}))
		}
		if f.OpSigned != nil {
			opts = append(opts, httpsrv.WithPreferenceAudit(f.OpSigned))
		}
		mux := http.NewServeMux()
		mux.Handle("/", httpsrv.NewHandler(f.MemStore, f.prefsTokens, opts...))
		if f.GoalServer != nil {
			f.GoalServer.Tokens = f.prefsTokens
			mux.Handle("/goals/", f.GoalServer)
			if f.EventServer != nil {
				f.EventServer.Tokens = f.prefsTokens
				mux.Handle("/session-events/", f.EventServer)
			}
		} else if f.SpiceDB != nil {
			svc := &goalcore.Service{Store: goalinmem.New()}
			var keys provenance.PublisherKeyLookup
			if f.Tokens != nil {
				keys = f.Tokens
			}
			handler := &goalweb.Server{Keys: keys, Service: svc, Reader: f.K8s, Memory: f.MemStore, Tokens: f.prefsTokens, Auth: f.SpiceDB}
			svc.Auth = handler
			mux.Handle("/goals/", handler)
		}
		f.prefsSrv = httptest.NewServer(mux)
	})
	return true
}

// subjectResolveSpiceDBAdapter adapts *spicedb.Client to
// subjectresolve.RelationReader for the harness's preferences server —
// the exact same forwarding shape internal/cmd/operator's own
// subjectResolveSpiceDBAdapter uses (UserSubjects("<objType>", "<objID>",
// "<relation>") is the "<type>:<id>#<relation>" shape LookupSubjects already
// parses), duplicated rather than imported because the operator's version is
// unexported in package main.
type subjectResolveSpiceDBAdapter struct {
	cl *spicedb.Client
}

func (a subjectResolveSpiceDBAdapter) UserSubjects(ctx context.Context, objType, objID, relation string) ([]string, error) {
	return a.cl.LookupSubjects(ctx, objType+":"+objID+"#"+relation)
}

// buildLoop assembles the per-session runner.Loop. Mirrors the shape in
// internal/cmd/runner/main.go but uses the in-process helpers (LocalMemoryAdapter,
// LocalStatusPatcher / NewStatusPatcher) and only wires the auto-registered
// meta tools.
func (f *InProcessRunnerFactory) buildLoop(sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass, resolvedSidecars []spiceboxv1alpha1.ResolvedSidecarToolbox) (*runner.Loop, error) {
	if class == nil {
		return nil, fmt.Errorf("AgentClass is nil")
	}
	if f.LLM == nil {
		return nil, fmt.Errorf("LLM provider is nil")
	}

	// loopPtr is a pointer-to-pointer captured by the respond_to_user
	// tool's LeakageGate closure. The Loop is constructed AFTER the
	// tools list, so we can't reference it directly; capture this
	// indirection and assign loop just before return. Mirrors
	// internal/cmd/runner/main.go's `leakageGateFn` late-bind pattern.
	var loop *runner.Loop
	loopPtr := &loop

	// toolLookupFn is late-bound once tools (the final merged tool list,
	// below) is known — mirrors internal/cmd/runner/main.go's own toolLookupFn. env's
	// ToolLookup field below is a wrapper forwarding to this variable,
	// captured by credential_update's Offer during capability.Assemble while
	// still nil; only actually called at tool-execute time, well after the
	// late-bind assignment following Assemble.
	var toolLookupFn func(name string) (tool.Tool, bool)

	memKey := memory.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}

	var status *runner.StatusPatcher
	if f.K8s != nil {
		// f.K8s is the manager's CACHED client; production (internal/cmd/runner) uses a
		// direct one. RequestWake reads back the phase WriteIdle just wrote, and
		// a cached Get lags its own write — it would hand back the pre-Idle
		// phase and silently skip every post-idle wake, disabling the fix here
		// while production behaved correctly. Point that read at the uncached
		// APIReader so the harness matches production.
		status = runner.NewStatusPatcher(f.K8s, types.NamespacedName{
			Namespace: sess.Namespace, Name: sess.Name,
		}).WithDirectReader(f.APIReader)
	} else {
		status = runner.LocalStatusPatcher()
	}

	// LabelStore caches (resourceType, id) → friendly label tuples from MCP
	// tool responses for approval-time render substitution. Per-session,
	// threaded into every MCPTool below + onto the Loop. Constructed before the
	// non-meta tool builders because buildMCPTools writes into it.
	labelStore := runner.NewLabelStore(0)

	// One persistent MCP session per server URL for this AgentSession, mirroring
	// internal/cmd/runner/main.go: a stateful sidecar keys its per-session selection by the
	// MCP session id, so every tool call must ride the same session. Registered
	// per session and Closed in Stop.
	sessionCache := mcpprobe.NewSessionCache()
	f.registerSessionCache(sess, sessionCache)

	// agentUIRequestedTools is condition (1) of the three-way browser-tool
	// grant (runner.MaterializeAppTools / pkg/web/uigrant.Materialize) — mirrors
	// internal/cmd/runner/main.go's identical resolution. class.Spec.AgentUI
	// (condition 3, the deployment grant) is read directly off class below.
	// A class with no AgentUI grant at all never attempts a Get. Unlike
	// production, a missing f.K8s here is a HARNESS wiring bug (a fixture
	// that configured an AgentUI grant but didn't wire a K8s client) rather
	// than a real fail-closed scenario, so it errors loudly — mirroring
	// buildMCPTools's identical treatment of a nil f.K8s below.
	var agentUIRequestedTools []string
	agentUIKey := ""
	if class.Spec.AgentUI != nil {
		agentUIKey = sess.Namespace + "/" + class.Spec.AgentUI.Ref
		if f.K8s == nil {
			return nil, fmt.Errorf("buildLoop: K8s client is nil but AgentClass references an AgentUI (%s)", class.Spec.AgentUI.Ref)
		}
		tools, auiErr := runner.ResolveAgentUITools(context.Background(), f.K8s, sess.Namespace, class.Spec.AgentUI)
		if auiErr != nil {
			slog.Default().Info("e2e: resolve AgentUI for browser-tool grant errored; AppTools will be empty",
				"session", sess.Namespace+"/"+sess.Name,
				"agentClass", sess.Namespace+"/"+sess.Spec.Class,
				"agentUI", agentUIKey,
				"err", auiErr.Error())
		} else {
			agentUIRequestedTools = tools
		}
	}

	// authFailureByOrigin maps every credential-bearing tool origin to the
	// provider's declared authFailure: block for the credential-update
	// corroboration recorder. All three tool builders below contribute to it —
	// MCP, toolkit (sandbox), and sidecar — exactly as internal/cmd/runner/main.go's
	// three boot loops do. An origin kind missing here would make this harness
	// exercise a runner whose credentials of that kind are silently
	// uncorroborable, which production's would not be.
	authFailureByOrigin := runner.NewAuthFailureOrigins()

	// Non-meta tools (MCP / sandbox / sidecar / harness ExtraTools) are built
	// FIRST so they can be handed to capability.Assemble as NonMetaTools — the
	// registry appends them after the meta tools and lets the introspection
	// capability resolve over the union. Mirrors internal/cmd/runner/main.go, which
	// builds these before its Assemble call.
	//
	// MCP tools: synthesize one tool per allowlisted entry per
	// AgentClass.spec.mcpServers ref. The probe step hits the live MCP server
	// (MCPStub in tests) over HTTP, then mcpdispatch.Synthesize builds dispatch
	// closures the runner invokes via Tool.Execute. Failures surface as a
	// runner-startup error rather than a runner silently missing the MCP tools.
	mcpTools, mcpAppTools, mcpAppOrigins, err := f.buildMCPTools(sess, class, labelStore, status, authFailureByOrigin)
	if err != nil {
		return nil, err
	}

	// Sandbox tools: synthesize one Tool per toolspec per bundle from the
	// AgentSession's resolved bundle sessions. Mirrors internal/cmd/runner/main.go's
	// loop; trimmed to use the same f.K8s client and the in-memory builtin
	// toolkit set (registry+CR-backed toolkits are out of scope for this
	// factory — extend if a scenario requires them).
	//
	// bundleSessionMap is what SessionContext.BundleSessions expects:
	// bundle.name → SpiceboxSession.metadata.name. Populated alongside
	// tool synthesis so a bundle that fails to synthesize never leaks a
	// bundleSessionMap entry the runner could route a tool_use through.
	sandboxTools, bundleSessionMap, err := f.buildSandboxTools(sess, class, authFailureByOrigin)
	if err != nil {
		return nil, err
	}

	// Sidecar-toolbox tools: synthesize one Tool per allowlisted entry per
	// resolved sidecar, mirroring internal/cmd/runner/main.go's sidecar block. The
	// resolved snapshot is threaded in from Start (opts.ResolvedSidecars) —
	// the reconciler computes it and passes it on the same call, so it's the
	// authoritative source (vs. re-reading sess.Status, which may lag the
	// informer cache).
	sidecarTools, err := f.buildSidecarTools(resolvedSidecars, sessionCache, authFailureByOrigin)
	if err != nil {
		return nil, err
	}

	// nonMetaTools threads sandbox+mcp+sidecar+ExtraTools into Assemble in the
	// same order internal/cmd/runner/main.go uses (sandbox, mcp, sidecar). ExtraTools are
	// a harness-only seam (see the field doc); folding them into the non-meta
	// set keeps them in the LLM-facing catalog AND lets introspection see them,
	// exactly as production non-meta tools are treated.
	nonMetaTools := make([]tool.Tool, 0, len(sandboxTools)+len(mcpTools)+len(sidecarTools)+len(f.ExtraTools))
	nonMetaTools = append(nonMetaTools, sandboxTools...)
	nonMetaTools = append(nonMetaTools, mcpTools...)
	nonMetaTools = append(nonMetaTools, sidecarTools...)
	nonMetaTools = append(nonMetaTools, f.ExtraTools...)

	// A whole-session replay holds back the tools its capture ungated at the
	// fixture, so the composed surfaces (system prompt, introspect_tool's name
	// index, the plan-gate surface) describe the session that was recorded.
	// held rejoins the live tool set right after Assemble — that is where a
	// mid-session refresher's tools live, and it is what the captured run had.
	// Empty for every scenario but a replay; see the field docs.
	nonMetaTools, held := partitionHeldTools(nonMetaTools, f.HoldToolsFromAssembly)

	// bindingOrNil is the bound ChannelBinding when channel-attached, else nil;
	// capabilities gate their channel-only tools on Binding != nil.
	chanAttached := sess.Spec.InputChannel != nil
	var bindingOrNil *spiceboxv1alpha1.ChannelBinding
	if chanAttached {
		bindingOrNil = sess.Spec.InputChannel
	}

	// NATS publish/request closures backed by f.NATS. Both tolerate a nil
	// f.NATS by returning an error on use rather than silently claiming
	// delivery — the same fail-loud shape the pre-Assemble factory used for
	// respond_to_user / read_channel_history.
	nc := f.NATS
	publish := func(_ context.Context, subject string, payload []byte) error {
		if nc == nil {
			return fmt.Errorf("respond_to_user: NATS not wired into InProcessRunnerFactory")
		}
		return nc.Publish(subject, payload)
	}
	natsRequest := func(ctx context.Context, subject string, payload []byte) ([]byte, error) {
		if nc == nil {
			return nil, fmt.Errorf("read_channel_history: NATS not wired into InProcessRunnerFactory")
		}
		msg, rerr := nc.RequestWithContext(ctx, subject, payload)
		if rerr != nil {
			return nil, rerr
		}
		return msg.Data, nil
	}

	// Resolve the bound Channel/Secret/Kind ONCE — shared by every
	// channel-sourced capability (mention_lookup, channel_history) via
	// RunnerEnv.Resolved*/ResolveErr, mirroring internal/cmd/runner/main.go. Only
	// meaningful when channel-attached; a resolve error is non-fatal (those
	// capabilities decline), so warn once and carry it forward.
	var resolvedChannel *spiceboxv1alpha1.Channel
	var resolvedSecret *corev1.Secret
	var resolvedKind channelkinds.Kind
	var resolveErr error
	var subjectPrefix string
	if chanAttached {
		subjectPrefix = sess.Spec.InputChannel.NATSSubjectPrefix
		resolvedChannel, resolvedSecret, resolvedKind, resolveErr = resolve.ForSession(context.Background(), f.K8s, sess)
		if resolveErr != nil {
			slog.Warn("channel resolve failed; channel-sourced meta tools disabled",
				"session", sess.Namespace+"/"+sess.Name, "err", resolveErr.Error())
		}
	}

	// RunnerEnv built from the factory's fakes — the SAME shape internal/cmd/runner
	// populates (Task 9), so the two assembly paths cannot drift.
	// await pump: when AwaitIdleTTL is opted in (> 0), await_user_message parks
	// in-process on this channel until channelsd's wakeup fires (fed by
	// subscribeFactoryWake below). nil/zero otherwise → immediate idle-exit, the
	// path every other scenario relies on.
	var inboundCh chan struct{}
	if f.AwaitIdleTTL > 0 {
		inboundCh = make(chan struct{}, 1)
	}

	// uiViewRT is the runner-side handle backing update_view/read_view — nil
	// for the common case of a class that references no AgentUI, mirroring
	// internal/cmd/runner/main.go's identical construction. Client is set now (f.K8s is
	// already confirmed non-nil above when class.Spec.AgentUI != nil); Mem is
	// deferred — unlike internal/cmd/runner, this factory's signed memory facade
	// (memSigned) isn't built until AFTER capability.Assemble runs below, so it
	// is backfilled once memSigned exists, alongside AttachUIView. Base()
	// (the only method the two view capabilities call at assembly time, to
	// enumerate writable slots) only needs Client, never Mem, so the deferred
	// field is never read before it is filled in.
	// subagentTagMem backs delegate's `inputs`, and is DEFERRED for the same
	// reason uiViewRT.Mem is: this factory's signed facade (memSigned) is not
	// built until after capability.Assemble runs below, but the resolver is
	// captured into the DelegateConfig at assembly time. The closure reads the
	// variable, so filling it later is enough — and it fails closed if that
	// fill is ever removed, rather than resolving every slot to "no data",
	// which would read to a model as "that call produced nothing" and send it
	// to paste the data into the task text instead.
	var subagentTagMem memory.Memory

	var uiViewRT *uiview.Runtime
	if class.Spec.AgentUI != nil {
		uiViewRT = &uiview.Runtime{
			Namespace: sess.Namespace, Session: sess.Name, UIName: class.Spec.AgentUI.Ref,
			Client: f.K8s,
		}
	}

	// EffectiveSettings must resolve BEFORE the capability env is built: the
	// plan-gate mode is read from it, and the registry needs that mode to decide
	// whether to offer select_phase.
	//
	// If the reconciler already stamped the snapshot (normal case in the
	// harness) use it; otherwise resolve via the pure tier resolver so there is
	// one code path and no replicated merge logic.
	if sess.Status.EffectiveSettings == nil {
		e, _ := settings.Resolve(settings.Inputs{
			ClassModel: class.Spec.Model, ClassBudget: class.Spec.Budget, ClassAuthz: class.Spec.Authz,
			SessionBudget: sess.Spec.Budget,
			ForSession:    true,
		})
		st := e.ToStatus()
		sess.Status.EffectiveSettings = &st
	}

	// Harness signing identity. Mirrors internal/cmd/runner/main.go's
	// auditSeed: production reads the per-session Secret's audit-signing-key
	// seed (mounted at AUDIT_SIGNING_KEY_PATH) and derives BOTH its envelope
	// signer and its memory-provenance signer from it. This harness has no
	// filesystem mount, so it reads the SAME Secret directly — the one the
	// real AgentSession reconciler (pkg/controllers/agentsession) already
	// ensures and anchors onto status.AuditPublicKey/AuditKeyID
	// (syncAuditKeyStatus) before ever calling into this factory. That
	// anchor is why pk MUST come from the Secret rather than a fixed value
	// whenever one exists: channelsd's envelopeVerifier (Task 4) checks a
	// published envelope's sigKeyId against the LIVE session's
	// status.AuditKeyID, and the reconciler re-derives that status field
	// from the Secret on every reconcile (never from whatever a runner
	// wrote), so any identity this harness invents independently of the
	// Secret would only coincidentally match — and did not, in practice, the
	// first time this wiring landed (subagent-chat's reply_to_subagent send
	// was refused: sigKeyId from a fixed 0x07 seed vs. the reconciler's own
	// randomly-minted key).
	//
	// Falls back to a FIXED test seed (all-0x07) when f.K8s is nil (a bare
	// factory built by a package-local unit test with no controller running
	// at all) or the Secret doesn't exist/carry the key yet: there is no
	// K8s-anchored identity to match in that mode, so any deterministic key
	// will do, and the anchor block below establishes one. Every non-Secret
	// path below is LOGGED, never silently swallowed (AGENTS.md "never
	// silently drop errors"): a transient Get failure at the wrong moment
	// would otherwise mint a self-consistent non-Secret-backed key with
	// zero breadcrumb linking a later sigKeyId-mismatch refusal back here.
	var pk ed25519.PrivateKey
	if f.K8s != nil {
		var sec corev1.Secret
		secKey := client.ObjectKey{Namespace: sess.Namespace, Name: agentsession.MemoryTokenSecretName(sess)}
		if serr := f.credentialUpdateClient().Get(context.Background(), secKey, &sec); serr != nil {
			slog.Info("harness: could not read the per-session Secret's audit-signing-key; falling back to the fixed test seed",
				"session", sess.Namespace+"/"+sess.Name, "secret", secKey.String(), "secretKey", "audit-signing-key", "err", serr.Error())
		} else {
			// "audit-signing-key": pkg/controllers/agentsession/rbac.go's
			// agentSessionSecretAuditSigningKey, unexported there — this literal
			// is the Secret data key production's runner and reconciler agree on
			// (also podspec.go's SubPath, also documented on
			// AgentSession.Status.AuditPublicKey's own doc comment).
			switch seedHex := sec.Data["audit-signing-key"]; {
			case len(seedHex) == 0:
				slog.Info("harness: per-session Secret exists but carries no audit-signing-key entry; falling back to the fixed test seed",
					"session", sess.Namespace+"/"+sess.Name, "secret", secKey.String(), "secretKey", "audit-signing-key")
			default:
				seed, herr := hex.DecodeString(string(seedHex))
				switch {
				case herr != nil:
					slog.Info("harness: per-session Secret's audit-signing-key is not valid hex; falling back to the fixed test seed",
						"session", sess.Namespace+"/"+sess.Name, "secret", secKey.String(), "secretKey", "audit-signing-key", "err", herr.Error())
				case len(seed) != ed25519.SeedSize:
					slog.Info("harness: per-session Secret's audit-signing-key decodes to the wrong length; falling back to the fixed test seed",
						"session", sess.Namespace+"/"+sess.Name, "secret", secKey.String(), "secretKey", "audit-signing-key",
						"gotBytes", len(seed), "wantBytes", ed25519.SeedSize)
				default:
					pk = ed25519.NewKeyFromSeed(seed)
				}
			}
		}
	}
	if pk == nil {
		seed := make([]byte, ed25519.SeedSize)
		for i := range seed {
			seed[i] = 0x07
		}
		pk = ed25519.NewKeyFromSeed(seed)
	}
	pub := pk.Public().(ed25519.PublicKey)
	if f.Tokens != nil {
		f.Tokens.SetPublisherKey(provenance.SessionPublisher(sess.Namespace, sess.Name), provenance.KeyID(pub), pub)
	}
	envSigner, err := channelevents.NewEnvelopeSigner(pk, provenance.SessionPublisher(sess.Namespace, sess.Name), string(sess.UID))
	if err != nil {
		return nil, fmt.Errorf("harness envelope signer: %w", err)
	}
	// Anchor the pubkey on the session CR when it isn't already there. In the
	// common case (pk read from the real Secret above) this is a no-op:
	// status already carries the matching pubkey/keyID, derived from the
	// identical seed by the reconciler's own syncAuditKeyStatus. It only
	// does real work in the fallback modes above, so channelsd's verifier
	// still has an anchor to check the fixed-seed signature against.
	if f.K8s != nil && sess.Status.AuditPublicKey == "" {
		sess.Status.AuditPublicKey = base64.StdEncoding.EncodeToString(pub)
		sess.Status.AuditKeyID = provenance.KeyID(pub)
		if err := f.K8s.Status().Update(context.Background(), sess); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("anchor harness audit key on session status: %w", err)
			}
			// Best-effort: some low-level tests (e.g.
			// BuildLoopUserProfileWiringForTest) build a factory against a fake
			// client that never Create'd the AgentSession object at all —
			// exercising tool wiring in isolation, with no live session to
			// anchor onto. sess.Status carries the in-memory pubkey regardless;
			// the only reader that would ever notice the write didn't land is a
			// K8s-backed verifier (channelsd's envelopeVerifier), and it already
			// fails closed on a session it cannot Get at all.
			slog.Info("harness: session not found in K8s; audit key anchored in-memory only",
				"session", sess.Namespace+"/"+sess.Name)
		}
	}

	env := capability.RunnerEnv{
		NATSPublish: publish,
		// EnvelopeSigner signs every envelope a meta tool publishes with the
		// harness identity minted above, mirroring internal/cmd/runner/main.go's
		// identical field. Set here (not backfilled after Assemble) because
		// Assemble snapshots Env by value.
		EnvelopeSigner: envSigner,
		// Redirects the trigger-status tools' provider calls at a stand-in.
		// Empty in production and in every scenario that does not set it.
		TriggerStatusAPIBaseURL: f.TriggerStatusAPIBaseURL,
		// Completion requirements + the bypass recorder, mirroring
		// internal/cmd/runner/main.go: the class declares what a round must
		// produce, and a recorded override lands on session status AND on the
		// channel. Wired here rather than left nil so a scenario exercising the
		// gate sees the same two surfaces production does.
		CompletionRequirements: class.Spec.CompletionRequirements,
		RecordCompletionBypass: runner.CompletionBypassRecorder(
			status,
			func(subject string, b []byte) error { return publish(context.Background(), subject, b) },
			envSigner,
			channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
			func(msg string, kv ...any) { slog.Info(msg, kv...) },
		),
		// Plan gate, mirroring cmd/runner. `loop` is declared above and assigned
		// below; these closures fire at tool-execute time, long after that.
		PlanGateActive: sess.Status.EffectiveSettings.PlanGateActive(),
		FreezePhases: func(ctx context.Context, phases []plans.Phase) ([]string, error) {
			if loop == nil {
				return nil, fmt.Errorf("plan gate is not ready for this session")
			}
			var call meta.GoalsCaller
			if c := f.preferencesClientFor(sess); c != nil && f.SpiceDB != nil {
				call = func(ctx context.Context, r goalcore.Request) (goalcore.Response, error) {
					return c.Goals(ctx, sess.Namespace, sess.Name, r)
				}
			}
			authored, err := runner.PreparePlanReminders(ctx, phases, call, sess.Spec.GoalExecution == nil && sess.Spec.Parent == nil)
			if err != nil {
				return nil, err
			}
			notices, err := loop.FreezeAndRecordPhases(ctx, authored)
			if err != nil {
				return nil, err
			}
			return notices, loop.RequestPlanReminderApproval(ctx)
		},
		ActivePlan: func(ctx context.Context) (plangate.Plan, bool) {
			if loop == nil {
				return plangate.Plan{}, false
			}
			return loop.ActiveFrozenPlan(ctx)
		},
		RecordPhaseSelection: func(ctx context.Context, index int) error {
			if loop == nil {
				return fmt.Errorf("plan gate is not ready for this session")
			}
			return loop.RecordPhaseSelection(ctx, index)
		},
		RecordPhaseCompletion: func(ctx context.Context, index int, outcome string) error {
			if loop == nil {
				return fmt.Errorf("plan gate is not ready for this session")
			}
			return loop.RecordPhaseCompletion(ctx, index, outcome)
		},
		PhaseCompletion: func(ctx context.Context) map[int]bool {
			if loop == nil {
				return nil
			}
			return loop.PhaseCompletionMap(ctx)
		},
		PhaseEntered: func(ctx context.Context) map[int]bool {
			if loop == nil {
				return nil
			}
			return loop.PhaseEnteredMap(ctx)
		},
		PhaseEntries: func(ctx context.Context) map[int]int {
			if loop == nil {
				return nil
			}
			return loop.PhaseEntryCounts(ctx)
		},
		// UserPreferences mirrors internal/cmd/runner/main.go's identical field:
		// the class's own declared schema, needed by the capability's gate
		// (len(o.Env.UserPreferences) == 0 ⇒ feature absent) independent of
		// whether a reader/saver is wired below.
		UserPreferences: class.Spec.UserPreferences,
		// PreferencesReader backs the preferences capability's read tool, over
		// the REAL httpsrv preferences GET route — see preferencesClientFor.
		// turnIndex is late-bound like every other loop-dependent closure in
		// this Env: it reads `loop` only at CALL time, so a read always
		// reflects whatever turn is current when the tool actually runs.
		PreferencesReader: func() meta.PreferencesReader {
			c := f.preferencesClientFor(sess)
			if c == nil {
				return nil
			}
			return runner.NewPreferencesReader(c, sess.Namespace, sess.Name, func() int {
				if loop == nil {
					return -1
				}
				return loop.CurrentUserTurnIndex()
			})
		}(),
		// PreferenceSaver backs the preferences capability's save tool. Unlike
		// PreferencesReader, no HTTP client is needed here — Save publishes a
		// preference_save ask and blocks for the decision through the Loop's
		// own *runnerHost, mirroring internal/cmd/runner/main.go's
		// NewLateBoundPreferenceSaver(func() *runner.Loop { return
		// loopRef.Load() }) exactly, but reading the plain `loop` var this
		// harness threads through every other late-bound field in this Env
		// (FreezePhases, ActivePlan, ... above).
		PreferenceSaver: runner.NewLateBoundPreferenceSaver(func() *runner.Loop { return loop }),
		NATSRequest:     natsRequest,
		ChannelAttached: chanAttached,
		SubjectPrefix:   subjectPrefix,
		Client:          f.K8s,
		// resolve.ForSession result, shared across channel-sourced capabilities.
		ResolvedChannel: resolvedChannel,
		ResolvedSecret:  resolvedSecret,
		ResolvedKind:    resolvedKind,
		ResolveErr:      resolveErr,
		// Memory / search / knowledge are OPT-IN capabilities; the availability
		// flags must be HONEST about what the factory's fakes actually back:
		//   - MemoryAvailable=true: query_memory IS backed. The in-process runner
		//     always wires a usable memory facade below (loop.Mem = shared MemStore
		//     or a fresh per-session inmem), so a granted `memory` capability's
		//     query_memory works exactly as in production.
		//   - SearchAvailable tracks whether a SEARCHER is wired, which is the
		//     same question as "did this factory get the harness's shared
		//     facade?". e2e.Start builds that one with a searcher — the inmem
		//     provider, a CompositeSearcher over it, and a real SpiceDB
		//     authorizer (harness.go, memBackend/searchProvider/searcher) — so a
		//     granted `memory` capability is offered search_memory and the call
		//     returns rows. The fallback facade a few hundred lines below
		//     (memory.NewLocal over a bare inmem backend, for a factory built
		//     directly in a Go test with no MemStore) has NO searcher, and
		//     search_memory there would answer ErrNoSearchProviders on every
		//     call — so the flag follows f.MemStore rather than being a
		//     constant. It is deliberately NOT `true` unconditionally: offering
		//     a tool that cannot work is the dishonesty this block exists to
		//     prevent, in the other direction.
		//   - KGAvailable=false: loop.KG is never wired in this factory, so a
		//     granted `knowledge` capability's query_knowledge would fail on every
		//     call. Off → the knowledge capability gracefully skips (logged
		//     SkipReason) instead of injecting a broken tool.
		// Turn KG on here only once the factory grows a real KG client to back it.
		GoalsCaller: func() meta.GoalsCaller {
			c := f.preferencesClientFor(sess)
			if c == nil || f.SpiceDB == nil {
				return nil
			}
			return func(ctx context.Context, r goalcore.Request) (goalcore.Response, error) {
				return c.Goals(ctx, sess.Namespace, sess.Name, r)
			}
		}(),
		MemoryAvailable: true,
		SearchAvailable: f.MemStore != nil,
		KGAvailable:     false,
		// LeakageGate late-bound via loopPtr, mirroring the pre-Assemble factory:
		// the gate is a method on the Loop constructed below.
		LeakageGate: func(ctx context.Context, sctx *tool.SessionContext, text string, attachments []channelevents.AttachmentRef) error {
			if loopPtr == nil || *loopPtr == nil {
				return nil
			}
			return (*loopPtr).LeakageGateForRespondAdapter()(ctx, sctx, text, attachments)
		},
		// OnAwaitYield/Resume late-bound to the Loop for await_user_message
		// yield/resume signaling, mirroring internal/cmd/runner's loopRef closures.
		OnAwaitYield: func(ctx context.Context) {
			if loopPtr != nil && *loopPtr != nil {
				(*loopPtr).OnAwaitYield(ctx)
			}
		},
		OnAwaitResume: func(ctx context.Context) {
			if loopPtr != nil && *loopPtr != nil {
				(*loopPtr).OnAwaitResume(ctx)
			}
		},
		// ViewerCanInteract backs show_agent_ui's per-call check of whether
		// the participant being replied to may open the page. Late-bound
		// through loopPtr like the closures above — the Loop owns the current
		// speaker and is built below — and fired only at tool-execute time.
		//
		// f.SpiceDB is checked as the CONCRETE pointer, which is not the same
		// check CurrentSpeakerCanInteract already makes on its interface
		// parameter: a nil *spicedb.Client passed as an InteractChecker
		// becomes a NON-nil interface wrapping a nil pointer, so that guard
		// would pass and the call would dereference. Unlike internal/cmd/runner, where
		// SpiceDB is required and a connect failure aborts startup, a factory
		// here is routinely constructed with no SpiceDB at all.
		ViewerCanInteract: func(ctx context.Context) (bool, error) {
			if loopPtr == nil || *loopPtr == nil {
				return false, fmt.Errorf("show_agent_ui: session not ready")
			}
			if f.SpiceDB == nil {
				return false, fmt.Errorf("show_agent_ui: no authorization client")
			}
			return (*loopPtr).CurrentSpeakerCanInteract(ctx, f.SpiceDB, sess.Namespace, sess.Name)
		},
		// InboundCh / IdleTTL drive the in-process await park, opt-in via
		// AwaitIdleTTL (both zero → immediate idle-exit, the default path most
		// scenarios rely on; multi-turn continuity there uses session restart via
		// MemStore). Clock stays nil (real clock). AppendSystemNote stays nil (no
		// resume-dedupe in the e2e fixture). RenderFetch / MarkupGen stay unset —
		// no in-process fixture drives artifact_offer_view's preview path.
		// Artifacts / ArtifactReader are filled in below, conditionally on
		// f.ArtifactStore (SetArtifactStore) — the artifacts capability is
		// opt-in and most fixtures still grant it nothing.
		InboundCh: inboundCh,
		IdleTTL:   f.AwaitIdleTTL,
		// UIView backs the update_view/read_view capabilities — see uiViewRT's
		// construction above for why Mem is still nil at this point.
		UIView: uiViewRT,
		// CredentialUpdateClient mirrors internal/cmd/runner's own wiring, which hands
		// this a DIRECT (uncached) client on purpose — see DirectK8s for the
		// create-then-immediately-poll read this protects.
		CredentialUpdateClient: f.credentialUpdateClient(),
		// SubagentCreate/SubagentPoll back the delegate tool's create-then-poll
		// round trip against SubagentRequest, mirroring internal/cmd/runner's own
		// wiring. credentialUpdateClient() is reused despite its name — it is
		// just "the direct client if wired, else the cached one" and the same
		// create-then-immediately-poll staleness risk applies here.
		SubagentCreate: func(ctx context.Context, sr *spiceboxv1alpha1.SubagentRequest) error {
			// Owner-ref to the PARENT session, mirroring internal/cmd/runner's own
			// wiring — see meta.DelegateOwnerReference's doc for why: without it,
			// deleting the parent mid-delegation leaves the request (and any
			// already-created child) orphaned, with the child's standing
			// resolving to nobody.
			sr.OwnerReferences = append(sr.OwnerReferences, meta.DelegateOwnerReference(sess))
			return f.credentialUpdateClient().Create(ctx, sr)
		},
		SubagentPoll: func(ctx context.Context, srName string) (*spiceboxv1alpha1.SubagentRequest, error) {
			var sr spiceboxv1alpha1.SubagentRequest
			if err := f.credentialUpdateClient().Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: srName}, &sr); err != nil {
				return nil, err
			}
			return &sr, nil
		},
		// Shares meta.DefaultTimeout with NewDelegateTool's own zero-value
		// fallback and internal/cmd/runner's identical wiring, rather than
		// repeating the literal at three sites.
		SubagentTimeout: meta.DefaultTimeout,
		// SubagentSend backs reply_to_subagent, mirroring internal/cmd/runner:
		// one agent_message_send envelope on THIS session's own inbound
		// subject, naming the child in the payload, which channelsd delivers
		// through the Channel joining the pair. Nil when the harness wired no
		// NATS — the tool then refuses the call explicitly rather than
		// publishing into the void.
		SubagentSend: f.subagentSendFunc(sess, envSigner),
		// Backs delegate's `inputs` — see internal/cmd/runner/main.go for the
		// same wiring, and subagentTagMem's declaration for why it is filled
		// after assembly rather than here.
		SubagentResolveDataTag: func(ctx context.Context, toolUseID string) (string, error) {
			if subagentTagMem == nil {
				return "", fmt.Errorf("the harness never filled the provenance store for this session")
			}
			return runner.ResolveDataTagFor(subagentTagMem,
				memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name})(ctx, toolUseID)
		},
		// Backs send_input — see internal/cmd/runner/main.go. DirectK8s, like
		// the credential-update client beside it: this reads back a request
		// the same session just created, and a cached client can hand back the
		// pre-create view.
		SubagentBindDataSlot: runner.BindDataSlotFor(f.credentialUpdateClient(), sess.Namespace, sess.Name),
		// AskParent backs the child half, ask_parent. It records the question
		// on the child's OWN status; the SubagentRequest controller mirrors it
		// onto the request the parent polls. Nil in local mode (no CR to
		// record on), which the capability reports as a skip.
		//
		// Wrapped in the delegation's exchange budget, mirroring
		// internal/cmd/runner: a question the SubagentRequest controller would
		// refuse comes back to the child as a tool result in the same turn.
		// Without the wrapper here a scenario would see the controller's
		// refusal (which still happens) with the child parked instead of
		// continuing, which is not what production does.
		AskParent: runner.AskParentBudgetGuard(f.credentialUpdateClient(), sess, status.AskParent),
		// RequestInput — see internal/cmd/runner/main.go for the same wiring
		// and why it is unguarded where AskParent is not.
		RequestInput: func(ctx context.Context, slot, why string) error {
			_, err := status.RequestInput(ctx, slot, why)
			return err
		},
		// ToolLookup late-bound via toolLookupFn, mirroring internal/cmd/runner/main.go's
		// own wrapper: captured here (possibly nil) by credential_update's
		// Offer during capability.Assemble below, only actually called much
		// later at tool-execute time, by which point toolLookupFn is set.
		ToolLookup: func(name string) (tool.Tool, bool) {
			if toolLookupFn == nil {
				return nil, false
			}
			return toolLookupFn(name)
		},
	}
	if f.ArtifactStore != nil {
		// f.MemStore is always non-nil (set in harness.go's InProcessRunnerFactory
		// construction), so this mirrors internal/cmd/runner/main.go's artifactSvc :=
		// artifacts.NewService(memHTTP, nil) exactly — same constructor, an
		// in-process memory.Memory standing in for the HTTP-backed one.
		// The two options are no-ops when a scenario left the funcs nil, which
		// is every bundle but a replay: WithRenderNameMinter(nil) /
		// WithRevisionIDMinter(nil) store nil, and the Service's own nil checks
		// then take the production path. So this stays the same construction
		// internal/cmd/runner/main.go makes.
		env.Artifacts = artifacts.NewService(f.MemStore, f.NewArtifactID,
			artifacts.WithRenderNameMinter(f.NewRenderName),
			artifacts.WithRevisionIDMinter(f.NewRevisionID))
		env.ArtifactReader = files.StoreReader{Store: f.ArtifactStore}
	}

	// Assemble the complete tool list through the capability registry — the SAME
	// seam internal/cmd/runner drives. Each capability decides, from the AgentClass grant
	// (or default-on) plus runtime availability, whether it is active and which
	// meta tools it contributes. This replaces the former hand-rolled
	// meta.Load()/respond/read_channel_history assembly so this factory and the
	// real runner cannot drift.
	assembled := capability.AssembleAll(context.Background(), capability.AssembleDeps{
		Class:        class,
		Session:      sess,
		Binding:      bindingOrNil,
		Env:          env,
		NonMetaTools: nonMetaTools,
		Logger:       logr.FromSlogHandler(slog.Default().Handler()),
	})
	tools := assembled.Tools
	if sess.Spec.GoalExecution != nil {
		call := func(ctx context.Context, req goalcore.Request) (goalcore.Response, error) {
			return f.preferencesClientFor(sess).Goals(ctx, sess.Namespace, sess.Name, req)
		}
		tools = meta.BoundedGoalTools(tools, sess.Spec.GoalExecution.ConsentDigest, func(ctx context.Context) error {
			_, err := call(ctx, goalcore.Request{Operation: "authorize_execution"})
			return err
		}, call)
	}

	// A whole-session replay may CAN one of the assembled meta tools: its
	// Execute is swapped for a recorded reply and everything else about it —
	// name, kind, description, input schema, declared permission — is left as
	// assembled. Applied HERE, immediately after Assemble and before anything
	// reads the list, so exactly one list exists downstream; every surface
	// composed below is byte-identical either way, which is the property that
	// makes "only Execute changes" true rather than merely intended.
	//
	// Nil for every scenario but such a replay, in which case the list is
	// handed back untouched. See bt.MetaToolCanner and the rules in
	// bronzethread's metatool.go.
	if f.ReplaceAssembledTool != nil {
		for i, t := range tools {
			replaced, rerr := f.ReplaceAssembledTool(t)
			if rerr != nil {
				return nil, fmt.Errorf("inprocess: replacing assembled tool %d: %w", i, rerr)
			}
			tools[i] = replaced
		}
	}

	// toolLookupFn late-binds now that tools is final, mirroring
	// internal/cmd/runner/main.go's identical late-bind right after its own Assemble
	// call — so credential_update sees the SAME complete tool table in both
	// the e2e factory and production.
	toolLookupFn = tool.LookupByName(tools)

	// user_profile: mirrors internal/cmd/runner/main.go's identical call, ahead of
	// ComposeSystem, so the SAME profileActive value feeds both consumers: it
	// gates fetchProfile/profileFields on the Loop field assignment further
	// down, and it is passed straight into ComposeSystem to gate the
	// profile-marker prompt paragraph. One gate, two consumers (this factory +
	// internal/cmd/runner) — a second Offer call anywhere in either binary would risk
	// drifting from this decision.
	fetchProfile, profileFields, profileActive := userprofilegate.Offer(
		logr.FromSlogHandler(slog.Default().Handler()), class, resolvedKind, resolvedSecret)

	// Prompt scope: inline only. ConfigMap-backed resolves to empty —
	// see godoc on InProcessRunnerFactory.
	systemPrompt := class.Spec.SystemPrompt.Inline
	// Enumerated here rather than at its later use: the prompt needs the surface
	// to print the handle vocabulary, and a scenario whose prompt omits it would
	// exercise a different agent contract than production.
	planGateSurface := permsurface.Enumerate(tool.Candidates(tools))
	composeOpts := []runner.ComposeOption{
		runner.WithPlannableHandles(permsurface.Handles(planGateSurface)),
		runner.WithPlannableSurface(planGateSurface),
		runner.WithAuthoredExamples(runner.AuthoredExamplesOf(class)),
		runner.WithPlanningNotes(runner.PlanningNotesOf(class)),
		runner.WithUserProfileActive(profileActive),
	}
	if runner.PlanGateRequirePlan(sess.Status.EffectiveSettings) {
		composeOpts = append(composeOpts, runner.WithRequirePlan())
	}
	// mirrors internal/cmd/runner/main.go — the harness is a second wiring site.
	composeOpts = append(composeOpts, runner.WithPromptSections(assembled.Sections))
	composedSystem := runner.ComposeSystem(systemPrompt, tools, nil, nil, nil, nil, composeOpts...)

	// User prompt resolution mirrors the system prompt: inline only.
	userPrompt := sess.Spec.Prompt.Inline

	// Plan gate, mirroring cmd/runner. The mode comes from the RESOLVED
	// settings (which the block above guarantees exist), never from
	// class.Spec.GetAuthz() — otherwise the e2e suite would exercise a
	// different precedence than production and could not catch a clamp
	// regression. The plan is the synthesized one-phase session plan over the
	// live surface, so its ceiling equals today's. (Enumerated above, where the
	// prompt needs it.)

	// Memory: prefer the shared MemStore so the channelsd pipeline (which
	// writes inbound user turns into the same facade) and the runner see
	// the same transcript. Falls back to a fresh per-session in-memory
	// facade when MemStore is nil — adequate for kubectl-style cold
	// starts where the entire prompt rides through session.spec.prompt.
	memStore := f.MemStore
	if memStore == nil {
		memStore = memory.NewLocal(memoryinmem.NewBackend())
	}

	// Sign every append-only memory write the in-process runner makes,
	// mirroring internal/cmd/runner/main.go's memSigned wrapper. Reuses the
	// pk minted above (harness envelope signer + provenance audit signer
	// share one identity, mirroring production's single auditSeed feeding
	// both envSigner and the audit-log signer). Append-only writers below go
	// through memSigned; non-append-only Puts pass through unchanged.
	signer := provenance.NewSigner(pk, provenance.SessionPublisher(sess.Namespace, sess.Name))
	memSigned := provenance.NewSigningMemory(memStore, signer)

	// Backfill SetMemory onto every MCP and sandbox tool built above —
	// buildMCPTools/buildSandboxTools run before memSigned exists (same
	// reason env.UIView.Mem is backfilled below), so this is the earliest
	// point their SetRelWriter/SetSlotBoundChecker wiring can be paired with
	// SetMemory the way internal/cmd/runner/main.go pairs all three at once.
	// Without this, the harness would exercise a runner that differs from
	// production: neither dispatch path's relwrites_audit post-effect
	// (RecordWritten) nor its Observes post-effect would ever fire, silently,
	// for every e2e scenario.
	for _, t := range mcpTools {
		if mt, ok := t.(*mcpdispatch.MCPTool); ok {
			mt.SetMemory(memSigned)
		}
	}
	for _, t := range sandboxTools {
		if st, ok := t.(*sandbox.SandboxTool); ok {
			st.SetMemory(memSigned)
		}
	}

	// An inherited plan-gate ceiling raises this session's mode to enforcing,
	// exactly as cmd/runner does. Read here, once the memory facade exists,
	// because the answer lives in this session's own plan-gate log.
	planGateMode := sess.Status.EffectiveSettings.ResolvedPlanGateMode()
	if inherited, ierr := plangate.HasInheritedCeiling(
		memory.WithSystemApproval(context.Background(), "plan_gate"), memSigned,
		memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}); ierr == nil {
		planGateMode = plangate.EffectiveMode(planGateMode, inherited)
	}
	// The deferred fill promised at subagentTagMem's declaration.
	subagentTagMem = memSigned

	// ToolGuard: resolve the breaker/rate-limit policy from the per-tier
	// shapes the reconciler stamped onto status.effectiveSettings + the
	// AgentClass-level rules. Mirrors internal/cmd/runner/main.go 1:1 (default-on: the
	// builtin breaker rule applies even when no tier configures anything, so
	// every harness session runs with at least the builtin breaker —
	// threshold 5 / origin 10 / deny — exactly like production). A glob error
	// is a session-start config error; surface it as a buildLoop failure
	// (fail closed), matching the factory's idiom for fatal startup errors.
	var effTG *spiceboxv1alpha1.EffectiveToolGuard
	if sess.Status.EffectiveSettings != nil {
		effTG = sess.Status.EffectiveSettings.ToolGuard
	}
	tgTiers := toolguard.Tiers{Class: class.Spec.ToolGuard}
	if effTG != nil {
		tgTiers.Namespace, tgTiers.Cluster, tgTiers.Ceiling = effTG.Namespace, effTG.Cluster, effTG.Ceiling
	}
	toolGuardPolicy, err := toolguard.ResolvePolicy(tgTiers)
	if err != nil {
		return nil, fmt.Errorf("inprocess: resolve toolguard policy: %w", err)
	}

	// Mirror internal/cmd/runner/main.go: seed a RunClock from the session's persisted
	// run-time so budget accounting behaves the same under the in-process
	// harness (active run-time survives a simulated sleep/resume boundary).
	var e2eRunSeed time.Duration
	if sess.Status.RunDuration != nil {
		e2eRunSeed = sess.Status.RunDuration.Duration
	}
	runClock := runner.NewRunClock(e2eRunSeed, time.Now)
	var e2eSessionStartedAt time.Time
	if sess.Status.StartedAt != nil {
		e2eSessionStartedAt = sess.Status.StartedAt.Time
	}

	delegatedChild, err := spiceboxv1alpha1.IsDelegatedChild(sess, chregistry.AllowsSessionCounterparty)
	if err != nil {
		return nil, fmt.Errorf("inprocess: resolve whether this session is a delegated child: %w", err)
	}

	loop = &runner.Loop{
		Provider: f.LLM,
		Memory:   runner.LocalMemoryAdapter(memSigned, memKey),
		Status:   status,
		// EnvelopeSigner signs every envelope this Loop publishes with the
		// harness identity minted above, mirroring internal/cmd/runner/main.go's
		// identical field.
		EnvelopeSigner: envSigner,
		// The held tools rejoin HERE and nowhere above, which is precisely
		// where the mid-session refresher's tools live in a real run: offerable
		// and dispatchable, but absent from the system prompt, from
		// introspect_tool's name index, from the plan-gate surface and from the
		// credential_update lookup, all of which were composed before they
		// existed. A plain `tools` unless HoldToolsFromAssembly named something.
		Tools: append(slices.Clone(tools), held...),
		// AppTools is the separate MCP-UI app-visible-only registry — mirrors
		// internal/cmd/runner/main.go: NEVER folded into nonMetaTools/tools/l.Tools, so
		// buildToolDefs structurally never offers these to the LLM. Populated
		// but inert (no transport) in this phase. Runs through the exact same
		// runner.MaterializeAppTools choke point as internal/cmd/runner/main.go (the
		// fail-closed three-way grant: agentUIRequestedTools, mcpAppOrigins,
		// class.Spec.AgentUI) so this harness's enforcement cannot silently
		// drift from production's — see internal/cmd/runner/main.go:~1478.
		AppTools: appToolsByName(runner.MaterializeAppTools(slog.Default(), runner.AppToolsLogContext{
			Session:    sess.Namespace + "/" + sess.Name,
			AgentClass: sess.Namespace + "/" + sess.Spec.Class,
			AgentUI:    agentUIKey,
		}, agentUIRequestedTools, mcpAppOrigins, class.Spec.AgentUI, mcpAppTools)),

		// Plan gate. Slice 1 synthesizes one phase over the whole surface, so
		// the ceiling equals today's and the gate cannot change behavior.
		//
		// planGateMode, not the raw resolved mode: an inherited ceiling raises
		// it to enforcing (plangate.EffectiveMode). This harness builds its own
		// Loop, so a rule applied only in cmd/runner would be silently absent
		// from every e2e scenario — including the delegation ones that exist to
		// prove the ceiling is enforced.
		PlanGateMode:               planGateMode,
		PlanGatePlan:               plangate.SessionPlan(planGateSurface),
		PlanGateSurface:            planGateSurface,
		PlanGateSlotTypes:          class.Spec.SlotResourceTypes(),
		PlanGateSlotPermissions:    class.Spec.SlotPermissions(),
		PlanGateSlotPermissionSets: class.Spec.SlotPermissionSets(),
		PlanGateSlotTransforms:     runner.SlotTransformsOf(class),
		PlanGateSlotStanding:       runner.SlotStandingOf(class),
		PlanGatePermissionTitles:   runner.PermissionTitlesOf(class),
		PlanGateResourceDisplays:   runner.ResourceDisplaysOf(class),
		ResourceStandings:          runner.ResourceStandingsOf(class),
		PlanGateRequirePlan:        runner.PlanGateRequirePlan(sess.Status.EffectiveSettings),
		PlanGateMaxAutoApprove: func() int {
			if sess.Spec.GoalExecution != nil {
				return 0
			}
			return runner.PlanGateMaxAutoApprove(sess.Status.EffectiveSettings)
		}(),
		PlanApprovalDeriver: func() func(context.Context, plangate.Plan, int) (*plangateaudit.ApprovalAuthority, error) {
			if sess.Spec.GoalExecution == nil {
				return nil
			}
			return runner.GoalPlanApprovalDeriver(func(ctx context.Context, req goalcore.Request) (goalcore.Response, error) {
				return f.preferencesClientFor(sess).Goals(ctx, sess.Namespace, sess.Name, req)
			})
		}(),
		PlanGateMaxCardHandles: runner.PlanGateMaxCardHandles(sess.Status.EffectiveSettings),

		System:            composedSystem,
		UserPrompt:        userPrompt,
		Budget:            runner.NewBudget(sess.Status.EffectiveSettings.Budget, runClock, e2eSessionStartedAt),
		RunClock:          runClock,
		Model:             sess.Status.EffectiveSettings.Model.Name,
		ReportSessionCost: sess.Status.EffectiveSettings.ReportSessionCost,
		RecordSessionCost: sess.Spec.GoalExecution != nil,
		// Mirror internal/cmd/runner/main.go: the resolved catalog price flows to the cost
		// hook so the in-process runner's session cost is catalog-authoritative
		// (matches the admin dashboard). Without this the hook falls back to the
		// provider's built-in table and catalog-price e2e assertions fail.
		ModelInputPerMTok:  sess.Status.EffectiveSettings.ModelInputPerMTok,
		ModelOutputPerMTok: sess.Status.EffectiveSettings.ModelOutputPerMTok,
		// 65536 mirrors internal/cmd/runner/main.go — gives headroom for large
		// inline tool args (e.g. artifact_prepare payloads). Per-session
		// total spend is still bounded by Budget.
		MaxTokens:       65536,
		SessionKey:      memKey,
		ChannelAttached: sess.Spec.InputChannel != nil,
		// Mirrors internal/cmd/runner/main.go, through the same helper and the
		// same registry predicate: a delegated child's agent_work_complete
		// COMPLETES the session rather than parking it Idle, which is what
		// carries status.result back to the SubagentRequest its parent is
		// polling. Resolved above so a wiring error fails buildLoop rather
		// than silently deciding a delegation can never finish.
		DelegatedChild: delegatedChild,
		AgentName:      sess.Spec.Class,
		// Notify mirrors internal/cmd/runner/main.go's buildLoopNotify: publishes a
		// KindNotification envelope on the runner's NATS connection so the
		// (fake, in tests) channel kind's Sender renders it. Wiring this is
		// what makes runner-side Notify(...) calls — including the
		// post-session cost reporter's completion/failure message —
		// observable via the fake driver's Notifications().
		Notify: f.buildNotify(sess),

		// Slice 3: thread the live CR pointers so the loop's binding +
		// autofill paths can run. Without these, BoundEntities binding
		// is silently skipped (the centerdot fixture has one bound
		// entity for `crm_company`, but its defaults Check fires only
		// when AgentClass is non-nil here).
		AgentClass:   class,
		AgentSession: sess,

		// ArtifactReader backs the attachment-hydration pass's native-block
		// reads, mirroring internal/cmd/runner/main.go, which sets the SAME value here
		// and on capability.RunnerEnv.ArtifactReader (env, above) from one
		// construction. Left nil when no scenario called SetArtifactStore,
		// matching the production nil case (a kubectl-driven session with no
		// artifact store): hydration then degrades every attachment to
		// reference form. Wiring only env.ArtifactReader — as this factory did
		// before — would make fetch_artifact work in-process while silently
		// degrading every native block, so a scenario asserting an image
		// reaches the provider natively could never pass.
		ArtifactReader: env.ArtifactReader,

		// LabelStore is the same instance threaded into every MCPTool
		// in buildMCPTools above. The Loop's approval-emit path
		// snapshots it into the outbound ToolApprovalRequestPayload.
		LabelStore: labelStore,

		// ToolGuardPolicy is the session-start-resolved breaker/rate-limit
		// policy (default-on via the builtin rule). Drives the guard hooks in
		// the runner pipeline — mirrors internal/cmd/runner/main.go so every harness
		// scenario runs with at least the builtin breaker, like production.
		ToolGuardPolicy: toolGuardPolicy,

		// AuthFailures mirrors internal/cmd/runner/main.go's identical wiring: the
		// credential-update corroboration recorder, over the SAME StatusPatcher
		// this factory already uses for every other runner-owned status
		// observation, the SAME per-origin authFailure: shapes the MCP boot loop
		// resolved, and seeded from the SAME status field. A recorder present in
		// only one of the two sites would make this harness exercise a runner
		// that differs from the one production ships.
		AuthFailures: authfail.New(status, authFailureByOrigin.Lookup, sess.Status.CredentialAuthFailures),

		// Representative non-nil key; ask and check sides share it so
		// keyed-args-hash approval flows work end-to-end in harness tests.
		ArgsHashKey: []byte("e2e-harness-args-hash-key-32by!!"),
	}

	// Backfill env.UIView's Mem (deferred until memSigned existed — see its
	// construction above) and attach it to the now-live Loop. Both steps
	// belong here, together: AttachUIView's own doc comment requires the Loop
	// to already exist, and nothing calls into the Runtime between its
	// construction and here (a meta tool's Execute only runs inside the loop).
	//
	// Reads env.UIView, deliberately NOT the uiViewRT local: mirrors
	// internal/cmd/runner/main.go's identical guard. If the `UIView: uiViewRT` field
	// on the env literal above were ever deleted, uiViewRT would have no
	// remaining use anywhere in this function and go build would fail on
	// "declared and not used" — turning a silent capability black hole into
	// a compile error rather than a gap no test in this repo catches. Do not
	// reintroduce a bare uiViewRT read here.
	if env.UIView != nil {
		env.UIView.Mem = memSigned
		runner.AttachUIView(env.UIView, loop)
	}

	// user_profile: fetchProfile/profileFields/profileActive were resolved by
	// userprofilegate.Offer above, mirroring internal/cmd/runner/main.go's identical
	// post-construction assignment. A nil FetchSpeakerProfile is the Loop's
	// own "not granted" state, so leave both fields zero when declined.
	if profileActive {
		loop.FetchSpeakerProfile = fetchProfile
		loop.SpeakerProfileFields = profileFields
	}

	// ContentGuard: wire test-injected inspectors when set via SetContentInspectors.
	// Mirrors the Minter seam: empty = existing tests unaffected.
	loop.ContentInspectors = f.ContentInspectors
	loop.ContentInspectorIDs = f.ContentInspectorIDs

	// Identity-choice gate (identityMode=ask|dynamic). Mirrors internal/cmd/runner/main.go:
	//   - IdentityGatePending registers the SessionStart IdentityChoiceGate only
	//     for a first-boot ask|dynamic session (effectiveIdentityMode unset); a
	//     re-spawn past the choice or a static class leaves it false.
	//   - IdentityChoiceTimeout bounds the human-choice wait; read from the
	//     AgentClass so the runner's live wait and the operator's deadline backstop
	//     measure the same window (0 ⇒ the gate's package default).
	//   - IdentityRecommender is the isolated advisory LLM that distinguishes
	//     dynamic from a plain ask; wired ONLY for dynamic (ask leaves it nil ⇒
	//     "no recommendation"). Injected by tests via SetIdentityRecommender.
	loop.IdentityGatePending = identityGatePending(sess, class)
	if class.Spec.IdentityChoiceTimeout != nil {
		loop.IdentityChoiceTimeout = class.Spec.IdentityChoiceTimeout.Duration
	}
	if class.Spec.IdentityMode == spiceboxv1alpha1.IdentityModeDynamic {
		loop.IdentityRecommender = f.IdentityRecommender
	}

	// SessionContext: sandbox tools dereference K8sClient, BundleSessions,
	// AgentSessionUID and Operations on Execute (see
	// pkg/agent/tool/sandbox/sandbox_tool.go). Wire them so a tool_use
	// against a synthesized sandbox tool reaches the ToolCall controller
	// instead of erroring on a nil SessionContext field.
	//
	// Always constructed (even without bundles) so SecretOut is set: the
	// runner's applySecretOutput reads sess.SecretOut, and a nil store turns
	// a producer's secret-output into a "no store configured" error. Mirrors
	// internal/cmd/runner/main.go, which always sets SessionContext.SecretOut.
	//
	// SubmitResult and Memory are populated inside Loop.Run; the runner
	// loop owns those bindings since they need to capture per-loop state.
	// Operations is only initialized when bundle sessions are present —
	// sandbox tools require operation tracking, but non-bundle sessions
	// (MCP-only, sidecar-only) leave it nil so MCP dispatch and other
	// consumers skip the operation-id requirement check.
	sctx := &tool.SessionContext{
		Namespace:        sess.Namespace,
		Name:             sess.Name,
		AgentSessionUID:  sess.UID,
		IsDelegatedChild: sess.Spec.Parent != nil,
		K8sClient:        f.K8s,
		SecretOut:        secretout.NewSessionStore(string(sess.UID)),
	}
	if len(bundleSessionMap) > 0 {
		sctx.BundleSessions = bundleSessionMap
		sctx.Operations = operations.New(nil, f.NewOperationID)
	}
	// How a sandbox tool reads back the stdout/stderr the ToolCall controller
	// stored: status carries refs, not bytes. Production wires this to the
	// operator's artifact endpoint (internal/cmd/runner/main.go's
	// NewHTTPArtifactClient over the one cluster-wide store); in-process the
	// same store is reachable directly.
	//
	// Assigned into an `any` field, so it is guarded rather than set
	// unconditionally: a typed-nil artifactstore.Store wrapped in this
	// interface-typed field would be non-nil to the `art == nil` check inside
	// readArtifact and panic on the method call (AGENTS.md's typed-nil rule).
	if f.ToolCallArtifacts != nil {
		sctx.ArtifactClient = storeArtifactClient{store: f.ToolCallArtifacts}
	}
	// Wire the session-state registry (plans, ...) so state tools like
	// update_plan work in-process. Mirrors internal/cmd/runner/main.go: the plans store
	// persists wrapped "plans" system_notes via AppendSystemNote into the same
	// signed memory the runner reads (so a resumed session reconstructs them).
	//
	// The state registry SHARES sctx.Operations when it exists (bundle sessions),
	// so an update_plan-minted operation_id resolves in a downstream sandbox/MCP
	// call exactly as production does (internal/cmd/runner uses one ops instance for both).
	// Non-bundle sessions keep sctx.Operations nil — MCP dispatch must skip the
	// operation-id requirement (see the block above) — but the plans store still
	// needs an Operations for pending->in_progress transitions, so it gets a
	// standalone one (a non-bundle session has no sandbox tool to chain into).
	stateOps := sctx.Operations
	if stateOps == nil {
		stateOps = operations.New(nil, f.NewOperationID)
	}
	stateScope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
	sctx.State = state.NewRegistry(state.Deps{
		Operations:       stateOps,
		AppendSystemNote: runner.AppendSystemNoteFunc(turn.NewAppender(memSigned, stateScope)),
	})
	loop.SessionContext = sctx

	// Mem is the extensible-memory facade the runner's authz + toolguard hooks
	// read/write (toolguard_audit kind, breaker-status). internal/cmd/runner/main.go sets
	// loop.Mem unconditionally; set it here too so the toolguard audit trail
	// works in scenarios without SpiceDB (the authz block below re-assigns the
	// same store, which is harmless). nil-safe consumers degrade to best-effort.
	//
	// Wrapped in sysApprovedMem (see its own doc comment) rather than the bare
	// memSigned: this harness wires the runner straight to the shared
	// *memory.Local, bypassing the operator httpsrv that mints a per-request
	// capability approval in production — and some Loop.Mem writes run from a
	// callback with no request ctx to inherit an approval from at all (e.g.
	// uiActionRecorder.onApproval, which is invoked from a SpiceDB-approval
	// observer with a bare context.Background() by design — see that method's
	// own doc comment). A ctx-threading fix at every Loop.Mem CALL SITE cannot
	// reach that one; wrapping the STORE itself can.
	loop.Mem = sysApprovedMem{inner: memSigned}

	// LifecycleMemory is the session-signing facade the runner's sequencer Puts
	// typed lifecycle transition events through (publisher session:<ns/name>) —
	// same signing memory as Mem, mirroring internal/cmd/runner/main.go's
	// LifecycleMemory: memSigned. Without it the runner's emitLifecycleEvent
	// (IdentityChoicePending on the identity gate, IdentityChoiceResolved on the
	// answer, IdleYield / terminal at end-of-turn) is a silent no-op — the
	// AppendLog effect short-circuits on the nil facade — and the operator, which
	// derives fold-only phases like AwaitingIdentityChoice by re-reading this same
	// shared log (see the operator Reconciler's LifecycleMemory in harness.go and
	// the harness note in identity_choice_test.go), has nothing to project from.
	//
	// Scoped to identity-choice first-boot sessions only (identityGatePending),
	// same as the placeholder-Pod wiring above. Wiring LifecycleMemory also arms
	// claimAndRecover's fold-on-every-Run (pkg/agent/runner/sequencer.go): each
	// Loop.Run — including every cold-start respawn a channel-attached session
	// does between turns — reads back the full signed transition log and
	// re-arms any ReissuePending decision waits before the turn even starts.
	// That's a real per-respawn memory read + fold that non-identity sessions
	// never needed pre-Task-10; on the tool-approval TIMEOUT scenarios (a tight
	// ~3s approval deadline inside a 30s test budget) the added latency was
	// enough to turn a reliable ~14s pass into an intermittent 40s+ timeout
	// under suite contention (test/e2e/scenarios/centerdot/contacts_owner_timeout).
	// Gating it keeps non-identity sessions on the pre-Task-10 (nil
	// LifecycleMemory, fold short-circuits) code path.
	if identityGatePending(sess, class) || sess.Spec.GoalExecution != nil {
		loop.LifecycleMemory = memSigned
	}

	// SecretOutPublisher: ships captured secret values to the operator's
	// /secret-output endpoint so they land in the per-session Secret and the
	// handle is recorded on status. Mirrors internal/cmd/runner/main.go's
	// loop.SecretOutPublisher = secretout.NewHTTPPublisher(...). nil when
	// f.K8s is unset (no cluster) — the in-memory capture + scrub still run.
	loop.SecretOutPublisher = f.secretOutPublisherFor(sess)

	// Hold a whole-session replay to the tool catalog its captured run
	// recorded. nil for every other scenario and for production; see the field
	// doc on runner.Loop.
	loop.ReplayToolCatalog = f.FilterOfferedTools

	// Mid-session sidecar re-synthesis. Mirrors internal/cmd/runner/main.go's
	// loop.ToolRefresher = newSidecarToolRefresher(...) — the headline
	// secret-gated-sidecar flow: the agent runs a producer tool that emits the
	// gating secret MID-session, the operator (here: the harness's
	// MarkSidecarReady stamper) brings the secret-gated sidecar "pod" up, and on
	// a LATER turn the runner notices it Ready in status, synthesizes its tools,
	// and adds them to the live set WITHOUT restarting.
	//
	// Unlike internal/cmd/runner (which arms the refresher only when a separate-pod
	// sidecar already exists in status at boot), the in-process harness stamps
	// the sidecar into status mid-session, so there's nothing to gate on at boot.
	// We arm it whenever K8s is wired; the refresher itself is cheap (re-Gets the
	// session, returns nil unless a newly-ready separate-pod sidecar appears).
	if f.K8s != nil {
		loop.ToolRefresher = f.newSidecarToolRefresher(types.NamespacedName{
			Namespace: sess.Namespace, Name: sess.Name,
		}, sessionCache)
	}

	// Authz wiring: enables per-tool SpiceDB Checks, the approval flow
	// (publish interaction_request on Check denial, resume on
	// interaction_applied), and JIT writesRelationships effects.
	// Skipped when SpiceDB is nil — the runner then runs every tool
	// unchecked, which is fine for the no-authz smoke tests but breaks
	// T14's canonical scenario.
	if f.SpiceDB != nil {
		loop.AuthzCli = f.SpiceDB
		loop.AuthzCache = toolcheck.NewZedTokenCache()
		// Engine: the single authz aggregator the runner routes CheckToolCall
		// AND the slot binding lifecycle (BindClassDefaults, PromoteExtractedSlots,
		// PromoteObservedSlots) through. Mirrors internal/cmd/runner/main.go's
		// loop.Engine = engine.New(...). Without it, loop.promoteObservedSlots
		// returns at its `l.Engine == nil` guard (pkg/agent/runner/loop_autofill.go),
		// so no fillFrom:[observed] binding is ever exercised and a fork bundle
		// asserting on that binding would pass while testing nothing.
		//
		// Memory is the RAW signing memory (memSigned), NOT loop.Mem: loop.Mem is
		// sysApprovedMem-wrapped, and the engine's slot-lifecycle writes reach the
		// memory capability doors with the runner goroutine's ctx, which already
		// carries memory.WithSystemApproval (see Start's runCtx) — the harness
		// equivalent of production's httpsrv per-request mint. SessionExpiration
		// mirrors main.go: slot grants expire with the session's wall-clock cap,
		// not its active run-time budget.
		loop.Engine = engine.New(engine.Deps{
			ToolChecker:            toolcheck.Checker{Cli: f.SpiceDB, Cache: loop.AuthzCache},
			SessionInteractChecker: f.SpiceDB,
			Granter:                f.SpiceDB,
			RelWriter:              f.SpiceDB.Relations(),
			SlotLister:             f.SpiceDB,
			Lookuper:               f.SpiceDB,
			Memory:                 memSigned,
			SessionExpiration:      sess.Status.EffectiveSettings.Budget.SessionExpiration.Duration,
		})
		loop.ResolveAuthSubjects(sess, class)
		loop.ToolAuthMode = class.Spec.GetAuthz().GetToolCalls().Mode

		// Approval orchestrator: the runner blocks on this when a
		// tool's Check denies. The shared NATS subscriber (installed
		// once via ensureApplySubscriber) routes incoming Apply
		// envelopes into the per-session orchestrator's Deliver.
		loop.Approval = approval.New()
		loop.StartedByCanonical = spiceboxv1alpha1.ResolveStartedByCanonical(sess)
		if sess.Spec.InputChannel != nil {
			loop.ChannelKind = sess.Spec.InputChannel.Kind
		}
		loop.GuardianGrantWriter = f.SpiceDB.Writer(grants.Source)
		loop.SlotRevoker = f.SpiceDB.Relations()
		// RelationsWithFloor mirrors cmd/runner/main.go: advance the session
		// ZedToken floor to each in-process slot-grant write so the ToolCallAuthz
		// check that immediately follows a plan-gate approval/amendment sees it,
		// instead of reading a pre-write snapshot off the warm cache.
		loop.SlotBinder = f.SpiceDB.RelationsWithFloor(loop.AdvanceAuthzFloor)
		// Approver standing for the plan gate's slot requests — mirrors
		// cmd/runner/main.go, FullyConsistent on both for the same reason.
		//
		// Wired here because leaving them nil is not "one less thing to set up":
		// the filter that bounds a plan-gate approval to what its approver can
		// personally speak for reads nil as "everything is delegable", so every
		// bundle asserting on a plan-gate approval passed with the control
		// switched off — green under a wiring production does not have, which is
		// the one thing an e2e harness must never be.
		loop.SpiceDBHasOnResource = func(ctx context.Context, resourceType, resourceID, permission string, canonicalID identity.CanonicalUserID) (bool, error) {
			return f.SpiceDB.CheckOnResource(ctx, resourceType, resourceID, permission, canonicalID, true)
		}
		loop.SpiceDBHasAnyOfType = func(ctx context.Context, resourceType, permission string, canonicalID identity.CanonicalUserID) (bool, error) {
			return f.SpiceDB.HasAnyOfType(ctx, resourceType, permission, canonicalID, true)
		}

		// Per-datum provenance (pt-tag). The runner mints a tag at
		// PostToolCall for every declared read; the operator normally serves
		// that as POST /memory/_pttag_mint. Here the Minter is constructed IN
		// PROCESS — the same shortcut the memory facade above takes. What a
		// bundle asserts on is the TAG (its derived reader set, its
		// untrusted-origin flag), and that is the minter's output whether or
		// not an HTTP hop sits in front of it; standing up the operator's HTTP
		// surface here would exercise the transport, not the lattice.
		//
		// Signed as the OPERATOR publisher, NOT through memSigned. pt_tag is
		// append-only and component-written, and the property that makes the
		// lattice mean anything is that a session can never author its own
		// audience — `direct_reader` GRANTS disclosure. Reusing the session
		// signer here would make the harness green under a wiring production
		// forbids, which is the one thing an e2e harness must never be.
		ptTagSeed := make([]byte, ed25519.SeedSize)
		for i := range ptTagSeed {
			ptTagSeed[i] = 0x09
		}
		ptTagPriv := ed25519.NewKeyFromSeed(ptTagSeed)
		ptTagPub := ptTagPriv.Public().(ed25519.PublicKey)
		if f.Tokens != nil {
			f.Tokens.SetPublisherKey(ptTagOperatorPublisher, provenance.KeyID(ptTagPub), ptTagPub)
		}
		minter := &pttagmint.Minter{
			Subjects: f.SpiceDB,
			Rels:     f.SpiceDB.Writer(pttagmint.Source),
			Mem: provenance.NewSigningMemory(memStore,
				provenance.NewSigner(ptTagPriv, ptTagOperatorPublisher)),
		}
		ptTagScope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
		loop.PtTagMint = func(ctx context.Context, req memory.PtTagMintRequest) (string, error) {
			// Stamp the per-session token exactly as the production memory HTTP
			// door does (httpsrv.go:321) before it reaches the minter. Skipping
			// the HTTP hop here is deliberate (see the block above), but skipping
			// its AUTHORIZATION CONTEXT is not: without the token the per-kind
			// write door short-circuits to "not token-originated" and never
			// runs, so a component-write regression — a session token reaching a
			// pt_tag Put — ships green. That is precisely how the live mint
			// failure ("pt_tag is component-written") passed every bundle. With
			// the token present, the minter must hand off authorship
			// (WithoutTokenSession) before writing, and this harness proves it.
			ctx = memory.WithTokenSession(ctx, memory.NamespacedName{
				Namespace: sess.Namespace, Name: sess.Name,
			})
			return minter.MintPtTag(ctx, ptTagScope, req)
		}
		// PtTagVerify mirrors the operator's _pttag_verify route in-process: read
		// the content store COMPONENT-side (pt_tag_content is component-read, so a
		// token-originated read is refused by design) and bind the regions with
		// the same shared pttagcontent.Bind the route uses. Skipping the HTTP hop
		// is deliberate (see the mint block); the component-read semantics are
		// not. Without this seam the runner would read pt_tag_content directly and
		// fall to the coarse floor on a real cluster — the live-only failure this
		// harness now exercises.
		loop.PtTagVerify = func(ctx context.Context, regions []memory.PtTagRegion) (memory.PtTagVerifyResponse, error) {
			cctx := memory.WithSystemApproval(memory.WithoutTokenSession(ctx), "pt_egress_verify")
			recs, err := pttagcontent.List(cctx, memStore, ptTagScope)
			if err != nil {
				return memory.PtTagVerifyResponse{}, err
			}
			return pttagcontent.Bind(recs, regions), nil
		}

		// The trifecta reads the class's OWN mode, never toolCalls.mode: a
		// control that switches off with an unrelated permission check is not
		// a control. Mirrors internal/cmd/runner/main.go.
		loop.TrifectaMode = class.Spec.GetAuthz().GetTrifecta().Mode
		// A LIVE lookup, not a start-time snapshot — a tag can be bound into
		// this session mid-run (a parent answering ask_parent), and legs
		// derived from a stale list judge the session on data it no longer
		// holds.
		loop.TrifectaBoundTags = func(ctx context.Context) ([]string, error) {
			bindings, err := f.SpiceDB.ListDataSlotGrants(ctx, sess.Namespace, sess.Name)
			if err != nil {
				return nil, err
			}
			tags := make([]string, 0, len(bindings))
			for _, b := range bindings {
				tags = append(tags, b.TagID)
			}
			return tags, nil
		}
		// Data slots — see internal/cmd/runner/main.go for the same wiring. The
		// runner asks the operator's _pttag_resolve route (pt_tag_content is
		// component-read); this closure is that route in-process — component read
		// of the parent scope + the SAME pt_tag#access gate and shared
		// pttagcontent.ResolveEntitled, so a child only receives tags its
		// granted_to binding entitles it to. Skipping the HTTP hop is deliberate;
		// skipping the entitlement gate is not.
		if sess.Spec.Parent != nil {
			parentScope := memory.Scope{
				Kind: "session",
				ID:   sess.Spec.Parent.Namespace + "/" + sess.Spec.Parent.Name,
			}
			childRef := "agentsession:" + sess.Namespace + "/" + sess.Name
			tagAccess := leakagewiring.NewSpiceDBCheck(f.SpiceDB)
			resolve := func(ctx context.Context, scope memory.Scope, tagIDs []string) ([]memory.PtTagContent, error) {
				cctx := memory.WithSystemApproval(memory.WithoutTokenSession(ctx), "pt_resolve")
				recs, rerr := pttagcontent.List(cctx, memStore, scope)
				if rerr != nil {
					return nil, rerr
				}
				return pttagcontent.ResolveEntitled(recs, tagIDs, func(tagID string) (bool, error) {
					return tagAccess(ctx, childRef, "access", "pt_tag:"+tagID)
				})
			}
			loop.ResolveBoundSlots = runner.ResolveBoundSlotsFor(resolve, parentScope,
				func(ctx context.Context) ([]authz.DataSlotBinding, error) {
					return f.SpiceDB.ListDataSlotGrants(ctx, sess.Namespace, sess.Name)
				})
		}
		loop.TrifectaDeps = trifecta.Deps{
			TagCarriesUntrusted: f.SpiceDB.TagCarriesUntrusted,
			TagReaders:          f.SpiceDB.TagReaders,
			ChildAudience:       f.SpiceDB.SessionReadTranscriptAudience,
		}
		// Reads THIS session's stamped closure verdict. The operator derives
		// and stamps it; the runner only reads the answer, because its Role
		// pins agentsessions to its own name with no list verb.
		loop.SessionStatusReader = func(ctx context.Context) (bool, error) {
			var cur spiceboxv1alpha1.AgentSession
			if err := f.K8s.Get(ctx, client.ObjectKey{
				Namespace: sess.Namespace, Name: sess.Name,
			}, &cur); err != nil {
				return false, err
			}
			return cur.Status.ClosureDenied != nil && *cur.Status.ClosureDenied, nil
		}

		if f.NATS != nil {
			nc := f.NATS
			// Match production: timeout reports pass through channelsd's canonical
			// terminal-transition path before any surface receives an outcome.
			loop.TimeoutAppliedPublish = func(_ context.Context, envNS, envName string, env channelevents.Envelope) error {
				return runner.PublishTimeoutApplied(nc.Publish, nil, envNS, envName, env)
			}

			f.registerOrchestrator(sess, loop.Approval)

			// The applied-answer back-channel is wired for EVERY session with NATS,
			// NOT only channel-attached ones. plan_phase / plan_amendment /
			// tool_approval / info_leakage decisions all route on the session PREFIX,
			// so a HEADLESS delegated child that raises a plan-gate approval must
			// receive its own resolved KindInteractionApplied here or its gate's
			// Await never unblocks and the child hangs until the class budget
			// expires. This mirrors internal/cmd/runner/main.go, which wires
			// subscribeInteractionApplied in the NATS-gated (channel-or-headless)
			// block precisely because "approvals route on the session prefix, so
			// they must not be gated on chanAttached." Gating it on InputChannel (as
			// this once did) silently stranded every headless child that hit the
			// plan gate — invisible until a bundle ran an enforcing delegated child.
			icSubCtx, icSubCancel := context.WithCancel(context.Background())
			go subscribeFactoryInteractionApplied(icSubCtx, nc, loop.Approval, sess.Namespace, sess.Name)
			f.registerToolSessionShutdown(sess, icSubCancel)

			// Identity-choice PUBLISH, by contrast, stays gated on channel
			// attachment (not just NATS): an ask|dynamic session with no input
			// channel must leave IdentityChoicePublish nil so the runner gate fails
			// closed (the non-interactive guard) — it must NEVER silently run as the
			// agent. Mirrors internal/cmd/runner/main.go, which only sets
			// IdentityChoicePublish inside its chanAttached block. The publish
			// targets the OUT subject directly (like production): the runner's gate
			// hands a pre-built KindInteractionRequest envelope (identity_choice is a
			// category on the generic interaction model), and the outbound relay
			// routes it to the fake "interaction" sub-channel sender for capture.
			if sess.Spec.InputChannel != nil {
				loop.IdentityChoicePublish = func(_ context.Context, envNS, envName string, env channelevents.Envelope) error {
					body, merr := json.Marshal(env)
					if merr != nil {
						return fmt.Errorf("marshal identity choice envelope: %w", merr)
					}
					subject := channelevents.SubjectOut(
						channelevents.SubjectPrefix(envNS, envName), env.Kind)
					return nc.Publish(subject, body)
				}
			}
		}

		// Info-leakage gate wiring. Mirrors internal/cmd/runner/main.go's wiring
		// 1:1 via the shared pkg/agent/runner/leakagewiring helpers — when this
		// diverges from the production path, the e2e leakage tests stop
		// catching the production wiring bugs they're meant to.
		loop.LeakageConfig = class.Spec.GetAuthz().InformationLeakage
		// Mem is the extensible-memory facade the runner's authz hooks read
		// directly (mirrors internal/cmd/runner/main.go's loop.Mem = memHTTP). The
		// InfoLeakAudience hook's durable approve/deny decision store
		// (infoleakage_decision kind) is wired off l.Mem; without this the
		// approve/deny state silently no-ops and the respond-time gate
		// re-prompts.
		//
		// sysApprovedMem-wrapped — see the identical, earlier loop.Mem
		// assignment's doc comment for why the bare memSigned isn't enough.
		// This branch re-assigns the SAME field, so it must carry the SAME
		// wrapper or a reachable session would silently lose it.
		loop.Mem = sysApprovedMem{inner: memSigned}
		scope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
		loop.LookupToolMapping = func(toolName string) *spiceboxv1alpha1.ToolResourceMapping {
			return leakagewiring.LookupMCPToolResourceMapping(
				context.Background(), f.K8s, sess.Namespace, class.Spec.MCPServers, toolName)
		}
		// Leg A's input — see internal/cmd/runner/main.go for the same wiring.
		loop.LookupToolUntrustedSource = func(toolName string) bool {
			return leakagewiring.LookupMCPToolUntrustedSource(
				context.Background(), f.K8s, sess.Namespace, class.Spec.MCPServers, toolName)
		}
		loop.TaintMemoryAppend = func(ctx context.Context, rec infoleakagetaint.TaintRecord) error {
			return infoleakagetaint.Append(ctx, memSigned, scope, rec)
		}
		loop.TaintMemoryList = func(ctx context.Context) ([]infoleakagetaint.TaintRecord, error) {
			return infoleakagetaint.List(ctx, memSigned, scope)
		}
		loop.AuditMemoryAppend = func(ctx context.Context, rec infoleakageaudit.AuditRecord) error {
			return infoleakageaudit.Append(ctx, memSigned, scope, rec)
		}
		loop.RequesterCanonicalID = func(_ context.Context, perCall identity.CanonicalUserID) (identity.Subject, error) {
			// Mirrors internal/cmd/runner: render the PER-CALL principal, never
			// loop.AuthSubject() — see Loop.RequesterCanonicalID.
			return identity.Subject(leakagewiring.RequesterSubjectRef(perCall.String())), nil
		}
		loop.SpiceDBCheck = leakagewiring.NewSpiceDBCheck(f.SpiceDB)
		loop.SpiceDBLookupSubjects = func(ctx context.Context, resource, permission string) ([]string, error) {
			return f.SpiceDB.LookupSubjects(ctx, resource+"#"+permission)
		}
		loop.LeakageGrantWriter = func(ctx context.Context, sessionNs, sessionName string, audience []string, resources []approval.LeakageGrantResource, ttl time.Duration) (string, error) {
			return approval.WriteInfoLeakageGrants(ctx, f.SpiceDB.Writer(approval.LeakageSource), sessionNs, sessionName, audience, resources, ttl)
		}
		// ChannelKindImpl: resolve the channel kind from the session's
		// input channel and bind its AudienceResolver. Non-fatal: a
		// kind without AudienceResolver yields nil (gate treats as
		// CapabilityUnsupported).
		if sess.Spec.InputChannel != nil {
			if _, _, k, err := resolve.ForSession(context.Background(), f.K8s, sess); err == nil {
				if ar, ok := k.(channelkinds.AudienceResolver); ok {
					loop.ChannelKindImpl = ar
				}
			}
		}
		// Subscribe to the runner-side wake-back: channelsd publishes
		// out.info_leakage_approval_applied after the approver acts.
		if f.NATS != nil {
			nc := f.NATS
			subCtx, subCancel := context.WithCancel(context.Background())
			go subscribeFactoryLeakageApplied(subCtx, nc, loop.Approval, sess.Namespace, sess.Name)
			f.registerToolSessionShutdown(sess, subCancel)

			// InteractionRequestPublish: content_inspection migrated onto the
			// generic Interaction model in Slice C1 (its gate now builds a
			// KindInteractionRequest via buildContentInspectionPending). IN-subject
			// routing: channelsd subscribes to in.interaction_request, parks the
			// session + re-emits on OUT for the generic "interaction" sub-channel
			// sender. Without this hook buildContentInspectionPending fails closed
			// ("interaction publish hook not configured") and the approve action
			// can never raise a prompt. Mirrors internal/cmd/runner/main.go's
			// loop.InteractionRequestPublish. Resume is handled by the generic
			// subscribeFactoryInteractionApplied (launched in the NATS block above
			// for EVERY session, headless children included), which now routes
			// content_inspection just like production's handleInteractionAppliedMessage.
			loop.InteractionRequestPublish = func(_ context.Context, envNS, envName string, env channelevents.Envelope) error {
				body, merr := json.Marshal(env)
				if merr != nil {
					return fmt.Errorf("marshal interaction request envelope: %w", merr)
				}
				subject := channelevents.SubjectIn(
					channelevents.SubjectPrefix(envNS, envName), env.Kind)
				return nc.Publish(subject, body)
			}
		}
	}

	// Interactive tool sessions: wire a per-session tool-session bridge
	// registry + inbound subscriber, and build the InteractiveHooks that
	// the runner attaches to each tool-dispatch context. Mirrors
	// internal/cmd/runner/main.go's wiring; the only divergence is that we share
	// the harness's NATS connection (vs. the runner's natsRuntime
	// wrapper) and stash the subscribe-goroutine cancel on the factory
	// so Stop can drain it per session.
	//
	// Gated on (NATS configured && session has an InputChannel) for the
	// same reason as InteractionRequestPublish above: without a channel
	// there's no transport to stream tool I/O over, and the runner-side
	// InteractiveHooks check keeps the dispatch path safe whether we
	// wire this or not.
	if f.NATS != nil && sess.Spec.InputChannel != nil {
		toolSessionReg := newToolSessionRegistry()
		subCtx, subCancel := context.WithCancel(context.Background())
		go subscribeToolSessionInput(subCtx, f.NATS, toolSessionReg)
		f.registerToolSessionShutdown(sess, subCancel)

		// Channel-initiated mid-turn interrupt: mirrors internal/cmd/runner/main.go's
		// `go subscribeInterruptRequest(rootCtx, natsRT, loop, ns, name)`, gated
		// there on (chanAttached && natsRT != nil) — the same condition as this
		// block, not SpiceDB. Without this the fake kind's queued_messages
		// interrupt-click round-trip (test/e2e's Interrupt helper) can publish
		// KindInterruptRequest all day and nothing ever calls loop.Interrupt or
		// answers with KindInterruptApplied.
		interruptSubCtx, interruptSubCancel := context.WithCancel(context.Background())
		go subscribeFactoryInterruptRequest(interruptSubCtx, f.NATS, loop, sess.Namespace, sess.Name)
		f.registerToolSessionShutdown(sess, interruptSubCancel)

		// UIPublish mirrors internal/cmd/runner/main.go's identical wiring, gated there
		// on (chanAttached && natsRT != nil) — the same condition as this
		// block. Delivers ui_action_update (uiActionRecorder) and
		// ui_view_update (uiview.Runtime.Write, via env.UIView.Publish above)
		// pushes to the fake channel kind's live-socket relay; a scenario with
		// no InputChannel or no NATS leaves it nil, which both callers already
		// treat as memory-only degradation, not an error.
		loop.UIPublish = func(_ context.Context, envNS, envName string, env channelevents.Envelope) error {
			body, merr := json.Marshal(env)
			if merr != nil {
				return fmt.Errorf("marshal %s envelope: %w", env.Kind, merr)
			}
			subject := channelevents.SubjectOut(channelevents.SubjectPrefix(envNS, envName), env.Kind)
			return f.NATS.Publish(subject, body)
		}

		// Await pump: when opted in (inboundCh != nil), forward channelsd's
		// KindUserMessage wakeup into the runner's await InboundCh so a follow-up
		// message resumes a parked await_user_message IN-PROCESS — the real path a
		// live conversation takes, and the one the budget-blowup incident hit.
		if inboundCh != nil {
			wakeSubCtx, wakeSubCancel := context.WithCancel(context.Background())
			go subscribeFactoryWake(wakeSubCtx, f.NATS, inboundCh, sess.Namespace, sess.Name)
			f.registerToolSessionShutdown(sess, wakeSubCancel)
		}

		// Agent-UI data-binding + action responders: a browser's declared
		// "tool"-sourced binding (pkg/web/webui/agentui's bindingsHandler, via
		// pkg/web/uibindings/tool) and a declared action (actionsHandler) both
		// round-trip webd->runner over these two subjects and block for the
		// synchronous reply. Mirrors internal/cmd/runner/main.go's unconditional
		// `go subscribeUIDataBinding(rootCtx, natsRT, loop, spdbCli, ns, name)` /
		// `go subscribeUIAction(...)` inside its own chanAttached block — this is
		// the harness's equivalent wiring, operating against the factory's
		// nats.Conn directly (the runner side wraps it in a natsRuntime which
		// isn't accessible to the factory). Without this, test/e2e's agent-UI
		// browser arm (agentui_browser.go) can open the page and reach the
		// bindings/actions HTTP routes, but every "tool" binding resolves to a
		// permanent timeout: nothing on the runner side ever answers.
		//
		// Neither ctx here carries memory.WithSystemApproval (unlike runCtx in
		// Start) — deliberately not fixed at this layer. loop.Mem itself is
		// wrapped in sysApprovedMem below, which is the fix this needed: the
		// action responder's uiActionRecorder writes a ui_action record via a
		// SpiceDB-approval-observer callback (onApproval) that mints its own
		// bare context.Background() with no ctx to inherit from (see that
		// method's own doc comment) — no ctx threaded onto this subscription
		// goroutine could ever reach it anyway.
		uiSubCtx, uiSubCancel := context.WithCancel(context.Background())
		go subscribeFactoryUIDataBinding(uiSubCtx, f.NATS, loop, f.SpiceDB, sess.Namespace, sess.Name)
		f.registerToolSessionShutdown(sess, uiSubCancel)
		uiActionSubCtx, uiActionSubCancel := context.WithCancel(context.Background())
		go subscribeFactoryUIAction(uiActionSubCtx, f.NATS, loop, f.SpiceDB, sess.Namespace, sess.Name)
		f.registerToolSessionShutdown(sess, uiActionSubCancel)

		nc := f.NATS
		pub := func(_ context.Context, subject string, data []byte) error {
			return nc.Publish(subject, data)
		}
		hooksNS, hooksName := sess.Namespace, sess.Name
		// KEEP IN SYNC WITH internal/cmd/runner/main.go
		loop.InteractiveHooks = &sandbox.InteractiveHooks{
			IdleTimeoutDefault: 15 * time.Minute,
			MaxDurationDefault: 0, // unbounded → controller's interactiveSafetyCeiling
			OnOutput: func(toolCallRef, stream string, data []byte) {
				if err := channelevents.PublishOut(
					func(subject string, b []byte) error { return pub(context.Background(), subject, b) },
					hooksNS, hooksName, channelevents.KindToolSessionDelta,
					channelevents.ToolSessionDeltaPayload{
						ToolCallRef: toolCallRef, Stream: stream, Data: data,
					},
				); err != nil {
					fmt.Fprintf(os.Stderr,
						"InProcessRunnerFactory: publish tool_session_delta for %s/%s toolCallRef=%s: %v\n",
						hooksNS, hooksName, toolCallRef, err)
				}
			},
			OnTerminal: func(toolCallRef, exitReason string, exitCode int32) {
				if err := channelevents.PublishOut(
					func(subject string, b []byte) error { return pub(context.Background(), subject, b) },
					hooksNS, hooksName, channelevents.KindToolSessionDelta,
					channelevents.ToolSessionDeltaPayload{
						ToolCallRef: toolCallRef, Stream: "stdout",
						Terminal: true, ExitReason: exitReason, ExitCode: exitCode,
					},
				); err != nil {
					fmt.Fprintf(os.Stderr,
						"InProcessRunnerFactory: publish tool_session_delta (terminal) for %s/%s toolCallRef=%s: %v\n",
						hooksNS, hooksName, toolCallRef, err)
				}
			},
			// OnEvent mirrors internal/cmd/runner/main.go's buildToolSessionEventPublisher:
			// when an interactive toolkit declares a streamFormat, the sandbox tool
			// wraps stdout through the registered parser and fires OnEvent per
			// parsed event. The harness publishes them as KindToolSessionEvent so
			// the e2e scenario can assert on them via the OUT subject.
			//
			// Inlined here (not delegated to buildToolSessionEventPublisher)
			// because the harness's pub callback has a different shape — the
			// helper captures a context.Context up front, while this factory
			// uses context.Background() per-call — and we log failures to
			// stderr via fmt.Fprintf rather than slog/besteffort.Log so test
			// output stays grep-friendly without slog handler setup. The
			// payload mapping is identical; mirror changes there.
			OnEvent: func(toolCallRef, reason, outerTool string, ev toolkitstream.Event) {
				if err := channelevents.PublishOut(
					func(subject string, b []byte) error { return pub(context.Background(), subject, b) },
					hooksNS, hooksName, channelevents.KindToolSessionEvent,
					channelevents.ToolSessionEventPayload{
						ToolCallRef: toolCallRef,
						Reason:      reason,
						OuterTool:   outerTool,
						EventType:   string(ev.Type),
						Text:        ev.Text,
						ToolName:    ev.ToolName,
						ToolID:      ev.ToolID,
						Summary:     ev.Summary,
						OK:          ev.OK,
						DurationMs:  ev.DurationMs,
						CostUSD:     ev.CostUSD,
					},
				); err != nil {
					fmt.Fprintf(os.Stderr,
						"InProcessRunnerFactory: publish tool_session_event for %s/%s toolCallRef=%s eventType=%s: %v\n",
						hooksNS, hooksName, toolCallRef, ev.Type, err)
				}
			},
			Register: toolSessionReg.register,
		}
		if f.BridgeDialOpt != nil {
			loop.InteractiveHooks.BridgeDialOpts = append(loop.InteractiveHooks.BridgeDialOpts, f.BridgeDialOpt)
		}
	}

	return loop, nil
}

// buildNotify mirrors internal/cmd/runner/main.go's buildLoopNotify: returns a
// function that publishes a KindNotification envelope on the harness's NATS
// connection so the channel kind's Sender (the fake kind in tests) renders
// it. Returns nil when the session has no InputChannel or NATS isn't wired —
// Loop.Run tolerates a nil Notify by skipping the call, same as production.
func (f *InProcessRunnerFactory) buildNotify(sess *spiceboxv1alpha1.AgentSession) func(ctx context.Context, text string) {
	if f.NATS == nil || sess.Spec.InputChannel == nil {
		return nil
	}
	nc := f.NATS
	ns, name := sess.Namespace, sess.Name
	return func(ctx context.Context, text string) {
		err := channelevents.PublishOut(
			func(subject string, data []byte) error { return nc.Publish(subject, data) },
			ns, name,
			channelevents.KindNotification,
			channelevents.NotificationPayload{Text: text},
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "InProcessRunnerFactory: publish notification for %s/%s: %v\n", ns, name, err)
		}
	}
}

// newSlotBoundChecker builds the read side of the slot-bound relwrites gate
// for one session, mirroring internal/cmd/runner/main.go. Both tool builders
// below call it, so the harness's Loop gates a requireSlotBound block exactly
// the way the production runner does.
//
// f.SpiceDB is checked as the CONCRETE pointer and NOT left to
// relwrites.NewSlotBoundChecker's own nil guard, which tests its INTERFACE
// parameter: a nil *spicedb.Client passed as a relwrites.SlotGrantLister
// becomes a non-nil interface wrapping a nil pointer, so that guard would pass
// and the first ListSlotGrants would dereference. Unlike the runner, where
// SpiceDB is required and a connect failure aborts startup, a factory here is
// routinely constructed with no SpiceDB at all — and the right answer then is a
// nil checker, which relwrites.Run refuses a marked block against, loudly.
func (f *InProcessRunnerFactory) newSlotBoundChecker(sess *spiceboxv1alpha1.AgentSession) relwrites.SlotBoundChecker {
	if f.SpiceDB == nil {
		return nil
	}
	return relwrites.NewSlotBoundChecker(f.SpiceDB, sess.Namespace, sess.Name)
}

// buildMCPTools synthesizes one tool.Tool per allowlisted MCPServer entry
// for the class. Mirrors the relevant subset of internal/cmd/runner/main.go's MCP
// loop with the production-only pieces (status writes on probe failure)
// trimmed: failures surface as a buildLoop error.
// The MCP probe is bounded by a 10s timeout to keep a slow MCPStub from
// hanging the test forever.
//
// Auth header: resolved via the real broker path (inproc.Broker backed by
// f.K8s), exactly as internal/cmd/runner/main.go does. This means the AgentIdentity
// credential referenced by MCPServer.spec.auth.credential is read from the
// in-process envtest Secret store and projected into the Authorization header.
// When the MCPServer declares no credential, mt.SetAuth is called with empty
// strings (unauthenticated server), matching production behavior.
//
// status is threaded through so a wired UseTokenGate (TokenAuthz=true) can
// fail the session closed on an indeterminate use_token check, mirroring
// internal/cmd/runner/main.go's statusPatcher.WriteFailed FailSession closure.
// buildMCPTools returns three values, mirroring internal/cmd/runner/main.go's split:
// the LLM-visible tools (offered to the model), MCP-UI app-visible-only
// tools (mcpdispatch.SynthesizeResult.AppTools), and the per-origin
// uigrant.Origin data condition (2) of the three-way browser-tool grant
// needs (runner.AppToolOrigin) — the caller runs both app-tool sets through
// runner.MaterializeAppTools before wiring Loop.AppTools, never Tools/
// NonMetaTools.
//
// It also records each MCP origin's auth-failure shape into authFailureByOrigin
// — the shared, session-scoped map the credential-update corroboration recorder
// classifies against. It is passed IN rather than returned because all three
// tool builders (MCP, sandbox, sidecar) contribute origins to the one map, as
// internal/cmd/runner/main.go's three boot loops do.
func (f *InProcessRunnerFactory) buildMCPTools(sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass, labelStore *runner.LabelStore, status *runner.StatusPatcher, authFailureByOrigin runner.AuthFailureOrigins) ([]tool.Tool, []tool.Tool, []uigrant.Origin, error) {
	if len(class.Spec.MCPServers) == 0 {
		return nil, nil, nil, nil
	}
	if f.K8s == nil {
		return nil, nil, nil, fmt.Errorf("buildMCPTools: K8s client is nil but AgentClass references %d MCPServer(s)",
			len(class.Spec.MCPServers))
	}
	ctx := context.Background()

	var out []tool.Tool
	var outApp []tool.Tool
	// outOrigins captures, per MCPServer processed below, condition (2) of
	// the three-way browser-tool grant (runner.AppToolOrigin) — mirrors
	// internal/cmd/runner/main.go's mcpAppOrigins accumulation.
	var outOrigins []uigrant.Origin
	// relWriter is shared across every synthesized tool — its
	// SpiceDB-backed Writer issues a single batched WriteRelationships
	// RPC per Execute, so per-tool instances would not buy anything.
	// Nil-safe: when SpiceDB is unset, MCPTool.SetRelWriter(nil) is a
	// no-op and the post-effect step is silently skipped.
	var relWriter relwrites.Writer
	if f.SpiceDB != nil {
		relWriter = &relwrites.SpiceDBWriter{
			Client: f.SpiceDB.Writer(relwrites.Source),
		}
	}
	// Read side of the slot-bound gate, mirroring internal/cmd/runner/main.go.
	// This harness is a SECOND wiring site with its own Loop, so a checker
	// wired only in the runner would leave every scenario here running against
	// a nil one — and relwrites.Run refuses a marked block against nil, so a
	// bundle meant to prove the gate ALLOWS a bound instance would fail for a
	// reason nobody would connect to wiring. NewSlotBoundChecker returns nil
	// when SpiceDB is unset, which is that same fail-closed refusal.
	slotBoundChecker := f.newSlotBoundChecker(sess)

	// Durable per-call authorization key: the operator's
	// reconcileCredentialGrants binds externaltoken grant value hashes under
	// the per-session args-hash-key Secret it mints before RunnerFactory.Start
	// (agentsession/controller.go's argsHashKeyBytes hoist +
	// credential_grants.go). Read it once here so every MCPTool's
	// UseTokenGate presents hashes computed under the SAME key the grant was
	// written with — a mismatched key fails every check closed. Only read
	// when TokenAuthz is opted in: reconcileCredentialGrants only runs (and
	// only then is this Secret guaranteed to already exist) when the paired
	// AgentSession reconciler TokenGranter is wired, which the harness gates
	// on the identical Options.WithTokenAuthz flag.
	var useTokenHMACKey []byte
	if f.TokenAuthz && f.SpiceDB != nil {
		k, kerr := f.mcpArgsHashKey(ctx, sess)
		if kerr != nil {
			return nil, nil, nil, fmt.Errorf("buildMCPTools: %w", kerr)
		}
		useTokenHMACKey = k
	}

	// Resolve the RuntimeIdentity used by credresolve.Descriptors for every
	// MCP server in this loop. Branch on AgentClass.spec.identityMode to mirror
	// internal/cmd/runner/main.go's identity-mode branching (commit 2d7ef619):
	//
	//   - userPassthrough: read the same-name SessionUserIdentity in the
	//     session's namespace; the operator creates it before un-parking
	//     the session, so its absence is a setup bug here.
	//   - agent (default/unset): load the class-level AgentIdentity. An
	//     empty agentIdentity field means no credentials; leave the
	//     identity zero-valued so credresolve.Descriptors resolves no
	//     descriptors for unauthenticated servers (the mcp kind emits no
	//     requirements) and errors loudly when a credential IS required.
	var runtimeIdentity runner.RuntimeIdentity
	// Consult the EFFECTIVE mode, not spec.identityMode: an ask|dynamic session
	// whose resolved choice was userPassthrough must read the SessionUserIdentity
	// here on re-spawn, and a first-boot ask|dynamic session resolves as the
	// provisional agent. Mirrors internal/cmd/runner/main.go's effectiveMode switch.
	switch effectiveIdentityMode(sess, class) {
	case spiceboxv1alpha1.IdentityModeUserPassthrough:
		var suid spiceboxv1alpha1.SessionUserIdentity
		if err := f.K8s.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &suid); err != nil {
			return nil, nil, nil, fmt.Errorf("buildMCPTools: get SessionUserIdentity %s/%s: %w", sess.Namespace, sess.Name, err)
		}
		runtimeIdentity = runner.RuntimeIdentityFromSessionUserIdentity(&suid)
	default: // identityMode=agent (or unset)
		var agentIdentity spiceboxv1alpha1.AgentIdentity
		if class.Spec.AgentIdentity != "" {
			if err := f.K8s.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: class.Spec.AgentIdentity}, &agentIdentity); err != nil {
				return nil, nil, nil, fmt.Errorf("buildMCPTools: get AgentIdentity %q: %w", class.Spec.AgentIdentity, err)
			}
			runtimeIdentity = runner.RuntimeIdentityFromAgentIdentity(&agentIdentity)
		}
		// When AgentIdentity is unset, runtimeIdentity stays zero —
		// credresolve.Descriptors resolves no descriptors for unauthenticated
		// servers and errors loudly otherwise (matching production behavior).
	}
	mcpBroker := inproc.NewWithMinter(f.K8s, f.Minter)
	// Assigned rather than passed to the constructor because the constructor
	// takes only the federation minter; the field is the broker's own seam and
	// nil leaves githubApp failing closed exactly as it does in production.
	mcpBroker.GitHubApp = f.GitHubAppMinter

	for _, ref := range class.Spec.MCPServers {
		var cr spiceboxv1alpha1.MCPServer
		if err := f.K8s.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: ref.Ref}, &cr); err != nil {
			return nil, nil, nil, fmt.Errorf("buildMCPTools: get MCPServer %q: %w", ref.Ref, err)
		}
		// Insist on Valid=True so a misconfigured MCPServer doesn't
		// silently produce zero tools (which would manifest later as
		// "unknown tool" from the LLM's perspective with no link
		// back to the controller-level validation failure).
		cond := apimeta.FindStatusCondition(cr.Status.Conditions, spiceboxv1alpha1.MCPServerConditionValid)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			msg := fmt.Sprintf("MCPServer/%s is not Valid", ref.Ref)
			if cond != nil {
				msg = fmt.Sprintf("MCPServer/%s: Valid=%s reason=%s message=%s",
					ref.Ref, cond.Status, cond.Reason, cond.Message)
			}
			return nil, nil, nil, fmt.Errorf("buildMCPTools: %s", msg)
		}

		// Resolve the credential for this MCP server through the real
		// broker path, mirroring internal/cmd/runner/main.go.
		mcpReqs := mcpkind.New().SetupRequirements(ctx, mcpkind.NewTarget(&cr))
		// Record this MCP origin's auth-failure shape, through the SAME shared
		// helper internal/cmd/runner/main.go uses, so the two derivations cannot drift.
		authFailureByOrigin.RecordRequirements(originfmt.ForMCPServer(cr.Name), mcpReqs)
		descs, derr := credresolve.Descriptors(mcpReqs, runtimeIdentity, ref.CredentialRemap)
		if derr != nil {
			return nil, nil, nil, fmt.Errorf("buildMCPTools: credential descriptor for MCPServer/%s: %w", ref.Ref, derr)
		}
		var authHeader, authValue string
		if len(descs) > 0 {
			res, rerr := mcpBroker.Resolve(ctx, broker.Request{Credentials: descs})
			if rerr != nil {
				return nil, nil, nil, fmt.Errorf("buildMCPTools: resolve credential for MCPServer/%s: %w", ref.Ref, rerr)
			}
			for h, v := range res.HTTPHeaders {
				authHeader, authValue = h, v
			}
		}
		// authCredID identifies, for the durable per-call use_token check
		// below, which resolved descriptor produced the (authHeader, authValue)
		// SetAuth is about to freeze — mirrors internal/cmd/runner/main.go's identical
		// derivation. Empty when TokenAuthz is off or the server is
		// unauthenticated: there is nothing to check for a credential that was
		// never sent upstream.
		var authCredID string
		for _, d := range descs {
			if d.Inject.Header != nil && authHeader != "" && d.Inject.Header.Name == authHeader {
				authCredID = externaltoken.CredID(d.Source)
				break
			}
		}

		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		// The e2e MCPStub is an httptest server bound to 127.0.0.1, which
		// the production SSRF-guarded probe client (correctly) refuses.
		// Inject a plain client so the in-process runner can probe the
		// loopback stub; production (internal/cmd/runner/main.go) omits HTTP and
		// gets the guarded default.
		live, perr := (&mcpprobe.Client{HTTP: http.DefaultClient, URL: cr.Spec.Server.URL}).ListTools(probeCtx, authHeader, authValue)
		cancel()
		if perr != nil {
			return nil, nil, nil, fmt.Errorf("buildMCPTools: probe MCPServer/%s at %s: %w",
				ref.Ref, cr.Spec.Server.URL, perr)
		}

		// Override the CR's metadata.name so the synthesized prefix
		// matches AgentClass.MCPServers[].Name (the LLM-prefix the
		// AgentClass author chose), not the CR's own metadata.name.
		// Mirrors internal/cmd/runner/main.go's cr2 override — including passing the
		// REAL CR name as the revocation origin so tool Origin() matches the
		// revoke key when name != ref.
		cr2 := cr
		cr2.Name = ref.Name
		// The e2e MCPStub is an httptest server bound to 127.0.0.1, which
		// the production SSRF-guarded client (correctly) refuses. Inject a
		// plain client so in-process tests can reach the loopback stub.
		res, err := mcpdispatch.Synthesize(&cr2, live,
			mcpdispatch.WithHTTPClient(http.DefaultClient),
			mcpdispatch.WithOriginName(cr.Name))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("buildMCPTools: synthesize %q: %w", ref.Ref, err)
		}
		// built is the LLM-visible set; res.AppTools (MCP-UI app-visible-only)
		// gets NO auth/use_token/reauth wiring below — mirrors
		// internal/cmd/runner/main.go: app tools are inert in this phase (no transport),
		// so there is nothing to wire.
		built := res.LLMTools
		// Attach auth + relwrites writer + logger to each MCPTool.
		for _, t := range built {
			if mt, ok := t.(*mcpdispatch.MCPTool); ok {
				mt.SetAuth(authHeader, authValue)
				mt.SetRelWriter(relWriter)
				mt.SetSlotBoundChecker(slotBoundChecker)
				mt.SetLabelSink(labelStore)
				mt.SetLogger(slog.Default())
				if authCredID != "" && f.TokenAuthz {
					// Wires the per-call durable SpiceDB use_token check — the
					// same gate internal/cmd/runner/main.go attaches — so a grant the
					// operator writes/revokes mid-session (reconcileCredentialGrants,
					// re-triggered by the AgentIdentity watch already registered on
					// this harness's AgentSession reconciler) takes effect on the
					// very next MCP call, exactly as in production.
					sessKey := types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}
					mt.SetUseTokenGate(&mcpdispatch.UseTokenGate{
						Checker:     f.SpiceDB,
						SessionNS:   sessKey.Namespace,
						SessionName: sessKey.Name,
						HMACKey:     useTokenHMACKey,
						CredID:      authCredID,
						FailSession: func(reason, msg string) {
							if werr := status.WriteFailed(context.Background(), reason, msg); werr != nil {
								slog.Default().Error("e2e: mcp use_token gate WriteFailed errored",
									"session", sessKey.Namespace+"/"+sessKey.Name,
									"reason", reason, "err", werr.Error())
							}
						},
					})
				}
			}
		}
		out = append(out, built...)
		outApp = append(outApp, res.AppTools...)
		ui := cr.Spec.MCPUIAppTools
		outOrigins = append(outOrigins, runner.AppToolOrigin(cr.Name, ui != nil && ui.Enabled, res.AppTools))
	}
	return out, outApp, outOrigins, nil
}

// appToolsByName indexes MCP-UI app-visible-only tools by name for
// Loop.AppTools — a registry deliberately separate from l.Tools. Mirrors
// internal/cmd/runner/main.go's identically-named helper (duplicated rather than
// shared: this package has no other reason to depend on internal/cmd/runner, and the
// helper is a trivial 8-line index).
func appToolsByName(tools []tool.Tool) map[string]tool.Tool {
	if len(tools) == 0 {
		return nil
	}
	m := make(map[string]tool.Tool, len(tools))
	for _, t := range tools {
		m[t.Name()] = t
	}
	return m
}

// mcpArgsHashKeySecretDataKey is the per-session -memory-token Secret's data
// key holding the args-hash HMAC key that both the operator's grant-writer
// (agentsession.reconcileCredentialGrants) and this gate's presented-value
// hashes key on. Mirrors agentSessionSecretArgsHashKey in
// pkg/controllers/agentsession/rbac.go (unexported there) and
// argsHashKeySecretDataKey in pkg/controllers/toolcall/controller.go —
// duplicated as a literal here per that same precedent rather than importing
// either controller package, which this one has no other reason to depend on.
const mcpArgsHashKeySecretDataKey = "args-hash-key"

// mcpArgsHashKey reads the per-session args-hash HMAC key from the
// -memory-token Secret the AgentSession reconciler mints before
// RunnerFactory.Start (see mcpArgsHashKeySecretDataKey doc). Called only when
// TokenAuthz is opted in, at which point reconcileCredentialGrants has
// already run in the SAME reconcile that calls RunnerFactory.Start, so the
// Secret + key are guaranteed present — a Get/data-key failure here is
// therefore a genuine harness-wiring bug, not a benign race, and is
// propagated as a hard buildMCPTools error rather than silently skipping the
// gate (which would make every use_token check pass a zero-length key,
// authorizing nothing it should and denying everything it shouldn't).
func (f *InProcessRunnerFactory) mcpArgsHashKey(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) ([]byte, error) {
	var reader client.Reader = f.K8s
	if f.APIReader != nil {
		reader = f.APIReader
	}
	var sec corev1.Secret
	secKey := client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name + spiceboxv1alpha1.MemoryTokenSecretSuffix}
	if err := reader.Get(ctx, secKey, &sec); err != nil {
		return nil, fmt.Errorf("get args-hash-key secret %s: %w", secKey.Name, err)
	}
	v := sec.Data[mcpArgsHashKeySecretDataKey]
	if len(v) == 0 {
		return nil, fmt.Errorf("secret %s missing data key %q", secKey.Name, mcpArgsHashKeySecretDataKey)
	}
	return v, nil
}

// buildSidecarTools synthesizes one tool.Tool per allowlisted entry per
// resolved sidecar toolbox, mirroring internal/cmd/runner/main.go's sidecar block.
// For each resolved sidecar it probes the live MCP endpoint (the in-process
// MCPStub in tests), then sidecartoolboxsynth.Synthesize builds the
// degrade-wrapped dispatch tools the runner invokes via Tool.Execute.
//
// Probe URL: production probes http://127.0.0.1:<rt.Port> (the
// operator-allocated loopback port the sidecar container binds). The
// in-process MCPStub listens on a RANDOM httptest port instead, so the
// harness sets f.SidecarProbeURL to redirect the probe at the stub. When
// the seam is nil (production-shaped), we fall back to the rt.Port URL.
//
// Like buildMCPTools, a probe/synthesis failure surfaces as a buildLoop
// error rather than a runner that silently runs without the sidecar tools.
func (f *InProcessRunnerFactory) buildSidecarTools(resolved []spiceboxv1alpha1.ResolvedSidecarToolbox, sessionCache *mcpprobe.SessionCache, authFailureByOrigin runner.AuthFailureOrigins) ([]tool.Tool, error) {
	if len(resolved) == 0 {
		return nil, nil
	}
	ctx := context.Background()

	// Record every RESOLVED sidecar's auth-failure shape up front — including
	// entries this pass may skip — mirroring internal/cmd/runner/main.go. The provider is
	// declared right on the resolved spec, so no probe is needed to learn it,
	// which is what keeps the map boot-only and race-free.
	for _, rt := range resolved {
		authFailureByOrigin.RecordProvider(originfmt.ForSidecar(rt), rt.Spec.UpstreamAuth.Provider)
	}

	var out []tool.Tool
	for _, rt := range resolved {
		probeURL := fmt.Sprintf("http://127.0.0.1:%d%s", rt.Port, sidecartoolboxsynth.EndpointPath(rt))
		if f.SidecarProbeURL != nil {
			probeURL = f.SidecarProbeURL(rt)
		}

		// Synthesize targets http://127.0.0.1:<rt.Port> internally; override
		// Port so the dispatch endpoint matches the probe endpoint (the stub),
		// not the controller-allocated port the stub never bound.
		rt2 := rt
		if f.SidecarProbeURL != nil {
			if p, ok := portFromURL(probeURL); ok {
				rt2.Port = p
			}
		}
		// Run the SAME shared core the production runner uses (probe +
		// synthesize + provenance + reachability). sp is nil here: secret-gated
		// sidecars are AwaitingSecret at boot, so their reachability is recorded
		// via the refresher path, not this boot pass. e2ePlainProber injects a
		// plain client (the stub is a 127.0.0.1 httptest server the production
		// SSRF-guarded client correctly refuses).
		built, err := runner.ProbeSynthSidecar(ctx, probeURL, rt2, e2ePlainProber, nil, nil, nil, sessionCache)
		if err != nil {
			return nil, fmt.Errorf("buildSidecarTools: sidecar %q: %w", rt.Ref, err)
		}
		out = append(out, built...)
	}
	return out, nil
}

// partitionHeldTools splits a non-meta tool list into the tools that go into
// capability.Assemble and the tools a replay holds back from it.
//
// Order is preserved in both halves: the LLM-facing tool order is part of what
// a replay reproduces, and a reordered list changes the request the model
// answers. A name in hold that matches nothing is simply not held — it is not
// silently swallowed, because a tool the bundle expected and the fixture never
// composed still surfaces, loudly, as a missing tool at the first turn its
// recorded catalog names it.
func partitionHeldTools(all []tool.Tool, hold []string) (kept, held []tool.Tool) {
	if len(hold) == 0 {
		return all, nil
	}
	byName := make(map[string]bool, len(hold))
	for _, n := range hold {
		byName[n] = true
	}
	kept = make([]tool.Tool, 0, len(all))
	for _, tt := range all {
		if byName[tt.Name()] {
			held = append(held, tt)
			continue
		}
		kept = append(kept, tt)
	}
	return kept, held
}

// e2ePlainProber is the harness's runner.SidecarProber: a plain (non-SSRF-
// guarded) tools/list against the 127.0.0.1 stub / operator-controlled pod IP,
// which the production guarded client correctly refuses. Shared by the boot
// pass and the mid-session refresher.
func e2ePlainProber(ctx context.Context, url string) ([]mcpprobe.Tool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return (&mcpprobe.Client{HTTP: http.DefaultClient, URL: url}).ListTools(probeCtx, "", "")
}

// portFromURL extracts the TCP port from a http://host:port URL. Used to
// align the sidecartoolbox synthesizer's dispatch endpoint with the
// harness's redirected probe URL (the random httptest port).
func portFromURL(raw string) (int32, bool) {
	u, err := neturl.Parse(raw)
	if err != nil {
		return 0, false
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		return 0, false
	}
	return int32(p), true
}

// newSidecarToolRefresher builds the in-process equivalent of
// internal/cmd/runner/sidecar_refresh.go's newSidecarToolRefresher: a
// runner.Loop.ToolRefresher consulted at the top of each turn that
// re-Gets the AgentSession, finds any newly-ready secret-gated
// separate-pod sidecar in status, probes its MCP endpoint, and
// synthesizes the degrade-wrapped tools to add to the live tool set.
//
// Two divergences from the production refresher, both mandated by the
// in-process harness having no real sidecar pod:
//
//  1. The dispatch + probe target is the resolved entry's SidecarPodIP:Port,
//     which MarkSidecarReady stamps to the harness's MCP stub address
//     (127.0.0.1:<random-httptest-port>). The stub stands in for the
//     separate-pod sidecar.
//  2. An EXTRA gate: a newly-ready sidecar is only synthesized once the
//     per-session secret-output Secret EXISTS. This mirrors the operator's
//     real semantics (it only flips AwaitingSecret=false once the bound
//     secret has landed) and makes the e2e flow deterministic WITHOUT racing
//     the producer: the gating Secret is written only after the producer tool
//     ran, so the sidecar tool is provably ABSENT until the producer emitted
//     its secret, and PRESENT on the next turn after.
//
// The closure owns its dedup via the captured `synthed` set (Name →
// last-synthesized SidecarPodIP), so a ready sidecar is synthesized exactly
// once per pod IP. Probe failures are returned (logged + non-fatal by the
// loop) and retried next turn; the recorded IP is left unchanged so a
// transiently-unreachable stub is not permanently dropped.
func (f *InProcessRunnerFactory) newSidecarToolRefresher(sessKey types.NamespacedName, sessionCache *mcpprobe.SessionCache) func(ctx context.Context) (runner.ToolRefreshResult, error) {
	synthed := map[string]string{}
	// Records status.sidecarReachability exactly as the production runner does,
	// so the harness exercises the runtime reachability reporting.
	sp := runner.NewStatusPatcher(f.K8s, sessKey)
	return func(ctx context.Context) (runner.ToolRefreshResult, error) {
		var sess spiceboxv1alpha1.AgentSession
		if err := f.K8s.Get(ctx, sessKey, &sess); err != nil {
			return runner.ToolRefreshResult{}, fmt.Errorf("inprocess sidecar refresh: get AgentSession: %w", err)
		}

		// Gate: the per-session secret-output Secret must exist before any
		// secret-gated sidecar is synthesized. Until the producer publishes
		// its secret (which creates this Secret) the sidecar tool stays absent,
		// regardless of what MarkSidecarReady stamped — the same gate the real
		// operator enforces via AwaitingSecret.
		secretExists := false
		var soSecret corev1.Secret
		if err := f.K8s.Get(ctx, types.NamespacedName{
			Namespace: sessKey.Namespace,
			Name:      secretoutsrv.SecretOutputSecretName(sessKey.Name),
		}, &soSecret); err == nil {
			secretExists = true
		}

		var added []tool.Tool
		for _, rt := range sess.Status.ResolvedSidecarToolboxes {
			if rt.RunMode != agentsession.RunModeSeparatePod {
				continue
			}
			if rt.AwaitingSecret || rt.SidecarPodIP == "" {
				continue
			}
			// Secret-gated sidecars (any SecretInput) only synthesize once the
			// gating Secret exists. A sidecar with no SecretInputs would not be
			// separate-pod, so in practice this always applies here.
			if len(rt.Spec.SecretInputs) > 0 && !secretExists {
				continue
			}
			// Already synthesized against this pod IP → nothing to do.
			if prevIP, ok := synthed[rt.Name]; ok && prevIP == rt.SidecarPodIP {
				continue
			}

			url := fmt.Sprintf("http://%s:%d%s", rt.SidecarPodIP, rt.Port, sidecartoolboxsynth.EndpointPath(rt))
			// Same shared core as the production runner: probe + synthesize +
			// provenance + record per-session reachability. sp writes
			// status.sidecarReachability so the harness exercises the runtime
			// reachability reporting (and the mid-session unreachable/recorded
			// path) exactly as production does.
			built, err := runner.ProbeSynthSidecar(ctx, url, rt, e2ePlainProber, sess.Status.ObservedPins, nil, sp, sessionCache)
			if err != nil {
				return runner.ToolRefreshResult{Added: added}, fmt.Errorf("inprocess sidecar refresh: %w", err)
			}
			synthed[rt.Name] = rt.SidecarPodIP
			added = append(added, built...)
		}
		return runner.ToolRefreshResult{Added: added}, nil
	}
}

// buildSandboxTools synthesizes one Tool per toolspec per bundle from
// the AgentSession's resolved bundle sessions, mirroring the relevant
// subset of internal/cmd/runner/main.go's loop.
//
// Lookups + harness invariants:
//   - sess.Status.BundleSessions is populated by the AgentSession
//     reconciler before the runner factory is invoked; an empty list
//     means the AgentClass has no bundles, in which case we return
//     (nil, nil, nil).
//   - The bundle SpiceboxSession's Status.ResolvedClass is what the
//     class tool catalog is read from. Tests using sandbox bundles
//     must stamp it (the SpiceboxSession controller is not wired in
//     the harness).
//   - Toolkits resolve from the in-process builtins (pkg/toolkits)
//     only. Toolkit CR-backed toolkits are out of scope.
//
// Returns the synthesized tools plus a bundle.name → SpiceboxSession.name
// map that's threaded into SessionContext.BundleSessions so the runner's
// tool dispatcher can route a tool_use to the right bundle session.
func (f *InProcessRunnerFactory) buildSandboxTools(
	sess *spiceboxv1alpha1.AgentSession,
	class *spiceboxv1alpha1.AgentClass,
	authFailureByOrigin runner.AuthFailureOrigins,
) ([]tool.Tool, map[string]string, error) {
	if len(sess.Status.BundleSessions) == 0 {
		return nil, nil, nil
	}
	if f.K8s == nil {
		return nil, nil, fmt.Errorf("buildSandboxTools: K8s client is nil but AgentSession has %d bundle(s)",
			len(sess.Status.BundleSessions))
	}
	ctx := context.Background()

	// Resolve the session-scoped RuntimeIdentity exactly as internal/cmd/runner/main.go
	// does, so the harness drives the SAME credential-source decision production
	// does (closing the fidelity gap that let the cross-namespace
	// passthrough-credential bug ship). For identityMode=userPassthrough this
	// loads the per-session SessionUserIdentity (named after the session, in the
	// session namespace); its StaticProjection redirects type=static credential
	// sources at the per-session materialized Secret in the session namespace,
	// which is what makes a sandbox ToolCall's Source.Namespace equal the
	// ToolCall namespace and pass ValidateCredentialSourceNamespaces. For
	// identityMode=agent it stays zero — the per-bundle switch in
	// runner.BundleRuntimeIdentity loads the bundle/class AgentIdentity instead.
	//
	// effMode is the RESOLVED identity mode: a userPassthrough choice on an
	// ask|dynamic class must load the SessionUserIdentity here on re-spawn, and a
	// first-boot ask|dynamic session resolves as the provisional agent. Threaded
	// into BundleRuntimeIdentity below so the per-bundle credential source matches.
	effMode := effectiveIdentityMode(sess, class)
	var sessionRuntimeIdentity runner.RuntimeIdentity
	if effMode == spiceboxv1alpha1.IdentityModeUserPassthrough {
		var suid spiceboxv1alpha1.SessionUserIdentity
		if err := f.K8s.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &suid); err != nil {
			return nil, nil, fmt.Errorf("buildSandboxTools: userPassthrough: get SessionUserIdentity %s/%s: %w",
				sess.Namespace, sess.Name, err)
		}
		sessionRuntimeIdentity = runner.RuntimeIdentityFromSessionUserIdentity(&suid)
	}

	// SpiceDB-backed Writer for the toolspec's WritesRelationships
	// post-effect — mirrors buildMCPTools' relWriter construction above.
	// Nil-safe: when SpiceDB is unset, SandboxTool.SetRelWriter(nil) is a
	// no-op and the post-effect step is silently skipped.
	var relWriter relwrites.Writer
	if f.SpiceDB != nil {
		relWriter = &relwrites.SpiceDBWriter{
			Client: f.SpiceDB.Writer(relwrites.Source),
		}
	}
	// Read side of the slot-bound gate — mirrors buildMCPTools above and
	// internal/cmd/runner/main.go's sandbox loop. See newSlotBoundChecker.
	slotBoundChecker := f.newSlotBoundChecker(sess)

	bundleSessionMap := make(map[string]string, len(sess.Status.BundleSessions))
	var out []tool.Tool
	for _, bs := range sess.Status.BundleSessions {
		bundleSessionMap[bs.Name] = bs.SpiceboxSessionName

		var sboxSess spiceboxv1alpha1.SpiceboxSession
		if err := f.K8s.Get(ctx, client.ObjectKey{
			Namespace: sess.Namespace, Name: bs.SpiceboxSessionName,
		}, &sboxSess); err != nil {
			return nil, nil, fmt.Errorf("buildSandboxTools: get bundle SpiceboxSession %q: %w",
				bs.SpiceboxSessionName, err)
		}
		if sboxSess.Status.ResolvedClass == nil {
			// In production the SpiceboxSession controller stamps
			// ResolvedClass before flipping Ready=True. The e2e
			// harness does not run that controller; tests that don't
			// exercise sandbox tools (e.g. p1_multi_bundle_pvc) stamp
			// Ready directly without populating ResolvedClass. Skip
			// the bundle silently rather than erroring out — the
			// runner will refuse to dispatch sandbox tool_uses for
			// this bundle (BundleSessions entry absent), which is
			// the correct shape for those scenarios.
			slog.Default().Info("buildSandboxTools: skipping bundle with no Status.ResolvedClass",
				"bundle", bs.Name, "session", bs.SpiceboxSessionName)
			delete(bundleSessionMap, bs.Name)
			continue
		}
		classTools := sboxSess.Status.ResolvedClass.Tools

		var bundleCfg spiceboxv1alpha1.ToolBundle
		bundleFound := false
		for _, b := range class.Spec.ToolBundles {
			if b.Name == bs.Name {
				bundleCfg = b
				bundleFound = true
				break
			}
		}
		if !bundleFound {
			return nil, nil, fmt.Errorf("buildSandboxTools: bundle %q in AgentSession.Status.BundleSessions but not in AgentClass.spec.toolBundles",
				bs.Name)
		}

		// Resolve the per-bundle RuntimeIdentity through the SAME shared helper
		// internal/cmd/runner/main.go uses, so the harness exercises the real
		// credential-source decision (passthrough → session-ns projected Secret;
		// agent → bundle/class AgentIdentity) rather than a hardcoded
		// agent-identity path.
		// Pass the RESOLVED effective mode (Task 10): a userPassthrough re-spawn of
		// an ask|dynamic class must resolve per-bundle credentials from the
		// SessionUserIdentity, not the class AgentIdentity. Static classes are
		// unaffected (effMode == class.Spec.IdentityMode for them).
		bundleRuntimeIdentity, err := runner.BundleRuntimeIdentity(ctx, f.K8s, sess.Namespace, sessionRuntimeIdentity, class, bundleCfg, effMode)
		if err != nil {
			return nil, nil, fmt.Errorf("buildSandboxTools: %w", err)
		}

		specs := make([]*spec.Spec, 0, len(bundleCfg.Toolspecs))
		tks := make([]*toolkit.Toolkit, 0, len(bundleCfg.Toolspecs))
		credentials := make([][]spiceboxv1alpha1.CredentialDescriptor, 0, len(bundleCfg.Toolspecs))
		for _, tsName := range bundleCfg.Toolspecs {
			var ts spiceboxv1alpha1.SpiceboxToolspec
			if err := f.K8s.Get(ctx, client.ObjectKey{Name: tsName}, &ts); err != nil {
				return nil, nil, fmt.Errorf("buildSandboxTools: get SpiceboxToolspec %q for bundle %q: %w",
					tsName, bs.Name, err)
			}
			s, err := ts.Spec.ToSpec()
			if err != nil {
				return nil, nil, fmt.Errorf("buildSandboxTools: convert SpiceboxToolspec %q: %w", tsName, err)
			}
			specs = append(specs, s)
			tk, terr := resolveToolkit(ctx, f.K8s, ts.Spec.Toolkit.Name, ts.Spec.Toolkit.Revision)
			if terr != nil {
				// Toolkit resolution failed: log and pass nil. Synthesize
				// tolerates nil entries (the LLM-facing description falls
				// back to plain text and Interactive=false is the safe
				// default). The ToolCall controller, which has its own
				// CR-aware registry, will surface the toolkit-resolution
				// error when the call actually dispatches.
				slog.Default().Info("buildSandboxTools: toolkit resolve failed; using nil",
					"toolkit", ts.Spec.Toolkit.Name, "revision", ts.Spec.Toolkit.Revision, "err", terr.Error())
				tks = append(tks, nil)
				credentials = append(credentials, nil)
			} else {
				tks = append(tks, tk)
				reqs := clikind.New().SetupRequirements(ctx, clikind.NewTarget(tk))
				// Record this toolkit origin's auth-failure shape, mirroring
				// internal/cmd/runner/main.go's bundle loop. A CLI toolkit exposes no HTTP
				// status, so exitCodes/stderrPatterns are its whole corroboration
				// surface — an unrecorded toolkit origin can never be corroborated.
				authFailureByOrigin.RecordRequirements(originfmt.ForToolkit(tk.Name), reqs)
				creds, cerr := credresolve.Descriptors(reqs, bundleRuntimeIdentity, bundleCfg.CredentialRemap)
				if cerr != nil {
					return nil, nil, fmt.Errorf("buildSandboxTools: credential descriptors for toolspec %q in bundle %q: %w",
						tsName, bs.Name, cerr)
				}
				credentials = append(credentials, creds)
			}
		}

		built, err := sandbox.Synthesize(bundleCfg, specs, tks, credentials, classTools)
		if err != nil {
			return nil, nil, fmt.Errorf("buildSandboxTools: synthesize bundle %q: %w", bs.Name, err)
		}
		// Attach the relwrites writer + logger to each synthesized sandbox
		// tool, mirroring buildMCPTools' per-tool SetRelWriter/SetLogger wiring.
		for _, t := range built {
			if st, ok := t.(*sandbox.SandboxTool); ok {
				st.SetRelWriter(relWriter)
				st.SetSlotBoundChecker(slotBoundChecker)
				st.SetLogger(slog.Default())
			}
		}
		out = append(out, built...)
	}
	return out, bundleSessionMap, nil
}

// resolveToolkit looks up a toolkit by (name, revision). Built-ins win on
// collision; otherwise SpiceboxToolkit CRs on the cluster (loaded via the
// in-process K8s client) are consulted. This is wider than internal/cmd/runner/main.go's
// helper of the same shape, which only consults built-ins — the e2e harness
// needs to surface non-builtin toolkits so scenarios can wire custom
// fixtures (e.g. the fake-codey-toolkit for interactive bridge tests). The
// production runner inherits the same surface area once the toolspec registry
// is threaded into internal/cmd/runner/main.go, but that change is out of scope here.
func resolveToolkit(ctx context.Context, k8s client.Client, name, revision string) (*toolkit.Toolkit, error) {
	for _, tk := range toolkits.All() {
		if tk.Name == name && tk.ToolkitRevision == revision {
			tk := tk
			return &tk, nil
		}
	}
	if k8s == nil {
		return nil, fmt.Errorf("resolveToolkit: no built-in for %s@%s and K8s client is nil", name, revision)
	}
	var list spiceboxv1alpha1.SpiceboxToolkitList
	if err := k8s.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("resolveToolkit: list SpiceboxToolkits: %w", err)
	}
	for i := range list.Items {
		s := &list.Items[i].Spec
		if s.Name == name && s.ToolkitRevision == revision {
			tk, err := s.ToToolkit()
			if err != nil {
				return nil, fmt.Errorf("resolveToolkit: convert SpiceboxToolkit %q: %w", list.Items[i].Name, err)
			}
			return tk, nil
		}
	}
	return nil, fmt.Errorf("resolveToolkit: %s@%s not found in built-ins or SpiceboxToolkit CRs", name, revision)
}

// effectiveIdentityMode resolves the identity mode the runner actually boots
// under, mirroring internal/cmd/runner/main.go's effectiveMode switch (commit d0249c68):
//
//   - static modes (agent/userPassthrough/unset) pass through unchanged.
//   - ask|dynamic on a re-spawn past the choice (status.effectiveIdentityMode
//     set) use the recorded resolved mode.
//   - ask|dynamic on first boot (status.effectiveIdentityMode empty) boot on a
//     PROVISIONAL agent identity — always available per the CEL rule that
//     ask|dynamic requires spec.agentIdentity — and the SessionStart gate then
//     confirms (agent) or hands off (userPassthrough).
//
// The per-tool identity resolution (buildMCPTools / buildSandboxTools /
// BundleRuntimeIdentity) MUST consult this rather than class.Spec.IdentityMode
// so a userPassthrough re-spawn resolves the SessionUserIdentity, not the agent.
func effectiveIdentityMode(sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass) string {
	mode := class.Spec.IdentityMode
	switch mode {
	case spiceboxv1alpha1.IdentityModeAsk, spiceboxv1alpha1.IdentityModeDynamic:
		if sess.Status.EffectiveIdentityMode != "" {
			return sess.Status.EffectiveIdentityMode
		}
		return spiceboxv1alpha1.IdentityModeAgent // provisional
	}
	return mode
}

// identityGatePending reports whether the SessionStart IdentityChoiceGate must
// run for this session: an ask|dynamic class that has NOT yet resolved its
// effective mode (first boot). A re-spawn past the choice (E set) skips the
// gate; static modes never gate.
func identityGatePending(sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass) bool {
	switch class.Spec.IdentityMode {
	case spiceboxv1alpha1.IdentityModeAsk, spiceboxv1alpha1.IdentityModeDynamic:
		return sess.Status.EffectiveIdentityMode == ""
	}
	return false
}

// registerOrchestrator adds the session's orchestrator to the shared
// Apply-subscriber registry. The shared subscriber (installed once on
// the first session start, see ensureApplySubscriber) routes Apply
// envelopes addressed to ap.session.<ns>.<name>.out.tool_approval_applied
// into the matching orchestrator's DeliverDecision.
//
// Unlike internal/cmd/runner/main.go (which subscribes per runner process), the
// e2e harness shares one NATS connection across many sessions and would
// pay a subscription-spawn-and-drain cost per Start/Stop. Sharing the
// subscriber sidesteps that and keeps the goleak.IgnoreCurrent baseline
// stable across tests.
// registerSessionCache records a session's MCP session cache so Stop can Close
// it. A re-Start for the same session (the passthrough re-spawn) replaces and
// closes the prior cache rather than leaking its open sessions.
func (f *InProcessRunnerFactory) registerSessionCache(sess *spiceboxv1alpha1.AgentSession, c *mcpprobe.SessionCache) {
	key := sessionKey(sess)
	f.sessionCacheMu.Lock()
	prev := f.sessionCaches[key]
	if f.sessionCaches == nil {
		f.sessionCaches = map[string]*mcpprobe.SessionCache{}
	}
	f.sessionCaches[key] = c
	f.sessionCacheMu.Unlock()
	if prev != nil {
		_ = prev.Close()
	}
}

// closeSessionCache Closes and drops a session's MCP session cache. Called from
// Stop; safe for a session that never built one.
func (f *InProcessRunnerFactory) closeSessionCache(sess *spiceboxv1alpha1.AgentSession) {
	key := sessionKey(sess)
	f.sessionCacheMu.Lock()
	c := f.sessionCaches[key]
	delete(f.sessionCaches, key)
	f.sessionCacheMu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// closeAllSessionCaches Closes and drops every registered MCP session cache,
// keyed directly (unlike closeSessionCache, it needs no *AgentSession — only
// the key string registerSessionCache/closeSessionCache already key by).
// Called from Shutdown, which cancels each runner's context but — unlike
// Stop — never called closeSessionCache for a session still running at
// test-cleanup time.
//
// A cached session backed by the real streamable-HTTP transport
// (test/e2e/workshop_mcp.go's mountWorkshopMCP, the first sidecar route this
// harness proxies to a REAL mcp.NewStreamableHTTPHandler rather than the
// canned MCPStub) holds a live, "active" HTTP connection for the session's
// whole lifetime — the server-to-client SSE stream a real MCP session keeps
// open. Cancelling the runner's context does not tell that connection to
// close; only Close()ing the ClientSession does. Without this, a session
// still running when Shutdown fires leaves that connection open, and the
// package's own t.Cleanup(srv.Close) for the mounted server (registered
// BEFORE the manager's, so it runs AFTER by LIFO order) blocks — observably,
// as Go's httptest.Server.Close prints "blocked in Close ... waiting for
// connections" and does not give up. The canned MCPStub never surfaced this:
// its handler is a plain request/response, so nothing is ever "active"
// between calls for Close to wait on.
func (f *InProcessRunnerFactory) closeAllSessionCaches() {
	f.sessionCacheMu.Lock()
	caches := make([]*mcpprobe.SessionCache, 0, len(f.sessionCaches))
	for key, c := range f.sessionCaches {
		caches = append(caches, c)
		delete(f.sessionCaches, key)
	}
	f.sessionCacheMu.Unlock()
	for _, c := range caches {
		_ = c.Close()
	}
}

func (f *InProcessRunnerFactory) registerOrchestrator(sess *spiceboxv1alpha1.AgentSession, orch *approval.Orchestrator) {
	f.applySubOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		f.applySubCancel = cancel
		nc := f.NATS
		sub, err := nc.Subscribe(channelevents.SubjectOut(channelevents.AnySessionPrefix(), channelevents.KindInteractionApplied), func(m *nats.Msg) {
			var env channelevents.Envelope
			if uerr := json.Unmarshal(m.Data, &env); uerr != nil {
				fmt.Fprintf(os.Stderr, "InProcessRunnerFactory: apply envelope decode: %v\n", uerr)
				return
			}
			// This subscription is the cluster-wide "ap.session.*.*.out."
			// wildcard and the orchestrator below is looked up by env.Session,
			// so the same rule the inbound wrappers enforce applies here: the
			// subject is the session identity the bus authorized, env.Session is
			// publisher-controlled JSON. Without this, a publish on one
			// session's out subtree could deliver an approval decision into
			// ANOTHER session's parked tool-approval gate. Production's
			// equivalent is pkg/channels/channelsd/outbound/relay.go's handle.
			if _, _, aerr := channelevents.AuthorizeOutSubject(m.Subject, env); aerr != nil {
				fmt.Fprintf(os.Stderr, "InProcessRunnerFactory: apply drop, %v (subject %q)\n", aerr, m.Subject)
				return
			}
			var pl channelevents.InteractionAppliedPayload
			if uerr := json.Unmarshal(env.Payload, &pl); uerr != nil {
				fmt.Fprintf(os.Stderr, "InProcessRunnerFactory: apply payload decode: %v\n", uerr)
				return
			}
			// Slice C2: tool_approval rides the generic interaction_applied. Only
			// the tool_approval category resumes off THIS cross-session subscriber;
			// info_leakage / content_inspection / identity_choice resume via their
			// own per-session subscribers (subscribeFactoryLeakageApplied /
			// subscribeFactoryInteractionApplied), which default other categories.
			if pl.Category != categories.ToolApproval {
				return
			}
			key := env.Session.Namespace + "/" + env.Session.Name
			f.applyMu.Lock()
			target := f.applyOrchestrators[key]
			f.applyMu.Unlock()
			if target == nil {
				// No registered orchestrator for this session — happens
				// if the envelope arrives after Stop. Log to stderr and
				// drop; the runner that would have resumed is already gone.
				fmt.Fprintf(os.Stderr, "InProcessRunnerFactory: no orchestrator for %s; dropping Apply\n", key)
				return
			}
			// The SHARED mapper, not a local copy of it. Hand-rolling this is
			// how the harness came to deliver a decision missing the approver's
			// structured identity: a field added to Decision reached production
			// and not the tests, so the e2e path was structurally unable to
			// exercise the thing it exists to cover.
			d, deliver, _ := approval.FromApplied(pl)
			if !deliver {
				return
			}
			target.DeliverDecision(pl.RequestRef, d)
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "InProcessRunnerFactory: subscribe Apply: %v\n", err)
			return
		}
		// Drain on ctx cancel so cleanup unwinds cleanly.
		go func() {
			<-ctx.Done()
			_ = sub.Drain()
		}()
	})
	f.applyMu.Lock()
	if f.applyOrchestrators == nil {
		f.applyOrchestrators = map[string]*approval.Orchestrator{}
	}
	f.applyOrchestrators[sess.Namespace+"/"+sess.Name] = orch
	f.applyMu.Unlock()
}

// registerToolSessionShutdown stashes cancel keyed by the session so Stop
// can invoke it on teardown. Mirrors the registerOrchestrator pattern: a
// small per-session registry the factory's lifecycle hooks consult to keep
// auxiliary goroutines from leaking past the runner they belong to.
func (f *InProcessRunnerFactory) registerToolSessionShutdown(sess *spiceboxv1alpha1.AgentSession, cancel context.CancelFunc) {
	f.toolSessionShutdownMu.Lock()
	if f.toolSessionShutdowns == nil {
		f.toolSessionShutdowns = map[string]context.CancelFunc{}
	}
	f.toolSessionShutdowns[sessionKey(sess)] = cancel
	f.toolSessionShutdownMu.Unlock()
}

func sessionKey(sess *spiceboxv1alpha1.AgentSession) string {
	return sess.Namespace + "/" + sess.Name
}

// subscribeFactoryLeakageApplied subscribes to the generic interaction_applied
// envelope for this session and routes the info_leakage category into the
// approval Orchestrator so the runner's leakage gate (AwaitDecision's
// leakagePostApprove) unblocks. Slice C2 flipped info_leakage off its legacy
// typed applied subscriber onto this generic interaction_applied path; the switch
// on Category keeps it partitioned from the other per-session subscriber
// (subscribeFactoryInteractionApplied handles identity_choice /
// content_inspection). Mirrors production's handleInteractionAppliedMessage.
func subscribeFactoryLeakageApplied(ctx context.Context, nc *nats.Conn, orch *approval.Orchestrator, ns, name string) {
	if nc == nil || orch == nil {
		return
	}
	subject := channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.KindInteractionApplied)
	sub, err := nc.Subscribe(
		subject,
		func(m *nats.Msg) {
			var env channelevents.Envelope
			if uerr := json.Unmarshal(m.Data, &env); uerr != nil {
				slog.Info("e2e leakage-applied: unmarshal envelope", "err", uerr.Error())
				return
			}
			var pl channelevents.InteractionAppliedPayload
			if uerr := json.Unmarshal(env.Payload, &pl); uerr != nil {
				slog.Info("e2e leakage-applied: unmarshal payload", "err", uerr.Error())
				return
			}
			if pl.Category != categories.InfoLeakage {
				return
			}
			// Shared mapper, same reason as the tool_approval subscriber above.
			d, deliver, _ := approval.FromApplied(pl)
			if !deliver {
				return
			}
			orch.DeliverDecision(pl.RequestRef, d)
		},
	)
	if err != nil {
		slog.Info("e2e leakage-applied subscribe failed", "err", err.Error())
		return
	}
	<-ctx.Done()
	_ = sub.Drain()
}

// subscribeFactoryInteractionApplied is the harness analog of internal/cmd/runner's
// subscribeInteractionApplied (specifically its handleInteractionAppliedMessage
// switch): it routes the generic KindInteractionApplied envelope (published on
// OUT by the channelsd pipeline's HandleInteractionDecision after the user
// resolves an interaction) into the approval Orchestrator the runner-side gate
// blocks on. It serves every runner-resuming category:
//   - identity_choice: the 3-way answer ("agent" | "userPassthrough" | "cancel")
//     rides in OutcomeText (the Applied payload has no Action field); the gate
//     switches on it — Approved/Denied alone is insufficient.
//   - content_inspection (Slice C1): a pure allow/deny gate; Outcome maps onto
//     Decision.Approved.
//
// Categories that resolve out-of-band (credential_link, portal_access, …) are
// ignored — they don't drive a runner gate. Kept in lockstep with production's
// handleInteractionAppliedMessage so the e2e path exercises the same dispatch.
// deliveredByADedicatedSubscriber names the categories another harness
// subscriber already resumes. It is a fact about how the HARNESS is wired, not
// about the categories themselves — production runs one subscription per runner
// process and needs no such exclusion.
var deliveredByADedicatedSubscriber = map[string]bool{
	categories.ToolApproval: true, // the cross-session Apply subscriber
	categories.InfoLeakage:  true, // subscribeFactoryLeakageApplied
}

func subscribeFactoryInteractionApplied(ctx context.Context, nc *nats.Conn, orch *approval.Orchestrator, ns, name string) {
	if nc == nil || orch == nil {
		return
	}
	subject := channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.KindInteractionApplied)
	sub, err := nc.Subscribe(
		subject,
		func(m *nats.Msg) {
			var env channelevents.Envelope
			if uerr := json.Unmarshal(m.Data, &env); uerr != nil {
				slog.Info("e2e interaction-applied: unmarshal envelope", "err", uerr.Error())
				return
			}
			var pl channelevents.InteractionAppliedPayload
			if uerr := json.Unmarshal(env.Payload, &pl); uerr != nil {
				slog.Info("e2e interaction-applied: unmarshal payload", "err", uerr.Error())
				return
			}
			// Harness TOPOLOGY, not resume policy: unlike a runner process, which
			// owns one subscription for all categories, the harness shares a NATS
			// connection across sessions and splits delivery three ways —
			// tool_approval resumes off the cross-session Apply subscriber and
			// info_leakage off subscribeFactoryLeakageApplied. Delivering them
			// here as well would resume the same gate twice, which shows up as an
			// extra model round rather than as an error.
			if deliveredByADedicatedSubscriber[pl.Category] {
				return
			}

			// The SAME dispatch production uses, not a copy of it. This used to
			// be its own switch over category names, with a comment claiming it
			// was kept in lockstep with cmd/runner's — it was not, and the drift
			// is what made two plan-gate bundles look like harness defects.
			d, deliver, registered := approval.FromApplied(pl)
			if !registered {
				slog.Info("e2e interaction-applied: unregistered category; nothing to resume",
					"category", pl.Category, "requestRef", pl.RequestRef)
				return
			}
			if !deliver {
				return // ResumeNone
			}
			orch.DeliverDecision(pl.RequestRef, d)
		},
	)
	if err != nil {
		slog.Info("e2e interaction-applied subscribe failed", "err", err.Error())
		return
	}
	<-ctx.Done()
	_ = sub.Drain()
}

// subscribeFactoryWake forwards channelsd's per-session KindUserMessage wakeup
// into the runner's await InboundCh, mirroring internal/cmd/runner's NATS inbound pump
// (natsRuntime.inboundCh). It is the harness half of the await-park/resume path:
// SendUserMessage → fake driver → channelsd pipeline appends the "inbox" turn +
// publishWakeup(KindUserMessage) → this subscriber → InboundCh →
// await_user_message resumes in-process. The send is non-blocking (InboundCh is
// buffered ≥1): a wakeup posted while the agent is mid-turn is coalesced with a
// pending one, never lost and never blocking the NATS callback.
func subscribeFactoryWake(ctx context.Context, nc *nats.Conn, inboundCh chan<- struct{}, ns, name string) {
	if nc == nil || inboundCh == nil {
		return
	}
	subject := channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name), channelevents.KindUserMessage)
	sub, err := nc.Subscribe(subject, func(_ *nats.Msg) {
		select {
		case inboundCh <- struct{}{}:
		default: // a wakeup is already pending and unconsumed — coalesce
		}
	})
	if err != nil {
		slog.Info("e2e await wake subscribe failed", "subject", subject, "err", err.Error())
		return
	}
	<-ctx.Done()
	_ = sub.Drain()
}

// subscribeFactoryInterruptRequest subscribes to inbound KindInterruptRequest
// envelopes for this session and routes them into loop.Interrupt, publishing
// the resulting KindInterruptApplied envelope on OUT so the outbound relay
// hands it to the channel kind's queued_messages sub-channel sender (see
// pkg/channels/channelkinds/fake's queued_messages sender + test/e2e/interrupt.go's
// Interrupt.Click / WaitInterruptApplied). Mirrors internal/cmd/runner/nats.go's
// subscribeInterruptRequest + handleInterruptRequest, but operates against
// the factory's nats.Conn directly (the runner side wraps it in a
// natsRuntime which isn't accessible to the factory) and is unconditional
// on the request's contents — unlike the Applied-routing subscribers above,
// there is no orchestrator Await to resume; loop.Interrupt itself decides
// whether anything was truthfully cancelled.
func subscribeFactoryInterruptRequest(ctx context.Context, nc *nats.Conn, loop *runner.Loop, ns, name string) {
	if nc == nil || loop == nil {
		return
	}
	subject := channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name), channelevents.KindInterruptRequest)
	sub, err := nc.Subscribe(
		subject,
		func(m *nats.Msg) {
			var env channelevents.Envelope
			if uerr := json.Unmarshal(m.Data, &env); uerr != nil {
				slog.Info("e2e interrupt-request: unmarshal envelope", "err", uerr.Error())
				return
			}
			var pl channelevents.InterruptRequestPayload
			if uerr := json.Unmarshal(env.Payload, &pl); uerr != nil {
				slog.Info("e2e interrupt-request: unmarshal payload", "err", uerr.Error())
				return
			}
			// Bounded, not context.Background(): loop.Interrupt itself is fast,
			// but its best-effort per-tool Cancel teardown runs on this NATS
			// subscription goroutine, and a slow Cancel must not stall it
			// forever. Mirrors internal/cmd/runner/nats.go's handleInterruptRequest.
			interruptCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			out := loop.Interrupt(interruptCtx)
			cancel()
			outcome := "rejected"
			if out.Interrupted {
				outcome = "interrupted"
			}
			applied := channelevents.InterruptAppliedPayload{
				RequestID:   pl.RequestID,
				Outcome:     outcome,
				Reason:      out.Reason,
				ResponseURL: pl.ResponseURL,
			}
			if perr := channelevents.PublishOut(
				func(subject string, b []byte) error { return nc.Publish(subject, b) },
				ns, name, channelevents.KindInterruptApplied, applied,
			); perr != nil {
				slog.Info("e2e interrupt-applied publish failed",
					"session", ns+"/"+name, "requestID", applied.RequestID, "err", perr.Error())
			}
		},
	)
	if err != nil {
		slog.Info("e2e interrupt-request subscribe failed", "session", ns+"/"+name, "err", err.Error())
		return
	}
	<-ctx.Done()
	_ = sub.Drain()
}

// subscribeFactoryUIDataBinding subscribes to inbound KindUIDataBinding
// requests for this session and REPLIES synchronously (m.Respond) with the
// AppToolCallResponse produced by loop.HandleUIDataBinding — the harness's
// mirror of internal/cmd/runner/nats.go's subscribeUIDataBinding, operating against
// the factory's nats.Conn directly (the runner side wraps it in a
// natsRuntime which isn't accessible to the factory). This is what makes
// pkg/web/webui/agentui's "tool"-sourced data bindings resolvable against the
// harness's real in-process runner: HandleUIDataBinding runs the SAME
// readonly-gated, interact-re-authorized app-tool dispatch a production
// runner pod answers — nothing between this responder and the upstream MCP
// tool is a stub.
//
// ia is the runner's own independent interact re-check (D-B4), same as
// internal/cmd/runner/main.go's spdbCli argument. A nil ia makes HandleUIDataBinding
// fail closed (deny).
//
// Returns when ctx is cancelled. Subscribe/respond errors are logged but not
// fatal — a session without a live data-binding responder simply cannot
// resolve browser data bindings; it is not otherwise degraded.
func subscribeFactoryUIDataBinding(ctx context.Context, nc *nats.Conn, loop *runner.Loop, ia runner.InteractChecker, ns, name string) {
	if nc == nil || loop == nil {
		return
	}
	subject := channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name), channelevents.KindUIDataBinding)
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		resp := loop.HandleUIDataBinding(ctx, ia, ns, name, m.Data)
		b, merr := json.Marshal(resp)
		if merr != nil {
			slog.Info("e2e ui_data_binding: marshal response failed", "session", ns+"/"+name, "err", merr.Error())
			return
		}
		if rerr := m.Respond(b); rerr != nil {
			slog.Info("e2e ui_data_binding respond failed", "session", ns+"/"+name, "status", resp.Status, "err", rerr.Error())
		}
	})
	if err != nil {
		slog.Info("e2e ui_data_binding subscribe failed", "session", ns+"/"+name, "err", err.Error())
		return
	}
	<-ctx.Done()
	_ = sub.Drain()
}

// subscribeFactoryUIAction is subscribeFactoryUIDataBinding's action-surface
// sibling — the harness's mirror of internal/cmd/runner/nats.go's subscribeUIAction.
// Replies synchronously with the channelevents.UIActionResponse produced by
// loop.HandleUIAction, which additionally drives the ui_action lifecycle
// recorder (a side-effecting action's DETACHED outcome reaching memory and
// the browser) — see HandleUIAction's own doc comment.
func subscribeFactoryUIAction(ctx context.Context, nc *nats.Conn, loop *runner.Loop, ia runner.InteractChecker, ns, name string) {
	if nc == nil || loop == nil {
		return
	}
	subject := channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name), channelevents.KindUIAction)
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		resp := loop.HandleUIAction(ctx, ia, ns, name, m.Data)
		b, merr := json.Marshal(resp)
		if merr != nil {
			slog.Info("e2e ui_action: marshal response failed", "session", ns+"/"+name, "err", merr.Error())
			return
		}
		if rerr := m.Respond(b); rerr != nil {
			slog.Info("e2e ui_action respond failed", "session", ns+"/"+name, "state", resp.State, "err", rerr.Error())
		}
	})
	if err != nil {
		slog.Info("e2e ui_action subscribe failed", "session", ns+"/"+name, "err", err.Error())
		return
	}
	<-ctx.Done()
	_ = sub.Drain()
}

// credentialUpdateClient returns the direct client when one is wired, else the
// cached one. See DirectK8s for why the distinction matters here specifically.
func (f *InProcessRunnerFactory) credentialUpdateClient() client.Client {
	if f.DirectK8s != nil {
		return f.DirectK8s
	}
	return f.K8s
}

// subagentSendFunc mirrors internal/cmd/runner's own: reply_to_subagent
// publishes ONE agent_message_send envelope on the SENDING session's own
// inbound subject, naming the destination child in the payload, and
// channelsd's HandleAgentMessageSend delivers it through the Channel joining
// the pair.
//
// Publishing on the sender's own prefix is not a harness convenience — it is
// the only subject a runner's per-session NATS grant authorizes
// (runnerNATSUserGrant), so a harness that published straight onto the
// child's inbound subject would be testing a configuration production cannot
// run.
//
// nil when the harness wired no NATS: the meta tool then refuses the call
// with its own message rather than reporting a delivery that never happened.
//
// signer mirrors internal/cmd/runner/nats.go's identical parameter: the
// channelsd pipeline's envelopeVerifier (pkg/channels/channelsd/pipeline)
// refuses an agent_message_send envelope that carries no session signature,
// so an unsigned harness send would be refused by the SAME fail-closed gate
// production traffic goes through, not a harness-only difference.
func (f *InProcessRunnerFactory) subagentSendFunc(sess *spiceboxv1alpha1.AgentSession, signer *channelevents.EnvelopeSigner) func(ctx context.Context, childNS, childName, text string) error {
	if f.NATS == nil {
		return nil
	}
	ns, name := sess.Namespace, sess.Name
	return func(_ context.Context, childNS, childName, text string) error {
		return signer.PublishIn(
			func(subject string, payload []byte) error { return f.NATS.Publish(subject, payload) },
			ns, name, channelevents.KindAgentMessageSend,
			channelevents.AgentMessageSendPayload{
				To:   channelevents.SessionRef{Namespace: childNS, Name: childName},
				Text: text,
			},
		)
	}
}
