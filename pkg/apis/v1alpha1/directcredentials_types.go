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
	SecretRef SecretKeyRef `json:"secretRef"`

	// Vault is the 1Password vault NAME; required iff source is onepassword.
	// +optional
	Vault string `json:"vault,omitempty"`
}
