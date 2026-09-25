// Package gke provides GKE-specific implementations of pkg/platform/cloud strategies.
package gke

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// GoogleManagedTLS provisions webd's TLS via Google-managed certificates
// (Certificate Manager) and attaches them to the Gateway with the
// networking.gke.io/certmap annotation.
//
// Why GKE differs: on GKE's managed L7 Gateway (gke-l7-global-external-managed)
// the cert-manager HTTP-01 solver fights GKE's backend health check (503, the
// cert never issues). Google-managed certs sidestep ACME entirely — Certificate
// Manager validates via a DNS authorization (a CNAME the operator creates) and
// the managed Gateway controller serves the cert from the attached certmap. So
// on GKE there is NO cert-manager, NO Let's Encrypt, and NO Certificate
// resources.
//
// The flow shells out to `gcloud certificate-manager …` and is fully
// idempotent: every "create" treats an AlreadyExists as success.
type GoogleManagedTLS struct{}

// googleManagedCertMap is the Certificate Manager map name the Gateway points at
// via the networking.gke.io/certmap annotation. One map holds an entry per webd
// hostname.
const googleManagedCertMap = "spicebox-webd"

// gkeCertMapAnnotation is the Gateway annotation GKE's managed controller reads
// to attach a Certificate Manager map to the Gateway's HTTPS listeners.
const gkeCertMapAnnotation = "networking.gke.io/certmap"

// certificateManagerAPI is the Google service that must be enabled on the project
// for Certificate Manager (Google-managed certs) to work.
const certificateManagerAPI = "certificatemanager.googleapis.com"

// Name returns the stable identifier for this strategy.
func (GoogleManagedTLS) Name() string { return "google-managed" }

// Prepare provisions, per hostname, a Certificate Manager DNS authorization +
// Google-managed certificate, then a single map with an entry per hostname, and
// returns the certmap Gateway annotation plus the DNS-authorization CNAME(s) to
// print. The listeners carry no per-listener Secret (the managed controller
// serves the cert from the map), so ListenerTLS.UsesListenerSecrets is false.
func (s GoogleManagedTLS) Prepare(ctx context.Context, p cloud.PrepareParams) (cloud.GatewayTLS, []cloud.DNSRecord, bool, error) {
	if p.Project == "" {
		return cloud.GatewayTLS{}, nil, false,
			fmt.Errorf("could not derive the GCP project for Google-managed certificates from the cluster's node providerID — is this a GKE cluster with running nodes?")
	}

	// Google-managed certs need the Certificate Manager API enabled on the
	// project. Offer to enable it if it isn't (every gcloud call below fails
	// otherwise). Declining is a graceful skip — like declining cert-manager on
	// the default strategy — not a hard failure: the core install stands and
	// external access waits for a re-run. This lives in the GKE strategy, not a
	// cloud branch in the consumer.
	proceed, err := ensureCertificateManagerAPI(ctx, p.Reporter, p.Project, p.AssumeYes)
	if err != nil {
		return cloud.GatewayTLS{}, nil, false, err
	}
	if !proceed {
		return cloud.GatewayTLS{}, nil, false, nil
	}

	hosts := []string{p.TrustedHostname}
	if p.SandboxHostname != "" {
		hosts = append(hosts, p.SandboxHostname)
	}

	p.Reporter.Step("provision Google-managed certificates (Certificate Manager)")

	// 1. Per-host: DNS authorization + managed certificate. Collect the
	//    authorization CNAMEs to print.
	var dns []cloud.DNSRecord
	for _, host := range hosts {
		auth := certManagerAuthName(host)
		cert := certManagerCertName(host)

		if err := gcloudCertManagerCreate(ctx, p.Reporter, p.Project,
			"dns-authorizations", auth, "create dns-authorization for "+host,
			"--domain="+host); err != nil {
			return cloud.GatewayTLS{}, nil, false, err
		}
		rec, err := describeDNSAuthorization(ctx, p.Reporter, p.Project, auth)
		if err != nil {
			return cloud.GatewayTLS{}, nil, false, err
		}
		dns = append(dns, rec)

		if err := gcloudCertManagerCreate(ctx, p.Reporter, p.Project,
			"certificates", cert, "create managed certificate for "+host,
			"--domains="+host, "--dns-authorizations="+auth); err != nil {
			return cloud.GatewayTLS{}, nil, false, err
		}
	}

	// 2. One map, with an entry per host pointing at that host's certificate.
	if err := gcloudCertManagerCreate(ctx, p.Reporter, p.Project,
		"maps", googleManagedCertMap, "create certificate map"); err != nil {
		return cloud.GatewayTLS{}, nil, false, err
	}
	for _, host := range hosts {
		entry := certManagerEntryName(host)
		cert := certManagerCertName(host)
		if err := gcloudCertManagerCreate(ctx, p.Reporter, p.Project,
			"maps entries", entry, "create certificate map entry for "+host,
			"--map="+googleManagedCertMap, "--certificates="+cert, "--hostname="+host); err != nil {
			return cloud.GatewayTLS{}, nil, false, err
		}
	}

	p.Reporter.Info("  Google-managed certificate map %q ready; the Gateway will attach it via %s", googleManagedCertMap, gkeCertMapAnnotation)

	gw := cloud.GatewayTLS{
		Listener: cloud.ListenerTLS{
			// No per-listener Secret: the managed Gateway controller serves the
			// cert from the attached certmap, so HTTPS listeners carry no
			// certificateRefs.
			UsesListenerSecrets: false,
		},
		Annotations: map[string]string{gkeCertMapAnnotation: googleManagedCertMap},
	}
	return gw, dns, true, nil
}

