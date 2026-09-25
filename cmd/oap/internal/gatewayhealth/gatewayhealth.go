// Package gatewayhealth is a registry of per-cloud backend-health provisioners
// for webd's external-access Gateway.
//
// `oap install` synthesizes webd's Gateway + HTTPRoutes; the cloud's managed L7
// load balancer then health-checks the webd backend before it will serve
// traffic. How that backend health check is configured is cloud-dependent.
//
// On GKE this matters: GKE's Gateway API does NOT infer the backend health
// check from the pod readiness probe (only the legacy GKE *Ingress* controller
// does that). The managed Gateway therefore defaults to probing "/" on the webd
// backend, which webd answers with 404 (its health endpoint is /healthz) — so
// GKE marks the backend unhealthy → 503 → webd is unreachable. The fix is a GKE
// HealthCheckPolicy pointing the check at /healthz. Other clouds run Envoy
// Gateway, which health-checks via the pod readiness probe and need nothing.
//
// This package factors that choice as a REGISTRY rather than a switch in the
// install code: each Provisioner registers itself and declares the cloud(s) it
// serves; the consumer dispatches via For(cloud). A Provisioner registered with no clouds is the default (used by
// every cloud no provisioner explicitly claims). The install consumer therefore
// contains no `if cloud == "gke"` branch — adding a new per-cloud backend-health
// flow is a new registration, not a new case.
//
// The registry is keyed on the cloud as a plain string (e.g. "gke") so this
// package does not import package main's Cloud type; callers pass string(cloud).
package gatewayhealth

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// Params carries the inputs a Provisioner needs to provision any cloud-specific
// backend-health config the webd Gateway requires after its routes exist. It is
// deliberately minimal: the impls read the webd Service name/namespace from
// package-main constants, so only the reporter and the cluster client bundle are
// passed.
type Params struct {
	// Reporter is the user-facing output sink; provisioners narrate through it so
	// their output stays consistent with the rest of the install flow's UI.
	Reporter cloud.Reporter
	// Bundle is the cluster client bundle (typed/dynamic/controller-runtime).
	Bundle *kube.Bundle
}

// Provisioner provisions any cloud-specific backend-health configuration the
// webd Gateway needs once its HTTPRoutes exist (so the backend Service is
// referenced). It runs AFTER the routes are applied.
type Provisioner interface {
	// Provision applies the cloud's backend-health config idempotently. A no-op
	// provisioner is registered as the default, so callers never get nil from
	// For and need no nil check.
	Provision(ctx context.Context, p Params) error
}

// noopProvisioner is the default: non-GKE external Gateways are Envoy Gateways,
// whose backend health check follows the pod readiness probe — they need no
// extra cloud-specific HealthCheckPolicy object. Registering this as the default
// keeps For non-nil so the consumer needs no nil check.
type noopProvisioner struct{}

func (noopProvisioner) Provision(context.Context, Params) error { return nil }

func init() { Register(noopProvisioner{}) }

// --- registry ---

var registry = struct {
	byCloud map[string]Provisioner
	def     Provisioner
}{byCloud: map[string]Provisioner{}}

// Register adds p to the registry for the given cloud strings. Registering with
// no clouds installs p as the DEFAULT, used by any cloud no provisioner
// explicitly claims. Re-registering the same cloud (or a second default) panics:
// that is a programming error (two provisioners fighting over one cloud), caught
// at init.
func Register(p Provisioner, clouds ...string) {
	if p == nil {
		panic("gatewayhealth: Register(nil)")
	}
	if len(clouds) == 0 {
		if registry.def != nil {
			panic(fmt.Sprintf("gatewayhealth: default provisioner already registered (%T); cannot also register %T as default", registry.def, p))
		}
		registry.def = p
		return
	}
	for _, c := range clouds {
		if existing, ok := registry.byCloud[c]; ok {
			panic(fmt.Sprintf("gatewayhealth: cloud %q already registered to %T; cannot also register %T", c, existing, p))
		}
		registry.byCloud[c] = p
	}
}

// For returns the Provisioner serving cloud, falling back to the default
// provisioner when no provisioner explicitly claims cloud. Because this package
// registers a no-op default in its own init(), For is never nil — the consumer
// needs no nil check.
func For(cloud string) Provisioner {
	if p, ok := registry.byCloud[cloud]; ok {
		return p
	}
	return registry.def
}
