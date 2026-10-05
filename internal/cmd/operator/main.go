package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	runtimedebug "runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxextv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	_ "github.com/authzed/openagentprimitives/pkg/agent/harness/apnative"     // register the default harness
	_ "github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability" // register meta-tool capabilities for AgentClass validation
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/css"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/image"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/oap" // the operator renders the workshop draft's bundle-passthrough kind
	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	"github.com/authzed/openagentprimitives/pkg/web/admind"

	// mcpui is registered ONLY here (the artifactrender controller's
	// registry.ByKind dispatch target for the runner's applyUIResource-created
	// CRs, straight from an MCP tool's structured UIResource field — never
	// through the agent-facing artifact_prepare meta tool). The agent's own
	// production menu is gated by Renderer.AgentSelectable() (mcpui and oap
	// both answer false, so AvailableAssetKinds skips them regardless of which
	// process the kind is registered in — that is the real gate); keeping this
	// blank import out of internal/cmd/runner and internal/cmd/webd, unlike
	// css/html/image/svg which are imported into all three, is defence in
	// depth — a second reason an agent process can never reach kind="mcpui",
	// not the reason. See pkg/channels/channelassets/mcpui's package doc.
	goalmodel "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	eventnative "github.com/authzed/openagentprimitives/pkg/agent/sessionevents/native"
	"github.com/authzed/openagentprimitives/pkg/authz"
	_ "github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/promptinjection" // register for settings-webhook content-inspector validation
	_ "github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/urlallowlist"    // register for settings-webhook content-inspector validation
	guardianschema "github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/authz/handoff"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/cli"   // register cli kind for settings webhook kind validation
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/image" // register image kind for settings webhook kind validation
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/mcp"   // register mcp kind for settings webhook kind validation
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/oap"   // register oap kind for settings webhook kind validation
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/skill" // register skill kind for settings webhook kind validation
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate/hold"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	_ "github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource/imports" // completes the relsource claim table before any guarded write (BootstrapSource, pttagmint, ...) is checked
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/mcpui"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/svg"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"       // register agent kind for channel-controller validation
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"       // register bento kind for channel-controller validation
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"     // register browser kind for channel-controller validation
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"        // register fake kind for channel-controller validation
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"      // register github kind for channel-controller validation + WebhookURLDrift; the SAME import also runs relsync_kind.go's init(), registering github's relsync.Kind for the relationshipsource controller (relsync.Get) — also reachable transitively via the relsource/imports blank import above, but this is the explicit wiring site for THIS aspect, not an accident of that one
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"       // register local kind for channel-controller validation
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/onepassword" // register onepassword relsync.Kind for the relationshipsource controller (relsync.Get); also reachable transitively via the relsource/imports blank import above, but this is the explicit wiring site for THIS aspect, not an accident of that one
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"       // register slack kind for channel-controller validation
	"github.com/authzed/openagentprimitives/pkg/cli/clikit"
	agentclassctrl "github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentidentity"
	agentsessionctrl "github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentui"
	"github.com/authzed/openagentprimitives/pkg/controllers/artifactrender"
	channelctrl "github.com/authzed/openagentprimitives/pkg/controllers/channel"
	clusteridpctrl "github.com/authzed/openagentprimitives/pkg/controllers/clusteridentityprovider"
	clusterskillctrl "github.com/authzed/openagentprimitives/pkg/controllers/clusterskill"
	clusterskillsourcectrl "github.com/authzed/openagentprimitives/pkg/controllers/clusterskillsource"
	"github.com/authzed/openagentprimitives/pkg/controllers/credentialupdaterequest"
	goalctrl "github.com/authzed/openagentprimitives/pkg/controllers/goals"
	guardianctrl "github.com/authzed/openagentprimitives/pkg/controllers/guardian"
	"github.com/authzed/openagentprimitives/pkg/controllers/inboxwake"
	"github.com/authzed/openagentprimitives/pkg/controllers/mcpserver"
	monitoringctrl "github.com/authzed/openagentprimitives/pkg/controllers/monitoring"
	publicendpointctrl "github.com/authzed/openagentprimitives/pkg/controllers/publicendpoint"
	relationshipsourcectrl "github.com/authzed/openagentprimitives/pkg/controllers/relationshipsource"
	"github.com/authzed/openagentprimitives/pkg/controllers/sessionhold"
	settingsctrl "github.com/authzed/openagentprimitives/pkg/controllers/settings"
	"github.com/authzed/openagentprimitives/pkg/controllers/sidecartoolbox"
	skillctrl "github.com/authzed/openagentprimitives/pkg/controllers/skill"
	skillsourcectrl "github.com/authzed/openagentprimitives/pkg/controllers/skillsource"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxtoolchain"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxtoolkit"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxtoolspec"
	"github.com/authzed/openagentprimitives/pkg/controllers/subagentrequest"
	"github.com/authzed/openagentprimitives/pkg/controllers/toolcall"
	"github.com/authzed/openagentprimitives/pkg/controllers/useridentity"
	websession "github.com/authzed/openagentprimitives/pkg/controllers/webhooks/agentsession"
	webgoalexecution "github.com/authzed/openagentprimitives/pkg/controllers/webhooks/goalexecution"
	websettings "github.com/authzed/openagentprimitives/pkg/controllers/webhooks/settings"
	webskill "github.com/authzed/openagentprimitives/pkg/controllers/webhooks/skill"
	websubagentreq "github.com/authzed/openagentprimitives/pkg/controllers/webhooks/subagentrequest"
	webtoolcall "github.com/authzed/openagentprimitives/pkg/controllers/webhooks/toolcall"
	webworkshop "github.com/authzed/openagentprimitives/pkg/controllers/webhooks/workshop"
	webworkspacejob "github.com/authzed/openagentprimitives/pkg/controllers/webhooks/workspacejob"
	workshopctrl "github.com/authzed/openagentprimitives/pkg/controllers/workshop"
	workshopprobectrl "github.com/authzed/openagentprimitives/pkg/controllers/workshopprobe"
	"github.com/authzed/openagentprimitives/pkg/controllers/workspacesource"
	"github.com/authzed/openagentprimitives/pkg/controllers/workspacevolume"
	memorypkg "github.com/authzed/openagentprimitives/pkg/memory"
	goalinmem "github.com/authzed/openagentprimitives/pkg/memory/goals/inmem"
	goalpostgres "github.com/authzed/openagentprimitives/pkg/memory/goals/postgres"
	goalsqlite "github.com/authzed/openagentprimitives/pkg/memory/goals/sqlite"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	kggraphiti "github.com/authzed/openagentprimitives/pkg/memory/kg/graphiti"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all" // register all memory Kinds + their server-side hooks
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/kgingestion"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttag"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/tool_dispatch_snapshot"
	"github.com/authzed/openagentprimitives/pkg/memory/pools"
	mempostgres "github.com/authzed/openagentprimitives/pkg/memory/postgres"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/publisherkeys"
	memsearch "github.com/authzed/openagentprimitives/pkg/memory/search"
	searchembedding "github.com/authzed/openagentprimitives/pkg/memory/search/embedding"
	graphitisearch "github.com/authzed/openagentprimitives/pkg/memory/search/graphiti"
	searchinmem "github.com/authzed/openagentprimitives/pkg/memory/search/inmem"
	pgsearch "github.com/authzed/openagentprimitives/pkg/memory/search/postgres"
	searchsqlite "github.com/authzed/openagentprimitives/pkg/memory/search/sqlite"
	eventsql "github.com/authzed/openagentprimitives/pkg/memory/sessionevents/sqlstore"
	memshadow "github.com/authzed/openagentprimitives/pkg/memory/shadow"
	"github.com/authzed/openagentprimitives/pkg/memory/spicedbauthorizer"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	eventweb "github.com/authzed/openagentprimitives/pkg/web/sessionevents"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/deplogs"
	"github.com/authzed/openagentprimitives/pkg/platform/extract/extractordclient"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/loader"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker/inproc"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/githubapp"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports" // register the static/oauth/federated/githubApp credkind.Kinds the broker dispatches to via registry.Get
	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation/idjag"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/googlekind"   // register "google" idp.Kind: ClusterIdentityProvider validity controller dispatches via registry.Get
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/oidckind"     // register "oidc" idp.Kind
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/passwordkind" // register "password" idp.Kind (NOT fakekind — test-only)
	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
	"github.com/authzed/openagentprimitives/pkg/platform/nats/subjects"
	"github.com/authzed/openagentprimitives/pkg/platform/startup"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
	_ "github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/git" // register "git" workspacekinds.Kind: WorkspaceSource controller dispatches via registry.Get
	execremote "github.com/authzed/openagentprimitives/pkg/tools/exec/remote"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	_ "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/agentsandbox" // register the agent-sandbox backend (BYO; skipped when its CRDs are absent)
	_ "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod"          // register the built-in sandbox backend
	sandboxregistry "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/tools/skillbundle"
	skillbundlemem "github.com/authzed/openagentprimitives/pkg/tools/skillbundle/memory"
	skillbundlepg "github.com/authzed/openagentprimitives/pkg/tools/skillbundle/postgres"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/skillfetch"
	_ "github.com/authzed/openagentprimitives/pkg/tools/toolchain/kinds/image" // register image toolchain delivery kind
	toolspecregistry "github.com/authzed/openagentprimitives/pkg/tools/toolspec/registry"
	"github.com/authzed/openagentprimitives/pkg/web/gateway"
	gatewayv1 "github.com/authzed/openagentprimitives/pkg/web/gateway/v1"
	goalweb "github.com/authzed/openagentprimitives/pkg/web/goals"
	_ "github.com/authzed/openagentprimitives/pkg/web/localtunnel/ngrok" // register "ngrok" localtunnel provider: PublicEndpoint controller dispatches via registry.Get (stub is test-only, not registered here)
	localtunnelregistry "github.com/authzed/openagentprimitives/pkg/web/localtunnel/registry"
	"github.com/authzed/openagentprimitives/pkg/web/secretoutsrv"
	"github.com/authzed/openagentprimitives/pkg/web/workshopdraftsrv"
	"github.com/authzed/openagentprimitives/pkg/web/workshopprojectsrv"
	"github.com/authzed/openagentprimitives/pkg/web/workshopthreadsrv"
	"github.com/authzed/openagentprimitives/pkg/web/workshoptranscriptsrv"
	"github.com/authzed/openagentprimitives/pkg/x/debug"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// TODO(ha): the operator is a SINGLE-REPLICA component and several things
// quietly depend on that. This block is the list of what would have to change
// before `replicas: 2`, written down because none of it announces itself: every
// item below fails silently, produces wrong data, or wedges a pod, and none of
// it is caught by a compiler, a linter, or the current test suites.
//
// config/manager/deployment.yaml pins replicas: 1, and
// TestOperatorDeploymentStaysSingleReplica (pkg/platform/manifests) fails if
// that changes, so the first symptom of someone enabling HA is a red test
// pointing here. That is deliberate. Do not relax the test to make HA "work";
// work through this list.
//
// 1. APPEND-ONLY WRITE-ONCE IS ENFORCED BY A PROCESS-LOCAL LOCK.
// memory.Local.Put decides an append-only entry is new with a backend Get and
// then stores it with a backend Put; appendOnlyWriteLocks
// (pkg/memory/appendonlylock.go) is a striped keyed mutex making that span
// atomic. It is a complete answer today only because every append-only write in
// the cluster reaches this one process — the runner, channelsd, authzd and webd
// all write over the memory HTTP API into the facade constructed in run(). Two
// replicas mean two independent lock arrays, both missing the same pre-check
// Get, and a write silently overwritten with no error and no log line; the
// damage shows up much later as a chain `oap audit verify` cannot tell from
// tampering.
//
// The fix is an atomic compare-and-set in the store, not a bigger lock:
// PutIfAbsent(ctx, e) (existing memory.Entry, stored bool, err error) on
// memory.Backend, with the facade calling it in place of Get-then-Put and
// deriving the idempotent-re-put / conflict answer from (existing, stored).
// Four implementations: inmem under its existing mutex; sqlite and postgres as
// INSERT ... ON CONFLICT DO NOTHING plus a read-back of the row that won
// (the read-back is not optional — the caller needs the stored entry to decide
// idempotent-vs-conflict, and postgres will not RETURNING a row it did not
// insert); shadow delegating to its primary and dual-writing the secondary as
// it does now. Keep the lock as well: it is the cheap in-process path and
// costs nothing.
//
// 2. THE MEMORY PVC IS ReadWriteOnce, SO TWO PODS CANNOT BOTH MOUNT IT.
// config/manager/pvc.yaml declares spicebox-operator-memory as ReadWriteOnce,
// and that is already why the Deployment uses strategy: Recreate (a rolling
// update deadlocks on the volume — see the comment there). The volume holds the
// sqlite database used when MEMORY_BACKEND=sqlite, which the base config does
// NOT select — it ships postgres, and `oap init --local` is what overrides to
// sqlite. So HA needs one of: postgres-only operation (drop the PVC and refuse
// to start under MEMORY_BACKEND=sqlite when replicas > 1), an RWX class, or
// per-replica storage via a StatefulSet volumeClaimTemplate. That choice also
// answers whether sqlite stays a supported backend under HA at all, and it
// should be answered explicitly rather than falling out of whatever the
// manifest happens to say.
//
// 3. LEADER ELECTION IS ALREADY ON; THE WORK IS CLASSIFYING EACH SUBSYSTEM.
// The --leader-elect flag defaults to false (newCommand, below), but
// config/manager/deployment.yaml passes --leader-elect=true, so every shipped
// install already elects. Nothing to turn on. What is missing is a decision per
// subsystem about what a NON-LEADER replica may run, and the current answers
// are wrong in at least two places:
//
//   - Controller reconcilers: leader-only, by controller-runtime construction.
//     Correct as-is.
//   - Admission webhooks (:9443), metrics (:8080) and health probes (:8081):
//     served by every replica, which is required — a Service fronts them.
//     Correct as-is.
//   - The debug/memory HTTP server (:8082): started as a plain goroutine before
//     mgr.Start, so it serves from every replica. That is also required — it is
//     the memory data plane, and the spicebox-operator Service selects all
//     operator pods. But its authentication is a PROCESS-LOCAL registry (see 5),
//     so a non-leader would 401 every runner it serves.
//   - The gRPC streaming gateway (:8443): same shape, same problem — the
//     gateway.Registry is process-local while spicebox-gateway load-balances
//     across pods, so a consumer can land on the replica that has no stream.
//   - admind's live view: mounted on the debug handler and therefore served
//     everywhere, but its AgentSession informer is registered with
//     mgr.Add(manager.RunnableFunc(...)), and a RunnableFunc does not implement
//     LeaderElectionRunnable, so controller-runtime treats it as LEADER-ONLY. A
//     non-leader would serve the admin API from an empty session aggregator.
//   - The NATS subscriptions (revocation, and admind's envelope tree) are plain
//     subscribes, not queue groups, so every replica receives every message.
//     For revocation that is exactly right — each process must invalidate its
//     own broker cache. For anything added later that performs an ACTION rather
//     than updating local state, a plain subscribe means it happens N times.
//
// 4. ONCE-ONLY STARTUP STEPS IN A SERVING CONTAINER.
// AGENTS.md has a whole section on this failure class, and the SpiceDB
// --datastore-bootstrap-files crashloop it records is the cautionary tale: safe
// at one replica, permanently fatal at two, invisible in the mode everyone
// tests. Every step this process performs before mgr.Start needs re-asking
// "is every boot a first boot?". Three that are already known:
//
//   - openPostgresWithRetry runs Migrate, which Execs the whole DDL script
//     (pkg/memory/postgres/schema.go). It is idempotent by intent — every
//     statement is IF NOT EXISTS — but idempotent is not concurrency-safe:
//     concurrent CREATE TABLE/INDEX IF NOT EXISTS in Postgres can still raise a
//     duplicate-object error from the catalog's unique index. Wrap it in a
//     pg_advisory_lock, or move migration to a Job.
//   - probeArtifactStore Put/Get/Deletes the FIXED key "_ap/startup-probe".
//     Two replicas probing at once delete each other's object between the other's
//     Put and Get; the retry is bounded and the ceiling is os.Exit(1). Make the
//     probe key unique per process.
//   - debug.EnsureToken is already correct, and is the shape the other two want:
//     it Creates, and on IsAlreadyExists re-reads what the winner wrote.
//
// 5. IN-MEMORY STATE THAT IS THE ONLY RECORD OF SOMETHING.
// The audit tooling has a durability lens for exactly this (state whose loss on
// restart is silent and unrecoverable). HA makes it strictly worse: state that
// merely did not survive a restart now also DIVERGES between two live replicas,
// so the failure depends on which pod the Service picked. The two that matter
// most:
//
//   - tokens.Registry (memTokens): the per-session memory bearer tokens the
//     /memory endpoints authenticate against. It is written by the AgentSession
//     reconciler (r.Tokens.Set) — leader-only — and read by the HTTP handler on
//     every replica. At two replicas a runner's request round-robins onto a pod
//     that has never seen its token and gets a 401 roughly half the time. It
//     needs to be derived from a durable source every replica can read (the
//     per-session Secret it is already rehydrated from after a restart), not
//     populated as a side effect of reconciling.
//   - gateway.Registry: stream registrations, process-local, as in 3.
//
// 6. PROVENANCE: THE KEYS ARE FINE, THE CHAIN IS NOT.
// Each process mints its own Ed25519 keypair at startup and registers it under
// the publisher "system:operator" with keyID = the key's content address (see
// the mint below, and configMapKeyRegistrar). Several keys for one publisher is
// explicitly supported: publisherkeys.Registry.Add is additive across keyIDs
// (that is how rotation works), the ConfigMap write is an additive
// read-modify-write under RetryOnConflict, and provenance verification looks up
// (publisher, keyID) with no expectation of a single key. So two replicas would
// each register a key and both would verify. No work needed there.
//
// The chain is the problem. provenance.Signer keeps seq and prevHash per
// (scope, publisher) in memory, seeded from durable storage, and
// SigningMemory's per-scope lock that makes Sign+Put atomic is likewise
// process-local. Two replicas signing as the SAME publisher for one scope will
// both seed to the same head and both mint seq N+1 — a fork, which
// `oap audit verify` reports as VerdictFork and cannot distinguish from
// tampering. Give each replica its own publisher identity instead —
// "system:operator/<pod>" or similar. The pod name is NOT plumbed in today:
// config/manager/deployment.yaml projects POD_NAMESPACE through the downward
// API and nothing else, so that stanza has to grow a POD_NAME sibling and a
// flag beside --pod-namespace. Per-replica identity makes each replica its own
// chain: ChainHeads is already keyed by publisher, verification
// already walks per (publisher, scope), and checkAuthor binds an in-process
// operator write by signature alone with no hardcoded name to update. The cost
// is that AgentSession.status.auditChainHeads and any consumer that assumes one
// operator chain per scope have to accept several.
var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(spiceboxv1alpha1.AddToScheme(scheme))
	// metrics.k8s.io/v1beta1 (PodMetrics) backs admind's cluster-health rollup
	// CPU/Memory: the health checker Lists PodMetrics via the manager's uncached
	// reader, so the types must be in the manager scheme. Absent metrics-server,
	// the List degrades to "n/a" (see pkg/web/admind/health) — registering the type
	// costs nothing when the API is unserved.
	utilruntime.Must(metricsv1beta1.AddToScheme(scheme))
	// Registered unconditionally: adding types to the scheme is harmless
	// without the CRDs. Only WATCHING and REQUESTING them is gated, by
	// NewRuntime's availability check.
	utilruntime.Must(sandboxv1beta1.AddToScheme(scheme))
	// A DIFFERENT API group from the line above (extensions.agents.x-k8s.io vs
	// agents.x-k8s.io) shipping as a separate CRD bundle: SandboxClaim /
	// SandboxWarmPool / SandboxTemplate, the pre-warming types the agent-sandbox
	// backend claims through. Same reasoning — the scheme entry is inert, and a
	// missing one would surface only as "no kind is registered for the type" on
	// the session hot path, long after startup.
	utilruntime.Must(sandboxextv1beta1.AddToScheme(scheme))
}

func gatewayPortFromAddr(addr string) string {
	if idx := strings.LastIndex(addr, ":"); idx >= 0 {
		return addr[idx+1:]
	}
	return "8443"
}

// setupOperatorLogger builds the operator's zap logger.
//
// CRITICAL: ctrl.Log is NOT this logger. pkg/authz/spicedb transitively imports
// spicedb's internal/logging, whose init() fulfills controller-runtime's
// delegating root sink with a no-op zerolog BEFORE main runs, and SetLogger is
// first-write-wins — so our ctrl.SetLogger is a silent no-op and anything logged
// through ctrl.Log goes to /dev/null. Symptom: klog leader-election lines keep
// flowing while every application log vanishes, and the operator looks dead.
//
// Use the returned logger DIRECTLY and hand it to the manager as
// ctrl.Options.Logger; controllers then receive it via log.FromContext(ctx),
// independent of the poisoned global.
func setupOperatorLogger() logr.Logger {
	zapLogger := zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr))
	// Best-effort: a no-op whenever a transitive dep's init() already won the
	// SetLogger race, but harmless to call.
	ctrl.SetLogger(zapLogger)
	return zapLogger
}

