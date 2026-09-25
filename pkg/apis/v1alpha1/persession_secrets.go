package v1alpha1

// PerSessionSecretSuffixes is the canonical set of Secret name suffixes for
// the deterministic per-session Secrets the operator/runner materialize —
// each owned by, and living in the namespace of, exactly one AgentSession:
//
//   - PassthroughCredentialSecretSuffix ("-passthrough-creds") — projected
//     userPassthrough credential values (see PassthroughCredentialSecretName).
//   - MemoryTokenSecretSuffix ("-memory-token") — the memory bearer token,
//     the Ed25519 audit-signing seed, and the args-hash HMAC key (see
//     pkg/controllers/agentsession.MemoryTokenSecretName).
//   - SecretOutputSecretSuffix ("-secret-outputs") — captured tool
//     secret-output values (see pkg/web/secretoutsrv.SecretOutputSecretName).
//   - WorkshopTokenSecretSuffix ("-workshop-token") — the agent-builder
//     workshop sidecar bearer token (see WorkshopTokenSecretName).
//
// ToolCall.ValidateCredentialSourceOwnership binds every credential source
// whose Secret name carries one of these suffixes to the ToolCall's own
// AgentSession: a matching suffix under a DIFFERENT session prefix is a
// cross-session reference and is refused. That matters most for
// MemoryTokenSecretSuffix, whose Secret holds the audit-signing seed — naming
// another session's would let a compromised runner forge signed append-only
// entries under the victim's identity and replay its args-hash HMAC key.
// A new per-session Secret kind anywhere in the codebase MUST add its suffix
// here, or ValidateCredentialSourceOwnership silently fails to bind it.
//
// The name-builder functions live in the packages that own each Secret's
// lifecycle, not here: they already import v1alpha1, so importing back would
// cycle. Each builds its name by appending the suffix declared here rather than
// a duplicated literal, and TestPerSessionSecretSuffixesMatchOwners pins that.
var PerSessionSecretSuffixes = []string{
	PassthroughCredentialSecretSuffix,
	MemoryTokenSecretSuffix,
	SecretOutputSecretSuffix,
	WorkshopTokenSecretSuffix,
}

// MemoryTokenSecretSuffix is the suffix of the per-session Secret holding the
// memory bearer token, the Ed25519 audit-signing seed, and the args-hash HMAC
// key. The full name is <sessionName> + this suffix; see
// pkg/controllers/agentsession.MemoryTokenSecretName.
const MemoryTokenSecretSuffix = "-memory-token"

// SecretOutputSecretSuffix is the suffix of the per-session Secret the
// operator writes captured tool secret-output values into. The full name is
// <sessionName> + this suffix; see pkg/web/secretoutsrv.SecretOutputSecretName.
const SecretOutputSecretSuffix = "-secret-outputs"

// WorkshopTokenSecretSuffix is the suffix of the per-session Secret holding
// the agent-builder workshop sidecar's bearer token. The full name is
// <sessionName> + this suffix; see WorkshopTokenSecretName.
const WorkshopTokenSecretSuffix = "-workshop-token"
