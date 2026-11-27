package installcmd

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/gatewayhealth"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// gkeGatewayHealth provisions a GKE HealthCheckPolicy that points the managed L7
// Gateway's backend health check at webd's /healthz endpoint. It is registered
// for GKE only.
//
// Why GKE differs: GKE's Gateway API does NOT infer the backend health check
// from the pod readiness probe (only the legacy GKE *Ingress* controller does
// that). The managed Gateway (gke-l7-global-external-managed) therefore defaults
// to probing "/" on the webd backend, which webd answers with 404 (its health
// endpoint is /healthz, which returns 200) — so GKE marks the backend unhealthy
// → 503 → webd is unreachable. The HealthCheckPolicy below redirects the probe
// to /healthz. The CRD (networking.gke.io/v1 HealthCheckPolicy) only exists on
// GKE; other clouds run Envoy Gateway, which health-checks via the pod readiness
// probe and need nothing (the registry's default no-op).
//
// This is the SIBLING of the LB-source-range NetworkPolicy
// (ensureWebdGatewayIngress): the NetworkPolicy lets GKE's LB health-check
// ranges reach webd at all, and this HealthCheckPolicy makes that probe hit
// /healthz instead of "/". Both are required.
type gkeGatewayHealth struct{}

func init() { gatewayhealth.Register(gkeGatewayHealth{}, cloud.KeyGKE) }

// Manifests returns the GKE HealthCheckPolicy that targets the webd Service and
// points the backend health check at /healthz. Built as an unstructured object
// (the CRD has no typed Go struct). Shared by Provision (`oap install`) and
// `oap render`, so both emit one definition.
func (gkeGatewayHealth) Manifests() []*unstructured.Unstructured {
	return []*unstructured.Unstructured{{Object: map[string]any{
		"apiVersion": "networking.gke.io/v1",
		"kind":       "HealthCheckPolicy",
		"metadata": map[string]any{
			"name":      cloud.WebdGatewayName,
			"namespace": cloud.WebdServiceNamespace,
		},
		"spec": map[string]any{
			"default": map[string]any{
				"config": map[string]any{
					"type": "HTTP",
					"httpHealthCheck": map[string]any{
						"requestPath": "/healthz",
					},
				},
			},
			"targetRef": map[string]any{
				"group": "",
				"kind":  "Service",
				"name":  cloud.WebdGatewayName,
			},
		},
	}}}
}

// Provision applies Manifests idempotently via kube.Apply, which resolves the
// GVR from apiVersion/kind (networking.gke.io/v1 HealthCheckPolicy → namespaced
// healthcheckpolicies) and does a server-side apply with create/update fallback.
func (g gkeGatewayHealth) Provision(ctx context.Context, p gatewayhealth.Params) error {
	p.Reporter.Step("provision GKE backend health check (HealthCheckPolicy → /healthz)")
	for _, obj := range g.Manifests() {
		if err := kube.Apply(ctx, p.Bundle.Dynamic, obj, "ap-install"); err != nil {
			return fmt.Errorf("apply %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
		}
		p.Reporter.Info("  %s %s/%s health-checks webd at /healthz", obj.GetKind(), obj.GetNamespace(), obj.GetName())
	}
	return nil
}
