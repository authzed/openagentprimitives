package certmanager

import (
	"context"
	"embed"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

//go:embed cert-manager.yaml
var certManagerFS embed.FS

// certManagerManifests returns the pinned cert-manager v1.16.2 release manifest
// as a single multi-doc group. Source:
// github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml
func certManagerManifests() ([][]byte, error) {
	b, err := certManagerFS.ReadFile("cert-manager.yaml")
	if err != nil {
		return nil, fmt.Errorf("read embedded cert-manager manifest: %w", err)
	}
	return [][]byte{b}, nil
}

var clusterIssuerGVR = schema.GroupVersionResource{
	Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers",
}

// hasCRD reports whether the cluster serves the given CRD (group/resource), used
// to detect cert-manager presence.
func hasCRD(ctx context.Context, cl cloud.Clients, gvr schema.GroupVersionResource) (bool, error) {
	_, err := cl.Dynamic.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
	switch {
	case err == nil:
		return true, nil
	case cloud.IsNoMatchError(err):
		return false, nil
	default:
		return false, err
	}
}

// CertManagerComponent returns a cloud.Component descriptor for cert-manager,
// suitable for passing to cloud.EnsureComponent. It detects presence via the
// ClusterIssuer CRD and installs the embedded v1.16.2 bundle.
func CertManagerComponent(cl cloud.Clients) cloud.Component {
	return cloud.Component{
		Name:      "cert-manager",
		Why:       "issues the TLS certificates for webd's hostnames via Let's Encrypt",
		Creates:   "namespace cert-manager, cert-manager CRDs, and the controller/webhook/cainjector Deployments",
		ManualCmd: "kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml",
		Present: func(ctx context.Context) (bool, error) {
			return hasCRD(ctx, cl, clusterIssuerGVR)
		},
		Manifests: certManagerManifests,
		Ready: func(ctx context.Context) error {
			// the webhook must be Ready before a ClusterIssuer can be applied.
			for _, d := range []string{"cert-manager", "cert-manager-webhook", "cert-manager-cainjector"} {
				if err := cloud.WaitForDeployment(ctx, cl, "cert-manager", d, 1); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
