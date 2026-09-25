// Package uibindings is the data-binding resolver contract: the seam a
// declared component prop's binding (pkg/web/uicomponents.Binding) crosses to
// become a value, resolved SERVER-SIDE under the viewer's subject. The browser
// never holds a capability and never resolves a binding itself; only the
// collaborator set in Deps can reach a tool, memory, or an artifact.
//
// This package holds only the contract. Each source's Resolver registers
// itself in pkg/web/uibindings/registry from its own init(), the same
// registry-over-branching shape as every other pluggable aspect in this repo —
// so the live set of sources is registry.Keys(), never a list transcribed into
// a comment. A consumer growing `if req.Source == "tool"` outside a resolver's
// own package means the abstraction is missing a method.
package uibindings

import (
	"context"
	"encoding/json"

	"github.com/go-logr/logr"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// Request is one binding to resolve, already located in the server-side
// declaration and already parameter-substituted.
type Request struct {
	Namespace string // from the URL path, never the request body
	Session   string // AgentSession name, from the URL path
	Subject   string // the cookie-verified viewer, "user:<...>" form
	Path      string // uicomponents.BindingPath — for logs and the response key
	Ref       string // the binding's Ref, verbatim from the declaration
	Args      json.RawMessage
}

// Result is what the browser receives for one binding.
type Result struct {
	// Value is the resolved data, before any declaration selector is applied.
	Value json.RawMessage
}

// Resolver brokers one binding source server-side under the viewer's subject.
type Resolver interface {
	Source() string
	Resolve(ctx context.Context, d Deps, req Request) (Result, error)
}

// ArtifactRenderBytesFunc fetches the raw bytes and MIME type of a resolved
// artifact render. It lives on Deps rather than being resolver-constructed
// because the bytes sit behind the operator's system-bearer artifact route
// that only webd holds a token for, and artifactref must not learn that
// token's shape — the same reason NATSRequest is a Deps method.
type ArtifactRenderBytesFunc func(ctx context.Context, ns, session, renderName string) ([]byte, string, error)

// Deps is the collaborator set every resolver draws on. A resolver uses only
// the collaborators its own source needs (a "tool" resolver has no reason to
// touch Artifacts, say) — but for the collaborators it DOES need, it must
// fail closed with a returned error when one is nil (an unconfigured webd:
// no POSTGRES_URI, no artifact store, no NATS), never proceed with a nil
// memory.Memory, a nil channelevents.RequestFunc, or a nil ArtifactRenderBytesFunc.
//
// The four collaborators below have THREE different nil-check shapes:
//   - Memory() returns an INTERFACE, so its nil check is honest only if the
//     construction site kept it one. Assign a typed-nil *memory.Local into it
//     and `Memory() != nil` reports TRUE, the fail-closed check passes, and
//     the resolver panics on first call — declare the variable as the
//     interface and assign only once a real value exists.
//   - Artifacts() returns a concrete POINTER, where nil means what it says.
//   - NATSRequest() and ArtifactRenderBytes() return FUNC types directly, so
//     there is no interface wrapper for a typed-nil to hide behind.
type Deps interface {
	Memory() memory.Memory
	Artifacts() *artifacts.Service
	NATSRequest() channelevents.RequestFunc
	ArtifactRenderBytes() ArtifactRenderBytesFunc
	Logger() logr.Logger
}
