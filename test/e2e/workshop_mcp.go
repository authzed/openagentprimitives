//go:build e2e

package e2e

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/tools/workshopmcp"
)

// mountWorkshopMCP builds the REAL pkg/tools/workshopmcp.Server — not a
// canned MCPStub — and serves it exactly as
// pkg/tools/workshopmcp/server_test.go's newTestMux does: an
// mcp.NewStreamableHTTPHandler on "/mcp" plus a "/healthz" 200 responder,
// wrapped in an httptest.Server this func's own t.Cleanup closes.
//
// k8sClient is the harness's REAL envtest client (env.Client), never a fake:
// workshop_inventory's cluster reads run against the live envtest apiserver,
// which is the entire point of mounting the real server instead of extending
// MCPStub with canned workshop_* replies. Ops is left nil — nothing this
// harness exercises calls a workshop tool that reaches the operator's memory
// routes (export/handoff), so the field is never touched, matching the
// production wiring's own "Ops: nil, inventory never touches it" contract.
// ProbeHTTP is http.DefaultClient, the same loopback-permitting override the
// MCPServer controller registration uses above — so a workshop probe_mcp
// call can reach the harness's own httptest fakes.
//
// namespace is used for ALL FOUR WorkshopIdentity fields (Namespace,
// SessionNamespace, SessionName, WorkshopID) at construction. That is a
// deliberate simplification, not a production shape: a live workshop
// sidecar's SessionNamespace/SessionName name the BUILDER AgentSession,
// distinct from its own workshop Namespace (see WorkshopIdentity's own doc).
// Collapsing them to one value at construction is necessary, not merely
// convenient: this func runs during Harness.Start, before any AgentSession
// exists, so there is no real session name yet to hand it. Namespace and
// WorkshopID stay frozen at that placeholder for the life of the mount — every
// tool that reads them (workshop_apply, the CRUD/inventory/render/probe
// tools) only ever needed "the harness's one namespace," which this satisfies
// exactly.
//
// SessionNamespace/SessionName do NOT stay frozen: eight tools key a
// Workshop-CR or parent-session lookup on them (test_tool, watch_test,
// test_sessions, request_credential, request_install, recommend_capability,
// close_others, and export_draft's registry read) — test_link is not among
// them: it Gets the candidate AgentClass by Identity.Namespace alone, which
// stays frozen to the harness's construction-time namespace and needs no
// bind. A lookup keyed on the literal string "<namespace>" can never find a real
// AgentSession or its Workshop CR (whose name is
// "<real-session-name>-workshop", not "<namespace>-workshop"). The returned
// *workshopmcp.Server exposes SetBuilderSession precisely so
// test/e2e/threadrun's driver can rebind those two fields once the pipeline
// has actually created the builder session (see driver.go's
// bindWorkshopSessionOnce and Harness.BindWorkshopSession) — safe to call
// while this mount is already serving other tool calls; see
// workshopmcp.Server's sessionMu doc for the concurrency guard.
//
// This mount proves the SERVER runs its tools for real against a real API
// server, now including the six identity-dependent tools once their session
// is bound. It deliberately does NOT exercise, and must never be read as
// evidence for, the production Workshop-Ready→sidecar-identity path, which
// does not exist yet. RunModeFor (pkg/controllers/agentsession/sidecars.go)
// returns RunModeSeparatePod — the only run mode that ever gets a minted
// WorkshopIdentity via BuildSidecarPod's identity branch — for either of two
// independent reasons: a SidecarToolbox declaring spec.secretInputs, or one
// declaring spec.isolation: isolated. The shipped workshop toolbox
// (pkg/tools/workshopmcp/deploy/sidecartoolbox.yaml) declares NEITHER, so it
// still runs in-pod. Opting it in is a real decision with a real cost (a pod
// per builder session), deliberately left unmade — see plan 0a.
// So workshopIdentityFor never runs in production for this toolbox either,
// and there is no in-process pod here for the harness to sidestep in the
// first place — this func just hands the real Server a synthetic identity
// directly, and the driver's later SetBuilderSession call is the harness
// substituting for that same missing wiring, not a stand-in for it having
// run. A green bundle exercising this mount is evidence the workshop TOOLS
// work against real cluster state (their Workshop CR included, when a
// scenario's fixture sanctions it via ensureWorkshop — see
// bindWorkshopSessionOnce's own doc); it is not evidence the sidecar-pod
// identity plumbing that would hand a live process this same identity is
// wired.
func mountWorkshopMCP(t *testing.T, k8sClient client.Client, namespace string) (*httptest.Server, *workshopmcp.Server) {
	t.Helper()

	s := workshopmcp.NewServer(k8sClient, nil, workshopmcp.WorkshopIdentity{
		Namespace:        namespace,
		SessionNamespace: namespace,
		SessionName:      namespace,
		WorkshopID:       namespace,
	}, "") // "" -> NewServer substitutes the production default field owner.
	s.ProbeHTTP = http.DefaultClient

	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "ap-workshop", Version: "0"}, nil)
	s.Register(mcpSrv)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv }, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}
