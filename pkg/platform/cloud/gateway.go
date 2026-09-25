package cloud

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Webd Gateway / TLS constants. These are the canonical resource names for
// webd's external-access Gateway, its TLS Secrets, and the Let's Encrypt
// ClusterIssuer oap creates.
const (
	WebdGatewayName           = "spicebox-webd"
	WebdServiceNamespace      = "agentprimitives-system"
	WebdTrustedCertSecretName = "spicebox-webd-trusted-tls"
	WebdSandboxCertSecretName = "spicebox-webd-sandbox-tls"
	WebdLetsEncryptIssuerName = "spicebox-webd-letsencrypt"

	// WebdServiceName is the Kubernetes Service name for webd. Used by
	// netcidrs.go (NetworkPolicy pod selector) and cmd/oap (port-forward).
	WebdServiceName = "spicebox-webd"
	// WebdServicePort is the container port webd listens on. Used by
	// netcidrs.go (NetworkPolicy ingress port) and cmd/oap (port-forward).
	WebdServicePort int32 = 8080
	// WebdPodSelector matches webd's Pods. A port-forward resolves the Pod to
	// bind through this label, since a Service name alone does not name one.
	WebdPodSelector = "app.kubernetes.io/name=" + WebdServiceName
)

const letsEncryptProdACME = "https://acme-v02.api.letsencrypt.org/directory"

var clusterIssuerGVR = schema.GroupVersionResource{
	Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers",
}

var certificateGVR = schema.GroupVersionResource{
	Group: "cert-manager.io", Version: "v1", Resource: "certificates",
}

// ApplyWebdGateway creates/updates the Gateway: an HTTPS listener per non-empty
// host plus one HTTP listener on :80 for the ACME HTTP-01 challenge. The
// per-listener TLS config and any Gateway-level annotations are supplied by the
// active TLS strategy (gw): cert-manager terminates TLS with a per-hostname
// Secret the listener references (filled by explicit Certificates,
// ApplyWebdCertificate — not the gateway-shim); google-managed attaches a
// Certificate Manager map via an annotation and leaves the listeners without
// certificateRefs. Idempotent; updates Spec + the strategy's annotations in
// place.
func ApplyWebdGateway(ctx context.Context, cl Clients, gatewayClass, trustedHost, sandboxHost string, gw GatewayTLS) error {
	httpsListener := func(name, host string) gatewayv1.Listener {
		h := gatewayv1.Hostname(host)
		return gatewayv1.Listener{
			Name:     gatewayv1.SectionName(name),
			Hostname: &h,
			Port:     443,
			Protocol: gatewayv1.HTTPSProtocolType,
			// The strategy decides the listener's TLS shape: a Secret-backed
			// certificateRefs (cert-manager) or nil (google-managed — the cert
			// is attached to the Gateway by annotation).
			TLS: HTTPSListenerTLS(gw.Listener, host),
		}
	}

	listeners := []gatewayv1.Listener{
		httpsListener("trusted-https", trustedHost),
	}
	if sandboxHost != "" {
		listeners = append(listeners, httpsListener("sandbox-https", sandboxHost))
	}
	listeners = append(listeners, gatewayv1.Listener{
		Name:     "acme-http",
		Port:     80,
		Protocol: gatewayv1.HTTPProtocolType,
	})

	desired := gatewayv1.GatewaySpec{
		GatewayClassName: gatewayv1.ObjectName(gatewayClass),
		Listeners:        listeners,
	}

	var existing gatewayv1.Gateway
	key := client.ObjectKey{Namespace: WebdServiceNamespace, Name: WebdGatewayName}
	err := cl.Ctrl.Get(ctx, key, &existing)
	switch {
	case err == nil:
		existing.Spec = desired
		// oap issues the cert-manager listener certs via explicit Certificates
		// (ApplyWebdCertificate), not cert-manager's gateway-shim — drop any
		// stale cluster-issuer annotation from a prior install so the shim
		// doesn't also act on this Gateway.
		delete(existing.Annotations, "cert-manager.io/cluster-issuer")
		setGatewayAnnotations(&existing, gw.Annotations)
		return cl.Ctrl.Update(ctx, &existing)
	case apierrors.IsNotFound(err):
		obj := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Namespace: WebdServiceNamespace, Name: WebdGatewayName},
			Spec:       desired,
		}
		setGatewayAnnotations(obj, gw.Annotations)
		return cl.Ctrl.Create(ctx, obj)
	default:
		return fmt.Errorf("get Gateway %s/%s: %w", WebdServiceNamespace, WebdGatewayName, err)
	}
}

