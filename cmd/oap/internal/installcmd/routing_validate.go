package installcmd

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// withHostnameSuffix expands the convenience --hostname-suffix into the two webd
// origins: trusted = webd.<suffix>, sandbox = sandbox.<suffix> (the sandbox is
// omitted when --disable-artifact-viewer). It is mutually exclusive with the
// explicit --trusted-hostname/--sandbox-hostname and with --manual-webd-routing.
// Pure; run before validateWebdRouting. Clears hostnameSuffix once consumed so
// it is idempotent (oap init derives, then passes the opts to RunInstall).
func (r WebdRoutingOpts) withHostnameSuffix() (WebdRoutingOpts, error) {
	suffix := strings.TrimPrefix(strings.TrimSpace(r.hostnameSuffix), ".")
	if suffix == "" {
		r.hostnameSuffix = ""
		return r, nil
	}
	if r.trustedHostname != "" || r.sandboxHostname != "" {
		return r, fmt.Errorf("--hostname-suffix is mutually exclusive with --trusted-hostname/--sandbox-hostname (it derives webd.<suffix> and sandbox.<suffix> for you)")
	}
	if r.manualWebdRouting {
		return r, fmt.Errorf("--hostname-suffix and --manual-webd-routing are mutually exclusive: one sets up webd routing, the other skips it")
	}
	r.trustedHostname = "webd." + suffix
	if !r.disableViewer {
		r.sandboxHostname = "sandbox." + suffix
	}
	r.hostnameSuffix = ""
	return r, nil
}

// validateWebdRouting enforces the no-silent-failure rules for webd external
// access. It is pure (no cluster calls) so it can run before any mutation.
//
//   - sandbox without trusted                       -> error
//   - trusted + manual-routing (contradictory)      -> error
//   - sandbox + disable-viewer (contradictory)      -> error
//   - trusted == sandbox                            -> error
//   - trusted set, no sandbox, no --disable-viewer  -> error
//   - no trusted, not opted out (--manual-routing),
//     on a managed non-local cluster                -> error
//
// localMode is gone: the profile answers whether this kind tolerates an
// install with no external hostname. `local` and `default` do (a dev tunnel
// and a service-only on-prem cluster respectively); the managed clouds do
// not, where the alternative is a cluster reachable only via
// `kubectl port-forward`.
func validateWebdRouting(p cloud.InstallProfile, trusted, sandbox string, disableViewer, manualRouting bool) error {
	if sandbox != "" && trusted == "" {
		return fmt.Errorf("--sandbox-hostname requires --trusted-hostname")
	}
	if trusted != "" && manualRouting {
		return fmt.Errorf("--trusted-hostname and --manual-webd-routing are mutually exclusive: either oap sets up webd routing or you do")
	}
	if sandbox != "" && disableViewer {
		return fmt.Errorf("--sandbox-hostname and --disable-artifact-viewer cannot both be set: choose a sandbox origin or disable the viewer")
	}
	if trusted != "" {
		if sandbox == "" && !disableViewer {
			return fmt.Errorf("--trusted-hostname is set without --sandbox-hostname: the artifact viewer needs a distinct sandbox origin. Pass --sandbox-hostname=<host> (must differ from --trusted-hostname) or --disable-artifact-viewer to install without it")
		}
		if sandbox != "" && sandbox == trusted {
			return fmt.Errorf("--sandbox-hostname (%s) must differ from --trusted-hostname: the sandbox is a distinct origin for cross-origin isolation of untrusted artifact content", sandbox)
		}
		return nil
	}
	// trusted == "" here.
	if manualRouting {
		return nil // explicit opt-out
	}
	if p.RequiresExternalHostname() {
		return fmt.Errorf("installing on a managed cloud cluster without an external hostname leaves webd reachable only via `kubectl port-forward`. oap can't invent a hostname — you supply one you control and oap provisions the Gateway + TLS cert and prints the DNS target to point it at. Pass --hostname-suffix=<your-domain> (derives webd.<domain> + sandbox.<domain>), or --trusted-hostname/--sandbox-hostname for explicit control, or --manual-webd-routing to wire routing yourself")
	}
	return nil
}
