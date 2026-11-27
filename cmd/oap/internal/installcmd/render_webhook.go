package installcmd

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

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
)

// webhookVWCNames are the operator's ValidatingWebhookConfigurations, all served
// by the spicebox-webhook-tls cert. `oap install` patches each one's caBundle
// (patchWebhookVWCBundle); render annotates each for the ca-injector. One list,
// so a new VWC cannot be covered by one path and missed by the other.
var webhookVWCNames = []string{"spicebox-settings", "spicebox-goal-execution"}

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

// webhookCAInjectPatch annotates every ValidatingWebhookConfiguration so
// cert-manager's ca-injector writes the Certificate's CA into each webhook's
// caBundle.
func webhookCAInjectPatch() []byte {
	docs := make([]string, 0, len(webhookVWCNames))
	for _, name := range webhookVWCNames {
		docs = append(docs, "apiVersion: admissionregistration.k8s.io/v1\n"+
			"kind: ValidatingWebhookConfiguration\n"+
			"metadata:\n"+
			"  name: "+name+"\n"+
			"  annotations:\n"+
			"    cert-manager.io/inject-ca-from: agentprimitives-system/"+webhookCertName+"\n")
	}
	return []byte(strings.Join(docs, "---\n"))
}

// webhookCABundleRemovals returns the kustomization `patches:` entries that
// remove every literal `caBundle: ""` from the base VWCs. The ca-injector owns
// that field; left in the desired manifest, Argo self-heal or Flux's server-side
// apply would reset it to empty and fight the injector, failing admission TLS in
// between. Indices are read from the base itself, so a webhook added to
// config/manager/webhook.yaml is covered without touching this file.
func webhookCABundleRemovals() (string, error) {
	docs, err := manifests.Split(manifests.Install)
	if err != nil {
		return "", fmt.Errorf("parse base bundle: %w", err)
	}
	var out strings.Builder
	for _, name := range webhookVWCNames {
		var ops []string
		found := false
		for _, d := range docs {
			if d.GetKind() != "ValidatingWebhookConfiguration" || d.GetName() != name {
				continue
			}
			found = true
			hooks, _, _ := unstructured.NestedSlice(d.Object, "webhooks")
			for i, h := range hooks {
				hm, ok := h.(map[string]any)
				if !ok {
					return "", fmt.Errorf("ValidatingWebhookConfiguration %s webhook %d is not a map", name, i)
				}
				if _, has, _ := unstructured.NestedFieldNoCopy(hm, "clientConfig", "caBundle"); has {
					ops = append(ops, fmt.Sprintf("      - op: remove\n        path: /webhooks/%d/clientConfig/caBundle\n", i))
				}
			}
		}
		if !found {
			return "", fmt.Errorf("ValidatingWebhookConfiguration %s is not in the base bundle (webhookVWCNames is stale)", name)
		}
		if len(ops) == 0 {
			continue
		}
		out.WriteString("  - target:\n" +
			"      kind: ValidatingWebhookConfiguration\n" +
			"      name: " + name + "\n" +
			"    patch: |-\n" + strings.TrimSuffix(strings.Join(ops, ""), "\n") + "\n")
	}
	return out.String(), nil
}
