package v1alpha1

// DirectCredentialsSource declares a website login the browser toolbox fills
// at use time, never resolved into an injectable env var or header.
//
// Deliberately defined in its own file rather than inside agentidentity_types.go
// (where AgentCredential's union lives): its Source field carries its OWN
// +kubebuilder:validation:Enum marker (onepassword;secret), and
// pkg/platform/identity/credkind/enum_invariant_test.go's
// TestEveryCRDEnumValueHasARegisteredKind scans every Enum marker found
// anywhere in agentidentity_types.go and credential_descriptor.go, asserting
// each VALUE is a registered credkind.Kind's Type(). "onepassword" and
// "secret" are directcred.Source kinds (pkg/platform/identity/directcred),
// not credkind.Kind types, so landing this marker in either scanned file
// would fail that test. See pkg/platform/identity/credkind/directcredentials's
// doc comment for why Resolve never reads either arm.
type DirectCredentialsSource struct {
	// Source names the login backend: onepassword resolves through a
	// 1Password Connect token; secret reads a host-keyed login Secret
	// written directly.
	// +kubebuilder:validation:Enum=onepassword;secret
	Source string `json:"source"`

	// SecretRef names the source's token Secret (key "token", onepassword)
	// or the host-keyed login Secret (secret).
	SecretRef DirectCredentialSecretRef `json:"secretRef"`

	// Vault is the 1Password vault NAME; required iff source is onepassword.
	// +optional
	Vault string `json:"vault,omitempty"`
}

// DirectCredentialSecretRef names a directCredentials block's backing
// Secret. Unlike SecretKeyRef (agentidentity_types.go), Key is OPTIONAL: the
// "secret" backend's login Secret is a multi-key shape with no single named
// value key, so pkg/platform/identity/credkind/directcredentials's
// ValidateSpec requires Key be EMPTY for that source — an omitted key in the
// user's YAML must validate, not trip the apiserver's structural `required`
// first.
//
// This is a dedicated type rather than making SecretKeyRef.Key optional at
// the shared-type level: SecretKeyRef is also embedded by
// StaticCredentialSource, ClusterSkillSource's auth.secretRef, and
// AgentClass/Settings' model apiKey, none of which have a ValidateSpec (or
// any other pre-apply check) enforcing key presence on their own — the
// apiserver's structural marker is the ONLY thing stopping a keyless ref
// there today. Loosening the shared type would have silently deferred their
// failures from an immediate apply-time rejection to an async reconcile-time
// status condition. See pkg/platform/manifests's
// TestSecretKeyRefStaysRequiredElsewhere for the pinned regression.
type DirectCredentialSecretRef struct {
	// Name is the Secret's name, in the referring object's own namespace.
	Name string `json:"name"`
	// Key is the data key within the Secret holding the value. Optional:
	// "token" (or omitted) for source=onepassword; must be omitted for
	// source=secret, whose login Secret is a multi-key shape with no single
	// named value key. ValidateSpec enforces the exact rule per source; this
	// marker only ensures the apiserver never rejects an omitted key before
	// ValidateSpec gets to run.
	// +optional
	Key string `json:"key,omitempty"`
}
