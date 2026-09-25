package cloud

// Kind keys. These are the wire values: they appear in --cluster-kind, in the
// AP_CLUSTER_KIND env stamped onto the operator and webd Deployments, and in
// the registry map. Changing one breaks already-installed clusters, whose
// Deployments carry the old value until re-installed.
//
// Consumers spell kinds with these constants, never as bare string literals,
// so a typo is a compile error rather than a runtime "unknown cluster kind".
const (
	// KeyLocal is the lightweight developer profile: sqlite memory, an
	// in-memory SpiceDB datastore, a file:// artifact PVC, local :dev images.
	// It is opt-in ONLY (--local, the public-tunnel dev flow) and is never
	// returned by Detect. It does NOT serve the built-in web chat: --local
	// opens a public ngrok tunnel, and the chat plugin is single-user by
	// design with no multi-tenant authorization story. See KeyDesktop for the
	// one place that chat is safe to serve.
	KeyLocal = "local"

	// KeyDesktop is `oap desktop`'s kind: local in every respect (same dev
	// profile facts, same Validate/TLS/storage/Gateway behavior) except it
	// DOES serve the built-in web chat — the desktop VM is a network-confined,
	// single-user environment (loopback port-forward only, no public
	// ingress), unlike `local`'s public ngrok tunnel. It is opt-in ONLY (`oap
	// desktop` is the sole caller that selects it) and is never returned by
	// Detect.
	KeyDesktop = "desktop"

	// KeyDefault is the fallback kind for any cluster with no recognized
	// providerID: on-prem, bare-metal, kind, undetectable. It carries the
	// production profile.
	KeyDefault = "default"

	KeyGKE = "gke"
	KeyEKS = "eks"
	KeyAKS = "aks"
)