// operatorNamespace returns the operator's own namespace, from the
// --pod-namespace flag (bound to POD_NAMESPACE env via the downward API in the
// deployment) or the in-cluster ServiceAccount file. The flag value is passed
// in (cfg.podNamespace) so both this helper and the startup log read the same
// resolved value rather than re-reading the environment.
func operatorNamespace(podNamespace string) string {
	if podNamespace != "" {
		return podNamespace
	}
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err == nil {
		return strings.TrimSpace(string(data))
	}
	return "default"
}

// trustedImageRegistryFrom reverses apimage.Image.RegistryRef/DigestRef's
// composition — "<registry>/" + img.LocalRef() [+ "@" + digest] — to recover
// just the "<registry>" this operator was itself installed with, given one
// of its own resolved image-flag values (ref).
//
// Returns "" when ref carries no registry prefix at all: the exact bare
// "<name>:<version>" apimage.Image.LocalRef produces, which is what every
// first-party image flag defaults to and what a local/dev install (no
// `--image-registry`) leaves it as. A ref that does not even contain the
// image's own name (an operator run with a fully custom, unrelated
// --sandbox-image override) also returns "" — the caller then treats every
// SidecarToolbox image as local-dev-only, which is the fail-SAFE direction
// for a value this function could not confidently parse: it narrows what a
// workshop sidecar may pull rather than widening it.
func trustedImageRegistryFrom(img apimage.Image, ref string) string {
	marker := "/" + img.Name + ":"
	if i := strings.Index(ref, marker); i >= 0 {
		return ref[:i]
	}
	return ""
}

// loadNATSIdentity reads the install-time NATS trust material from the
// spicebox-nats-identity and spicebox-nats-tls Secrets in the operator
// namespace. It returns the account identity used to mint per-session runner
// user JWTs plus the server CA cert.
//
// Both Secrets are created by `oap install`. When either is absent — local dev,
// or a cluster bootstrapped without `oap install` — it returns (nil, nil, nil);
// the caller logs and leaves NATSIdentity nil, so channel-attached runners spawn
// without NATS creds rather than crashing the operator.
//
// sr MUST be the operator-wide SecretReader (allowlisted for spicebox-nats-*)
// so the read goes through the adoptguard tripwire, which detects any
// unintended read of a non-adopted, non-allowlisted Secret.
func loadNATSIdentity(ctx context.Context, sr *adoptguard.SecretReader, namespace string) (*apnats.Identity, []byte, error) {
	identitySec, err := sr.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "spicebox-nats-identity"})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("get spicebox-nats-identity secret: %w", err)
	}

	tlsSec, err := sr.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "spicebox-nats-tls"})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("get spicebox-nats-tls secret: %w", err)
	}

	// MintUser only consults AccountPublicKey + AccountSigningSeed.
	id := &apnats.Identity{
		AccountPublicKey:   string(identitySec.Data["account-public-key"]),
		AccountSigningSeed: identitySec.Data["account-signing-seed"],
	}
	if id.AccountPublicKey == "" || len(id.AccountSigningSeed) == 0 {
		return nil, nil, fmt.Errorf("spicebox-nats-identity secret is missing account-public-key or account-signing-seed")
	}
	caPEM := tlsSec.Data["ca.crt"]
	if len(caPEM) == 0 {
		return nil, nil, fmt.Errorf("spicebox-nats-tls secret is missing ca.crt")
	}
	return id, caPEM, nil
}

const (
	// publisherKeysConfigMap names the ConfigMap (in the operator
	// namespace) that persists component publisher keys for provenance
	// verify-on-write across operator restarts.
	publisherKeysConfigMap = "publisher-keys"
	// publisherKeysConfigMapField is the ConfigMap data key holding the
	// JSON array of publisherkeys.Entry.
	publisherKeysConfigMapField = "keys"

	// postgresConnectCeiling bounds the operator's in-process retry on the
	// postgres memory store at startup. Generous on purpose: it must outlast a
	// cold-node postgres image pull plus first-accept, because crash-looping
	// here cascades — dependents POST their publisher key to the operator and
	// crash-loop in turn. On ceiling we exit loud; CrashLoopBackoff is the
	// backstop.
	postgresConnectCeiling = 4 * time.Minute
)

// loadPublisherKeys reads the publisher-keys ConfigMap and decodes its
// keys field into entries. A missing ConfigMap is not an error (returns
// nil, nil) — it just means no component keys have been registered yet.
//
// cmr MUST be the operator-wide ConfigMapReader (allowlisted for
// publisher-keys): reads are routed through the adoptguard tripwire so any
// unintended read of a non-adopted, non-allowlisted ConfigMap is detected
// immediately. The guard uses its Reader (APIReader / uncached direct path)
// for allowlisted objects, so this is safe pre-cache-start.
func loadPublisherKeys(ctx context.Context, cmr *adoptguard.ConfigMapReader, namespace string) ([]publisherkeys.Entry, error) {
	cm, err := cmr.Get(ctx, types.NamespacedName{Namespace: namespace, Name: publisherKeysConfigMap})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get %s ConfigMap: %w", publisherKeysConfigMap, err)
	}
	raw := cm.Data[publisherKeysConfigMapField]
	if raw == "" {
		return nil, nil
	}
	var entries []publisherkeys.Entry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("decode %s.%s: %w", publisherKeysConfigMap, publisherKeysConfigMapField, err)
	}
	return entries, nil
}

// installComponentPublisherKeys loads the publisher-keys ConfigMap via the
// guarded ConfigMapReader and installs the keys into reg, returning the number
// installed. A read error is RETURNED, never swallowed: the caller must treat
// it as fatal. Continuing with an unpopulated registry makes verify-on-write
// reject every component append-only write — a total, silent outage surfacing
// only as a generic internal-error notice in the channel. An absent
// ConfigMap is not an error (no components have registered yet).
func installComponentPublisherKeys(ctx context.Context, cmr *adoptguard.ConfigMapReader, namespace string, reg *publisherkeys.Registry, log logr.Logger) (int, error) {
	entries, err := loadPublisherKeys(ctx, cmr, namespace)
	if err != nil {
		return 0, err
	}
	// A rejected entry (malformed, content-address mismatch, or conflict)
	// is not trusted and must be visible — never silently dropped. The
	// rest still load (one tampered entry must not blind the operator to
	// the legitimate keys).
	for _, rejErr := range reg.Load(entries) {
		log.Info("publisher-keys ConfigMap entry rejected; not trusted", "err", rejErr.Error())
	}
	return len(entries), nil
}

// openPostgresWithRetry connects to postgres and runs migrations, retrying with
// bounded backoff so a not-yet-ready postgres at startup does not crash the
// operator. open is the actual connect+migrate step, injected so tests drive the
// retry behavior without a real database.
func openPostgresWithRetry(ctx context.Context, log logr.Logger, ceiling time.Duration,
	open func(context.Context) (*mempostgres.Client, error)) (*mempostgres.Client, error) {
	var client *mempostgres.Client
	err := startup.Retry(ctx, "connect to postgres memory store", ceiling,
		func(ctx context.Context) error {
			c, e := open(ctx)
			if e != nil {
				return e
			}
			client = c
			return nil
		},
		func(attempt int, err error, next time.Duration) {
			log.Info("startup: postgres not ready, backing off",
				"attempt", attempt, "err", err, "retryIn", next.String())
		})
	if err != nil {
		return nil, err
	}
	return client, nil
}

// artifactProbeCeiling bounds the startup Put/Get/Delete self-check. Generous
// on purpose: a GCS IAM binding minted by `oap install` moments earlier can
// take ~a minute to propagate; crash-looping through that window is the
// failure mode pkg/platform/startup exists to avoid.
const artifactProbeCeiling = 2 * time.Minute

// probeArtifactStore exercises a full Put/Get/Delete round-trip against store,
// retrying with bounded backoff so a not-yet-ready backend (bucket still
// propagating IAM, PVC still mounting) at startup does not crash the operator.
func probeArtifactStore(ctx context.Context, log logr.Logger, store artifactstore.Store, url string) error {
	return startup.Retry(ctx, "artifact store readiness (Put/Get/Delete probe)", artifactProbeCeiling,
		func(ctx context.Context) error {
			attemptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			ref, err := store.Put(attemptCtx, "_ap/startup-probe", strings.NewReader("ok"))
			if err != nil {
				return fmt.Errorf("put: %w", err)
			}
			rc, err := store.Get(attemptCtx, ref)
			if err != nil {
				return fmt.Errorf("get: %w", err)
			}
			if _, err := io.Copy(io.Discard, rc); err != nil {
				_ = rc.Close()
				return fmt.Errorf("read: %w", err)
			}
			if err := rc.Close(); err != nil {
				return fmt.Errorf("close: %w", err)
			}
			if err := store.Delete(attemptCtx, ref); err != nil {
				return fmt.Errorf("delete: %w", err)
			}
			return nil
		},
		func(attempt int, err error, next time.Duration) {
			log.Info("startup: artifact store not ready, backing off",
				"url", url, "attempt", attempt, "err", err, "retryIn", next.String())
		})
}

// compositeKeyLookup resolves a (publisher, keyID) against the
// per-session tokens registry first, then the component publisherkeys
// registry. It implements provenance.PublisherKeyLookup.
type compositeKeyLookup struct {
	tokens  *tokens.Registry
	pubKeys *publisherkeys.Registry
}

func (c compositeKeyLookup) PublisherKey(publisher, keyID string) (ed25519.PublicKey, bool) {
	if pub, ok := c.tokens.PublisherKey(publisher, keyID); ok {
		return pub, true
	}
	return c.pubKeys.PublisherKey(publisher, keyID)
}

// configMapKeyRegistrar persists a component publisher key into the
// in-memory publisherkeys registry AND the publisher-keys ConfigMap
// (read-modify-write, additive — keys are never removed). It implements
// httpsrv.PublisherKeyRegistrar.
type configMapKeyRegistrar struct {
	client    client.Client
	namespace string
	registry  *publisherkeys.Registry
	log       logr.Logger
}

func (r configMapKeyRegistrar) RegisterPublisherKey(ctx context.Context, publisher, keyID string, pub ed25519.PublicKey) error {
	// In-memory first so the running operator sees the key immediately,
	// even if the ConfigMap write below races a concurrent registration.
	// Fail closed: a content-address mismatch or a first-write-wins
	// conflict means the caller is trying to register a forged or
	// conflicting key — refuse before it reaches the ConfigMap.
	if err := r.registry.Add(publisher, keyID, pub); err != nil {
		return fmt.Errorf("register publisher key in-memory: %w", err)
	}

	entry := publisherkeys.Entry{
		Publisher: publisher,
		KeyID:     keyID,
		PubKeyB64: base64.StdEncoding.EncodeToString(pub),
	}

	var cm corev1.ConfigMap
	err := r.client.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: publisherKeysConfigMap}, &cm)
	switch {
	case apierrors.IsNotFound(err):
		body, mErr := json.Marshal([]publisherkeys.Entry{entry})
		if mErr != nil {
			return fmt.Errorf("marshal publisher-keys entry: %w", mErr)
		}
		create := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: publisherKeysConfigMap, Namespace: r.namespace},
			Data:       map[string]string{publisherKeysConfigMapField: string(body)},
		}
		if cErr := r.client.Create(ctx, create); cErr != nil {
			return fmt.Errorf("create %s ConfigMap: %w", publisherKeysConfigMap, cErr)
		}
		r.log.Info("registered component publisher key (new ConfigMap)", "publisher", publisher, "keyId", keyID)
		return nil
	case err != nil:
		return fmt.Errorf("get %s ConfigMap: %w", publisherKeysConfigMap, err)
	}

	// Read-modify-write: append unless an identical entry already exists.
	// Wrapped in RetryOnConflict so a concurrent Update from another
	// registrar re-fetches the latest ResourceVersion before retrying.
	if rErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if gErr := r.client.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: publisherKeysConfigMap}, &cm); gErr != nil {
			return fmt.Errorf("get %s ConfigMap: %w", publisherKeysConfigMap, gErr)
		}
		var existing []publisherkeys.Entry
		if raw := cm.Data[publisherKeysConfigMapField]; raw != "" {
			if uErr := json.Unmarshal([]byte(raw), &existing); uErr != nil {
				return fmt.Errorf("decode existing %s.%s: %w", publisherKeysConfigMap, publisherKeysConfigMapField, uErr)
			}
		}
		for _, e := range existing {
			if e.Publisher == entry.Publisher && e.KeyID == entry.KeyID && e.PubKeyB64 == entry.PubKeyB64 {
				return nil // idempotent: already persisted
			}
		}
		existing = append(existing, entry)
		body, mErr := json.Marshal(existing)
		if mErr != nil {
			return fmt.Errorf("marshal publisher-keys entries: %w", mErr)
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[publisherKeysConfigMapField] = string(body)
		return r.client.Update(ctx, &cm)
	}); rErr != nil {
		return fmt.Errorf("update %s ConfigMap: %w", publisherKeysConfigMap, rErr)
	}
	r.log.Info("registered component publisher key", "publisher", publisher, "keyId", keyID)
	return nil
}

// config holds the operator's non-secret configuration, populated from flags
// and (via clikit) the environment. Secret values (SPICEDB_TOKEN,
// EMBEDDING_API_KEY) and the GOTRACEBACK runtime knob are read directly from
// the environment in run() so they never appear in argv or --help.
type config struct {
	// Container-arg flags wired in config/manager/deployment.yaml — names and
	// defaults are load-bearing and must not change.
	metricsAddr            string
	probeAddr              string
	leaderElect            bool
	debugAddr              string
	gatewayAddr            string
	runnerImage            string
	sandboxImage           string
	imagePullSecret        string
	channelsdTokenFile     string
	webdTokenFile          string
	authzdTokenFile        string
	webhookCertDir         string
	snapshotImage          string
	snapshotServiceAccount string

	// Tunable flags (defaults preserved; not all are passed as container args).
	natsURL                       string
	defaultChannelArchiveAfter    time.Duration
	defaultSessionSleepAfter      time.Duration
	sessionStorageRetention       time.Duration
	nodePinnedStorageReclaimGrace time.Duration
	idleStorageReclaimAfter       time.Duration
	failedSandboxReapGrace        time.Duration
	oauthRefreshThreshold         time.Duration
	workspaceStorageClass         string
	workspaceSize                 string
	snapshotStoreSize             string
	workspaceBaseStorageClass     string
	workspaceBaseSize             string
	materializeImage              string
	sessionNetworkPolicies        bool
	sessionNamespaceDefaultDeny   bool
	planGateDenialStreakThreshold int
	trifectaClosureTripper        bool

	// Flags bound to their env names via clikit.EnvOverridePreRunE. SpiceDB and
	// PostgreSQL are deliberately NOT here: their connection params are resolved
	// by spicedb.LoadEnvConfig / mempostgres.LoadEnvConfig straight from the
	// environment (token-path resolution, required-var validation, secret
	// handling). An echo-only flag that did not drive the connection would be a
	// footgun, and POSTGRES_URI is secret-sourced.
	graphitiEndpoint   string
	embeddingEndpoint  string
	embeddingModel     string
	memoryBackend      string
	memoryReadSource   string
	kgIngestionStrat   string
	podNamespace       string
	artifactStoreURL   string
	clusterKind        string
	extractordEndpoint string

	// watchNamespaces optionally scopes the manager cache (informers + Owns
	// watches) to a set of namespaces. Empty = cluster-wide (the default,
	// matching most operators). When set, it is the prerequisite for running
	// with per-namespace RoleBindings instead of the cluster-wide
	// ClusterRoleBinding. See cacheNamespaces.
	watchNamespaces []string

	// secretGuardMode controls the adoptguard enforcement level for all
	// SecretReader/ConfigMapReader instances constructed in main. "warn" logs
	// non-adopted reads and continues (bring-up aid); "panic" makes any
	// non-adopted, non-allowlisted read fatal (production default once all
	// consumers are converted). Default: "warn" until the migration is complete.
	secretGuardMode string
}

