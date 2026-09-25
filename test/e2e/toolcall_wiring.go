//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/nats-io/nats.go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/controllers/toolcall"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	"github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
	_ "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod" // register the built-in sandbox backend so the ToolCall reconciler's Runtimes resolves it
	toolspecregistry "github.com/authzed/openagentprimitives/pkg/tools/toolspec/registry"
	"github.com/authzed/openagentprimitives/pkg/web/gateway"
	gatewayv1 "github.com/authzed/openagentprimitives/pkg/web/gateway/v1"
)

// storeArtifactClient adapts an artifactstore.Store to the reader the sandbox
// tool composes its result through (pkg/agent/tool/sandbox.ArtifactClient,
// satisfied structurally — that package is not imported here).
//
// It is the in-process stand-in for production's HTTPArtifactClient, which
// fetches the same bytes over the operator's artifact endpoint. Read-only by
// construction: Get is the whole of what a result composition needs, and
// exposing Put would let a tool write into the store the controller owns.
type storeArtifactClient struct {
	store artifactstore.Store
}

func (c storeArtifactClient) Get(ctx context.Context, ref string) (io.ReadCloser, error) {
	return c.store.Get(ctx, artifactstore.Ref(ref))
}

// installToolCallController registers the real ToolCall reconciler with the
// harness's manager, starts gateway.Server on a bufconn listener, wires a
// fake.Binder, and subscribes to NATS out.tool_session_delta so
// ExpectToolSessionDelta (Task 5) can block on captured envelopes.
//
// Replaces the stub in harness.go that t.Fatals.
func (h *Harness) installToolCallController(t *testing.T, mgrCtx context.Context) {
	t.Helper()

	h.fakeExec = fake.New()
	h.gatewayReg = gateway.NewRegistry()
	// Kept on the harness, not local, because the sandbox tool has to READ BACK
	// what this controller writes: it stores stdout/stderr as artifacts and puts
	// only the refs on ToolCall.status, so the model-facing result is composed by
	// fetching them again through SessionContext.ArtifactClient. Production wires
	// that to the operator's artifact endpoint over the one cluster-wide store
	// (internal/cmd/runner/main.go's NewHTTPArtifactClient); in-process, "the same
	// store" is this variable. Before it was shared, every sandbox tool in the
	// harness composed its result from an empty stdout — invisible, because no
	// scenario asserted on sandbox output content, only on FakeExec().Calls().
	store := blobstore.NewMem()
	h.toolCallStore = store // read back by storeArtifactClient; see its doc

	// In-process gateway over bufconn. The runner's bridge dials the
	// "passthrough:///bufnet" target with our dialer option.
	lis := bufconn.Listen(1 << 20)
	gsrv := grpc.NewServer()
	gatewayv1.RegisterGatewayServer(gsrv, gateway.NewServer(h.gatewayReg))
	go func() { _ = gsrv.Serve(lis) }()
	t.Cleanup(func() {
		gsrv.Stop()
		_ = lis.Close()
	})

	h.gatewayDialer = grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	})

	// Toolspec registry: built-ins only is fine for the scenarios.
	toolkitReg, err := toolspecregistry.NewWithBuiltins(h.K8s)
	if err != nil {
		t.Fatalf("toolspec registry: %v", err)
	}

	// Reuse the existing manager from startManager (h.mgr set in harness.go).
	if h.mgr == nil {
		t.Fatal("installToolCallController: harness manager not set; check Start ordering")
	}
	// Runtimes resolves through the SAME binder h.fakeExec, mirroring how the
	// operator builds one Runtimes map and shares it — a scenario's
	// SpiceboxSession.Status.Sandbox handle (kind "pod") must resolve to an
	// executor whose responses this test can actually program.
	runtimes := testenv.SandboxRuntimesWithExec(t, h.mgr.GetClient(), h.fakeExec.For)
	if err := (&toolcall.Reconciler{
		Client:          h.mgr.GetClient(),
		Scheme:          h.mgr.GetScheme(),
		Runtimes:        runtimes,
		Store:           store,
		Registry:        h.gatewayReg,
		GatewayEndpoint: "passthrough:///bufnet",
		ToolkitRegistry: toolkitReg,
		APIReader:       h.mgr.GetAPIReader(),
		// Broker resolves credential descriptors stamped on ToolCall.spec.credentials
		// by the runner (via credresolve.Descriptors). Must be set so the controller
		// can inject GIT_TOKEN and similar sensitive env vars into the exec.Request.
		//
		// This is a SECOND broker, distinct from the runner's own: a sandbox call
		// is a ToolCall CR this controller resolves credentials for, so a minter
		// wired only into the runner's broker leaves every sandbox dispatch
		// failing with "no GitHub App minter configured" while the MCP path works.
		// The delegating minter below is what lets both be pointed at one place
		// AFTER Start, which is when a bundle's fixture provider exists.
		Broker: h.newToolCallBroker(),
		// Snapshotter completes the pre-dispatch snapshot Job for stateful
		// (stateImpact ∈ {readwrite, external}) tool dispatches. Without it the
		// controller errors+requeues forever in the snapshot path and never
		// reaches reconcileStreaming, hanging interactive scenarios (no kubelet
		// runs the Job under envtest). RecordSnapshotFn is left nil — the audit
		// write is best-effort and skipped when unset.
		Snapshotter: completingSnapshotter{c: h.mgr.GetClient()},
	}).SetupWithManager(h.mgr); err != nil {
		t.Fatalf("register ToolCall reconciler: %v", err)
	}

	// NATS subscription: capture every out.tool_session_delta across all
	// sessions; ExpectToolSessionDelta reads from the resulting slice with
	// a per-call cursor (mirrors ExpectAgentReply's Drv.Sent() pattern), so
	// non-matching envelopes stay queued for the next Expect call rather
	// than being silently consumed. Reuse the harness's own NATS
	// connection (drained on harness cleanup); no need for a second conn.
	sub, err := h.nc.Subscribe(channelevents.SubjectOut(channelevents.AnySessionPrefix(), channelevents.KindToolSessionDelta), func(m *nats.Msg) {
		var env channelevents.Envelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			slog.Info("e2e: tool_session_delta unmarshal", "err", err.Error())
			return
		}
		h.toolSessionDeltasMu.Lock()
		h.toolSessionDeltas = append(h.toolSessionDeltas, toolSessionDelta{Subject: m.Subject, Env: env})
		h.toolSessionDeltasMu.Unlock()
	})
	if err != nil {
		t.Fatalf("nats subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Drain() })
}
