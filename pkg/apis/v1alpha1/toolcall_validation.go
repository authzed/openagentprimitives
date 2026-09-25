package v1alpha1

import (
	"fmt"
	"strings"
)

// ValidateCredentialSourceNamespaces returns an error if any credential
// source references a Secret outside the ToolCall's own namespace.
//
// The operator resolves these sources via the broker and injects the
// resulting Secret values into the sandbox exec. A source naming a Secret in
// another namespace (e.g. kube-system) would be a confused-deputy
// secret-theft vector — the operator holds cluster-wide secret read, the
// ToolCall does not. Legitimate sources are always in the session's own
// namespace (the runner resolves them from there). Enforced both at admission
// (the ToolCall webhook) and again in the operator before the broker reads
// the Secret (defense-in-depth).
func (tc *ToolCall) ValidateCredentialSourceNamespaces() error {
	for i, c := range tc.Spec.Credentials {
		if c.Source.Namespace != tc.Namespace {
			return fmt.Errorf(
				"credentials[%d].source.namespace %q must equal the ToolCall namespace %q (cross-namespace Secret reference refused)",
				i, c.Source.Namespace, tc.Namespace)
		}
	}
	return nil
}

// ValidateCredentialSourceOwnership returns an error if any credential source
// references another session's per-session Secret.
//
// Every per-session Secret (PerSessionSecretSuffixes) lives in the session's
// own namespace, which channel-attached sessions share, so the namespace check
// alone does not stop a ToolCall owned by session A from naming session B's.
// That matters most for B's -memory-token, which holds the audit-signing seed
// and the args-hash HMAC key: reading it would let a compromised runner forge
// signed append-only entries under B's identity, defeating the tamper-evident
// trust root. ToolCall.spec is stamped by trusted runner code rather than the
// LLM, so this only bites under runner compromise — exactly the confused-deputy
// case this layer defends.
//
// So a source whose name ends in a per-session suffix is bound to THIS
// ToolCall's own AgentSession; names matching no suffix (a legitimate MCPServer
// static credential, say) are unaffected. Enforced at admission and re-checked
// in the operator before the broker resolves anything.
func (tc *ToolCall) ValidateCredentialSourceOwnership() error {
	owner := tc.agentSessionOwnerName()
	for i, c := range tc.Spec.Credentials {
		name := c.Source.Name
		suffix := perSessionSecretSuffix(name)
		if suffix == "" {
			continue
		}
		if owner == "" {
			return fmt.Errorf(
				"credentials[%d].source.name %q is a per-session Secret (suffix %q) but the ToolCall has no AgentSession owner to bind it to",
				i, name, suffix)
		}
		if want := owner + suffix; name != want {
			return fmt.Errorf(
				"credentials[%d].source.name %q references another session's per-session Secret; this ToolCall (owned by AgentSession %q) may reference only %q (cross-session credential reference refused)",
				i, name, owner, want)
		}
	}
	return nil
}

// perSessionSecretSuffix returns the PerSessionSecretSuffixes entry that name
// ends with, or "" when name matches none of them.
func perSessionSecretSuffix(name string) string {
	for _, suffix := range PerSessionSecretSuffixes {
		if strings.HasSuffix(name, suffix) {
			return suffix
		}
	}
	return ""
}

// agentSessionOwnerName returns the name of the AgentSession that owns this
// ToolCall, or "" when none is set. The webhook independently requires an
// AgentSession owner (an unpinned ToolCall is refused), so "" here means the
// owner check has not — or cannot — be satisfied.
func (tc *ToolCall) agentSessionOwnerName() string {
	for _, o := range tc.OwnerReferences {
		if o.Kind == "AgentSession" {
			return o.Name
		}
	}
	return ""
}
