package cloud

import (
	"context"
	"fmt"
	"io"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

// GatewayControllerParams is the input to Strategy.EnsureGatewayController.
type GatewayControllerParams struct {
	Clients   Clients
	Reporter  Reporter
	In        io.Reader // stdin for the consent prompt
	AssumeYes bool
	// BundledController is the offer-to-install Envoy Gateway Component, built by
	// cmd/oap from its embedded manifests and passed in (pkg/platform/cloud cannot import
	// cmd/oap). Managed-Gateway clouds (GKE) ignore it.
	BundledController *Component
	// GatewayClassOverride is the operator's --gateway-class; when set the
	// strategy skips detection/installation and returns it verbatim.
	GatewayClassOverride string
}

// GatewayControllerResult is the output of Strategy.EnsureGatewayController.
// Proceed=false means external access must be skipped (the strategy already
// surfaced why); GatewayClass is then irrelevant.
type GatewayControllerResult struct {
	Proceed      bool
	GatewayClass string
}

// GatewayAPIServed reports whether the cluster serves the Gateway API
// (gateway.networking.k8s.io). It uses ServerGroups discovery with a fresh
// client each call (no stale RESTMapper cache), so a missing API surfaces as a
// clean false rather than the cryptic "no matches for gateway.networking.k8s.io/v1"
// an apply/read would emit.
func GatewayAPIServed(restCfg *rest.Config) (bool, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return false, fmt.Errorf("build discovery client: %w", err)
	}
	groups, err := dc.ServerGroups()
	if err != nil {
		return false, fmt.Errorf("list server API groups: %w", err)
	}
	for _, g := range groups.Groups {
		if g.Name == "gateway.networking.k8s.io" {
			return true, nil
		}
	}
	return false, nil
}

const (
	// EnvoyGatewayClass is the GatewayClass name oap binds to the bundled Envoy
	// Gateway controller on clouds without a managed Gateway controller.
	EnvoyGatewayClass = "eg"
	// EnvoyGatewayControllerName is the controllerName the Envoy Gateway bundle
	// reconciles; the eg GatewayClass points at it.
	EnvoyGatewayControllerName = "gateway.envoyproxy.io/gatewayclass-controller"
)

// servedCheck is GatewayAPIServed, indirected through a package var so this
// package's tests can drive the served/not-served branches without a real
// apiserver. Production always uses the real discovery check.
var servedCheck = GatewayAPIServed

// gatewayClassGVR is the cluster-scoped GatewayClass resource.
var gatewayClassGVR = schema.GroupVersionResource{
	Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gatewayclasses",
}

// EnsureEnvoyGatewayController makes a Gateway controller + the Gateway API
// available on clouds that ship without their own controller (EKS/AKS/local). It honors
// an explicit --gateway-class override, reuses any GatewayClass already on the
// cluster, and otherwise offers to install the bundled Envoy Gateway (via
// EnsureComponent) and creates the "eg" GatewayClass. Proceed=false means the
// operator declined the install (EnsureComponent already printed the manual
// command) and the caller skips external access.
func EnsureEnvoyGatewayController(ctx context.Context, p GatewayControllerParams) (GatewayControllerResult, error) {
	if p.GatewayClassOverride != "" {
		return GatewayControllerResult{Proceed: true, GatewayClass: p.GatewayClassOverride}, nil
	}

	// If the Gateway API is already served, an existing GatewayClass means a
	// controller is already installed — reuse it (prefer "eg") and skip the
	// install. When not served, skip the list (it would fail with a discovery
	// error) and go straight to installing the controller, which brings the CRDs.
	served, err := servedCheck(p.Clients.REST)
	if err != nil {
		return GatewayControllerResult{}, fmt.Errorf("check Gateway API availability: %w", err)
	}
	if served {
		list, err := p.Clients.Dynamic.Resource(gatewayClassGVR).List(ctx, metav1.ListOptions{})
		if err != nil {
			return GatewayControllerResult{}, fmt.Errorf("list GatewayClasses: %w", err)
		}
		var first string
		for i := range list.Items {
			name := list.Items[i].GetName()
			if name == EnvoyGatewayClass {
				return GatewayControllerResult{Proceed: true, GatewayClass: EnvoyGatewayClass}, nil
			}
			if first == "" {
				first = name
			}
		}
		if first != "" {
			return GatewayControllerResult{Proceed: true, GatewayClass: first}, nil
		}
	}

	if p.BundledController == nil {
		return GatewayControllerResult{}, fmt.Errorf("no bundled Gateway controller available to install")
	}
	present, err := EnsureComponent(ctx, p.Reporter, p.In, p.Clients, *p.BundledController, p.AssumeYes)
	if err != nil {
		return GatewayControllerResult{}, err
	}
	if !present {
		return GatewayControllerResult{Proceed: false}, nil
	}
	if err := ensureEnvoyGatewayClass(ctx, p.Clients); err != nil {
		return GatewayControllerResult{}, err
	}
	return GatewayControllerResult{Proceed: true, GatewayClass: EnvoyGatewayClass}, nil
}

// ensureEnvoyGatewayClass creates the "eg" GatewayClass bound to the Envoy
// controller via the dynamic client. Idempotent: an AlreadyExists is success.
func ensureEnvoyGatewayClass(ctx context.Context, cl Clients) error {
	gc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "GatewayClass",
		"metadata":   map[string]any{"name": EnvoyGatewayClass},
		"spec":       map[string]any{"controllerName": EnvoyGatewayControllerName},
	}}
	_, err := cl.Dynamic.Resource(gatewayClassGVR).Create(ctx, gc, metav1.CreateOptions{FieldManager: "ap-install"})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %s GatewayClass: %w", EnvoyGatewayClass, err)
	}
	return nil
}