// setGatewayAnnotations stamps the strategy's annotations onto the Gateway,
// preserving any others already present. A nil/empty map is a no-op (so the
// cert-manager strategy, which contributes none, leaves the object untouched).
func setGatewayAnnotations(gw *gatewayv1.Gateway, anns map[string]string) {
	if len(anns) == 0 {
		return
	}
	if gw.Annotations == nil {
		gw.Annotations = map[string]string{}
	}
	for k, v := range anns {
		gw.Annotations[k] = v
	}
}

// ApplyWebdCertificate creates/updates a cert-manager Certificate for one webd
// hostname, named after its TLS Secret. These are created EXPLICITLY rather than
// via cert-manager's gateway-shim, which must be separately enabled and is too
// unreliable at reconciling the GKE Gateway.
//
// The issue-temporary-certificate annotation makes cert-manager write a
// self-signed cert into the Secret IMMEDIATELY, breaking a deadlock: the LB
// address is needed for the HTTP-01 challenge, but GKE's Gateway controller
// refuses to program the LB without the Secret (event GWCER102). Once DNS points
// at the LB and HTTP-01 validates, cert-manager swaps in the trusted cert.
//
// Applied as unstructured to avoid a cert-manager Go dependency. Idempotent.
func ApplyWebdCertificate(ctx context.Context, cl Clients, namespace, secretName, dnsName, issuerName string) error {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name":        secretName,
			"namespace":   namespace,
			"annotations": map[string]any{"cert-manager.io/issue-temporary-certificate": "true"},
		},
		"spec": map[string]any{
			"secretName": secretName,
			"dnsNames":   []any{dnsName},
			"issuerRef":  map[string]any{"name": issuerName, "kind": "ClusterIssuer", "group": "cert-manager.io"},
		},
	}}
	ri := cl.Dynamic.Resource(certificateGVR).Namespace(namespace)
	existing, err := ri.Get(ctx, secretName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, cErr := ri.Create(ctx, obj, metav1.CreateOptions{})
		return cErr
	case err != nil:
		return fmt.Errorf("get Certificate %s: %w", secretName, err)
	default:
		obj.SetResourceVersion(existing.GetResourceVersion())
		_, uErr := ri.Update(ctx, obj, metav1.UpdateOptions{})
		return uErr
	}
}

// ApplyLetsEncryptClusterIssuer creates/updates a cert-manager ClusterIssuer
// that solves ACME HTTP-01 challenges through the given Gateway
// (gatewayHTTPRoute solver). Applied as unstructured to avoid a cert-manager Go
// dependency.
func ApplyLetsEncryptClusterIssuer(ctx context.Context, cl Clients, name, email, gatewayName, gatewayNamespace string) error {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "ClusterIssuer",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"acme": map[string]any{
				"server":              letsEncryptProdACME,
				"email":               email,
				"privateKeySecretRef": map[string]any{"name": name + "-account-key"},
				"solvers": []any{
					map[string]any{
						"http01": map[string]any{
							"gatewayHTTPRoute": map[string]any{
								"parentRefs": []any{
									map[string]any{
										"name":      gatewayName,
										"namespace": gatewayNamespace,
										"kind":      "Gateway",
										"group":     "gateway.networking.k8s.io",
									},
								},
							},
						},
					},
				},
			},
		},
	}}

	existing, err := cl.Dynamic.Resource(clusterIssuerGVR).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, cErr := cl.Dynamic.Resource(clusterIssuerGVR).Create(ctx, obj, metav1.CreateOptions{})
		return cErr
	case err != nil:
		return fmt.Errorf("get ClusterIssuer %s: %w", name, err)
	default:
		obj.SetResourceVersion(existing.GetResourceVersion())
		_, uErr := cl.Dynamic.Resource(clusterIssuerGVR).Update(ctx, obj, metav1.UpdateOptions{})
		return uErr
	}
}

