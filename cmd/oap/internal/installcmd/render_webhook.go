package installcmd

// Webhook serving-cert provisioning for the GitOps path, via cert-manager.
//
// The operator's webhook server mounts spicebox-webhook-tls as a non-optional
// volume, so the Secret must exist before the operator pod can start — it cannot
// be minted by the pre-start Job (a same-cluster Job can't get the cert into the
// operator pod's mount before the pod is created) nor by the operator itself.
// cert-manager solves both halves declaratively: a self-signed Issuer + a
// Certificate populate the Secret, and the ca-injector copies the CA into the
// ValidatingWebhookConfiguration's caBundle via the inject-ca-from annotation —
// no ordering races, continuously reconciled.
//
// This makes cert-manager a hard dependency of the GitOps path. A user who does
// not run it passes --external-webhook-tls and provides spicebox-webhook-tls +
// the VWC caBundle themselves.
const (
	webhookCertResourceFile  = "webhook-cert.yaml"
	webhookCAInjectPatchFile = "patches/webhook-ca-inject.yaml"

	// webhookCertName is the cert-manager Certificate name; the VWC's
	// inject-ca-from annotation references it as <namespace>/<name>.
	webhookCertName = "spicebox-webhook-tls"
	webhookVWCName  = "spicebox-settings"
)

// renderWebhookCertResources returns the self-signed Issuer + Certificate that
// populate the spicebox-webhook-tls Secret with a cert whose SANs cover the
// spicebox-webhook Service. A pure constant — no cfg dependence.
func renderWebhookCertResources() []byte {
	return []byte("apiVersion: cert-manager.io/v1\n" +
		"kind: Issuer\n" +
		"metadata:\n" +
		"  name: spicebox-webhook-selfsign\n" +
		"  namespace: agentprimitives-system\n" +
		"spec:\n" +
		"  selfSigned: {}\n" +
		"---\n" +
		"apiVersion: cert-manager.io/v1\n" +
		"kind: Certificate\n" +
		"metadata:\n" +
		"  name: " + webhookCertName + "\n" +
		"  namespace: agentprimitives-system\n" +
		"spec:\n" +
		"  secretName: " + webhookCertName + "\n" +
		"  dnsNames:\n" +
		"    - spicebox-webhook.agentprimitives-system.svc\n" +
		"    - spicebox-webhook.agentprimitives-system.svc.cluster.local\n" +
		"  issuerRef:\n" +
		"    name: spicebox-webhook-selfsign\n" +
		"    kind: Issuer\n")
}

// webhookCAInjectPatch annotates the ValidatingWebhookConfiguration so
// cert-manager's ca-injector writes the Certificate's CA into every webhook's
// caBundle.
func webhookCAInjectPatch() []byte {
	return []byte("apiVersion: admissionregistration.k8s.io/v1\n" +
		"kind: ValidatingWebhookConfiguration\n" +
		"metadata:\n" +
		"  name: " + webhookVWCName + "\n" +
		"  annotations:\n" +
		"    cert-manager.io/inject-ca-from: agentprimitives-system/" + webhookCertName + "\n")
}
