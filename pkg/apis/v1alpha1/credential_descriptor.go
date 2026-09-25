package v1alpha1

// CredentialDescriptor is a self-contained, resolvable credential need:
// a reference to the backing Secret plus how to inject the resolved
// token. It carries references only — never token bytes. It is the unit
// the token broker resolves and the element type of
// ToolCall.spec.credentials.
type CredentialDescriptor struct {
	// Source locates the credential's backing Secret.
	Source CredentialSource `json:"source"`
	// Inject describes how to project the resolved token.
	Inject CredentialInjection `json:"inject"`
}

// CredentialSource references one credential's backing Secret. Type
// drives the read + expiry rules: "static" reads Key; "oauth" reads the
// fixed multi-key OAuth shape and gates on expires_at.
type CredentialSource struct {
	// Type drives the read and expiry rules; see the type doc.
	// +kubebuilder:validation:Enum=static;oauth;federated;githubApp
	Type string `json:"type"`
	// Namespace is the Secret's namespace; must equal the ToolCall's own, which
	// ToolCall.ValidateCredentialSourceNamespaces enforces.
	Namespace string `json:"namespace"`
	// Name is the Secret name. For type=federated it is the IdP-identity
	// Secret (the subject token source), NOT the upstream token.
	Name string `json:"name"`
	// Key is the Secret data key for type=static; ignored otherwise.
	// +optional
	Key string `json:"key,omitempty"`
	// Resource / ResourceServerURL / Scopes are set only for
	// type=federated and drive the ID-JAG mint.
	// +optional
	Resource string `json:"resource,omitempty"`
	// +optional
	ResourceServerURL string `json:"resourceServerURL,omitempty"`
	// +optional
	Scopes []string `json:"scopes,omitempty"`
}

// CredentialInjection describes how to project a resolved token.
// Exactly one of EnvVar / Header is set.
type CredentialInjection struct {
	// EnvVar projects the token into this environment variable
	// (CLI / toolspec tools).
	// +optional
	EnvVar string `json:"envVar,omitempty"`
	// Header projects the token into this HTTP header (MCP tools).
	// +optional
	Header *HeaderInjection `json:"header,omitempty"`
}

type HeaderInjection struct {
	// Name is the HTTP header the token is written to.
	Name string `json:"name"`
	// ValuePrefix is prepended to the token in the header value (e.g. "Bearer ").
	// +optional
	ValuePrefix string `json:"valuePrefix,omitempty"`
}
