// Command workshop runs the ap-workshop sidecar: a Streamable-HTTP MCP server
// that lives inside the plan-2 workshop namespace W and exposes the agent
// builder's workshop tools (added starting with Task 2) to the builder
// agent's runner.
//
// It holds exactly two controller-issued tokens, both minted by the
// AgentSession/Workshop reconcilers (pkg/controllers/agentsession/sidecarpod.go's
// identity branch, pkg/controllers/workshop/controller.go) and never by this
// binary itself:
//
//   - a projected ServiceAccount token at
//     /var/run/secrets/workshop/serviceaccount/token, RBAC-bounded to W, used
//     to build the controller-runtime client every workshop CR read/write
//     goes through; and
//   - an operator bearer, delivered as the env var "token" (the
//     <session>-workshop-token Secret's key, envFrom'd by the controller),
//     used to reach the operator's memory/HTTP routes.
//
// This sidecar holds no LLM credential and no "pods" verb — see the design
// spec §13 and the plan-3b Global Constraints.
package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/platform/deplogs"
	"github.com/authzed/openagentprimitives/pkg/tools/workshopmcp"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// buildVersion is the MCP Implementation.Version this server reports to a
// connecting client. Not currently wired to -ldflags; a fixed string is
// enough until the sidecar's own release story exists.
var buildVersion = "0"

// workshopSATokenFile is the fixed path BuildSidecarPod's identity branch
// projects the workshop ServiceAccount token to
// (pkg/controllers/agentsession/sidecarpod.go's workshopTokenMountPath). Both
// sides hard-code this path rather than passing it through an env var — it is
// part of the identity seam's contract, not configuration.
const workshopSATokenFile = "/var/run/secrets/workshop/serviceaccount/token"

// scheme is the SA-token K8s client's type registry: core Kubernetes types
// (Secrets, Namespaces, …) plus every agentprimitives CRD this sidecar's
// tools read or write (WorkshopProbe, SpiceboxToolspec, MCPServer, …).
var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(spiceboxv1alpha1.AddToScheme(scheme))
}

func main() {
	// Before anything can compile a SpiceDB schema (this binary links the
	// v1alpha1 CRD types, which pull in the schema compiler transitively):
	// its compiler logs a trace line per definition through zerolog's
	// process-global logger, and this binary's stderr is `kubectl logs` for
	// every session pod. See pkg/platform/deplogs.
	deplogs.Silence()

	logger := slog.Default()
	if err := run(logger); err != nil {
		logger.Error("workshop: exiting", "err", err.Error())
		os.Exit(1)
	}
}

// probeModeEnv is the env var the SidecarToolbox admission probe
// (pkg/controllers/sidecartoolbox buildProbePod) sets to opt this image into
// declaration-only mode. It is an EXPLICIT signal, deliberately not inferred
// from absent identity inputs: a session sidecar boots identity-less for the
// ~60s its Workshop provisions and is INPUT-IDENTICAL to the probe (only
// MCP_PORT), but its identity is COMING — it must fail-closed (crash) so the
// runner's reachability boot-probe waits for the rebuild-with-identity, not
// serve declaration-only and let the runner run against dead tools. Only the
// probe, which never gets an identity, sets this.
const probeModeEnv = "AP_PROBE_MODE"

// declarationOnlyRequested reports whether this process is the admission
// probe (AP_PROBE_MODE set). See probeModeEnv for why this is an explicit
// flag rather than an inference from missing identity.
func declarationOnlyRequested() bool {
	return os.Getenv(probeModeEnv) != ""
}

// buildServer resolves this process's mode and builds the workshop MCP
// server: declaration-only when every identity input is absent (the
// admission probe), fully wired — or a loud boot error — otherwise. Factored
// from run() so tests can pin the mode decision without a listener.
func buildServer() (*workshopmcp.Server, bool, error) {
	if declarationOnlyRequested() {
		return workshopmcp.NewDeclarationOnly(), true, nil
	}

	k8sClient, err := newWorkshopK8sClient()
	if err != nil {
		// Fail-closed: a workshop sidecar holding any part of an identity
		// must hold all of it — every tool it offers acts as that identity.
		return nil, false, fmt.Errorf("build SA-token K8s client: %w", err)
	}

	memURL := os.Getenv("OPERATOR_MEMORY_URL")
	memToken := os.Getenv("token")
	if memURL == "" || memToken == "" {
		// Fail-closed: a sidecar that cannot reach the operator must not claim
		// it can — every export/inventory-ceiling read goes through this
		// client.
		return nil, false, fmt.Errorf("OPERATOR_MEMORY_URL and token (the operator bearer env var) are both required; got url=%q tokenSet=%v", memURL, memToken != "")
	}
	opsClient := httpclient.New(memURL, memToken)

	identity, err := workshopIdentityFromEnv()
	if err != nil {
		return nil, false, fmt.Errorf("resolve workshop identity: %w", err)
	}

	// fieldOwner is left empty: workshopmcp.NewServer defaults it to its own
	// package-private defaultFieldOwner ("workshop-sidecar") when empty, which
	// is the same value this call passed explicitly before server.go moved to
	// pkg/tools/workshopmcp and that constant became inaccessible from here.
	return workshopmcp.NewServer(k8sClient, opsClient, identity, ""), false, nil
}

