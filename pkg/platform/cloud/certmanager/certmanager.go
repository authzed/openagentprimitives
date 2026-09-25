// Package certmanager implements the cert-manager + Let's Encrypt TLS strategy
// for webd's external-access Gateway. It is the DEFAULT strategy, used on every
// cloud where no other strategy explicitly claims TLS (EKS, AKS, bare clusters
// with an Envoy-Gateway-behind-an-L4-LB setup where HTTP-01 is reachable).
//
// Prepare ensures cert-manager is installed, turns on its Gateway API
// integration, creates the Let's Encrypt ClusterIssuer (unless --tls-issuer
// names an existing one), and returns the per-listener Secret-ref TLS config;
// HTTP-01 needs no extra DNS records. Complete applies the Certificates that
// populate those Secrets, using an issue-temporary-certificate annotation to
// break the program-vs-issue deadlock.
//
// Wire it by returning Strategy{} from a cloud.Strategy's TLS() method — there
// is no init() Register call.
package certmanager

import (
	"context"
	"fmt"
	"os"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// Strategy implements cloud.TLSStrategy using cert-manager + Let's Encrypt
// (HTTP-01 through the Gateway). Name() returns "cert-manager".
type Strategy struct{}

// Name returns the stable identifier for this strategy.
func (Strategy) Name() string { return "cert-manager" }

// effectiveIssuer resolves the ClusterIssuer name: an explicit --tls-issuer
// wins, otherwise the Let's Encrypt issuer oap creates. Both Prepare and Complete
// call this to agree on the issuer without threading an extra value.
func (Strategy) effectiveIssuer(tlsIssuer string) string {
	if tlsIssuer != "" {
		return tlsIssuer
	}
	return cloud.WebdLetsEncryptIssuerName
}

// Prepare ensures cert-manager + the Gateway API integration, creates the Let's
// Encrypt ClusterIssuer (unless --tls-issuer was given), and returns the
// per-listener Secret-ref TLS config. HTTP-01 needs no extra DNS records.
// Returns proceed=false (with a warning via p.Reporter) if cert-manager was
// declined; the caller should skip external-access setup in that case.
func (s Strategy) Prepare(ctx context.Context, p cloud.PrepareParams) (cloud.GatewayTLS, []cloud.DNSRecord, bool, error) {
	if p.TLSIssuer == "" {
		if p.ACMEEmail == "" {
			return cloud.GatewayTLS{}, nil, false,
				fmt.Errorf("--acme-email is required with --trusted-hostname (or pass --tls-issuer to use an existing ClusterIssuer)")
		}
		present, err := cloud.EnsureComponent(ctx, p.Reporter, os.Stdin, p.Clients, CertManagerComponent(p.Clients), p.AssumeYes)
		if err != nil {
			return cloud.GatewayTLS{}, nil, false, err
		}
		if !present {
			p.Reporter.Warn("cert-manager not installed; skipping the TLS issuer + webd external access. Run the manual install command printed above, then re-run `oap init`.")
			return cloud.GatewayTLS{}, nil, false, nil
		}
		// The LE ClusterIssuer uses an HTTP-01 gatewayHTTPRoute solver to present
		// challenges through the existing Gateway; that needs cert-manager's Gateway
		// API integration turned on, or the challenge fails "gateway api is not
		// enabled" and the cert never issues.
		if err := cloud.EnsureCertManagerGatewayAPI(ctx, p.Clients, p.Reporter); err != nil {
			return cloud.GatewayTLS{}, nil, false, err
		}
		if err := cloud.ApplyLetsEncryptClusterIssuer(
			ctx, p.Clients,
			cloud.WebdLetsEncryptIssuerName, p.ACMEEmail,
			cloud.WebdGatewayName, cloud.WebdServiceNamespace,
		); err != nil {
			return cloud.GatewayTLS{}, nil, false, fmt.Errorf("create Let's Encrypt ClusterIssuer: %w", err)
		}
	}

	// Build the per-listener Secret mapping: the sandbox listener uses the sandbox
	// Secret, every other (trusted) listener the trusted Secret. The consumer never
	// builds an HTTPS listener for an empty host, so a non-empty host is always one
	// of the two webd origins.
	sandboxHost := p.SandboxHostname
	gw := cloud.GatewayTLS{
		Listener: cloud.ListenerTLS{
			UsesListenerSecrets: true,
			SecretFor: func(host string) string {
				if host == "" {
					return ""
				}
				if sandboxHost != "" && host == sandboxHost {
					return cloud.WebdSandboxCertSecretName
				}
				return cloud.WebdTrustedCertSecretName
			},
		},
	}
	return gw, nil, true, nil
}

// Complete applies the explicit cert-manager Certificates that populate the
// per-hostname listener Secrets (the issue-temporary-certificate annotation lets
// the LB program before ACME completes).
func (s Strategy) Complete(ctx context.Context, p cloud.CompleteParams) error {
	issuer := s.effectiveIssuer(p.TLSIssuer)
	if err := cloud.ApplyWebdCertificate(
		ctx, p.Clients,
		cloud.WebdServiceNamespace, cloud.WebdTrustedCertSecretName,
		p.TrustedHostname, issuer,
	); err != nil {
		return fmt.Errorf("apply webd trusted Certificate: %w", err)
	}
	if p.SandboxHostname != "" {
		if err := cloud.ApplyWebdCertificate(
			ctx, p.Clients,
			cloud.WebdServiceNamespace, cloud.WebdSandboxCertSecretName,
			p.SandboxHostname, issuer,
		); err != nil {
			return fmt.Errorf("apply webd sandbox Certificate: %w", err)
		}
	}
	p.Reporter.Info("  Until DNS resolves, webd serves a temporary self-signed cert; Let's Encrypt issues the real one via HTTP-01 once the records point at the LB.")
	return nil
}
