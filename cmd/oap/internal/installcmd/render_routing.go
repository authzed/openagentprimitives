package installcmd

import (
	"bytes"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// routingResourceFile is the overlay file holding the emitted Gateway + HTTPRoutes.
const routingResourceFile = "routing.yaml"

// renderRoutingResources builds webd's external-access Gateway + HTTPRoute(s) as
// marshaled YAML, but ONLY when a trusted hostname is configured — routing is
// opt-in, so a render with no --trusted-hostname emits nothing and the user
// brings their own ingress.
//
// cert-manager objects (ClusterIssuer / Certificate) are deliberately NOT
// emitted: the HTTPS listeners reference the standard cert Secret names, and the
// platform provides the issuer and certificates (the GitOps norm — ingress and
// cert-manager are cluster-wide concerns, not per-app bundle state).
func renderRoutingResources(r WebdRoutingOpts) (out []byte, emitted bool, err error) {
	if r.trustedHostname == "" {
		return nil, false, nil
	}
	docs := make([][]byte, 0, 3)

	gw, err := marshalClean(buildWebdGateway(r))
	if err != nil {
		return nil, false, err
	}
	docs = append(docs, gw)

	tr, err := marshalClean(buildWebdHTTPRoute(webdTrustedRouteName, r.trustedHostname))
	if err != nil {
		return nil, false, err
	}
	docs = append(docs, tr)

	if r.sandboxHostname != "" {
		sb, err := marshalClean(buildWebdHTTPRoute(webdSandboxRouteName, r.sandboxHostname))
		if err != nil {
			return nil, false, err
		}
		docs = append(docs, sb)
	}

	return bytes.Join(docs, []byte("---\n")), true, nil
}

// buildWebdGateway mirrors cloud.ApplyWebdGateway's listener shape (trusted-https
// 443, optional sandbox-https 443, acme-http 80) but with cert-manager
// Secret-backed TLS (Terminate + certificateRefs), the portable shape. TypeMeta
// is set explicitly so kustomize can parse the marshaled object.
func buildWebdGateway(r WebdRoutingOpts) *gatewayv1.Gateway {
	terminate := gatewayv1.TLSModeTerminate
	httpsListener := func(name, host, certSecret string) gatewayv1.Listener {
		h := gatewayv1.Hostname(host)
		return gatewayv1.Listener{
			Name:     gatewayv1.SectionName(name),
			Hostname: &h,
			Port:     443,
			Protocol: gatewayv1.HTTPSProtocolType,
			TLS: &gatewayv1.GatewayTLSConfig{
				Mode:            &terminate,
				CertificateRefs: []gatewayv1.SecretObjectReference{{Name: gatewayv1.ObjectName(certSecret)}},
			},
		}
	}
	listeners := []gatewayv1.Listener{httpsListener("trusted-https", r.trustedHostname, cloud.WebdTrustedCertSecretName)}
	if r.sandboxHostname != "" {
		listeners = append(listeners, httpsListener("sandbox-https", r.sandboxHostname, cloud.WebdSandboxCertSecretName))
	}
	listeners = append(listeners, gatewayv1.Listener{Name: "acme-http", Port: 80, Protocol: gatewayv1.HTTPProtocolType})
	return &gatewayv1.Gateway{
		TypeMeta:   metav1.TypeMeta{APIVersion: gatewayv1.GroupVersion.String(), Kind: "Gateway"},
		ObjectMeta: metav1.ObjectMeta{Name: cloud.WebdGatewayName, Namespace: cloud.WebdServiceNamespace},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(r.gatewayClass),
			Listeners:        listeners,
		},
	}
}

// marshalClean marshals a typed object to YAML with the noise a GitOps reader
// does not want: the empty `status` stanza (controller-owned, ignored on apply)
// and null-valued optional fields (e.g. a SecretObjectReference's defaulted
// group/kind). It round-trips through a map and prunes, so the committed
// resource reads cleanly.
func marshalClean(obj any) ([]byte, error) {
	raw, err := yaml.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	delete(m, "status")
	pruneNulls(m)
	return yaml.Marshal(m)
}

// pruneNulls recursively deletes nil map values (an unset optional field marshals
// to null; dropping it is equivalent and cleaner).
func pruneNulls(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if val == nil {
				delete(t, k)
				continue
			}
			pruneNulls(val)
		}
	case []any:
		for _, e := range t {
			pruneNulls(e)
		}
	}
}

// buildWebdHTTPRoute mirrors applyWebdHTTPRoute's spec (parentRef → the Gateway,
// one hostname, backendRef → webd:8080). TypeMeta set for kustomize parsing.
func buildWebdHTTPRoute(name, host string) *gatewayv1.HTTPRoute {
	h := gatewayv1.Hostname(host)
	return &gatewayv1.HTTPRoute{
		TypeMeta:   metav1.TypeMeta{APIVersion: gatewayv1.GroupVersion.String(), Kind: "HTTPRoute"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cloud.WebdServiceNamespace},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName(cloud.WebdGatewayName)}},
			},
			Hostnames: []gatewayv1.Hostname{h},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: gatewayv1.ObjectName(cloud.WebdServiceName),
							Port: ptr(gatewayv1.PortNumber(cloud.WebdServicePort)),
						},
					},
				}},
			}},
		},
	}
}
