// Package broker defines the token broker: the single seam that turns credential
// descriptors into injectable tool tokens; the in-process implementation lives in
// ./inproc. Inputs and outputs are plain serializable data — no client.Client, no
// CRD root objects — so an out-of-process implementation behind this same
// interface stays a non-breaking change.
package broker

import (
	"context"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Broker resolves credential descriptors into injectable tokens.
type Broker interface {
	// Resolve reads the Secret behind each descriptor and projects the token into
	// the descriptor's injection shape. Implementations are the only code that
	// touches Secret bytes.
	Resolve(ctx context.Context, req Request) (Resolution, error)

	// InvalidateSecret drops any cached resolution for the backing Secret
	// (namespace, name) so the next Resolve re-reads it; a safe no-op when absent.
	// This is THE credential-revocation entry point: the `credential` invalidator
	// on the `ap.revocation` bus calls it within tens of ms of the operator
	// observing a credential removal or replacement, covering UserIdentity and
	// AgentIdentity credentials alike — both resolve to a backing Secret.
	InvalidateSecret(namespace, name string) error
}

// Request is a set of credential descriptors to resolve together.
type Request struct {
	Credentials []spiceboxv1alpha1.CredentialDescriptor
}

// Resolution is the unified injectable output. Either map may be empty.
type Resolution struct {
	EnvVars     map[string]string
	HTTPHeaders map[string]string
}
