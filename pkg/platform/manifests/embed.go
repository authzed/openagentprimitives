// Package manifests embeds the kustomize-rendered operator install YAML.
// Regenerate with: mage manifests
package manifests

import (
	"embed"
	"fmt"
)

//go:embed install.yaml
var Install []byte

// readAll reads each named file from fsys in order, wrapping any read error
// with the file name. The returned slice preserves file order, which is
// load-bearing for callers that apply the manifests in sequence.
func readAll(fsys embed.FS, files ...string) ([][]byte, error) {
	out := make([][]byte, 0, len(files))
	for _, f := range files {
		b, err := fsys.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read embedded %s: %w", f, err)
		}
		out = append(out, b)
	}
	return out, nil
}

//go:embed all:nats
var natsFS embed.FS

// NATS returns the NATS manifest bytes in apply order: ConfigMap, Service, StatefulSet.
func NATS() ([][]byte, error) {
	return readAll(natsFS,
		"nats/configmap.yaml",
		"nats/service.yaml",
		"nats/statefulset.yaml",
	)
}

//go:embed all:channelsd
var channelsdFS embed.FS

// ChannelsD returns the channelsd manifest bytes in apply order:
// ServiceAccount, ClusterRole, ClusterRoleBinding, Deployment.
func ChannelsD() ([][]byte, error) {
	return readAll(channelsdFS,
		"channelsd/serviceaccount.yaml",
		"channelsd/clusterrole.yaml",
		"channelsd/clusterrolebinding.yaml",
		"channelsd/deployment.yaml",
	)
}

//go:embed all:postgres
var postgresFS embed.FS

// Postgres returns the Postgres manifest bytes in apply order: PVC, Service, Deployment.
// Caller must apply the spicebox-postgres-token Secret separately (generated at
// install time, not embedded).
func Postgres() ([][]byte, error) {
	return readAll(postgresFS,
		"postgres/pvc.yaml",
		"postgres/service.yaml",
		"postgres/deployment.yaml",
	)
}

//go:embed all:neo4j
var neo4jFS embed.FS

// Neo4j returns the Neo4j manifest bytes in apply order: Service, StatefulSet.
// Caller must apply the spicebox-neo4j-token Secret separately (generated at
// install time, not embedded).
func Neo4j() ([][]byte, error) {
	return readAll(neo4jFS,
		"neo4j/service.yaml",
		"neo4j/statefulset.yaml",
	)
}

//go:embed all:graphiti
var graphitiFS embed.FS

// Graphiti returns the Graphiti manifest bytes in apply order: Service, Deployment.
// Caller must apply the spicebox-graphiti-config Secret separately (generated at
// install time, not embedded).
func Graphiti() ([][]byte, error) {
	return readAll(graphitiFS,
		"graphiti/service.yaml",
		"graphiti/deployment.yaml",
	)
}

//go:embed all:cert-manager
var certManagerFS embed.FS

// CertManager returns the pinned cert-manager v1.16.2 release manifest as a
// single multi-doc group (applyManifestGroups splits it). Source:
// github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml
func CertManager() ([][]byte, error) {
	return readAll(certManagerFS, "cert-manager/cert-manager.yaml")
}

//go:embed all:envoy-gateway
var envoyGatewayFS embed.FS

// EnvoyGateway returns the pinned Envoy Gateway v1.2.4 release manifest as a
// single multi-doc group (applyManifestGroups splits it). Installs CRDs and the
// envoy-gateway-system controller. Note: this bundle does NOT include a
// GatewayClass instance named "eg" — callers must apply one separately.
// Source: github.com/envoyproxy/gateway/releases/download/v1.2.4/install.yaml
func EnvoyGateway() ([][]byte, error) {
	return readAll(envoyGatewayFS, "envoy-gateway/install.yaml")
}
