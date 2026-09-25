// cmd/oap/internal/installcmd/webdurl.go — how `oap install` hands webd's
// external URL over to the cluster.
//
// The ConfigMap it names has exactly one owner at a time, and TWO facts decide
// which:
//
//  1. THE ROUTING FLAGS, which say whether install itself owns the value.
//     `--trusted-hostname` means install provisions a Gateway and a
//     certificate and then patches the two keys with the genuine https://
//     hosts (patchWebdExternalURLs, called from setupWebdExternalAccess).
//     `--manual-webd-routing` means the operator owns them by hand, which its
//     own message promises. Either way the value is spoken for, and nothing
//     else may take it — see webdExternalURLHasAnotherOwner.
//  2. THE CLUSTER KIND, which says whether a tunnel may reach this cluster at
//     all. Where the routing flags leave the value unclaimed AND the kind's
//     policy answers CreatedAtInstall, install creates a PublicEndpoint and
//     writes nothing itself: the operator's reconciler owns the ConfigMap
//     from then on, publishing spec.localURL while no tunnel is up and
//     status.url once one is.
//
// The two are independent, and the combination is real: `--cluster-kind=local
// --trusted-hostname=h` installs Envoy Gateway on a local cluster and is
// supported. Gating on the kind alone would create an endpoint whose
// controller then seizes the very keys the Gateway path just filled in, under
// ForceOwnership, and rewrites them to a loopback address.
//
// The creation itself is not here. It lives in
// cmd/oap/internal/publicendpoint, the one door every path that creates a
// PublicEndpoint goes through — install's, `oap agent install`'s and the
// desktop's alike — so that cloud.CheckPublicEndpointAllowed is asked before
// anything is written. What is here is install's own half of the decision:
// which flags claim the URL, and what the kind's policy then says.
//
// webdLocalSeedURL is shared with ensureWebdExternalURLConfigMap's seed rather
// than retyped, so the controller's first write carries the same DATA the
// ConfigMap already holds and no consumer ever sees the URL change. (The apply
// still transfers field ownership from install's manager and bumps
// resourceVersion once; what it does not do is flap the value.)
package installcmd

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// webdLocalSeedURL is where webd is reachable from the host on an install that
// has no external hostname: a `kubectl port-forward` to spicebox-webd:8080
// binds exactly this.
//
// It is ONE constant because two things must agree on it byte for byte — the
// ConfigMap ensureWebdExternalURLConfigMap seeds, and the PublicEndpoint's
// spec.localURL the operator publishes into that same ConfigMap while no tunnel
// is up. Two literals that merely look alike would change the VALUE webd reads
// on the controller's first write, for no change in meaning.
const webdLocalSeedURL = "http://localhost:8080"

// patchWebdExternalURLs updates the spicebox-webd-external-url ConfigMap's
// trusted-url + sandbox-url keys in a single Update. It is for the
// REAL-INGRESS path only: the two URLs are the genuine https:// hosts
// setupWebdExternalAccess just provisioned a Gateway and certificate for.
//
// The ConfigMap is created by `oap install`; if it is missing we surface a
// clear "run oap install first" error rather than creating it here, which
// would let this function's idea of the defaults drift from install's.
//
// It runs only where webdExternalURLHasAnotherOwner is true, which is exactly
// where no PublicEndpoint is created — one owner at a time.
func patchWebdExternalURLs(ctx context.Context, c client.Client, trustedURL, sandboxURL string) error {
	var cm corev1.ConfigMap
	key := types.NamespacedName{
		Namespace: cloud.WebdServiceNamespace,
		Name:      v1alpha1.WebdExternalURLConfigMap,
	}
	if err := c.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("ConfigMap %s/%s not found — run `oap install` first",
				cloud.WebdServiceNamespace, v1alpha1.WebdExternalURLConfigMap)
		}
		return fmt.Errorf("get configmap: %w", err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[v1alpha1.WebdTrustedURLKey] = trustedURL
	cm.Data[v1alpha1.WebdSandboxURLKey] = sandboxURL
	if err := c.Update(ctx, &cm); err != nil {
		return fmt.Errorf("update configmap: %w", err)
	}
	return nil
}

// webdExternalURLHasAnotherOwner reports whether the routing flags have already
// spoken for webd's external-URL ConfigMap, leaving nothing for a
// PublicEndpoint to own.
//
// Two flags claim it, for different reasons:
//
//   - --trusted-hostname: install provisions a Gateway and a certificate and
//     then patches the two keys with the real https:// hosts
//     (setupWebdExternalAccess → patchWebdExternalURLs). An endpoint created
//     alongside would hand those same keys to a controller that applies them
//     under ForceOwnership every reconcile, rewriting a working public
//     hostname to http://localhost:8080 and then to a tunnel URL. It also
//     breaks the byte-identity that makes the controller's first write
//     invisible: with a trusted hostname the seed is EMPTY, not
//     webdLocalSeedURL.
//   - --manual-webd-routing: install writes no routing at all and tells the
//     operator, in as many words, to configure the ConfigMap themselves.
//     Creating an endpoint would break that promise silently.
//
// This is deliberately NOT a refusal in validateWebdRouting. `--cluster-kind=local
// --trusted-hostname=h --sandbox-hostname=s` is a supported, sensible install —
// local.Strategy.EnsureGatewayController installs Envoy Gateway for exactly
// that case — and a local cluster reachable at a real hostname simply does not
// need a tunnel. The combination is not a mistake to reject; it is an answer to
// "who owns the URL", and this function is where it is answered.
func webdExternalURLHasAnotherOwner(r WebdRoutingOpts) bool {
	return r.trustedHostname != "" || r.manualWebdRouting
}

// webdURLClaimFlag names WHICH of the two flags claimed the URL, for the
// refusal an existing endpoint earns (publicendpoint.RefuseWebdEndpointWhenURLIsClaimed).
//
// The name is in the message because the remedy differs by flag — one is
// dropped to keep the tunnel, the other means the operator intended to own the
// keys by hand — and "something claims it" is not an instruction. Empty when
// nothing claims it, which is the case that never reaches the refusal.
func webdURLClaimFlag(r WebdRoutingOpts) string {
	switch {
	case r.trustedHostname != "":
		return "--trusted-hostname"
	case r.manualWebdRouting:
		return "--manual-webd-routing"
	default:
		return ""
	}
}

// installCreatesWebdPublicEndpoint is the WHOLE decision `oap install` makes
// about creating a PublicEndpoint, in one place, so the call site holds no
// condition of its own and the composition is testable across both inputs.
//
// Both conjuncts are load-bearing and neither implies the other:
//
//   - Nothing else already owns webd's external URL. The routing flags decide
//     this, and they are independent of the cluster kind:
//     `--cluster-kind=local --trusted-hostname=h` is a supported install (local
//     installs Envoy Gateway for exactly that), and creating an endpoint there
//     would hand the Gateway's two ConfigMap keys to a controller that
//     re-applies them under ForceOwnership on every reconcile.
//   - The kind's policy says install is the thing that creates one. `desktop`
//     answers OnDemand and creates nothing here; every durable kind is refused
//     outright inside publicendpoint.EnsureWebd.
func installCreatesWebdPublicEndpoint(r WebdRoutingOpts, strat cloud.Strategy) bool {
	if webdExternalURLHasAnotherOwner(r) {
		return false
	}
	return strat.InstallProfile().PublicEndpointPolicy().CreatedAtInstall()
}
