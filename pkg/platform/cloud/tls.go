package cloud

import (
	"context"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// DNSRecord is one DNS record the operator must create for the TLS flow to
// validate — printed by the consumer alongside the Gateway's A records. The
// Google-managed strategy returns the Certificate Manager DNS-authorization
// CNAME(s) here; cert-manager (HTTP-01) needs none.
type DNSRecord struct {
	Name string // record name (host), e.g. "_acme-challenge.webd.example.com."
	Type string // record type, e.g. "CNAME"
	Data string // record value, e.g. "abc123.authorize.certificatemanager.goog."
}

// ListenerTLS is the per-Gateway-listener TLS configuration a TLSStrategy
// hands back from Prepare so the consumer can stamp it onto the synthesized
// Gateway's HTTPS listeners. It abstracts over the two terminate-mode shapes:
//   - cert-manager: a TLS config that references a per-hostname Secret
//     (SecretRef set; the listener's certificateRefs points at it).
//   - google-managed: no per-listener Secret — the cert lives in a Certificate
//     Manager map attached to the Gateway via an annotation (see
//     GatewayTLS.Annotations); GatewayTLSConfig is left nil so the listener
//     carries no certificateRefs.
type ListenerTLS struct {
	// UsesListenerSecrets, when true, means each HTTPS listener has a Secret
	// backing its cert (cert-manager). When false, HTTPS listeners carry no
	// TLS certificateRefs (google-managed: cert map attached by annotation).
	UsesListenerSecrets bool

	// SecretFor returns the TLS Secret name backing the listener for host, or
	// "" if this strategy uses no per-listener Secret. Only consulted when
	// UsesListenerSecrets is true.
	SecretFor func(host string) string
}

// GatewayTLS is everything a TLSStrategy contributes to the Gateway object
// itself: the per-listener TLS config (Listener) plus any Gateway-level
// annotations the consumer must stamp on (google-managed attaches its
// Certificate Manager map via the networking.gke.io/certmap annotation).
type GatewayTLS struct {
	Listener    ListenerTLS
	Annotations map[string]string
}

// PrepareParams carries the inputs a TLSStrategy needs to provision its cert
// machinery before the Gateway is applied.
type PrepareParams struct {
	// Reporter is the user-facing output sink; strategies write Step/OK/Info/Warn
	// through it. Replaces the old io.Writer + cliout coupling.
	Reporter Reporter
	// Clients is the cluster client bundle (typed/dynamic/controller-runtime).
	// Replaces the old *kube.Bundle coupling.
	Clients Clients
	// Cloud is the detected cloud string (e.g. "gke"); informational.
	Cloud string
	// TrustedHostname is webd's trusted-origin host (always set).
	TrustedHostname string
	// SandboxHostname is the artifact-viewer host, or "" when the viewer is
	// disabled.
	SandboxHostname string
	// Project is the GCP project derived from the node providerID (google-managed
	// only); "" for other clouds.
	Project string
	// ACMEEmail is the Let's Encrypt ACME account email (cert-manager only).
	ACMEEmail string
	// TLSIssuer, when non-empty, names an existing cert-manager ClusterIssuer to
	// use instead of creating a Let's Encrypt one (cert-manager only).
	TLSIssuer string
	// AssumeYes accepts offered component installs (cert-manager) non-interactively.
	AssumeYes bool
}

// CompleteParams carries the inputs a TLSStrategy needs AFTER the Gateway exists.
type CompleteParams struct {
	// Reporter is the user-facing output sink.
	Reporter Reporter
	// Clients is the cluster client bundle.
	Clients         Clients
	TrustedHostname string
	SandboxHostname string
	// TLSIssuer mirrors PrepareParams.TLSIssuer so the cert-manager strategy
	// resolves the SAME effective ClusterIssuer in Complete that it set up in
	// Prepare (empty ⇒ the Let's Encrypt issuer oap created).
	TLSIssuer string
}

// TLSStrategy provisions TLS for webd's Gateway listeners. The two-phase shape
// mirrors the chicken-and-egg between the Gateway and its certs:
//
//   - Prepare runs BEFORE the Gateway is applied. It provisions whatever cert
//     machinery must exist for the Gateway to come up (cert-manager: the issuer;
//     google-managed: the Certificate Manager dns-authorization/cert/map), and
//     returns the TLS config + annotations the consumer stamps on the Gateway,
//     plus any extra DNS records to print.
//   - Complete runs AFTER the Gateway exists. cert-manager applies the
//     per-hostname Certificates here (they fill the listener Secrets the Gateway
//     references); google-managed is a no-op (the cert is already attached by
//     annotation).
type TLSStrategy interface {
	// Name is a stable identifier for the strategy (e.g. "cert-manager",
	// "google-managed").
	Name() string

	// Prepare provisions pre-Gateway cert machinery and returns the Gateway TLS
	// config + annotations to stamp on, plus extra DNS records to print. proceed
	// is false when a required component was declined and the caller should skip
	// external-access setup (a warning has already been surfaced via p.Reporter).
	Prepare(ctx context.Context, p PrepareParams) (gw GatewayTLS, extraDNS []DNSRecord, proceed bool, err error)

	// Complete runs post-Gateway cert finalization (cert-manager Certificates);
	// a no-op for strategies whose cert is attached during Prepare.
	Complete(ctx context.Context, p CompleteParams) error
}

// HTTPSListenerTLS builds the gatewayv1.GatewayTLSConfig for an HTTPS listener
// given a strategy's ListenerTLS and the listener's hostname. It returns nil
// when the strategy uses no per-listener Secret (google-managed), so the
// listener carries no certificateRefs. Centralizing this here keeps the
// Secret-ref shape in one place and out of the consumer.
func HTTPSListenerTLS(lt ListenerTLS, host string) *gatewayv1.GatewayTLSConfig {
	if !lt.UsesListenerSecrets || lt.SecretFor == nil {
		return nil
	}
	secret := lt.SecretFor(host)
	if secret == "" {
		return nil
	}
	mode := gatewayv1.TLSModeTerminate
	return &gatewayv1.GatewayTLSConfig{
		Mode: &mode,
		CertificateRefs: []gatewayv1.SecretObjectReference{
			{Name: gatewayv1.ObjectName(secret)},
		},
	}
}