func run(logger *slog.Logger) error {
	s, declOnly, err := buildServer()
	if err != nil {
		return err
	}
	if declOnly {
		logger.Info("workshop: no identity inputs present; serving DECLARATION-ONLY (admission-probe mode) — every tool call will be refused")
	}

	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "ap-workshop", Version: buildVersion}, nil)
	s.Register(mcpSrv)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv }, nil)

	mux := http.NewServeMux()
	mux.Handle(mcpPathFromEnv(), handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	port := os.Getenv("MCP_PORT")
	if port == "" {
		return fmt.Errorf("MCP_PORT is required")
	}
	addr := ":" + port
	logger.Info("workshop: listening",
		"addr", addr, "path", mcpPathFromEnv(),
		"declarationOnly", declOnly,
		"workshopNamespace", s.Identity.Namespace,
		"session", s.Identity.SessionNamespace+"/"+s.Identity.SessionName)
	srv := &http.Server{Addr: addr, Handler: mux}
	safehttp.HardenServer(srv)
	if err := srv.ListenAndServe(); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// mcpPathFromEnv returns MCP_PATH, defaulting to "/mcp" when unset.
func mcpPathFromEnv() string {
	if p := os.Getenv("MCP_PATH"); p != "" {
		return p
	}
	return "/mcp"
}

// newWorkshopK8sClient builds the SA-token-bound controller-runtime client:
// the in-cluster Host/CA (apiserver location and trust root do not depend on
// WHICH ServiceAccount is presenting) with the bearer overridden to read from
// the workshop token projection rather than this pod's own
// /var/run/secrets/kubernetes.io/serviceaccount/token (there is none — the
// pod's automount is explicitly off, see sidecarpod.go).
//
// Fails closed when the projected token file is absent: a workshop sidecar
// that cannot prove it holds the workshop's own identity must not serve any
// tool, since every tool call is authorized by the apiserver on that
// identity's RBAC alone.
// workshopSACADir is the mount dir the operator projects the workshop SA
// token AND the cluster CA into (sidecarpod.go's workshopTokenMountPath). The
// pod runs with automountServiceAccountToken off, so the default
// /var/run/secrets/kubernetes.io/serviceaccount is absent — rest.InClusterConfig
// cannot be used, and the client is built from THIS dir alone.
const workshopSACADir = "/var/run/secrets/workshop/serviceaccount"

// workshopRESTConfig builds the API-server config for the workshop SA WITHOUT
// rest.InClusterConfig: that reads token AND ca.crt from the default
// automount path, which this pod does not have. Host comes from the in-pod
// KUBERNETES_SERVICE_* env; the bearer and CA come from the projected mount.
// Fails closed and NAMES the missing piece, so a mis-projected pod surfaces a
// real cause instead of a generic dial error.
func workshopRESTConfig() (*rest.Config, error) {
	if _, err := os.Stat(workshopSATokenFile); err != nil {
		return nil, fmt.Errorf("workshop SA token %s not present: %w", workshopSATokenFile, err)
	}
	caFile := workshopSACADir + "/ca.crt"
	if _, err := os.Stat(caFile); err != nil {
		return nil, fmt.Errorf("cluster CA %s not present (kube-root-ca.crt projection missing): %w", caFile, err)
	}
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not running in-cluster: KUBERNETES_SERVICE_HOST/PORT unset (host=%q port=%q)", host, port)
	}
	return &rest.Config{
		Host:            "https://" + net.JoinHostPort(host, port),
		BearerTokenFile: workshopSATokenFile,
		TLSClientConfig: rest.TLSClientConfig{CAFile: caFile},
	}, nil
}

func newWorkshopK8sClient() (client.Client, error) {
	cfg, err := workshopRESTConfig()
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("build controller-runtime client: %w", err)
	}
	return c, nil
}

// workshopIdentityFromEnv reads the identity BuildSidecarPod's identity
// branch injects (WORKSHOP_NAMESPACE/WORKSHOP_SESSION_NAMESPACE/
// WORKSHOP_SESSION_NAME/WORKSHOP_ID). Fails closed when any is empty: a
// sidecar with an incomplete identity must not guess which workshop it
// belongs to.
func workshopIdentityFromEnv() (workshopmcp.WorkshopIdentity, error) {
	id := workshopmcp.WorkshopIdentity{
		Namespace:        os.Getenv("WORKSHOP_NAMESPACE"),
		SessionNamespace: os.Getenv("WORKSHOP_SESSION_NAMESPACE"),
		SessionName:      os.Getenv("WORKSHOP_SESSION_NAME"),
		WorkshopID:       os.Getenv("WORKSHOP_ID"),
	}
	// Fixed order (not a map range) so the error message is deterministic
	// across runs.
	required := []struct{ name, value string }{
		{"WORKSHOP_NAMESPACE", id.Namespace},
		{"WORKSHOP_SESSION_NAMESPACE", id.SessionNamespace},
		{"WORKSHOP_SESSION_NAME", id.SessionName},
		{"WORKSHOP_ID", id.WorkshopID},
	}
	var missing []string
	for _, r := range required {
		if r.value == "" {
			missing = append(missing, r.name)
		}
	}
	if len(missing) > 0 {
		return workshopmcp.WorkshopIdentity{}, fmt.Errorf("missing required env var(s): %v", missing)
	}
	return id, nil
}