func main() {
	// First statement: SpiceDB's schema compiler (reached from the guardian
	// controller's schema composer) logs a trace line per definition through
	// zerolog's process-global logger, and this binary's stderr is `kubectl
	// logs`. This does NOT touch the operator's own logging — setupOperatorLogger
	// below builds a zap logger and hands it to the manager explicitly; the
	// zerolog logger spicedb bridges into controller-runtime is a Nop. See
	// pkg/platform/deplogs.
	deplogs.Silence()

	// Earliest possible stderr breadcrumb: confirms the binary's main
	// function is being entered at all. If this line is absent from
	// container logs, the binary is dying during package init (e.g.
	// a transitive init() panic recovered by a parent), NOT during
	// main(). Without this it's impossible to distinguish "main ran
	// and exited silently" from "main was never called."

	// Top-level panic recovery: anything that escapes controller-runtime's
	// per-reconcile recovery (a panic in a goroutine we forgot to wrap, a
	// panic during mgr.Start setup, etc.) lands here. Without this, a
	// silent panic produces an exit-code-1 crash with NO log output, as
	// observed in the slice-2 silent-crash incident: only klog
	// leader-election lines appeared and the actual panic was lost.
	//
	// fmt.Fprintf is used in preference to ctrl.Log because the logger
	// may not be initialised yet at the panic site, and we want bytes
	// flushed to stderr before os.Exit.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "operator: panic at main: %v\n%s\n", r, runtimedebug.Stack())
			os.Exit(1)
		}
	}()

	// The operator installs its OWN signal handler inside run() via
	// ctrl.SetupSignalHandler() (handed to mgr.Start). That registration
	// panics if called twice, so main() uses a plain Execute() and does NOT
	// register a signal.NotifyContext context of its own.
	if err := newCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	cfg := &config{}
	cmd := &cobra.Command{
		Use:          "operator",
		Short:        "Spicebox control-plane operator: reconcilers, memory/search/KG stack, gateway, and validating webhooks.",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			run(cfg)
			return nil
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&cfg.metricsAddr, "metrics-bind-address", "127.0.0.1:8080",
		"Address for the metrics endpoint. The endpoint is unauthenticated, so the default binds loopback-only; widen it (e.g. \":8080\") only behind an authenticating or network-policy-enforced scrape path.")
	fs.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "Address for health probes.")
	fs.BoolVar(&cfg.leaderElect, "leader-elect", false, "Enable leader election.")
	fs.StringVar(&cfg.debugAddr, "debug-bind-address", ":8082", "Address for the debug HTTP server (artifact fetcher). Set to empty to disable.")
	fs.StringVar(&cfg.gatewayAddr, "gateway-bind-address", ":8443", "Address for the streaming gRPC gateway. Empty to disable.")
	fs.StringVar(&cfg.runnerImage, "runner-image", apimage.Runner.LocalRef(), "Container image for the agent runner pod.")
	fs.StringVar(&cfg.sandboxImage, "sandbox-image", apimage.Sandbox.LocalRef(), "Default container image for sandbox/bundle pods of a SpiceboxClass that does not pin spec.image.")
	fs.StringVar(&cfg.imagePullSecret, "image-pull-secret", "", "Optional imagePullSecret name added to operator-synthesized runner + sidecar pods (private registries).")
	fs.StringVar(&cfg.natsURL, "nats-url", "nats://spicebox-nats.agentprimitives-system.svc:4222", "NATS connection URL passed to runner pods.")
	fs.DurationVar(&cfg.defaultChannelArchiveAfter, "default-channel-archive-after", 4*time.Hour,
		"Default channel-attached AgentSession archive interval. Per-class override on AgentClass.spec.channels.archiveAfter. Set to 0 to disable archival entirely.")
	fs.DurationVar(&cfg.defaultSessionSleepAfter, "default-session-sleep-after", 10*time.Minute,
		"how long an idle channel session keeps its pods warm before the operator reaps them (0 = never sleep)")
	fs.DurationVar(&cfg.sessionStorageRetention, "session-storage-retention", 72*time.Hour,
		"How long a terminal (Succeeded/Failed) AgentSession's workspace + snapshot-store PVCs are kept past finishedAt before deletion. The session record itself is kept. Per-class override on AgentClass.spec.channels.storageRetention. Set to 0 to disable the sweep.")
	fs.DurationVar(&cfg.nodePinnedStorageReclaimGrace, "node-pinned-storage-reclaim-grace", 15*time.Minute,
		"When the workspace class is the node-local bundled class (ap-workspace-rwx), CAP a terminal session's workspace + snapshot-store PVC retention at this short grace instead of --session-storage-retention. Bounds the node-local disk-pressure deadlock: local-path PV bytes live on one node's disk with no quota. Kept as a debug window (not instant), and trades away restart-from-here/fork for such sessions. Set to 0 to disable the cap (use the normal retention for node-local too).")
	fs.DurationVar(&cfg.idleStorageReclaimAfter, "idle-storage-reclaim-after", 30*time.Minute,
		"When the workspace class is the node-local bundled class (ap-workspace-rwx), reclaim a SLEPT idle channel session's workspace + snapshot-store PVCs once it has been asleep this long, while keeping the session Idle and wakeable (wake re-provisions empty storage and re-clones). Bounds the node-local disk a parked, wakeable population holds: the idle-sleep reaper frees their CPU but keeps their PVCs, so without this their scratch accumulates until a node crosses its ephemeral-storage watermark. Ignored for cross-node RWX classes (no single node is at risk). Set to 0 to disable.")
	fs.DurationVar(&cfg.failedSandboxReapGrace, "failed-sandbox-reap-grace", time.Minute,
		"How long a Failed session's sandbox pods (bundle SpiceboxSessions + runner pod) are kept for debugging before teardown. The AgentSession itself is kept either way. Set to 0 (or negative) to disable reaping entirely (keep everything, for debugging).")
	fs.DurationVar(&cfg.oauthRefreshThreshold, "oauth-refresh-threshold", 5*time.Minute,
		"Default refresh threshold for type=oauth AgentIdentity credentials. AgentIdentities with spec.refreshThreshold override this per-identity.")
	fs.StringVar(&cfg.workspaceStorageClass, "workspace-storage-class", "",
		"RWX StorageClass for AgentSession shared-workspace PVCs. Empty disables shared workspaces (isolated fallback).")
	fs.StringVar(&cfg.workspaceSize, "workspace-size", "2Gi",
		"Requested size for AgentSession workspace PVCs. Raised to the workspace StorageClass's known minimum at PVC-create time (e.g. Filestore multishare's 10Gi share floor) so an undersized request cannot wedge a session.")
	fs.StringVar(&cfg.snapshotStoreSize, "snapshot-store-size", "",
		"Requested size for AgentSession snapshot-store PVCs (used by workspace snapshot/restore). EMPTY (the default) follows --workspace-size, so a session's node-local footprint stays workspace+workspace rather than workspace+8Gi — a fixed oversized default drove the node-local (local-path) class into disk pressure. Set a value only to override that. Raised to the workspace StorageClass's known minimum at PVC-create time, same as --workspace-size.")
	fs.StringVar(&cfg.workspaceBaseStorageClass, "workspace-base-storage-class", "", "RWX/RWO StorageClass for WorkspaceSource base PVCs. Empty → WorkspaceSource Ready=False (BaseStorageUnconfigured).")
	fs.StringVar(&cfg.workspaceBaseSize, "workspace-base-size", "2Gi", "Requested size for WorkspaceSource base PVCs.")
	fs.StringVar(&cfg.materializeImage, "materialize-image", "alpine/git:latest",
		"Container image (must contain git) for WorkspaceSource base-materialize Jobs. Override and pin a digest for production.")
	fs.BoolVar(&cfg.sessionNetworkPolicies, "session-network-policies", true,
		"Stamp pod-scoped NetworkPolicies (runner allowlist + sandbox deny) into each session namespace. Disable on clusters whose topology the stamped policies would break — e.g. external SpiceDB on a nonstandard port, or MCP servers reachable only over non-443 ports (including in-cluster http://*.svc URLs).")
	fs.BoolVar(&cfg.sessionNamespaceDefaultDeny, "session-namespace-default-deny", false,
		"Stamp a NAMESPACE-WIDE default-deny NetworkPolicy into each session namespace. OFF by default: it selects every pod in a namespace this operator does not own, so in a shared namespace it severs workloads unrelated to any agent -- that is the cluster operator's call. It is also the only control that covers a pod whose labels its creator chose; the per-session policies select session labels only, so anything else in the namespace matches no policy and NetworkPolicy leaves it unrestricted in both directions.")
	fs.IntVar(&cfg.planGateDenialStreakThreshold, "plangate-denial-streak-threshold", 0,
		"Number of CONSECUTIVE plan-gate denials (denied or, under logging mode, would_deny) that trips a forensic SessionHold. 0 (default) disables the tripper entirely.")
	fs.BoolVar(&cfg.trifectaClosureTripper, "trifecta-closure-tripper", false,
		"Freeze a session whose delegation closure holds all three trifecta legs — untrusted input, sensitive access, and the ability to act. OFF by default: this parks live sessions for human review, so it is switched on deliberately. Independent of the per-class trifecta.mode, which governs the runner's dispatch-time refusal; this is the operator-side containment judgement and re-derives its legs from what the operator can see for itself.")
	fs.StringVar(&cfg.webhookCertDir, "webhook-cert-dir", "/var/run/operator/webhook-certs",
		"directory holding tls.crt/tls.key for the validating webhook server")
	fs.StringVar(&cfg.channelsdTokenFile, "channelsd-memory-token-file",
		"/var/run/operator/channelsd-memory-token",
		"Path to the system channelsd memory token file")
	fs.StringVar(&cfg.authzdTokenFile, "authzd-memory-token-file",
		"/var/run/operator/authzd-memory-token",
		"Path to the system authzd memory token file")
	fs.StringVar(&cfg.webdTokenFile, "webd-memory-token-file",
		"/var/run/operator/webd-memory-token",
		"Path to the read-only webd memory token file")
	fs.StringVar(&cfg.snapshotImage, "snapshot-image", "busybox:1.36",
		"Container image for workspace snapshot/restore Jobs")
	fs.StringVar(&cfg.snapshotServiceAccount, "snapshot-service-account", "ap-snapshotter",
		"Service account workspace snapshot/restore Jobs run under")

	// Defaults are empty where the body applies its own fallback
	// (memory-read-source, kg-ingestion-strategy, pod-namespace). SpiceDB and
	// PostgreSQL are intentionally absent — resolved via their own LoadEnvConfig
	// from the environment; see the config struct comment.
	fs.StringVar(&cfg.graphitiEndpoint, "graphiti-endpoint", "", "Graphiti REST endpoint. When set, enables the graphiti search provider, KG provider, and KG ingestion.")
	fs.StringVar(&cfg.embeddingEndpoint, "embedding-endpoint", "", "OpenAI-compatible embedding endpoint. When set (with postgres), enables pgvector cosine search.")
	fs.StringVar(&cfg.embeddingModel, "embedding-model", "text-embedding-3-small", "Embedding model name for the OpenAI-compatible embedder.")
	fs.StringVar(&cfg.memoryBackend, "memory-backend", "", "REQUIRED memory backend selector: 'inmem' (pure in-memory), 'postgres' (shadow inmem+postgres dual-write, reads from postgres), or 'sqlite' (persistent single-file, requires MEMORY_SQLITE_PATH). Empty is a FATAL startup error (no silent default): the cluster bundle sets 'postgres', `oap init --local` sets 'sqlite'. 'postgres' requires POSTGRES_URI.")
	fs.StringVar(&cfg.memoryReadSource, "memory-read-source", "", "Optional shadow-backend read-source OVERRIDE: 'primary' (inmem) or 'secondary' (postgres). Empty defaults to 'secondary' under MEMORY_BACKEND=postgres (read durable postgres); set 'primary' only to temporarily read ephemeral inmem for debugging.")
	fs.StringVar(&cfg.kgIngestionStrat, "kg-ingestion-strategy", "", "Knowledge-graph ingestion strategy: every_turn (default), content_gated, or batch. Empty defaults to every_turn.")
	fs.StringVar(&cfg.podNamespace, "pod-namespace", "", "Operator's own namespace (downward API). Empty falls back to the in-cluster ServiceAccount namespace file, then 'default'.")
	fs.StringVar(&cfg.artifactStoreURL, "artifact-store-url", "",
		"REQUIRED artifact store URL: gs://<bucket> | s3://<bucket> | azblob://<container> | file:///<path> | mem:// (dev only). `oap install` injects this; unset fails startup.")
	fs.StringVar(&cfg.clusterKind, "cluster-kind", "",
		fmt.Sprintf("REQUIRED cluster kind stamped by `oap install` (one of: %s). Selects the InstallProfile that gates local-only-by-design controls (e.g. the password ClusterIdentityProvider kind). Empty or unrecognized is a FATAL startup error — no silent default; a Deployment predating this flag must be re-applied via `oap install`.", strings.Join(cloud.RegisteredKeys(), ", ")))
	fs.StringVar(&cfg.extractordEndpoint, "extractord-endpoint", "",
		"extractord HTTP endpoint (e.g. http://agentprimitives-extractord.agentprimitives-system.svc:8080). When set, enables inbound-attachment text extraction on the /inbound-asset route. Empty means unconfigured: bytes are still stored, but every attachment's channelsd-facing outcome is the TRANSIENT \"could not be read\" note, never the permanent \"unsupported type\" one and never a silent skip (see pkg/memory/httpsrv's WithAttachmentExtractor).")
	fs.StringSliceVar(&cfg.watchNamespaces, "watch-namespaces", nil,
		"Optional comma-separated list of namespaces to scope the cache (informers + Owns watches) to. Empty = cluster-wide (default). The operator's own namespace is always folded in. Set this to run with per-namespace RoleBindings instead of the cluster-wide ClusterRoleBinding; a cluster-wide list/watch would 403 under namespaced RBAC.")
	fs.StringVar(&cfg.secretGuardMode, "secret-guard-mode", "panic",
		"Adoptguard enforcement level for Secret/ConfigMap readers: 'panic' (a read of a non-adopted, non-allowlisted Secret/ConfigMap is fatal — the default) or 'warn' (log + continue, for bring-up). All operator reads go through the guard, so 'panic' makes any overreach loud and immediate.")

	cmd.PreRunE = clikit.EnvOverridePreRunE(map[string]string{
		"graphiti-endpoint":     "GRAPHITI_ENDPOINT",
		"embedding-endpoint":    "EMBEDDING_ENDPOINT",
		"embedding-model":       "EMBEDDING_MODEL",
		"memory-backend":        "MEMORY_BACKEND",
		"memory-read-source":    "MEMORY_READ_SOURCE",
		"kg-ingestion-strategy": "KG_INGESTION_STRATEGY",
		"pod-namespace":         "POD_NAMESPACE",
		"watch-namespaces":      "WATCH_NAMESPACES",
		"secret-guard-mode":     "SECRET_GUARD_MODE",
		"artifact-store-url":    "ARTIFACT_STORE_URL",
		"cluster-kind":          "AP_CLUSTER_KIND",
		"extractord-endpoint":   "EXTRACTORD_ENDPOINT",
	})
	return cmd
}

// Memory backend kinds selected by the MEMORY_BACKEND env/flag.
const (
	memoryBackendInmem    = "inmem"
	memoryBackendPostgres = "postgres"
	memoryBackendSqlite   = "sqlite"
)

// validateMemoryBackend resolves the explicit MEMORY_BACKEND selector to a
// concrete backend kind, failing closed. It is pure (no I/O) so the fail-closed
// rules are unit-testable without a live postgres:
//
//   - "" → error. There is NO silent default — an unset selector is a
//     configuration bug, not a request for ephemeral memory. (`oap init --local`
//     sets sqlite; the cluster bundle sets postgres.)
//   - "inmem" → ok (pure in-memory).
//   - "postgres" → requires a non-empty POSTGRES_URI, else error. The shadow
//     backend cannot dual-write/read postgres without a connection string.
//   - "sqlite" → requires a non-empty MEMORY_SQLITE_PATH, else error. The
//     SQLite backend persists to a single on-disk file (macOS bundle target).
//   - anything else → error.
func validateMemoryBackend(backend, postgresURI string) (string, error) {
	switch backend {
	case "":
		return "", fmt.Errorf(`MEMORY_BACKEND must be set to "inmem", "postgres", or "sqlite" (no silent default; oap init --local uses sqlite)`)
	case memoryBackendInmem:
		return memoryBackendInmem, nil
	case memoryBackendPostgres:
		if postgresURI == "" {
			return "", fmt.Errorf("MEMORY_BACKEND=postgres requires POSTGRES_URI")
		}
		return memoryBackendPostgres, nil
	case memoryBackendSqlite:
		if os.Getenv(memsqlite.EnvPath) == "" {
			return "", fmt.Errorf("MEMORY_BACKEND=sqlite requires MEMORY_SQLITE_PATH")
		}
		return memoryBackendSqlite, nil
	default:
		return "", fmt.Errorf("unknown MEMORY_BACKEND %q (want inmem|postgres|sqlite)", backend)
	}
}

// validateArtifactStoreURL enforces the fail-closed artifact store contract:
// no silent in-memory fallback. `oap install` always injects a value; a bare
// `kubectl apply` of the bundle intentionally lands here.
func validateArtifactStoreURL(u string) error {
	if u == "" {
		return errors.New("ARTIFACT_STORE_URL (or --artifact-store-url) is required: gs://<bucket>, s3://<bucket>, azblob://<container>, file:///<path>, or mem:// (dev only); run `oap install` to provision and inject it")
	}
	if !strings.Contains(u, "://") {
		return fmt.Errorf("ARTIFACT_STORE_URL %q has no scheme (want gs://, s3://, azblob://, file://, or mem://)", u)
	}
	return nil
}

// newAttachmentExtractor builds the operator's AttachmentExtractor from the
// extractord endpoint config. Declared as the interface return type — never
// *extractordclient.Client — so an empty endpoint is unrepresentable as
// anything but a TRUE nil interface: e is never assigned, so it keeps its
// zero value, and an interface's zero value is genuinely nil (not a
// {*extractordclient.Client, nil} tuple, which `== nil` would report false
// for). See AGENTS.md's "Nil interfaces" section and this function's call
// site in run() for the production outage this specific pattern documents.
func newAttachmentExtractor(endpoint string) httpsrv.AttachmentExtractor {
	var e httpsrv.AttachmentExtractor
	if endpoint != "" {
		e = extractordclient.New(endpoint)
	}
	return e
}

// newAttachmentExploder is newAttachmentExtractor's sibling for archives, and
// is deliberately the same shape for the same reason: declared as the
// INTERFACE and assigned only inside the if, so an empty endpoint is
// unrepresentable as anything but a true nil interface.
//
// One endpoint drives both, because both are extractord: an operator that
// configured extraction has configured archive fan-out, and a second flag
// would only create a state where a .zip behaves differently from a .pdf for
// no reason anyone chose.
func newAttachmentExploder(endpoint string) httpsrv.AttachmentExploder {
	var e httpsrv.AttachmentExploder
	if endpoint != "" {
		e = extractordclient.New(endpoint)
	}
	return e
}

// memHandlerDeps names the collaborators newMemHandlerOpts assembles the
// memory HTTP handler's options from. A struct rather than nine positional
// parameters: the fields have no meaningful order, and a struct lets a test
// build the exact same assembly run() uses without threading nine arguments
// through a call it doesn't otherwise care about.
type memHandlerDeps struct {
	K8sClient           client.Client
	ArtifactStore       artifactstore.Store
	MemLocal            *memorypkg.Local
	PubKeyRegistrar     httpsrv.PublisherKeyRegistrar
	AttachmentExtractor httpsrv.AttachmentExtractor
	ExtractordEndpoint  string
	SpiceDBClient       *spicedb.Client
	OpSigned            *provenance.SigningMemory
	// PoolsReader is the ALREADY-DERIVED spiceDBClient.Pools() view, not the
	// raw client SpiceDBClient above still is for the pt-tag options below it.
	// *spicedb.Client has no interface seam a test can substitute — its
	// methods hit a live gRPC connection — so if WithPools took d.SpiceDBClient
	// directly, no plain `go test` could ever supply a fake
	// pools.RelationshipReader and prove the resource-pool wiring; it could
	// only prove the pt-tag options compile. Pre-deriving it lets run() do the
	// one real spiceDBClient.Pools() call it always did, while a test passes a
	// stub here instead.
	PoolsReader pools.RelationshipReader
	KGProvider  memorypkg.KGProvider
}

// newMemHandlerOpts assembles the memory HTTP handler's options. Extracted
// out of run() so a test can exercise the SAME assembly the operator serves
// with, rather than a test building its own handler out of hand-picked
// options that main.go's real wiring could silently stop matching — the
// exact shape that let the pt-tag mint route (see pttag_minter_test.go's
// header comment) and, before this extraction, the resource-pool route (see
// pools_wiring_test.go) ship reachable-in-test and dead-in-production: every
// option existed and was individually tested, and nothing proved run()
// still called it.
//
// Pure assembly, no behaviour of its own: same options, same order, same
// conditional KG append as when this lived inline in run().
// memoryColdRegistryGrace is how long after this operator process starts an
// unregistered per-session bearer is answered 503 (retryable) rather than 401
// (permanent). The in-process token registry is empty on every boot and the
// AgentSession reconciler refills it per session (reregisterMemoryToken), so a
// live runner writing in that window would otherwise take a terminal 401 the
// instant the operator restarts — an OOM, a rollout — and fail its session with
// MemoryUnavailable. The memory httpclient retries 5xx across a ~17s budget, so
// a window comfortably shorter than this rides out with no session loss; 60s
// leaves headroom over the reconcile-refill lag (observed ~12s) without letting
// a genuinely forged bearer look transient for long. Only this production wiring
// opts in — every in-process test harness Sets tokens synchronously, has no
// window, and keeps the plain 401.
const memoryColdRegistryGrace = 60 * time.Second

func newMemHandlerOpts(d memHandlerDeps) []httpsrv.HandlerOption {
	opts := []httpsrv.HandlerOption{
		httpsrv.WithColdRegistryGrace(memoryColdRegistryGrace),
		httpsrv.WithArtifact(d.K8sClient, d.ArtifactStore),
		// GET /memory/_preferences and POST /memory/_preferences_commit both
		// resolve the AgentSession/AgentClass/AgentSettings CRs the preference
		// schema, admin globals, and lock policy come from — d.K8sClient reads
		// them the same way WithArtifact's CR lookups do; the commit route
		// writes only through h.mem (the user_preference memory Kind), never
		// through this reader.
		httpsrv.WithPreferences(d.K8sClient),
		// GET /memory/_preferences?user-ref=: subject-named reads. d.SpiceDBClient
		// is a real, non-nil *spicedb.Client by the time run() builds a
		// memHandlerDeps (see WithPtTagMinter's comment above for why), so
		// newSubjectResolveAdapter cannot produce a typed-nil interface.
		//
		// d.OpSigned, NOT d.MemLocal: preference_access is ComponentWritten AND
		// append-only, so it must be authored through the operator's own
		// signing facade — an unsigned Put through MemLocal would be refused by
		// the verify-on-write check the instant this route tried to use it, the
		// same reasoning WithPtTagMinter's Mem argument already documents.
		// WithPreferenceAudit's own doc explains why a nil here refuses
		// ?user-ref= outright rather than serving it unaudited — both options
		// are wired here together on purpose, not independently, so one is
		// never live without the other in this binary.
		httpsrv.WithSubjectResolution(newSubjectResolveAdapter(d.SpiceDBClient)),
		httpsrv.WithPreferenceAudit(d.OpSigned),
		// POST /memory/_preferences_firstparty_commit: the class-addressed
		// commit the Slack App Home (and any other first-party UI with no
		// session of its own) uses to save a subject's own preference value
		// directly. d.OpSigned, NOT d.MemLocal, for the identical reason the
		// line above is: preference_write is ComponentWritten AND append-only,
		// so an unsigned Put would be refused the instant the verify-on-write
		// check saw it carried no Provenance envelope.
		httpsrv.WithPreferenceWriteAudit(d.OpSigned),
		httpsrv.WithDeleter(d.MemLocal),
		httpsrv.WithReindexer(d.MemLocal),
		httpsrv.WithPublisherKeyRegistrar(d.PubKeyRegistrar),
		httpsrv.WithAttachmentExtractor(d.AttachmentExtractor),
		httpsrv.WithAttachmentExploder(newAttachmentExploder(d.ExtractordEndpoint)),
		// The pt-tag mint route. It is enabled HERE, in the operator, and
		// nowhere else: minting derives a tag's reader set server-side from the
		// resources named in the request, so the component doing it needs the
		// SpiceDB lookup, the relationship write, and the COMPONENT memory
		// credential — all three of which exist only in this process.
		//
		// The security property is that a session can never author a tag. A
		// tag's direct_reader set GRANTS disclosure, so a session credential
		// able to write one could name an audience its source never authorized.
		// The runner says which RESOURCES a call touched; this answers with who
		// may read the result. There is deliberately no request field for an
		// audience.
		//
		// d.SpiceDBClient is a real, non-nil *spicedb.Client by the time run()
		// builds a memHandlerDeps — its construction failure exits the process
		// earlier in run() — so assigning it into these interface fields cannot
		// produce a typed-nil interface, the same reasoning the PlatformLinker
		// wiring records.
		//
		// opSigned, NOT memLocal: pt_tag / pt_tag_content are append-only, so the
		// verifier (WithProvenanceVerifier, active on every Put through memLocal)
		// refuses any unsigned write of them — "signed provenance required". The
		// minter is the operator authoring these records AS ITSELF, exactly like
		// the lifecycle/restart writes opSigned already covers, so it must sign as
		// "system:operator". Handing it raw memLocal made every live mint 500 and
		// silently drop the datum to the coarse floor; the in-process e2e harness
		// signs its minter's facade the same way, which is why this only ever
		// failed on a real cluster.
		httpsrv.WithPtTagMinter(newPtTagMinter(newPtTagSpiceDBAdapter(d.SpiceDBClient), d.OpSigned)),
		// The content-resolve route: returns a datum's bytes to a caller entitled
		// to them (pt_tag#access). Gated operator-side because it returns content
		// — see newPtTagResolver. memLocal is fine here (the handler reads
		// pt_tag_content component-side itself); the SpiceDB access check is the
		// gate.
		httpsrv.WithPtTagResolver(newPtTagResolver(d.SpiceDBClient)),
		// Resource-memory pool discovery: mintPoolApprovals (pkg/memory/httpsrv)
		// consults this reader to find which resource pools a session's slot
		// grants reach, and handleSearch widens _search's Scopes with the read
		// ones. Everything downstream of it — the resource Scope kind, the
		// slot-grant discovery in pkg/memory/pools, the per-pool approval mint,
		// and this scope expansion — has existed and been tested for a while;
		// until this line, nothing ever called WithPools, so h.pools stayed a
		// true nil interface and every _search request got the session scope
		// alone, exactly as if resource pools did not exist. This is the call
		// that makes them reachable.
		//
		// d.PoolsReader is spiceDBClient.Pools() (see memHandlerDeps' doc for
		// why it arrives pre-derived rather than a raw client here). run()
		// only ever assigns it from a real, non-nil *spicedb.Client — its
		// construction failure exits the process earlier in run() — so this
		// cannot produce a typed-nil interface, the same reasoning
		// WithPtTagMinter/WithPtTagResolver above already rely on; no nil
		// guard needed.
		httpsrv.WithPools(d.PoolsReader),
	}
	if d.KGProvider != nil {
		opts = append(opts, httpsrv.WithKG(d.KGProvider))
	}
	return opts
}