// Complete is a no-op for Google-managed TLS: the cert is already attached to the
// Gateway via the certmap annotation in Prepare; there are no post-Gateway
// Certificate resources to apply.
func (GoogleManagedTLS) Complete(_ context.Context, _ cloud.CompleteParams) error {
	return nil
}

// ensureCertificateManagerAPI makes sure the Certificate Manager API is enabled on
// the project, offering to enable it if it is not (the dns-authorization /
// certificate / map gcloud calls all fail otherwise). proceed is true once the API
// is on — already, or just enabled. On a decline (an interactive "no", or a
// non-interactive run without --yes) it prints the exact enable command and
// returns proceed=false so the caller skips external access — a surfaced, graceful
// skip, never a silent one — mirroring how the cert-manager strategy treats a
// declined cert-manager install.
func ensureCertificateManagerAPI(ctx context.Context, rep cloud.Reporter, project string, assumeYes bool) (bool, error) {
	enabled, err := serviceEnabled(ctx, rep, project, certificateManagerAPI)
	if err != nil {
		return false, fmt.Errorf("check whether the Certificate Manager API is enabled: %w", err)
	}
	if enabled {
		return true, nil
	}
	rep.Warn("the Certificate Manager API (%s) is not enabled on project %s — Google-managed certificates require it", certificateManagerAPI, project)
	if !cloud.Confirm(os.Stdin, rep, fmt.Sprintf("Enable %s on project %s now?", certificateManagerAPI, project), assumeYes) {
		rep.Warn("skipping webd external access; enable it with `gcloud services enable %s --project=%s` and re-run `oap init`", certificateManagerAPI, project)
		return false, nil
	}
	rep.Step("enable the Certificate Manager API")
	if err := gcloudEnableService(ctx, rep, project, certificateManagerAPI); err != nil {
		return false, err
	}
	return true, nil
}

