package adoptguard

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SecretReader is a guarded reader for Secret objects.
type SecretReader = Guard[*corev1.Secret]

// ConfigMapReader is a guarded reader for ConfigMap objects.
type ConfigMapReader = Guard[*corev1.ConfigMap]

// NewSecretReader wires a guarded Secret reader. cache is the label-filtered
// manager cache (mgr.GetClient()); apiReader is the uncached reader
// (mgr.GetAPIReader()) used for the fixed-infra allowlist branch.
func NewSecretReader(cache, apiReader client.Reader, mode Mode, allowlisted func(types.NamespacedName) bool) *SecretReader {
	return &SecretReader{
		Cache:       cache,
		Reader:      apiReader,
		Mode:        mode,
		Allowlisted: allowlisted,
		New:         func() *corev1.Secret { return &corev1.Secret{} },
	}
}

// NewConfigMapReader wires a guarded ConfigMap reader. cache is the
// label-filtered manager cache (mgr.GetClient()); apiReader is the uncached
// reader (mgr.GetAPIReader()) used for the fixed-infra allowlist branch.
func NewConfigMapReader(cache, apiReader client.Reader, mode Mode, allowlisted func(types.NamespacedName) bool) *ConfigMapReader {
	return &ConfigMapReader{
		Cache:       cache,
		Reader:      apiReader,
		Mode:        mode,
		Allowlisted: allowlisted,
		New:         func() *corev1.ConfigMap { return &corev1.ConfigMap{} },
	}
}

// FixedInfraDefaults is the concrete operator-infra allowlist: the objects the
// operator reads at startup that are never CR-referenced (so never adopted).
// systemNS is the operator's own namespace (POD_NAMESPACE / downward API,
// resolved via operatorNamespace() in internal/cmd/operator/main.go).
//
// Allowlisted objects (all in systemNS):
//   - spicebox-nats-identity  Secret  — NATS account trust material (loadNATSIdentity)
//   - spicebox-nats-tls       Secret  — NATS server CA cert (loadNATSIdentity)
//   - publisher-keys          ConfigMap — component Ed25519 publisher keys (installComponentPublisherKeys)
func FixedInfraDefaults(systemNS string) func(types.NamespacedName) bool {
	return FixedInfra(
		types.NamespacedName{Namespace: systemNS, Name: "spicebox-nats-identity"},
		types.NamespacedName{Namespace: systemNS, Name: "spicebox-nats-tls"},
		types.NamespacedName{Namespace: systemNS, Name: "publisher-keys"},
	)
}