// resolveClusterKindFromEnv turns the AP_CLUSTER_KIND value stamped by
// `oap install` into the Strategy this operator serves. Fail-closed: an unset
// or unrecognized value is fatal, matching the MEMORY_BACKEND /
// ARTIFACT_STORE_URL contracts above. A silent fallback would flip
// authorization gates (e.g. AllowsLocalOnlyIdentityProviders) on a cluster
// whose Deployment simply predates this flag — the accepted consequence is
// that upgrading a pre-cluster-kind install requires re-running `oap install`;
// an out-of-band `kubectl apply` of the bundle crash-loops here instead of
// silently serving the wrong profile.
func resolveClusterKindFromEnv(kind string) (cloud.Strategy, error) {
	return cloud.For(kind)
}

func run(cfg *config) {
	// Logger setup. See setupOperatorLogger for the full story on why
	// ctrl.Log is unreliable here and we pass an explicit zap logger
	// to ctrl.Options.Logger below.
	zapLogger := setupOperatorLogger()
	log := zapLogger.WithName("main")
	log.Info("operator logger ready (passed to manager via Options.Logger)")

	// Log all parsed flags + env that gate optional subsystems. These
	// lines are the first zap-formatted output and confirm the logger
	// is wired correctly — when the next deployment crashes silently
	// we'll know whether it died before or after this point.
	log.Info("operator starting",
		"metricsAddr", cfg.metricsAddr,
		"probeAddr", cfg.probeAddr,
		"leaderElect", cfg.leaderElect,
		"debugAddr", cfg.debugAddr,
		"gatewayAddr", cfg.gatewayAddr,
		"runnerImage", cfg.runnerImage,
		"imagePullSecret", cfg.imagePullSecret,
		"natsURL", cfg.natsURL,
		"defaultChannelArchiveAfter", cfg.defaultChannelArchiveAfter.String(),
		"defaultSessionSleepAfter", cfg.defaultSessionSleepAfter.String(),
		"failedSandboxReapGrace", cfg.failedSandboxReapGrace.String(),
		"oauthRefreshThreshold", cfg.oauthRefreshThreshold.String(),
		"channelsdTokenFile", cfg.channelsdTokenFile,
		"authzdTokenFile", cfg.authzdTokenFile,
		"spicedbEndpoint", os.Getenv(spicedb.EnvEndpoint),
		"spicedbInsecure", os.Getenv(spicedb.EnvInsecure),
		"postgresURI", os.Getenv("POSTGRES_URI"),
		"graphitiEndpoint", cfg.graphitiEndpoint,
		"extractordEndpoint", cfg.extractordEndpoint,
		"memoryBackend", cfg.memoryBackend,
		"memoryReadSource", cfg.memoryReadSource,
		"artifactStoreURL", cfg.artifactStoreURL,
		"podNamespace", cfg.podNamespace,
		"watchNamespaces", cfg.watchNamespaces,
		"goTraceback", os.Getenv("GOTRACEBACK"),
		"sessionNetworkPolicies", cfg.sessionNetworkPolicies,
		"sessionNamespaceDefaultDeny", cfg.sessionNamespaceDefaultDeny,
		"clusterKind", cfg.clusterKind,
	)

	// clusterStrategy resolves the AP_CLUSTER_KIND `oap install` stamped onto
	// this Deployment, FAIL-CLOSED: an unset or unrecognized value is fatal,
	// never a silent fallback to cloud.KeyDefault. See
	// resolveClusterKindFromEnv's doc for why.
	clusterStrategy, err := resolveClusterKindFromEnv(cfg.clusterKind)
	if err != nil {
		log.Error(err, "invalid --cluster-kind/AP_CLUSTER_KIND (fail-closed startup)")
		os.Exit(1)
	}

	// Label-filter the Secret/ConfigMap informers so the operator's cache only
	// holds objects it has ADOPTED (a CR references them, so they carry
	// adoptguard.AdoptedLabel). This is the watch-side half of the adoptguard
	// model — only annotated objects are watched/cached, so non-adopted secret
	// contents never enter operator memory. Guarded reads go through the live
	// APIReader (not this cache), and the adopt flow's Get-first is also live, so
	// adopting-then-reading a not-yet-cached object is unaffected.
	adoptedReq, err := labels.NewRequirement(adoptguard.AdoptedLabel, selection.Exists, nil)
	if err != nil {
		log.Error(err, "building adopted-label selector for the Secret/ConfigMap cache")
		os.Exit(1)
	}
	adoptedSel := labels.NewSelector().Add(*adoptedReq)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: cfg.metricsAddr},
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         cfg.leaderElect,
		LeaderElectionID:       "spicebox-operator.agentprimitives.authzed.com",
		// Pass our zap logger explicitly. Without this the manager
		// falls back to log.Log (= the broken ctrl.Log polluted by
		// spicedb's init), and every controller's
		// log.FromContext(ctx) returns a no-op logger. See the
		// "logger setup" comment block above for the full story.
		Logger:        zapLogger,
		WebhookServer: webhook.NewServer(webhook.Options{Port: 9443, CertDir: cfg.webhookCertDir}),
		Cache: cache.Options{
			// nil = cluster-wide (default); a non-empty --watch-namespaces scopes
			// every informer/Owns watch and folds in the operator's own namespace.
			DefaultNamespaces: cacheNamespaces(cfg.watchNamespaces, operatorNamespace(cfg.podNamespace)),
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Secret{}:    {Label: adoptedSel},
				&corev1.ConfigMap{}: {Label: adoptedSel},
			},
		},
	})
	if err != nil {
		log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
	// debugServerListening flips true once the debug HTTP server
	// (--debug-bind-address, :8082) has bound its listener. That server hosts
	// the /memory API the runner reads AND the /admin API the dashboard uses;
	// it starts in a goroutine (below) that races mgr.Start's readyz server, so
	// WITHOUT this gate the pod reports Ready — and the Service adds its
	// endpoint — before :8082 is listening. The runner's first memory read then
	// hits "connection refused" (killing the conversation) and the dashboard
	// 500s. Gating readiness on it means the Service only routes once :8082
	// actually serves, and since `oap init`/`oap desktop` wait on the operator
	// rollout, bring-up cannot enable the chat + dashboard too early.
	var debugServerListening atomic.Bool
	if err := mgr.AddReadyzCheck("debug-http", func(*http.Request) error {
		if cfg.debugAddr != "" && !debugServerListening.Load() {
			return fmt.Errorf("debug/memory/admin HTTP server (%s) not yet listening", cfg.debugAddr)
		}
		return nil
	}); err != nil {
		log.Error(err, "unable to set up debug-http ready check")
		os.Exit(1)
	}

	// Resolve the operator-wide adoptguard mode from --secret-guard-mode.
	// All SecretReader / ConfigMapReader instances are constructed here, once,
	// and injected into reconcilers — so a single flag controls the enforcement
	// level across the entire operator. Defaults to "panic" (all consumers are
	// converted); "warn" remains available for bring-up of new secret consumers.
	guardMode := adoptguard.Warn
	if cfg.secretGuardMode == "panic" {
		guardMode = adoptguard.Panic
	}
	systemNS := operatorNamespace(cfg.podNamespace)
	secretReader := adoptguard.NewSecretReader(
		mgr.GetClient(), mgr.GetAPIReader(),
		guardMode, adoptguard.FixedInfraDefaults(systemNS),
	)
	configMapReader := adoptguard.NewConfigMapReader(
		mgr.GetClient(), mgr.GetAPIReader(),
		guardMode, adoptguard.FixedInfraDefaults(systemNS),
	)
	log.Info("adoptguard readers constructed", "guardMode", cfg.secretGuardMode)

	// Built before the SpiceboxClass reconciler: its builtin toolkits
	// supply the sensitive env-var names that form the reserved set for
	// EnvDefaults validation.
	toolkitRegistry, err := toolspecregistry.NewWithBuiltins(mgr.GetClient())
	if err != nil {
		log.Error(err, "build toolkit registry")
		os.Exit(1)
	}

	// NOTE: the SpiceboxClass controller is registered further below (after
	// sandboxRuntimes is built) — it needs that map to type-assert a kind's
	// Runtime against sandboxkinds.Prewarmer, and construction order matters
	// more than registration order here: every SetupWithManager call just
	// needs to land before mgr.Start, not in any particular sequence relative
	// to the others (see the identical note on SpiceboxSession below).

	// NOTE: the SpiceboxSession controller is registered further below
	// (after opSigned is constructed) — it needs opSigned as its AuditMemory,
	// and construction order matters more than registration order here: every
	// SetupWithManager call just needs to land before mgr.Start, not in any
	// particular sequence relative to the others.

	// NATS connection for operator-published envelopes. Three consumers
	// share it:
	//   1. The monitoring watchers (PublishMonitoring on a fixed subject).
	//   2. The revocation.Publisher (revokePub) below — one shared instance
	//      wired into every revocable-resource controller.
	//
	// Best-effort: a NATS connection error logs + leaves all downstream
	// consumers without a publisher. Every revocation consumer tolerates a
	// nil/no-NATS publisher (Emit then no-ops).
	//
	// apnats.ConnectFromEnv mirrors how channelsd/authzd connect: it reads
	// NATS_CREDS_PATH + NATS_CA_PATH and connects with TLS + user creds. The
	// bundled NATS server REQUIRES TLS, so the previous raw plain nats.Connect
	// here panicked a nats background goroutine — the silent operator exit-2 on
	// GKE Autopilot. With NATS_CA_PATH set, ConnectFromEnv either connects with
	// TLS or returns a clean error (it never plain-connects), so a NATS failure
	// now LOGS here, best-effort, instead of crashing the operator.
	// RetryOnFailedConnect (inside Connect) still suppresses unreachable-server
	// errors, so a non-nil err is a real misconfig (bad URL / unreadable CA or
	// creds), not transient unavailability.
	var operatorNC *nats.Conn
	if conn, err := apnats.ConnectFromEnv(cfg.natsURL, "operator"); err != nil {
		log.Error(err, "operator: NATS connection error; envelope publishers disabled", "natsURL", cfg.natsURL)
	} else {
		operatorNC = conn
		if !operatorNC.IsConnected() {
			log.Info("operator: NATS not reachable at startup; connecting in the background", "natsURL", cfg.natsURL)
		}
		defer operatorNC.Drain() //nolint:errcheck // best-effort drain on shutdown
	}

	// monitoringPublish fans framework-health warnings out to role=monitoring
	// Channels: the AgentSession reconciler's capacity/scheduling stalls, and
	// the sandbox backends' degradations (agent-sandbox reports a cluster whose
	// agent-sandbox install refuses AP's pre-warm claims, so its sessions are
	// silently falling back to cold starts). Declared HERE, above every
	// consumer, so both take the SAME connection — a second NATS connection for
	// the same one-way subject would be pure overhead.
	//
	// A PublishFunc is a func type, so an unconfigured bus leaves a true nil
	// and every consumer's nil-guard skips the emit. Monitoring is how an
	// operator LEARNS of a degradation, never how a component decides to
	// degrade, so nothing behavioural is gated on it.
	var monitoringPublish channelevents.PublishFunc
	if operatorNC != nil {
		monitoringPublish = operatorNC.Publish
	}

	// revokePub is the single shared *revocation.Publisher used by every
	// revocable-resource controller. When operatorNC is nil it is constructed
	// with a nil bus — Emit then no-ops — so all controllers are nil-tolerant
	// without each needing their own nil-NATS branch.
	var revokePub *revocation.Publisher
	if operatorNC != nil {
		revokePub = revocation.NewPublisher(newNATSEnvelopePublisher(operatorNC))
		log.Info("constructed revocation.Publisher", "subject", revocation.Subject)
	} else {
		revokePub = revocation.NewPublisher(nil)
		log.Info("revocation.Publisher constructed without NATS bus; envelopes will not be emitted")
	}

	// signalCtx is created once here and handed to both the revocation
	// subscriber (below, once the broker exists) and mgr.Start. Registering the
	// signal handler this early is harmless — an early SIGTERM simply cancels the
	// context, and mgr.Start then returns immediately.
	signalCtx := ctrl.SetupSignalHandler()

	// Diagnostic breadcrumb (direct stderr, bypasses the lossy ctrl.Log/zap
	// path): zap "registered controller" lines stop right before here on the
	// GKE-Autopilot silent exit-2 crash. If THIS line appears on a redeploy but
	// later "registered controller" lines don't, the crash is downstream with
	// lost zap logs; if it's absent, the crash is in nats.Connect above. Remove
	// once the operator-startup-crash root cause (task #11) is found.

	// Per-controller stateful wrappers that diff credential/allowlist state.
	// ONE instance each — shared across the two settings reconcilers so their
	// independent ClusterAgentSettings and AgentSettings observations land in
	// the same diff map.
	settingsRevoke := settingsctrl.NewRevokePublisher(revokePub)
	agentIdentityRevoke := agentidentity.NewRevokePublisher(revokePub)

	if err := (&spiceboxtoolkit.Reconciler{
		Client:          mgr.GetClient(),
		Registry:        toolkitRegistry,
		RevokePublisher: revokePub,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register SpiceboxToolkit controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "SpiceboxToolkit")

	if err := (&spiceboxtoolspec.Reconciler{
		Client:   mgr.GetClient(),
		Registry: toolkitRegistry,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register SpiceboxToolspec controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "SpiceboxToolspec")

	if err := (&spiceboxtoolchain.Reconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register SpiceboxToolchain controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "SpiceboxToolchain")

	if err := (&mcpserver.Reconciler{
		Client:          mgr.GetClient(),
		SecretReader:    secretReader,
		RevokePublisher: revokePub,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up MCPServer controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "MCPServer")

	if err := (&skillctrl.Reconciler{Client: mgr.GetClient()}).
		SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up Skill controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "Skill")

	if err := (&clusterskillctrl.Reconciler{Client: mgr.GetClient()}).
		SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up ClusterSkill controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "ClusterSkill")

	if err := (&sidecartoolbox.Reconciler{
		Client:          mgr.GetClient(),
		APIReader:       mgr.GetAPIReader(), // uncached probe-Pod reads; the cache lags on loaded clusters
		ConfigMapReader: configMapReader,
		RevokePublisher: revokePub,
		// Admission-probe ingress stamping keys off the SAME opt-in as the
		// per-session policies: both halves of one posture (the probe policy
		// admits the operator to probe pods that a namespace default-deny
		// floor would otherwise cut off).
		ProbeNetpol: sidecartoolbox.ProbeNetpolConfig{
			Enabled:           cfg.sessionNetworkPolicies,
			OperatorNamespace: operatorNamespace(cfg.podNamespace),
		},
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller", "controller", "SidecarToolbox")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "SidecarToolbox")

	if err := (&workspacesource.Reconciler{
		Client:                    mgr.GetClient(),
		BaseStorageClass:          cfg.workspaceBaseStorageClass,
		BaseSize:                  cfg.workspaceBaseSize,
		MaterializeImage:          cfg.materializeImage,
		MaterializeServiceAccount: cfg.snapshotServiceAccount,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller", "controller", "WorkspaceSource")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "WorkspaceSource")

	// Stranded-workspace-volume janitor: reclaims Released hostPath PVs whose
	// node the autoscaler removed (their provisioner-side reclaim can never
	// run — see the package doc). Gated on the workspace class being
	// configured: without one, no workspace volumes exist to judge, and the
	// PV watch would be pure overhead.
	if cfg.workspaceStorageClass != "" {
		if err := (&workspacevolume.Reconciler{
			Client:       mgr.GetClient(),
			StorageClass: cfg.workspaceStorageClass,
		}).SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to create controller", "controller", "WorkspaceVolumeJanitor")
			os.Exit(1)
		}
		log.Info("registered controller", "name", "WorkspaceVolumeJanitor")
	}

	if err := (&agentidentity.RefreshReconciler{
		Client:           mgr.GetClient(),
		APIReader:        mgr.GetAPIReader(),
		SecretReader:     secretReader,
		DefaultThreshold: cfg.oauthRefreshThreshold,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register AgentIdentityRefresh controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "AgentIdentityRefresh",
		"defaultThreshold", cfg.oauthRefreshThreshold.String())

	if err := (&useridentity.RefreshReconciler{
		Client:           mgr.GetClient(),
		APIReader:        mgr.GetAPIReader(),
		SecretReader:     secretReader,
		DefaultThreshold: cfg.oauthRefreshThreshold,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register UserIdentityRefresh controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "UserIdentityRefresh")

	executor, err := execremote.New(mgr.GetConfig())
	if err != nil {
		log.Error(err, "unable to build remote executor")
		os.Exit(1)
	}
	if err := validateArtifactStoreURL(cfg.artifactStoreURL); err != nil {
		log.Error(err, "invalid artifact store config (fail-closed startup)")
		os.Exit(1)
	}
	store, err := blobstore.Open(context.Background(), cfg.artifactStoreURL)
	if err != nil {
		log.Error(err, "artifact store open failed", "url", cfg.artifactStoreURL)
		os.Exit(1)
	}
	log.V(1).Info("startup: artifact store open", "url", cfg.artifactStoreURL)
	if err := probeArtifactStore(context.Background(), log, store, cfg.artifactStoreURL); err != nil {
		log.Error(err, "artifact store readiness probe failed — check the bucket, the operator's workload-identity IAM binding, or the PVC mount", "url", cfg.artifactStoreURL)
		os.Exit(1)
	}

	// SpiceDB is REQUIRED — the AgentClass reconciler validates schema
	// fragments against it, the AgentSession finalizer cleans up
	// relationships in it, and the Guardian controller composes the
	// session-grants schema into it. Silently skipping those when env
	// is missing is exactly the footgun we want to prevent; failing
	// the pod here surfaces the misconfiguration immediately.
	spdbCfg, err := spicedb.LoadEnvConfig()
	if err != nil {
		log.Error(err, "SpiceDB env not configured")
		os.Exit(1)
	}
	spiceDBClient, err := spicedb.NewClient(spdbCfg.Endpoint, spdbCfg.Token, spdbCfg.Insecure)
	if err != nil {
		log.Error(err, "unable to create SpiceDB client", "endpoint", spdbCfg.Endpoint)
		os.Exit(1)
	}
	log.V(1).Info("startup: spicedb client ready")

	// Registered here, AFTER the SpiceDB client exists, rather than beside the
	// other identity controllers above: the AgentIdentity reconciler writes the
	// agentidentity#platform link that makes agentidentity#update_credential
	// satisfiable, and a nil PlatformLinker would silently disable every
	// credential-update card. spiceDBClient is a real, non-nil *spicedb.Client
	// by this point (construction failure exits above), so assigning it into the
	// PlatformLinker interface field cannot produce a typed-nil interface.
	if err := (&agentidentity.Reconciler{
		Client:          mgr.GetClient(),
		APIReader:       mgr.GetAPIReader(),
		SecretReader:    secretReader,
		RevokePublisher: agentIdentityRevoke,
		PlatformLinker:  spiceDBClient,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register AgentIdentity controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "AgentIdentity")

	// Registered here for the same reason the AgentIdentity reconciler above
	// is: it needs the SpiceDB client. It writes the attested-identity edge
	// binding a verified forge account (github_user:<numeric id>) to the
	// catalog's declared owner, and a nil SpiceDB would silently mint nothing.
	// spiceDBClient is a real, non-nil *spicedb.Client by this point
	// (construction failure exits above), so assigning it into the
	// AttestedIdentityWriter interface field cannot produce a typed-nil
	// interface — the failure mode agentclass's SpiceDBSchema actually shipped.
	//
	// monitoringPublish carries the shared-account conflict notice; it is a
	// func type, so an unconfigured bus leaves a true nil and the reconciler's
	// own guard skips the emit without losing the edge.
	if err := (&useridentity.Reconciler{
		Client:            mgr.GetClient(),
		APIReader:         mgr.GetAPIReader(),
		SecretReader:      secretReader,
		RevokePublisher:   useridentity.NewRevokePublisher(revokePub),
		SpiceDB:           spiceDBClient,
		MonitoringPublish: monitoringPublish,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register UserIdentity controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "UserIdentity")

	// RelationshipSource — polls an upstream directory (Slack first) through
	// the registered relsync.Kind and syncs the membership relationships it
	// reports into SpiceDB. Registered here for the same reason AgentIdentity
	// and UserIdentity are: it needs the SpiceDB client, both to bind the
	// guarded writer to each kind's own claimed relsource.Source and to read
	// back what it owns. spiceDBClient is a real, non-nil *spicedb.Client by
	// this point (construction failure exits above), so assigning it into the
	// SpiceDBClient interface field cannot produce a typed-nil interface.
	relationshipSourceReconciler := relationshipsourcectrl.NewReconciler(mgr.GetClient(), secretReader, spiceDBClient)
	relationshipSourceReconciler.MonitoringPublish = monitoringPublish
	if err := relationshipSourceReconciler.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register RelationshipSource controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "RelationshipSource")

	// Note: spiceDBClient.Close() is best-effort at manager shutdown;
	// the gRPC connection will be cleaned up by the OS when the
	// process exits.

	// Derive the per-session NetworkPolicy config only when stamping is
	// on: the values are unused when disabled, and an opted-out cluster
	// with an atypical NATS URL should still be able to start.
	// NamespaceDefaultDeny is set OUTSIDE the sessionNetworkPolicies branch: the
	// two are independent opt-ins, and an operator who disabled per-session
	// stamping (usually for a port-topology reason) may still want the floor.
	var netpolCfg agentsessionctrl.NetpolConfig
	netpolCfg.NamespaceDefaultDeny = cfg.sessionNamespaceDefaultDeny
	if cfg.sessionNetworkPolicies {
		natsPort, err := agentsessionctrl.ParseNATSPort(cfg.natsURL)
		if err != nil {
			log.Error(err, "invalid --nats-url; cannot derive session NetworkPolicy config")
			os.Exit(1)
		}
		spicedbPort, spicedbInCluster, err := agentsessionctrl.ParseSpiceDBEndpoint(spdbCfg.Endpoint)
		if err != nil {
			log.Error(err, "invalid SPICEDB_ENDPOINT; cannot derive session NetworkPolicy config")
			os.Exit(1)
		}
		netpolCfg.Enabled = true
		netpolCfg.OperatorNamespace = operatorNamespace(cfg.podNamespace)
		netpolCfg.NATSPort = natsPort
		netpolCfg.SpiceDBPort = spicedbPort
		netpolCfg.SpiceDBInCluster = spicedbInCluster
	}

	// One unified memory: an in-process Backend wrapped in a Local
	// facade. The Local owns the Kind registry's ScopeHooks fan-out;
	// the blank import of kinds/all above registers every Kind so its
	// hooks run server-side here.
	//
	// The backend is chosen by an EXPLICIT MEMORY_BACKEND selector, resolved
	// fail-closed by validateMemoryBackend: unset is fatal (no silent inmem
	// fallback), 'postgres' requires POSTGRES_URI. The cluster bundle sets
	// 'postgres'; `oap init --local` sets 'sqlite'.
	var memBackend memorypkg.Backend
	var pgClient *mempostgres.Client
	var sqliteClient *memsqlite.Client
	backendKind, err := validateMemoryBackend(cfg.memoryBackend, os.Getenv(mempostgres.EnvURI))
	if err != nil {
		log.Error(err, "invalid MEMORY_BACKEND (fail-closed startup)")
		os.Exit(1)
	}
	switch backendKind {
	case memoryBackendPostgres:
		pgCfg, err := mempostgres.LoadEnvConfig()
		if err != nil {
			log.Error(err, "PostgreSQL env config")
			os.Exit(1)
		}
		log.V(1).Info("startup: connecting to postgres memory store")
		pgClient, err = openPostgresWithRetry(context.Background(), log, postgresConnectCeiling,
			func(ctx context.Context) (*mempostgres.Client, error) {
				attemptCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
				defer cancel()
				c, e := mempostgres.NewClient(attemptCtx, pgCfg.URI)
				if e != nil {
					return nil, e
				}
				if e := c.Migrate(attemptCtx); e != nil {
					c.Close() // NewClient Pinged successfully, so the pool is live; don't leak it on a Migrate failure
					return nil, e
				}
				return c, nil
			})
		if err != nil {
			log.Error(err, "PostgreSQL connect+migrate failed after retry ceiling", "uri", pgCfg.URI)
			os.Exit(1)
		}
		if err := pgsearch.Migrate(context.Background(), pgClient.Pool()); err != nil {
			log.Info("PostgreSQL search migration partial (pgvector may not be installed, text search still works)", "err", err.Error())
		}
		// Reads default to SECONDARY (durable postgres): the shadow still
		// dual-writes inmem+postgres as a safety net, but the read path serves
		// postgres so a pod roll doesn't wipe what admin UI / agents read.
		// MEMORY_READ_SOURCE stays an explicit override — honored when set (e.g.
		// 'primary' to temporarily read ephemeral inmem for debugging).
		readSource := cfg.memoryReadSource
		if readSource == "" {
			readSource = memshadow.ReadFromSecondary
		}
		shadowBackend, err := memshadow.New(
			memoryinmem.NewBackend(),
			mempostgres.NewBackend(pgClient),
			readSource,
			log.WithName("shadow-backend"),
		)
		if err != nil {
			log.Error(err, "shadow backend config", "readSource", readSource)
			os.Exit(1)
		}
		memBackend = shadowBackend
		log.Info("using postgres memory backend (shadow inmem+postgres, reads=secondary)",
			"postgresURI", pgCfg.URI, "readSource", readSource)
	case memoryBackendInmem:
		memBackend = memoryinmem.NewBackend()
		log.Info("using in-memory backend (MEMORY_BACKEND=inmem)")
	case memoryBackendSqlite:
		cfg, err := memsqlite.LoadEnvConfig() // reads MEMORY_SQLITE_PATH
		if err != nil {
			log.Error(err, "SQLite env config")
			os.Exit(1)
		}
		sqliteClient, err = memsqlite.NewClient(cfg.Path)
		if err != nil {
			log.Error(err, "SQLite open", "path", cfg.Path)
			os.Exit(1)
		}
		if err := sqliteClient.Migrate(context.Background()); err != nil {
			log.Error(err, "SQLite schema migration", "path", cfg.Path)
			os.Exit(1)
		}
		memBackend = memsqlite.NewBackend(sqliteClient)
		log.Info("using SQLite memory backend (persistent)", "path", cfg.Path)
	}

	// Skill-bundle store: durable postgres when the postgres backend is
	// selected (pgClient built above), so the bundle survives operator restarts
	// and is readable by the AgentSession controller's staging path; else
	// in-memory. The SAME store is shared by the SkillSource controller (which
	// Puts bundles) and the AgentSession controller (which Gets + stages them).
	// Declared as the interface so the nil-interface footgun (AGENTS.md) can't
	// bite.
	var skillBundleStore skillbundle.Store
	if pgClient != nil {
		pgBundle := skillbundlepg.New(pgClient.Pool())
		if err := pgBundle.Migrate(context.Background()); err != nil {
			log.Error(err, "skillbundle postgres migrate")
			os.Exit(1)
		}
		skillBundleStore = pgBundle
		log.Info("using postgres skillbundle store")
	} else {
		skillBundleStore = skillbundlemem.New()
		log.Info("using in-memory skillbundle store (MEMORY_BACKEND=inmem)")
	}

	if err := (&skillsourcectrl.Reconciler{
		Client:       mgr.GetClient(),
		BundleStore:  skillBundleStore,
		Fetcher:      skillfetch.NewGoGit(),
		SecretReader: secretReader,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up SkillSource controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "SkillSource")

	if err := (&clusterskillsourcectrl.Reconciler{
		Client:       mgr.GetClient(),
		BundleStore:  skillBundleStore,
		Fetcher:      skillfetch.NewGoGit(),
		SecretReader: secretReader,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up ClusterSkillSource controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "ClusterSkillSource")

	var memOpts []memorypkg.LocalOption
	if spiceDBClient != nil {
		memOpts = append(memOpts, memorypkg.WithAuthorizer(
			spicedbauthorizer.New(spiceDBClient),
		))
	}

	// Search providers: inmem always, postgres when POSTGRES_URI is set,
	// sqlite FTS when the sqlite backend is selected.
	searchProviders := []memorypkg.SearchProvider{searchinmem.New(memBackend)}
	if pgClient != nil {
		var embedder memorypkg.EmbeddingProvider
		if ep := cfg.embeddingEndpoint; ep != "" {
			model := cfg.embeddingModel
			if model == "" {
				model = "text-embedding-3-small"
			}
			embedder = searchembedding.New(ep, os.Getenv("EMBEDDING_API_KEY"),
				searchembedding.WithModel(model))
			log.Info("embedding provider configured", "endpoint", ep)
		}
		searchProviders = append(searchProviders, pgsearch.New(pgClient.Pool(), embedder))
	}
	if sqliteClient != nil {
		ftsProvider, err := searchsqlite.New(sqliteClient)
		if err != nil {
			log.Error(err, "SQLite FTS provider init")
			os.Exit(1)
		}
		searchProviders = append(searchProviders, ftsProvider)
		log.Info("SQLite FTS search provider configured")
	}
	// Graphiti supplies BOTH a SearchProvider and the KGProvider off one
	// endpoint, so the "is it configured?" question is answered exactly once,
	// here, rather than by carrying a concrete *GraphitiSearchProvider forward
	// as a second enable-flag.
	//
	// kgProvider is declared as the INTERFACE, never as
	// *kggraphiti.GraphitiKGProvider: a typed-nil pointer assigned into an
	// interface field yields a NON-nil interface whose every method call
	// panics, and the consumers below (memory HTTP handler, admind, KG
	// ingestion) all gate on `kgProvider != nil`.
	var kgProvider memorypkg.KGProvider
	if ep := cfg.graphitiEndpoint; ep != "" {
		searchProv := graphitisearch.New(ep)
		searchProviders = append(searchProviders, searchProv)
		kgProvider = kggraphiti.New(searchProv)
		log.Info("graphiti search + KG providers configured", "endpoint", ep)
	}

	// Build CompositeSearcher with RRF ranking and optional SpiceDB authorization.
	searchOpts := []memsearch.Option{
		memsearch.WithProviders(searchProviders...),
		memsearch.WithLogger(log.WithName("search")),
	}
	if spiceDBClient != nil {
		searchOpts = append(searchOpts, memsearch.WithAuthorizer(
			spicedbauthorizer.New(spiceDBClient),
		))
	}
	searcher := memsearch.New(searchOpts...)

	memOpts = append(memOpts, memorypkg.WithSearchProviders(searchProviders...))
	memOpts = append(memOpts, memorypkg.WithSearcher(searcher))
	memOpts = append(memOpts, memorypkg.WithLogger(log.WithName("memory")))

	// --- Provenance: verify-on-write + the operator's own signing key. ---
	//
	// The verify-on-write enforcer, its composite key lookup, the
	// component-key registry (loaded from the publisher-keys ConfigMap),
	// and the per-session tokens registry are all constructed HERE —
	// before NewLocal — because the verifier is a NewLocal option and the
	// operator's own in-process append-only writes (lifecycle, snapshot,
	// restart copy) flow through that same facade. With the verifier
	// enabled, an unsigned in-process write is rejected, so the operator
	// must sign its own writes too (opSigned below).
	memTokens := tokens.NewRegistry()
	for _, t := range []struct {
		path   string
		name   string // structured "X memory token" label in the log lines
		absent string // log.Info message when the file does not exist
		set    func(string)
	}{
		{
			path:   cfg.channelsdTokenFile,
			name:   "channelsd",
			absent: "channelsd memory token not present; channelsd memory POSTs will fail",
			set:    memTokens.SetChannelsdToken,
		},
		{
			path:   cfg.authzdTokenFile,
			name:   "authzd",
			absent: "authzd memory token not present; authzd memory reads will fall back to per-session tokens",
			set:    memTokens.SetAuthzdToken,
		},
		{
			path:   cfg.webdTokenFile,
			name:   "webd",
			absent: "webd memory token not present; webd artifact view will be unauthorized until provisioned",
			set:    memTokens.SetWebdToken,
		},
	} {
		if b, err := os.ReadFile(t.path); err == nil {
			t.set(strings.TrimSpace(string(b)))
		} else if !os.IsNotExist(err) {
			log.Error(err, "reading "+t.name+" memory token", "path", t.path)
		} else {
			log.Info(t.absent, "path", t.path)
		}
	}

	// Component publisher keys (channelsd/authzd/operator) for provenance
	// verify-on-write. Loaded from the publisher-keys ConfigMap in the
	// operator namespace (additive; absent = empty). The composite lookup
	// resolves per-session keys (tokens registry) first, then component
	// keys (publisherkeys registry).
	// READ uses configMapReader (guarded, allowlisted for publisher-keys, routes
	// through mgr.GetAPIReader() — the uncached direct path — so this is safe
	// pre-cache-start). WRITE (registrar RMW) still uses a dedicated direct
	// client because configMapReader is read-only.
	pubKeyClient, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		log.Error(err, "building direct client for publisher keys registrar")
		os.Exit(1)
	}

	memPubKeys := publisherkeys.New()
	n, err := installComponentPublisherKeys(context.Background(), configMapReader, operatorNamespace(cfg.podNamespace), memPubKeys, log)
	if err != nil {
		// FAIL CLOSED. An unpopulated registry makes verify-on-write reject
		// every component append-only write — a silent, total outage. Crash
		// loudly so Kubernetes restarts us and retries, rather than serving
		// in a broken state. (A genuinely-absent ConfigMap is NOT an error.)
		log.Error(err, "loading publisher-keys ConfigMap; refusing to start with an unverifiable registry",
			"namespace", operatorNamespace(cfg.podNamespace))
		os.Exit(1)
	}
	log.Info("loaded component publisher keys", "count", n)

	// Registrar: persists a component key into the publisherkeys registry
	// AND the publisher-keys ConfigMap (read-modify-write, additive). Also
	// served at the POST /memory/_publisher_key endpoint for channelsd/authzd.
	// Uses the direct client so the operator's own boot-time registration
	// (below, pre-cache-start) and runtime HTTP registrations both succeed.
	pubKeyRegistrar := configMapKeyRegistrar{
		client:    pubKeyClient,
		namespace: operatorNamespace(cfg.podNamespace),
		registry:  memPubKeys,
		log:       log.WithName("publisher-keys"),
	}

	// Mint the operator's own signing key BEFORE the manager starts (and
	// thus before any lifecycle/snapshot/restart signal can fire a write).
	// Register it in the component registry immediately (in-memory is
	// authoritative for the running process) and best-effort persist it to
	// the ConfigMap so verification survives restart. A persist failure is
	// logged, not fatal — in-memory registration is sufficient to verify
	// this process's own writes.
	opPub, opPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Error(err, "minting operator signing key")
		os.Exit(1)
	}
	opKeyID := provenance.KeyID(opPub)
	if rErr := pubKeyRegistrar.RegisterPublisherKey(context.Background(), "system:operator", opKeyID, opPub); rErr != nil {
		// RegisterPublisherKey adds to the in-memory registry first, so the
		// running operator can verify its own writes even when the ConfigMap
		// persist fails. Log and continue rather than fatal.
		log.Error(rErr, "persisting operator publisher key to ConfigMap; continuing with in-memory registration",
			"keyId", opKeyID)
	} else {
		log.Info("registered operator publisher key", "keyId", opKeyID)
	}
	opSigner := provenance.NewSigner(opPriv, "system:operator")

	// Composite key lookup → verify-on-write enforcer. ENFORCEMENT IS NOW
	// ACTIVE: every append-only Put through memLocal (in-process or via the
	// HTTP handler) must carry a Provenance envelope verifying under a
	// registered publisher key.
	keyLookup := compositeKeyLookup{memTokens, memPubKeys}
	verifier := provenance.NewWriteVerifier(keyLookup)
	memOpts = append(memOpts, memorypkg.WithProvenanceVerifier(verifier))

	memLocal := memorypkg.NewLocal(memBackend, memOpts...)

	// opSigned wraps memLocal so the operator's OWN in-process append-only
	// writes (lifecycle events, tool_dispatch_snapshot records, restart
	// memory copies) are signed as "system:operator" before they reach the
	// verifier. memLocal (UNwrapped) is handed to the HTTP handler, where
	// token-authenticated callers sign for themselves, and to read-only /
	// DeleteScope consumers.
	opSigned := provenance.NewSigningMemory(memLocal, opSigner, provenance.WithSeedMemory(auditSeedMemory{Memory: memLocal}))
	log.V(1).Info("startup: memory facade + provenance verifier ready")

	// Sandbox runtimes: one per registered sandbox kind (pkg/tools/sandboxkinds/registry),
	// built once here and shared by SetupWithManager (for the owned-object
	// watches each kind contributes) and the Reconciler (for
	// Ensure/Status/Teardown/Executor). A kind whose NewRuntime fails — e.g. a
	// bring-your-own backend whose peer CRD is not installed in this cluster —
	// is skipped with a logged warning, not fatal: the kind simply has no
	// runtime, and any SpiceboxClass selecting it is refused at validation with
	// a clear message, so one unavailable optional backend cannot take down the
	// whole operator. rt is declared as the interface type and assigned only on
	// success, so a failed construction never puts a typed-nil pointer into the
	// map (see AGENTS.md on nil interfaces).
	sandboxLog := log.WithName("sandboxkinds")
	sandboxRuntimes := sandboxkinds.Runtimes{}
	for _, k := range sandboxregistry.All() {
		var rt sandboxkinds.Runtime
		rt, err := k.NewRuntime(sandboxkinds.Deps{
			Client:  mgr.GetClient(),
			ExecFor: executor.For,
			Logger:  sandboxLog.WithName(k.Name()),
			// Same connection the AgentSession reconciler publishes on. A
			// backend degradation (pre-warming refused by a peer controller AP
			// does not configure) is a cluster-operator problem no session
			// participant can act on, so it goes to the monitoring channel and
			// not the participant-facing notice path.
			MonitoringPublish: monitoringPublish,
		})
		if err != nil {
			sandboxLog.Info("sandbox kind unavailable; skipping runtime construction", "kind", k.Name(), "err", err.Error())
			continue
		}
		sandboxRuntimes[k.Name()] = rt
	}

	if err := (&spiceboxclass.Reconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Registry: toolkitRegistry,
		Runtimes: sandboxRuntimes,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register SpiceboxClass controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "SpiceboxClass")

	// SpiceboxSession is registered here (not up with the other Spicebox
	// controllers above) because it needs opSigned as its AuditMemory: it
	// publishes the signed toolchain-audit "resolved" entry once a session's
	// toolchain resolution is frozen. AuditMemory MUST be the interface, not a
	// concrete pointer — a typed-nil in an interface field is non-nil and
	// panics on first call (see AGENTS.md).
	if err := (&spiceboxsession.Reconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		SandboxImage: cfg.sandboxImage,
		AuditMemory:  opSigned,
		Runtimes:     sandboxRuntimes,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register SpiceboxSession controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "SpiceboxSession")

	// lifecycle is the one Kind whose hooks write entries themselves —
	// Setup hands them the facade to Put into. They write the append-only
	// "lifecycle" Kind, so they get the SIGNING facade.
	lifecycle.Setup(opSigned)

	// Knowledge-graph ingestion hooks. Gated on the KGProvider itself, not on
	// any particular backend: the hooks need somewhere to Ingest to, and that
	// is the whole requirement. Deferred to here rather than folded into the
	// provider construction above because Setup needs memLocal, which does not
	// exist yet at that point.
	if kgProvider != nil {
		strategy := cfg.kgIngestionStrat
		if strategy == "" {
			strategy = string(kgingestion.StrategyEveryTurn)
		}
		kgingestion.Setup(memLocal, kgProvider, kgingestion.IngestionConfig{
			Strategy: kgingestion.IngestionStrategy(strategy),
		})
		log.Info("knowledge graph ingestion configured", "strategy", strategy)
	}

	// Plan-gate denial-streak tripper: freezes a session that keeps attempting
	// governed calls outside its approved ceiling. Threshold ZERO MEANS
	// DISABLED, so denialStreak is declared as the hold.Tripper INTERFACE and
	// assigned only when the flag is positive — never a nil *hold.DenialStreak,
	// which would make hold.Kind's ScopeHooks a non-nil interface that panics
	// on its first OnSignal call (see AGENTS.md on typed-nil interfaces).
	// hold.Setup is called either way so a redeploy that drops the flag back to
	// 0 clears a previously-wired tripper rather than leaving it live.
	var denialStreak hold.Tripper
	if cfg.planGateDenialStreakThreshold > 0 {
		denialStreak = hold.NewDenialStreak(hold.DenialStreakDeps{
			Threshold: cfg.planGateDenialStreakThreshold,
			Mem:       memLocal,
			Client:    mgr.GetClient(),
			Logger:    slog.Default(),
		})
		log.Info("plangate denial-streak tripper enabled", "threshold", cfg.planGateDenialStreakThreshold)
	} else {
		log.Info("plangate denial-streak tripper disabled (--plangate-denial-streak-threshold=0)")
	}

	// Trifecta closure tripper: the CONTAINMENT half, sibling to the runner's
	// dispatch gate rather than a duplicate of it. The gate asks "may this call
	// proceed?" and denies one call; this asks "has this delegation gone
	// wrong?" and freezes the session for review. A closure can reach the
	// second state without the first ever firing — the gate may be in logging
	// mode, or the runner may never have reported anything at all.
	//
	// Every input is OPERATOR-VISIBLE, which is the design rather than a
	// convenience: hold's own doc says the runner is the party under suspicion,
	// so a tripper reading runner-produced records would be defeated by the
	// simplest attack — a compromised runner writes none and the closure reads
	// clean. Bound tags come from SubagentRequest STATUS (controller-written),
	// and leg C from the class's declared tools via the same derivation the
	// admission validator uses.
	//
	// Declared as the INTERFACE and assigned only when enabled, never a nil
	// *TrifectaTripper: a typed-nil here would make Trippers see a non-nil
	// element and hand ScopeHooks something that panics on first signal.
	var trifectaTripper hold.Tripper
	if cfg.trifectaClosureTripper {
		trifectaTripper = hold.NewTrifectaTripper(hold.TrifectaTripperDeps{
			Enabled: true,
			Client:  mgr.GetClient(),
			BoundTagsOf: func(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) ([]string, error) {
				return operatorBoundTagsOf(ctx, mgr.GetClient(), sess)
			},
			TagCarriesUntrusted: spiceDBClient.TagCarriesUntrusted,
			TagReaders:          spiceDBClient.TagReaders,
			ChildAudience: func(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) ([]string, error) {
				return spiceDBClient.SessionReadTranscriptAudience(ctx,
					authz.SessionRef{Namespace: sess.Namespace, Name: sess.Name})
			},
			CanAct: func(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (bool, error) {
				return operatorClassCanAct(ctx, mgr.GetClient(), sess)
			},
			Logger: slog.Default(),
		})
		log.Info("trifecta closure tripper enabled")
	} else {
		log.Info("trifecta closure tripper disabled (--trifecta-closure-tripper=false)")
	}

	// The §2.8 closure denial set. Not a judgement — it freezes nothing — but
	// it rides the same signal because that is the moment the fact becomes
	// true, and stamping it here costs one fan-out per denial instead of a
	// closure walk on every reconcile of every session.
	//
	// Always on: it records a fact rather than acting on one, and the control
	// that DOES act on it (the trifecta gate's every-mode refusal) is already
	// behind that gate's own mode. Gating the record too would mean a denial
	// that happened while the gate was off is invisible once it is turned on.
	closureDenialStamp := newClosureDenialStamper(mgr.GetClient(), memLocal)

	// One signal, three consumers. Trippers keeps a failure in one from
	// disabling the others — if the streak tripper cannot read memory that is a
	// reason to log, not a reason the closure judgement or the denial stamp
	// stops running.
	hold.Setup(hold.Trippers(denialStreak, trifectaTripper, closureDenialStamp))

	// Snapshotter: one operator-wide instance. Namespace and
	// SnapshotStorePVC are left empty in cfg — BuildSnapshotJob/
	// BuildRestoreJob derive them per-call from the source PVC's
	// namespace and the SnapshotHandle's SnapshotStorePVC field
	// (set by the toolcall controller from podspec.SnapshotStoreClaimName).
	snapshotter := workspace.NewCPByPod(mgr.GetClient(), workspace.CPByPodConfig{
		Image:          cfg.snapshotImage,
		ServiceAccount: cfg.snapshotServiceAccount,
	})

	// RecordSnapshotFn writes a tool_dispatch_snapshot memory entry at
	// the parent AgentSession's scope once a snapshot Job succeeds.
	// tool_dispatch_snapshot is append-only, so it closes over opSigned (the
	// operator-signing facade), not raw memLocal; avoids an import cycle in
	// the toolcall package.
	recordSnapshot := func(ctx context.Context, sessName, sessNS string, h workspace.SnapshotHandle, toolUseID, bundleName string) error {
		scope := memorypkg.Scope{Kind: "session", ID: sessNS + "/" + sessName}
		return tool_dispatch_snapshot.Record(ctx, opSigned, scope, tool_dispatch_snapshot.Content{
			ToolUseID:       toolUseID,
			SpiceboxSession: bundleName,
			TurnIndex:       h.TurnIndex,
			Sequence:        h.Sequence,
			SessionUID:      h.SessionUID,
		})
	}

	// Attachment extraction. newAttachmentExtractor returns the INTERFACE type
	// and assigns a real value only inside its own if — never
	// `var c *extractordclient.Client; ...; WithAttachmentExtractor(c)`, which
	// wraps a nil pointer in a non-nil interface and panics on first use. This
	// wiring must not lean on WithAttachmentExtractor's reflection guard.
	//
	// An unset endpoint yields a TRUE nil interface, which serveInboundAsset
	// (pkg/memory/httpsrv) reads as "extraction unconfigured": bytes are still
	// stored, but every attachment resolves through the pipeline's transient
	// "could not be read" path — never the permanent "unsupported type" one,
	// and never a silent skip.
	attachmentExtractor := newAttachmentExtractor(cfg.extractordEndpoint)
	if attachmentExtractor != nil {
		log.Info("attachment extraction configured", "endpoint", cfg.extractordEndpoint)
	} else {
		// Never silent: an operator debugging "why does every attachment say
		// 'could not be read'" needs a line naming this as the deciding cause,
		// not just the absence of the "configured" line above.
		log.Info("attachment extraction NOT configured; every inbound attachment will report a transient read failure",
			"flag", "--extractord-endpoint", "env", "EXTRACTORD_ENDPOINT")
	}

	memHandlerOpts := newMemHandlerOpts(memHandlerDeps{
		K8sClient:           mgr.GetClient(),
		ArtifactStore:       store,
		MemLocal:            memLocal,
		PubKeyRegistrar:     pubKeyRegistrar,
		AttachmentExtractor: attachmentExtractor,
		ExtractordEndpoint:  cfg.extractordEndpoint,
		SpiceDBClient:       spiceDBClient,
		OpSigned:            opSigned,
		PoolsReader:         spiceDBClient.Pools(),
		KGProvider:          kgProvider,
	})
	var eventStore *eventsql.Store
	var goalStore goalmodel.Store
	switch backendKind {
	case memoryBackendSqlite:
		eventStore = eventsql.New(sqliteClient.DB(), false)
		gs := goalsqlite.New(sqliteClient.DB())
		if err := gs.Migrate(context.Background()); err != nil {
			log.Error(err, "goals SQLite migration failed")
			os.Exit(1)
		}
		goalStore = gs
	case memoryBackendPostgres:
		eventStore = eventsql.New(stdlib.OpenDBFromPool(pgClient.Pool()), true)
		gs := goalpostgres.New(pgClient.Pool())
		if err := gs.Migrate(context.Background()); err != nil {
			log.Error(err, "goals PostgreSQL migration failed")
			os.Exit(1)
		}
		goalStore = gs
	case memoryBackendInmem:
		goalStore = goalinmem.New()
	}
	// Goal evidence and audit chains must follow durable storage even when the
	// general memory facade is explicitly set to shadow's ephemeral read source.
	// This private facade is used only by the trusted goal endpoint/publisher;
	// it shares the existing database and signature verifier, without indexing
	// historical snapshots into ordinary memory search.
	goalMemory := memLocal
	if backendKind == memoryBackendPostgres {
		goalMemory = memorypkg.NewLocal(mempostgres.NewBackend(pgClient), memorypkg.WithProvenanceVerifier(verifier), memorypkg.WithLogger(log.WithName("goal-memory")))
	}
	if eventStore != nil {
		if err := eventStore.Migrate(context.Background()); err != nil {
			log.Error(err, "event ledger migration failed")
			os.Exit(1)
		}
	}
	goalService := &goalmodel.Service{Store: goalStore}
	var goalAuth goalweb.Authority
	if spiceDBClient != nil {
		goalAuth = spiceDBClient
	}
	goalHandler := &goalweb.Server{Service: goalService, Reader: mgr.GetAPIReader(), Memory: goalMemory, Tokens: memTokens, Keys: keyLookup, Auth: goalAuth, ColdRegistryUntil: time.Now().Add(memoryColdRegistryGrace)}
	goalService.Auth = goalHandler
	var eventHandler *eventweb.Server
	var eventRouter *sessionevents.Router
	var eventDispatcher *sessionevents.Dispatcher
	if eventStore != nil && goalAuth != nil {
		nativeAccess := &eventweb.NativeSessions{Reader: mgr.GetAPIReader(), Memory: goalMemory, Auth: goalAuth}
		sources := sessionevents.NewRegistry()
		sources.Register(&eventnative.Adapter{Memory: goalMemory, Keys: keyLookup, Authority: nativeAccess, Access: nativeAccess, Publishers: map[string]bool{"system:channelsd": true}})
		execution := &goalmodel.EventExecution{Service: goalService, Sources: sources}
		consumers := sessionevents.NewConsumers()
		consumers.Legacy = "goals"
		consumers.Register("goals", execution)
		triggers := &sessionevents.Triggers{Store: eventStore, Observations: eventStore, Authority: consumers}
		if discoveryStore, ok := goalStore.(goalmodel.DiscoveryStore); ok {
			discovery := &goalweb.Discovery{Server: goalHandler, Store: discoveryStore, Triggers: triggers, Sources: sources}
			goalHandler.Discovery = discovery
			consumers.Register("goal-discovery", discovery)
			if err := mgr.Add(discovery); err != nil {
				log.Error(err, "register goal discovery")
				os.Exit(1)
			}
		}
		execution.Triggers = triggers
		goalService.Events = execution
		goalHandler.EventSources = sources
		eventRouter = &sessionevents.Router{Triggers: triggers, Store: eventStore}
		eventDispatcher = &sessionevents.Dispatcher{Triggers: triggers, Consumer: consumers}
		eventHandler = &eventweb.Server{Ingester: &sessionevents.Ingester{Store: eventStore, Adapters: sources}, Tokens: memTokens}
	}
	consentPublisher := &goalweb.ConsentPublisher{Service: goalService, Memory: goalMemory, Writer: &goalReplyMemory{Memory: goalMemory, Writer: opSigned}, Publish: monitoringPublish}
	goalHandler.Consent = consentPublisher
	goalPublisher := &goalweb.Publisher{Store: goalStore, Memory: goalMemory, Signer: opSigner, Notify: consentPublisher.Notify}
	if err := mgr.Add(goalPublisher); err != nil {
		log.Error(err, "register goal audit publisher")
		os.Exit(1)
	}
	var goalValidator agentsessionctrl.GoalSessionValidator
	if occurrenceStore, ok := goalStore.(goalmodel.OccurrenceStore); ok && goalAuth != nil && monitoringPublish != nil {
		goalService.ExecutionAuth = goalHandler
		dispatcher := &goalctrl.Dispatcher{EventRouter: eventRouter, EventDispatcher: eventDispatcher, Service: goalService, Store: occurrenceStore, Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Worker: uuid.NewString(), DeliveryMemory: &goalReplyMemory{Memory: goalMemory, Writer: opSigned}}
		goalValidator = dispatcher
		goalHandler.ExecutionSessions = dispatcher
		if err := mgr.Add(dispatcher); err != nil {
			log.Error(err, "register goal dispatcher")
			os.Exit(1)
		}
	}
	memHandler := httpsrv.NewHandler(memLocal, memTokens, memHandlerOpts...)
	log.V(1).Info("startup: memory HTTP handler + search providers ready")

	registry := gateway.NewRegistry()
	var gatewayEndpoint string
	if cfg.gatewayAddr != "" {
		lis, err := net.Listen("tcp", cfg.gatewayAddr)
		if err != nil {
			log.Error(err, "gateway listen")
			os.Exit(1)
		}
		grpcSrv := grpc.NewServer()
		gatewayv1.RegisterGatewayServer(grpcSrv, gateway.NewServer(registry))
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error(nil, "gateway goroutine panic",
						"panic", fmt.Sprintf("%v", r),
						"stack", string(runtimedebug.Stack()))
				}
			}()
			log.Info("starting gateway", "address", cfg.gatewayAddr)
			if err := grpcSrv.Serve(lis); err != nil {
				log.Error(err, "gateway serve exited with error")
			} else {
				log.Info("gateway serve exited cleanly")
			}
		}()
		// Cluster-facing endpoint for clients (published in ToolCall.status.streaming).
		gatewayEndpoint = fmt.Sprintf("spicebox-gateway.%s.svc:%s", operatorNamespace(cfg.podNamespace), gatewayPortFromAddr(cfg.gatewayAddr))
	}

	minter := idjag.New(safehttp.Client())
	// gitHubAppMinter is hoisted for the same reason as minter above: it is
	// wired into BOTH the ToolCall broker below and the AgentSession
	// Reconciler's own Deps.GitHubApp (materializeSidecarSecret's minted-kind
	// branch), and must be the SAME value passed to both rather than each
	// site constructing its own. GitHub's API host is fixed (never
	// attacker-influenceable), so this does not need the SSRF-guarded
	// safehttp client federation's discovery leg does.
	gitHubAppMinter := githubapp.Adapt(githubapp.NewHTTPMinter())

	// toolCallBroker is hoisted out of the Reconciler literal because it is also
	// the invalidation target of the revocation subscriber below. Both must hold
	// the SAME instance — a second broker would have its own cache, and revoking
	// a credential would drop an entry nobody reads.
	toolCallBroker := inproc.NewWithMinter(mgr.GetClient(), minter)
	toolCallBroker.GitHubApp = gitHubAppMinter
	// The manager client above reads Secrets through the ADOPTION-FILTERED cache
	// configured earlier, so a Secret carrying no adoption label — a hand-created
	// one, or one recreated to rotate a token — reads as NotFound. The uncached
	// APIReader lets the broker say which of the two it actually hit; it is used
	// for a metadata-only probe, never to read a non-adopted Secret's value.
	toolCallBroker.LiveReader = mgr.GetAPIReader()

	// The operator publishes revocation envelopes; it must also consume them, or
	// its own broker cache serves revoked credentials into every new ToolCall for
	// the life of the process. Fail-loud: a broker that silently misses
	// invalidations is a security defect, not a degraded mode.
	if err := subscribeRevocation(signalCtx, operatorNC, toolCallBroker); err != nil {
		log.Error(err, "unable to subscribe to revocation bus")
		os.Exit(1)
	}
	if operatorNC != nil {
		log.Info("subscribed to revocation bus", "subject", revocation.Subject, "scope", revocation.AllNamespaces)
	} else {
		log.Info("revocation subscriber disabled: no NATS bus; broker cache will not be invalidated mid-process")
	}

	if err := (&toolcall.Reconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Broker:           toolCallBroker,
		Runtimes:         sandboxRuntimes,
		Store:            store,
		Registry:         registry,
		GatewayEndpoint:  gatewayEndpoint,
		ToolkitRegistry:  toolkitRegistry,
		APIReader:        mgr.GetAPIReader(),
		Snapshotter:      snapshotter,
		RecordSnapshotFn: recordSnapshot,
		Now:              time.Now,
		TokenChecker:     spiceDBClient, // *spicedb.Client implements toolcall.TokenChecker (CheckUseToken); SpiceDB is a hard startup requirement (see above), never nil here
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register ToolCall controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "ToolCall", "gatewayEndpoint", gatewayEndpoint)

	if err := (&artifactrender.Reconciler{
		Client:            mgr.GetClient(),
		Store:             store,
		MaxOutputAbsolute: 10 << 20,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register ArtifactRender controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "ArtifactRender")

	// toolCallBroker (constructed above) is reused here rather than a second
	// broker instance: a second broker would carry its own cache, and
	// revoking a credential would drop an entry nobody reads (same reasoning
	// as the ToolCall reconciler's own comment on toolCallBroker above).
	//
	// This reconciler DETERMINES ONLY: it resolves identity, refreshes, probes,
	// runs Determine, and writes its own CR status. Minting a link, building a
	// card and publishing belong to channelsd's CredentialUpdateWatcher
	// (pkg/channels/channelsd/pipeline/credential_update.go), which holds the
	// passthrough signing key by a NON-OPTIONAL volume mount — so a missing
	// Secret blocks channelsd's pod start rather than silently degrading it —
	// and runs an externalurl.Provider.
	if err := (&credentialupdaterequest.Reconciler{
		Client:  mgr.GetClient(),
		Broker:  toolCallBroker,
		IdleTTL: credentialupdaterequest.DefaultCredentialUpdateIdleTTL,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register CredentialUpdateRequest controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "CredentialUpdateRequest")

	// PlatformLinker writes the agentclass#platform link that makes
	// agentclass#start_session satisfiable — without it the browser's agent
	// picker is silently empty on every cluster.
	//
	// spiceDBClient is a real, non-nil *spicedb.Client here (construction
	// failure exits above), so assigning it into these interface fields cannot
	// produce a typed-nil. This struct literal shipped exactly that bug once on
	// SpiceDBSchema: the `!= nil` guard passed on a non-nil interface wrapping a
	// nil pointer, every reconcile panicked, and controller-runtime's silent
	// panic recovery hid it. Never assign a possibly-nil concrete pointer here.
	if err := (&agentclassctrl.Reconciler{
		Client:             mgr.GetClient(),
		APIReader:          mgr.GetAPIReader(),
		SpiceDBSchema:      spiceDBClient,
		PlatformLinker:     spiceDBClient,
		StarterLinker:      spiceDBClient,
		AllowTestProvider:  false, // production: refuse provider=test
		SecretReader:       secretReader,
		ConfigMapReader:    configMapReader,
		MaxDelegationDepth: 3,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register AgentClass controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "AgentClass")

	if err := (&agentui.Reconciler{}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register AgentUI controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "AgentUI")

	// webdBaseURL is the same live-updating accessor channelsd's pipeline
	// watchers already use for webd's own externally-reachable URL — see
	// webdURLProvider in internal/cmd/channelsd/main.go. A single
	// kubernetes.NewForConfig here is cheap and stateless (see adminClientset
	// below for the same pattern).
	//
	// Two consumers share it, and neither treats it as a hard dependency:
	//
	//   - the channel controller's WebhookURLDrift check, a diagnostic rather
	//     than an input to Channel validity; and
	//   - the runner pod factory, which stamps the value onto each runner so
	//     the pod — which cannot read a ConfigMap in another namespace — can
	//     compose the durable artifact link a trigger's status surface carries.
	//
	// A construction failure is logged and leaves both degraded (Provider.Get
	// would have returned "" anyway until the first poll), never fatal.
	var webdBaseURL func() string
	if cs, csErr := kubernetes.NewForConfig(mgr.GetConfig()); csErr != nil {
		log.Error(csErr, "operator: building the clientset for webd's external-URL provider failed; the WebhookURLDrift check stays skipped and runners spawn with no webd address")
	} else {
		webdURLProvider := externalurl.NewProviderFor(cs, log,
			spiceboxv1alpha1.WebdExternalURLConfigMap, spiceboxv1alpha1.WebdTrustedURLKey, "")
		go webdURLProvider.Run(signalCtx)
		webdBaseURL = webdURLProvider.Get
	}

	if err := (&channelctrl.Reconciler{
		Client:          mgr.GetClient(),
		SecretReader:    secretReader,
		ExternalBaseURL: webdBaseURL,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create channel controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "Channel")

	// OperatorURL is empty when --debug-bind-address is disabled; the
	// AgentSession reconciler treats an empty URL as "memory API not
	// available" (runners spawned in this configuration would fail their
	// memory writes — that's expected when the operator is run without
	// the debug port).
	operatorURL := ""
	if cfg.debugAddr != "" {
		operatorURL = fmt.Sprintf("http://spicebox-operator.%s.svc%s", operatorNamespace(cfg.podNamespace), cfg.debugAddr)
	}

	// trustedImageRegistry: derived from cfg.sandboxImage rather than passed
	// as its own flag, because cfg.sandboxImage already IS the answer —
	// `--sandbox-image` defaults to apimage.Sandbox.LocalRef() (the bare
	// "spicebox-sandbox:dev" config/manager/deployment.yaml ships), and `oap
	// install --image-registry <reg>` rewrites that exact literal in the
	// rendered manifest (pkg/platform/manifests.Substitute does a byte-level
	// replace of every "<name>:<version>" occurrence, args included) to
	// apimage.Sandbox.RegistryRef(reg) or ...DigestRef(reg, digest) — the
	// same composition a workshop sidecar's OR a probed user image's
	// first-party image would need to match. Reversing that composition
	// names the registry this operator was actually installed with, with no
	// new flag and no guess. Computed once here and reused by the AgentSession
	// reconciler (below, as Reconciler.TrustedImageRegistry), the WorkshopProbe
	// controller's PodProber, and the Workshop admission webhook (further
	// down) so none of the three ever drift onto different answers for the
	// identical question.
	trustedImageRegistry := trustedImageRegistryFrom(apimage.Sandbox, cfg.sandboxImage)

	podFactory := &agentsessionctrl.PodRunnerFactory{
		Client:          mgr.GetClient(),
		RunnerImage:     cfg.runnerImage,
		ImagePullSecret: cfg.imagePullSecret,
		OperatorURL:     operatorURL,
		NATSURL:         cfg.natsURL,
		SpiceDBEndpoint: spdbCfg.Endpoint,
		SpiceDBInsecure: spdbCfg.Insecure,
		SecretReader:    secretReader,
		// Read per pod, so a session started after the external address landed
		// gets it without restarting the operator. nil when the provider could
		// not be built above; the factory answers that in one place.
		WebdBaseURL: webdBaseURL,
	}

	// Load the install-time NATS trust material so the AgentSession
	// reconciler can mint per-session runner user JWTs. secretReader routes
	// through the adoptguard tripwire (allowlisted for spicebox-nats-*) and
	// uses mgr.GetAPIReader() — the uncached direct path — so this is safe
	// pre-cache-start. A missing identity Secret (local dev without `oap
	// install`) leaves natsIdentity nil; channel-attached runners then spawn
	// without NATS creds (logged below) rather than crashing the operator.
	var natsIdentity *apnats.Identity
	var natsCAPEM []byte
	{
		var loadErr error
		natsIdentity, natsCAPEM, loadErr = loadNATSIdentity(context.Background(), secretReader, systemNS)
		if loadErr != nil {
			log.Error(loadErr, "unable to load NATS identity")
			os.Exit(1)
		}
		if natsIdentity == nil {
			log.Info("NATS identity Secrets not found; channel-attached runners will be spawned without NATS creds",
				"identitySecret", "spicebox-nats-identity", "tlsSecret", "spicebox-nats-tls")
		} else {
			log.Info("loaded NATS identity for per-session creds minting")
		}
	}

	// forkNoticePublish delivers the SessionFork deny notice to channelsd's
	// out.metaagent_notice subscriber (ephemeral Slack message to the forker, in
	// the PARENT thread). ns/name must be separate NATS tokens to match
	// channelsd's "ap.session.*.*.out.metaagent_notice" subscription. nil when
	// NATS is unreachable at startup — the fork Host then logs + drops the notice
	// (the deny still stands; delivery is best-effort).
	var forkNoticePublish func(ctx context.Context, ns, name, requester, body string) error
	if operatorNC != nil {
		nc := operatorNC
		forkNoticePublish = func(_ context.Context, ns, name, requester, body string) error {
			// requester here is a CANONICAL SpiceDB subject ("user:<base64url>"):
			// the fork host is bound to PendingRestart.TriggeredBy, and the
			// operator has no channel-native id for the forker (TriggeredBy is
			// canonical-only; NewOwnerExternalID is takeover-mode, which skips
			// this gate). So it MUST go in requesterCanonical — channelsd resolves
			// it to a Slack user_id at delivery time. Putting it in `requester`,
			// which is passed to PostEphemeral verbatim, is what made these
			// notices permanently undeliverable.
			payload, err := json.Marshal(map[string]string{
				"requesterCanonical": requester,
				"body":               body,
			})
			if err != nil {
				return err
			}
			subject := channelevents.SubjectOut(
				channelevents.SubjectPrefix(ns, name), channelevents.KindMetaagentNotice)
			return nc.Publish(subject, payload)
		}
	} else {
		log.Info("operator: NATS unavailable; SessionFork deny notices will be logged + dropped")
	}

	if err := (&agentsessionctrl.Reconciler{
		GoalValidator: goalValidator,
		Client:        mgr.GetClient(),
		APIReader:     mgr.GetAPIReader(),
		// On local/desktop clusters, images are loaded by mutable tag and are not
		// pullable by digest, so the per-session SidecarToolbox by-digest launch
		// rewrite must be skipped (mirrors `oap install --no-digest-pin`).
		UsesLocalDevImages: clusterStrategy.InstallProfile().UsesLocalDevImages(),
		SecretReader:       secretReader,
		ConfigMapReader:    configMapReader,
		Tokens:             memTokens,
		Memory:             memLocal,
		RunnerFactory:      podFactory,
		// OperatorURL mirrors podFactory.OperatorURL above (same value, same
		// "" when the debug port is disabled) so the ONE workshop-identity
		// sidecar pod can carry OPERATOR_MEMORY_URL too (BuildSidecarPod's
		// identity branch, threaded through reconcileSidecarPod).
		OperatorURL: operatorURL,
		// TrustedImageRegistry mirrors the WorkshopProbe/webhook value (same
		// variable, computed once above) so the ONE workshop-identity sidecar
		// pod can carry WORKSHOP_TRUSTED_IMAGE_REGISTRY too (BuildSidecarPod's
		// identity branch, threaded through reconcileSidecarPod).
		TrustedImageRegistry:           trustedImageRegistry,
		DefaultChannelArchiveAfter:     cfg.defaultChannelArchiveAfter,
		DefaultSessionSleepAfter:       cfg.defaultSessionSleepAfter,
		FailedSandboxReapGrace:         cfg.failedSandboxReapGrace,
		DefaultSessionStorageRetention: cfg.sessionStorageRetention,
		NodePinnedStorageReclaimGrace:  cfg.nodePinnedStorageReclaimGrace,
		IdleStorageReclaimAfter:        cfg.idleStorageReclaimAfter,
		SpiceDBDeleter:                 spiceDBClient,
		WorkspaceStorageClass:          cfg.workspaceStorageClass,
		WorkspaceSize:                  cfg.workspaceSize,
		SnapshotStoreSize:              cfg.snapshotStoreSize,
		SnapshotImage:                  cfg.snapshotImage,
		SnapshotServiceAccount:         cfg.snapshotServiceAccount,
		WorkspaceReconcileImage:        cfg.materializeImage,
		NATSIdentity:                   natsIdentity,
		NATSCAPEM:                      natsCAPEM,
		SpiceDBToken:                   spdbCfg.Token,
		Snapshotter:                    snapshotter,
		// RestartMemory copies append-only kinds (turn, …) from the parent
		// scope into a forked child scope and appends a fresh inbox turn —
		// all append-only writes that must be signed. opSigned re-signs each
		// copied entry as "system:operator" for the new scope (the original
		// per-scope signature would not verify under the child scope's digest).
		RestartMemory: opSigned,
		// LifecycleMemory is the same operator-signing facade: the sequencer
		// appends typed transition events (the append-only "lifecycle" kind,
		// signed as system:operator) and folds them to compute phase.
		LifecycleMemory: opSigned,
		AuthzGranter:    spiceDBClient,
		OrgViewerSyncer: spiceDBClient, // *spicedb.Client implements authz.ArtifactOrgViewerSyncer (SyncArtifactOrgViewer): levels the class's artifactVisibility opt-in per session
		TokenGranter:    spiceDBClient, // *spicedb.Client implements agentsession.TokenGranter (authorized_token grant methods)
		TokenChecker:    spiceDBClient, // *spicedb.Client implements agentsession.TokenChecker (CheckUseToken); SpiceDB is a hard startup requirement (see above), never nil here
		ForkChecker:     spiceDBClient, // *spicedb.Client implements authz.ForkChecker (CheckFork)
		DeniedLister:    spiceDBClient, // *spicedb.Client implements authz.DeniedLister (ListDeniedUsers)
		// *spicedb.Client implements authz.SlotGrantCopier (ListSlotGrants +
		// ListSlotPins + CopySlotTuples). Carries a parent's bound instances AND
		// pin onto a restart/inherit child, verbatim (no gate); the takeover
		// carve-out lives in ReconcileRestart.
		SlotGrantCopier:   spiceDBClient,
		ForkNoticePublish: forkNoticePublish,
		// StartChecker enforces spec.authz.session.allowedStarters before any
		// pod is created. *spicedb.Client implements agentsession.StartChecker
		// (CheckAgentClassStart); SpiceDB is a hard startup requirement (see
		// above), never nil here.
		StartChecker: spiceDBClient,
		// StartRefusedNoticePublish tells the refused requester, on the same
		// out.metaagent_notice delivery ForkNoticePublish uses. nil when NATS
		// was unreachable at startup — the refusal still stands, logged + dropped.
		StartRefusedNoticePublish: forkNoticePublish,
		// PublisherKeys authenticates status.pendingRestart before the restart
		// reconciler acts on it. Same registry that backs provenance
		// verify-on-write, so channelsd's already-registered signing key is
		// resolvable here with no extra wiring — and an operator restart
		// rehydrates it from the publisher-keys ConfigMap, which is additive
		// across key IDs, so markers signed by a channelsd predecessor still
		// verify.
		PublisherKeys:     memPubKeys,
		BundleStore:       skillBundleStore,
		Netpol:            netpolCfg,
		Minter:            minter,
		GitHubApp:         gitHubAppMinter,
		RevokePublisher:   revokePub,
		ImagePullSecret:   cfg.imagePullSecret,
		MonitoringPublish: monitoringPublish,
		// AuditKeyMemory witnesses each session's audit key binding in the
		// session's own scope, signed system:operator — the durable half of the
		// trust root, since the status field and the Secret both die with the CR
		// while the records the key signed do not.
		AuditKeyMemory: opSigned,
		// AuditChainHeads anchors tail-truncation detection for ended sessions:
		// on terminal-phase transition, scan the session scope for each
		// publisher's final signed entry. Reads go through raw memLocal —
		// reads need no signing.
		AuditChainHeads: func(ctx context.Context, scope memorypkg.Scope) (map[string]provenance.ChainHead, error) {
			return provenance.ComputeChainHeads(ctx, memLocal, scope)
		},
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register AgentSession controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "AgentSession", "operatorURL", operatorURL)

	// Workshop provisions/tears down a builder session's isolated namespace,
	// exact RBAC, SpiceDB tuple and bearer token (spec §1.2/§1.3). spiceDBClient
	// is a real, non-nil *spicedb.Client here (construction failure exits
	// above) and implements workshop.WorkshopTuples, so this is a hard startup
	// requirement, never nil — matching the same reasoning as every other
	// spiceDBClient assignment above. ExpiredNoticePublish reuses
	// forkNoticePublish: same out.metaagent_notice delivery every other
	// operator-originated session notice uses, nil (logged + dropped) when
	// NATS was unreachable at startup.
	if err := (&workshopctrl.Reconciler{
		Client:               mgr.GetClient(),
		APIReader:            mgr.GetAPIReader(),
		Tuples:               spiceDBClient,
		Tokens:               memTokens,
		ExpiredNoticePublish: forkNoticePublish,
		// The browser (webd) is the only caller that ever needs to start a
		// session INSIDE a workshop namespace (a person's own test of what
		// they just built). Named/namespaced from the same two constants
		// webhookrbac.go already exports for binding webd's identity into a
		// per-Channel Role — not re-derived from the operator's own
		// namespace, which is a coincidence of today's install layout, not a
		// guarantee that webd runs alongside the operator.
		BrowserServiceAccount:          channelctrl.WebdServiceAccountName,
		BrowserServiceAccountNamespace: channelctrl.WebdServiceAccountNamespace,
		// The two halves of telling a builder session about the person's own
		// test (spec.testWatch), identical to what the SubagentRequest
		// controller below is handed for waking an attended child's parent:
		// opSigned rather than memLocal, because the watch appends to the turn
		// Kind and an unsigned append-only write is rejected at Local.Put's own
		// door; and the IN-subject publisher, nil when NATS was unreachable at
		// startup, which inboxwake logs rather than failing the reconcile over.
		ParentMemory:       opSigned,
		PublishInteraction: inboxwake.Publish(subagentInteractionPublish(operatorNC)),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register Workshop controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "Workshop")

	// WorkshopProbe turns a WorkshopProbe CR into a run probe: gate on the
	// workshop:<id>#build SpiceDB tuple, gate on
	// Workshop.spec.limits.maxConcurrentProbes, then run the hardened pod and
	// record what it found (spec §2.5). spiceDBClient is a real, non-nil
	// *spicedb.Client here (construction failure exits above) and implements
	// workshopprobectrl.WorkshopBuildChecker (CheckWorkshopBuild), so this is
	// a hard startup requirement, never nil — matching workshopctrl.
	// Reconciler's Tuples field immediately above, and assigned into the
	// WorkshopBuildChecker INTERFACE field (never a *spicedb.Client field) so
	// this package's own tests can inject a fake with no live SpiceDB — see
	// CLAUDE.md's typed-nil rule and workshopprobectrl.WorkshopBuildChecker's
	// doc comment.
	if err := (&workshopprobectrl.Reconciler{
		Client: mgr.GetClient(),
		Prober: &workshopprobectrl.PodProber{
			Client:               mgr.GetClient(),
			OperatorNamespace:    systemNS,
			TrustedImageRegistry: trustedImageRegistry,
		},
		Build: spiceDBClient,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register WorkshopProbe controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "WorkshopProbe")

	// SessionHold controller publishes the session_release approval card once
	// a hold has been observed Active (stamped by the AgentSession
	// reconciler's own reconcileHold, above) and applies the human's decision
	// on it. Memory is opSigned — the same operator-signing facade
	// RestartMemory/LifecycleMemory use — so its EventApprovalsCleared write
	// is signed and verified identically to every other operator-authored
	// append-only entry. NATSPublish reuses monitoringPublish: the operator's
	// one shared NATS connection, already reused this same way for the
	// sandbox runtimes' degradation events above.
	if err := (&sessionhold.Reconciler{
		Client:      mgr.GetClient(),
		Memory:      opSigned,
		NATSPublish: monitoringPublish,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register SessionHold controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "SessionHold")

	// SubagentRequest controller is what actually authorizes and performs
	// delegation: the runner's delegate tool creates the CR and polls it, but
	// deliberately cannot create an AgentSession itself — the party whose
	// behaviour delegation constrains must not also be the party that
	// authorizes it, the same reasoning that puts forensic-hold trippers here
	// rather than in the runner. MaxDelegationDepth matches the AgentClass
	// controller's own ceiling (above) so a roster re-validated here is judged
	// by the same bound it was admitted under. DefaultMaxDelegatedAgents is the
	// built-in pooled per-root total-agent ceiling used whenever the settings
	// fold resolves no explicit BudgetConfig.MaxDelegatedAgents — 8 is enough
	// for a lead plus a handful of specialists at depth, small enough that a
	// runaway fan-out is structurally impossible. spiceDBClient is a real,
	// non-nil *spicedb.Client here (construction failure exits above), so
	// assigning it into the LineageWriter and WorkshopBuildChecker interface
	// fields cannot produce a typed-nil interface. WorkshopBuild is the ONE
	// relaxation of step 0's same-namespace admission rule (spec §2.6):
	// without it wired, every cross-namespace parent -- workshop or not --
	// denies exactly as before this field existed, fail-closed.
	// MaxAwaitingParent and TerminalRetention are both left at their zero
	// values on purpose: the reconciler resolves those to
	// subagentrequest.DefaultMaxAwaitingParent and DefaultTerminalRetention,
	// and there is no operator-level knob to override either with today. Zero
	// never means "no bound" or "reclaim immediately" here — see
	// maxAwaitingParent's and terminalRetention's docs for why each of those
	// distinctions is load-bearing.
	if err := (&subagentrequest.Reconciler{
		Client: mgr.GetClient(),
		// Uncached: the attended-parent notice decides on the parent's phase,
		// and the cache lags it on loaded clusters.
		APIReader:                 mgr.GetAPIReader(),
		Scheme:                    mgr.GetScheme(),
		Authz:                     spiceDBClient,
		WorkshopBuild:             spiceDBClient,
		MaxDelegationDepth:        3,
		DefaultMaxDelegatedAgents: 8,
		// Grading closes the half attenuation does not cover. The controller
		// already refuses a tag the PARENT cannot read; this refuses one it can
		// read but whose binding would disclose to the CHILD's audience, or
		// which carries untrusted content.
		//
		// Composed here rather than inside the controller so that package keeps
		// no opinion about how a reader set is resolved — it asks for a verdict
		// and gets one. The three lookups come from one client, so the audience
		// a decision is made against and the tuples enforcing it cannot be
		// resolved from different datastores.
		Grader: func(ctx context.Context, child authz.SessionRef, tagID string) (handoff.Grade, string, error) {
			return handoff.GradeRequest(ctx, handoff.Deps{
				ChildAudience:       spiceDBClient.SessionReadTranscriptAudience,
				TagReaders:          spiceDBClient.TagReaders,
				TagCarriesUntrusted: spiceDBClient.TagCarriesUntrusted,
			}, child, tagID)
		},
		// The child inherits its portion of the parent's plan as its own gate
		// root. opSigned: plan_gate_audit is append-only, so the WRITE must be
		// signed — the same facade the fork path (RestartMemory) writes through.
		// The read→fold→derive→write (and the idempotency + the write-before-CR
		// contract) lives in plangate.WriteChildRoot so this and the e2e harness
		// cannot drift. Scopes are fixed by the request's own parent/child refs.
		DeriveChildPlanRoot: func(ctx context.Context, parent, child authz.SessionRef) error {
			return plangate.WriteChildRoot(ctx, opSigned,
				parent.Namespace+"/"+parent.Name, child.Namespace+"/"+child.Name)
		},
		// The card that ASKS. Without it the routing still runs and still
		// fails at the wait window with a stated reason — a controller that
		// bound what it could not get a decision on would be the one outcome
		// worse than not asking — but nobody is prompted.
		//
		// Published on the parent session's IN subject, the same route the
		// runner's own interaction requests take, so channelsd parks the
		// session and writes the durable record before rendering. A nil bus
		// leaves this nil and the log above says so. Reused a second time, by
		// notifyAttendedParent, for the forced KindUserMessage wake an
		// attended child's watching parent gets on the child's terminal
		// transition — same publish, different envelope kind.
		PublishInteraction: subagentInteractionPublish(operatorNC),
		// The operator's signing facade, not raw memLocal: notifyAttendedParent
		// appends to the turn Kind, which is append-only, so an unsigned write
		// would be rejected at Local.Put's own door. Same reach
		// agentsession.Reconciler.LifecycleMemory uses, for the same reason.
		ParentMemory: opSigned,
		// pt_tag has no owner relation, so the only route to a human who can
		// speak for a datum is back through the objects it was minted from.
		//
		// memLocal, and system-approved: this is the operator reading a record
		// it wrote itself, at a scope fixed by the request's own parent rather
		// than named by any agent.
		// #owner, not #viewer: reading a thing is not authority to authorize
		// sharing it. Same distinction the info-leakage approver model draws.
		ResourceOwners: func(ctx context.Context, resourceType, resourceID string) ([]string, error) {
			return spiceDBClient.LookupSubjects(ctx, resourceType+":"+resourceID+"#owner")
		},
		TagSources: func(ctx context.Context, parent authz.SessionRef, tagID string) ([]string, error) {
			return pttag.SourcesOf(
				memorypkg.WithSystemApproval(ctx, "data_slot_disclosure"),
				memLocal,
				memorypkg.Scope{Kind: "session", ID: parent.Namespace + "/" + parent.Name},
				tagID)
		},
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register SubagentRequest controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "SubagentRequest")

	// Guardian controller composes the AgentSession SpiceDB schema from
	// the union of AgentSessionGrants CRs. spiceDBClient is guaranteed
	// non-nil because LoadEnvConfig was required above.
	//
	// spicedb.BootstrapSource names this reconciler's writes — the same var
	// test/e2e/harness.go references for its in-process mirror of this
	// wiring. Claims nothing DELIBERATELY — the bootstrap surface's
	// relations are arbitrary and operator-supplied, so it can own none of
	// them: see pkg/authz/spicedb/relsource and pkg/authz/spicedb/writer.go.
	var schemaIO guardianschema.SchemaIO = spicedb.SchemaIOFor(spiceDBClient)
	guardianReconciler := guardianctrl.NewReconciler(mgr.GetClient(), schemaIO, spiceDBClient.Writer(spicedb.BootstrapSource))
	// Reader wires SpiceDBBootstrap drift detection's read-back. spiceDBClient
	// is the same guaranteed-non-nil *spicedb.Client as above — assigned
	// directly into the Reader interface field (never through a possibly-nil
	// intermediate variable), so this is a genuine non-nil interface, not
	// the typed-nil trap AGENTS.md documents against this exact file.
	guardianReconciler.Reader = spiceDBClient
	// MonitoringPublish reports SpiceDBBootstrap drift onto the same
	// monitoring subject every other framework-health event uses. A nil
	// monitoringPublish (NATS unconfigured) leaves this nil too — drift
	// detection still runs and still logs, only the monitoring fan-out is
	// skipped (bootstrap_drift.go's publishDrift).
	guardianReconciler.MonitoringPublish = monitoringPublish
	if err := guardianReconciler.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register Guardian AgentSessionGrants controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "GuardianAgentSessionGrants")

	if err := (&settingsctrl.ClusterReconciler{
		RevokePublisher: settingsRevoke,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register ClusterAgentSettings controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "ClusterAgentSettings")
	if err := (&settingsctrl.NamespaceReconciler{
		RevokePublisher: settingsRevoke,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register AgentSettings controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "AgentSettings")

	// AllowsLocalOnlyIdentityProviders gates idp.Kind.AllowedNonLocal()==false
	// kinds (e.g. password, a single-user local admin gate with no
	// anti-brute-force posture of its own): they must only ever validate on a
	// local cluster.
	//
	// The answer comes from the cluster kind `oap install` stamped, and MUST NOT
	// be re-derived from anything that merely correlates with local-ness (the
	// memory backend, say) — an install running Postgres locally or sqlite
	// remotely would then silently flip an authorization gate.
	localOnlyIdP := clusterStrategy.InstallProfile().AllowsLocalOnlyIdentityProviders()
	if err := (&clusteridpctrl.Reconciler{
		SecretReader: secretReader,
		LocalCluster: localOnlyIdP,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register ClusterIdentityProvider controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "ClusterIdentityProvider",
		"clusterKind", clusterStrategy.Key(), "localOnlyIdP", localOnlyIdP)

	// PublicEndpoint: opens the cluster's public tunnel and publishes where the
	// cluster is reachable. The tunnel runs IN THIS PROCESS (every registered
	// provider is a Go client, not an agent image), so the reconciler holds the
	// live tunnel and the auth token never leaves the operator pod. Leader
	// election keeps that to one replica: two operators each opening a session
	// would take two provider sessions for one endpoint.
	// ClusterKind carries the resolved AP_CLUSTER_KIND: kinds whose
	// PublicEndpointPolicy is Never open no tunnel and never touch webd's
	// external URL, which on a durable cluster already names a real https://
	// host that `oap install` configured.
	if err := (&publicendpointctrl.Reconciler{
		SecretReader: secretReader,
		ClusterKind:  clusterStrategy,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to register PublicEndpoint controller")
		os.Exit(1)
	}
	log.Info("registered controller", "name", "PublicEndpoint",
		"clusterKind", clusterStrategy.Key(),
		"publicEndpointPolicy", clusterStrategy.InstallProfile().PublicEndpointPolicy(),
		"tunnelProviders", localtunnelregistry.Names())

	// Register the validating admission webhook handlers. The decoder is
	// constructed once here and passed to each handler constructor — in
	// controller-runtime v0.23.3 the DecoderInjector auto-injection was
	// removed, so callers are responsible for building and passing the
	// decoder explicitly. admission.NewDecoder returns a Decoder value
	// (no error).
	dec := admission.NewDecoder(mgr.GetScheme())
	// The operator's own authenticated identity, as the API server presents it
	// in AdmissionRequest.UserInfo. The skill webhooks need it to tell a
	// MATERIALIZATION from a forgery: every other signal available to them
	// (the owner-ref, the SkillSource's repoURL) is written by the same tenant
	// who writes the Skill, so only "who made this request" distinguishes the
	// two, and userInfo comes from the API server rather than from the object.
	//
	// Derived, not configured: the SA name is fixed by config/manager and the
	// namespace already resolves through the same helper every other
	// namespace-scoped decision here uses.
	operatorSAUsername := "system:serviceaccount:" + operatorNamespace(cfg.podNamespace) + ":" + operatorServiceAccountName
	ws := mgr.GetWebhookServer()
	ws.Register(websettings.PathAgentClass, &admission.Webhook{Handler: websettings.NewAgentClassWebhook(mgr.GetClient(), dec)})
	ws.Register(websettings.PathAgentSession, &admission.Webhook{Handler: websettings.NewAgentSessionWebhook(mgr.GetClient(), dec)})
	ws.Register(websettings.PathClusterAgentSettings, &admission.Webhook{Handler: websettings.NewClusterAgentSettingsWebhook(mgr.GetClient(), dec)})
	ws.Register(websettings.PathAgentSettings, &admission.Webhook{Handler: websettings.NewAgentSettingsWebhook(mgr.GetClient(), dec)})
	ws.Register(websettings.PathClusterIdentityProvider, &admission.Webhook{Handler: websettings.NewClusterIdentityProviderWebhook(mgr.GetClient(), dec)})
	ws.Register(webskill.PathSkill, &admission.Webhook{Handler: newSkillWebhook(mgr.GetClient(), dec, operatorSAUsername)})
	ws.Register(webtoolcall.Path, &admission.Webhook{Handler: webtoolcall.New(dec)})
	ws.Register(webskill.PathClusterSkill, &admission.Webhook{Handler: newClusterSkillWebhook(mgr.GetClient(), dec, operatorSAUsername)})
	ws.Register(websession.Path, &admission.Webhook{Handler: websession.New(dec)})
	ws.Register(webgoalexecution.Path, &admission.Webhook{Handler: &webgoalexecution.Handler{Decoder: dec, OperatorSubject: "system:serviceaccount:" + systemNS + ":spicebox-operator"}})
	ws.Register(websubagentreq.Path, &admission.Webhook{Handler: websubagentreq.New(dec)})
	ws.Register(webworkspacejob.Path, &admission.Webhook{Handler: webworkspacejob.New(mgr.GetAPIReader(), dec)})
	// spiceDBClient is a real, non-nil *spicedb.Client here (construction
	// above is a hard startup requirement) and implements
	// webworkshop.WorkshopBuildChecker (CheckWorkshopBuild). trustedImageRegistry
	// was computed once, alongside operatorURL, and reused by the AgentSession
	// reconciler, the WorkshopProbe controller, and here — see that comment
	// for the derivation.
	ws.Register(webworkshop.PathWorkshopObject, &admission.Webhook{Handler: webworkshop.New(mgr.GetClient(), spiceDBClient, trustedImageRegistry, dec)})
	log.Info("registered validating webhook handlers",
		"agentclass", websettings.PathAgentClass,
		"agentsession", websettings.PathAgentSession,
		"clusteragentsettings", websettings.PathClusterAgentSettings,
		"agentsettings", websettings.PathAgentSettings,
		"clusteridentityprovider", websettings.PathClusterIdentityProvider,
		"skill", webskill.PathSkill,
		"clusterskill", webskill.PathClusterSkill,
		"agentsessionIdentity", websession.Path,
		"goalExecution", webgoalexecution.Path,
		"subagentRequestParent", websubagentreq.Path,
		"workspaceJob", webworkspacejob.Path,
		"workshopObject", webworkshop.PathWorkshopObject,
	)

	// Framework monitoring: reuse the shared operator NATS connection
	// (constructed above for the RevokePublisher) so MonitoringEvents
	// fan out to channelsd's role=monitoring relay. Best-effort — the
	// operator must keep running regardless of NATS availability.
	if operatorNC == nil {
		log.Info("monitoring: NATS not configured; framework monitoring disabled")
	} else {
		if err := monitoringctrl.Register(mgr, operatorNC.Publish); err != nil {
			log.Error(err, "unable to register monitoring watchers")
			os.Exit(1)
		}
		log.Info("registered controller", "name", "MonitoringWatchers")
	}

	// adminClientset is admind's typed reader for the oap-install capacity
	// preflight (pkg/platform/cloud.Strategy.SchedulingCeiling needs a
	// kubernetes.Interface, not the controller-runtime client.Client K8s
	// already is). Declared as the INTERFACE — never a *kubernetes.Clientset —
	// so a construction failure below leaves adminClientset a genuine nil
	// interface rather than a typed-nil pointer boxed into one; admind.New's
	// Clientset == nil check (and every fail-safe branch behind it) depends on
	// seeing a true nil there, not a non-nil interface wrapping a nil pointer.
	var adminClientset kubernetes.Interface
	if cs, csErr := kubernetes.NewForConfig(mgr.GetConfig()); csErr != nil {
		log.Error(csErr, "admind: building typed clientset failed; oap-install capacity checks will be skipped")
	} else {
		adminClientset = cs
	}

	// admind: the operator-mounted admin API (live sessions + audit +
	// kill), gated by the spicebox-admind-token Secret so no other pod
	// can call it. Absent token file → admind stays unmounted (the
	// admin UI is then disabled in webd, which fails closed too).
	var adm *admind.Admind
	admindTokenFile := os.Getenv("ADMIND_TOKEN_PATH")
	if admindTokenFile == "" {
		admindTokenFile = "/var/run/operator/admind-token/token"
	}
	if b, err := os.ReadFile(admindTokenFile); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		adm, err = admind.New(admind.Config{
			Mem:         memLocal,
			K8s:         mgr.GetClient(),
			APIReader:   mgr.GetAPIReader(),
			Checker:     spiceDBClient,
			Token:       strings.TrimSpace(string(b)),
			Logger:      log.WithName("admind"),
			GraphitiURL: cfg.graphitiEndpoint,
			KG:          kgProvider, // nil when --graphiti-endpoint unset → Knowledge panel degrades
			Clientset:   adminClientset,
			// The workshops-install route loads a workshop's drafted .oap by the
			// ref the controller recorded on Workshop.status.export.
			ArtifactStore: store,
			// spiceDBClient is a real, non-nil *spicedb.Client by this point
			// (construction failure exits above) — assigning it directly into
			// the Identities interface field cannot produce a typed-nil
			// interface, the same reasoning as Checker above.
			Identities: spiceDBClient,
			// Same reasoning again: spiceDBClient is a real, non-nil
			// *spicedb.Client here, so assigning it directly into the Scopes
			// interface field cannot produce a typed-nil interface either.
			Scopes: spiceDBClient,
		})
		if err != nil {
			log.Error(err, "admind: construction failed")
			os.Exit(1)
		}
	} else if err != nil && !os.IsNotExist(err) {
		log.Error(err, "admind: reading token file", "path", admindTokenFile)
	} else if err == nil {
		log.Info("admind token file empty; admin API disabled", "path", admindTokenFile)
	} else {
		log.Info("admind token not present; admin API disabled", "path", admindTokenFile)
	}

	if adm != nil {
		// AgentSession informer feeds the live aggregator. Registered as
		// a Runnable so it runs once the manager's cache has started.
		theAdm := adm
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			inf, err := mgr.GetCache().GetInformer(ctx, &spiceboxv1alpha1.AgentSession{})
			if err != nil {
				return fmt.Errorf("admind: agentsession informer: %w", err)
			}
			_, err = inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					if s, ok := obj.(*spiceboxv1alpha1.AgentSession); ok {
						theAdm.Aggregator().UpsertSession(s)
					}
				},
				UpdateFunc: func(_, obj interface{}) {
					if s, ok := obj.(*spiceboxv1alpha1.AgentSession); ok {
						theAdm.Aggregator().UpsertSession(s)
					}
				},
				DeleteFunc: func(obj interface{}) {
					if d, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
						obj = d.Obj
					}
					if s, ok := obj.(*spiceboxv1alpha1.AgentSession); ok {
						theAdm.Aggregator().DeleteSession(s.Namespace, s.Name)
					}
				},
			})
			if err != nil {
				return fmt.Errorf("admind: add event handler: %w", err)
			}
			<-ctx.Done()
			return nil
		})); err != nil {
			log.Error(err, "admind: registering informer runnable")
			os.Exit(1)
		}

		// NATS overlay (optional): without it the live view degrades to
		// CRD-watch granularity — functional, just no sub-turn animation.
		if natsURL := os.Getenv("NATS_URL"); natsURL != "" {
			nc, err := apnats.Connect(apnats.Options{
				URL:        natsURL,
				CredsPath:  os.Getenv("NATS_CREDS_PATH"),
				CAPath:     os.Getenv("NATS_CA_PATH"),
				ServerName: apnats.ServerName,
				Name:       "operator-admind",
			})
			if err != nil {
				log.Error(err, "admind: NATS connect failed; live view degrades to CRD watch", "url", natsURL)
			} else if _, err := nc.Subscribe(subjects.AnyOutTree, func(m *nats.Msg) {
				// m.Subject, not just m.Data: this subscription is cluster-wide
				// while each publisher's NATS JWT permits exactly one session's
				// subtree, so the subject is the only session identity NATS
				// authorized. The aggregator keys off it and cross-checks the
				// envelope body against it.
				theAdm.Aggregator().HandleEnvelopeBytes(m.Subject, m.Data)
			}); err != nil {
				log.Error(err, "admind: NATS subscribe failed; live view degrades to CRD watch")
			}
		} else {
			log.Info("admind: NATS_URL not set; live view uses CRD watch only")
		}
	}

	if cfg.debugAddr != "" {
		ns := operatorNamespace(cfg.podNamespace)
		// Use an uncached client; the manager cache isn't ready until Start.
		directClient, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
		if err != nil {
			log.Error(err, "debug: build direct client")
			os.Exit(1)
		}
		token, err := debug.EnsureToken(context.Background(), directClient, ns)
		if err != nil {
			log.Error(err, "debug: ensure token", "namespace", ns)
			os.Exit(1)
		}
		debugHandler := debug.NewHandlerWithMemAuth(store, token, memHandler, memTokens)
		debugHandler.Handle(httpsrv.AuditPath, httpsrv.NewAuditHandler(memLocal, token))
		// Mount the operator-mediated secret-output endpoint next to /memory.
		// The runner POSTs a captured secret value here; the operator (which
		// holds Secret-write RBAC; the runner does not) writes it into the
		// per-session secret-output Secret. Auth reuses the per-session token
		// registry — session-scoped, so a token may only write its own session.
		debugHandler.Handle("/goals/", goalHandler)
		if eventHandler != nil {
			debugHandler.Handle("/session-events/", eventHandler)
		}
		debugHandler.Handle("/secret-output/", secretoutsrv.NewHandler(mgr.GetClient(), memTokens))
		// The tuple-authorized workshop draft-export route (agent-builder plan
		// 3b, Task 6 — Ruling A). A workshop sidecar's operator bearer is
		// registered under the SYNTHETIC {builderSessionNamespace,
		// WorkshopName(builderSessionName)} key (plan 2), not the builder
		// session itself, so this route re-resolves the caller's own Workshop
		// CR and re-checks workshop:<W>#build@agentsession:<B>/<X> before ever
		// storing a byte — see pkg/web/workshopdraftsrv's package doc.
		// spiceDBClient is a real, non-nil *spicedb.Client here (construction
		// failure exits above, same reasoning as every other spiceDBClient
		// assignment in this file) and implements
		// workshopdraftsrv.WorkshopBuildChecker (CheckWorkshopBuild). memLocal
		// and store are the same operator memory facade and artifact store
		// httpsrv.WithArtifact wires above — the route runs IN the operator, so
		// it writes through them directly rather than needing its own session
		// bearer with mutate scope.
		debugHandler.Handle(workshopdraftsrv.Path, workshopdraftsrv.NewHandler(mgr.GetClient(), memLocal, store, memTokens, spiceDBClient))
		// The read_transcript-authorized workshop transcript route (agent-builder
		// plan 4a, Task 8 — same Ruling A as workshopdraftsrv above). A workshop
		// bearer may read a NAMED CHILD session's transcript only when the child
		// lives in the bearer's own workshop namespace AND
		// agentsession:<child>#read_transcript@agentsession:<B>/<X> holds — the
		// `+ parent` schema arm plan 4a Task 3 added, populated by the
		// SubagentRequest controller's TouchLineage the moment it creates a child
		// naming spec.parent = B/X. spiceDBClient implements
		// workshoptranscriptsrv.ReadTranscriptChecker
		// (CheckReadTranscriptForSession); memLocal is the same operator memory
		// facade this route reads the child's turns back through.
		debugHandler.Handle(workshoptranscriptsrv.Path, workshoptranscriptsrv.NewHandler(mgr.GetClient(), memLocal, memTokens, spiceDBClient))
		// The agents-in-thread lookup (agent-builder plan 9a, Task 1). A
		// workshop bearer may learn which other AgentSessions share its
		// builder session's own conversation thread — and their delegation
		// closures — once workshop:<W>#build@agentsession:<B>/<X> re-checks
		// clean; see pkg/web/workshopthreadsrv's package doc for why this
		// route needs no memory.Memory and no read_transcript-shaped
		// per-child check (its payload is identity metadata, never message
		// content). spiceDBClient implements workshopthreadsrv.WorkshopBuildChecker
		// (CheckWorkshopBuild), the same method workshopdraftsrv above reuses.
		// log is this binary's own logr.Logger (declared at the top of main),
		// so a per-participant delegation-closure walk failure (a dangling
		// Spec.Parent) lands in the operator's own log stream rather than
		// nowhere.
		debugHandler.Handle(workshopthreadsrv.Path, workshopthreadsrv.NewHandler(mgr.GetClient(), memTokens, spiceDBClient, log))
		// The credential-free stand-in projection route (agent-builder plan
		// 9b, Task 1). A workshop bearer may project an ordinary,
		// same-namespace AgentClass into ITS OWN workshop namespace for
		// another agent it may want to rehearse handing work to — but ONLY
		// one that appears in the bearer's own agents-in-thread result
		// (workshopthreadsrv.ResolveAgentsInThread, reused here rather than
		// re-derived — see pkg/web/workshopprojectsrv's own package doc for
		// why). The stand-in carries the source's system prompt and skills
		// only; no identity, no tools, no capabilities, no authz policy — see
		// that package's whitelist ruling. spiceDBClient implements
		// workshopprojectsrv.WorkshopBuildChecker (CheckWorkshopBuild), the
		// same method every other workshop route above reuses.
		debugHandler.Handle(workshopprojectsrv.Path, workshopprojectsrv.NewHandler(mgr.GetClient(), memTokens, spiceDBClient, log))
		if adm != nil {
			debugHandler.Handle("/admin/", adm.Handler())
		}
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error(nil, "debug server goroutine panic",
						"panic", fmt.Sprintf("%v", r),
						"stack", string(runtimedebug.Stack()))
				}
			}()
			log.Info("starting debug server", "address", cfg.debugAddr, "namespace", ns, "tokenSecret", debug.TokenSecretName)
			srv := &http.Server{Addr: cfg.debugAddr, Handler: debugHandler}
			safehttp.HardenServer(srv)
			// Bind the listener explicitly (rather than srv.ListenAndServe) so
			// we can flip debugServerListening the instant :8082 accepts
			// connections. The debug-http readyz gate blocks the operator's
			// Ready condition until then, so the Service never routes the
			// runner's /memory read (or the dashboard's /admin call) to an
			// unbound port. net.Listen makes the socket accept immediately;
			// srv.Serve then handles — connections between the two queue in the
			// backlog rather than being refused.
			lis, err := net.Listen("tcp", cfg.debugAddr)
			if err != nil {
				log.Error(err, "debug server listen failed", "address", cfg.debugAddr)
				return
			}
			debugServerListening.Store(true)
			log.Info("debug server listening", "address", cfg.debugAddr)
			err = srv.Serve(lis)
			if err != nil && err != http.ErrServerClosed {
				log.Error(err, "debug server Serve failed",
					"address", cfg.debugAddr)
			} else {
				log.Info("debug server stopped", "address", cfg.debugAddr, "err", err)
			}
		}()
	} else {
		log.Info("debug server disabled (--debug-bind-address empty); memory + artifact HTTP endpoints will not be served")
	}

	log.Info("starting manager")
	mgrStart := time.Now()
	// signalCtx, not a second ctrl.SetupSignalHandler() — that function panics if
	// called twice. It is created once above so the revocation subscriber and the
	// manager share one shutdown signal.
	err = mgr.Start(signalCtx)
	// Log mgr.Start's exit regardless of whether err is nil. The slice-2
	// silent-crash incident hinged on Start returning nil after a context
	// cancel that nothing else logged, so the operator exited with code 0
	// from main but the container saw exit-code-1 from a panic upstream.
	// This makes the manager-exit decision explicit.
	log.Info("manager.Start returned",
		"duration", time.Since(mgrStart).String(),
		"err", errStr(err))
	if err != nil {
		log.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// errStr renders an error as a non-empty string, or "<nil>" when nil.
// Used in structured logs so the value of an error parameter is always
// visible — including the "exited cleanly" case which would otherwise
// log err="" and be indistinguishable from an unset field.
func errStr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// natsEnvelopePublisher is the internal/cmd/operator-internal implementation of
// revocation.EventPublisher backed by a *nats.Conn. JSON-encodes the
// envelope and publishes it on the unified revocation.Subject
// (ap.revocation). Single publisher (the operator) → fan-out to every
// runner's revocation subscriber. Modeled after MonitoringEventSubject:
// cluster-scoped fanout, fixed subject (no per-AgentSession suffix).
//
// Errors are returned (not swallowed) so the publisher's Observe path
// can log the failure with full context per the AGENTS.md
// "never silently drop errors" rule.
type natsEnvelopePublisher struct {
	nc *nats.Conn
}

func newNATSEnvelopePublisher(nc *nats.Conn) *natsEnvelopePublisher {
	return &natsEnvelopePublisher{nc: nc}
}

func (p *natsEnvelopePublisher) Publish(_ context.Context, env channelevents.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	if err := p.nc.Publish(revocation.Subject, data); err != nil {
		return fmt.Errorf("nats publish %s: %w", revocation.Subject, err)
	}
	return nil
}
