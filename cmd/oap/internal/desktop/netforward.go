package desktop

import (
	"fmt"

	"k8s.io/client-go/tools/clientcmd"
)

// RewriteKubeconfigServer rewrites every cluster server URL in the k3s
// kubeconfig to point at the guest's NAT IP (k3s emits 127.0.0.1).
func RewriteKubeconfigServer(raw []byte, guestIP string) ([]byte, error) {
	if guestIP == "" {
		return nil, fmt.Errorf("desktop: empty guest IP")
	}
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("desktop: parse kubeconfig: %w", err)
	}
	for _, c := range cfg.Clusters {
		c.Server = fmt.Sprintf("https://%s:6443", guestIP)
		// k3s' server cert is for 127.0.0.1/localhost/internal IPs; the
		// emitted CA already covers the cluster. If TLS SAN errors occur,
		// k3s --tls-san <guestIP> must be set in k3s-config.yaml (Task 2.1).
	}
	return clientcmd.Write(*cfg)
}