// DetectLetsEncryptEmail reads the ACME account email off the webd Let's Encrypt
// ClusterIssuer (spec.acme.email). Returns "" with no error when the issuer does
// not exist yet — a fresh cluster, or an install that used --tls-issuer — so a
// caller can seed it as a prompt default and fall through to asking when absent.
func DetectLetsEncryptEmail(ctx context.Context, cl Clients) (string, error) {
	obj, err := cl.Dynamic.Resource(clusterIssuerGVR).Get(ctx, WebdLetsEncryptIssuerName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("get ClusterIssuer %s: %w", WebdLetsEncryptIssuerName, err)
	}
	email, _, err := unstructured.NestedString(obj.Object, "spec", "acme", "email")
	if err != nil {
		return "", fmt.Errorf("read spec.acme.email from ClusterIssuer %s: %w", WebdLetsEncryptIssuerName, err)
	}
	return email, nil
}

// EnsureCertManagerGatewayAPI turns on cert-manager's Gateway API integration
// so the HTTP-01 gatewayHTTPRoute solver can present ACME challenges through
// the existing Gateway (rather than provisioning a separate Ingress load
// balancer). cert-manager gates this behind the controller's
// --enable-gateway-api flag (the ExperimentalGatewayAPISupport feature gate is
// on by default as of v1.15); without it every challenge fails "gateway api is
// not enabled" and the cert never issues. Idempotent: a no-op once the flag is
// present.
func EnsureCertManagerGatewayAPI(ctx context.Context, cl Clients, rep Reporter) error {
	const ns, name, flag = "cert-manager", "cert-manager", "--enable-gateway-api"
	dep, err := cl.Typed.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get cert-manager deployment: %w", err)
	}
	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return fmt.Errorf("cert-manager deployment %s/%s has no containers", ns, name)
	}
	for _, a := range dep.Spec.Template.Spec.Containers[0].Args {
		if a == flag {
			return nil // already enabled
		}
	}
	dep.Spec.Template.Spec.Containers[0].Args = append(dep.Spec.Template.Spec.Containers[0].Args, flag)
	if _, err := cl.Typed.AppsV1().Deployments(ns).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("enable cert-manager Gateway API integration: %w", err)
	}
	rep.Info("  enabled cert-manager Gateway API integration (--enable-gateway-api)")
	if err := WaitForDeployment(ctx, cl, ns, name, 1); err != nil {
		return fmt.Errorf("wait for cert-manager after enabling Gateway API: %w", err)
	}
	return nil
}

// WaitForDeployment polls until the named Deployment has at least wantReady
// ready replicas, or the parent context deadline elapses. The poll interval is
// 2s with a 2-minute sub-deadline (in addition to any parent ctx deadline).
func WaitForDeployment(ctx context.Context, cl Clients, namespace, name string, wantReady int32) error {
	deadline := 2 * time.Minute
	deadlineCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	for {
		d, err := cl.Typed.AppsV1().Deployments(namespace).Get(deadlineCtx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get deployment %s/%s: %w", namespace, name, err)
		}
		if d.Status.ReadyReplicas >= wantReady {
			return nil
		}
		select {
		case <-deadlineCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("wait: timeout waiting for deployment %s/%s", namespace, name)
		case <-time.After(2 * time.Second):
		}
	}
}