// serviceEnabled reports whether the given Google service API is enabled on the
// project (gcloud services list --enabled, filtered to that service).
func serviceEnabled(ctx context.Context, rep cloud.Reporter, project, service string) (bool, error) {
	out, err := cloud.Gcloud(ctx, rep,
		"services", "list", "--enabled",
		"--project="+project,
		"--filter=config.name="+service,
		"--format=value(config.name)")
	if err != nil {
		return false, fmt.Errorf("gcloud services list: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}

// gcloudEnableService enables the given Google service API on the project. Like
// the Gateway API enable, `gcloud services enable` is a server-side operation
// that emits its own progress and can take tens of seconds, so it streams live
// (GcloudStreaming) rather than buffering — otherwise the operator sees a silent
// hang.
func gcloudEnableService(ctx context.Context, rep cloud.Reporter, project, service string) error {
	if err := cloud.GcloudStreaming(ctx, rep, "services", "enable", service, "--project="+project); err != nil {
		return fmt.Errorf("enable %s (gcloud services enable): %w", service, err)
	}
	return nil
}

// gcloudCertManagerCreate runs `gcloud certificate-manager <group> create <name>
// --project=<project> [args…]` idempotently: an AlreadyExists on the create is
// treated as success (the resource already exists from a prior install), like
// the registry setup's repo-create. group may contain a space ("maps entries"),
// which is split into separate gcloud args. desc is a short human label used in
// the wrapped error.
func gcloudCertManagerCreate(ctx context.Context, rep cloud.Reporter, project string, group, name, desc string, extraArgs ...string) error {
	args := []string{"certificate-manager"}
	args = append(args, strings.Fields(group)...)
	args = append(args, "create", name, "--project="+project)
	args = append(args, extraArgs...)

	_, err := cloud.Gcloud(ctx, rep, args...)
	if err != nil {
		if cloud.IsGcloudAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("%s (gcloud %s): %w", desc, strings.Join(args, " "), err)
	}
	return nil
}

// describeDNSAuthorization reads back the dns-authorization's required DNS record
// (name, type, data) so the caller can print the exact CNAME to create. The
// authorization stays pending until that CNAME resolves; the managed certificate
// then validates and issues.
func describeDNSAuthorization(ctx context.Context, rep cloud.Reporter, project, auth string) (cloud.DNSRecord, error) {
	out, err := cloud.Gcloud(ctx, rep,
		"certificate-manager", "dns-authorizations", "describe", auth,
		"--project="+project,
		"--format=value(dnsResourceRecord.name,dnsResourceRecord.type,dnsResourceRecord.data)")
	if err != nil {
		return cloud.DNSRecord{}, fmt.Errorf("describe dns-authorization %q: %w", auth, err)
	}
	// gcloud's value() formatter tab-separates the three fields on one line.
	fields := strings.Split(strings.TrimSpace(out), "\t")
	if len(fields) != 3 || fields[0] == "" {
		return cloud.DNSRecord{}, fmt.Errorf("dns-authorization %q returned an unexpected record %q (want name<TAB>type<TAB>data)", auth, strings.TrimSpace(out))
	}
	return cloud.DNSRecord{Name: fields[0], Type: fields[1], Data: fields[2]}, nil
}

// certManagerAuthName / certManagerCertName / certManagerEntryName derive stable,
// DNS-safe Certificate Manager resource names from a hostname. Certificate
// Manager names must be lowercase, start with a letter, and use only letters,
// digits, and hyphens — so dots in the host become hyphens.
func certManagerAuthName(host string) string {
	return "spicebox-webd-" + sanitizeCMName(host) + "-auth"
}
func certManagerCertName(host string) string {
	return "spicebox-webd-" + sanitizeCMName(host) + "-cert"
}
func certManagerEntryName(host string) string { return "spicebox-webd-" + sanitizeCMName(host) }

// sanitizeCMName lowercases host and replaces every run of non-alphanumeric
// characters with a single hyphen, trimming leading/trailing hyphens, so it is a
// valid Certificate Manager resource-name component.
func sanitizeCMName(host string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(host) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
